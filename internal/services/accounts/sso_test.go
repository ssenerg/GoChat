package accounts

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/gocql/gocql"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

// ssoRows keeps the fake repo's flows, tickets and identities.
type ssoRows struct {
	mu         sync.Mutex
	flows      map[string]*models.SSOFlow
	tickets    map[string]*models.SSOTicket
	identities map[[2]string]*models.SSOIdentity
}

func newSSORows() *ssoRows {
	return &ssoRows{flows: make(map[string]*models.SSOFlow), tickets: make(map[string]*models.SSOTicket), identities: make(map[[2]string]*models.SSOIdentity)}
}

func (r *ssoRows) CreateSSOFlow(_ context.Context, f *models.SSOFlow, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := *f
	r.flows[string(f.StateHash)] = &c
	return nil
}

func (r *ssoRows) ConsumeSSOFlow(_ context.Context, hash []byte) (*models.SSOFlow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.flows[string(hash)]
	if !ok {
		return nil, ierrors.NotFound("not found")
	}
	delete(r.flows, string(hash))
	return f, nil
}

func (r *ssoRows) CreateSSOTicket(_ context.Context, t *models.SSOTicket, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := *t
	r.tickets[string(t.TokenHash)] = &c
	return nil
}

func (r *ssoRows) GetSSOTicket(_ context.Context, hash []byte) (*models.SSOTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tickets[string(hash)]
	if !ok {
		return nil, ierrors.NotFound("not found")
	}
	c := *t
	return &c, nil
}

func (r *ssoRows) ConsumeSSOTicket(_ context.Context, hash []byte) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.tickets[string(hash)]
	delete(r.tickets, string(hash))
	return ok, nil
}

func (r *ssoRows) GetSSOIdentity(_ context.Context, issuer, subject string) (gocql.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.identities[[2]string{issuer, subject}]
	if !ok {
		return gocql.UUID{}, ierrors.NotFound("not found")
	}
	return id.UserID, nil
}

func (r *ssoRows) LinkSSOIdentity(_ context.Context, id *models.SSOIdentity) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := [2]string{id.Issuer, id.Subject}
	if cur, ok := r.identities[key]; ok {
		return cur.UserID == id.UserID, nil
	}
	c := *id
	r.identities[key] = &c
	return true, nil
}

func (r *ssoRows) UnlinkSSOIdentity(_ context.Context, id *models.SSOIdentity) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := [2]string{id.Issuer, id.Subject}
	if cur, ok := r.identities[key]; ok && cur.UserID == id.UserID {
		delete(r.identities, key)
	}
	return nil
}

func (r *ssoRows) list(match func(*models.SSOIdentity) bool) []*models.SSOIdentity {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*models.SSOIdentity
	for _, id := range r.identities {
		if match(id) {
			c := *id
			out = append(out, &c)
		}
	}
	slices.SortFunc(out, func(a, b *models.SSOIdentity) int { return strings.Compare(a.UserID.String(), b.UserID.String()) })
	return out
}

func (r *ssoRows) ListUserSSOIdentities(_ context.Context, userID gocql.UUID) ([]*models.SSOIdentity, error) {
	return r.list(func(id *models.SSOIdentity) bool { return id.UserID == userID }), nil
}

func (r *ssoRows) ListIssuerSSOIdentities(_ context.Context, issuer string, after gocql.UUID, limit int) ([]*models.SSOIdentity, error) {
	list := r.list(func(id *models.SSOIdentity) bool {
		return id.Issuer == issuer && (ids.IsZero(after) || id.UserID.String() > after.String())
	})
	return list[:min(len(list), limit)], nil
}

var (
	idpKeyOnce sync.Once
	idpKeys    [2]*rsa.PrivateKey
)

// idpKey is the key the fake provider signs with, or with forged one it
// does not publish.
func idpKey(t *testing.T, forged bool) *rsa.PrivateKey {
	idpKeyOnce.Do(func() {
		for i := range idpKeys {
			k, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			idpKeys[i] = k
		}
	})
	if forged {
		return idpKeys[1]
	}
	return idpKeys[0]
}

// person is who signs in at the fake provider: claims merged over its
// defaults, and forged to sign with a key it does not publish.
type person struct {
	claims map[string]any
	forged bool
}

type grant struct {
	nonce, challenge, redirect string
	who                        person
}

// fakeIdP is an OpenID provider: discovery and keys from oidctest, and an
// authorization and a token endpoint that check PKCE.
type fakeIdP struct {
	t   *testing.T
	URL string
	mu  sync.Mutex
	// codes are the grants of the codes it gave and has yet to redeem.
	codes map[string]grant
}

const (
	testClientID     = "gochat"
	testClientSecret = "client-secret"
	testRedirect     = "https://chat.example.test/login/sso/callback"
)

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	idp := &fakeIdP{t: t, codes: make(map[string]grant)}
	keys := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: idpKey(t, false).Public(), KeyID: "k1", Algorithm: oidc.RS256}}}
	mux := http.NewServeMux()
	mux.Handle("/", keys)
	mux.HandleFunc("POST /token", idp.token)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	keys.SetIssuer(srv.URL)
	idp.URL = srv.URL
	return idp
}

// authorize plays the browser and the provider's sign-in page: it checks the
// request start sends to and returns the code it would redirect back with.
func (idp *fakeIdP) authorize(start string, who person) string {
	idp.t.Helper()
	u, err := url.Parse(start)
	if err != nil {
		idp.t.Fatal(err)
	}
	q := u.Query()
	if u.Path != "/auth" || q.Get("client_id") != testClientID || q.Get("response_type") != "code" ||
		!slices.Contains(strings.Fields(q.Get("scope")), "openid") || q.Get("code_challenge_method") != "S256" ||
		q.Get("state") == "" || q.Get("nonce") == "" || q.Get("code_challenge") == "" {
		idp.t.Fatalf("authorization request: %s", start)
	}
	code := secure.Token()
	idp.mu.Lock()
	idp.codes[code] = grant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), who: who}
	idp.mu.Unlock()
	return code
}

func (idp *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	fail := func(code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
	}
	if id != testClientID || secret != testClientSecret {
		fail("invalid_client")
		return
	}
	idp.mu.Lock()
	g, ok := idp.codes[r.PostFormValue("code")]
	delete(idp.codes, r.PostFormValue("code"))
	idp.mu.Unlock()
	if !ok || r.PostFormValue("grant_type") != "authorization_code" || r.PostFormValue("redirect_uri") != g.redirect ||
		s256(r.PostFormValue("code_verifier")) != g.challenge {
		fail("invalid_grant")
		return
	}
	now := time.Now()
	claims := map[string]any{
		"iss": idp.URL, "aud": testClientID, "sub": "subject-1", "nonce": g.nonce,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"email": "ann@example.test", "email_verified": true, "name": "Ann Example", "preferred_username": "ann",
	}
	for k, v := range g.who.claims {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		fail("server_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": secure.Token(), "token_type": "Bearer", "expires_in": 3600,
		"id_token": oidctest.SignIDToken(idpKey(idp.t, g.who.forged), "k1", oidc.RS256, string(raw)),
	})
}

func newSSOFixture(t *testing.T, mutate func(*Config)) (*fixture, *fakeIdP) {
	t.Helper()
	idp := newFakeIdP(t)
	f := newFixture(t, func(c *Config) {
		c.OpenSignup = true
		c.SSO = SSOConfig{FlowTTL: 10 * time.Minute, Timeout: 5 * time.Second, Providers: []SSOProvider{{
			ID: "idp", Name: "Test ID", Issuer: idp.URL, ClientID: testClientID, ClientSecret: testClientSecret,
		}}}
		if mutate != nil {
			mutate(c)
		}
	})
	if err := f.deps.Cfg.SSO.Validate(); err != nil {
		t.Fatal(err)
	}
	f.repo.bootstrapped = true
	return f, idp
}

// sso signs who in at the provider, starting with begin and finishing with
// finish, which name neither the provider nor the flow.
func (f *fixture) sso(idp *fakeIdP, who person, begin BeginSSO, finish FinishSSO) (*SSOResult, error) {
	f.t.Helper()
	begin.Provider, begin.RedirectURI = "idp", testRedirect
	start, err := ask[*SSOStart](f, begin)
	if err != nil {
		f.t.Fatalf("begin: %v", err)
	}
	finish.State, finish.Code = start.State, idp.authorize(start.URL, who)
	return ask[*SSOResult](f, finish)
}

// signedIn checks res is a session and returns its principal.
func (f *fixture) signedIn(res *SSOResult, err error) *models.Principal {
	f.t.Helper()
	if err != nil {
		f.t.Fatalf("sign-in: %v", err)
	}
	if res.Login == nil || res.Login.Session == nil {
		f.t.Fatalf("no session: %+v", res)
	}
	p, err := ask[*models.Principal](f, Authenticate{Token: res.Login.Session.Token})
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

func TestSSOSignsUpAndBackIn(t *testing.T) {
	f, idp := newSSOFixture(t, nil)
	p := f.signedIn(f.sso(idp, person{}, BeginSSO{}, FinishSSO{IP: "10.0.0.1"}))
	u, _ := f.repo.GetUser(context.Background(), p.UserID)
	if u.Username != "ann" || u.Email != "ann@example.test" || u.DisplayName != "Ann Example" || u.PasswordHash != "" || u.Role != models.SystemRoleUser {
		t.Fatalf("new account: %+v", u)
	}
	again := f.signedIn(f.sso(idp, person{claims: map[string]any{"email": "ann@elsewhere.test", "preferred_username": "other"}}, BeginSSO{}, FinishSSO{}))
	if again.UserID != p.UserID {
		t.Fatal("the same subject signed in to another account")
	}
	// No password, so no password sign-in, and no internal error either.
	if _, err := ask[*LoginResult](f, Login{Login: "ann", Password: ""}); !ierrors.Is(err, ierrors.CodeUnauthenticated) {
		t.Fatalf("password sign-in of an SSO account: %v", err)
	}
	if _, err := ask[*TOTPSetup](f, BeginTOTP{P: p, Password: "x"}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("TOTP setup without a password: %v", err)
	}

	// Another subject with a taken username gets the next free one.
	p2 := f.signedIn(f.sso(idp, person{claims: map[string]any{"sub": "subject-2", "email": "ann@second.test"}}, BeginSSO{}, FinishSSO{}))
	u2, _ := f.repo.GetUser(context.Background(), p2.UserID)
	if p2.UserID == p.UserID || u2.Username != "ann2" {
		t.Fatalf("second account: %+v", u2)
	}
	list, err := ask[[]*pb.SSOIdentity](f, ListSSOIdentities{P: p})
	if err != nil || len(list) != 1 || list[0].Subject != "subject-1" || list[0].ProviderName != "Test ID" || list[0].Issuer != idp.URL {
		t.Fatalf("identities: %v %+v", err, list)
	}
}

func TestSSORefusesBadTokens(t *testing.T) {
	f, idp := newSSOFixture(t, nil)
	past := time.Now().Add(-time.Minute).Unix()
	for name, who := range map[string]person{
		"wrong audience":         {claims: map[string]any{"aud": "someone-else"}},
		"wrong nonce":            {claims: map[string]any{"nonce": "another"}},
		"no nonce":               {claims: map[string]any{"nonce": nil}},
		"wrong issuer":           {claims: map[string]any{"iss": "https://evil.example.test"}},
		"expired":                {claims: map[string]any{"exp": past, "iat": past - 3600}},
		"forged signature":       {forged: true},
		"several audiences":      {claims: map[string]any{"aud": []string{testClientID, "other"}}},
		"another party's":        {claims: map[string]any{"aud": []string{testClientID, "other"}, "azp": "other"}},
		"no subject":             {claims: map[string]any{"sub": nil}},
		"a subject over 255 B":   {claims: map[string]any{"sub": strings.Repeat("s", 256)}},
		"email_verified unknown": {claims: map[string]any{"email_verified": "maybe"}},
	} {
		res, err := f.sso(idp, who, BeginSSO{}, FinishSSO{})
		if !ierrors.Is(err, ierrors.CodeUnauthenticated) {
			t.Errorf("%s: %v %+v", name, err, res)
		}
	}
	if n := len(f.repo.users); n != 0 {
		t.Fatalf("bad tokens made %d accounts", n)
	}
	// The one party of several audiences may sign in.
	f.signedIn(f.sso(idp, person{claims: map[string]any{"aud": []string{testClientID, "other"}, "azp": testClientID}}, BeginSSO{}, FinishSSO{}))
}

func TestSSOFlowIsSingleUse(t *testing.T) {
	f, idp := newSSOFixture(t, nil)
	start, err := ask[*SSOStart](f, BeginSSO{Provider: "idp", RedirectURI: testRedirect})
	if err != nil {
		t.Fatal(err)
	}
	code := idp.authorize(start.URL, person{})
	f.signedIn(ask[*SSOResult](f, FinishSSO{State: start.State, Code: code}))
	if _, err := ask[*SSOResult](f, FinishSSO{State: start.State, Code: code}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("a state was used twice: %v", err)
	}
	if _, err := ask[*SSOResult](f, FinishSSO{State: secure.Token(), Code: code}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("an unknown state finished: %v", err)
	}
	// A code of another flow fails PKCE at the provider, and spends the flow.
	other, _ := ask[*SSOStart](f, BeginSSO{Provider: "idp", RedirectURI: testRedirect})
	stolen := idp.authorize(start.URL, person{})
	if _, err := ask[*SSOResult](f, FinishSSO{State: other.State, Code: stolen}); !ierrors.Is(err, ierrors.CodeUnauthenticated) {
		t.Fatalf("a code finished another flow: %v", err)
	}
	if _, err := ask[*SSOStart](f, BeginSSO{Provider: "nope", RedirectURI: testRedirect}); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("an unknown provider began: %v", err)
	}
	late, _ := ask[*SSOStart](f, BeginSSO{Provider: "idp", RedirectURI: testRedirect})
	f.repo.ssoRows.mu.Lock()
	f.repo.flows[string(secure.Hash(late.State))].ExpiresAt = time.Now().Add(-time.Second)
	f.repo.ssoRows.mu.Unlock()
	if _, err := ask[*SSOResult](f, FinishSSO{State: late.State, Code: idp.authorize(late.URL, person{})}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("an expired flow finished: %v", err)
	}
}

func TestSSONeverLinksOnEmailAlone(t *testing.T) {
	f, idp := newSSOFixture(t, nil)
	bob := f.user("bob", models.SystemRoleUser)
	who := person{claims: map[string]any{"email": "BOB@example.test"}}
	res, err := f.sso(idp, who, BeginSSO{}, FinishSSO{})
	if err != nil || res.Login != nil || res.LinkToken == "" {
		t.Fatalf("an email matched an account: %v %+v", err, res)
	}
	if _, err := f.repo.GetSSOIdentity(context.Background(), idp.URL, "subject-1"); err == nil {
		t.Fatal("the identity was linked before the account's owner signed in")
	}
	info, err := ask[*SSOLinkInfo](f, GetSSOLink{Token: res.LinkToken})
	if err != nil || info.Provider != "Test ID" || info.Email != "bob@example.test" {
		t.Fatalf("link info: %v %+v", err, info)
	}
	// The owner proves the account is theirs with its password first.
	bp, _ := f.login(bob, "10.0.0.2")
	if _, err := ask[*SSOLinkInfo](f, LinkSSO{P: bp, Token: res.LinkToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := ask[*SSOLinkInfo](f, LinkSSO{P: bp, Token: res.LinkToken}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("a link token was used twice: %v", err)
	}
	if p := f.signedIn(f.sso(idp, who, BeginSSO{}, FinishSSO{})); p.UserID != bob.ID {
		t.Fatal("the linked identity signed in to someone else")
	}
	if n := len(f.repo.users); n != 1 {
		t.Fatalf("%d accounts", n)
	}
}

func TestSSOLinksForTheSignedIn(t *testing.T) {
	f, idp := newSSOFixture(t, nil)
	carol := f.user("carol", models.SystemRoleUser)
	dave := f.user("dave", models.SystemRoleUser)
	cp, _ := f.login(carol, "10.0.0.3")
	dp, _ := f.login(dave, "10.0.0.4")
	who := person{claims: map[string]any{"sub": "carol-at-idp", "email": "carol@corp.test"}}

	if _, err := f.sso(idp, who, BeginSSO{P: cp}, FinishSSO{P: dp}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("a link carol started finished for dave: %v", err)
	}
	if _, err := f.sso(idp, who, BeginSSO{P: cp}, FinishSSO{}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("a link finished signed out: %v", err)
	}
	res, err := f.sso(idp, who, BeginSSO{P: cp}, FinishSSO{P: cp})
	if err != nil || !res.Linked || res.Login != nil {
		t.Fatalf("link: %v %+v", err, res)
	}
	if p := f.signedIn(f.sso(idp, who, BeginSSO{}, FinishSSO{})); p.UserID != carol.ID {
		t.Fatal("the link signed in someone else")
	}
	if _, err := f.sso(idp, who, BeginSSO{P: dp}, FinishSSO{P: dp}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("dave took carol's identity: %v", err)
	}
	// An app cannot link.
	if _, err := ask[*SSOStart](f, BeginSSO{Provider: "idp", RedirectURI: testRedirect, P: cp, AppChallenge: s256(strings.Repeat("v", 43))}); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
		t.Fatalf("an app began a link: %v", err)
	}
}

func TestSSOSignUpFollowsTheSignUpRules(t *testing.T) {
	f, idp := newSSOFixture(t, func(c *Config) { c.OpenSignup = false })
	if _, err := f.sso(idp, person{}, BeginSSO{}, FinishSSO{}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("an account was made while sign-up is closed: %v", err)
	}
	admin := f.user("ada", models.SystemRoleAdmin)
	token := secure.Token()
	teamID := ids.New()
	f.repo.invites[string(secure.Hash(token))] = &models.Invite{TokenHash: secure.Hash(token), TeamID: teamID, CreatedBy: admin.ID, ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := f.sso(idp, person{claims: map[string]any{"email_verified": false}}, BeginSSO{}, FinishSSO{InviteToken: token}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("an unverified email made an account: %v", err)
	}
	res, err := f.sso(idp, person{}, BeginSSO{}, FinishSSO{InviteToken: token})
	p := f.signedIn(res, err)
	if res.InviteTeamID != teamID || res.User == nil || res.User.ID != p.UserID {
		t.Fatalf("the invite's team: %+v", res)
	}
	if _, err := f.sso(idp, person{claims: map[string]any{"sub": "other", "email": "o@example.test"}}, BeginSSO{}, FinishSSO{InviteToken: token}); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("a used invite made a second account: %v", err)
	}

	f, idp = newSSOFixture(t, nil)
	f.repo.bootstrapped = false
	if _, err := f.sso(idp, person{}, BeginSSO{}, FinishSSO{}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("SSO made the first account: %v", err)
	}
}

func TestSSOAllowedDomains(t *testing.T) {
	f, idp := newSSOFixture(t, func(c *Config) { c.SSO.Providers[0].AllowedDomains = []string{"Corp.Test"} })
	for name, who := range map[string]person{
		"another domain":     {claims: map[string]any{"email": "ann@example.test"}},
		"a subdomain":        {claims: map[string]any{"email": "ann@eu.corp.test"}},
		"an unverified mail": {claims: map[string]any{"email": "ann@corp.test", "email_verified": false}},
		"no mail":            {claims: map[string]any{"email": nil}},
	} {
		if _, err := f.sso(idp, who, BeginSSO{}, FinishSSO{}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
	f.signedIn(f.sso(idp, person{claims: map[string]any{"email": "ann@CORP.test"}}, BeginSSO{}, FinishSSO{}))
}

func TestSSOKeepsTheSecondFactorAndDeactivation(t *testing.T) {
	f, idp := newSSOFixture(t, nil)
	eve := f.user("eve", models.SystemRoleUser)
	ep, _ := f.login(eve, "10.0.0.5")
	who := person{claims: map[string]any{"sub": "eve", "email": "eve@corp.test"}}
	if res, err := f.sso(idp, who, BeginSSO{P: ep}, FinishSSO{P: ep}); err != nil || !res.Linked {
		t.Fatalf("link: %v", err)
	}
	f.repo.mu.Lock()
	f.repo.users[eve.ID].TOTPEnabled = true
	f.repo.mu.Unlock()
	res, err := f.sso(idp, who, BeginSSO{}, FinishSSO{})
	if err != nil || res.Login == nil || res.Login.Session != nil || res.Login.MFAToken == "" {
		t.Fatalf("TOTP was skipped: %v %+v", err, res)
	}

	f.repo.mu.Lock()
	f.repo.users[eve.ID].Deactivated = true
	f.repo.mu.Unlock()
	if _, err := f.sso(idp, who, BeginSSO{}, FinishSSO{}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("a deactivated account signed in: %v", err)
	}
}

func TestSSOHandsOffToApps(t *testing.T) {
	f, idp := newSSOFixture(t, nil)
	verifier := secure.Token() + secure.Token()
	appSignIn := func() string {
		res, err := f.sso(idp, person{}, BeginSSO{AppChallenge: s256(verifier)}, FinishSSO{})
		if err != nil || res.Login != nil || res.AppCode == "" {
			t.Fatalf("app sign-in: %v %+v", err, res)
		}
		return res.AppCode
	}
	if _, err := ask[*SSOStart](f, BeginSSO{Provider: "idp", RedirectURI: testRedirect, AppChallenge: "short"}); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
		t.Fatalf("a bad challenge began: %v", err)
	}
	code := appSignIn()
	if _, err := ask[*LoginResult](f, RedeemSSO{Code: code, Verifier: secure.Token() + secure.Token()}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("another verifier redeemed the code: %v", err)
	}
	if _, err := ask[*LoginResult](f, RedeemSSO{Code: code, Verifier: verifier}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("a code survived a wrong verifier: %v", err)
	}
	res, err := ask[*LoginResult](f, RedeemSSO{Code: appSignIn(), Verifier: verifier})
	if err != nil || res.Session == nil {
		t.Fatalf("redeem: %v %+v", err, res)
	}

	// An account with the email cannot be linked from an app.
	f.user("bob", models.SystemRoleUser)
	if _, err := f.sso(idp, person{claims: map[string]any{"sub": "b", "email": "bob@example.test"}}, BeginSSO{AppChallenge: s256(verifier)}, FinishSSO{}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("an app got a link: %v", err)
	}
}

func TestSSOUnlinkAndAdminListing(t *testing.T) {
	f, idp := newSSOFixture(t, nil)
	admin := f.user("ada", models.SystemRoleAdmin)
	ap, _ := f.login(admin, "10.0.0.1")
	ann := f.signedIn(f.sso(idp, person{}, BeginSSO{}, FinishSSO{}))
	if _, err := ask[Done](f, UnlinkSSO{P: ann, Issuer: idp.URL, Subject: "subject-1"}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("a password-less account lost its last identity: %v", err)
	}
	bob := f.user("bob", models.SystemRoleUser)
	bp, _ := f.login(bob, "10.0.0.2")
	if _, err := f.sso(idp, person{claims: map[string]any{"sub": "bob"}}, BeginSSO{P: bp}, FinishSSO{P: bp}); err != nil {
		t.Fatal(err)
	}

	page, err := ask[*pb.ListSSOAccountsResponse](f, ListSSOAccounts{P: ap, Provider: "idp", Limit: 1})
	if err != nil || len(page.Accounts) != 1 || page.Next == "" {
		t.Fatalf("first page: %v %+v", err, page)
	}
	rest, err := ask[*pb.ListSSOAccountsResponse](f, ListSSOAccounts{P: ap, Provider: "idp", After: page.Next})
	if err != nil || len(rest.Accounts) != 1 || rest.Next != "" || rest.Accounts[0].User.Id == page.Accounts[0].User.Id {
		t.Fatalf("second page: %v %+v", err, rest)
	}
	if _, err := ask[*pb.ListSSOAccountsResponse](f, ListSSOAccounts{P: bp, Provider: "idp"}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("a user listed linked accounts: %v", err)
	}
	if _, err := ask[[]*pb.SSOIdentity](f, ListSSOIdentities{P: bp, UserID: ann.UserID.String()}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("bob listed ann's identities: %v", err)
	}
	if list, err := ask[[]*pb.SSOIdentity](f, ListSSOIdentities{P: ap, UserID: ann.UserID.String()}); err != nil || len(list) != 1 {
		t.Fatalf("the admin listed ann's identities: %v %d", err, len(list))
	}

	// Bob has a password, so he may unlink; an admin may unlink anyone's.
	if _, err := ask[Done](f, UnlinkSSO{P: bp, Issuer: idp.URL, Subject: "bob"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ask[Done](f, UnlinkSSO{P: bp, UserID: ann.UserID.String(), Issuer: idp.URL, Subject: "subject-1"}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("bob unlinked ann: %v", err)
	}
	if _, err := ask[Done](f, UnlinkSSO{P: ap, UserID: ann.UserID.String(), Issuer: idp.URL, Subject: "subject-1"}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.repo.identities); n != 0 {
		t.Fatalf("%d identities left", n)
	}
}

func TestSSOConfig(t *testing.T) {
	good := func() *SSOConfig {
		return &SSOConfig{FlowTTL: 10 * time.Minute, Timeout: time.Second, Providers: []SSOProvider{{
			ID: "google", Name: "Google", Issuer: "https://accounts.google.com", ClientID: "id", ClientSecret: "s",
		}}}
	}
	if err := good().Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (&SSOConfig{}).Validate(); err != nil {
		t.Fatalf("no providers: %v", err)
	}
	for name, mutate := range map[string]func(*SSOConfig){
		"plain http":     func(c *SSOConfig) { c.Providers[0].Issuer = "http://accounts.example.test" },
		"a bad id":       func(c *SSOConfig) { c.Providers[0].ID = "Google!" },
		"a repeated id":  func(c *SSOConfig) { c.Providers = append(c.Providers, c.Providers[0]) },
		"no client id":   func(c *SSOConfig) { c.Providers[0].ClientID = "" },
		"two secrets":    func(c *SSOConfig) { c.Providers[0].ClientSecretFile = "/run/secret" },
		"no secret":      func(c *SSOConfig) { c.Providers[0].ClientSecret = "" },
		"a bad domain":   func(c *SSOConfig) { c.Providers[0].AllowedDomains = []string{"@corp"} },
		"no name":        func(c *SSOConfig) { c.Providers[0].Name = "" },
		"a long flow":    func(c *SSOConfig) { c.FlowTTL = 2 * time.Hour },
		"no timeout":     func(c *SSOConfig) { c.Timeout = 0 },
		"issuer's query": func(c *SSOConfig) { c.Providers[0].Issuer += "?x=1" },
	} {
		c := good()
		mutate(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestUsernameBase(t *testing.T) {
	for in, want := range map[string]string{
		"Ann.Example@corp.test": "ann.example",
		"__x":                   "x",
		"José":                  "jos",
		"a-":                    "a",
		"":                      "",
		strings.Repeat("z", 40): strings.Repeat("z", 26),
	} {
		if got := usernameBase(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
