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
)

// The close codes of the client API's event socket; see client.proto.
const (
	CloseRetry           websocket.StatusCode = 4000
	CloseUnauthenticated websocket.StatusCode = 4001
	CloseRevoked         websocket.StatusCode = 4003
	CloseTooMany         websocket.StatusCode = 4008
	CloseTooSlow         websocket.StatusCode = 4009
)

// MaxWatched bounds the people an app may show the presence of.
const MaxWatched = 1000

// clientSession serves one event socket of the client API: native apps get
// the person's events as the chat server sends them, in binary ClientFrames,
// and render them themselves. They say what they show in AppFrames, which
// the session hands the router as a page's session does.
type clientSession struct {
	*Deps
	connID   uint64
	call     drpc.Call
	ws       *websocket.Conn
	out      *outbox
	in       *Inflow
	closing  bool
	stopPing context.CancelFunc
	expiry   *time.Timer
	// resync is the ClientReady that waits its turn to tell the app to
	// load again, after the chat server came back.
	resync *pb.ClientReady
}

func NewClientSession(d *Deps, connID uint64, call drpc.Call, ws *websocket.Conn, in *Inflow) actor.Producer {
	return func() actor.Receiver {
		return &clientSession{Deps: d, connID: connID, call: call, ws: ws, out: newOutbox(d.Cfg.SendBuffer), in: in}
	}
}

func (s *clientSession) Receive(c *actor.Context) {
	switch msg := c.Message().(type) {
	case actor.Started:
		go s.write()
		ctx, cancel := context.WithCancel(context.Background())
		s.stopPing = cancel
		go s.ping(ctx)
		c.Send(s.Router, register{connID: s.connID, token: s.call.Session, pid: c.PID()})
	case actor.Stopped:
		c.Send(s.Router, unregister{connID: s.connID})
		s.stopPing()
		if s.expiry != nil {
			s.expiry.Stop()
		}
		s.closing = true
		s.out.close(websocket.StatusNormalClosure)
	case Frame:
		s.frame(c, msg.Data)
		s.in.done(len(msg.Data))
	case Ready:
		// The socket ends with its session.
		if s.expiry == nil && !msg.ExpiresAt.IsZero() {
			pid, engine := c.PID(), c.Engine()
			s.expiry = time.AfterFunc(time.Until(msg.ExpiresAt), func() { engine.Send(pid, expired{}) })
		}
		ready := &pb.ClientReady{User: msg.User, SessionId: msg.SessionID, Resync: msg.Again}
		if !msg.ExpiresAt.IsZero() {
			ready.ExpiresAt = timestamppb.New(msg.ExpiresAt)
		}
		if !msg.Again {
			s.send(&pb.ClientFrame{Frame: &pb.ClientFrame_Ready{Ready: ready}})
			return
		}
		// After the chat server came back, apps load again on their turn,
		// as pages do, rather than all at once; events flow meanwhile.
		if s.resync == nil {
			self, engine := c.PID(), c.Engine()
			time.AfterFunc(resyncWait(s.Resyncs, s.Cfg.ResyncJitter), func() { engine.Send(self, resyncDue{}) })
		}
		s.resync = ready
	case resyncDue:
		if s.resync != nil {
			s.send(&pb.ClientFrame{Frame: &pb.ClientFrame_Ready{Ready: s.resync}})
			s.resync = nil
		}
	case Rejected:
		switch {
		case msg.TooMany:
			s.close(CloseTooMany)
		case msg.Retry:
			s.close(CloseRetry)
		default:
			s.close(CloseUnauthenticated)
		}
	case Revoked, expired:
		s.close(CloseRevoked)
	case Event:
		// The router shares one event among its recipients, and its
		// recipient list names the others on this gateway: the app gets the
		// payload alone.
		s.send(&pb.ClientFrame{Frame: &pb.ClientFrame_Event{Event: &pb.Event{Payload: msg.Event.Payload}}})
	}
}

// frame takes what the app shows: the channel, whose typing it gets, and
// the people, whose presence and profile changes it gets.
func (s *clientSession) frame(c *actor.Context, data []byte) {
	var f pb.AppFrame
	if err := proto.Unmarshal(data, &f); err != nil {
		return
	}
	switch fr := f.Frame.(type) {
	case *pb.AppFrame_View:
		id := fr.View.GetChannelId()
		if id != "" && !uuidRe.MatchString(id) {
			return
		}
		c.Send(s.Router, viewing{connID: s.connID, channelID: id})
	case *pb.AppFrame_Watch:
		ids := fr.Watch.GetUserIds()
		if len(ids) > MaxWatched {
			ids = ids[:MaxWatched]
		}
		users := make([]string, 0, len(ids))
		seen := make(map[string]bool, len(ids))
		for _, id := range ids {
			if uuidRe.MatchString(id) && !seen[id] {
				seen[id] = true
				users = append(users, id)
			}
		}
		c.Send(s.Router, watching{connID: s.connID, users: users})
	}
}

// send queues a frame, closing a socket whose app does not keep up: it
// reconnects and loads what it shows again.
func (s *clientSession) send(f *pb.ClientFrame) {
	if s.closing {
		return
	}
	data, err := proto.Marshal(f)
	if err != nil {
		s.Logger.Error("failed to encode a client frame", zap.Error(err))
		return
	}
	if !s.out.push(data) {
		s.Logger.Debug("app too slow, closing its socket", zap.Uint64("conn", s.connID))
		s.closing = true
		s.out.close(0)
		// What is queued is dropped: the close goes out of line, as the
		// handshake may take a while.
		go func() { _ = s.ws.Close(CloseTooSlow, "too far behind") }()
	}
}

// close closes the socket with code once what is queued is written.
func (s *clientSession) close(code websocket.StatusCode) {
	if !s.closing {
		s.closing = true
		s.out.close(code)
	}
}

// write owns the socket's sending side.
func (s *clientSession) write() { writeBinary(s.ws, s.out, s.Cfg) }

func (s *clientSession) ping(ctx context.Context) { pingLoop(ctx, s.ws, s.Cfg) }

// writeBinary writes what out holds to ws, as binary messages, until out
// closes.
func writeBinary(ws *websocket.Conn, out *outbox, cfg *Config) {
	for {
		items, done, code := out.take()
		for _, b := range items {
			ctx, cancel := context.WithTimeout(context.Background(), cfg.WriteTimeout)
			err := ws.Write(ctx, websocket.MessageBinary, b)
			cancel()
			out.sent(len(b))
			if err != nil {
				out.close(0)
				_ = ws.CloseNow()
				return
			}
		}
		if done {
			if code != 0 {
				_ = ws.Close(code, "")
			}
			return
		}
	}
}

// pingLoop pings ws until ctx ends, closing a socket that stops answering.
func pingLoop(ctx context.Context, ws *websocket.Conn, cfg *Config) {
	t := time.NewTicker(cfg.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, cfg.WriteTimeout)
			err := ws.Ping(pctx)
			cancel()
			if err != nil {
				_ = ws.CloseNow()
				return
			}
		}
	}
}
