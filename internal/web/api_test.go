package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
	"golang.org/x/time/rate"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
)

func TestAPICallsAreUnaryCallsOfPeopleServices(t *testing.T) {
	n := 0
	for _, names := range apiCalls {
		n += len(names)
	}
	if len(apiMethods) != n {
		t.Fatalf("%d calls listed, %d resolved", n, len(apiMethods))
	}
	for full := range apiMethods {
		svc := strings.SplitN(full, "/", 2)[0]
		if svc == "gochat.v1.GatewayService" || svc == "gochat.v1.VoiceServerService" || svc == "gochat.v1.FileService" {
			t.Errorf("%s belongs to gateways or the file endpoints", full)
		}
	}
	for _, full := range []string{"gochat.v1.UserService/SetPhoto", "gochat.v1.TeamService/SetTeamPhoto", "gochat.v1.AdminService/SetUserDeactivated"} {
		if _, ok := apiMethods[full]; ok {
			t.Errorf("%s is open to apps", full)
		}
	}
	for full := range apiLimited {
		if _, ok := apiMethods[full]; !ok {
			t.Errorf("limited call %s is not a call", full)
		}
	}
}

func TestAPIToken(t *testing.T) {
	good := strings.Repeat("A", 43)
	for header, want := range map[string]string{
		"Bearer " + good:          good,
		"bearer " + good:          "",
		"Bearer " + good + "=":    "",
		"Bearer short":            "",
		"Basic " + good:           "",
		"":                        "",
		"Bearer  " + good:         "",
		"Bearer " + good + " xyz": "",
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := apiToken(r); got != want {
			t.Errorf("%q: got %q", header, got)
		}
	}
}

func TestEncodingOf(t *testing.T) {
	for mt, want := range map[string]int{
		"application/proto":               1,
		"application/x-protobuf":          1,
		"application/json":                2,
		"application/json; charset=utf-8": 2,
		"text/plain":                      0,
		"":                                0,
		"multipart/form-data; boundary=x": 0,
	} {
		enc, ok := encodingOf(mt)
		got := 0
		if ok {
			got = 1
			if enc.json {
				got = 2
			}
		}
		if got != want {
			t.Errorf("%q: got %d, want %d", mt, got, want)
		}
	}
}

// The refusals that need no chat server.
func TestAPIRPCRefusals(t *testing.T) {
	h := &Handler{logger: zap.NewNop(), auth: newIPLimiter(rate.Limit(1), 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/rpc/{service}/{method}", h.apiRPC)
	do := func(path, ctype, auth string) (*httptest.ResponseRecorder, *pb.ApiError) {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r.Header.Set("Content-Type", ctype)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		e := &pb.ApiError{}
		if ctype == "application/proto" {
			_ = proto.Unmarshal(w.Body.Bytes(), e)
		}
		return w, e
	}
	if w, _ := do("/api/v1/rpc/gochat.v1.AuthService/Me", "text/plain", ""); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain: %d", w.Code)
	}
	if w, e := do("/api/v1/rpc/gochat.v1.GatewayService/Connect", "application/proto", ""); w.Code != http.StatusNotFound || e.Code != 5 {
		t.Errorf("a gateway stream: %d %v", w.Code, e)
	}
	if w, e := do("/api/v1/rpc/gochat.v1.AuthService/Me", "application/proto", "Bearer nope"); w.Code != http.StatusUnauthorized || e.Code != 16 ||
		w.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("a malformed token: %d %v", w.Code, e)
	}
}

func TestAppCallsHaveBudgetsOfTheirOwn(t *testing.T) {
	slow := rate.Limit(0.001)
	h := &Handler{logger: zap.NewNop(), reqs: newIPLimiter(slow, 2), apiSessions: newIPLimiter(slow, 2), apiShared: newIPLimiter(slow, 5)}
	next := h.throttle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	call := func(path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		// Every phone behind one carrier address.
		r.RemoteAddr = "203.0.113.7:4000"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		next.ServeHTTP(w, r)
		return w
	}
	a, b := strings.Repeat("A", 43), strings.Repeat("B", 43)
	for _, token := range []string{a, a, b, b} {
		if w := call("/api/v1/rpc/gochat.v1.AuthService/Me", token); w.Code != http.StatusNoContent {
			t.Fatalf("a phone within its budget: %d", w.Code)
		}
	}
	// A session past its budget gets an ApiError.
	w := call("/api/v1/rpc/gochat.v1.AuthService/Me", a)
	e := &pb.ApiError{}
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" || protojson.Unmarshal(w.Body.Bytes(), e) != nil || e.Code != 8 {
		t.Fatalf("past its budget: %d %q", w.Code, w.Body.String())
	}
	// Made-up tokens run into the address's budget.
	if w := call("/api/v1/rpc/gochat.v1.AuthService/Me", strings.Repeat("C", 43)); w.Code != http.StatusTooManyRequests {
		t.Fatalf("a made-up token: %d", w.Code)
	}
	// Pages, and calls without a session, keep the address's own budget.
	for i, want := range []int{http.StatusNoContent, http.StatusNoContent, http.StatusTooManyRequests} {
		if w := call("/t/acme", ""); w.Code != want {
			t.Fatalf("page %d: %d", i, w.Code)
		}
	}
}

func TestAPITeamPhotoRefusals(t *testing.T) {
	h := &Handler{logger: zap.NewNop()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/teams/{team}/photo", h.apiSetTeamPhoto)
	do := func(team, auth string) (*httptest.ResponseRecorder, *pb.ApiError) {
		r := httptest.NewRequest(http.MethodPost, "/api/v2/teams/"+team+"/photo", strings.NewReader("picture"))
		r.Header.Set("Accept", "application/proto")
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		e := &pb.ApiError{}
		_ = proto.Unmarshal(w.Body.Bytes(), e)
		return w, e
	}
	token := "Bearer " + strings.Repeat("A", 43)
	if w, e := do("6f1c1f9e-8a51-4f3a-9d2e-1b7c2a9f0e11", ""); w.Code != http.StatusUnauthorized || e.Code != 16 {
		t.Errorf("without a token: %d %v", w.Code, e)
	}
	if w, e := do("acme", token); w.Code != http.StatusNotFound || e.Code != 5 {
		t.Errorf("a team that is no id: %d %v", w.Code, e)
	}
}
