package push

import (
	"context"
	"slices"
	"strings"
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
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/hub"
	"github.com/ssenerg/gochat/internal/services/users"
)

// fakeRepo keeps devices, sessions, people and mutes in memory.
type fakeRepo struct {
	mu       sync.Mutex
	devices  map[string]*models.PushDevice
	holders  map[string]string
	sessions map[string]*models.Session
	users    map[gocql.UUID]*models.User
	muted    map[string]bool
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		devices: map[string]*models.PushDevice{}, holders: map[string]string{},
		sessions: map[string]*models.Session{}, users: map[gocql.UUID]*models.User{}, muted: map[string]bool{},
	}
}

func deviceKey(userID gocql.UUID, hash []byte) string { return userID.String() + "/" + string(hash) }
func tokenKey(platform, token string) string          { return platform + "/" + token }

func (f *fakeRepo) PutPushDevice(_ context.Context, d *models.PushDevice, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *d
	f.devices[deviceKey(d.UserID, d.SessionHash)] = &c
	f.holders[tokenKey(d.Platform, d.Token)] = deviceKey(d.UserID, d.SessionHash)
	return nil
}

func (f *fakeRepo) GetPushDevice(_ context.Context, userID gocql.UUID, hash []byte) (*models.PushDevice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.devices[deviceKey(userID, hash)]
	if !ok {
		return nil, ierrors.NotFound("not found")
	}
	c := *d
	return &c, nil
}

func (f *fakeRepo) ListPushDevices(_ context.Context, userID gocql.UUID) ([]*models.PushDevice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*models.PushDevice
	for _, d := range f.devices {
		if d.UserID == userID {
			c := *d
			out = append(out, &c)
		}
	}
	return out, nil
}

func (f *fakeRepo) PushTokenHolder(_ context.Context, platform, token string) (gocql.UUID, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key, ok := f.holders[tokenKey(platform, token)]
	if !ok {
		return gocql.UUID{}, nil, nil
	}
	d, ok := f.devices[key]
	if !ok {
		return gocql.UUID{}, nil, nil
	}
	return d.UserID, d.SessionHash, nil
}

func (f *fakeRepo) DeletePushDevice(_ context.Context, d *models.PushDevice) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.devices, deviceKey(d.UserID, d.SessionHash))
	delete(f.holders, tokenKey(d.Platform, d.Token))
	return nil
}

func (f *fakeRepo) DeletePushToken(_ context.Context, platform, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.holders, tokenKey(platform, token))
	return nil
}

func (f *fakeRepo) GetSession(_ context.Context, hash []byte) (*models.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[string(hash)]
	if !ok {
		return nil, ierrors.NotFound("not found")
	}
	c := *s
	return &c, nil
}

func (f *fakeRepo) GetUserIDByUsername(_ context.Context, name string) (gocql.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, u := range f.users {
		if u.Username == name {
			return id, nil
		}
	}
	return gocql.UUID{}, ierrors.NotFound("not found")
}

func (f *fakeRepo) IsMuted(_ context.Context, userID, channelID gocql.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.muted[userID.String()+"/"+channelID.String()], nil
}

func (f *fakeRepo) GetUser(_ context.Context, id gocql.UUID) (*models.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return nil, ierrors.NotFound("not found")
	}
	c := *u
	return &c, nil
}

func (f *fakeRepo) GetUsers(ctx context.Context, list []gocql.UUID) ([]*models.User, error) {
	var out []*models.User
	for _, id := range list {
		if u, err := f.GetUser(ctx, id); err == nil {
			out = append(out, u)
		}
	}
	return out, nil
}

// fakeProvider keeps what it was asked to push, and turns away the dead
// tokens.
type fakeProvider struct {
	mu   sync.Mutex
	sent []delivery
	dead map[string]bool
}

type delivery struct {
	token string
	n     *Notice
}

func (p *fakeProvider) Send(_ context.Context, d *models.PushDevice, n *Notice) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dead[d.Token] {
		return ErrDeadToken
	}
	p.sent = append(p.sent, delivery{token: d.Token, n: n})
	return nil
}

// take returns, and clears, the pushes so far, ordered by token.
func (p *fakeProvider) take() []delivery {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.sent
	p.sent = nil
	slices.SortFunc(out, func(a, b delivery) int { return strings.Compare(a.token, b.token) })
	return out
}

// stream is a gateway's stream that goes nowhere.
type stream struct{}

func (stream) Send(*pb.ConnectResponse) error { return nil }
func (stream) Close() error                   { return nil }

type fixture struct {
	t      *testing.T
	repo   *fakeRepo
	fcm    *fakeProvider
	apns   *fakeProvider
	deps   *Deps
	engine *actor.Engine
	pid    *actor.PID
	conns  uint64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	engine, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	repo := newFakeRepo()
	hubPID := engine.Spawn(hub.New(zap.NewNop(), &hub.Config{GatewayQueueLimit: 1024, PresenceGrace: time.Millisecond}), "hub")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := actors.Ask[hub.Registered](ctx, engine, hubPID, hub.RegisterGateway{ID: "gw", Stream: stream{}}); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, repo: repo, fcm: &fakeProvider{dead: map[string]bool{}}, apns: &fakeProvider{dead: map[string]bool{}}, engine: engine}
	f.deps = &Deps{
		Logger: zap.NewNop(), Repo: repo, Users: users.NewCache(repo, 100, time.Nanosecond), Engine: engine, Hub: hubPID,
		Cfg:         &Config{Workers: 2, OpTimeout: 5 * time.Second, SendTimeout: time.Second, Excerpts: true},
		Providers:   map[string]Provider{models.PushFCM: f.fcm, models.PushAPNs: f.apns},
		IdleTimeout: 7 * 24 * time.Hour,
	}
	f.pid = engine.Spawn(Pool(f.deps), "push")
	t.Cleanup(func() {
		<-engine.Poison(f.pid).Done()
		<-engine.Poison(hubPID).Done()
	})
	return f
}

func ask[T any](f *fixture, msg any) (T, error) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return actors.Ask[T](ctx, f.engine, f.pid, msg)
}

// person makes a user, signed in on one session.
func (f *fixture) person(name string) *models.Principal {
	f.t.Helper()
	u := &models.User{ID: ids.New(), Username: name, DisplayName: strings.ToUpper(name[:1]) + name[1:]}
	f.repo.mu.Lock()
	f.repo.users[u.ID] = u
	f.repo.mu.Unlock()
	return f.session(u.ID)
}

func (f *fixture) session(userID gocql.UUID) *models.Principal {
	now := time.Now()
	hash := secure.Hash(secure.Token())
	f.repo.mu.Lock()
	f.repo.sessions[string(hash)] = &models.Session{TokenHash: hash, UserID: userID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	f.repo.mu.Unlock()
	return &models.Principal{UserID: userID, SessionHash: hash, SessionID: secure.PublicID(hash), ExpiresAt: now.Add(time.Hour)}
}

func fcmToken(name string) string                        { return "fcm:" + name + strings.Repeat("x", 30) }
func apnsToken(seed byte) string                         { return strings.Repeat(string("0123456789abcdef"[seed%16]), 64) }
func (f *fixture) user(p *models.Principal) *models.User { return f.repo.users[p.UserID] }

func (f *fixture) register(p *models.Principal, platform pb.PushPlatform, token string) {
	f.t.Helper()
	on, err := ask[Registered](f, Register{P: p, Platform: platform, Token: token})
	if err != nil || !on {
		f.t.Fatalf("register: %v %v", on, err)
	}
}

// connect opens a connection for p's user, as an open page or app does.
func (f *fixture) connect(p *models.Principal) {
	f.conns++
	f.engine.Send(f.deps.Hub, hub.Attach{GatewayID: "gw", ConnID: f.conns, User: &pb.User{Id: p.UserID.String()}, SessionID: p.SessionID, ExpiresAt: p.ExpiresAt})
}

// post hands push a message, as a channel's actor does, and waits for it
// to be pushed.
func (f *fixture) post(msg Posted) {
	f.t.Helper()
	(&worker{Deps: f.deps}).posted(msg)
}

func members(ps ...*models.Principal) hub.Members {
	out := hub.Members{}
	for _, p := range ps {
		out[p.UserID.String()] = struct{}{}
	}
	return out
}

func message(author *models.User, channelID gocql.UUID, content string) *pb.Message {
	return &pb.Message{
		Id: ids.NewTime().String(), ChannelId: channelID.String(), Content: content,
		Author: &pb.User{Id: author.ID.String(), Username: author.Username, DisplayName: author.DisplayName},
	}
}

func tokens(list []delivery) []string {
	out := make([]string, len(list))
	for i, d := range list {
		out[i] = d.token
	}
	return out
}

func TestRegisterKeepsOneDevicePerSession(t *testing.T) {
	f := newFixture(t)
	ann := f.person("ann")
	f.register(ann, pb.PushPlatform_PUSH_PLATFORM_FCM, fcmToken("a1"))
	f.register(ann, pb.PushPlatform_PUSH_PLATFORM_FCM, fcmToken("a2"))
	list, _ := f.repo.ListPushDevices(context.Background(), ann.UserID)
	if len(list) != 1 || list[0].Token != fcmToken("a2") || list[0].Sandbox {
		t.Fatalf("devices after a new token: %+v", list)
	}
	if _, ok := f.repo.holders[tokenKey(models.PushFCM, fcmToken("a1"))]; ok {
		t.Fatal("the old token is still held")
	}

	// The device signs in again, as Bob: Ann's session loses it.
	bob := f.person("bob")
	f.register(bob, pb.PushPlatform_PUSH_PLATFORM_FCM, fcmToken("a2"))
	if list, _ := f.repo.ListPushDevices(context.Background(), ann.UserID); len(list) != 0 {
		t.Fatalf("ann kept the device bob signed in on: %+v", list)
	}

	// APNs takes the sandbox; FCM ignores it.
	on, err := ask[Registered](f, Register{P: ann, Platform: pb.PushPlatform_PUSH_PLATFORM_APNS, Token: apnsToken(1), Sandbox: true})
	if err != nil || !on {
		t.Fatal(on, err)
	}
	if d, _ := f.repo.GetPushDevice(context.Background(), ann.UserID, ann.SessionHash); d == nil || d.Platform != models.PushAPNs || !d.Sandbox {
		t.Fatalf("apns device: %+v", d)
	}

	if _, err := ask[Done](f, Unregister{P: ann}); err != nil {
		t.Fatal(err)
	}
	if list, _ := f.repo.ListPushDevices(context.Background(), ann.UserID); len(list) != 0 {
		t.Fatalf("unregister kept %+v", list)
	}
}

func TestRegisterRefuses(t *testing.T) {
	f := newFixture(t)
	ann := f.person("ann")
	for _, c := range []struct {
		platform pb.PushPlatform
		token    string
	}{
		{pb.PushPlatform_PUSH_PLATFORM_UNSPECIFIED, fcmToken("a")},
		{pb.PushPlatform_PUSH_PLATFORM_FCM, "short"},
		{pb.PushPlatform_PUSH_PLATFORM_FCM, fcmToken("a") + " "},
		{pb.PushPlatform_PUSH_PLATFORM_APNS, apnsToken(1)[:63]},
		{pb.PushPlatform_PUSH_PLATFORM_APNS, strings.ToUpper(strings.Repeat("ab", 32))},
	} {
		if _, err := ask[Registered](f, Register{P: ann, Platform: c.platform, Token: c.token}); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
			t.Errorf("%v %q: %v", c.platform, c.token, err)
		}
	}
	bot := *ann
	bot.Bot = true
	if _, err := ask[Registered](f, Register{P: &bot, Platform: pb.PushPlatform_PUSH_PLATFORM_FCM, Token: fcmToken("b")}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Errorf("a bot registered: %v", err)
	}

	// A platform without credentials keeps nothing.
	delete(f.deps.Providers, models.PushAPNs)
	on, err := ask[Registered](f, Register{P: ann, Platform: pb.PushPlatform_PUSH_PLATFORM_APNS, Token: apnsToken(2)})
	if err != nil || on {
		t.Fatalf("apns without credentials: %v %v", on, err)
	}
	if list, _ := f.repo.ListPushDevices(context.Background(), ann.UserID); len(list) != 0 {
		t.Fatalf("kept %+v", list)
	}
}

func TestForgetDropsRevokedSessionsDevices(t *testing.T) {
	f := newFixture(t)
	ann := f.person("ann")
	phone, tablet := ann, f.session(ann.UserID)
	f.register(phone, pb.PushPlatform_PUSH_PLATFORM_FCM, fcmToken("phone"))
	f.register(tablet, pb.PushPlatform_PUSH_PLATFORM_APNS, apnsToken(3))
	bob := f.person("bob")
	f.register(bob, pb.PushPlatform_PUSH_PLATFORM_FCM, fcmToken("bob"))

	forget := func(msg Forget) {
		f.engine.Send(f.pid, msg)
		// A request after it waits for it on every worker.
		for range f.deps.Cfg.Workers {
			if _, err := ask[Done](f, Unregister{P: f.person("x")}); err != nil {
				t.Fatal(err)
			}
		}
	}
	forget(Forget{UserID: ann.UserID, Hashes: [][]byte{phone.SessionHash}})
	list, _ := f.repo.ListPushDevices(context.Background(), ann.UserID)
	if len(list) != 1 || list[0].Platform != models.PushAPNs {
		t.Fatalf("after revoking the phone: %+v", list)
	}
	forget(Forget{UserID: ann.UserID, All: true})
	if list, _ := f.repo.ListPushDevices(context.Background(), ann.UserID); len(list) != 0 {
		t.Fatalf("after revoking all: %+v", list)
	}
	if list, _ := f.repo.ListPushDevices(context.Background(), bob.UserID); len(list) != 1 {
		t.Fatalf("bob lost his device: %+v", list)
	}
}

func TestWhoIsPushed(t *testing.T) {
	f := newFixture(t)
	ann, bob, cat, dan := f.person("ann"), f.person("bob"), f.person("cat"), f.person("dan")
	f.register(bob, pb.PushPlatform_PUSH_PLATFORM_FCM, fcmToken("bob"))
	f.register(cat, pb.PushPlatform_PUSH_PLATFORM_APNS, apnsToken(4))
	f.register(dan, pb.PushPlatform_PUSH_PLATFORM_FCM, fcmToken("dan"))
	team, channel, dm := ids.New(), ids.New(), ids.New()
	all := members(ann, bob, cat, dan)
	inChannel := func(content string) Posted {
		return Posted{Message: message(f.user(ann), channel, content), TeamID: team, ChannelName: "general", Members: all}
	}
	pushed := func() []string { return append(tokens(f.fcm.take()), tokens(f.apns.take())...) }

	// A direct message: the other person, never the author.
	f.post(Posted{Message: message(f.user(ann), dm, "hi bob"), TeamID: team, Conversation: true, Members: members(ann, bob)})
	got := f.fcm.take()
	if len(got) != 1 || got[0].token != fcmToken("bob") {
		t.Fatalf("direct message pushed to %v", tokens(got))
	}
	n := got[0].n
	if n.Title != "Ann" || n.Body != "hi bob" || n.Thread != dm.String() ||
		n.Data["channel_id"] != dm.String() || n.Data["team_id"] != team.String() || n.Data["root_id"] != "" {
		t.Fatalf("direct message's push: %+v", n)
	}

	// A channel message pushes to no one, a mention to whom it names.
	f.post(inChannel("hello all"))
	if got := pushed(); len(got) != 0 {
		t.Fatalf("a plain message pushed to %v", got)
	}
	f.post(inChannel("@cat look, and @nobody"))
	got = f.apns.take()
	if len(got) != 1 || got[0].n.Title != "Ann in #general" || len(f.fcm.take()) != 0 {
		t.Fatalf("a mention pushed to %v", tokens(got))
	}
	f.post(inChannel("@channel standup"))
	if got := pushed(); !slices.Equal(got, []string{fcmToken("bob"), fcmToken("dan"), apnsToken(4)}) {
		t.Fatalf("@channel pushed to %v", got)
	}
	f.post(inChannel("@here standup"))
	if got := pushed(); len(got) != 0 {
		t.Fatalf("@here pushed to %v", got)
	}
	// A mention of someone outside the channel pushes to no one.
	f.post(Posted{Message: message(f.user(ann), channel, "@dan"), TeamID: team, ChannelName: "general", Members: members(ann, bob)})
	if got := pushed(); len(got) != 0 {
		t.Fatalf("a mention outside the channel pushed to %v", got)
	}

	// A reply: the thread's people.
	reply := message(f.user(ann), channel, "agreed")
	reply.RootId = ids.NewTime().String()
	f.post(Posted{Message: reply, TeamID: team, ChannelName: "general", Members: all, Participants: []string{dan.UserID.String()}})
	got = f.fcm.take()
	if len(got) != 1 || got[0].token != fcmToken("dan") || got[0].n.Title != "Ann replied in #general" || got[0].n.Data["root_id"] != reply.RootId {
		t.Fatalf("a reply pushed %+v", got)
	}

	// Someone with a connection open sees it there.
	f.connect(bob)
	f.post(inChannel("@channel again"))
	if got := pushed(); !slices.Equal(got, []string{fcmToken("dan"), apnsToken(4)}) {
		t.Fatalf("with bob connected, pushed to %v", got)
	}
}

func TestPushRespectsMuteAndDND(t *testing.T) {
	f := newFixture(t)
	ann, bob := f.person("ann"), f.person("bob")
	f.register(bob, pb.PushPlatform_PUSH_PLATFORM_FCM, fcmToken("bob"))
	dm := ids.New()
	send := func() int {
		f.post(Posted{Message: message(f.user(ann), dm, "hi"), Conversation: true, Members: members(ann, bob)})
		return len(f.fcm.take())
	}
	if send() != 1 {
		t.Fatal("not pushed")
	}

	f.repo.muted[bob.UserID.String()+"/"+dm.String()] = true
	if send() != 0 {
		t.Fatal("pushed in a muted conversation")
	}
	delete(f.repo.muted, bob.UserID.String()+"/"+dm.String())

	f.repo.users[bob.UserID].Status = "dnd"
	if send() != 0 {
		t.Fatal("pushed while bob does not want to be disturbed")
	}
	f.repo.users[bob.UserID].DNDUntil = time.Now().Add(time.Hour)
	if send() != 0 {
		t.Fatal("pushed during an hour of do not disturb")
	}
	f.repo.users[bob.UserID].DNDUntil = time.Now().Add(-time.Minute)
	if send() != 1 {
		t.Fatal("not pushed after do not disturb ended")
	}
	f.repo.users[bob.UserID].Status = "away"
	if send() != 1 {
		t.Fatal("not pushed while away")
	}
}

func TestPushContent(t *testing.T) {
	f := newFixture(t)
	ann, bob := f.person("ann"), f.person("bob")
	f.register(bob, pb.PushPlatform_PUSH_PLATFORM_FCM, fcmToken("bob"))
	dm := ids.New()
	long := strings.Repeat("word ", 100)
	send := func(m *pb.Message) *Notice {
		t.Helper()
		f.post(Posted{Message: m, Conversation: true, Members: members(ann, bob)})
		got := f.fcm.take()
		if len(got) != 1 {
			t.Fatalf("pushed %d", len(got))
		}
		return got[0].n
	}

	n := send(message(f.user(ann), dm, long))
	if r := []rune(n.Body); len(r) != excerptRunes+1 || !strings.HasSuffix(n.Body, "…") || strings.Contains(n.Body, "  ") {
		t.Fatalf("a long message's push says %q", n.Body)
	}
	if n.Data["team_id"] != "" {
		t.Fatalf("a conversation of no team names one: %+v", n.Data)
	}
	voice := message(f.user(ann), dm, "")
	voice.Attachments = []*pb.Attachment{{Voice: true}}
	if n := send(voice); n.Body != "Sent a voice message" {
		t.Fatalf("a voice message's push says %q", n.Body)
	}

	f.deps.Cfg.Excerpts = false
	if n := send(message(f.user(ann), dm, "the secret plan")); n.Body != "New message" || n.Title != "Ann" {
		t.Fatalf("without excerpts the push says %q: %q", n.Title, n.Body)
	}
}

func TestDeadDevicesAreForgotten(t *testing.T) {
	f := newFixture(t)
	ann, bob := f.person("ann"), f.person("bob")
	tablet := f.session(bob.UserID)
	stale := f.session(bob.UserID)
	f.register(bob, pb.PushPlatform_PUSH_PLATFORM_FCM, fcmToken("phone"))
	f.register(tablet, pb.PushPlatform_PUSH_PLATFORM_APNS, apnsToken(5))
	f.register(stale, pb.PushPlatform_PUSH_PLATFORM_FCM, fcmToken("stale"))
	dm := ids.New()

	// The provider no longer knows the phone, and the stale session went
	// unused past the idle timeout.
	f.fcm.dead[fcmToken("phone")] = true
	f.repo.sessions[string(stale.SessionHash)].CreatedAt = time.Now().Add(-8 * 24 * time.Hour)
	f.post(Posted{Message: message(f.user(ann), dm, "hi"), Conversation: true, Members: members(ann, bob)})
	if got := f.apns.take(); len(got) != 1 || len(f.fcm.take()) != 0 {
		t.Fatalf("pushed to %v", tokens(got))
	}
	list, _ := f.repo.ListPushDevices(context.Background(), bob.UserID)
	if len(list) != 1 || list[0].Platform != models.PushAPNs {
		t.Fatalf("devices left: %+v", list)
	}

	// A session that ended loses its device too.
	delete(f.repo.sessions, string(tablet.SessionHash))
	f.post(Posted{Message: message(f.user(ann), dm, "hi"), Conversation: true, Members: members(ann, bob)})
	if got := f.apns.take(); len(got) != 0 {
		t.Fatalf("pushed to an ended session: %v", tokens(got))
	}
	if list, _ := f.repo.ListPushDevices(context.Background(), bob.UserID); len(list) != 0 {
		t.Fatalf("devices left: %+v", list)
	}
}
