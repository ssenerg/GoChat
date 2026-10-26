package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/secure"
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
