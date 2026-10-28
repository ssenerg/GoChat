package e2e

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
)

// app calls the client API the way the native apps do: protobuf over HTTP
// with a bearer token.
type app struct {
	t     *testing.T
	http  *http.Client
	token string
}

func newApp(t *testing.T) *app {
	return &app{t: t, http: &http.Client{Timeout: 20 * time.Second}}
}

// raw sends one request and returns the answer, waiting out the sign-in
// limiter like the browser clients do.
func (a *app) raw(method, path string, body []byte, header http.Header) (*http.Response, []byte) {
	a.t.Helper()
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(method, baseURL+path, bytes.NewReader(body))
		if err != nil {
			a.t.Fatal(err)
		}
		for k, v := range header {
			req.Header[k] = v
		}
		if a.token != "" && req.Header.Get("Authorization") == "" {
			req.Header.Set("Authorization", "Bearer "+a.token)
		}
		res, err := a.http.Do(req)
		if err != nil {
			a.t.Fatalf("%s %s: %v", method, path, err)
		}
		b, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusTooManyRequests || attempt == 5 {
			return res, b
		}
		time.Sleep(10 * time.Second)
	}
}

// call makes one RPC; it returns the ApiError when the call fails.
func (a *app) call(method string, in, out proto.Message) (int, *pb.ApiError) {
	a.t.Helper()
	body, _ := proto.Marshal(in)
	res, b := a.raw(http.MethodPost, "/api/v1/rpc/"+method, body, http.Header{"Content-Type": {"application/proto"}})
	if res.StatusCode != http.StatusOK {
		e := &pb.ApiError{}
		if err := proto.Unmarshal(b, e); err != nil {
			a.t.Fatalf("%s: %d and no ApiError: %q", method, res.StatusCode, b)
		}
		return res.StatusCode, e
	}
	if err := proto.Unmarshal(b, out); err != nil {
		a.t.Fatalf("%s: %v", method, err)
	}
	return res.StatusCode, nil
}

func (a *app) must(method string, in, out proto.Message) {
	a.t.Helper()
	if status, e := a.call(method, in, out); e != nil {
		a.t.Fatalf("%s: %d %v", method, status, e)
	}
}

func (a *app) login(login, password string) *pb.User {
	a.t.Helper()
	res := &pb.LoginResponse{}
	a.must("gochat.v1.AuthService/Login", &pb.LoginRequest{Login: login, Password: password}, res)
	if res.GetSession().GetToken() == "" {
		a.t.Fatalf("login gave no session: %v", res)
	}
	a.token = res.GetSession().GetToken()
	me := &pb.MeResponse{}
	a.must("gochat.v1.AuthService/Me", &pb.MeRequest{}, me)
	return me.User
}

type appSocket struct {
	t      *testing.T
	conn   *websocket.Conn
	frames chan *pb.ClientFrame
	closed chan websocket.StatusCode
}

func (a *app) socket() *appSocket {
	a.t.Helper()
	u, _ := url.Parse(baseURL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+u.Host+"/api/v1/gateway", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + a.token}},
	})
	if err != nil {
		a.t.Fatalf("event socket: %v", err)
	}
	conn.SetReadLimit(4 << 20)
	s := &appSocket{t: a.t, conn: conn, frames: make(chan *pb.ClientFrame, 256), closed: make(chan websocket.StatusCode, 1)}
	go func() {
		for {
			typ, data, err := conn.Read(context.Background())
			if err != nil {
				s.closed <- websocket.CloseStatus(err)
				close(s.frames)
				return
			}
			f := &pb.ClientFrame{}
			if typ != websocket.MessageBinary || proto.Unmarshal(data, f) != nil {
				s.t.Errorf("the event socket sent a frame that is no ClientFrame: %q", data)
				continue
			}
			s.frames <- f
		}
	}()
	return s
}

// next waits for the first frame that match accepts.
func (s *appSocket) next(d time.Duration, what string, match func(*pb.ClientFrame) bool) *pb.ClientFrame {
	s.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case f, ok := <-s.frames:
			if !ok {
				s.t.Fatalf("the event socket closed while waiting for %s", what)
			}
			if match(f) {
				return f
			}
		case <-deadline:
			s.t.Fatalf("no %s on the event socket", what)
		}
	}
}

// send writes an AppFrame, as the apps say what they show.
func (s *appSocket) send(f *pb.AppFrame) {
	s.t.Helper()
	b, _ := proto.Marshal(f)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.conn.Write(ctx, websocket.MessageBinary, b); err != nil {
		s.t.Fatalf("app frame: %v", err)
	}
}

// eventually repeats act until s receives a frame match accepts: what an app
// says it shows reaches the chat server a moment after.
func (s *appSocket) eventually(what string, act func(), match func(*pb.ClientFrame) bool) {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		act()
		wait := time.After(time.Second)
	frames:
		for {
			select {
			case f, ok := <-s.frames:
				if !ok {
					s.t.Fatalf("the event socket closed while waiting for %s", what)
				}
				if match(f) {
					return
				}
			case <-wait:
				break frames
			}
		}
	}
	s.t.Fatalf("no %s on the event socket", what)
}

// never fails when s receives a frame match accepts within d.
func (s *appSocket) never(d time.Duration, what string, match func(*pb.ClientFrame) bool) {
	s.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case f, ok := <-s.frames:
			if !ok {
				return
			}
			if match(f) {
				s.t.Fatalf("got %s: %v", what, f)
			}
		case <-deadline:
			return
		}
	}
}

func (s *appSocket) closedWith(d time.Duration) websocket.StatusCode {
	s.t.Helper()
	for {
		select {
		case <-s.frames:
		case code := <-s.closed:
			return code
		case <-time.After(d):
			s.t.Fatal("the event socket stayed open")
		}
	}
}

func testClientAPI(t *testing.T, bobName, bobPass, town string) {
	admin, bob := newApp(t), newApp(t)
	adminUser := admin.login(adminName, adminPass)
	bobUser := bob.login(bobName, bobPass)

	t.Run("teams and channels list", func(t *testing.T) {
		teams := &pb.ListTeamsResponse{}
		bob.must("gochat.v1.TeamService/ListTeams", &pb.ListTeamsRequest{}, teams)
		var team *pb.Team
		for _, tm := range teams.Teams {
			if tm.Name == teamName {
				team = tm
			}
		}
		if team == nil {
			t.Fatalf("bob's teams miss %s: %v", teamName, teams.Teams)
		}
		chans := &pb.ListChannelsResponse{}
		bob.must("gochat.v1.ChannelService/ListChannels", &pb.ListChannelsRequest{TeamId: team.Id}, chans)
		found := false
		for _, ch := range chans.Channels {
			found = found || ch.Id == town
		}
		if !found {
			t.Fatal("bob's channels miss the town square")
		}
	})

	adminWS, bobWS := admin.socket(), bob.socket()
	for _, s := range []*appSocket{adminWS, bobWS} {
		f := s.next(5*time.Second, "ready", func(f *pb.ClientFrame) bool { return f.GetReady() != nil })
		if f.GetReady().GetUser().GetId() == "" || f.GetReady().GetExpiresAt() == nil {
			t.Fatalf("ready without its user or expiry: %v", f)
		}
	}

	t.Run("messages and typing arrive as events", func(t *testing.T) {
		posted := &pb.PostMessageResponse{}
		admin.must("gochat.v1.MessageService/PostMessage", &pb.PostMessageRequest{ChannelId: town, Content: "from the app <b>raw</b>"}, posted)
		f := bobWS.next(5*time.Second, "the message", func(f *pb.ClientFrame) bool {
			return f.GetEvent().GetMessagePosted().GetMessage().GetId() == posted.Message.Id
		})
		if got := f.GetEvent().GetMessagePosted().GetMessage(); got.Content != "from the app <b>raw</b>" || got.Author.GetId() != adminUser.Id {
			t.Fatalf("the event changed the message: %v", got)
		}
		if ev := f.GetEvent(); len(ev.ConnIds) != 0 || ev.Broadcast {
			t.Fatalf("the event names its recipients: %v", ev)
		}
		// Typing reaches the apps that show the channel, as it does pages.
		adminWS.send(&pb.AppFrame{Frame: &pb.AppFrame_View{View: &pb.AppView{ChannelId: town}}})
		adminWS.eventually("typing", func() {
			bob.call("gochat.v1.MessageService/SendTyping", &pb.SendTypingRequest{ChannelId: town}, &pb.SendTypingResponse{})
		}, func(f *pb.ClientFrame) bool {
			return f.GetEvent().GetTyping().GetUser().GetId() == bobUser.Id
		})
		bob.must("gochat.v1.MessageService/React", &pb.ReactRequest{MessageId: posted.Message.Id, ChannelId: town, Emoji: "tada"}, &pb.ReactResponse{})
		adminWS.next(5*time.Second, "the reaction", func(f *pb.ClientFrame) bool {
			return f.GetEvent().GetReactionsChanged().GetMessageId() == posted.Message.Id
		})
	})

	t.Run("link previews", func(t *testing.T) {
		edited := func(id string, preview bool) func(*pb.ClientFrame) bool {
			return func(f *pb.ClientFrame) bool {
				m := f.GetEvent().GetMessageEdited().GetMessage()
				return m.GetId() == id && (m.GetLinkPreview() != nil) == preview
			}
		}
		// A link into the server's own network is never fetched.
		posted := &pb.PostMessageResponse{}
		admin.must("gochat.v1.MessageService/PostMessage", &pb.PostMessageRequest{ChannelId: town, Content: "see http://127.0.0.1:9100/metrics"}, posted)
		bobWS.never(2*time.Second, "a preview of a private address", edited(posted.Message.Id, true))
		remove := &pb.RemoveLinkPreviewRequest{ChannelId: town, MessageId: posted.Message.Id}
		if status, e := bob.call("gochat.v1.MessageService/RemoveLinkPreview", remove, &pb.RemoveLinkPreviewResponse{}); e == nil || e.Code != 7 {
			t.Fatalf("bob removed the preview of admin's message: %d %v", status, e)
		}
		admin.must("gochat.v1.MessageService/RemoveLinkPreview", remove, &pb.RemoveLinkPreviewResponse{})
		bobWS.next(5*time.Second, "the removal", edited(posted.Message.Id, false))
		if res, _ := bob.raw(http.MethodGet, "/api/v2/previews/"+strings.Repeat("0", 32), nil, nil); res.StatusCode != http.StatusNotFound {
			t.Fatalf("a picture no preview has: %d", res.StatusCode)
		}
		// A public page, when one is named, gets its card and its picture.
		page := os.Getenv("GOCHAT_E2E_PREVIEW_URL")
		if page == "" {
			return
		}
		admin.must("gochat.v1.MessageService/PostMessage", &pb.PostMessageRequest{ChannelId: town, Content: "look: " + page}, posted)
		p := bobWS.next(30*time.Second, "the preview", edited(posted.Message.Id, true)).GetEvent().GetMessageEdited().GetMessage().GetLinkPreview()
		if p.Url != page || p.Title == "" {
			t.Fatalf("preview %v", p)
		}
		if p.ImageId != "" {
			res, b := bob.raw(http.MethodGet, "/api/v2/previews/"+p.ImageId, nil, nil)
			if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "image/") || len(b) == 0 {
				t.Fatalf("the picture: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
			}
		}
		got := &pb.GetMessageResponse{}
		bob.must("gochat.v1.MessageService/GetMessage", &pb.GetMessageRequest{ChannelId: town, MessageId: posted.Message.Id}, got)
		if got.Message.GetLinkPreview().GetTitle() != p.Title {
			t.Fatalf("the message keeps no preview: %v", got.Message)
		}
		t.Logf("preview of %s: %q from %q, picture %s %dx%d", page, p.Title, p.SiteName, p.ImageId, p.ImageWidth, p.ImageHeight)
	})

	t.Run("presence reaches the apps that show the person", func(t *testing.T) {
		adminWS.send(&pb.AppFrame{Frame: &pb.AppFrame_Watch{Watch: &pb.AppWatch{UserIds: []string{bobUser.Id, "not-an-id"}}}})
		away := false
		adminWS.eventually("bob's presence", func() {
			away = !away
			status := pb.Status_STATUS_ONLINE
			if away {
				status = pb.Status_STATUS_AWAY
			}
			bob.must("gochat.v1.UserService/SetStatus", &pb.SetStatusRequest{Status: status}, &pb.SetStatusResponse{})
		}, func(f *pb.ClientFrame) bool {
			return f.GetEvent().GetPresence().GetUserId() == bobUser.Id
		})
		bob.must("gochat.v1.UserService/SetStatus", &pb.SetStatusRequest{Status: pb.Status_STATUS_ONLINE}, &pb.SetStatusResponse{})
	})

	t.Run("apps watch only the people they share a team with", func(t *testing.T) {
		// Carol's only team is one bob is not in; the admin is in both.
		team := &pb.CreateTeamResponse{}
		admin.must("gochat.v1.TeamService/CreateTeam", &pb.CreateTeamRequest{Name: "apart-" + random(4), DisplayName: "Apart"}, team)
		invite := &pb.CreateInviteResponse{}
		admin.must("gochat.v1.TeamService/CreateInvite", &pb.CreateInviteRequest{TeamId: team.Team.Id}, invite)
		carol := newApp(t)
		name := "carol-" + random(4)
		signed := &pb.SignUpResponse{}
		carol.must("gochat.v1.AuthService/SignUp", &pb.SignUpRequest{
			Username: name, Email: name + "@example.test", DisplayName: "Carol", Password: "pw-" + random(12), InviteToken: invite.Token,
		}, signed)
		carol.token = signed.Session.GetToken()
		presence := &pb.GetPresenceResponse{}
		carol.must("gochat.v1.UserService/GetPresence", &pb.GetPresenceRequest{UserIds: []string{bobUser.Id}}, presence)
		if len(presence.OnlineUserIds) != 0 || len(presence.Statuses) != 0 {
			t.Fatalf("a stranger's presence: %v", presence)
		}
		carolWS := carol.socket()
		carolWS.next(5*time.Second, "ready", func(f *pb.ClientFrame) bool { return f.GetReady() != nil })
		carolWS.send(&pb.AppFrame{Frame: &pb.AppFrame_Watch{Watch: &pb.AppWatch{UserIds: []string{bobUser.Id, adminUser.Id}}}})
		// The admin's changes reach carol: the watch is in place.
		away := false
		carolWS.eventually("the admin's presence", func() {
			away = !away
			status := pb.Status_STATUS_ONLINE
			if away {
				status = pb.Status_STATUS_AWAY
			}
			admin.must("gochat.v1.UserService/SetStatus", &pb.SetStatusRequest{Status: status}, &pb.SetStatusResponse{})
		}, func(f *pb.ClientFrame) bool {
			return f.GetEvent().GetPresence().GetUserId() == adminUser.Id
		})
		admin.must("gochat.v1.UserService/SetStatus", &pb.SetStatusRequest{Status: pb.Status_STATUS_ONLINE}, &pb.SetStatusResponse{})
		// Bob's never do, nor his custom status.
		bob.must("gochat.v1.UserService/SetStatus", &pb.SetStatusRequest{Status: pb.Status_STATUS_DND}, &pb.SetStatusResponse{})
		bob.must("gochat.v1.UserService/SetCustomStatus", &pb.SetCustomStatusRequest{Emoji: "palm_tree", Text: "away from " + random(4)}, &pb.SetCustomStatusResponse{})
		bob.must("gochat.v1.UserService/SetStatus", &pb.SetStatusRequest{Status: pb.Status_STATUS_ONLINE}, &pb.SetStatusResponse{})
		bob.must("gochat.v1.UserService/SetCustomStatus", &pb.SetCustomStatusRequest{}, &pb.SetCustomStatusResponse{})
		carolWS.never(2*time.Second, "bob's presence or profile", func(f *pb.ClientFrame) bool {
			ev := f.GetEvent()
			return ev.GetPresence().GetUserId() == bobUser.Id || ev.GetProfileChanged().GetUser().GetId() == bobUser.Id
		})
	})

	t.Run("apps learn the server's version without a token", func(t *testing.T) {
		res, b := newApp(t).raw(http.MethodGet, "/api/v1/info", nil, http.Header{"Accept": {"application/proto"}})
		info := &pb.ServerInfo{}
		if res.StatusCode != http.StatusOK || proto.Unmarshal(b, info) != nil || info.Version == "" {
			t.Fatalf("info: %d %q", res.StatusCode, b)
		}
	})

	t.Run("JSON works too", func(t *testing.T) {
		res, b := bob.raw(http.MethodPost, "/api/v1/rpc/gochat.v1.MessageService/ListMessages",
			[]byte(`{"channelId":"`+town+`","limit":5}`), http.Header{"Content-Type": {"application/json"}})
		out := &pb.ListMessagesResponse{}
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/json" || protojson.Unmarshal(b, out) != nil || len(out.Messages) == 0 {
			t.Fatalf("JSON call: %d %s", res.StatusCode, b)
		}
	})

	t.Run("files upload and download", func(t *testing.T) {
		data := []byte("app attachment " + random(8))
		res, b := bob.raw(http.MethodPost, "/api/v1/files?channel_id="+town+"&name=notes.txt", data,
			http.Header{"Accept": {"application/proto"}, "Content-Type": {"application/octet-stream"}})
		att := &pb.Attachment{}
		if res.StatusCode != http.StatusOK || proto.Unmarshal(b, att) != nil || att.Id == "" || att.Size != int64(len(data)) {
			t.Fatalf("upload: %d %q", res.StatusCode, b)
		}
		posted := &pb.PostMessageResponse{}
		bob.must("gochat.v1.MessageService/PostMessage", &pb.PostMessageRequest{ChannelId: town, AttachmentIds: []string{att.Id}}, posted)
		res, got := admin.raw(http.MethodGet, "/api/v1/files/"+att.Id, nil, nil)
		if res.StatusCode != http.StatusOK || !bytes.Equal(got, data) || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain") ||
			!strings.Contains(res.Header.Get("Content-Security-Policy"), "sandbox") {
			t.Fatalf("download: %d %v %q", res.StatusCode, res.Header, got)
		}
		webm := []byte("\x1a\x45\xdf\xa3" + strings.Repeat("\x00", 64))
		res, b = bob.raw(http.MethodPost, "/api/v1/files?channel_id="+town+"&name=voice.webm&voice=1&duration_ms=1800", webm,
			http.Header{"Accept": {"application/proto"}})
		voice := &pb.Attachment{}
		if res.StatusCode != http.StatusOK || proto.Unmarshal(b, voice) != nil || !voice.Voice || voice.DurationMs != 1800 || voice.ContentType != "audio/webm" {
			t.Fatalf("voice upload: %d %q", res.StatusCode, b)
		}
		res, b = bob.raw(http.MethodPost, "/api/v1/files?channel_id="+town+"&name=voice.webm&voice=1", []byte("not a recording"), nil)
		if res.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), `"code":3`) {
			t.Fatalf("a voice message that is no recording: %d %s", res.StatusCode, b)
		}
	})

	img := image.NewRGBA(image.Rect(0, 0, 300, 200))
	for x := range 300 {
		for y := range 200 {
			img.Set(x, y, color.RGBA{R: uint8(x), G: 90, B: uint8(y), A: 255})
		}
	}
	var picture bytes.Buffer
	_ = png.Encode(&picture, img)

	t.Run("profile photo", func(t *testing.T) {
		res, b := bob.raw(http.MethodPost, "/api/v1/photo", picture.Bytes(), http.Header{"Accept": {"application/proto"}})
		u := &pb.User{}
		if res.StatusCode != http.StatusOK || proto.Unmarshal(b, u) != nil || u.Photo == "" {
			t.Fatalf("set photo: %d %q", res.StatusCode, b)
		}
		res, got := admin.raw(http.MethodGet, "/api/v1/photos/u/"+u.Id+"/"+u.Photo, nil, nil)
		if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "image/") || len(got) == 0 {
			t.Fatalf("get photo: %d %v", res.StatusCode, res.Header)
		}
		bob.must("gochat.v1.UserService/RemovePhoto", &pb.RemovePhotoRequest{}, &pb.RemovePhotoResponse{})
	})

	t.Run("team photo", func(t *testing.T) {
		teams := &pb.ListTeamsResponse{}
		admin.must("gochat.v1.TeamService/ListTeams", &pb.ListTeamsRequest{}, teams)
		var teamID string
		for _, tm := range teams.Teams {
			if tm.Name == teamName {
				teamID = tm.Id
			}
		}
		path := "/api/v2/teams/" + teamID + "/photo"
		accept := http.Header{"Accept": {"application/proto"}}
		if res, b := bob.raw(http.MethodPost, path, picture.Bytes(), accept); res.StatusCode != http.StatusForbidden {
			t.Fatalf("a member set the team photo: %d %q", res.StatusCode, b)
		}
		if res, b := admin.raw(http.MethodPost, path, []byte("not a picture"), accept); res.StatusCode != http.StatusBadRequest {
			t.Fatalf("a team photo that is no picture: %d %q", res.StatusCode, b)
		}
		res, b := admin.raw(http.MethodPost, path, picture.Bytes(), accept)
		team := &pb.Team{}
		if res.StatusCode != http.StatusOK || proto.Unmarshal(b, team) != nil || team.Photo == "" || team.Id != teamID {
			t.Fatalf("set the team photo: %d %q", res.StatusCode, b)
		}
		res, got := bob.raw(http.MethodGet, "/api/v1/photos/t/"+teamID+"/"+team.Photo, nil, nil)
		if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "image/") || len(got) == 0 {
			t.Fatalf("get the team photo: %d %v", res.StatusCode, res.Header)
		}
		removed := &pb.RemoveTeamPhotoResponse{}
		admin.must("gochat.v1.TeamService/RemoveTeamPhoto", &pb.RemoveTeamPhotoRequest{TeamId: teamID}, removed)
		if removed.Team.GetPhoto() != "" {
			t.Fatalf("the team photo stayed: %v", removed.Team)
		}
	})

	t.Run("apps talk in voice channels", func(t *testing.T) {
		teams := &pb.ListTeamsResponse{}
		admin.must("gochat.v1.TeamService/ListTeams", &pb.ListTeamsRequest{}, teams)
		var teamID string
		for _, tm := range teams.Teams {
			if tm.Name == teamName {
				teamID = tm.Id
			}
		}
		created := &pb.CreateChannelResponse{}
		admin.must("gochat.v1.ChannelService/CreateChannel", &pb.CreateChannelRequest{
			TeamId: teamID, Name: "app-talk-" + random(3), DisplayName: "App talk", Type: pb.ChannelType_CHANNEL_TYPE_VOICE,
		}, created)
		room := created.Channel.Id
		ticket := func(a *app) voiceTicket {
			res := &pb.JoinVoiceResponse{}
			a.must("gochat.v1.VoiceService/JoinVoice", &pb.JoinVoiceRequest{ChannelId: room}, res)
			if res.Endpoint == "" || res.Token == "" || res.Channel.GetId() != room {
				t.Fatalf("join voice: %v", res)
			}
			return voiceTicket{Endpoint: res.Endpoint, Token: res.Token}
		}
		// Apps send no Origin, which the voice server takes.
		a := connectVoice(t, ticket(admin), "")
		b := connectVoice(t, ticket(bob), "")
		a.waitFor("both are ready", func() bool { return a.id() != "" && b.id() != "" })
		a.waitFor("admin hears bob", func() bool { return a.heardFrom(b.id()) > 10 })
		b.waitFor("bob hears admin", func() bool { return b.heardFrom(a.id()) > 10 })

		bob.must("gochat.v1.VoiceService/UpdateVoiceState", &pb.UpdateVoiceStateRequest{SelfMute: true}, &pb.UpdateVoiceStateResponse{})
		adminWS.next(5*time.Second, "bob muted", func(f *pb.ClientFrame) bool {
			ch := f.GetEvent().GetVoiceChannelUpdate().GetChannel()
			if ch.GetChannelId() != room {
				return false
			}
			for _, s := range ch.States {
				if s.GetUser().GetId() == bobUser.Id && s.SelfMute {
					return true
				}
			}
			return false
		})

		if status, e := bob.call("gochat.v1.VoiceService/DisconnectVoiceMember", &pb.DisconnectVoiceMemberRequest{ChannelId: room, UserId: adminUser.Id}, &pb.DisconnectVoiceMemberResponse{}); e == nil || e.Code != 7 {
			t.Fatalf("bob disconnected admin: %d %v", status, e)
		}
		admin.must("gochat.v1.VoiceService/DisconnectVoiceMember", &pb.DisconnectVoiceMemberRequest{ChannelId: room, UserId: bobUser.Id}, &pb.DisconnectVoiceMemberResponse{})
		if ce := b.closedWith(); ce.Code != 4002 || ce.Reason != "disconnected by a moderator" {
			t.Fatalf("bob's close: %d %q", ce.Code, ce.Reason)
		}
		admin.must("gochat.v1.VoiceService/LeaveVoice", &pb.LeaveVoiceRequest{}, &pb.LeaveVoiceResponse{})
		if ce := a.closedWith(); ce.Code != 4002 {
			t.Fatalf("admin's close: %d %q", ce.Code, ce.Reason)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		anon := newApp(t)
		if status, e := anon.call("gochat.v1.AuthService/Me", &pb.MeRequest{}, &pb.MeResponse{}); status != http.StatusUnauthorized || e.Code != 16 {
			t.Fatalf("no token: %d %v", status, e)
		}
		anon.token = strings.Repeat("A", 43)
		if status, _ := anon.call("gochat.v1.AuthService/Me", &pb.MeRequest{}, &pb.MeResponse{}); status != http.StatusUnauthorized {
			t.Fatalf("an unknown token: %d", status)
		}
		for _, m := range []string{"gochat.v1.GatewayService/Connect", "gochat.v1.FileService/Upload", "gochat.v1.UserService/SetPhoto", "gochat.v1.VoiceServerService/Connect", "gochat.v1.Nope/Nope"} {
			if status, e := bob.call(m, &pb.MeRequest{}, &pb.MeResponse{}); status != http.StatusNotFound || e.Code != 5 {
				t.Errorf("%s: %d %v", m, status, e)
			}
		}
		res, _ := bob.raw(http.MethodPost, "/api/v1/rpc/gochat.v1.AuthService/Me", nil, http.Header{"Content-Type": {"text/plain"}})
		if res.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("a text/plain body: %d", res.StatusCode)
		}
		// A browser's cookies sign nothing in.
		web := newClient(t, "web")
		web.login(bobName, bobPass)
		req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/rpc/gochat.v1.AuthService/Me", nil)
		req.Header.Set("Content-Type", "application/proto")
		res, err := web.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("a cookie authenticated an API call: %d", res.StatusCode)
		}
		u, _ := url.Parse(baseURL)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, res, err := websocket.Dial(ctx, "ws://"+u.Host+"/api/v1/gateway", nil); err == nil || res == nil || res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("an event socket without a token: %v %v", err, res)
		}
		anonWS := anon.socket()
		if code := anonWS.closedWith(5 * time.Second); code != 4001 {
			t.Fatalf("an event socket with an unknown token closed with %d", code)
		}
		// A user that is not a member reads nothing private: the server
		// checks every call.
		if status, _ := bob.call("gochat.v1.ChannelService/ArchiveChannel", &pb.ArchiveChannelRequest{ChannelId: town, Archived: true}, &pb.ArchiveChannelResponse{}); status < 400 {
			t.Fatalf("bob archived the town square: %d", status)
		}
	})

	t.Run("logout ends the event socket", func(t *testing.T) {
		admin.must("gochat.v1.AuthService/Logout", &pb.LogoutRequest{}, &pb.LogoutResponse{})
		if code := adminWS.closedWith(5 * time.Second); code != 4003 {
			t.Fatalf("the socket of a signed-out session closed with %d", code)
		}
		if status, _ := admin.call("gochat.v1.AuthService/Me", &pb.MeRequest{}, &pb.MeResponse{}); status != http.StatusUnauthorized {
			t.Fatalf("a signed-out token still works: %d", status)
		}
		_ = bobWS.conn.CloseNow()
	})
}
