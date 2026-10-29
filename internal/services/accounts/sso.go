package accounts

// Single sign-on over OpenID Connect: the authorization code flow with PKCE,
// a state and a nonce, and the ID token checked against the keys the issuer
// publishes. An identity is its issuer and subject, never its email.
//
// A provider replaces the password, not the second factor: an account with
// TOTP on still answers for its code.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gocql/gocql"
	"go.uber.org/zap"
	"golang.org/x/oauth2"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/secure"
	"github.com/ssenerg/gochat/internal/core/validate"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/convert"
)

// ssoTicketTTL bounds how long an app's code waits to be redeemed.
const ssoTicketTTL = 2 * time.Minute

var (
	errSSOExpired = ierrors.FailedPrecondition("the sign-in expired, sign in again")
	errNoProvider = ierrors.NotFound("no such sign-in provider")
)

type ssoProvider struct {
	cfg    SSOProvider
	secret string
	client *http.Client
	mu     sync.Mutex
	// oidc is discovered on first use, so a provider down at start-up does
	// not stop the server.
	oidc *oidc.Provider
}

type ssoProviders struct {
	all  []*ssoProvider
	byID map[string]*ssoProvider
}

func newSSOProviders(cfg *SSOConfig) (*ssoProviders, error) {
	out := &ssoProviders{byID: make(map[string]*ssoProvider)}
	client := &http.Client{Timeout: cfg.Timeout}
	for _, c := range cfg.Providers {
		secret := c.ClientSecret
		if c.ClientSecretFile != "" {
			b, err := os.ReadFile(c.ClientSecretFile)
			if err != nil {
				return nil, err
			}
			secret = strings.TrimSpace(string(b))
		}
		p := &ssoProvider{cfg: c, secret: secret, client: client}
		out.all = append(out.all, p)
		out.byID[c.ID] = p
	}
	return out, nil
}

func (s *ssoProviders) list() []SSOProviderInfo {
	out := make([]SSOProviderInfo, len(s.all))
	for i, p := range s.all {
		out[i] = SSOProviderInfo{ID: p.cfg.ID, Name: p.cfg.Name}
	}
	return out
}

func (s *ssoProviders) get(id string) (*ssoProvider, error) {
	if p, ok := s.byID[id]; ok {
		return p, nil
	}
	return nil, errNoProvider
}

// name is what people know the issuer by, its id when no provider has it.
func (s *ssoProviders) name(issuer, id string) string {
	for _, p := range s.all {
		if p.cfg.Issuer == issuer {
			return p.cfg.Name
		}
	}
	return id
}

func (p *ssoProvider) discover(ctx context.Context) (*oidc.Provider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.oidc != nil {
		return p.oidc, nil
	}
	op, err := oidc.NewProvider(oidc.ClientContext(ctx, p.client), p.cfg.Issuer)
	if err != nil {
		return nil, &ierrors.Error{Code: ierrors.CodeUnavailable, Message: "cannot reach " + p.cfg.Name + ", try again later", Cause: err}
	}
	p.oidc = op
	return op, nil
}

func (p *ssoProvider) oauth(op *oidc.Provider, redirect string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.secret,
		Endpoint:     op.Endpoint(),
		RedirectURL:  redirect,
		Scopes:       append([]string{oidc.ScopeOpenID, "email", "profile"}, p.cfg.Scopes...),
	}
}

// flexBool takes a boolean claim some providers send as a string.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	if s, err := strconv.Unquote(string(data)); err == nil {
		data = []byte(s)
	}
	v, err := strconv.ParseBool(string(data))
	*b = flexBool(v)
	return err
}

type ssoClaims struct {
	Subject           string   `json:"sub"`
	AuthorizedParty   string   `json:"azp"`
	Email             string   `json:"email"`
	EmailVerified     flexBool `json:"email_verified"`
	Name              string   `json:"name"`
	PreferredUsername string   `json:"preferred_username"`
	AuthTime          int64    `json:"auth_time"`
}

// admits reports whether the claims satisfy the provider's domains.
func (p *ssoProvider) admits(c *ssoClaims) bool {
	if len(p.cfg.AllowedDomains) == 0 {
		return true
	}
	at := strings.LastIndexByte(c.Email, '@')
	return bool(c.EmailVerified) && at > 0 && slices.Contains(p.cfg.AllowedDomains, strings.ToLower(c.Email[at+1:]))
}

// validChallenge reports whether s is shaped like an S256 PKCE challenge.
func validChallenge(s string) bool {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == sha256.Size
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (w *worker) beginSSO(ctx context.Context, msg BeginSSO) (*SSOStart, error) {
	p, err := w.sso.get(msg.Provider)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(msg.RedirectURI)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(msg.RedirectURI) > 512 {
		return nil, ierrors.InvalidArgument("invalid redirect URI")
	}
	if msg.AppChallenge != "" && (msg.P != nil || !validChallenge(msg.AppChallenge)) {
		return nil, ierrors.InvalidArgument("invalid app challenge")
	}
	if msg.P != nil && msg.P.Bot {
		return nil, ierrors.PermissionDenied()
	}
	if msg.Reauth && msg.P == nil && msg.AppChallenge == "" {
		return nil, ierrors.InvalidArgument("signing in again needs a session or an app")
	}
	op, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	state, nonce, verifier := secure.Token(), secure.Token(), oauth2.GenerateVerifier()
	now := time.Now().UTC()
	f := &models.SSOFlow{
		StateHash:    secure.Hash(state),
		Provider:     p.cfg.ID,
		Nonce:        nonce,
		RedirectURI:  msg.RedirectURI,
		AppChallenge: msg.AppChallenge,
		ExpiresAt:    now.Add(w.Cfg.SSO.FlowTTL),
		Reauth:       msg.Reauth,
	}
	f.Verifier = w.sealer.Seal([]byte(verifier), f.StateHash)
	if msg.P != nil {
		f.LinkUser = msg.P.UserID
	}
	if err := w.Repo.CreateSSOFlow(ctx, f, now); err != nil {
		return nil, err
	}
	opts := []oauth2.AuthCodeOption{oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)}
	// A sign-in again must ask for the password, not ride a live session.
	if msg.Reauth {
		opts = append(opts, oauth2.SetAuthURLParam("max_age", "0"))
	}
	return &SSOStart{URL: p.oauth(op, msg.RedirectURI).AuthCodeURL(state, opts...), State: state}, nil
}

// identify trades the code for the ID token and checks it: signed by the
// issuer's keys, issued by it to this client, unexpired, with the flow's
// nonce.
func (w *worker) identify(ctx context.Context, p *ssoProvider, f *models.SSOFlow, code string) (*ssoClaims, error) {
	failed := ierrors.New(ierrors.CodeUnauthenticated, p.cfg.Name+" did not sign you in, try again")
	op, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	verifier, err := w.sealer.Open(f.Verifier, f.StateHash)
	if err != nil {
		return nil, ierrors.Internal(err)
	}
	ctx = oidc.ClientContext(ctx, p.client)
	tok, err := p.oauth(op, f.RedirectURI).Exchange(ctx, code, oauth2.VerifierOption(string(verifier)))
	if err != nil {
		w.Logger.Warn("sso code exchange failed", zap.String("provider", p.cfg.ID), zap.Error(err))
		return nil, failed
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		w.Logger.Warn("sso token response had no ID token", zap.String("provider", p.cfg.ID))
		return nil, failed
	}
	idt, err := op.Verifier(&oidc.Config{ClientID: p.cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		w.Logger.Warn("sso ID token refused", zap.String("provider", p.cfg.ID), zap.Error(err))
		return nil, failed
	}
	if !secure.Equal(idt.Nonce, f.Nonce) {
		w.Logger.Warn("sso ID token has another nonce", zap.String("provider", p.cfg.ID))
		return nil, failed
	}
	var c ssoClaims
	if err := idt.Claims(&c); err != nil || c.Subject == "" || len(c.Subject) > 255 {
		w.Logger.Warn("sso ID token claims unusable", zap.String("provider", p.cfg.ID), zap.Error(err))
		return nil, failed
	}
	// A token for several clients names the one it was given to.
	if len(idt.Audience) > 1 && c.AuthorizedParty != p.cfg.ClientID {
		w.Logger.Warn("sso ID token given to another client", zap.String("provider", p.cfg.ID))
		return nil, failed
	}
	// Some providers keep the email for their userinfo endpoint.
	if c.Email == "" && op.UserInfoEndpoint() != "" {
		if info, err := op.UserInfo(ctx, oauth2.StaticTokenSource(tok)); err == nil && info.Subject == c.Subject {
			var more ssoClaims
			if info.Claims(&more) == nil {
				c.Email, c.EmailVerified = more.Email, more.EmailVerified
				if c.Name == "" {
					c.Name = more.Name
				}
			}
		}
	}
	c.Email = strings.ToLower(strings.TrimSpace(c.Email))
	return &c, nil
}

func (w *worker) finishSSO(ctx context.Context, msg FinishSSO) (*SSOResult, error) {
	if !secure.ValidToken(msg.State) {
		return nil, errSSOExpired
	}
	f, err := w.Repo.ConsumeSSOFlow(ctx, secure.Hash(msg.State))
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return nil, errSSOExpired
		}
		return nil, err
	}
	p, err := w.sso.get(f.Provider)
	if err != nil || !time.Now().Before(f.ExpiresAt) {
		return nil, errSSOExpired
	}
	if msg.Code == "" || len(msg.Code) > 4096 {
		return nil, ierrors.InvalidArgument("the provider gave no code")
	}
	c, err := w.identify(ctx, p, f, msg.Code)
	if err != nil {
		return nil, err
	}
	if !p.admits(c) {
		return nil, ierrors.New(ierrors.CodePermissionDenied, "sign in with a "+p.cfg.Name+" account of "+strings.Join(p.cfg.AllowedDomains, ", "))
	}
	id := &models.SSOIdentity{Issuer: p.cfg.Issuer, Subject: c.Subject, Provider: p.cfg.ID, Email: c.Email}
	userID, err := w.Repo.GetSSOIdentity(ctx, id.Issuer, id.Subject)
	known := err == nil
	if err != nil && !ierrors.Is(err, ierrors.CodeNotFound) {
		return nil, err
	}
	if known {
		if known, err = w.stillHeld(ctx, id, userID); err != nil {
			return nil, err
		}
	}
	out := &SSOResult{Provider: p.cfg.Name}

	if f.Reauth {
		if !w.signedInSince(c, f) {
			w.Logger.Warn("sso reauth without a fresh sign-in", zap.String("provider", p.cfg.ID))
			return nil, ierrors.FailedPrecondition(p.cfg.Name + " did not ask you to sign in again: sign out of it, then try again")
		}
		return w.reauthed(ctx, p, f, id, msg.P, out)
	}
	if !ids.IsZero(f.LinkUser) {
		if msg.P == nil || msg.P.UserID != f.LinkUser {
			return nil, ierrors.New(ierrors.CodePermissionDenied, "sign in to the account you were linking, then link it again")
		}
		if known && userID != f.LinkUser {
			return nil, errLinkedElsewhere(p.cfg.Name)
		}
		if !known {
			if err := w.link(ctx, id, f.LinkUser); err != nil {
				return nil, err
			}
		}
		out.Linked = true
		return out, nil
	}

	var user *models.User
	if known {
		if user, err = w.Repo.GetUser(ctx, userID); err != nil {
			return nil, err
		}
	} else {
		taken, err := w.emailTaken(ctx, c.Email)
		if err != nil {
			return nil, err
		}
		// An account with the email links only once its owner signs in.
		if taken {
			if f.AppChallenge != "" {
				return nil, ierrors.FailedPrecondition("an account has this email: sign in with its password once, then link " + p.cfg.Name + " in Settings on the web")
			}
			out.LinkToken, err = w.ticket(ctx, &models.SSOTicket{Kind: models.SSOTicketLink, Provider: p.cfg.ID, Issuer: id.Issuer, Subject: id.Subject, Email: c.Email}, w.Cfg.SSO.FlowTTL)
			return out, err
		}
		if user, err = w.ssoSignUp(ctx, p, c, id, msg.InviteToken, out); err != nil {
			return nil, err
		}
	}
	if user.Deactivated {
		return nil, ierrors.New(ierrors.CodePermissionDenied, "account is deactivated")
	}
	w.Logger.Info("sso sign-in", zap.String("user", user.ID.String()), zap.String("provider", p.cfg.ID))
	if f.AppChallenge != "" {
		out.AppCode, err = w.ticket(ctx, &models.SSOTicket{Kind: models.SSOTicketApp, UserID: user.ID, Challenge: f.AppChallenge, Provider: p.cfg.ID}, ssoTicketTTL)
		return out, err
	}
	out.Login, err = w.signIn(ctx, user, msg.IP, msg.UserAgent)
	return out, err
}

// reauthTTL bounds how long a reauth proves who signed in.
const reauthTTL = 5 * time.Minute

// authSkew allows for a provider's clock running behind this one.
const authSkew = time.Minute

// signedInSince reports whether the provider says the person signed in
// after the flow began, as max_age=0 asks; one that does not say is not
// taken at its word.
func (w *worker) signedInSince(c *ssoClaims, f *models.SSOFlow) bool {
	began := f.ExpiresAt.Add(-w.Cfg.SSO.FlowTTL)
	return c.AuthTime > 0 && !time.Unix(c.AuthTime, 0).Before(began.Add(-authSkew))
}

// stillHeld reports whether the identity's account is still there. One
// whose deletion failed to unlink it is freed here instead.
func (w *worker) stillHeld(ctx context.Context, id *models.SSOIdentity, userID gocql.UUID) (bool, error) {
	u, err := w.Repo.GetUser(ctx, userID)
	if err != nil && !ierrors.Is(err, ierrors.CodeNotFound) {
		return false, err
	}
	if err == nil && u.DeletedAt.IsZero() {
		return true, nil
	}
	stale := &models.SSOIdentity{UserID: userID, Issuer: id.Issuer, Subject: id.Subject}
	if err := w.Repo.UnlinkSSOIdentity(ctx, stale); err != nil {
		return false, err
	}
	w.Logger.Info("freed a deleted account's sign-in", zap.String("user", userID.String()))
	return false, nil
}

// reauthed proves who signed in again at a provider: only someone whose
// account it is linked to, and with a session, the one who began.
func (w *worker) reauthed(ctx context.Context, p *ssoProvider, f *models.SSOFlow, id *models.SSOIdentity, caller *models.Principal, out *SSOResult) (*SSOResult, error) {
	userID, err := w.Repo.GetSSOIdentity(ctx, id.Issuer, id.Subject)
	if ierrors.Is(err, ierrors.CodeNotFound) || (err == nil && !ids.IsZero(f.LinkUser) && userID != f.LinkUser) {
		return nil, ierrors.FailedPrecondition("sign in with the " + p.cfg.Name + " account linked to yours")
	}
	if err != nil {
		return nil, err
	}
	if !ids.IsZero(f.LinkUser) && (caller == nil || caller.UserID != f.LinkUser) {
		return nil, ierrors.New(ierrors.CodePermissionDenied, "sign in to the account you were deleting, then try again")
	}
	token, err := w.ticket(ctx, &models.SSOTicket{Kind: models.SSOTicketReauth, UserID: userID, Challenge: f.AppChallenge, Provider: p.cfg.ID}, reauthTTL)
	if err != nil {
		return nil, err
	}
	if f.AppChallenge != "" {
		out.AppCode = token
	} else {
		out.ReauthToken = token
	}
	return out, nil
}

func (w *worker) emailTaken(ctx context.Context, email string) (bool, error) {
	if email == "" {
		return false, nil
	}
	_, err := w.Repo.GetUserIDByEmail(ctx, email)
	if ierrors.Is(err, ierrors.CodeNotFound) {
		return false, nil
	}
	return err == nil, err
}

func errLinkedElsewhere(name string) error {
	return ierrors.FailedPrecondition("this " + name + " account is linked to another GoChat account")
}

// ticket stores t under a new token and returns it.
func (w *worker) ticket(ctx context.Context, t *models.SSOTicket, ttl time.Duration) (string, error) {
	token := secure.Token()
	now := time.Now().UTC()
	t.TokenHash, t.ExpiresAt = secure.Hash(token), now.Add(ttl)
	if err := w.Repo.CreateSSOTicket(ctx, t, now); err != nil {
		return "", err
	}
	return token, nil
}

// takeTicket consumes the ticket of token, if it is of kind and current.
func (w *worker) takeTicket(ctx context.Context, token, kind string) (*models.SSOTicket, error) {
	if !secure.ValidToken(token) {
		return nil, errSSOExpired
	}
	hash := secure.Hash(token)
	t, err := w.Repo.GetSSOTicket(ctx, hash)
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return nil, errSSOExpired
		}
		return nil, err
	}
	if t.Kind != kind || !time.Now().Before(t.ExpiresAt) {
		return nil, errSSOExpired
	}
	ok, err := w.Repo.ConsumeSSOTicket(ctx, hash)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errSSOExpired
	}
	return t, nil
}

func (w *worker) link(ctx context.Context, id *models.SSOIdentity, userID gocql.UUID) error {
	id.UserID, id.LinkedAt = userID, time.Now().UTC()
	ok, err := w.Repo.LinkSSOIdentity(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return errLinkedElsewhere(w.sso.name(id.Issuer, id.Provider))
	}
	w.Logger.Info("sso identity linked", zap.String("user", userID.String()), zap.String("provider", id.Provider))
	return nil
}

// ssoSignUp creates the account of a new identity, as SignUp would: with an
// invite, or while sign-up is open, and never the first account.
func (w *worker) ssoSignUp(ctx context.Context, p *ssoProvider, c *ssoClaims, id *models.SSOIdentity, invite string, out *SSOResult) (*models.User, error) {
	var (
		inv *models.Invite
		err error
	)
	if invite != "" {
		if inv, err = w.validInvite(ctx, invite); err != nil {
			return nil, err
		}
	}
	bootstrapped, err := w.Repo.Bootstrapped(ctx)
	if err != nil {
		return nil, err
	}
	if !bootstrapped {
		return nil, ierrors.FailedPrecondition("create the first account with the sign-up form")
	}
	if inv == nil && !w.Cfg.OpenSignup {
		return nil, ierrors.New(ierrors.CodePermissionDenied, "no account is linked to this "+p.cfg.Name+" account: sign up with an invite, or sign in with your password and link it in Settings")
	}
	if !c.EmailVerified {
		return nil, ierrors.New(ierrors.CodePermissionDenied, p.cfg.Name+" did not share a verified email address")
	}
	email, err := validate.Email(c.Email)
	if err != nil {
		return nil, err
	}
	username, err := w.freeUsername(ctx, c)
	if err != nil {
		return nil, err
	}
	displayName, err := validate.Line("display name", c.Name, 1, 64)
	if err != nil {
		displayName = username
	}
	now := time.Now().UTC()
	user := &models.User{
		ID: ids.New(), Username: username, Email: email, DisplayName: displayName,
		Role: models.SystemRoleUser, CreatedAt: now, UpdatedAt: now,
	}
	if inv != nil {
		ok, err := w.Repo.ConsumeInvite(ctx, inv, user.ID, now)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ierrors.NotFound("invite is invalid or expired")
		}
	}
	if err := w.Repo.CreateUser(ctx, user); err != nil {
		if inv != nil {
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = w.Repo.ReleaseInvite(rctx, inv, user.ID, time.Now())
		}
		return nil, err
	}
	w.Logger.Info("account created", zap.String("user", user.ID.String()), zap.String("role", string(user.Role)), zap.String("provider", p.cfg.ID))
	if err := w.link(ctx, id, user.ID); err != nil {
		return nil, err
	}
	out.User = user
	if inv != nil {
		out.InviteTeamID, out.InviteHash = inv.TeamID, inv.TokenHash
	}
	return user, nil
}

// freeUsername makes a username of the claims that no one has yet.
func (w *worker) freeUsername(ctx context.Context, c *ssoClaims) (string, error) {
	base := usernameBase(c.PreferredUsername)
	if base == "" {
		base = usernameBase(c.Email)
	}
	if base == "" {
		base = "user"
	}
	for i := range 20 {
		name := base
		switch {
		case i > 0 && i < 10:
			name += strconv.Itoa(i + 1)
		case i >= 10:
			name += "-" + strings.ToLower(rand.Text()[:4])
		}
		if len(name) < 3 {
			name += "000"[:3-len(name)]
		}
		_, err := w.Repo.GetUserIDByUsername(ctx, name)
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", ierrors.Unavailable("no free username, try again")
}

// usernameBase keeps what a username may hold of s, an email's local part.
func usernameBase(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if at := strings.IndexByte(s, '@'); at >= 0 {
		s = s[:at]
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case (r == '.' || r == '_' || r == '-') && b.Len() > 0:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 26 {
		out = out[:26]
	}
	return strings.TrimRight(out, "._-")
}

func (w *worker) redeemSSO(ctx context.Context, msg RedeemSSO) (*LoginResult, error) {
	t, err := w.takeTicket(ctx, msg.Code, models.SSOTicketApp)
	if err != nil {
		return nil, err
	}
	// The code is spent either way, so a stolen one is no use to guess with.
	if len(msg.Verifier) < 43 || len(msg.Verifier) > 128 || !secure.Equal(s256(msg.Verifier), t.Challenge) {
		w.Logger.Warn("sso code redeemed without its verifier", zap.String("user", t.UserID.String()), zap.String("ip", msg.IP))
		return nil, errSSOExpired
	}
	user, err := w.Repo.GetUser(ctx, t.UserID)
	if err != nil {
		return nil, err
	}
	if user.Deactivated {
		return nil, ierrors.New(ierrors.CodePermissionDenied, "account is deactivated")
	}
	return w.signIn(ctx, user, msg.IP, msg.UserAgent)
}

func (w *worker) getSSOLink(ctx context.Context, msg GetSSOLink) (*SSOLinkInfo, error) {
	if !secure.ValidToken(msg.Token) {
		return nil, errSSOExpired
	}
	t, err := w.Repo.GetSSOTicket(ctx, secure.Hash(msg.Token))
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return nil, errSSOExpired
		}
		return nil, err
	}
	if t.Kind != models.SSOTicketLink || !time.Now().Before(t.ExpiresAt) {
		return nil, errSSOExpired
	}
	return &SSOLinkInfo{Provider: w.sso.name(t.Issuer, t.Provider), Email: t.Email}, nil
}

func (w *worker) linkSSO(ctx context.Context, msg LinkSSO) (*SSOLinkInfo, error) {
	if msg.P.Bot {
		return nil, ierrors.PermissionDenied()
	}
	t, err := w.takeTicket(ctx, msg.Token, models.SSOTicketLink)
	if err != nil {
		return nil, err
	}
	id := &models.SSOIdentity{Issuer: t.Issuer, Subject: t.Subject, Provider: t.Provider, Email: t.Email}
	if owner, err := w.Repo.GetSSOIdentity(ctx, id.Issuer, id.Subject); err == nil {
		if owner != msg.P.UserID {
			return nil, errLinkedElsewhere(w.sso.name(id.Issuer, id.Provider))
		}
	} else if !ierrors.Is(err, ierrors.CodeNotFound) {
		return nil, err
	} else if err := w.link(ctx, id, msg.P.UserID); err != nil {
		return nil, err
	}
	return &SSOLinkInfo{Provider: w.sso.name(t.Issuer, t.Provider), Email: t.Email}, nil
}

// identityOwner is whose identities a call is about: the caller's, or for
// someone allowed act, those of raw.
func (w *worker) identityOwner(p *models.Principal, raw string, act authz.Action) (gocql.UUID, error) {
	if p.Bot {
		return gocql.UUID{}, ierrors.PermissionDenied()
	}
	if raw == "" {
		return p.UserID, nil
	}
	id, ok := ids.Parse(raw)
	if !ok {
		return gocql.UUID{}, ierrors.InvalidArgument("invalid user id")
	}
	if id != p.UserID && !w.Authz.Can(authz.ObjUser, act, string(p.Role)) {
		return gocql.UUID{}, ierrors.PermissionDenied()
	}
	return id, nil
}

func (s *ssoProviders) identity(id *models.SSOIdentity) *pb.SSOIdentity {
	return &pb.SSOIdentity{
		ProviderId: id.Provider, ProviderName: s.name(id.Issuer, id.Provider), Issuer: id.Issuer,
		Subject: id.Subject, Email: id.Email, LinkedAt: convert.Time(id.LinkedAt),
	}
}

func (w *worker) listSSOIdentities(ctx context.Context, msg ListSSOIdentities) ([]*pb.SSOIdentity, error) {
	userID, err := w.identityOwner(msg.P, msg.UserID, authz.ActFind)
	if err != nil {
		return nil, err
	}
	list, err := w.Repo.ListUserSSOIdentities(ctx, userID)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(list, func(a, b *models.SSOIdentity) int { return a.LinkedAt.Compare(b.LinkedAt) })
	out := make([]*pb.SSOIdentity, len(list))
	for i, id := range list {
		out[i] = w.sso.identity(id)
	}
	return out, nil
}

// unlinkSSO keeps someone's last way to sign in: an account without a
// password keeps its last identity, unless an admin takes it away.
func (w *worker) unlinkSSO(ctx context.Context, msg UnlinkSSO) (Done, error) {
	userID, err := w.identityOwner(msg.P, msg.UserID, authz.ActDeactivate)
	if err != nil {
		return Done{}, err
	}
	list, err := w.Repo.ListUserSSOIdentities(ctx, userID)
	if err != nil {
		return Done{}, err
	}
	i := slices.IndexFunc(list, func(id *models.SSOIdentity) bool { return id.Issuer == msg.Issuer && id.Subject == msg.Subject })
	if i < 0 {
		return Done{}, ierrors.NotFound("that account is not linked")
	}
	if userID == msg.P.UserID && len(list) == 1 {
		user, err := w.Repo.GetUser(ctx, userID)
		if err != nil {
			return Done{}, err
		}
		if user.PasswordHash == "" {
			return Done{}, ierrors.FailedPrecondition("this is the only way you sign in, so it stays linked")
		}
	}
	if err := w.Repo.UnlinkSSOIdentity(ctx, list[i]); err != nil {
		return Done{}, err
	}
	w.Logger.Info("sso identity unlinked", zap.String("user", userID.String()), zap.String("provider", list[i].Provider), zap.String("by", msg.P.UserID.String()))
	return Done{}, nil
}

const maxListedAccounts = 100

func (w *worker) listSSOAccounts(ctx context.Context, msg ListSSOAccounts) (*pb.ListSSOAccountsResponse, error) {
	if !w.Authz.Can(authz.ObjUser, authz.ActFind, string(msg.P.Role)) {
		return nil, ierrors.PermissionDenied()
	}
	p, err := w.sso.get(msg.Provider)
	if err != nil {
		return nil, err
	}
	var after gocql.UUID
	if msg.After != "" {
		var ok bool
		if after, ok = ids.Parse(msg.After); !ok {
			return nil, ierrors.InvalidArgument("invalid cursor")
		}
	}
	limit := msg.Limit
	if limit < 1 || limit > maxListedAccounts {
		limit = maxListedAccounts
	}
	list, err := w.Repo.ListIssuerSSOIdentities(ctx, p.cfg.Issuer, after, limit)
	if err != nil {
		return nil, err
	}
	userIDs := make([]gocql.UUID, len(list))
	for i, id := range list {
		userIDs[i] = id.UserID
	}
	users, err := w.Users.GetMany(ctx, userIDs)
	if err != nil {
		return nil, err
	}
	out := &pb.ListSSOAccountsResponse{}
	for _, id := range list {
		if u := users[id.UserID]; u != nil {
			out.Accounts = append(out.Accounts, &pb.SSOAccount{User: convert.User(u), Email: u.Email, Identity: w.sso.identity(id)})
		}
	}
	if len(list) == limit {
		out.Next = list[len(list)-1].UserID.String()
	}
	return out, nil
}
