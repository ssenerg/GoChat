package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ssenerg/gochat/internal/core/secure"
)

func TestSSOGatewayCallsAreClosedToApps(t *testing.T) {
	for _, m := range []string{"BeginSSO", "FinishSSO", "GetSSOLink", "LinkSSO"} {
		if _, ok := apiMethods["gochat.v1.AuthService/"+m]; ok {
			t.Errorf("%s is open to apps", m)
		}
	}
	for full := range anonymousCalls {
		if _, ok := apiMethods[full]; !ok {
			t.Errorf("anonymous call %s is not a call", full)
		}
	}
	if !apiLimited["gochat.v1.AuthService/RedeemSSO"] {
		t.Error("redeeming a code is not on the sign-in budget")
	}
}

// cookies carries what w set to a new request for target.
func cookies(w *httptest.ResponseRecorder, target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	for _, c := range w.Result().Cookies() {
		r.AddCookie(c)
	}
	return r
}

func TestSSOFlowCookie(t *testing.T) {
	h := &Handler{}
	state, invite := secure.Token(), secure.Token()
	w := httptest.NewRecorder()
	h.setSSOFlow(w, ssoFlow{State: state, Next: "/t/team?x=1&y=2", Invite: invite, App: true})
	f, ok := h.ssoFlow(cookies(w, "/"))
	if !ok || f.State != state || f.Next != "/t/team?x=1&y=2" || f.Invite != invite || !f.App {
		t.Fatalf("round trip: %v %+v", ok, f)
	}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: h.cookieName("sso"), Value: url.Values{"state": {state}, "next": {"//evil.test"}, "invite": {"x"}}.Encode()})
	if f, ok := h.ssoFlow(r); !ok || f.Next != "" || f.Invite != "" {
		t.Fatalf("a tampered cookie: %+v", f)
	}
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: h.cookieName("sso"), Value: "state=short"})
	if _, ok := h.ssoFlow(r); ok {
		t.Fatal("a cookie without a state was taken")
	}
}

func TestSSOCallbackNeedsTheBrowsersState(t *testing.T) {
	h := &Handler{}
	w := httptest.NewRecorder()
	h.setSSOFlow(w, ssoFlow{State: secure.Token(), App: true})
	for _, query := range []string{"?code=c&state=" + secure.Token(), "?code=c"} {
		rec := httptest.NewRecorder()
		h.ssoCallbackPage(rec, cookies(w, ssoCallback+query))
		loc := rec.Header().Get("Location")
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, appRedirect+"?error=") {
			t.Fatalf("%s: %d %q", query, rec.Code, loc)
		}
		if c := rec.Result().Cookies(); len(c) != 1 || c[0].MaxAge >= 0 {
			t.Fatalf("%s: the flow's cookie was kept: %v", query, c)
		}
	}
}
