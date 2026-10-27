package voiceserver

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/anthdm/hollywood/actor"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/client/drpc"
)

// Messages for the link actor.
type (
	// claim replies with *claimed or an error.
	claim   struct{ token string }
	claimed struct {
		userID, channelID, session string
	}
	peerLeft struct {
		userID, channelID, session string
	}
	// peerMedia says whether a room forwards a peer's camera, and its
	// screen.
	peerMedia struct {
		userID, channelID, session string
		video, stream              bool
	}
	setRooms struct{ pid *actor.PID }
)

type (
	linkUp struct {
		gen    uint64
		stream pb.DRPCVoiceServerService_ConnectClient
	}
	linkDown struct{ gen uint64 }
	linkIn   struct {
		gen  uint64
		resp *pb.VoiceServerServiceConnectResponse
	}
)

var (
	linkGen     atomic.Uint64
	errNoLink   = errors.New("the voice server is not connected to the chat server")
	errBadToken = errors.New("the token is not valid")
)

// link keeps this voice server's stream to the chat server, which claims
// the tokens browsers identify with and says whom to disconnect.
type link struct {
	logger  *zap.Logger
	cfg     *Config
	client  *drpc.Client
	rooms   *actor.PID
	stream  pb.DRPCVoiceServerService_ConnectClient
	gen     uint64
	seq     uint64
	pending map[uint64]*actor.PID
	// ready tells the health check whether the stream is up.
	ready  *atomic.Bool
	cancel context.CancelFunc
}

func newLink(logger *zap.Logger, cfg *Config, client *drpc.Client, ready *atomic.Bool) actor.Producer {
	return func() actor.Receiver {
		return &link{logger: logger, cfg: cfg, client: client, ready: ready, pending: make(map[uint64]*actor.PID)}
	}
}

func (l *link) Receive(c *actor.Context) {
	switch msg := c.Message().(type) {
	case actor.Started:
		ctx, cancel := context.WithCancel(context.Background())
		l.cancel = cancel
		go l.dial(ctx, c.Engine(), c.PID())
	case actor.Stopped:
		if l.cancel != nil {
			l.cancel()
		}
	case setRooms:
		l.rooms = msg.pid
	case claim:
		if l.stream == nil {
			actors.Reply(c, errNoLink)
			return
		}
		l.seq++
		l.pending[l.seq] = c.Sender()
		l.write(&pb.VoiceServerServiceConnectRequest{Frame: &pb.VoiceServerServiceConnectRequest_Claim{Claim: &pb.ClaimVoiceToken{RequestId: l.seq, Token: msg.token}}})
	case peerLeft:
		l.write(&pb.VoiceServerServiceConnectRequest{Frame: &pb.VoiceServerServiceConnectRequest_PeerLeft{PeerLeft: &pb.VoicePeerLeft{
			UserId: msg.userID, ChannelId: msg.channelID, Session: msg.session,
		}}})
	case peerMedia:
		l.write(&pb.VoiceServerServiceConnectRequest{Frame: &pb.VoiceServerServiceConnectRequest_PeerMedia{PeerMedia: &pb.VoicePeerMedia{
			UserId: msg.userID, ChannelId: msg.channelID, Session: msg.session, Video: msg.video, Stream: msg.stream,
		}}})
	case linkUp:
		l.gen, l.stream = msg.gen, msg.stream
		l.ready.Store(true)
		l.logger.Info("connected to the chat server")
	case linkDown:
		if msg.gen != l.gen {
			return
		}
		l.stream = nil
		l.ready.Store(false)
		for id, pid := range l.pending {
			c.Engine().Send(pid, errNoLink)
			delete(l.pending, id)
		}
		// The chat server forgot the talk on this server with the stream.
		if l.rooms != nil {
			c.Send(l.rooms, dropAll{reason: "the voice server lost the chat server"})
		}
		l.logger.Warn("lost the chat server, reconnecting")
	case linkIn:
		if msg.gen == l.gen {
			l.handle(c, msg.resp)
		}
	}
}

func (l *link) handle(c *actor.Context, resp *pb.VoiceServerServiceConnectResponse) {
	switch f := resp.Frame.(type) {
	case *pb.VoiceServerServiceConnectResponse_Claimed:
		pid := l.pending[f.Claimed.RequestId]
		if pid == nil {
			return
		}
		delete(l.pending, f.Claimed.RequestId)
		if f.Claimed.Error != "" {
			c.Engine().Send(pid, errBadToken)
			return
		}
		c.Engine().Send(pid, &claimed{userID: f.Claimed.GetUser().GetId(), channelID: f.Claimed.ChannelId, session: f.Claimed.Session})
	case *pb.VoiceServerServiceConnectResponse_Disconnect:
		d := f.Disconnect
		if l.rooms != nil {
			c.Send(l.rooms, disconnect{channelID: d.ChannelId, session: d.Session, reason: d.Reason})
		}
	}
}

func (l *link) write(req *pb.VoiceServerServiceConnectRequest) {
	if l.stream == nil {
		return
	}
	if err := l.stream.Send(req); err != nil {
		l.logger.Warn("failed to write to the chat server", zap.Error(err))
		_ = l.stream.Close()
	}
}

// dial keeps one stream to the chat server open, backing off between
// attempts.
func (l *link) dial(ctx context.Context, engine *actor.Engine, self *actor.PID) {
	delay := l.cfg.ReconnectMin
	for ctx.Err() == nil {
		gen := linkGen.Add(1)
		started := time.Now()
		err := l.session(ctx, engine, self, gen)
		engine.Send(self, linkDown{gen: gen})
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			delay = l.cfg.ReconnectMin
		}
		l.logger.Warn("chat server stream ended", zap.Error(err), zap.Duration("retry_in", delay))
		jitter := time.Duration(rand.Int64N(int64(delay)/2 + 1)) //nolint:gosec // jitter needs no secrecy
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay/2 + jitter):
		}
		delay = min(delay*2, l.cfg.ReconnectMax)
	}
}

func (l *link) session(ctx context.Context, engine *actor.Engine, self *actor.PID, gen uint64) error {
	cc, err := l.client.Dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cc.Close() }()
	sctx, cancel := context.WithCancel(l.client.Stream(ctx, drpc.Call{}))
	defer cancel()
	stream, err := pb.NewDRPCVoiceServerServiceClient(cc).Connect(sctx)
	if err != nil {
		return err
	}
	hello := &pb.VoiceServerHello{ServerId: l.cfg.ID, Endpoint: l.cfg.PublicURL}
	if err := stream.Send(&pb.VoiceServerServiceConnectRequest{Frame: &pb.VoiceServerServiceConnectRequest_Hello{Hello: hello}}); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetWelcome() == nil {
		return io.ErrUnexpectedEOF
	}
	engine.Send(self, linkUp{gen: gen, stream: stream})
	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}
		engine.Send(self, linkIn{gen: gen, resp: resp})
	}
}
