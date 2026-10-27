package voice

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/hub"
)

type fakeRepo struct{ team, member gocql.UUID }

func (r *fakeRepo) GetTeamMember(_ context.Context, teamID, userID gocql.UUID) (*models.TeamMember, error) {
	if teamID == r.team && userID == r.member {
		return &models.TeamMember{TeamID: teamID, UserID: userID}, nil
	}
	return nil, ierrors.NotFound("no")
}

func (r *fakeRepo) ListTeamMembers(context.Context, gocql.UUID) ([]*scylla.TeamMemberEntry, error) {
	return []*scylla.TeamMemberEntry{{TeamMember: models.TeamMember{UserID: r.member}}}, nil
}

// fakeStream records what the voice actor sends a voice server.
type fakeStream struct {
	mu  sync.Mutex
	out []*pb.VoiceServerServiceConnectResponse
}

func (s *fakeStream) Send(r *pb.VoiceServerServiceConnectResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out = append(s.out, r)
	return nil
}

func (s *fakeStream) Close() error { return nil }

// wait returns the first frame sent that match accepts.
func (s *fakeStream) wait(t *testing.T, match func(*pb.VoiceServerServiceConnectResponse) bool) *pb.VoiceServerServiceConnectResponse {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		for _, r := range s.out {
			if match(r) {
				s.mu.Unlock()
				return r
			}
		}
		s.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the voice server got no such frame")
	return nil
}

// fakeHub records the voice changes delivered.
type fakeHub struct {
	mu      sync.Mutex
	updates []*pb.VoiceChannelUpdate
}

func (h *fakeHub) Receive(c *actor.Context) {
	if d, ok := c.Message().(hub.Deliver); ok {
		h.mu.Lock()
		h.updates = append(h.updates, d.Event.GetVoiceChannelUpdate())
		h.mu.Unlock()
	}
}

// last returns the newest update of a channel, waiting for one at least
// version.
func (h *fakeHub) last(t *testing.T, channelID gocql.UUID, version uint64) *pb.VoiceChannelState {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		var best *pb.VoiceChannelState
		for _, u := range h.updates {
			if u.Channel.ChannelId == channelID.String() && (best == nil || u.Channel.Version > best.Version) {
				best = u.Channel
			}
		}
		h.mu.Unlock()
		if best != nil && best.Version >= version {
			return best
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no update of version %d", version)
	return nil
}

type rig struct {
	t      *testing.T
	engine *actor.Engine
	voice  *actor.PID
	hub    *fakeHub
	team   gocql.UUID
	stream *fakeStream
	reqs   uint64
}

// newRig starts a voice actor whose team has one member, member.
func newRig(t *testing.T, maxPerChannel int, member gocql.UUID) *rig {
	engine, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, engine: engine, hub: &fakeHub{}, team: ids.New(), stream: &fakeStream{}}
	hubPID := engine.Spawn(func() actor.Receiver { return r.hub }, "hub")
	cfg := &Config{TokenTTL: time.Minute, MaxPerChannel: maxPerChannel, MaxPendingPerUser: 3, ServerQueueLimit: 64, MembersTTL: time.Minute, OpTimeout: time.Second}
	r.voice = engine.Spawn(New(&Deps{Logger: zap.NewNop(), Cfg: cfg, Repo: &fakeRepo{team: r.team, member: member}, Engine: engine, Hub: hubPID}), "voice")
	return r
}

func (r *rig) ask(msg any) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return actors.Ask[any](ctx, r.engine, r.voice, msg)
}

func (r *rig) serverUp(key string) {
	if _, err := r.ask(ServerUp{Key: key, ID: key, Endpoint: "wss://" + key + "/rtc", Stream: r.stream}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) user() *models.Principal {
	return &models.Principal{UserID: ids.New(), Role: models.SystemRoleUser, SessionID: ids.New().String()}
}

func (r *rig) join(p *models.Principal, channelID gocql.UUID) (*Ticket, error) {
	out, err := r.ask(Join{P: p, User: &pb.User{Id: p.UserID.String()}, Channel: &models.Channel{ID: channelID, TeamID: r.team, Kind: models.ChannelVoice}})
	if err != nil {
		return nil, err
	}
	return out.(*Ticket), nil
}

// connect joins and claims the token as voice server key, returning the
// claim's answer.
func (r *rig) connect(p *models.Principal, channelID gocql.UUID, key string) *pb.VoiceTokenClaimed {
	r.t.Helper()
	ticket, err := r.join(p, channelID)
	if err != nil {
		r.t.Fatal(err)
	}
	return r.claim(key, ticket.Token)
}

func (r *rig) claim(key, token string) *pb.VoiceTokenClaimed {
	r.t.Helper()
	r.reqs++
	id := r.reqs
	r.engine.Send(r.voice, Claim{Key: key, RequestID: id, Token: token})
	return r.stream.wait(r.t, func(f *pb.VoiceServerServiceConnectResponse) bool {
		return f.GetClaimed().GetRequestId() == id
	}).GetClaimed()
}

func (r *rig) disconnected(session string) *pb.VoiceDisconnect {
	r.t.Helper()
	return r.stream.wait(r.t, func(f *pb.VoiceServerServiceConnectResponse) bool {
		return f.GetDisconnect().GetSession() == session
	}).GetDisconnect()
}

func users(s *pb.VoiceChannelState) []string {
	var out []string
	for _, st := range s.States {
		out = append(out, st.User.Id)
	}
	return out
}

func TestJoinNeedsAVoiceServer(t *testing.T) {
	r := newRig(t, 10, gocql.UUID{})
	if _, err := r.join(r.user(), ids.New()); !ierrors.Is(err, ierrors.CodeUnavailable) {
		t.Fatalf("joined without a voice server: %v", err)
	}
}

func TestClaimJoinsAndTokensWorkOnce(t *testing.T) {
	r := newRig(t, 10, gocql.UUID{})
	r.serverUp("vs1")
	room, p := ids.New(), r.user()
	ticket, err := r.join(p, room)
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Endpoint != "wss://vs1/rtc" {
		t.Fatalf("endpoint %q", ticket.Endpoint)
	}
	got := r.claim("vs1", ticket.Token)
	if got.Error != "" || got.User.GetId() != p.UserID.String() || got.ChannelId != room.String() || got.Session == "" {
		t.Fatalf("claim: %+v", got)
	}
	if s := r.hub.last(t, room, 1); len(s.States) != 1 || s.States[0].User.Id != p.UserID.String() {
		t.Fatalf("update: %+v", s)
	}
	if again := r.claim("vs1", ticket.Token); again.Error == "" {
		t.Fatal("a token was claimed twice")
	}
	if bad := r.claim("vs1", "not-a-token"); bad.Error == "" {
		t.Fatal("a malformed token was claimed")
	}
}

func TestTokensBelongToTheirServer(t *testing.T) {
	r := newRig(t, 10, gocql.UUID{})
	r.serverUp("vs1")
	ticket, err := r.join(r.user(), ids.New())
	if err != nil {
		t.Fatal(err)
	}
	r.serverUp("vs2")
	if got := r.claim("vs2", ticket.Token); got.Error == "" {
		t.Fatal("another voice server claimed the token")
	}
}

func TestOneChannelAtATime(t *testing.T) {
	r := newRig(t, 10, gocql.UUID{})
	r.serverUp("vs1")
	a, b, p := ids.New(), ids.New(), r.user()
	first := r.connect(p, a, "vs1")
	second := r.connect(p, b, "vs1")
	if d := r.disconnected(first.Session); d.UserId != p.UserID.String() || d.ChannelId != a.String() {
		t.Fatalf("disconnect: %+v", d)
	}
	if s := r.hub.last(t, a, 2); len(s.States) != 0 {
		t.Fatalf("still in the first channel: %v", users(s))
	}
	if s := r.hub.last(t, b, 1); len(s.States) != 1 {
		t.Fatalf("not in the second channel: %v", users(s))
	}
	// Rejoining the same room replaces the earlier connection.
	r.connect(p, b, "vs1")
	r.disconnected(second.Session)
	if s := r.hub.last(t, b, 3); len(s.States) != 1 {
		t.Fatalf("rejoining doubled the person: %v", users(s))
	}
}

func TestPeerLeftNeedsTheSameSession(t *testing.T) {
	r := newRig(t, 10, gocql.UUID{})
	r.serverUp("vs1")
	room, p := ids.New(), r.user()
	got := r.connect(p, room, "vs1")
	r.engine.Send(r.voice, PeerLeft{Key: "vs1", UserID: p.UserID.String(), ChannelID: room.String(), Session: "stale"})
	r.engine.Send(r.voice, PeerLeft{Key: "vs2", UserID: p.UserID.String(), ChannelID: room.String(), Session: got.Session})
	time.Sleep(50 * time.Millisecond)
	if s := r.hub.last(t, room, 1); len(s.States) != 1 {
		t.Fatal("a stale report ended the talk")
	}
	r.engine.Send(r.voice, PeerLeft{Key: "vs1", UserID: p.UserID.String(), ChannelID: room.String(), Session: got.Session})
	if s := r.hub.last(t, room, 2); len(s.States) != 0 {
		t.Fatal("leaving did not end the talk")
	}
}

func TestPeerMediaShowsWhatTheVoiceServerForwards(t *testing.T) {
	r := newRig(t, 10, gocql.UUID{})
	r.serverUp("vs1")
	room, p := ids.New(), r.user()
	got := r.connect(p, room, "vs1")
	media := func(key, session string, video, stream bool) PeerMedia {
		return PeerMedia{Key: key, UserID: p.UserID.String(), ChannelID: room.String(), Session: session, Video: video, Stream: stream}
	}
	r.engine.Send(r.voice, media("vs1", "stale", true, true))
	r.engine.Send(r.voice, media("vs2", got.Session, true, true))
	r.engine.Send(r.voice, media("vs1", got.Session, true, false))
	if s := r.hub.last(t, room, 2); !s.States[0].SelfVideo || s.States[0].SelfStream {
		t.Fatalf("the camera does not show alone: %+v", s.States[0])
	}
	r.engine.Send(r.voice, media("vs1", got.Session, false, true))
	if s := r.hub.last(t, room, 3); s.States[0].SelfVideo || !s.States[0].SelfStream {
		t.Fatalf("the screen does not show alone: %+v", s.States[0])
	}
	// Muting keeps them.
	if _, err := r.ask(Update{UserID: p.UserID, Mute: true}); err != nil {
		t.Fatal(err)
	}
	if s := r.hub.last(t, room, 4); !s.States[0].SelfMute || !s.States[0].SelfStream {
		t.Fatalf("muting lost the screen: %+v", s.States[0])
	}
}

// A channel whose talk ended and started again keeps counting up: pages
// drop any state older than the one they show.
func TestVersionsOutliveTheRoom(t *testing.T) {
	r := newRig(t, 10, gocql.UUID{})
	r.serverUp("vs1")
	room, p, q := ids.New(), r.user(), r.user()
	r.connect(p, room, "vs1")
	if _, err := r.ask(Leave{UserID: p.UserID}); err != nil {
		t.Fatal(err)
	}
	if s := r.hub.last(t, room, 2); len(s.States) != 0 {
		t.Fatalf("still talking: %v", users(s))
	}
	r.connect(q, room, "vs1")
	if s := r.hub.last(t, room, 3); s.Version != 3 || len(s.States) != 1 || s.States[0].User.Id != q.UserID.String() {
		t.Fatalf("the new talk started over: version %d, %v", s.Version, users(s))
	}
}

func TestMuteKickRevokeAndDrop(t *testing.T) {
	r := newRig(t, 10, gocql.UUID{})
	r.serverUp("vs1")
	room := ids.New()
	a, b, c := r.user(), r.user(), r.user()
	ca, cb, cc := r.connect(a, room, "vs1"), r.connect(b, room, "vs1"), r.connect(c, room, "vs1")

	if _, err := r.ask(Update{UserID: a.UserID, Deaf: true}); err != nil {
		t.Fatal(err)
	}
	s := r.hub.last(t, room, 4)
	if !s.States[0].SelfDeaf || !s.States[0].SelfMute {
		t.Fatalf("deafened but not muted: %+v", s.States[0])
	}
	if _, err := r.ask(Update{UserID: ids.New()}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("updated someone not talking: %v", err)
	}

	if _, err := r.ask(Kick{ChannelID: ids.New(), UserID: b.UserID}); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("kicked from another channel: %v", err)
	}
	if _, err := r.ask(Kick{ChannelID: room, UserID: b.UserID}); err != nil {
		t.Fatal(err)
	}
	if d := r.disconnected(cb.Session); d.Reason == "" {
		t.Fatal("no reason given")
	}

	r.engine.Send(r.voice, RevokeSessions{UserID: c.UserID, SessionIDs: []string{"other"}})
	r.engine.Send(r.voice, RevokeSessions{UserID: c.UserID, SessionIDs: []string{c.SessionID}})
	r.disconnected(cc.Session)

	r.engine.Send(r.voice, DropUser{UserID: a.UserID, TeamID: ids.New()})
	r.engine.Send(r.voice, DropUser{UserID: a.UserID, TeamID: r.team})
	r.disconnected(ca.Session)
	if s := r.hub.last(t, room, 7); len(s.States) != 0 {
		t.Fatalf("left behind: %v", users(s))
	}
}

// Deactivating someone revokes every session they have, listed or not.
func TestRevokingAllSessionsEndsTheTalk(t *testing.T) {
	r := newRig(t, 10, gocql.UUID{})
	r.serverUp("vs1")
	room, p := ids.New(), r.user()
	got := r.connect(p, room, "vs1")
	r.engine.Send(r.voice, RevokeSessions{UserID: p.UserID, All: true})
	if d := r.disconnected(got.Session); d.Reason != "signed out" {
		t.Fatalf("reason %q", d.Reason)
	}
}

func TestFullChannels(t *testing.T) {
	r := newRig(t, 2, gocql.UUID{})
	r.serverUp("vs1")
	room := ids.New()
	a := r.user()
	r.connect(a, room, "vs1")
	r.connect(r.user(), room, "vs1")
	if _, err := r.join(r.user(), room); !ierrors.Is(err, ierrors.CodeResourceExhausted) {
		t.Fatalf("joined a full channel: %v", err)
	}
	// Who is inside may join again.
	if _, err := r.join(a, room); err != nil {
		t.Fatalf("could not rejoin: %v", err)
	}
}

func TestServerDownEndsItsTalk(t *testing.T) {
	r := newRig(t, 10, gocql.UUID{})
	r.serverUp("vs1")
	room := ids.New()
	r.connect(r.user(), room, "vs1")
	pendingTicket, err := r.join(r.user(), room)
	if err != nil {
		t.Fatal(err)
	}
	r.engine.Send(r.voice, ServerDown{Key: "vs1"})
	if s := r.hub.last(t, room, 2); len(s.States) != 0 {
		t.Fatal("talk outlived its voice server")
	}
	r.serverUp("vs2")
	if got := r.claim("vs2", pendingTicket.Token); got.Error == "" {
		t.Fatal("a token of the lost server was claimed")
	}
	if ticket, err := r.join(r.user(), room); err != nil || ticket.Endpoint != "wss://vs2/rtc" {
		t.Fatalf("the room did not move to the new server: %v %v", ticket, err)
	}
}

func TestListNeedsTheTeam(t *testing.T) {
	member := ids.New()
	r := newRig(t, 10, member)
	r.serverUp("vs1")
	room := ids.New()
	p := &models.Principal{UserID: member, Role: models.SystemRoleUser}
	r.connect(p, room, "vs1")
	if _, err := r.ask(List{P: r.user(), TeamID: r.team}); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("an outsider listed the team: %v", err)
	}
	out, err := r.ask(List{P: p, TeamID: r.team})
	if err != nil {
		t.Fatal(err)
	}
	list := out.([]*pb.VoiceChannelState)
	if len(list) != 1 || users(list[0])[0] != member.String() {
		t.Fatalf("list: %+v", list)
	}
	if out, err := r.ask(List{P: p, TeamID: ids.New()}); err == nil && len(out.([]*pb.VoiceChannelState)) > 0 {
		t.Fatal("listed another team's talk")
	}
	admin := &models.Principal{UserID: ids.New(), Role: models.SystemRoleAdmin}
	if _, err := r.ask(List{P: admin, TeamID: r.team}); err != nil {
		t.Fatalf("a system admin could not list: %v", err)
	}
}

func TestUnclaimedTokensAreCapped(t *testing.T) {
	r := newRig(t, 10, gocql.UUID{})
	r.serverUp("vs1")
	p, room := r.user(), ids.New()
	var first *Ticket
	for i := range 3 {
		ticket, err := r.join(p, room)
		if err != nil {
			t.Fatalf("join %d: %v", i, err)
		}
		if first == nil {
			first = ticket
		}
	}
	if _, err := r.join(p, room); !ierrors.Is(err, ierrors.CodeResourceExhausted) {
		t.Fatalf("a fourth unclaimed token: %v", err)
	}
	if _, err := r.join(r.user(), room); err != nil {
		t.Fatalf("someone else was held back: %v", err)
	}
	r.claim("vs1", first.Token)
	if _, err := r.join(p, room); err != nil {
		t.Fatalf("a claimed token still counted: %v", err)
	}
}

func TestExpiredSignInsEndTheTalk(t *testing.T) {
	r := newRig(t, 10, gocql.UUID{})
	r.serverUp("vs1")
	room := ids.New()
	p := r.user()
	p.ExpiresAt = time.Now().Add(100 * time.Millisecond)
	got := r.connect(p, room, "vs1")
	q := r.user()
	q.ExpiresAt = time.Now().Add(time.Hour)
	r.connect(q, room, "vs1")
	time.Sleep(150 * time.Millisecond)
	r.engine.Send(r.voice, sweep{})
	if d := r.disconnected(got.Session); d.Reason != "your sign-in expired" {
		t.Fatalf("reason %q", d.Reason)
	}
	if s := r.hub.last(t, room, 3); len(s.States) != 1 || s.States[0].User.Id != q.UserID.String() {
		t.Fatalf("left: %v", users(s))
	}
}
