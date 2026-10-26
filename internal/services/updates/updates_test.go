package updates

import (
	"context"
	"errors"
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
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/hub"
)

type memRepo struct {
	mu       sync.Mutex
	logs     map[gocql.UUID][]*scylla.LogEntry
	messages map[gocql.UUID]*models.Message
	users    map[gocql.UUID]*models.User
	failLoad bool
}

func (r *memRepo) Get(_ context.Context, id gocql.UUID) (*models.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u := r.users[id]; u != nil {
		return u, nil
	}
	return nil, ierrors.NotFound("not found")
}

func newMemRepo() *memRepo {
	return &memRepo{logs: map[gocql.UUID][]*scylla.LogEntry{}, messages: map[gocql.UUID]*models.Message{}, users: map[gocql.UUID]*models.User{}}
}

func (r *memRepo) GetMessage(_ context.Context, _, id gocql.UUID) (*models.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.messages[id]; m != nil {
		return m, nil
	}
	return nil, ierrors.NotFound("not found")
}

func (r *memRepo) GetReply(ctx context.Context, channelID, _, id gocql.UUID) (*models.Message, error) {
	return r.GetMessage(ctx, channelID, id)
}

func (r *memRepo) AccountPts(_ context.Context, userID gocql.UUID) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failLoad {
		return 0, errors.New("down")
	}
	log := r.logs[userID]
	if len(log) == 0 {
		return 0, nil
	}
	return log[len(log)-1].Pts, nil
}

func (r *memRepo) AppendAccountUpdate(_ context.Context, userID gocql.UUID, e *scylla.LogEntry, _ int64, _ time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs[userID] = append(r.logs[userID], e)
	return nil
}

func (r *memRepo) AccountUpdates(_ context.Context, userID gocql.UUID, after, upTo int64, limit int) ([]*scylla.LogEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*scylla.LogEntry
	for _, e := range r.logs[userID] {
		if e.Pts > after && e.Pts <= upTo && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

type sink struct {
	mu  sync.Mutex
	got []*pb.Event
}

func (s *sink) take(t *testing.T, n int) []*pb.Event {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		s.mu.Lock()
		if len(s.got) >= n {
			out := s.got
			s.got = nil
			s.mu.Unlock()
			return out
		}
		s.mu.Unlock()
	}
	t.Fatalf("fewer than %d events arrived", n)
	return nil
}

func start(t *testing.T, repo *memRepo) (*actor.Engine, *actor.PID, *sink) {
	t.Helper()
	engine, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	seen := &sink{}
	h := engine.SpawnFunc(func(c *actor.Context) {
		if d, ok := c.Message().(hub.Deliver); ok {
			seen.mu.Lock()
			seen.got = append(seen.got, d.Event)
			seen.mu.Unlock()
		}
	}, "hub")
	render := func(_ context.Context, _ gocql.UUID, list []*models.Message) ([]*pb.Message, error) {
		out := make([]*pb.Message, len(list))
		for i, m := range list {
			out[i] = &pb.Message{Id: m.ID.String(), Content: m.Content}
		}
		return out, nil
	}
	pid := engine.Spawn(New(&Deps{
		Logger: zap.NewNop(), Repo: repo, Engine: engine, Hub: h, Render: render, Users: repo,
		Cfg: &Config{Retention: time.Hour, IdleTimeout: time.Minute, OpTimeout: time.Second, DifferenceLimit: 2},
	}), "updates")
	return engine, pid, seen
}

func muted(id string) *pb.Event {
	return &pb.Event{Payload: &pb.Event_ChannelMuted{ChannelMuted: &pb.ChannelMuted{ChannelId: id, Muted: true}}}
}

func TestEachAccountNumbersItsOwn(t *testing.T) {
	repo := newMemRepo()
	engine, pid, seen := start(t, repo)
	alice, bob := ids.New(), ids.New()
	shared := muted("one")
	engine.Send(pid, Push{UserIDs: []gocql.UUID{alice, bob}, Event: shared})
	engine.Send(pid, Push{UserIDs: []gocql.UUID{alice}, Event: muted("two")})
	got := seen.take(t, 3)
	byNumber := map[int64]int{}
	for _, ev := range got {
		if ev.PtsCount != 1 || ev.PtsChannelId != "" {
			t.Fatalf("not an account's update: %v", ev)
		}
		byNumber[ev.Pts]++
	}
	if byNumber[1] != 2 || byNumber[2] != 1 {
		t.Fatalf("numbers %v", byNumber)
	}
	if shared.Pts != 0 {
		t.Fatal("the shared event was modified")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if pts, err := actors.Ask[int64](ctx, engine, pid, State{UserID: alice}); err != nil || pts != 2 {
		t.Fatalf("alice's state %d, %v", pts, err)
	}
}

func TestNumbersOutliveTheSequencer(t *testing.T) {
	repo := newMemRepo()
	user := ids.New()
	engine, pid, seen := start(t, repo)
	engine.Send(pid, Push{UserIDs: []gocql.UUID{user}, Event: muted("a")})
	seen.take(t, 1)
	<-engine.Poison(pid).Done()

	engine, pid, seen = start(t, repo)
	engine.Send(pid, Push{UserIDs: []gocql.UUID{user}, Event: muted("b")})
	if got := seen.take(t, 1); got[0].Pts != 2 {
		t.Fatalf("after a restart the next update is %d, want 2", got[0].Pts)
	}
}

func TestDifferencePagesAndReadsMessagesAsTheyAre(t *testing.T) {
	repo := newMemRepo()
	engine, pid, seen := start(t, repo)
	bot, channel := ids.New(), ids.New()
	kept := &models.Message{ID: ids.NewTime(), ChannelID: channel, Content: "kept"}
	gone := &models.Message{ID: ids.NewTime(), ChannelID: channel, Content: "gone"}
	repo.messages[kept.ID] = kept
	repo.messages[gone.ID] = gone
	for _, m := range []*models.Message{kept, gone} {
		engine.Send(pid, Push{UserIDs: []gocql.UUID{bot}, Event: &pb.Event{Payload: &pb.Event_MessagePosted{MessagePosted: &pb.MessagePosted{
			Message: &pb.Message{Id: m.ID.String(), ChannelId: channel.String(), Content: m.Content},
		}}}})
	}
	engine.Send(pid, Push{UserIDs: []gocql.UUID{bot}, Event: muted("x")})
	seen.take(t, 3)
	repo.mu.Lock()
	kept.Content = "kept, edited"
	delete(repo.messages, gone.ID)
	repo.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, err := actors.Ask[*Difference](ctx, engine, pid, GetDifference{UserID: bot, Pts: 0})
	if err != nil {
		t.Fatal(err)
	}
	// The limit is two: the first page ends at 2, with more to come.
	if d.Pts != 2 || !d.More || len(d.Updates) != 1 {
		t.Fatalf("first page: %+v", d)
	}
	if c := d.Updates[0].GetMessagePosted().GetMessage().GetContent(); c != "kept, edited" {
		t.Fatalf("replayed %q", c)
	}
	d, err = actors.Ask[*Difference](ctx, engine, pid, GetDifference{UserID: bot, Pts: d.Pts})
	if err != nil {
		t.Fatal(err)
	}
	if d.Pts != 3 || d.More || len(d.Updates) != 1 || d.Updates[0].GetChannelMuted() == nil {
		t.Fatalf("second page: %+v", d)
	}
}

func TestUnloadedAccountStillDelivers(t *testing.T) {
	repo := newMemRepo()
	repo.failLoad = true
	engine, pid, seen := start(t, repo)
	user := ids.New()
	engine.Send(pid, Push{UserIDs: []gocql.UUID{user}, Event: muted("a")})
	if got := seen.take(t, 1); got[0].Pts != 0 {
		t.Fatalf("an update numbered without its account's state: %v", got[0])
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := actors.Ask[int64](ctx, engine, pid, State{UserID: user}); !ierrors.Is(err, ierrors.CodeUnavailable) {
		t.Fatalf("state of an unloaded account: %v", err)
	}
	repo.mu.Lock()
	repo.failLoad = false
	repo.mu.Unlock()
	if pts, err := actors.Ask[int64](ctx, engine, pid, State{UserID: user}); err != nil || pts != 0 {
		t.Fatalf("state once loaded: %d, %v", pts, err)
	}
}

func TestAPressKeepsOnlyAReference(t *testing.T) {
	repo := newMemRepo()
	engine, pid, seen := start(t, repo)
	bot, channel := ids.New(), ids.New()
	m := &models.Message{ID: ids.NewTime(), ChannelID: channel, Content: "Agree?"}
	repo.messages[m.ID] = m
	press := func(id string) {
		engine.Send(pid, Push{UserIDs: []gocql.UUID{bot}, Event: &pb.Event{Payload: &pb.Event_CallbackQuery{CallbackQuery: &pb.CallbackQuery{
			Id: id, Data: "yes", Message: &pb.Message{Id: m.ID.String(), ChannelId: channel.String(), Content: m.Content},
		}}}})
	}
	press("q1")
	seen.take(t, 1)
	repo.mu.Lock()
	logged := repo.logs[bot][0]
	repo.mu.Unlock()
	if logged.MessageID != m.ID || strings.Contains(string(logged.Event), "Agree?") {
		t.Fatalf("logged %+v", logged)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d, err := actors.Ask[*Difference](ctx, engine, pid, GetDifference{UserID: bot, Pts: 0})
	if err != nil {
		t.Fatal(err)
	}
	if q := d.Updates[0].GetCallbackQuery(); q.GetId() != "q1" || q.GetData() != "yes" || q.GetMessage().GetContent() != "Agree?" {
		t.Fatalf("replayed %v", d.Updates[0])
	}
	// A press on a message deleted since is left out.
	press("q2")
	seen.take(t, 1)
	repo.mu.Lock()
	delete(repo.messages, m.ID)
	repo.mu.Unlock()
	if d, err = actors.Ask[*Difference](ctx, engine, pid, GetDifference{UserID: bot, Pts: 1}); err != nil || len(d.Updates) != 0 || d.Pts != 2 {
		t.Fatalf("a press on a deleted message: %+v, %v", d, err)
	}
}

func TestBotsThatCanNoLongerActGetNoUpdates(t *testing.T) {
	repo := newMemRepo()
	owner, gone := ids.New(), ids.New()
	live, deleted, orphan := ids.New(), ids.New(), ids.New()
	repo.users[owner] = &models.User{ID: owner}
	repo.users[gone] = &models.User{ID: gone, Deactivated: true}
	repo.users[live] = &models.User{ID: live, Bot: true, BotOwner: owner}
	repo.users[deleted] = &models.User{ID: deleted, Bot: true, BotOwner: owner, Deactivated: true}
	repo.users[orphan] = &models.User{ID: orphan, Bot: true, BotOwner: gone}
	engine, pid, seen := start(t, repo)
	engine.Send(pid, Push{UserIDs: []gocql.UUID{deleted, orphan, live}, Event: muted("x")})
	engine.Send(pid, Push{UserIDs: []gocql.UUID{live}, Event: muted("y")})
	seen.take(t, 2)
	time.Sleep(50 * time.Millisecond)
	seen.mu.Lock()
	extra := len(seen.got)
	seen.mu.Unlock()
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if extra != 0 || len(repo.logs[deleted]) != 0 || len(repo.logs[orphan]) != 0 || len(repo.logs[live]) != 2 {
		t.Fatalf("logged %d for the deleted bot, %d for the orphan, %d for the live one", len(repo.logs[deleted]), len(repo.logs[orphan]), len(repo.logs[live]))
	}
}
