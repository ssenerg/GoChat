package accounts

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	potp "github.com/pquerna/otp/totp"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/authz/authztest"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/keylock"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/password"
	"github.com/ssenerg/gochat/internal/core/secure"
	"github.com/ssenerg/gochat/internal/core/totp"
	"github.com/ssenerg/gochat/internal/core/window"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/hub"
	"github.com/ssenerg/gochat/internal/services/users"
)

var errUnused = errors.New("not used by these tests")

// fakeRepo keeps users and sessions in memory.
type fakeRepo struct {
	mu           sync.Mutex
	users        map[gocql.UUID]*models.User
	sessions     map[string]*models.Session
	bootstrapped bool
	touched      int
	bots         map[gocql.UUID]*models.Bot
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{users: make(map[gocql.UUID]*models.User), sessions: make(map[string]*models.Session)}
}

func (f *fakeRepo) CreateUser(_ context.Context, u *models.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.users {
		if o.Username == u.Username || o.Email == u.Email {
			return ierrors.AlreadyExists("taken")
		}
	}
	c := *u
	f.users[u.ID] = &c
	return nil
}

func (f *fakeRepo) GetUser(_ context.Context, id gocql.UUID) (*models.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return nil, ierrors.NotFound("not found")
	}
	c := *u
	return &c, nil
}

func (f *fakeRepo) GetUsers(ctx context.Context, idList []gocql.UUID) ([]*models.User, error) {
	var out []*models.User
	for _, id := range idList {
		if u, err := f.GetUser(ctx, id); err == nil {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *fakeRepo) userBy(match func(*models.User) bool) (gocql.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, u := range f.users {
		if match(u) {
			return id, nil
		}
	}
	return gocql.UUID{}, ierrors.NotFound("not found")
}

func (f *fakeRepo) GetUserIDByUsername(_ context.Context, name string) (gocql.UUID, error) {
	return f.userBy(func(u *models.User) bool { return u.Username == name })
}

func (f *fakeRepo) GetUserIDByEmail(_ context.Context, email string) (gocql.UUID, error) {
	return f.userBy(func(u *models.User) bool { return u.Email == email })
}

func (f *fakeRepo) SetDeactivated(_ context.Context, id gocql.UUID, deactivated bool, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[id].Deactivated = deactivated
	return nil
}

func (f *fakeRepo) ClaimBootstrap(context.Context, gocql.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	claimed := !f.bootstrapped
	f.bootstrapped = true
	return claimed, nil
}

func (f *fakeRepo) ReleaseBootstrap(context.Context, gocql.UUID) error { return nil }

func (f *fakeRepo) Bootstrapped(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bootstrapped, nil
}

func (f *fakeRepo) CreateSession(_ context.Context, s *models.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *s
	f.sessions[string(s.TokenHash)] = &c
	return nil
}

func (f *fakeRepo) GetSession(_ context.Context, hash []byte) (*models.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[string(hash)]
	if !ok {
		return nil, ierrors.NotFound("not found")
	}
	c := *s
	return &c, nil
}

func (f *fakeRepo) TouchSession(_ context.Context, s *models.Session, at time.Time, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched++
	if cur, ok := f.sessions[string(s.TokenHash)]; ok {
		cur.LastSeen, cur.LastIP = at, ip
	}
	return nil
}

func (f *fakeRepo) DeleteSession(_ context.Context, hash []byte, _ gocql.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, string(hash))
	return nil
}

func (f *fakeRepo) ListUserSessions(_ context.Context, userID gocql.UUID) ([]*models.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*models.Session
	for _, s := range f.sessions {
		if s.UserID == userID {
			c := *s
			out = append(out, &c)
		}
	}
	return out, nil
}

func (f *fakeRepo) DeleteUserSessions(ctx context.Context, userID gocql.UUID, keep []byte) ([][]byte, error) {
	list, _ := f.ListUserSessions(ctx, userID)
	var out [][]byte
	for _, s := range list {
		if string(s.TokenHash) != string(keep) {
			_ = f.DeleteSession(ctx, s.TokenHash, userID)
			out = append(out, s.TokenHash)
		}
	}
	return out, nil
}

func (f *fakeRepo) UpdatePassword(context.Context, gocql.UUID, string, time.Time) error { return nil }
func (f *fakeRepo) SetTOTPPending(context.Context, gocql.UUID, []byte, time.Time) error {
	return errUnused
}

func (f *fakeRepo) EnableTOTP(context.Context, gocql.UUID, []byte, int64, time.Time) (bool, error) {
	return false, errUnused
}
func (f *fakeRepo) DisableTOTP(context.Context, gocql.UUID, time.Time) error { return errUnused }
func (f *fakeRepo) AdvanceTOTPStep(context.Context, gocql.UUID, int64) (bool, error) {
	return false, errUnused
}
func (f *fakeRepo) ReplaceRecoveryCodes(context.Context, gocql.UUID, [][]byte) error {
	return errUnused
}
func (f *fakeRepo) ConsumeRecoveryCode(context.Context, gocql.UUID, []byte) (bool, error) {
	return false, errUnused
}

func (f *fakeRepo) CreateMFAChallenge(context.Context, *models.MFAChallenge, time.Time) error {
	return errUnused
}

func (f *fakeRepo) GetMFAChallenge(context.Context, []byte) (*models.MFAChallenge, error) {
	return nil, errUnused
}

func (f *fakeRepo) CountMFAAttempt(context.Context, *models.MFAChallenge, time.Time) (bool, error) {
	return false, errUnused
}
func (f *fakeRepo) ConsumeMFAChallenge(context.Context, []byte) (bool, error) {
	return false, errUnused
}
func (f *fakeRepo) GetTeam(context.Context, gocql.UUID) (*models.Team, error) { return nil, errUnused }
func (f *fakeRepo) GetTeamMember(context.Context, gocql.UUID, gocql.UUID) (*models.TeamMember, error) {
	return nil, errUnused
}
func (f *fakeRepo) GetInvite(context.Context, []byte) (*models.Invite, error) { return nil, errUnused }
func (f *fakeRepo) ConsumeInvite(context.Context, *models.Invite, gocql.UUID, time.Time) (bool, error) {
	return false, errUnused
}

func (f *fakeRepo) ReleaseInvite(context.Context, *models.Invite, gocql.UUID, time.Time) error {
	return errUnused
}

// recorder keeps what the accounts send the hub.
type recorder struct{ got chan hub.RevokeSessions }

func (r *recorder) Receive(c *actor.Context) {
	if msg, ok := c.Message().(hub.RevokeSessions); ok {
		r.got <- msg
	}
}

var testPassword = password.Params{Time: 1, Memory: 8 * 1024, Threads: 1}

type fixture struct {
	t      *testing.T
	repo   *fakeRepo
	engine *actor.Engine
	pid    *actor.PID
	hub    chan hub.RevokeSessions
	deps   *Deps
}

func newFixture(t *testing.T, mutate func(*Config)) *fixture {
	t.Helper()
	cfg := &Config{
		Workers: 2, OpTimeout: 5 * time.Second, Password: testPassword, MinPasswordLength: 10,
		SessionTTL: time.Hour, SessionCacheTTL: time.Minute, SessionCacheSize: 1000, LastSeenInterval: 5 * time.Minute,
		MFATTL: time.Minute, MFAMaxAttempts: 5, TOTPIssuer: "GoChat",
		SecretKey: base64.StdEncoding.EncodeToString(make([]byte, 32)), SetupToken: "setup-token",
		LoginFailures: 100, LoginWindow: time.Minute,
	}
	if mutate != nil {
		mutate(cfg)
	}
	engine, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	repo := newFakeRepo()
	got := make(chan hub.RevokeSessions, 16)
	hubPID := engine.Spawn(func() actor.Receiver { return &recorder{got: got} }, "hub")
	deps, err := NewDeps(zap.NewNop(), cfg, repo, users.NewCache(repo, 100, time.Minute), authztest.New(t), engine, hubPID)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, repo: repo, engine: engine, hub: got, deps: deps}
	f.pid = engine.Spawn(Pool(deps), "accounts")
	t.Cleanup(func() { <-engine.Poison(f.pid).Done() })
	return f
}

func ask[T any](f *fixture, msg any) (T, error) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return actors.Ask[T](ctx, f.engine, f.pid, msg)
}

func (f *fixture) user(name string, role models.SystemRole) *models.User {
	f.t.Helper()
	u := &models.User{ID: ids.New(), Username: name, Email: name + "@example.test", Role: role, PasswordHash: password.Hash(name+"-password", testPassword)}
	if err := f.repo.CreateUser(context.Background(), u); err != nil {
		f.t.Fatal(err)
	}
	return u
}

// login signs u in and returns the session's principal.
func (f *fixture) login(u *models.User, ip string) (*models.Principal, string) {
	f.t.Helper()
	res, err := ask[*LoginResult](f, Login{Login: u.Username, Password: u.Username + "-password", IP: ip, UserAgent: "test"})
	if err != nil {
		f.t.Fatal(err)
	}
	p, err := ask[*models.Principal](f, Authenticate{Token: res.Session.Token, IP: ip})
	if err != nil {
		f.t.Fatal(err)
	}
	return p, res.Session.Token
}

func (f *fixture) revocation() hub.RevokeSessions {
	f.t.Helper()
	select {
	case msg := <-f.hub:
		return msg
	case <-time.After(5 * time.Second):
		f.t.Fatal("the hub was not told of a revocation")
		return hub.RevokeSessions{}
	}
}

func TestDeactivationSignsOutEverywhere(t *testing.T) {
	f := newFixture(t, nil)
	admin := f.user("ada", models.SystemRoleAdmin)
	bob := f.user("bob", models.SystemRoleUser)
	ap, _ := f.login(admin, "10.0.0.1")
	bp1, token := f.login(bob, "10.0.0.2")
	bp2, _ := f.login(bob, "10.0.0.3")

	if _, err := ask[*models.User](f, SetDeactivated{P: bp1, UserID: admin.ID.String(), Deactivated: true}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("a user deactivated an admin: %v", err)
	}
	if _, err := ask[*models.User](f, SetDeactivated{P: ap, UserID: admin.ID.String(), Deactivated: true}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("an admin deactivated themself: %v", err)
	}
	u, err := ask[*models.User](f, SetDeactivated{P: ap, UserID: bob.ID.String(), Deactivated: true})
	if err != nil || !u.Deactivated {
		t.Fatalf("deactivate: %v %+v", err, u)
	}
	rev := f.revocation()
	slices.Sort(rev.SessionIDs)
	want := []string{bp1.SessionID, bp2.SessionID}
	slices.Sort(want)
	if rev.UserID != bob.ID.String() || !rev.All || !slices.Equal(rev.SessionIDs, want) {
		t.Fatalf("revocation sent to the hub: %+v, want %v", rev, want)
	}
	if list, _ := f.repo.ListUserSessions(context.Background(), bob.ID); len(list) != 0 {
		t.Fatalf("%d sessions survived", len(list))
	}
	if _, ok := f.deps.Cached(token); ok {
		t.Fatal("a revoked session is still cached")
	}
	if _, err := ask[*models.Principal](f, Authenticate{Token: token}); !ierrors.Is(err, ierrors.CodeUnauthenticated) {
		t.Fatalf("a revoked session authenticated: %v", err)
	}
	_, err = ask[*LoginResult](f, Login{Login: "bob", Password: "bob-password"})
	if !ierrors.Is(err, ierrors.CodePermissionDenied) || ierrors.As(err).Message != "account is deactivated" {
		t.Fatalf("a deactivated user signed in: %v", err)
	}

	if _, err := ask[*models.User](f, SetDeactivated{P: ap, UserID: bob.ID.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := ask[*LoginResult](f, Login{Login: "bob", Password: "bob-password"}); err != nil {
		t.Fatalf("a reactivated user could not sign in: %v", err)
	}
}

func TestSessionsListAndRevoke(t *testing.T) {
	f := newFixture(t, nil)
	admin := f.user("ada", models.SystemRoleAdmin)
	bob := f.user("bob", models.SystemRoleUser)
	carol := f.user("carol", models.SystemRoleUser)
	ap, _ := f.login(admin, "10.0.0.1")
	bp, _ := f.login(bob, "10.0.0.2")
	other, _ := f.login(bob, "10.0.0.3")
	third, _ := f.login(bob, "10.0.0.4")
	cp, _ := f.login(carol, "10.0.0.5")

	list, err := ask[[]*pb.SessionInfo](f, ListSessions{P: bp})
	if err != nil || len(list) != 3 {
		t.Fatalf("list: %v %d", err, len(list))
	}
	for _, s := range list {
		if s.Current != (s.Id == bp.SessionID) || s.LastIp == "" {
			t.Fatalf("listed %+v", s)
		}
	}
	if _, err := ask[[]*pb.SessionInfo](f, ListSessions{P: cp, UserID: bob.ID.String()}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("carol listed bob's sessions: %v", err)
	}
	if _, err := ask[Done](f, RevokeSession{P: cp, SessionID: other.SessionID}); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("carol revoked bob's session: %v", err)
	}
	if _, err := ask[Done](f, RevokeSession{P: bp, SessionID: other.SessionID}); err != nil {
		t.Fatal(err)
	}
	if rev := f.revocation(); rev.All || !slices.Equal(rev.SessionIDs, []string{other.SessionID}) {
		t.Fatalf("revocation: %+v", rev)
	}
	n, err := ask[Revoked](f, RevokeSessions{P: bp})
	if err != nil || n != 1 {
		t.Fatalf("revoke others: %v %d", err, n)
	}
	if rev := f.revocation(); rev.All || !slices.Equal(rev.SessionIDs, []string{third.SessionID}) {
		t.Fatalf("revocation of the others: %+v", rev)
	}
	if _, err := ask[*models.Principal](f, Authenticate{Token: "x"}); err == nil {
		t.Fatal("a bad token authenticated")
	}
	if _, err := ask[Revoked](f, RevokeSessions{P: cp, UserID: bob.ID.String()}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("carol revoked bob's sessions: %v", err)
	}
	if n, err := ask[Revoked](f, RevokeSessions{P: ap, UserID: bob.ID.String()}); err != nil || n != 1 {
		t.Fatalf("admin revoke: %v %d", err, n)
	}
	if rev := f.revocation(); !rev.All || !slices.Equal(rev.SessionIDs, []string{bp.SessionID}) {
		t.Fatalf("admin revocation: %+v", rev)
	}
}

func TestSessionUseIsRecordedSparingly(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.SessionCacheTTL = time.Nanosecond })
	bob := f.user("bob", models.SystemRoleUser)
	_, token := f.login(bob, "10.0.0.2")
	for range 3 {
		if _, err := ask[*models.Principal](f, Authenticate{Token: token, IP: "10.0.0.2"}); err != nil {
			t.Fatal(err)
		}
	}
	if f.repo.touched != 1 {
		t.Fatalf("recorded %d uses from one address", f.repo.touched)
	}
	if _, err := ask[*models.Principal](f, Authenticate{Token: token, IP: "10.0.0.9"}); err != nil {
		t.Fatal(err)
	}
	s, _ := f.repo.GetSession(context.Background(), secure.Hash(token))
	if f.repo.touched != 2 || s.LastIP != "10.0.0.9" {
		t.Fatalf("a move to another address was not recorded: %d %q", f.repo.touched, s.LastIP)
	}
}

func TestFindUser(t *testing.T) {
	f := newFixture(t, nil)
	admin := f.user("ada", models.SystemRoleAdmin)
	bob := f.user("bob", models.SystemRoleUser)
	ap, _ := f.login(admin, "10.0.0.1")
	bp, _ := f.login(bob, "10.0.0.2")
	for _, msg := range []FindUser{{P: ap, Login: " BOB "}, {P: ap, Login: "bob@example.test"}, {P: ap, UserID: bob.ID.String()}} {
		if u, err := ask[*models.User](f, msg); err != nil || u.ID != bob.ID {
			t.Fatalf("find %+v: %v", msg, err)
		}
	}
	if _, err := ask[*models.User](f, FindUser{P: ap, Login: "nobody"}); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("found nobody: %v", err)
	}
	if _, err := ask[*models.User](f, FindUser{P: bp, Login: "ada"}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("a user looked an account up: %v", err)
	}
}

func signUp(f *fixture, name, token string) (*SignUpResult, error) {
	return ask[*SignUpResult](f, SignUp{Username: name, Email: name + "@example.test", Password: "a long password", SetupToken: token})
}

func TestBootstrapNeedsASetupToken(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.SetupToken = "" })
	info, err := ask[*SignupInfoResult](f, SignupInfo{})
	if err != nil || !info.SetupClosed {
		t.Fatalf("signup info: %v %+v", err, info)
	}
	if _, err := signUp(f, "eve", ""); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("the first account was created without a setup token: %v", err)
	}

	f = newFixture(t, nil)
	if _, err := signUp(f, "eve", "wrong"); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("a wrong setup token was accepted: %v", err)
	}
	res, err := signUp(f, "ada", "setup-token")
	if err != nil || res.User.Role != models.SystemRoleAdmin {
		t.Fatalf("bootstrap with the token: %v", err)
	}

	f = newFixture(t, func(c *Config) { c.SetupToken, c.AllowOpenBootstrap = "", true })
	if info, _ := ask[*SignupInfoResult](f, SignupInfo{}); info.SetupClosed {
		t.Fatal("an open bootstrap reads as closed")
	}
	if res, err := signUp(f, "ada", ""); err != nil || res.User.Role != models.SystemRoleAdmin {
		t.Fatalf("open bootstrap: %v", err)
	}
	if _, err := signUp(f, "eve", ""); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("a second account was created without an invite: %v", err)
	}
}

func TestConfigNeedsALastSeenInterval(t *testing.T) {
	f := newFixture(t, nil)
	cfg := *f.deps.Cfg
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.LastSeenInterval = time.Second
	if err := cfg.Validate(); err == nil {
		t.Fatal("a last_seen_interval of 1s was accepted")
	}
}

func TestIdleSessionsEnd(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.SessionCacheTTL = time.Nanosecond; c.SessionIdleTimeout = time.Hour })
	bob := f.user("bob", models.SystemRoleUser)
	_, token := f.login(bob, "10.0.0.2")
	if _, err := ask[*models.Principal](f, Authenticate{Token: token, IP: "10.0.0.2"}); err != nil {
		t.Fatal(err)
	}
	hash := secure.Hash(token)
	f.repo.mu.Lock()
	s := f.repo.sessions[string(hash)]
	s.CreatedAt, s.LastSeen = s.CreatedAt.Add(-2*time.Hour), time.Now().Add(-61*time.Minute)
	f.repo.mu.Unlock()
	if _, err := ask[*models.Principal](f, Authenticate{Token: token, IP: "10.0.0.2"}); !ierrors.Is(err, ierrors.CodeUnauthenticated) {
		t.Fatalf("an idle session was accepted: %v", err)
	}
	if _, err := f.repo.GetSession(context.Background(), hash); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatal("the idle session was not deleted")
	}
	cfg := *f.deps.Cfg
	cfg.SessionIdleTimeout = 5 * time.Minute
	if err := cfg.Validate(); err == nil {
		t.Fatal("an idle timeout shorter than the use is recorded was accepted")
	}
}

// totpRepo stores one user. Each replacement of the recovery codes takes
// longer than the one before, so a racing confirmation writes last.
type totpRepo struct {
	Repo
	mu       sync.Mutex
	user     models.User
	codes    [][]byte
	replaced int
}

func (r *totpRepo) GetUser(context.Context, gocql.UUID) (*models.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u := r.user
	return &u, nil
}

func (r *totpRepo) GetUsers(ctx context.Context, _ []gocql.UUID) ([]*models.User, error) {
	u, err := r.GetUser(ctx, gocql.UUID{})
	return []*models.User{u}, err
}

func (r *totpRepo) ReplaceRecoveryCodes(_ context.Context, _ gocql.UUID, hashes [][]byte) error {
	r.mu.Lock()
	r.replaced++
	wait := time.Duration(r.replaced*r.replaced) * 10 * time.Millisecond
	r.mu.Unlock()
	time.Sleep(wait)
	r.mu.Lock()
	r.codes = hashes
	r.mu.Unlock()
	return nil
}

func (r *totpRepo) EnableTOTP(_ context.Context, _ gocql.UUID, sealed []byte, step int64, _ time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !bytes.Equal(r.user.TOTPPending, sealed) {
		return false, nil
	}
	r.user.TOTPEnabled, r.user.TOTPSecret, r.user.TOTPPending, r.user.TOTPLastStep = true, sealed, nil, step
	return true, nil
}

func TestConfirmShowsTheStoredCodes(t *testing.T) {
	key := make([]byte, 32)
	sealer, err := secure.NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := totp.Generate("GoChat", "ada")
	if err != nil {
		t.Fatal(err)
	}
	id := ids.New()
	repo := &totpRepo{user: models.User{ID: id, TOTPPending: sealer.Seal([]byte(secret), id[:])}}
	w := &worker{Deps: &Deps{
		Logger: zap.NewNop(), Cfg: &Config{}, Repo: repo, Users: users.NewCache(repo, 10, time.Minute),
		sealer: sealer, recoveryKey: secure.DeriveKey(key, "recovery-codes"),
		failures: window.New(10, time.Minute), totp: keylock.New(),
	}}
	code, err := potp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// A double submission of the form.
	p := &models.Principal{UserID: id, Role: models.SystemRoleUser}
	shown := make([]RecoveryCodes, 2)
	var wg sync.WaitGroup
	for i := range shown {
		wg.Go(func() {
			shown[i], _ = lockTOTP(context.Background(), w, p, ConfirmTOTP{P: p, Code: code}, w.confirmTOTP)
		})
	}
	wg.Wait()
	shown = slices.DeleteFunc(shown, func(c RecoveryCodes) bool { return c == nil })
	if len(shown) != 1 {
		t.Fatalf("%d confirmations showed codes", len(shown))
	}
	for i, c := range shown[0] {
		if !bytes.Equal(w.recoveryHash(id, c), repo.codes[i]) {
			t.Fatal("the codes shown are not the ones stored")
		}
	}
	if _, err := lockTOTP(context.Background(), w, p, ConfirmTOTP{P: p, Code: code}, w.confirmTOTP); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("confirming again: %v", err)
	}
}
