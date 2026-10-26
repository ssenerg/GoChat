package updates

import (
	"context"
	"strconv"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/hub"
)

type Repo interface {
	Messages
	AccountPts(ctx context.Context, userID gocql.UUID) (int64, error)
	AppendAccountUpdate(ctx context.Context, userID gocql.UUID, e *scylla.LogEntry, stamp int64, ttl time.Duration) error
	AccountUpdates(ctx context.Context, userID gocql.UUID, after, upTo int64, limit int) ([]*scylla.LogEntry, error)
}

type Deps struct {
	Logger *zap.Logger
	Cfg    *Config
	Repo   Repo
	Engine *actor.Engine
	Hub    *actor.PID
	// Render renders the messages a bot's log refers to.
	Render Renderer
	// Users tells the bots that can no longer act, whose updates are
	// dropped; nil keeps every update.
	Users Accounts
}

// Accounts reads accounts.
type Accounts interface {
	Get(ctx context.Context, id gocql.UUID) (*models.User, error)
}

// Push numbers Event in the updates of each of UserIDs, logs it and sends
// it to them. Event is shared, never modified; a numbered event of a
// channel is numbered again, in each account.
type Push struct {
	UserIDs []gocql.UUID
	Event   *pb.Event
}

// State replies with the number of the account's latest update.
type State struct{ UserID gocql.UUID }

// GetDifference replies with *Difference: the account's updates after Pts.
type GetDifference struct {
	UserID gocql.UUID
	Pts    int64
	Limit  int
}

// Listen has PID nudged, with Nudge, whenever UserID gets an update;
// Unlisten stops it.
type (
	Listen struct {
		UserID gocql.UUID
		PID    *actor.PID
	}
	Unlisten struct{ UserID gocql.UUID }
	Nudge    struct{ UserID gocql.UUID }
)

// request is a message for one account's sequencer.
type request interface{ account() gocql.UUID }

type push struct {
	userID gocql.UUID
	event  *pb.Event
}

func (p push) account() gocql.UUID          { return p.userID }
func (s State) account() gocql.UUID         { return s.UserID }
func (d GetDifference) account() gocql.UUID { return d.UserID }

type (
	idleCheck struct{}
	passivate struct {
		id      gocql.UUID
		pid     *actor.PID
		handled uint64
	}
	child struct {
		pid    *actor.PID
		routed uint64
	}
)

type manager struct {
	*Deps
	children  map[gocql.UUID]*child
	gen       uint64
	listeners map[gocql.UUID]*actor.PID
}

// New runs one sequencer per active account, so each account's updates
// leave in the order they were numbered.
func New(d *Deps) actor.Producer {
	return func() actor.Receiver {
		return &manager{Deps: d, children: make(map[gocql.UUID]*child), listeners: make(map[gocql.UUID]*actor.PID)}
	}
}

func (m *manager) Receive(c *actor.Context) {
	switch msg := c.Message().(type) {
	case passivate:
		if ch, ok := m.children[msg.id]; ok && ch.pid.Equals(msg.pid) && ch.routed == msg.handled {
			delete(m.children, msg.id)
			c.Engine().Poison(ch.pid)
		}
	case Push:
		for _, id := range msg.UserIDs {
			m.route(c, push{userID: id, event: msg.Event})
			// A difference asked after the nudge follows the push.
			if pid := m.listeners[id]; pid != nil {
				c.Send(pid, Nudge{UserID: id})
			}
		}
	case Listen:
		m.listeners[msg.UserID] = msg.PID
	case Unlisten:
		delete(m.listeners, msg.UserID)
	case State:
		m.route(c, msg)
	case GetDifference:
		m.route(c, msg)
	}
}

func (m *manager) route(c *actor.Context, req request) {
	id := req.account()
	ch, ok := m.children[id]
	if !ok || c.Child(ch.pid.ID) == nil {
		m.gen++
		ch = &child{pid: c.SpawnChild(newAccount(m.Deps, id), "account",
			actor.WithID(id.String()+"."+strconv.FormatUint(m.gen, 36)),
			actor.WithInboxSize(256),
			actor.WithMiddleware(actors.Recover(m.Logger)))}
		m.children[id] = ch
	}
	ch.routed++
	c.Engine().SendWithSender(ch.pid, req, c.Sender())
}

// account numbers one account's updates.
type account struct {
	*Deps
	id       gocql.UUID
	pts      int64
	stamp    int64
	loaded   bool
	loadErr  error
	active   time.Time
	asked    bool
	handled  uint64
	repeater *actor.SendRepeater
}

func newAccount(d *Deps, id gocql.UUID) actor.Producer {
	return func() actor.Receiver {
		return &account{Deps: d, id: id}
	}
}

func (a *account) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), a.Cfg.OpTimeout)
}

func (a *account) Receive(c *actor.Context) {
	switch msg := c.Message().(type) {
	case actor.Started:
		a.active = time.Now()
		r := c.SendRepeat(c.PID(), idleCheck{}, a.Cfg.IdleTimeout/2)
		a.repeater = &r
	case actor.Stopped:
		if a.repeater != nil {
			a.repeater.Stop()
		}
	case idleCheck:
		if time.Since(a.active) >= a.Cfg.IdleTimeout && !a.asked {
			a.asked = true
			c.Send(c.Parent(), passivate{id: a.id, pid: c.PID(), handled: a.handled})
		}
	case request:
		a.active = time.Now()
		a.handled++
		a.asked = false
		if !a.loaded {
			a.loaded = true
			a.load()
		}
		a.handle(c, msg)
		// A failed load is tried again with the next request.
		if a.loadErr != nil {
			a.loaded, a.loadErr = false, nil
		}
	}
}

func (a *account) load() {
	ctx, cancel := a.ctx()
	defer cancel()
	pts, err := a.Repo.AccountPts(ctx, a.id)
	if err != nil {
		a.Logger.Error("failed to load an account's updates", zap.String("user", a.id.String()), zap.Error(err))
		a.loadErr = ierrors.Unavailable("updates are unavailable, try again")
		return
	}
	a.pts = pts
}

func (a *account) handle(c *actor.Context, req request) {
	switch msg := req.(type) {
	case push:
		a.push(msg)
	case State:
		if a.loadErr != nil {
			actors.Reply(c, a.loadErr)
			return
		}
		actors.Reply(c, a.pts)
	case GetDifference:
		if a.loadErr != nil {
			actors.Reply(c, a.loadErr)
			return
		}
		a.difference(c, msg)
	}
}

// push numbers ev in the account, logs it, then sends it: an update is
// never seen before a difference could replay it. An update that cannot be
// numbered is still sent, without its number.
func (a *account) push(msg push) {
	if a.retired() {
		return
	}
	ev := &pb.Event{Payload: msg.event.Payload}
	if a.loadErr == nil {
		a.pts++
		ev.Pts, ev.PtsCount = a.pts, 1
		e := Entry(ev)
		e.Pts = a.pts
		a.stamp = max(time.Now().UnixMicro(), a.stamp+1)
		ctx, cancel := a.ctx()
		if err := a.Repo.AppendAccountUpdate(ctx, a.id, e, a.stamp, a.Cfg.Retention); err != nil {
			a.Logger.Warn("failed to log an update", zap.String("user", a.id.String()), zap.Error(err))
		}
		cancel()
	}
	a.Engine.Send(a.Hub, hub.Deliver{UserIDs: []string{a.id.String()}, Event: ev})
}

// retired reports whether the account is a bot that can no longer act:
// deleted, or its owner deactivated.
func (a *account) retired() bool {
	if a.Users == nil {
		return false
	}
	ctx, cancel := a.ctx()
	defer cancel()
	u, err := a.Users.Get(ctx, a.id)
	if err != nil || !u.Bot {
		return false
	}
	if u.Deactivated {
		return true
	}
	owner, err := a.Users.Get(ctx, u.BotOwner)
	return err == nil && owner.Deactivated
}

// difference reads off the actor: every update up to the current number is
// logged already.
func (a *account) difference(c *actor.Context, msg GetDifference) {
	sender, engine := c.Sender(), c.Engine()
	if !actors.IsAsk(sender) {
		return
	}
	limit := a.Cfg.DifferenceLimit
	if msg.Limit > 0 && msg.Limit < limit {
		limit = msg.Limit
	}
	from, upTo := msg.Pts, a.pts
	r := &Resolver{Messages: a.Repo, Render: a.Render}
	go func() {
		ctx, cancel := a.ctx()
		defer cancel()
		d, err := Read(ctx, from, upTo, limit, func(after, upTo int64, limit int) ([]*scylla.LogEntry, error) {
			return a.Repo.AccountUpdates(ctx, a.id, after, upTo, limit)
		}, r.Resolve)
		if err != nil {
			if e := ierrors.As(err); e.Code == ierrors.CodeInternal {
				a.Logger.Error("failed to read updates", append(e.Fields(), zap.String("user", a.id.String()))...)
			}
			engine.Send(sender, err)
			return
		}
		engine.Send(sender, d)
	}()
}
