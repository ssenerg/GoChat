package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
)

func TestDeliveries(t *testing.T) {
	var (
		mu   sync.Mutex
		sent []*pb.PostMessageRequest
	)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		in := &pb.PostMessageRequest{}
		if r.URL.Path != "/api/v2/bot/sendMessage" || r.Header.Get("Authorization") != "Bearer b_test" || jsonIn.Unmarshal(body, in) != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"bad call"}`)
			return
		}
		mu.Lock()
		sent = append(sent, in)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message":{"id":"m2"}}}`)
	}))
	defer api.Close()
	b := &bot{api: api.URL + "/api/v2/bot/", token: "b_test", secret: "s3cret", http: api.Client(), me: &pb.User{Id: "b1", Username: "ping_bot"}}

	post := func(content, author string) string {
		return `{"pts":"7","pts_count":1,"message_posted":{"message":{"id":"m1","channel_id":"c1","author":{"id":"` + author + `"},"content":"` + content + `"}}}`
	}
	for _, tc := range []struct {
		name, method, secret, body string
		status, sent               int
	}{
		{"no secret", http.MethodPost, "", post("/ping", "u1"), http.StatusUnauthorized, 0},
		{"a wrong secret", http.MethodPost, "s3cres", post("/ping", "u1"), http.StatusUnauthorized, 0},
		{"a GET", http.MethodGet, "s3cret", "", http.StatusMethodNotAllowed, 0},
		{"a ping", http.MethodPost, "s3cret", post("/ping", "u1"), http.StatusNoContent, 1},
		{"a ping to the bot", http.MethodPost, "s3cret", post("/ping@ping_bot", "u1"), http.StatusNoContent, 2},
		{"a ping to another bot", http.MethodPost, "s3cret", post("/ping@other_bot", "u1"), http.StatusNoContent, 2},
		{"the bot's own", http.MethodPost, "s3cret", post("/ping", "b1"), http.StatusNoContent, 2},
		{"a field it does not know", http.MethodPost, "s3cret", `{"pts":"8","pts_count":1,"brand_new":{"x":1}}`, http.StatusNoContent, 2},
		{"no JSON", http.MethodPost, "s3cret", "nope", http.StatusNoContent, 2},
	} {
		req := httptest.NewRequest(tc.method, "/", strings.NewReader(tc.body))
		if tc.secret != "" {
			req.Header.Set(secretHeader, tc.secret)
		}
		w := httptest.NewRecorder()
		b.ServeHTTP(w, req)
		mu.Lock()
		n := len(sent)
		mu.Unlock()
		if w.Code != tc.status || n != tc.sent {
			t.Errorf("%s: answered %d and sent %d messages, want %d and %d", tc.name, w.Code, n, tc.status, tc.sent)
		}
	}
	if m := sent[0]; m.ChannelId != "c1" || m.Content != "pong" {
		t.Errorf("sent %v", m)
	}
}
