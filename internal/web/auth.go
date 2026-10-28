package web

import (
	"net/http"
	"net/url"
	"time"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/client/drpc"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

const mfaCookieTTL = 5 * time.Minute

type loginData struct {
	Page
	Next  string
	Login string
	// Providers are the single sign-on buttons, and Link the account at
	// one that signing in links.
	Providers []*pb.SSOProvider
	Link      *pb.GetSSOLinkResponse
}

func (h *Handler) loginData(w http.ResponseWriter, r *http.Request, next, msg string) loginData {
	return loginData{
		Page: Page{Title: "Sign in", CSRF: h.csrfToken(w, r), Error: msg}, Next: next,
		Providers: h.ssoProviders(r), Link: h.pendingLink(r),
	}
}

// nextParam keeps a redirect target only when it stays on this site.
func nextParam(r *http.Request) string {
	n := r.FormValue("next")
	if safeNext(n) != n {
		return ""
	}
	return n
}

func (h *Handler) anonymousCtx(r *http.Request) drpc.Call {
	c := h.call(r)
	c.Session = ""
	return c
}

func (h *Handler) startSession(w http.ResponseWriter, s *pb.Session) {
	ttl := time.Until(s.GetExpiresAt().AsTime())
	h.setCookie(w, "session", s.Token, ttl, http.SameSiteLaxMode)
	// A new session gets a new CSRF token.
	h.setCookie(w, "csrf", secure.Token(), 0, http.SameSiteLaxMode)
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	next := nextParam(r)
	if u, _ := h.currentUser(r); u != nil {
		http.Redirect(w, r, safeNext(next), http.StatusSeeOther) //nolint:gosec // safeNext keeps it on this site
		return
	}
	h.page(w, r, http.StatusOK, "login", h.loginData(w, r, next, ""))
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	next := nextParam(r)
	login := r.PostFormValue("login")
	ctx, cancel := h.client.Context(r.Context(), h.anonymousCtx(r))
	defer cancel()
	res, err := h.client.Auth.Login(ctx, &pb.LoginRequest{Login: login, Password: r.PostFormValue("password")})
	if err != nil {
		msg, ok := formError(err)
		if !ok {
			msg = drpc.Err(err).Message
		}
		if len(login) > 254 {
			login = ""
		}
		data := h.loginData(w, r, next, msg)
		data.Login = login
		h.page(w, r, statusOf(drpc.Err(err).Code), "login", data)
		return
	}
	if token := res.GetMfaToken(); token != "" {
		h.setCookie(w, "mfa", token, mfaCookieTTL, http.SameSiteStrictMode)
		target := "/login/mfa"
		if next != "" {
			target += "?next=" + url.QueryEscape(next)
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	h.startSession(w, res.GetSession())
	http.Redirect(w, r, h.afterSignIn(r, next), http.StatusSeeOther)
}

func (h *Handler) mfaToken(r *http.Request) string {
	c, err := r.Cookie(h.cookieName("mfa"))
	if err != nil || !secure.ValidToken(c.Value) {
		return ""
	}
	return c.Value
}

func (h *Handler) mfaPage(w http.ResponseWriter, r *http.Request) {
	if h.mfaToken(r) == "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	h.page(w, r, http.StatusOK, "mfa", loginData{Page: Page{Title: "Verify", CSRF: h.csrfToken(w, r)}, Next: nextParam(r)})
}

func (h *Handler) mfa(w http.ResponseWriter, r *http.Request) {
	next := nextParam(r)
	token := h.mfaToken(r)
	if token == "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ctx, cancel := h.client.Context(r.Context(), h.anonymousCtx(r))
	defer cancel()
	res, err := h.client.Auth.VerifyMFA(ctx, &pb.VerifyMFARequest{MfaToken: token, Code: r.PostFormValue("code")})
	if err != nil {
		e := drpc.Err(err)
		if e.Code == ierrors.CodeFailedPrecondition {
			h.clearCookie(w, "mfa")
			h.page(w, r, http.StatusUnauthorized, "login", h.loginData(w, r, next, e.Message))
			return
		}
		h.page(w, r, statusOf(e.Code), "mfa", loginData{Page: Page{Title: "Verify", CSRF: h.csrfToken(w, r), Error: e.Message}, Next: next})
		return
	}
	h.clearCookie(w, "mfa")
	h.startSession(w, res.Session)
	http.Redirect(w, r, h.afterSignIn(r, next), http.StatusSeeOther)
}

type signupForm struct {
	Username, DisplayName, Email string
}

type signupData struct {
	Page
	Providers   []*pb.SSOProvider
	Allowed     bool
	Bootstrap   bool
	SetupToken  bool
	SetupClosed bool
	Invite      string
	TeamName    string
	Next        string
	Form        signupForm
}

func signupAllowed(info *pb.GetSignupInfoResponse) bool {
	return !info.SetupClosed && (info.Bootstrap || info.Open || info.InviteValid)
}

func (h *Handler) signupInfo(r *http.Request, invite string) (*pb.GetSignupInfoResponse, error) {
	if invite != "" && !secure.ValidToken(invite) {
		invite = ""
	}
	ctx, cancel := h.client.Context(r.Context(), h.anonymousCtx(r))
	defer cancel()
	return h.client.Auth.GetSignupInfo(ctx, &pb.GetSignupInfoRequest{InviteToken: invite})
}

func (h *Handler) signupPage(w http.ResponseWriter, r *http.Request) {
	if u, _ := h.currentUser(r); u != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	invite := r.URL.Query().Get("invite")
	info, err := h.signupInfo(r, invite)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	data := signupData{
		Page:        Page{Title: "Sign up", CSRF: h.csrfToken(w, r)},
		Providers:   h.ssoProviders(r),
		Allowed:     signupAllowed(info),
		Bootstrap:   info.Bootstrap,
		SetupToken:  info.SetupTokenRequired,
		SetupClosed: info.SetupClosed,
		TeamName:    info.TeamDisplayName,
		Next:        nextParam(r),
	}
	if info.InviteValid {
		data.Invite = invite
	}
	h.page(w, r, http.StatusOK, "signup", data)
}

func (h *Handler) signup(w http.ResponseWriter, r *http.Request) {
	invite := r.PostFormValue("invite")
	form := signupForm{
		Username:    r.PostFormValue("username"),
		DisplayName: r.PostFormValue("display_name"),
		Email:       r.PostFormValue("email"),
	}
	info, err := h.signupInfo(r, invite)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	data := signupData{
		Page:        Page{Title: "Sign up", CSRF: h.csrfToken(w, r)},
		Providers:   h.ssoProviders(r),
		Allowed:     signupAllowed(info),
		Bootstrap:   info.Bootstrap,
		SetupToken:  info.SetupTokenRequired,
		SetupClosed: info.SetupClosed,
		TeamName:    info.TeamDisplayName,
		Form:        form,
	}
	if info.InviteValid {
		data.Invite = invite
	}
	pass := r.PostFormValue("password")
	if pass != r.PostFormValue("confirm") {
		data.Error = "The passwords do not match."
		h.page(w, r, http.StatusBadRequest, "signup", data)
		return
	}
	ctx, cancel := h.client.Context(r.Context(), h.anonymousCtx(r))
	defer cancel()
	res, err := h.client.Auth.SignUp(ctx, &pb.SignUpRequest{
		Username: form.Username, Email: form.Email, DisplayName: form.DisplayName, Password: pass, InviteToken: data.Invite,
		SetupToken: r.PostFormValue("setup_token"),
	})
	if err != nil {
		data.Error = drpc.Err(err).Message
		h.page(w, r, statusOf(drpc.Err(err).Code), "signup", data)
		return
	}
	h.startSession(w, res.Session)
	switch {
	case res.Team != nil:
		http.Redirect(w, r, "/t/"+res.Team.Name, http.StatusSeeOther)
	case info.Bootstrap:
		http.Redirect(w, r, "/teams/new", http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if token := h.sessionToken(r); token != "" {
		ctx, cancel := h.ctx(r)
		_, _ = h.client.Auth.Logout(ctx, &pb.LogoutRequest{})
		cancel()
		h.me.Delete(token)
	}
	h.clearCookie(w, "session")
	h.reload(w, r, "/login")
}

type joinData struct {
	Page
	Valid    bool
	TeamName string
	Token    string
}

func (h *Handler) joinData(w http.ResponseWriter, r *http.Request) (joinData, error) {
	token := r.PathValue("token")
	u, _ := h.currentUser(r)
	data := joinData{Page: Page{Title: "Join", CSRF: h.csrfToken(w, r), User: u}}
	if !secure.ValidToken(token) {
		return data, nil
	}
	ctx, cancel := h.client.Context(r.Context(), h.anonymousCtx(r))
	defer cancel()
	res, err := h.client.Teams.GetInvite(ctx, &pb.GetInviteRequest{Token: token})
	if err != nil {
		return data, err
	}
	data.Valid, data.TeamName, data.Token = res.Valid, res.TeamDisplayName, token
	return data, nil
}

func (h *Handler) joinPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	data, err := h.joinData(w, r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.page(w, r, http.StatusOK, "join", data)
}

func (h *Handler) join(w http.ResponseWriter, r *http.Request) {
	data, err := h.joinData(w, r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if data.User == nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape("/join/"+r.PathValue("token")), http.StatusSeeOther)
		return
	}
	if !data.Valid {
		h.page(w, r, http.StatusNotFound, "join", data)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.client.Teams.AcceptInvite(ctx, &pb.AcceptInviteRequest{Token: data.Token})
	if err != nil {
		if msg, ok := formError(err); ok {
			data.Error = msg
			h.page(w, r, statusOf(drpc.Err(err).Code), "join", data)
			return
		}
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/t/"+res.Team.Name, http.StatusSeeOther)
}
