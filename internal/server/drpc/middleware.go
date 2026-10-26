package drpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"go.uber.org/zap"
	"storj.io/drpc"
	"storj.io/drpc/drpcerr"
	"storj.io/drpc/drpcmetadata"

	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

// The metadata a gateway sends with every call.
const (
	MetaGatewayKey = "x-gateway-key"
	MetaSession    = "x-session"
	MetaClientIP   = "x-client-ip"
	MetaUserAgent  = "x-user-agent"
)

// voiceServerRPC is the one call voice servers make.
const voiceServerRPC = "/gochat.v1.VoiceServerService/Connect"

var streaming = map[string]bool{
	"/gochat.v1.GatewayService/Connect": true,
	voiceServerRPC:                      true,
	"/gochat.v1.FileService/Upload":     true,
	"/gochat.v1.FileService/Download":   true,
}

// botCalls are the calls open to bots: reading and writing in the
// channels they are in, about themselves, and their updates. Any other is
// refused, whatever its own checks.
var botCalls = map[string]bool{
	"/gochat.v1.AuthService/Me":                    true,
	"/gochat.v1.UserService/GetUser":               true,
	"/gochat.v1.UserService/UpdateProfile":         true,
	"/gochat.v1.TeamService/ListTeams":             true,
	"/gochat.v1.TeamService/GetTeam":               true,
	"/gochat.v1.TeamService/ListTeamMembers":       true,
	"/gochat.v1.ChannelService/ListChannels":       true,
	"/gochat.v1.ChannelService/GetChannel":         true,
	"/gochat.v1.ChannelService/ListChannelMembers": true,
	"/gochat.v1.ChannelService/JoinChannel":        true,
	"/gochat.v1.ChannelService/LeaveChannel":       true,
	"/gochat.v1.MessageService/PostMessage":        true,
	"/gochat.v1.MessageService/EditMessage":        true,
	"/gochat.v1.MessageService/DeleteMessage":      true,
	"/gochat.v1.MessageService/GetMessage":         true,
	"/gochat.v1.MessageService/ListMessages":       true,
	"/gochat.v1.MessageService/ListThread":         true,
	"/gochat.v1.MessageService/SendTyping":         true,
	"/gochat.v1.MessageService/React":              true,
	"/gochat.v1.FileService/Upload":                true,
	"/gochat.v1.FileService/Download":              true,
	"/gochat.v1.UpdatesService/GetState":           true,
	"/gochat.v1.UpdatesService/GetDifference":      true,
	"/gochat.v1.BotService/SetCommands":            true,
	"/gochat.v1.BotService/GetCommands":            true,
	"/gochat.v1.BotService/AnswerCallback":         true,
	"/gochat.v1.BotService/SetWebhook":             true,
	"/gochat.v1.BotService/DeleteWebhook":          true,
	"/gochat.v1.BotService/GetWebhookInfo":         true,
	"/gochat.v1.BotService/ConfirmUpdates":         true,
}

type principalKey struct{}

type clientKey struct{}

type Client struct {
	IP        string
	UserAgent string
}

// Principal returns the authenticated caller, or Unauthenticated.
func Principal(ctx context.Context) (*models.Principal, error) {
	p, ok := ctx.Value(principalKey{}).(*models.Principal)
	if !ok || p == nil {
		return nil, ierrors.Unauthenticated()
	}
	return p, nil
}

func ClientOf(ctx context.Context) Client {
	c, _ := ctx.Value(clientKey{}).(Client)
	return c
}

// Resolver turns a session token into its principal.
type Resolver func(ctx context.Context, token string) (*models.Principal, error)

type ctxStream struct {
	drpc.Stream
	ctx context.Context
}

func (s *ctxStream) Context() context.Context { return s.ctx }

// Guard authenticates the gateway and the session of every call, bounds unary
// calls in time, and turns every error into what a caller may see.
func Guard(logger *zap.Logger, cfg *Config, resolve Resolver) func(drpc.Handler) drpc.Handler {
	logger = logger.Named("server.drpc")
	return func(next drpc.Handler) drpc.Handler {
		return handlerFunc(func(stream drpc.Stream, rpc string) (err error) {
			start := time.Now()
			defer func() {
				if v := recover(); v != nil {
					logger.Error("rpc panicked", zap.String("rpc", rpc), zap.Any("panic", v), zap.Stack("stack"))
					err = ierrors.Internal(fmt.Errorf("panic: %v", v))
				}
				err = public(logger, rpc, err)
				if logger.Core().Enabled(zap.DebugLevel) {
					logger.Debug("rpc", zap.String("rpc", rpc), zap.Duration("took", time.Since(start)), zap.Error(err))
				}
			}()
			ctx := stream.Context()
			md, _ := drpcmetadata.Get(ctx)
			if !keyAllows(cfg, rpc, md[MetaGatewayKey]) {
				logger.Warn("call without a valid key", zap.String("rpc", rpc))
				return ierrors.Unauthenticated()
			}
			ctx = context.WithValue(ctx, clientKey{}, Client{IP: md[MetaClientIP], UserAgent: md[MetaUserAgent]})
			if token := md[MetaSession]; token != "" {
				p, err := resolve(ctx, token)
				if err != nil && !ierrors.Is(err, ierrors.CodeUnauthenticated) {
					return err
				}
				if p != nil && p.Bot && !botCalls[rpc] {
					return ierrors.New(ierrors.CodeUnimplemented, "bots cannot make this call")
				}
				if p != nil {
					ctx = context.WithValue(ctx, principalKey{}, p)
				}
			}
			if !streaming[rpc] {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, cfg.RequestTimeout)
				defer cancel()
			}
			return next.HandleRPC(&ctxStream{Stream: stream, ctx: ctx}, rpc)
		})
	}
}

// keyAllows reports whether sent is the key rpc takes: the voice servers'
// for their link, the gateways' for every other call. Neither opens the
// other's calls, so a voice server, which faces the internet, holds nothing
// that acts for a user, and a gateway cannot pose as a voice server.
func keyAllows(cfg *Config, rpc, sent string) bool {
	want := cfg.GatewayKey
	if rpc == voiceServerRPC {
		want = cfg.VoiceServerKey
	}
	return want != "" && secure.Equal(sent, want)
}

type handlerFunc func(stream drpc.Stream, rpc string) error

func (f handlerFunc) HandleRPC(stream drpc.Stream, rpc string) error { return f(stream, rpc) }

// public keeps an error's code and public message and drops its cause.
func public(logger *zap.Logger, rpc string, err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return err
	}
	e := ierrors.As(err)
	if e.Code == ierrors.CodeInternal {
		logger.Error("rpc failed", append(e.Fields(), zap.String("rpc", rpc))...)
	}
	return drpcerr.WithCode(errors.New(e.Message), uint64(e.Code))
}
