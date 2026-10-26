package web

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/coder/websocket"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/client/drpc"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/web/gateway"
)

// The GoChat protocol is served at /api/v2: docs/protocol.md specifies it.

// protocolName is the socket's WebSocket subprotocol.
const protocolName = "gochat.v2"

// anonymousCalls are those a socket makes before it has a session.
var anonymousCalls = map[string]bool{
	"gochat.v1.AuthService/GetSignupInfo": true,
	"gochat.v1.AuthService/SignUp":        true,
	"gochat.v1.AuthService/Login":         true,
	"gochat.v1.AuthService/VerifyMFA":     true,
	"gochat.v1.TeamService/GetInvite":     true,
}

// apiSocket opens a socket of the GoChat protocol; the client says who it
// is in its first frame.
func (h *Handler) apiSocket(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	// Clients are apps and bots, which send no Origin; the library refuses
	// a cross-origin one, and a page's socket carries no token anyway.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{protocolName}})
	if err != nil {
		h.logger.Debug("socket upgrade failed", zap.Error(err))
		return
	}
	if ws.Subprotocol() != protocolName {
		_ = ws.Close(gateway.CloseProtocol, "speak "+protocolName)
		return
	}
	ws.SetReadLimit(apiReadLimit)
	id := h.connSeq.Add(1)
	in := gateway.NewInflow(h.gw.Cfg)
	call := h.call(r)
	call.Session = ""
	pid := h.engine.Spawn(gateway.NewLink(h.gw, socketCalls{h}, id, call, ws, in), "link", actor.WithID(strconv.FormatUint(id, 36)),
		actor.WithMiddleware(actors.Recover(h.logger)))
	defer h.engine.Poison(pid)
	for {
		typ, data, err := ws.Read(context.Background())
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary {
			_ = ws.Close(gateway.CloseProtocol, "frames are binary")
			return
		}
		if !in.Admit(len(data)) {
			_ = ws.Close(websocket.StatusPolicyViolation, "too fast")
			return
		}
		h.engine.Send(pid, gateway.Frame{Data: data})
	}
}

// socketCalls makes the calls of the chat API for sockets, as the HTTP API
// makes them.
type socketCalls struct{ h *Handler }

func (s socketCalls) Allow(call drpc.Call, method string, anonymous bool) error {
	m, ok := apiMethods[method]
	if !ok {
		return ierrors.New(ierrors.CodeUnimplemented, "no such call")
	}
	if anonymous && !anonymousCalls[method] {
		return ierrors.Unauthenticated()
	}
	if m.limited {
		if ok, _ := s.h.auth.Allow(call.ClientIP); !ok {
			return ierrors.ResourceExhausted("too many attempts, wait a minute")
		}
	}
	if call.Session == "" {
		if ok, _ := s.h.reqs.Allow(call.ClientIP); !ok {
			return ierrors.ResourceExhausted("too many requests, slow down")
		}
		return nil
	}
	if ok, _ := s.h.apiShared.Allow(call.ClientIP); !ok {
		return ierrors.ResourceExhausted("too many requests, slow down")
	}
	if ok, _ := s.h.apiSessions.Allow("session:" + secure.PublicID(secure.Hash(call.Session))); !ok {
		return ierrors.ResourceExhausted("too many requests, slow down")
	}
	return nil
}

func (s socketCalls) Invoke(ctx context.Context, call drpc.Call, method string, payload []byte) ([]byte, error) {
	m, ok := apiMethods[method]
	if !ok {
		return nil, ierrors.New(ierrors.CodeUnimplemented, "no such call")
	}
	in := m.in.New().Interface()
	if err := proto.Unmarshal(payload, in); err != nil {
		return nil, ierrors.InvalidArgument("the request is not a valid " + string(m.in.Descriptor().Name()))
	}
	out := m.out.New().Interface()
	ctx, cancel := s.h.client.Context(ctx, call)
	defer cancel()
	if err := s.h.client.Invoke(ctx, "/"+method, in, out); err != nil {
		return nil, err
	}
	return proto.Marshal(out)
}
