package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"storj.io/drpc/drpcmetadata"
	"storj.io/drpc/drpcmux"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/web/gateway"
)

func TestBotMethodsAreCalls(t *testing.T) {
	for name, full := range botMethods {
		if _, ok := apiMethods[full]; !ok {
			t.Errorf("%s names %s, no call", name, full)
		}
	}
}

func TestNewerPollEndsTheOlder(t *testing.T) {
	polls := &botPolls{polls: map[string]*poll{}}
	first, doneFirst := polls.begin(context.Background(), "bot")
	second, doneSecond := polls.begin(context.Background(), "bot")
	defer doneSecond()
	if !errors.Is(context.Cause(first), errPollReplaced) {
		t.Fatalf("the older poll ended with %v", context.Cause(first))
	}
	doneFirst()
	if second.Err() != nil {
		t.Fatal("ending the older poll ended the newer")
	}
	if polls.polls["bot"] == nil {
		t.Fatal("the newer poll was forgotten")
	}
}

func TestOnlyAnAccountUpdateWakesAPoll(t *testing.T) {
	events := make(chan any, 4)
	events <- gateway.Event{Event: &pb.Event{Payload: &pb.Event_Typing{Typing: &pb.Typing{}}}}
	events <- gateway.Event{Event: &pb.Event{Pts: 3, PtsCount: 1, PtsChannelId: "c"}}
	deadline := make(chan time.Time)
	go func() {
		time.Sleep(50 * time.Millisecond)
		events <- gateway.Event{Event: &pb.Event{Pts: 7, PtsCount: 1}}
	}()
	if err := awaitUpdate(context.Background(), events, deadline); err != nil {
		t.Fatal(err)
	}
	expired := make(chan time.Time, 1)
	expired <- time.Now()
	if err := awaitUpdate(context.Background(), make(chan any), expired); !errors.Is(err, errPollTimeout) {
		t.Fatalf("a poll that saw nothing: %v", err)
	}
}

func TestBotAPIWantsABotToken(t *testing.T) {
	h := &Handler{logger: zap.NewNop()}
	for token, want := range map[string]int{"": http.StatusUnauthorized, secure.Token(): http.StatusUnauthorized} {
		r := httptest.NewRequest(http.MethodPost, "/api/v2/bot/getMe", strings.NewReader("{}"))
		r.SetPathValue("method", "getMe")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.botAPI(w, r)
		var reply botReply
		_ = json.Unmarshal(w.Body.Bytes(), &reply)
		if w.Code != want || reply.OK || reply.ErrorCode != want {
			t.Errorf("token %q: %d %+v", token, w.Code, reply)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v2/bot/nope", nil)
	r.SetPathValue("method", "nope")
	r.Header.Set("Authorization", "Bearer "+secure.BotToken())
	w := httptest.NewRecorder()
	h.botAPI(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("an unknown method: %d", w.Code)
	}
}

// directChannels opens a bot's direct conversations while it has some
// left.
type directChannels struct {
	pb.DRPCChannelServiceUnimplementedServer
	token  string
	mu     sync.Mutex
	left   int
	opened []*pb.OpenDirectChannelRequest
}

func (f *directChannels) OpenDirectChannel(ctx context.Context, req *pb.OpenDirectChannelRequest) (*pb.OpenDirectChannelResponse, error) {
	if md, _ := drpcmetadata.Get(ctx); md["x-session"] != f.token {
		return nil, rpcErr(ierrors.CodeUnauthenticated, "sign in")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.left == 0 {
		return nil, rpcErr(ierrors.CodeResourceExhausted, "the bot opened too many conversations this hour, try again later")
	}
	f.left--
	f.opened = append(f.opened, req)
	return &pb.OpenDirectChannelResponse{Channel: &pb.Channel{Id: testChannel, TeamId: req.TeamId, Type: pb.ChannelType_CHANNEL_TYPE_DIRECT}}, nil
}

func TestBotAPIOpensADirectConversation(t *testing.T) {
	token := secure.BotToken()
	f := &directChannels{token: token, left: 1}
	b := newGateway(t, func(mux *drpcmux.Mux) error { return pb.DRPCRegisterChannelService(mux, f) })
	open := func() (int, botReply) {
		r := httptest.NewRequest(http.MethodPost, flowOrigin+"/api/v2/bot/openDirect", strings.NewReader(`{"team_id": "`+testTeam+`", "user_id": "`+testPeer+`"}`))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		b.h.ServeHTTP(w, r)
		var reply botReply
		_ = json.Unmarshal(w.Body.Bytes(), &reply)
		return w.Code, reply
	}
	status, reply := open()
	if status != http.StatusOK || !reply.OK || !strings.Contains(string(reply.Result), `"id":"`+testChannel+`"`) {
		t.Fatalf("openDirect: %d %+v", status, reply)
	}
	if len(f.opened) != 1 || f.opened[0].TeamId != testTeam || f.opened[0].UserId != testPeer {
		t.Fatalf("asked %v", f.opened)
	}
	if status, reply := open(); status != http.StatusTooManyRequests || reply.OK || reply.ErrorCode != http.StatusTooManyRequests || !strings.Contains(reply.Description, "too many conversations") {
		t.Fatalf("openDirect past the limit: %d %+v", status, reply)
	}
}
