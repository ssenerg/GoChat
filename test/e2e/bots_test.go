package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/pkg/gochat"
)

var (
	botTokenRe = regexp.MustCompile(`<code>(b_[A-Za-z0-9_-]+)</code>`)
	botIDRe    = regexp.MustCompile(`/settings/bots/([0-9a-f-]{36})/team`)
)

// botNext waits for the bot's next update that matches.
func botNext(t *testing.T, c *gochat.Client, what string, match func(*pb.Event) bool) *pb.Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case u, ok := <-c.Updates():
			if !ok {
				t.Fatalf("the bot stopped waiting for %s: %v", what, c.Err())
			}
			if u.Event != nil && match(u.Event) {
				return u.Event
			}
		case <-deadline:
			t.Fatalf("the bot got no %s", what)
		}
	}
}

// postedBy matches a message posted by someone other than the bot.
func postedBy(bot string, content func(string) bool) func(*pb.Event) bool {
	return func(ev *pb.Event) bool {
		m := ev.GetMessagePosted().GetMessage()
		return m != nil && m.GetAuthor().GetId() != bot && content(m.Content)
	}
}

// botReply is the HTTP Bot API's answer.
type botReply struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
}

func botHTTP(t *testing.T, token, method string, body any) (int, botReply) {
	t.Helper()
	a := newApp(t)
	a.token = token
	b, _ := json.Marshal(body)
	res, out := a.raw(http.MethodPost, "/api/v2/bot/"+method, b, http.Header{"Content-Type": {"application/json"}})
	var r botReply
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("%s: %d %q", method, res.StatusCode, out)
	}
	return res.StatusCode, r
}

func testBots(t *testing.T, admin *client, adminWS *socket, teamID, town string, view func(*socket, string)) {
	tag := random(4)
	name := "e2e" + tag + "bot"
	_, page := admin.hx("/settings/bots", url.Values{"username": {name}, "display_name": {"Echo " + tag}})
	tm, im := botTokenRe.FindStringSubmatch(page), botIDRe.FindStringSubmatch(page)
	if tm == nil || im == nil {
		t.Fatalf("creating a bot showed no token: %s", page)
	}
	token, botID := tm[1], im[1]
	if _, page := admin.hx("/settings/bots/"+botID+"/team", url.Values{"team_id": {teamID}}); !strings.Contains(page, "joined the team") {
		t.Fatalf("adding the bot to the team: %s", page)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := gochat.Dial(ctx, gochat.Options{URL: baseURL, Token: token, Client: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if me := c.Me(); !me.Bot || me.Username != name {
		t.Fatalf("the bot is %v", me)
	}

	adminAPI := newApp(t)
	adminAPI.login(adminName, adminPass)
	adminAPI.must("gochat.v1.ChannelService/AddChannelMember", &pb.AddChannelMemberRequest{ChannelId: town, UserId: botID}, &pb.AddChannelMemberResponse{})
	botNext(t, c, "its joining", func(ev *pb.Event) bool { return ev.GetChannelJoined().GetChannel().GetId() == town })

	if err := c.Call(ctx, "gochat.v1.BotService/SetCommands", &pb.SetCommandsRequest{Commands: []*pb.BotCommand{
		{Name: "echo", Description: "Repeat " + tag},
	}}, &pb.SetCommandsResponse{}); err != nil {
		t.Fatal(err)
	}
	if _, list := admin.do(http.MethodGet, "/m/"+town+"/commands?q=ec", nil, "", true); !strings.Contains(list, "/echo") || !strings.Contains(list, "Repeat "+tag) {
		t.Fatalf("the composer suggests no /echo: %s", list)
	}

	view(adminWS, town)
	// Each step reports on its own test.
	step := func(name string, f func(t *testing.T)) {
		t.Run(name, func(t *testing.T) {
			admin.t, adminWS.t = t, t
			f(t)
		})
	}
	var reply *pb.Message
	step("a command gets an answer with a button", func(t *testing.T) {
		adminWS.send(map[string]string{"op": "post", "channel_id": town, "content": "/echo hi " + tag})
		ev := botNext(t, c, "the command", postedBy(botID, func(s string) bool { return s == "/echo hi "+tag }))
		m := ev.GetMessagePosted().GetMessage()
		if cmd, args, ok := gochat.Command(m.Content, name); !ok || cmd != "echo" || args != "hi "+tag {
			t.Fatalf("command %q %q %v", cmd, args, ok)
		}
		res := &pb.PostMessageResponse{}
		err := c.Call(ctx, "gochat.v1.MessageService/PostMessage", &pb.PostMessageRequest{
			ChannelId: town, Content: "hi " + tag,
			Markup: &pb.Markup{Rows: []*pb.ButtonRow{{Buttons: []*pb.Button{{Text: "Thanks", Action: &pb.Button_CallbackData{CallbackData: "thanks"}}}}}},
		}, res)
		if err != nil {
			t.Fatal(err)
		}
		reply = res.Message
		adminWS.expect(10*time.Second, `id="msg-`+reply.Id, `data-press="thanks"`, `badge bot`)
	})

	step("a press reaches the bot and its answer the presser", func(t *testing.T) {
		if reply == nil {
			t.Skip("no message to press")
		}
		adminWS.send(map[string]string{"op": "press", "channel_id": town, "message_id": reply.Id, "data": "thanks"})
		q := botNext(t, c, "the press", func(ev *pb.Event) bool { return ev.GetCallbackQuery() != nil }).GetCallbackQuery()
		if q.Data != "thanks" || q.GetMessage().GetId() != reply.Id || q.GetFrom().GetId() == "" {
			t.Fatalf("query %v", q)
		}
		if err := c.Call(ctx, "gochat.v1.BotService/AnswerCallback", &pb.AnswerCallbackRequest{QueryId: q.Id, Text: "welcome " + tag}, &pb.AnswerCallbackResponse{}); err != nil {
			t.Fatal(err)
		}
		adminWS.expect(10*time.Second, "toast", "welcome "+tag)
		// A button the message does not have.
		adminWS.send(map[string]string{"op": "press", "channel_id": town, "message_id": reply.Id, "data": "forged"})
		adminWS.expect(10*time.Second, "toast", "no such button")
	})

	step("privacy keeps plain messages from the bot", func(t *testing.T) {
		adminWS.send(map[string]string{"op": "post", "channel_id": town, "content": "just chatting " + tag})
		adminWS.expect(10*time.Second, "just chatting "+tag)
		adminWS.send(map[string]string{"op": "post", "channel_id": town, "content": "/ping " + tag})
		ev := botNext(t, c, "a message", postedBy(botID, func(s string) bool { return strings.HasSuffix(s, tag) }))
		if got := ev.GetMessagePosted().GetMessage().Content; got != "/ping "+tag {
			t.Fatalf("the bot got %q", got)
		}
	})

	step("a bot that was away gets what it missed", func(t *testing.T) {
		pts := c.Pts()
		_ = c.Close()
		want := []string{"/echo r1 " + tag, "/echo r2 " + tag, "/echo r3 " + tag}
		for _, s := range want {
			adminWS.send(map[string]string{"op": "post", "channel_id": town, "content": s})
			adminWS.expect(10*time.Second, s)
		}
		c, err = gochat.Dial(ctx, gochat.Options{URL: baseURL, Token: token, Pts: pts})
		if err != nil {
			t.Fatal(err)
		}
		missed := postedBy(botID, func(s string) bool { return strings.HasPrefix(s, "/echo r") && strings.HasSuffix(s, tag) })
		for _, s := range want {
			if got := botNext(t, c, s, missed).GetMessagePosted().GetMessage().Content; got != s {
				t.Fatalf("got %q for %q", got, s)
			}
		}
		select {
		case u := <-c.Updates():
			if u.Event != nil && missed(u.Event) {
				t.Fatalf("a missed message came twice: %v", u.Event)
			}
		case <-time.After(time.Second):
		}
	})

	step("the HTTP Bot API", func(t *testing.T) {
		if status, r := botHTTP(t, token, "getMe", struct{}{}); status != http.StatusOK || !r.OK || !strings.Contains(string(r.Result), name) {
			t.Fatalf("getMe: %d %+v", status, r)
		}
		if status, r := botHTTP(t, token, "sendMessage", map[string]string{"channel_id": town, "content": "over http " + tag}); status != http.StatusOK || !r.OK {
			t.Fatalf("sendMessage: %d %+v", status, r)
		}
		adminWS.expect(10*time.Second, "over http "+tag)
		if status, _ := botHTTP(t, token, "gochat.v1.TeamService/CreateInvite", map[string]string{"team_id": teamID}); status == http.StatusOK {
			t.Fatal("a bot made a call it may not")
		}
		if status, _ := botHTTP(t, "", "getMe", struct{}{}); status != http.StatusUnauthorized {
			t.Fatalf("getMe without a token: %d", status)
		}

		// A poll waits for the update that comes after it.
		offset := c.Pts() + 1
		go func() {
			time.Sleep(time.Second)
			b, _ := json.Marshal(map[string]string{"op": "post", "channel_id": town, "content": "/ping http " + tag})
			_ = adminWS.conn.Write(context.Background(), websocket.MessageText, b)
		}()
		found := false
		for range 5 {
			status, r := botHTTP(t, token, "getUpdates", map[string]int64{"offset": offset, "timeout": 10})
			if status != http.StatusOK || !r.OK {
				t.Fatalf("getUpdates: %d %+v", status, r)
			}
			var res struct {
				Updates []struct {
					Pts int64 `json:"pts,string"`
				} `json:"updates"`
			}
			if err := json.Unmarshal(r.Result, &res); err != nil {
				t.Fatalf("getUpdates: %v %s", err, r.Result)
			}
			for _, u := range res.Updates {
				offset = max(offset, u.Pts+1)
			}
			if strings.Contains(string(r.Result), "/ping http "+tag) {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("getUpdates never had the message")
		}
	})

	step("resetting the token stops the bot", func(t *testing.T) {
		_, page := admin.hx("/settings/bots/"+botID+"/token", url.Values{})
		m := botTokenRe.FindStringSubmatch(page)
		if m == nil {
			t.Fatalf("the reset showed no token: %s", page)
		}
		deadline := time.After(20 * time.Second)
	drain:
		for {
			select {
			case _, ok := <-c.Updates():
				if !ok {
					break drain
				}
			case <-deadline:
				t.Fatal("the bot's socket survived the reset")
			}
		}
		if err := c.Err(); err == nil || !strings.Contains(err.Error(), "4001") {
			t.Fatalf("the bot stopped with %v", err)
		}
		if status, _ := botHTTP(t, token, "getMe", struct{}{}); status != http.StatusUnauthorized {
			t.Fatalf("the old token: %d", status)
		}
		if status, _ := botHTTP(t, m[1], "getMe", struct{}{}); status != http.StatusOK {
			t.Fatalf("the new token: %d", status)
		}
		token = m[1]
	})

	step("deleting the bot ends its token", func(t *testing.T) {
		if _, page := admin.hx("/settings/bots/"+botID+"/delete", url.Values{}); !strings.Contains(page, "was deleted") {
			t.Fatalf("delete: %s", page)
		}
		if status, _ := botHTTP(t, token, "getMe", struct{}{}); status != http.StatusUnauthorized {
			t.Fatalf("a deleted bot's token: %d", status)
		}
	})
}
