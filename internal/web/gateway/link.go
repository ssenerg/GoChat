package gateway

import (
	"context"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/coder/websocket"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/client/drpc"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

// The GoChat protocol's own close codes; docs/protocol.md lists them all.
const (
	CloseProtocol websocket.StatusCode = 4002
	CloseOldLayer websocket.StatusCode = 4011
)

// Layer is the protocol layer this gateway speaks, OldestLayer the oldest
// it takes.
const (
	Layer       = 1
	OldestLayer = 1
)

const (
	// maxInflight bounds a socket's calls in progress, maxQueued those
	// waiting their turn.
	maxInflight = 32
	maxQueued   = 256
	// maxBuffered bounds the updates held while a socket catches up, and
	// maxReplay what it replays rather than telling the client to reload.
	maxBuffered = 4096
	maxReplay   = 2000
	// replayBatch is how many replayed updates go in one frame.
	replayBatch = 50
)

// Calls makes the chat API's calls for sockets.
type Calls interface {
	// Allow takes a call from its caller's budget, refusing what the caller
	// may not call: anonymous sockets only sign in.
	Allow(call drpc.Call, method string, anonymous bool) error
	// Invoke makes the call: payload is its request, the answer its
	// response, both in protobuf.
	Invoke(ctx context.Context, call drpc.Call, method string, payload []byte) ([]byte, error)
}

type (
	callDone struct {
		id      uint64
		payload []byte
		err     error
	}
	// caughtUp carries what a socket missed: the updates after from, up to
	// pts, or reset when there were too many.
	caughtUp struct {
		gen     uint64
		from    int64
		fresh   bool
		pts     int64
		updates []*pb.Event
		reset   bool
		err     error
	}
)

// link serves one socket of the GoChat protocol: the calls its client makes
// and the updates of its session, numbered, from where the client left
// off.
type link struct {
	*Deps
	calls  Calls
	connID uint64
	call   drpc.Call
	ws     *websocket.Conn
	out    *outbox
	in     *Inflow
	// greeted is set once the client said hello, anonymous while it has no
	// session.
	greeted   bool
	anonymous bool
	from      int64
	ready     *Ready
	// pts is the account's last update the client has, or is sent.
	pts      int64
	welcomed bool
	// While catching up, live updates wait in held.
	catching bool
	gen      uint64
	held     []*pb.Event
	inflight int
	queued   []*pb.Call
	closing  bool
	stopPing context.CancelFunc
	expiry   *time.Timer
}

func NewLink(d *Deps, calls Calls, connID uint64, call drpc.Call, ws *websocket.Conn, in *Inflow) actor.Producer {
	return func() actor.Receiver {
		return &link{Deps: d, calls: calls, connID: connID, call: call, ws: ws, out: newOutbox(d.Cfg.SendBuffer), in: in}
	}
}

func (l *link) Receive(c *actor.Context) {
	switch msg := c.Message().(type) {
	case actor.Started:
		go writeBinary(l.ws, l.out, l.Cfg)
		ctx, cancel := context.WithCancel(context.Background())
		l.stopPing = cancel
		go pingLoop(ctx, l.ws, l.Cfg)
	case actor.Stopped:
		if l.greeted && !l.anonymous {
			c.Send(l.Router, unregister{connID: l.connID})
		}
		l.stopPing()
		if l.expiry != nil {
			l.expiry.Stop()
		}
		l.closing = true
		l.out.close(websocket.StatusNormalClosure)
	case Frame:
		l.frame(c, msg.Data)
		l.in.done(len(msg.Data))
	case Ready:
		l.attached(c, msg)
	case caughtUp:
		l.caughtUp(c, msg)
	case callDone:
		l.done(c, msg)
	case Rejected:
		switch {
		case msg.TooMany:
			l.close(CloseTooMany)
		case msg.Retry:
			l.close(CloseRetry)
		default:
			l.close(CloseUnauthenticated)
		}
	case Revoked, expired:
		l.close(CloseRevoked)
	case Event:
		l.event(msg.Event)
	}
}

func (l *link) frame(c *actor.Context, data []byte) {
	var up pb.Up
	if err := proto.Unmarshal(data, &up); err != nil || up.Frame == nil {
		l.close(CloseProtocol)
		return
	}
	if h := up.GetHello(); h != nil {
		l.hello(c, h)
		return
	}
	if !l.greeted {
		l.close(CloseProtocol)
		return
	}
	switch f := up.Frame.(type) {
	case *pb.Up_Call:
		l.enqueue(c, f.Call)
	case *pb.Up_Ping:
		l.send(&pb.Down{Frame: &pb.Down_Pong{Pong: &pb.Pong{Id: f.Ping.Id}}})
	case *pb.Up_View:
		if !l.anonymous {
			if id := f.View.GetChannelId(); id == "" || uuidRe.MatchString(id) {
				c.Send(l.Router, viewing{connID: l.connID, channelID: id})
			}
		}
	case *pb.Up_Watch:
		if !l.anonymous {
			c.Send(l.Router, watching{connID: l.connID, users: watchList(f.Watch.GetUserIds())})
		}
	}
}

// hello opens the socket's session; an anonymous socket may say hello
// again, with the token it signed in for.
func (l *link) hello(c *actor.Context, h *pb.SocketHello) {
	if l.greeted && !l.anonymous {
		l.close(CloseProtocol)
		return
	}
	if h.Layer < OldestLayer {
		l.close(CloseOldLayer)
		return
	}
	l.greeted = true
	if h.Token == "" {
		l.anonymous = true
		l.send(&pb.Down{Frame: &pb.Down_Welcome{Welcome: &pb.SocketWelcome{Layer: Layer}}})
		return
	}
	l.anonymous = false
	l.call.Session = h.Token
	if h.Client != "" {
		l.call.UserAgent = truncate(h.Client, 256)
	}
	l.from = max(h.Pts, 0)
	c.Send(l.Router, register{connID: l.connID, token: h.Token, pid: c.PID()})
}

// attached starts the session once the server attached the socket: it
// catches the client up before any new update, which waits meanwhile.
func (l *link) attached(c *actor.Context, r Ready) {
	l.ready = &r
	if l.expiry == nil && !r.ExpiresAt.IsZero() {
		pid, engine := c.PID(), c.Engine()
		l.expiry = time.AfterFunc(time.Until(r.ExpiresAt), func() { engine.Send(pid, expired{}) })
	}
	// A client that said hello with no number starts from now; after that
	// the socket resumes from what it sent.
	from, fresh := l.from, l.from == 0
	if l.welcomed {
		from, fresh = l.pts, false
	}
	l.catching = true
	l.gen++
	go l.catchUp(c.Engine(), c.PID(), l.call, l.gen, from, fresh)
}

// catchUp reads what the account did after from, or when fresh its latest
// number.
func (l *link) catchUp(engine *actor.Engine, self *actor.PID, call drpc.Call, gen uint64, from int64, fresh bool) {
	out := caughtUp{gen: gen, from: from, fresh: fresh}
	ctx := context.Background()
	state := func() (int64, error) {
		b, err := l.calls.Invoke(ctx, call, "gochat.v1.UpdatesService/GetState", nil)
		if err != nil {
			return 0, err
		}
		var res pb.GetStateResponse
		return res.Pts, proto.Unmarshal(b, &res)
	}
	if fresh {
		out.pts, out.err = state()
		engine.Send(self, out)
		return
	}
	pts := from
	for {
		req, _ := proto.Marshal(&pb.GetDifferenceRequest{Pts: pts})
		b, err := l.calls.Invoke(ctx, call, "gochat.v1.UpdatesService/GetDifference", req)
		var res pb.GetDifferenceResponse
		if err == nil {
			err = proto.Unmarshal(b, &res)
		}
		if err != nil {
			out.err = err
			break
		}
		if res.TooLong || len(out.updates)+len(res.Updates) > maxReplay {
			out.updates, out.reset = nil, true
			out.pts, out.err = res.Pts, nil
			if !res.TooLong {
				out.pts, out.err = state()
			}
			break
		}
		out.updates = append(out.updates, res.Updates...)
		pts = res.Pts
		if !res.More {
			out.pts = pts
			break
		}
	}
	engine.Send(self, out)
}

func (l *link) caughtUp(c *actor.Context, msg caughtUp) {
	if msg.gen != l.gen || l.ready == nil {
		return
	}
	if msg.err != nil {
		l.Logger.Debug("failed to catch a socket up", zap.Error(msg.err))
		e := drpc.Err(msg.err)
		if e.Code == ierrors.CodeUnauthenticated {
			l.close(CloseUnauthenticated)
		} else {
			l.close(CloseRetry)
		}
		return
	}
	w := &pb.SocketWelcome{
		User: l.ready.User, SessionId: l.ready.SessionID, Pts: msg.from, Layer: Layer, Reload: msg.reset,
	}
	if !l.ready.ExpiresAt.IsZero() {
		w.ExpiresAt = timestamppb.New(l.ready.ExpiresAt)
	}
	if msg.reset || msg.fresh {
		w.Pts = msg.pts
	}
	l.welcomed = true
	l.pts = w.Pts
	l.send(&pb.Down{Frame: &pb.Down_Welcome{Welcome: w}})
	for start := 0; start < len(msg.updates); start += replayBatch {
		batch := msg.updates[start:min(start+replayBatch, len(msg.updates))]
		l.send(&pb.Down{Frame: &pb.Down_Updates{Updates: &pb.Updates{Updates: batch}}})
	}
	l.pts = max(l.pts, msg.pts)
	l.catching = false
	held := l.held
	l.held = nil
	for _, ev := range held {
		l.event(ev)
	}
}

// event sends an update on: a numbered one of the account once, in order,
// one without a number as ephemeral.
func (l *link) event(ev *pb.Event) {
	if l.catching {
		if len(l.held) >= maxBuffered {
			l.close(CloseTooSlow)
			return
		}
		l.held = append(l.held, ev)
		return
	}
	// The router shares one event among its recipients, and its recipient
	// list names the others on this gateway: the client gets the rest.
	out := &pb.Event{Payload: ev.Payload, Pts: ev.Pts, PtsCount: ev.PtsCount, PtsChannelId: ev.PtsChannelId}
	switch {
	case ev.Pts == 0:
		l.send(&pb.Down{Frame: &pb.Down_Ephemeral{Ephemeral: out}})
		return
	case ev.PtsChannelId == "":
		if ev.Pts <= l.pts {
			return
		}
		l.pts = ev.Pts
	}
	l.send(&pb.Down{Frame: &pb.Down_Updates{Updates: &pb.Updates{Updates: []*pb.Event{out}}}})
}

func (l *link) enqueue(c *actor.Context, call *pb.Call) {
	if l.inflight >= maxInflight {
		if len(l.queued) >= maxQueued {
			l.close(CloseTooSlow)
			return
		}
		l.queued = append(l.queued, call)
		return
	}
	l.start(c, call)
}

// start makes a call off the actor; its result comes back as callDone.
func (l *link) start(c *actor.Context, call *pb.Call) {
	l.inflight++
	self, engine, id := c.PID(), c.Engine(), call.Id
	if err := l.calls.Allow(l.call, call.Method, l.anonymous); err != nil {
		engine.Send(self, callDone{id: id, err: err})
		return
	}
	who := l.call
	go func() {
		payload, err := l.calls.Invoke(context.Background(), who, call.Method, call.Payload)
		engine.Send(self, callDone{id: id, payload: payload, err: err})
	}()
}

func (l *link) done(c *actor.Context, msg callDone) {
	l.inflight--
	res := &pb.Result{Id: msg.id}
	if msg.err != nil {
		e := drpc.Err(msg.err)
		if e.Code >= ierrors.CodeInternal && e.Code != ierrors.CodeUnavailable {
			l.Logger.Warn("socket call failed", zap.Error(msg.err))
		}
		res.Outcome = &pb.Result_Error{Error: &pb.ApiError{Code: uint32(e.Code), Message: e.Message}} //nolint:gosec // codes are small
	} else {
		res.Outcome = &pb.Result_Payload{Payload: msg.payload}
	}
	l.send(&pb.Down{Frame: &pb.Down_Result{Result: res}})
	if len(l.queued) > 0 && !l.closing {
		next := l.queued[0]
		l.queued = l.queued[1:]
		l.start(c, next)
	}
}

// send queues a frame, closing a socket whose client does not keep up: it
// reconnects with its number and catches up.
func (l *link) send(d *pb.Down) {
	if l.closing {
		return
	}
	data, err := proto.Marshal(d)
	if err != nil {
		l.Logger.Error("failed to encode a socket frame", zap.Error(err))
		return
	}
	if !l.out.push(data) {
		l.closing = true
		l.out.close(0)
		go func() { _ = l.ws.Close(CloseTooSlow, "too far behind") }()
	}
}

func (l *link) close(code websocket.StatusCode) {
	if !l.closing {
		l.closing = true
		l.out.close(code)
	}
}

// watchList keeps the well-formed ids of ids, once each, at most MaxWatched.
func watchList(ids []string) []string {
	if len(ids) > MaxWatched {
		ids = ids[:MaxWatched]
	}
	out := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if uuidRe.MatchString(id) && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
