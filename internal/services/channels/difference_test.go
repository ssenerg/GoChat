package channels

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/anthdm/hollywood/actor"
	stringadapter "github.com/casbin/casbin/v2/persist/string-adapter"
	"github.com/gocql/gocql"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/updates"
	"github.com/ssenerg/gochat/internal/services/users"
)

var testUpdates = &updates.Config{Retention: time.Hour, IdleTimeout: time.Minute, OpTimeout: time.Second, DifferenceLimit: 100}

// withLog keeps a channel's update log in memory over another fake.
type withLog struct {
	Repo
	mu      sync.Mutex
	log     []*scylla.LogEntry
	dropped bool
}

func logged(r Repo) *withLog { return &withLog{Repo: r} }

func (w *withLog) ChannelPts(context.Context, gocql.UUID) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.log) == 0 {
		return 0, nil
	}
	return w.log[len(w.log)-1].Pts, nil
}

func (w *withLog) AppendChannelUpdate(_ context.Context, _ gocql.UUID, e *scylla.LogEntry, _ int64, _ time.Duration) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.log = append(w.log, e)
	return nil
}

func (w *withLog) ChannelUpdates(_ context.Context, _ gocql.UUID, after, upTo int64, limit int) ([]*scylla.LogEntry, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []*scylla.LogEntry
	for _, e := range w.log {
		if e.Pts > after && e.Pts <= upTo && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (w *withLog) DeleteChannelUpdates(context.Context, gocql.UUID, int64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.log, w.dropped = nil, true
	return nil
}

// messageRepo holds a channel's messages.
type messageRepo struct {
	Repo
	mu       sync.Mutex
	messages map[gocql.UUID]*models.Message
}

func (r *messageRepo) GetMessage(_ context.Context, _, id gocql.UUID) (*models.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m := r.messages[id]; m != nil {
		c := *m
		return &c, nil
	}
	return nil, ierrors.NotFound("not found")
}

func (r *messageRepo) GetUser(_ context.Context, id gocql.UUID) (*models.User, error) {
	return &models.User{ID: id, Username: "u" + id.String()[:6]}, nil
}

func (r *messageRepo) GetUsers(_ context.Context, list []gocql.UUID) ([]*models.User, error) {
	out := make([]*models.User, len(list))
	for i, id := range list {
		out[i] = &models.User{ID: id, Username: "u" + id.String()[:6]}
	}
	return out, nil
}

func (r *messageRepo) GetAttachments(context.Context, []gocql.UUID) ([]*models.Attachment, error) {
	return nil, nil
}

func (r *messageRepo) ListReactions(context.Context, gocql.UUID, []gocql.UUID) ([]*scylla.Reaction, error) {
	return nil, nil
}

func newLoggedChannel(t *testing.T, kind models.ChannelKind) (*channel, *messageRepo, *withLog, gocql.UUID) {
	t.Helper()
	engine, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	sink := engine.SpawnFunc(func(*actor.Context) {}, "hub")
	repo := &messageRepo{messages: map[gocql.UUID]*models.Message{}}
	log := logged(repo)
	member := ids.New()
	ch := &models.Channel{ID: ids.New(), TeamID: ids.New(), Name: "general", Kind: kind}
	az, err := authz.New(zap.NewNop(), stringadapter.NewAdapter("p, channel_member, message, read"))
	if err != nil {
		t.Fatal(err)
	}
	a := &channel{
		Deps: &Deps{
			Logger: zap.NewNop(), Cfg: &Config{OpTimeout: time.Second, PostRate: 100, PostBurst: 100}, Repo: log, Authz: az,
			Users: users.NewCache(repo, 10, time.Minute), Engine: engine, Hub: sink, UpdatesCfg: testUpdates,
		},
		id: ch.ID, ch: ch, members: map[gocql.UUID]models.ChannelRole{member: models.ChannelRoleMember},
		cleared: map[gocql.UUID]gocql.UUID{}, typing: map[gocql.UUID]time.Time{}, posters: map[gocql.UUID]*rate.Limiter{},
	}
	a.snapshot()
	return a, repo, log, member
}

// say stores a message and broadcasts its post, as post does.
func say(a *channel, repo *messageRepo, author gocql.UUID, text string) *models.Message {
	m := &models.Message{ChannelID: a.id, ID: ids.NewTime(), UserID: author, Content: text}
	repo.mu.Lock()
	repo.messages[m.ID] = m
	repo.mu.Unlock()
	out := convertMessage(m)
	a.broadcast(&pb.Event{Payload: &pb.Event_MessagePosted{MessagePosted: &pb.MessagePosted{Message: out, TeamId: a.ch.TeamID.String()}}}, m)
	return m
}

func convertMessage(m *models.Message) *pb.Message {
	return &pb.Message{Id: m.ID.String(), ChannelId: m.ChannelID.String(), Content: m.Content}
}

// differenceOf asks the channel for its updates after pts.
func differenceOf(t *testing.T, a *channel, member gocql.UUID, pts int64) *updates.Difference {
	t.Helper()
	pid := a.Engine.Spawn(func() actor.Receiver {
		return actorFunc(func(c *actor.Context) {
			if req, ok := c.Message().(Request); ok {
				a.handle(c, req)
			}
		})
	}, "channel")
	defer a.Engine.Poison(pid)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d, err := actors.Ask[*updates.Difference](ctx, a.Engine, pid, Difference{
		Target: Target{P: &models.Principal{UserID: member, Role: models.SystemRoleUser}, ChannelID: a.id}, Pts: pts,
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

type actorFunc func(c *actor.Context)

func (f actorFunc) Receive(c *actor.Context) { f(c) }

func TestUpdatesAreNumberedAndReplayed(t *testing.T) {
	a, repo, log, member := newLoggedChannel(t, models.ChannelPublic)
	first := say(a, repo, member, "one")
	second := say(a, repo, member, "two")
	say(a, repo, member, "three")
	if a.pts != 3 || len(log.log) != 3 {
		t.Fatalf("pts %d with %d logged, want 3 and 3", a.pts, len(log.log))
	}
	// An edit, then the deletion of the first message.
	repo.mu.Lock()
	repo.messages[second.ID].Content = "two, edited"
	delete(repo.messages, first.ID)
	repo.mu.Unlock()
	a.broadcast(&pb.Event{Payload: &pb.Event_MessageDeleted{MessageDeleted: &pb.MessageDeleted{
		ChannelId: a.id.String(), MessageId: first.ID.String(),
	}}})

	d := differenceOf(t, a, member, 0)
	if d.TooLong || d.More || d.Pts != 4 {
		t.Fatalf("difference from 0: %+v", d)
	}
	// The deleted message's post is gone; the edited one reads as it is now.
	var got []string
	for _, ev := range d.Updates {
		if ev.PtsChannelId != a.id.String() || ev.PtsCount != 1 {
			t.Fatalf("an update without its channel's number: %v", ev)
		}
		switch p := ev.Payload.(type) {
		case *pb.Event_MessagePosted:
			got = append(got, p.MessagePosted.Message.Content)
		case *pb.Event_MessageDeleted:
			got = append(got, "deleted "+p.MessageDeleted.MessageId)
		}
	}
	want := []string{"two, edited", "three", "deleted " + first.ID.String()}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}

	if d := differenceOf(t, a, member, 3); len(d.Updates) != 1 || d.Pts != 4 {
		t.Fatalf("difference from 3: %+v", d)
	}
	if d := differenceOf(t, a, member, 4); len(d.Updates) != 0 || d.Pts != 4 || d.TooLong {
		t.Fatalf("difference from the latest: %+v", d)
	}
	if d := differenceOf(t, a, member, 9); !d.TooLong || d.Pts != 4 {
		t.Fatalf("difference from ahead of the channel: %+v", d)
	}
	// What the log no longer holds is too long to replay.
	log.mu.Lock()
	log.log = log.log[2:]
	log.mu.Unlock()
	if d := differenceOf(t, a, member, 1); !d.TooLong || d.Pts != 4 {
		t.Fatalf("difference past the log's start: %+v", d)
	}
}

func TestDifferenceHidesWhatWasCleared(t *testing.T) {
	a, repo, _, member := newLoggedChannel(t, models.ChannelDirect)
	say(a, repo, member, "before")
	a.cleared[member] = ids.NewTime()
	say(a, repo, member, "after")
	d := differenceOf(t, a, member, 0)
	if len(d.Updates) != 1 || d.Updates[0].GetMessagePosted().GetMessage().GetContent() != "after" {
		t.Fatalf("a cleared message replayed: %v", d.Updates)
	}
}

func TestDifferenceOnlyForMembers(t *testing.T) {
	a, _, _, _ := newLoggedChannel(t, models.ChannelPublic)
	pid := a.Engine.Spawn(func() actor.Receiver {
		return actorFunc(func(c *actor.Context) {
			if req, ok := c.Message().(Request); ok {
				a.handle(c, req)
			}
		})
	}, "channel")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := actors.Ask[*updates.Difference](ctx, a.Engine, pid, Difference{
		Target: Target{P: &models.Principal{UserID: ids.New(), Role: models.SystemRoleUser}, ChannelID: a.id},
	})
	if !ierrors.Is(err, ierrors.CodePermissionDenied) && !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("a stranger read a public channel's updates: %v", err)
	}
}

func TestLoadTakesTheLatestNumber(t *testing.T) {
	a, repo, log, member := newLoggedChannel(t, models.ChannelPublic)
	say(a, repo, member, "one")
	say(a, repo, member, "two")
	pts, err := log.ChannelPts(context.Background(), a.id)
	if err != nil || pts != 2 {
		t.Fatalf("pts %d, %v", pts, err)
	}
}
