package web

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
	"storj.io/drpc/drpcerr"
	"storj.io/drpc/drpcmetadata"
	"storj.io/drpc/drpcmux"
	"storj.io/drpc/drpcserver"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/client/drpc"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/web/render"
)

// fakeAuth answers the gateway's sign-in calls as the server would, for a
// provider "corp" whose codes say how the sign-in ends.
type fakeAuth struct {
	pb.DRPCAuthServiceUnimplementedServer
	mu      sync.Mutex
	begun   []*pb.BeginSSORequest
	linked  []string
	session string
}

func (f *fakeAuth) ListSSOProviders(context.Context, *pb.ListSSOProvidersRequest) (*pb.ListSSOProvidersResponse, error) {
	return &pb.ListSSOProvidersResponse{Providers: []*pb.SSOProvider{{Id: "corp", Name: "Corp ID"}}}, nil
}

func (f *fakeAuth) BeginSSO(_ context.Context, req *pb.BeginSSORequest) (*pb.BeginSSOResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.begun = append(f.begun, req)
	state := secure.Token()
	return &pb.BeginSSOResponse{Url: "https://id.corp.test/auth?state=" + state, State: state}, nil
}

func (f *fakeAuth) newSession() *pb.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.session = secure.Token()
	return &pb.Session{Token: f.session, Id: "s", ExpiresAt: timestamppb.New(time.Now().Add(time.Hour))}
}

func (f *fakeAuth) FinishSSO(_ context.Context, req *pb.FinishSSORequest) (*pb.FinishSSOResponse, error) {
	out := &pb.FinishSSOResponse{ProviderName: "Corp ID"}
	switch req.Code {
	case "ok":
		out.Result = &pb.FinishSSOResponse_Session{Session: f.newSession()}
	case "link":
		out.Result = &pb.FinishSSOResponse_LinkToken{LinkToken: strings.Repeat("L", 43)}
	case "app":
		out.Result = &pb.FinishSSOResponse_AppCode{AppCode: "app-code"}
	default:
		return nil, rpcErr(ierrors.CodePermissionDenied, "no account is linked")
	}
	return out, nil
}

func (f *fakeAuth) GetSSOLink(_ context.Context, req *pb.GetSSOLinkRequest) (*pb.GetSSOLinkResponse, error) {
	if req.LinkToken != strings.Repeat("L", 43) {
		return nil, rpcErr(ierrors.CodeFailedPrecondition, "expired")
	}
	return &pb.GetSSOLinkResponse{ProviderName: "Corp ID", Email: "ann@corp.test"}, nil
}

func (f *fakeAuth) signedIn(ctx context.Context) bool {
	md, _ := drpcmetadata.Get(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.session != "" && md["x-session"] == f.session
}

func (f *fakeAuth) LinkSSO(ctx context.Context, req *pb.LinkSSORequest) (*pb.LinkSSOResponse, error) {
	if !f.signedIn(ctx) {
		return nil, rpcErr(ierrors.CodeUnauthenticated, "sign in")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linked = append(f.linked, req.LinkToken)
	return &pb.LinkSSOResponse{ProviderName: "Corp ID"}, nil
}

func (f *fakeAuth) Login(context.Context, *pb.LoginRequest) (*pb.LoginResponse, error) {
	return &pb.LoginResponse{Result: &pb.LoginResponse_Session{Session: f.newSession()}}, nil
}

func (f *fakeAuth) Me(ctx context.Context, _ *pb.MeRequest) (*pb.MeResponse, error) {
	if !f.signedIn(ctx) {
		return nil, rpcErr(ierrors.CodeUnauthenticated, "sign in")
	}
	return &pb.MeResponse{User: &pb.User{Id: "u", Username: "ann"}, HasPassword: true}, nil
}

// rpcErr is an error as the server sends it.
func rpcErr(code ierrors.Code, msg string) error {
	return drpcerr.WithCode(errors.New(msg), uint64(code))
}

// browser keeps the cookies the gateway sets, as a browser would; with hx
// it asks as htmx does.
type browser struct {
	t       *testing.T
	h       http.Handler
	cookies map[string]*http.Cookie
	hx      bool
}

const flowOrigin = "http://chat.example.test"

func (b *browser) do(method, target string, form url.Values) *httptest.ResponseRecorder {
	b.t.Helper()
	var body io.Reader
	if form != nil {
		if c := b.cookies["gochat_csrf"]; c != nil {
			form.Set(csrfField, secure.MaskToken(c.Value))
		}
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, flowOrigin+target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", flowOrigin)
	}
	if b.hx {
		r.Header.Set("HX-Request", "true")
	}
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return w
}

func (b *browser) expect(w *httptest.ResponseRecorder, status int, location string) {
	b.t.Helper()
	if w.Code != status || w.Header().Get("Location") != location {
		b.t.Fatalf("got %d to %q, want %d to %q: %s", w.Code, w.Header().Get("Location"), status, location, w.Body.String())
	}
}

func newFlow(t *testing.T) (*browser, *fakeAuth) {
	t.Helper()
	auth := &fakeAuth{}
	return newGateway(t, func(mux *drpcmux.Mux) error { return pb.DRPCRegisterAuthService(mux, auth) }), auth
}

// newGateway serves the gateway in front of the services register adds.
func newGateway(t *testing.T, register func(*drpcmux.Mux) error) *browser {
	t.Helper()
	mux := drpcmux.New()
	if err := register(mux); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = drpcserver.New(mux).Serve(ctx, lis)
	}()
	client, err := drpc.New(&drpc.Config{
		Address: lis.Addr().String(), GatewayKey: "key", TLS: &drpc.TLSConfig{}, DialTimeout: time.Second,
		WriteTimeout: 5 * time.Second, RequestTimeout: 5 * time.Second, PoolCapacity: 4, PoolExpiration: time.Minute, MaxMessageSize: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		cancel()
		<-done
	})
	r, err := render.New(16)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(zap.NewNop(), &Config{
		PublicURL: flowOrigin, RateLimit: &RateLimitConfig{AuthPerMinute: 6000, AuthBurst: 100, RequestsPerSecond: 100, RequestBurst: 100},
		HTTP: &HTTPConfig{WriteTimeout: 10 * time.Second},
	}, client, r, nil, nil)
	return &browser{t: t, h: h.Routes(), cookies: make(map[string]*http.Cookie)}
}

// signIn holds token as its session, and a CSRF token.
func (b *browser) signIn(token string) {
	b.cookies["gochat_session"] = &http.Cookie{Name: "gochat_session", Value: token}
	b.cookies["gochat_csrf"] = &http.Cookie{Name: "gochat_csrf", Value: secure.Token()}
}

// toProvider starts a sign-in at the provider and returns the state its
// redirect carries.
func (b *browser) toProvider(target string) string {
	b.t.Helper()
	w := b.do(http.MethodGet, target, nil)
	loc, err := url.Parse(w.Header().Get("Location"))
	if w.Code != http.StatusSeeOther || err != nil || loc.Host != "id.corp.test" {
		b.t.Fatalf("start: %d %q", w.Code, w.Header().Get("Location"))
	}
	return loc.Query().Get("state")
}

func TestSSOSignInThroughTheGateway(t *testing.T) {
	b, auth := newFlow(t)
	if body := b.do(http.MethodGet, "/login?next=/t/team", nil).Body.String(); !strings.Contains(body, `href="/login/sso/corp?next=%2ft%2fteam"`) || !strings.Contains(body, "Sign in with Corp ID") {
		t.Fatalf("no button: %s", body)
	}
	b.toProvider("/login/sso/corp?next=/t/team")
	if got := auth.begun[0]; got.ProviderId != "corp" || got.RedirectUri != flowOrigin+ssoCallback || got.AppChallenge != "" {
		t.Fatalf("begin: %+v", got)
	}
	// Another browser's state does not finish this one's sign-in.
	b.expect(b.do(http.MethodGet, ssoCallback+"?code=ok&state="+secure.Token(), nil), http.StatusBadRequest, "")
	state := b.toProvider("/login/sso/corp?next=/t/team")
	b.expect(b.do(http.MethodGet, ssoCallback+"?code=ok&state="+state, nil), http.StatusSeeOther, "/t/team")
	if c := b.cookies["gochat_session"]; c == nil || c.Value != auth.session {
		t.Fatal("no session cookie")
	}
	if b.cookies["gochat_sso"] != nil {
		t.Fatal("the flow's cookie outlived it")
	}
	// The flow is spent.
	b.expect(b.do(http.MethodGet, ssoCallback+"?code=ok&state="+state, nil), http.StatusBadRequest, "")

	state = b.toProvider("/login/sso/corp")
	if w := b.do(http.MethodGet, ssoCallback+"?error=access_denied&state="+state, nil); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "The sign-in was cancelled.") {
		t.Fatalf("cancelled: %d", w.Code)
	}
	state = b.toProvider("/login/sso/corp")
	if w := b.do(http.MethodGet, ssoCallback+"?code=nobody&state="+state, nil); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "no account is linked") {
		t.Fatalf("refused: %d", w.Code)
	}
}

func TestSSOLinkAfterThePassword(t *testing.T) {
	b, auth := newFlow(t)
	state := b.toProvider("/login/sso/corp?next=/t/team")
	b.expect(b.do(http.MethodGet, ssoCallback+"?code=link&state="+state, nil), http.StatusSeeOther, "/login?next=%2Ft%2Fteam")
	page := b.do(http.MethodGet, "/login?next=%2Ft%2Fteam", nil).Body.String()
	if !strings.Contains(page, "Your Corp ID account ann@corp.test is not linked yet") || strings.Contains(page, "Sign in with Corp ID") {
		t.Fatalf("login page: %s", page)
	}
	b.expect(b.do(http.MethodPost, "/login", url.Values{"login": {"ann"}, "password": {"pw"}, "next": {"/t/team"}}), http.StatusSeeOther, "/login/sso/link?next=%2Ft%2Fteam")
	if len(auth.linked) != 0 {
		t.Fatal("linked before the owner said so")
	}
	if page := b.do(http.MethodGet, "/login/sso/link?next=%2Ft%2Fteam", nil).Body.String(); !strings.Contains(page, "ann@corp.test") || !strings.Contains(page, "@ann") {
		t.Fatalf("link page: %s", page)
	}
	b.expect(b.do(http.MethodPost, "/login/sso/link", url.Values{"next": {"/t/team"}}), http.StatusSeeOther, "/t/team")
	if len(auth.linked) != 1 || auth.linked[0] != strings.Repeat("L", 43) || b.cookies["gochat_sso_link"] != nil {
		t.Fatalf("linked: %v", auth.linked)
	}
}

func TestSSOHandsTheCodeToTheApp(t *testing.T) {
	b, auth := newFlow(t)
	challenge := strings.Repeat("c", 43)
	state := b.toProvider("/login/sso/corp?app=" + challenge)
	if auth.begun[0].AppChallenge != challenge {
		t.Fatalf("begin: %+v", auth.begun[0])
	}
	b.expect(b.do(http.MethodGet, ssoCallback+"?code=app&state="+state, nil), http.StatusSeeOther, appRedirect+"?code=app-code")
	state = b.toProvider("/login/sso/corp?app=" + challenge)
	b.expect(b.do(http.MethodGet, ssoCallback+"?code=nobody&state="+state, nil), http.StatusSeeOther, appRedirect+"?error=no+account+is+linked")
	if b.cookies["gochat_session"] != nil {
		t.Fatal("an app's sign-in left a session in the browser")
	}
}
