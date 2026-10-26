package drpc

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"storj.io/drpc"
	"storj.io/drpc/drpcerr"
	"storj.io/drpc/drpcmetadata"

	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

func TestKeysOpenOnlyTheirCalls(t *testing.T) {
	gateway, voice := strings.Repeat("g", 32), strings.Repeat("v", 32)
	cfg := &Config{GatewayKey: gateway, VoiceServerKey: voice}
	cases := []struct {
		rpc, key string
		want     bool
	}{
		{"/gochat.v1.MessageService/PostMessage", gateway, true},
		{"/gochat.v1.GatewayService/Connect", gateway, true},
		{voiceServerRPC, voice, true},
		// A voice server cannot act for users, nor a gateway pose as a voice
		// server.
		{"/gochat.v1.MessageService/PostMessage", voice, false},
		{"/gochat.v1.GatewayService/Connect", voice, false},
		{voiceServerRPC, gateway, false},
		{voiceServerRPC, "", false},
		{"/gochat.v1.MessageService/PostMessage", "", false},
	}
	for _, c := range cases {
		if got := keyAllows(cfg, c.rpc, c.key); got != c.want {
			t.Errorf("%s with %.1s…: %v, want %v", c.rpc, c.key, got, c.want)
		}
	}
	// Without a voice server key, no key opens the voice servers' link.
	cfg.VoiceServerKey = ""
	if keyAllows(cfg, voiceServerRPC, "") || keyAllows(cfg, voiceServerRPC, gateway) {
		t.Error("the link opened without a voice server key")
	}
}

func TestConfigRefusesASharedVoiceServerKey(t *testing.T) {
	key := strings.Repeat("k", 32)
	for _, voice := range []string{key, "short"} {
		cfg := &Config{Address: "127.0.0.1:7070", GatewayKey: key, VoiceServerKey: voice}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "voice_server_key") {
			t.Errorf("voice_server_key %q: %v", voice, err)
		}
	}
}

// metaStream is a stream whose context carries call metadata.
type metaStream struct {
	drpc.Stream
	ctx context.Context
}

func (s *metaStream) Context() context.Context { return s.ctx }

func TestBotsMakeOnlyTheirCalls(t *testing.T) {
	key := strings.Repeat("g", 32)
	cfg := &Config{GatewayKey: key, RequestTimeout: time.Second}
	bot := &models.Principal{UserID: ids.New(), Bot: true}
	person := &models.Principal{UserID: ids.New()}
	resolve := func(_ context.Context, token string) (*models.Principal, error) {
		if token == "bot" {
			return bot, nil
		}
		return person, nil
	}
	var reached bool
	next := handlerFunc(func(stream drpc.Stream, _ string) error {
		_, err := Principal(stream.Context())
		reached = err == nil
		return nil
	})
	guard := Guard(zap.NewNop(), cfg, resolve)(next)
	call := func(token, rpc string) error {
		reached = false
		ctx := drpcmetadata.AddPairs(context.Background(), map[string]string{MetaGatewayKey: key, MetaSession: token})
		return guard.HandleRPC(&metaStream{ctx: ctx}, rpc)
	}
	for _, rpc := range []string{"/gochat.v1.MessageService/PostMessage", "/gochat.v1.UpdatesService/GetDifference", "/gochat.v1.BotService/AnswerCallback"} {
		if err := call("bot", rpc); err != nil || !reached {
			t.Errorf("a bot's %s: %v", rpc, err)
		}
	}
	for _, rpc := range []string{
		"/gochat.v1.MessageService/SearchMessages", "/gochat.v1.ChannelService/OpenDirectChannel", "/gochat.v1.TeamService/CreateInvite",
		"/gochat.v1.AuthService/ChangePassword", "/gochat.v1.UpdatesService/GetChannelDifference", "/gochat.v1.BotService/CreateBot",
		"/gochat.v1.MessageService/PressButton",
	} {
		if err := call("bot", rpc); reached || drpcerr.Code(err) != uint64(ierrors.CodeUnimplemented) {
			t.Errorf("a bot's %s reached it: %v", rpc, err)
		}
	}
	if err := call("person", "/gochat.v1.MessageService/SearchMessages"); err != nil || !reached {
		t.Errorf("a person's search: %v", err)
	}
}
