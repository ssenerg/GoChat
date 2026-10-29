package web

import (
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/client/drpc"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

// Single sign-on: the providers' buttons, the round trip to a provider, and
// linking. The browser holds the flow's state in a cookie, which the
// provider's redirect back must match.

const (
	ssoCookieTTL = 15 * time.Minute
	// reauthTTL is how long the server takes a sign-in again.
	reauthTTL = 5 * time.Minute
	// appRedirect is where an app's sign-in comes back: a scheme the apps
	// claim, which then redeem the code with their PKCE verifier.
	appRedirect = "io.github.ssenerg.gochat:/sso"
	ssoCallback = "/login/sso/callback"
)

var providerIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ssoList caches the providers, which change only with the server's
// configuration.
type ssoList struct {
	mu   sync.Mutex
	list []*pb.SSOProvider
	at   time.Time
}

func (h *Handler) ssoProviders(r *http.Request) []*pb.SSOProvider {
	h.sso.mu.Lock()
	list, at := h.sso.list, h.sso.at
	h.sso.mu.Unlock()
	if time.Since(at) < time.Minute {
		return list
	}
	ctx, cancel := h.client.Context(r.Context(), h.anonymousCtx(r))
	defer cancel()
	res, err := h.client.Auth.ListSSOProviders(ctx, &pb.ListSSOProvidersRequest{})
	if err != nil {
		return list
	}
	h.sso.mu.Lock()
	h.sso.list, h.sso.at = res.Providers, time.Now()
	h.sso.mu.Unlock()
	return res.Providers
}

// ssoFlow is what the browser keeps of a sign-in at a provider.
type ssoFlow struct {
	State, Next, Invite string
	App                 bool
}

func (h *Handler) setSSOFlow(w http.ResponseWriter, f ssoFlow) {
	v := url.Values{"state": {f.State}}
	if f.Next != "" {
		v.Set("next", f.Next)
	}
	if f.Invite != "" {
		v.Set("invite", f.Invite)
	}
	if f.App {
		v.Set("app", "1")
	}
	h.setCookie(w, "sso", v.Encode(), ssoCookieTTL, http.SameSiteLaxMode)
}

func (h *Handler) ssoFlow(r *http.Request) (ssoFlow, bool) {
	c, err := r.Cookie(h.cookieName("sso"))
	if err != nil {
		return ssoFlow{}, false
	}
	v, err := url.ParseQuery(c.Value)
	if err != nil || !secure.ValidToken(v.Get("state")) {
		return ssoFlow{}, false
	}
	f := ssoFlow{State: v.Get("state"), Next: v.Get("next"), App: v.Get("app") == "1"}
	if safeNext(f.Next) != f.Next {
		f.Next = ""
	}
	if secure.ValidToken(v.Get("invite")) {
		f.Invite = v.Get("invite")
	}
	return f, true
}

// toApp sends an app's browser back to it, with the code or why not.
func toApp(w http.ResponseWriter, r *http.Request, key, value string) {
	http.Redirect(w, r, appRedirect+"?"+url.Values{key: {value}}.Encode(), http.StatusSeeOther)
}

// ssoFailed shows why a sign-in failed, on the sign-in page or in the app.
func (h *Handler) ssoFailed(w http.ResponseWriter, r *http.Request, f ssoFlow, status int, msg string) {
	if f.App {
		toApp(w, r, "error", msg)
		return
	}
	h.page(w, r, status, "login", h.loginData(w, r, f.Next, msg))
}

// ssoStart sends the browser to the provider. With ?app=, the S256
// challenge of an app's verifier, it comes back to the app.
func (h *Handler) ssoStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("provider")
	if !providerIDRe.MatchString(id) {
		h.notFound(w, r)
		return
	}
	q := r.URL.Query()
	f := ssoFlow{Next: nextParam(r), App: q.Has("app")}
	if secure.ValidToken(q.Get("invite")) {
		f.Invite = q.Get("invite")
	}
	ctx, cancel := h.client.Context(r.Context(), h.anonymousCtx(r))
	defer cancel()
	res, err := h.client.Auth.BeginSSO(ctx, &pb.BeginSSORequest{ProviderId: id, RedirectUri: h.cfg.PublicURL + ssoCallback, AppChallenge: q.Get("app")})
	if err != nil {
		e := drpc.Err(err)
		if e.Code == ierrors.CodeNotFound && !f.App {
			h.notFound(w, r)
			return
		}
		h.ssoFailed(w, r, f, statusOf(e.Code), e.Message)
		return
	}
	f.State = res.State
	h.setSSOFlow(w, f)
	http.Redirect(w, r, res.Url, http.StatusSeeOther)
}

// ssoLink starts linking an account at a provider to the signed-in one.
// It answers htmx with where to go: a form's redirect to another site
// would break the page's form-action policy.
func (h *Handler) ssoLink(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	id := r.PathValue("provider")
	if !providerIDRe.MatchString(id) {
		h.notFound(w, r)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Auth.BeginSSO(ctx, &pb.BeginSSORequest{ProviderId: id, RedirectUri: h.cfg.PublicURL + ssoCallback})
	if err != nil {
		h.ssoFragment(w, r, err, "")
		return
	}
	h.setSSOFlow(w, ssoFlow{State: res.State, Next: "/settings#sso"})
	h.reload(w, r, res.Url)
}

func (h *Handler) ssoCallbackPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	f, ok := h.ssoFlow(r)
	h.clearCookie(w, "sso")
	q := r.URL.Query()
	if !ok || !secure.Equal(q.Get("state"), f.State) {
		h.ssoFailed(w, r, f, http.StatusBadRequest, "The sign-in expired. Sign in again.")
		return
	}
	if e := q.Get("error"); e != "" {
		msg := "The provider did not sign you in."
		if e == "access_denied" {
			msg = "The sign-in was cancelled."
		}
		h.ssoFailed(w, r, f, http.StatusUnauthorized, msg)
		return
	}
	// The session, if any, is who links an account they asked to.
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Auth.FinishSSO(ctx, &pb.FinishSSORequest{State: f.State, Code: q.Get("code"), InviteToken: f.Invite})
	if err != nil {
		e := drpc.Err(err)
		h.ssoFailed(w, r, f, statusOf(e.Code), e.Message)
		return
	}
	next := safeNext(f.Next)
	if res.Team != nil && f.Next == "" {
		next = "/t/" + res.Team.Name
	}
	switch out := res.Result.(type) {
	case *pb.FinishSSOResponse_AppCode:
		toApp(w, r, "code", out.AppCode)
	case *pb.FinishSSOResponse_Session:
		h.startSession(w, out.Session)
		http.Redirect(w, r, h.afterSignIn(r, next), http.StatusSeeOther)
	case *pb.FinishSSOResponse_MfaToken:
		h.setCookie(w, "mfa", out.MfaToken, mfaCookieTTL, http.SameSiteStrictMode)
		http.Redirect(w, r, "/login/mfa?next="+url.QueryEscape(next), http.StatusSeeOther)
	case *pb.FinishSSOResponse_LinkToken:
		h.setCookie(w, "sso_link", out.LinkToken, ssoCookieTTL, http.SameSiteLaxMode)
		if u, _ := h.currentUser(r); u != nil {
			http.Redirect(w, r, "/login/sso/link?next="+url.QueryEscape(next), http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
	case *pb.FinishSSOResponse_Linked:
		http.Redirect(w, r, next, http.StatusSeeOther)
	case *pb.FinishSSOResponse_ReauthToken:
		v := url.Values{"token": {out.ReauthToken}, "provider": {res.ProviderName}}
		h.setCookie(w, reauthCookie, v.Encode(), reauthTTL, http.SameSiteLaxMode)
		http.Redirect(w, r, next, http.StatusSeeOther)
	default:
		h.ssoFailed(w, r, f, http.StatusBadGateway, "The server answered in a way this page does not understand.")
	}
}

// afterSignIn is where a sign-in goes on to: next, or first the link it
// was asked to make.
func (h *Handler) afterSignIn(r *http.Request, next string) string {
	next = safeNext(next)
	if h.linkToken(r) != "" {
		return "/login/sso/link?next=" + url.QueryEscape(next)
	}
	return next
}

func (h *Handler) linkToken(r *http.Request) string {
	c, err := r.Cookie(h.cookieName("sso_link"))
	if err != nil || !secure.ValidToken(c.Value) {
		return ""
	}
	return c.Value
}

// pendingLink describes the link the browser waits to make, or is nil.
func (h *Handler) pendingLink(r *http.Request) *pb.GetSSOLinkResponse {
	token := h.linkToken(r)
	if token == "" {
		return nil
	}
	ctx, cancel := h.client.Context(r.Context(), h.anonymousCtx(r))
	defer cancel()
	res, err := h.client.Auth.GetSSOLink(ctx, &pb.GetSSOLinkRequest{LinkToken: token})
	if err != nil {
		return nil
	}
	return res
}

type ssoLinkData struct {
	Page
	Link *pb.GetSSOLinkResponse
	Next string
}

func (h *Handler) ssoLinkPage(w http.ResponseWriter, r *http.Request) {
	u := h.signedIn(w, r)
	if u == nil {
		return
	}
	next := nextParam(r)
	link := h.pendingLink(r)
	if link == nil {
		h.clearCookie(w, "sso_link")
		http.Redirect(w, r, safeNext(next), http.StatusSeeOther) //nolint:gosec // safeNext keeps it on this site
		return
	}
	h.page(w, r, http.StatusOK, "sso-link", ssoLinkData{Page: Page{Title: "Link account", CSRF: h.csrfToken(w, r), User: u}, Link: link, Next: next})
}

// ssoLinkConfirm links the account the browser came back with, once the
// signed-in owner says so.
func (h *Handler) ssoLinkConfirm(w http.ResponseWriter, r *http.Request) {
	u := h.signedIn(w, r)
	if u == nil {
		return
	}
	next := nextParam(r)
	token := h.linkToken(r)
	h.clearCookie(w, "sso_link")
	if token == "" || r.PostFormValue("decline") == "1" {
		http.Redirect(w, r, safeNext(next), http.StatusSeeOther) //nolint:gosec // safeNext keeps it on this site
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	if _, err := h.client.Auth.LinkSSO(ctx, &pb.LinkSSORequest{LinkToken: token}); err != nil {
		msg, ok := formError(err)
		if !ok {
			h.fail(w, r, err)
			return
		}
		h.page(w, r, statusOf(drpc.Err(err).Code), "sso-link", ssoLinkData{Page: Page{Title: "Link account", CSRF: h.csrfToken(w, r), User: u, Error: msg}, Next: next})
		return
	}
	if next == "" {
		next = "/settings#sso"
	}
	http.Redirect(w, r, safeNext(next), http.StatusSeeOther) //nolint:gosec // safeNext keeps it on this site
}

type ssoData struct {
	Identities []*pb.SSOIdentity
	// Providers are those it may link, and HasPassword whether it signs in
	// with a password too.
	Providers     []*pb.SSOProvider
	HasPassword   bool
	Status, Error string
}

func (h *Handler) loadSSO(r *http.Request, hasPassword bool) (ssoData, error) {
	d := ssoData{Providers: h.ssoProviders(r), HasPassword: hasPassword}
	if len(d.Providers) == 0 {
		return d, nil
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Auth.ListSSOIdentities(ctx, &pb.ListSSOIdentitiesRequest{})
	if err != nil {
		return d, err
	}
	d.Identities = res.Identities
	return d, nil
}

// ssoFragment answers with the caller's linked accounts, and with what
// became of a change to them.
func (h *Handler) ssoFragment(w http.ResponseWriter, r *http.Request, err error, ok string) {
	ctx, cancel := h.ctx(r)
	me, merr := h.client.Auth.Me(ctx, &pb.MeRequest{})
	cancel()
	if merr != nil {
		h.fail(w, r, merr)
		return
	}
	d, lerr := h.loadSSO(r, me.HasPassword)
	if lerr != nil {
		h.fail(w, r, lerr)
		return
	}
	if err != nil {
		d.Error = drpc.Err(err).Message
	} else {
		d.Status = ok
	}
	h.fragment(w, r, http.StatusOK, "sso", d)
}

func (h *Handler) ssoUnlink(w http.ResponseWriter, r *http.Request) {
	if h.signedIn(w, r) == nil {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	_, err := h.client.Auth.UnlinkSSO(ctx, &pb.UnlinkSSORequest{Issuer: r.PostFormValue("issuer"), Subject: r.PostFormValue("subject")})
	h.ssoFragment(w, r, err, "Unlinked.")
}
