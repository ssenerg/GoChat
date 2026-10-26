package channels

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/hub"
	"github.com/ssenerg/gochat/internal/services/users"
)

// forgetRepo holds one direct conversation's messages and records what
// deleting it does.
type forgetRepo struct {
	Repo
	mu       sync.Mutex
	members  []*models.ChannelMember
	messages []*models.Message
	cleared  map[gocql.UUID]gocql.UUID
	removed  []gocql.UUID
	freed    string
	gone     []gocql.UUID
	threads  []gocql.UUID
	deleted  bool
}

func (r *forgetRepo) GetUsers(_ context.Context, list []gocql.UUID) ([]*models.User, error) {
	out := make([]*models.User, len(list))
	for i, id := range list {
		out[i] = &models.User{ID: id, Username: id.String()[:8]}
	}
	return out, nil
}

func (r *forgetRepo) GetUser(_ context.Context, id gocql.UUID) (*models.User, error) {
	return &models.User{ID: id, Username: id.String()[:8]}, nil
}

func (r *forgetRepo) SetCleared(_ context.Context, userID, _, clearedID gocql.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleared[userID] = clearedID
	return nil
}

func (r *forgetRepo) ClearMentions(context.Context, gocql.UUID, gocql.UUID) error { return nil }

func (r *forgetRepo) DeleteDirectKey(_ context.Context, key string, _ gocql.UUID) error {
	r.freed = key
	return nil
}

func (r *forgetRepo) ListChannelMembers(context.Context, gocql.UUID) ([]*models.ChannelMember, error) {
	return r.members, nil
}

func (r *forgetRepo) RemoveChannelMember(_ context.Context, m *models.ChannelMember) error {
	r.removed = append(r.removed, m.UserID)
	return nil
}

func (r *forgetRepo) ListPins(context.Context, gocql.UUID, int) ([]*scylla.Pin, error) {
	return nil, nil
}

func (r *forgetRepo) ListMessages(_ context.Context, _, before gocql.UUID, limit int) ([]*models.Message, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*models.Message
	for _, m := range r.messages {
		if ids.IsZero(before) || ids.After(before, m.ID) {
			out = append(out, m)
		}
	}
	more := len(out) > limit
	return out[:min(len(out), limit)], more, nil
}

func (r *forgetRepo) ThreadContents(_ context.Context, _, rootID gocql.UUID) ([]*models.Message, error) {
	return []*models.Message{{ID: ids.NewTime(), RootID: rootID, Content: "a reply"}}, nil
}

func (r *forgetRepo) DeleteThread(_ context.Context, _, rootID gocql.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.threads = append(r.threads, rootID)
	return nil
}

func (r *forgetRepo) UnfollowThreads(context.Context, []gocql.UUID, gocql.UUID, gocql.UUID) error {
	return nil
}

func (r *forgetRepo) DeleteReactions(context.Context, gocql.UUID, gocql.UUID) error { return nil }

func (r *forgetRepo) DeleteMessage(_ context.Context, _, id gocql.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gone = append(r.gone, id)
	return nil
}

func (r *forgetRepo) DeleteChannel(context.Context, gocql.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = true
	return nil
}

// events collects what the conversation tells people.
type events struct {
	mu  sync.Mutex
	got []*pb.Event
}

func (e *events) take() []*pb.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.got
	e.got = nil
	return out
}

func direct(t *testing.T, repo *forgetRepo, x, y gocql.UUID) (*channel, *events) {
	t.Helper()
	engine, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	seen := &events{}
	sink := engine.SpawnFunc(func(c *actor.Context) {
		if d, ok := c.Message().(hub.Deliver); ok {
			seen.mu.Lock()
			seen.got = append(seen.got, d.Event)
			seen.mu.Unlock()
		}
	}, "hub")
	team := ids.New()
	ch := &models.Channel{ID: ids.New(), TeamID: team, Name: x.String() + ":" + y.String(), Kind: models.ChannelDirect}
	a := &channel{
		Deps: &Deps{
			Logger: zap.NewNop(), Cfg: &Config{OpTimeout: time.Second}, Repo: logged(repo), UpdatesCfg: testUpdates,
			Users: users.NewCache(repo, 10, time.Minute), Engine: engine, Hub: sink,
		},
		id: ch.ID, ch: ch, members: map[gocql.UUID]models.ChannelRole{}, cleared: map[gocql.UUID]gocql.UUID{},
		typing: map[gocql.UUID]time.Time{}, posters: map[gocql.UUID]*rate.Limiter{},
	}
	for _, id := range []gocql.UUID{x, y} {
		a.members[id] = models.ChannelRoleMember
		repo.members = append(repo.members, &models.ChannelMember{ChannelID: ch.ID, UserID: id, TeamID: team, Role: models.ChannelRoleMember})
	}
	a.snapshot()
	return a, seen
}

// wait returns the events once n arrived.
func (e *events) wait(t *testing.T, n int) []*pb.Event {
	t.Helper()
	var got []*pb.Event
	for deadline := time.Now().Add(2 * time.Second); len(got) < n && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		got = append(got, e.take()...)
	}
	if len(got) != n {
		t.Fatalf("%d events, want %d: %v", len(got), n, got)
	}
	return got
}

func TestShown(t *testing.T) {
	old, cleared := ids.NewTime(), ids.NewTime()
	newer := ids.NewTime()
	cases := []struct {
		name string
		m    *models.Message
		want bool
	}{
		{"nothing cleared", &models.Message{ID: old}, true},
		{"a message before", &models.Message{ID: old}, false},
		{"a message after", &models.Message{ID: newer}, true},
		{"a later reply to a cleared root", &models.Message{ID: newer, RootID: old}, false},
	}
	for i, c := range cases {
		mark := cleared
		if i == 0 {
			mark = gocql.UUID{}
		}
		if got := shown(c.m, mark); got != c.want {
			t.Errorf("%s: shown %v", c.name, got)
		}
	}
}

func TestClearingWaitsForTheNextMessage(t *testing.T) {
	me, peer := ids.New(), ids.New()
	repo := &forgetRepo{cleared: map[gocql.UUID]gocql.UUID{}}
	a, seen := direct(t, repo, me, peer)
	before := ids.NewTime()
	if _, err := a.deleteConversation(DeleteConversation{Target: Target{P: principalOf(me), ChannelID: a.id}}); err != nil {
		t.Fatal(err)
	}
	if ids.IsZero(repo.cleared[me]) || a.cleared[me] != repo.cleared[me] || !ids.After(repo.cleared[me], before) {
		t.Fatalf("cleared %v, held %v", repo.cleared, a.cleared)
	}
	if len(a.members) != 2 || repo.freed != "" {
		t.Fatal("clearing for one deleted the conversation")
	}
	left := seen.wait(t, 1)[0].GetChannelLeft()
	if left == nil || !left.Own || !left.Deleted {
		t.Fatalf("the caller was told %v", left)
	}
	// The first message after the clear, which follows one from before it,
	// lists it again; the next, which follows that one, does not.
	if joined := seen.wait(t, 0); len(joined) != 0 {
		t.Fatalf("listed again before a message: %v", joined)
	}
	a.reappear(before)
	if ev := seen.wait(t, 1)[0]; ev.GetChannelJoined() == nil {
		t.Fatalf("listed again with %v", ev)
	}
	a.reappear(ids.NewTime())
	time.Sleep(50 * time.Millisecond)
	if got := seen.take(); len(got) != 0 {
		t.Fatalf("listed again once more: %v", got)
	}
}

func TestDeletingForBoth(t *testing.T) {
	me, peer := ids.New(), ids.New()
	root, plain := ids.NewTime(), ids.NewTime()
	repo := &forgetRepo{cleared: map[gocql.UUID]gocql.UUID{}, messages: []*models.Message{
		{ID: plain, Content: "later"}, {ID: root, Content: "first", ReplyCount: 1},
	}}
	a, seen := direct(t, repo, me, peer)
	if _, err := a.deleteConversation(DeleteConversation{Target: Target{P: principalOf(me), ChannelID: a.id}, Everyone: true}); err != nil {
		t.Fatal(err)
	}
	if repo.freed != scylla.ConversationKey(a.ch.TeamID, a.ch.Name) {
		t.Fatalf("freed %q", repo.freed)
	}
	if len(a.members) != 0 || len(repo.removed) != 2 {
		t.Fatalf("members %v, removed %v", a.members, repo.removed)
	}
	own := map[bool]int{}
	for _, ev := range seen.wait(t, 2) {
		left := ev.GetChannelLeft()
		if left == nil || !left.Deleted {
			t.Fatalf("told %v", ev)
		}
		own[left.Own]++
	}
	if own[true] != 1 || own[false] != 1 {
		t.Fatalf("own %v", own)
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		repo.mu.Lock()
		done := repo.deleted
		repo.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the conversation was not deleted")
		}
	}
	if len(repo.gone) != 2 || len(repo.threads) != 1 || repo.threads[0] != root {
		t.Fatalf("deleted messages %v, threads %v", repo.gone, repo.threads)
	}
	if log := a.Repo.(*withLog); !log.dropped {
		t.Fatal("the conversation's updates were kept")
	}
	if _, err := a.deleteConversation(DeleteConversation{Target: Target{P: principalOf(me), ChannelID: a.id}}); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("deleting a deleted conversation: %v", err)
	}
}

func TestOnlyDirectConversationsAreDeleted(t *testing.T) {
	me := ids.New()
	for _, kind := range []models.ChannelKind{models.ChannelGroup, models.ChannelPublic} {
		a := &channel{ch: &models.Channel{Kind: kind}, members: map[gocql.UUID]models.ChannelRole{me: models.ChannelRoleMember}}
		if _, err := a.deleteConversation(DeleteConversation{Target: Target{P: principalOf(me)}}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
			t.Errorf("%s: %v", kind, err)
		}
	}
}

func principalOf(id gocql.UUID) *models.Principal {
	return &models.Principal{UserID: id, Role: models.SystemRoleUser}
}
