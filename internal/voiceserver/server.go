// Package voiceserver is GoChat's voice server, an SFU in the manner of
// Discord's: browsers connect to it with a token from the chat server, send
// it their voice over WebRTC, and receive everyone else's in their voice
// channel, forwarded as it arrives.
package voiceserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/coder/websocket"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/client/drpc"
	"github.com/ssenerg/gochat/internal/core/secure"
)

// Server is a running voice server.
type Server struct {
	logger  *zap.Logger
	cfg     *Config
	engine  *actor.Engine
	media   *Media
	link    *actor.PID
	rooms   *actor.PID
	ready   *atomic.Bool
	origins []string
	// unidentified holds a place for each connection yet to identify.
	unidentified chan struct{}
	srv          *http.Server
	failures     chan error
	stopOnce     sync.Once
}

func New(logger *zap.Logger, cfg *Config, client *drpc.Client, engine *actor.Engine, media *Media) *Server {
	s := &Server{
		logger: logger.Named("voice"), cfg: cfg, engine: engine, media: media,
		ready: new(atomic.Bool), failures: make(chan error, 1),
		unidentified: make(chan struct{}, cfg.MaxUnidentified),
	}
	for _, o := range cfg.AllowedOrigins {
		if u, err := url.Parse(o); err == nil {
			s.origins = append(s.origins, u.Host)
		}
	}
	recovered := actor.WithMiddleware(actors.Recover(logger))
	s.link = engine.Spawn(newLink(logger.Named("link"), cfg, client, s.ready), "link", actor.WithID("main"), recovered)
	s.rooms = engine.Spawn(newRooms(logger.Named("room"), cfg, media, s.link), "rooms", actor.WithID("main"), recovered)
	engine.Send(s.link, setRooms{pid: s.rooms})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /rtc", s.rtc)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			http.Error(w, "not connected to the chat server", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	s.srv = &http.Server{
		Addr:              cfg.HTTP.Address,
		Handler:           mux,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		ErrorLog:          zap.NewStdLog(s.logger.WithOptions(zap.IncreaseLevel(zap.WarnLevel))),
	}
	return s
}

func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.HTTP.Address)
	if err != nil {
		return err
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.failures <- err
		}
	}()
	s.logger.Info("started", zap.String("address", s.cfg.HTTP.Address), zap.String("public_url", s.cfg.PublicURL))
	return nil
}

func (s *Server) Failures() <-chan error { return s.failures }

// Stop ends every connection and the actors.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.HTTP.ShutdownTimeout)
		defer cancel()
		<-s.engine.PoisonCtx(ctx, s.rooms).Done()
		<-s.engine.PoisonCtx(ctx, s.link).Done()
		if err := s.srv.Shutdown(ctx); err != nil {
			_ = s.srv.Close()
		}
	})
}

// toRoom hands a peer's message to its room.
type toRoom struct {
	channelID string
	msg       any
}

// rtc serves one browser's signaling websocket.
func (s *Server) rtc(w http.ResponseWriter, r *http.Request) {
	select {
	case s.unidentified <- struct{}{}:
	default:
		http.Error(w, "too many connections, try again", http.StatusServiceUnavailable)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.origins})
	if err != nil {
		<-s.unidentified
		return
	}
	ws.SetReadLimit(s.cfg.ReadLimit)
	ctx := r.Context()

	// A cancelled read drops the connection without a word: the timer
	// closes it with the reason instead.
	late := time.AfterFunc(s.cfg.IdentifyTimeout, func() { _ = ws.Close(closeIdentify, "identify first, with a valid token") })
	first, err := read(ctx, ws)
	in := late.Stop()
	<-s.unidentified
	if !in {
		return
	}
	if err != nil || first.Op != opIdentify || !secure.ValidToken(first.Token) {
		_ = ws.Close(closeIdentify, "identify first, with a valid token")
		return
	}
	cl, err := actors.Ask[*claimed](ctx, s.engine, s.link, claim{token: first.Token})
	if err != nil {
		if errors.Is(err, errBadToken) {
			_ = ws.Close(closeIdentify, "the token is not valid")
		} else {
			_ = ws.Close(closeUnavailable, "voice is unavailable right now")
		}
		return
	}

	out := newSender(s.cfg.SendQueue)
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	written := make(chan struct{})
	go func() {
		defer close(written)
		s.write(wctx, ws, out)
	}()
	if _, err := actors.Ask[joined](ctx, s.engine, s.rooms, addPeer{session: cl.session, userID: cl.userID, channelID: cl.channelID, video: first.Video, out: out}); err != nil {
		// The chat server counts them in the talk until told otherwise.
		s.engine.Send(s.link, peerLeft{userID: cl.userID, channelID: cl.channelID, session: cl.session})
		if errors.Is(err, errFull) {
			out.close(closeFull, errFull.Error())
		} else {
			s.logger.Warn("failed to add a peer", zap.Error(err))
			out.close(closeUnavailable, "voice is unavailable right now")
		}
		<-written
		return
	}
	defer s.engine.Send(s.rooms, toRoom{channelID: cl.channelID, msg: removePeer{session: cl.session, code: closeDisconnected, reason: "left"}})
	limit := rate.NewLimiter(rate.Limit(s.cfg.MessageRate), s.cfg.MessageBurst)
	for {
		msg, err := read(ctx, ws)
		if err != nil {
			return
		}
		if !limit.Allow() {
			out.close(closeFailed, "too many messages")
			<-written
			return
		}
		var m any
		switch msg.Op {
		case opAnswer:
			m = answer{session: cl.session, sdp: msg.SDP}
		case opCandidate:
			if msg.Candidate == nil {
				continue
			}
			m = candidate{session: cl.session, init: *msg.Candidate}
		case opSpeaking:
			m = speaking{session: cl.session, on: msg.Speaking}
		case opVideo:
			if msg.Source != sourceCamera && msg.Source != sourceScreen {
				continue
			}
			m = video{session: cl.session, source: msg.Source, on: msg.On}
		default:
			continue
		}
		s.engine.Send(s.rooms, toRoom{channelID: cl.channelID, msg: m})
	}
}

func read(ctx context.Context, ws *websocket.Conn) (*inbound, error) {
	typ, data, err := ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, errors.New("binary message")
	}
	var msg inbound
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// write owns a websocket's sending side, and pings it to notice a browser
// that went away without closing.
func (s *Server) write(ctx context.Context, ws *websocket.Conn, out *sender) {
	defer func() { _ = ws.CloseNow() }()
	ping := time.NewTicker(s.cfg.PingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-out.ch:
			wctx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
			err := ws.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				return
			}
		case f := <-out.closed:
			reason := f.reason
			if len(reason) > 120 {
				reason = reason[:120]
			}
			_ = ws.Close(websocket.StatusCode(f.code), reason)
			return
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
			err := ws.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}
