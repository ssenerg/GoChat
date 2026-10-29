package accounts

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gocql/gocql"
	potp "github.com/pquerna/otp/totp"

	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

func (f *fakeRepo) EraseUser(_ context.Context, u *models.User, name string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur := f.users[u.ID]
	*cur = models.User{
		ID: cur.ID, Role: cur.Role, CreatedAt: cur.CreatedAt, DisplayName: name, Deactivated: true, DeletedAt: now, UpdatedAt: now,
	}
	return nil
}

func (f *fakeRepo) ListSystemAdmins(context.Context) ([]gocql.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []gocql.UUID
	for id, u := range f.users {
		if u.Role == models.SystemRoleAdmin && !u.Deactivated {
			out = append(out, id)
		}
	}
	return out, nil
}

// deleted asks to delete p's account and, if allowed, erases it.
func (f *fixture) deleted(msg DeleteAccount) error {
	f.t.Helper()
	if _, err := ask[Done](f, msg); err != nil {
		return err
	}
	_, err := ask[Done](f, EraseAccount{P: msg.P})
	return err
}

func TestDeleteAccountErasesIt(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.OpenSignup = true })
	f.repo.bootstrapped = true
	admin := f.user("ada", models.SystemRoleAdmin)
	bob := f.user("bob", models.SystemRoleUser)
	ap, _ := f.login(admin, "10.0.0.1")
	bp, token := f.login(bob, "10.0.0.2")
	f.login(bob, "10.0.0.3")
	if _, err := ask[Done](f, DeleteAccount{P: bp, Password: "wrong-password"}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("a wrong password: %v", err)
	}
	if err := f.deleted(DeleteAccount{P: bp, Password: "bob-password"}); err != nil {
		t.Fatal(err)
	}
	u, _ := f.repo.GetUser(context.Background(), bob.ID)
	if u.Username != "" || u.Email != "" || u.PasswordHash != "" || u.DisplayName != DeletedName || !u.Deactivated || u.DeletedAt.IsZero() {
		t.Fatalf("left of the account: %+v", u)
	}
	if got := f.revocation(); got.UserID != bob.ID.String() || !got.All || len(got.SessionIDs) != 2 {
		t.Fatalf("revocation: %+v", got)
	}
	if got := f.forgotten(); got.UserID != bob.ID || !got.All {
		t.Fatalf("push forgot: %+v", got)
	}
	if _, err := ask[*models.Principal](f, Authenticate{Token: token}); !ierrors.Is(err, ierrors.CodeUnauthenticated) {
		t.Fatalf("a deleted account's session: %v", err)
	}
	if _, err := ask[*LoginResult](f, Login{Login: "bob", Password: "bob-password"}); !ierrors.Is(err, ierrors.CodeUnauthenticated) {
		t.Fatalf("a deleted account signed in: %v", err)
	}
	if _, err := ask[*models.User](f, SetDeactivated{P: ap, UserID: bob.ID.String(), Deactivated: false}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("an admin reactivated a deleted account: %v", err)
	}
	// The username and the email are free again.
	if _, err := signUp(f, "bob", ""); err != nil {
		t.Fatalf("signing up as bob again: %v", err)
	}
}

func TestDeleteAccountAsksForTheSecondFactor(t *testing.T) {
	f := newFixture(t, nil)
	bob := f.user("bob", models.SystemRoleUser)
	bp, _ := f.login(bob, "10.0.0.2")
	secret := "JBSWY3DPEHPK3PXP"
	f.repo.mu.Lock()
	f.repo.users[bob.ID].TOTPEnabled = true
	f.repo.users[bob.ID].TOTPSecret = f.deps.sealer.Seal([]byte(secret), bob.ID[:])
	f.repo.mu.Unlock()
	for _, code := range []string{"", "000000"} {
		if err := f.deleted(DeleteAccount{P: bp, Password: "bob-password", Code: code}); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
			t.Fatalf("code %q: %v", code, err)
		}
	}
	code, err := potp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.deleted(DeleteAccount{P: bp, Password: "bob-password", Code: code}); err != nil {
		t.Fatal(err)
	}
	if u, _ := f.repo.GetUser(context.Background(), bob.ID); u.DeletedAt.IsZero() || u.TOTPEnabled || u.TOTPSecret != nil {
		t.Fatalf("left of the account: %+v", u)
	}
}

func TestTheLastSystemAdminStays(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	ada := f.user("ada", models.SystemRoleAdmin)
	ap, _ := f.login(ada, "10.0.0.1")
	// Refused before the password is checked, so no code would be spent.
	if err := f.deleted(DeleteAccount{P: ap, Password: "wrong-password"}); !errors.Is(err, errOnlyAdmin) {
		t.Fatalf("the only admin: %v", err)
	}
	// Two admins deleting at once: the second is refused before anything
	// of theirs is taken apart.
	bea := f.user("bea", models.SystemRoleAdmin)
	bp, _ := f.login(bea, "10.0.0.2")
	msgs := []DeleteAccount{{P: ap, Password: "ada-password"}, {P: bp, Password: "bea-password"}}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, msg := range msgs {
		wg.Go(func() { _, errs[i] = ask[Done](f, msg) })
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("both or neither admin went: %v, %v", errs[0], errs[1])
	}
	stays := 0
	if errs[0] == nil {
		stays = 1
	}
	going, staying := msgs[1-stays].P, msgs[stays].P
	if !errors.Is(errs[stays], errOnlyAdmin) {
		t.Fatalf("the admin who stays: %v", errs[stays])
	}
	if admins, _ := f.repo.ListSystemAdmins(ctx); len(admins) != 1 || admins[0] != staying.UserID {
		t.Fatalf("active admins: %v", admins)
	}
	// Nor does the one going take the other with them.
	if _, err := ask[*models.User](f, SetDeactivated{P: going, UserID: staying.UserID.String(), Deactivated: true}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("deactivated the last admin: %v", err)
	}
	if _, err := ask[Done](f, EraseAccount{P: going}); err != nil {
		t.Fatal(err)
	}
}

func TestADeletionSuspendsTheAccountUntilItEnds(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	bob := f.user("bob", models.SystemRoleUser)
	bp, token := f.login(bob, "10.0.0.2")
	if _, err := ask[Done](f, DeleteAccount{P: bp, Password: "bob-password"}); err != nil {
		t.Fatal(err)
	}
	// While it is taken apart, nothing they do races it.
	if got := f.revocation(); got.UserID != bob.ID.String() || !got.All {
		t.Fatalf("revocation: %+v", got)
	}
	if _, err := ask[*models.Principal](f, Authenticate{Token: token}); !ierrors.Is(err, ierrors.CodeUnauthenticated) {
		t.Fatalf("a session of an account being deleted: %v", err)
	}
	if _, err := ask[*LoginResult](f, Login{Login: "bob", Password: "bob-password"}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("signed in to an account being deleted: %v", err)
	}
	// One that stops short gives the account back, to sign in to again.
	if _, err := ask[Done](f, ResumeAccount{P: bp}); err != nil {
		t.Fatal(err)
	}
	bp, _ = f.login(bob, "10.0.0.2")
	// Erased, there is no way back.
	if err := f.deleted(DeleteAccount{P: bp, Password: "bob-password"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ask[Done](f, ResumeAccount{P: bp}); err != nil {
		t.Fatal(err)
	}
	if u, _ := f.repo.GetUser(ctx, bob.ID); !u.Deactivated || u.DeletedAt.IsZero() {
		t.Fatalf("resumed a deleted account: %+v", u)
	}
}

func TestTeammatesHearOfADeletion(t *testing.T) {
	f := newFixture(t, nil)
	bob := f.user("bob", models.SystemRoleUser)
	bp, _ := f.login(bob, "10.0.0.2")
	ann := ids.New()
	if _, err := ask[Done](f, DeleteAccount{P: bp, Password: "bob-password"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ask[Done](f, EraseAccount{P: bp, Tell: []gocql.UUID{ann}}); err != nil {
		t.Fatal(err)
	}
	select {
	case d := <-f.delivered:
		if u := d.Event.GetProfileChanged().GetUser(); !slices.Equal(d.UserIDs, []string{ann.String()}) || u.GetId() != bob.ID.String() || u.GetDisplayName() != DeletedName {
			t.Fatalf("told %v of %v", d.UserIDs, u)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no teammate was told")
	}
}

func TestASignInAgainIsFresh(t *testing.T) {
	f, idp := newSSOFixture(t, nil)
	ann := f.signedIn(f.sso(idp, person{}, BeginSSO{}, FinishSSO{}))
	for reauth, want := range map[bool]string{true: "0", false: ""} {
		start, err := ask[*SSOStart](f, BeginSSO{P: ann, Provider: "idp", RedirectURI: testRedirect, Reauth: reauth})
		if err != nil {
			t.Fatal(err)
		}
		if u, _ := url.Parse(start.URL); u.Query().Get("max_age") != want {
			t.Fatalf("reauth %v asked for %s", reauth, start.URL)
		}
	}
	// A provider that let a live session through, or does not say.
	for name, claims := range map[string]map[string]any{
		"an hour-old sign-in": {"auth_time": time.Now().Add(-time.Hour).Unix()},
		"no time of sign-in":  {"auth_time": nil},
	} {
		if _, err := f.sso(idp, person{claims: claims}, BeginSSO{P: ann, Reauth: true}, FinishSSO{P: ann}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if res, err := f.sso(idp, person{}, BeginSSO{P: ann, Reauth: true}, FinishSSO{P: ann}); err != nil || res.ReauthToken == "" {
		t.Fatalf("a fresh sign-in: %v %+v", err, res)
	}
}

func TestADeletedAccountsSignInIsFreed(t *testing.T) {
	f, idp := newSSOFixture(t, nil)
	ctx := context.Background()
	ann := f.signedIn(f.sso(idp, person{}, BeginSSO{}, FinishSSO{}))
	// Erased by a deletion that failed to unlink it.
	u, _ := f.repo.GetUser(ctx, ann.UserID)
	if err := f.repo.EraseUser(ctx, u, DeletedName, time.Now()); err != nil {
		t.Fatal(err)
	}
	again := f.signedIn(f.sso(idp, person{}, BeginSSO{}, FinishSSO{}))
	if again.UserID == ann.UserID {
		t.Fatal("signed in to the deleted account")
	}
}

func TestSSOAccountsSignInAgainToDelete(t *testing.T) {
	f, idp := newSSOFixture(t, nil)
	ann := f.signedIn(f.sso(idp, person{}, BeginSSO{}, FinishSSO{}))
	bob := f.signedIn(f.sso(idp, person{claims: map[string]any{"sub": "bob", "email": "bob@example.test"}}, BeginSSO{}, FinishSSO{}))
	if err := f.deleted(DeleteAccount{P: ann, Password: "anything"}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("no password, no sign-in: %v", err)
	}
	if _, err := ask[*SSOStart](f, BeginSSO{Provider: "idp", RedirectURI: testRedirect, Reauth: true}); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
		t.Fatalf("a reauth for no one: %v", err)
	}
	reauth := func(p *models.Principal, sub string) (*SSOResult, error) {
		return f.sso(idp, person{claims: map[string]any{"sub": sub}}, BeginSSO{P: p, Reauth: true}, FinishSSO{P: p})
	}
	// Signing in as bob proves nothing of ann.
	if _, err := reauth(ann, "bob"); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("another account's reauth: %v", err)
	}
	bobs, err := reauth(bob, "bob")
	if err != nil || bobs.ReauthToken == "" || bobs.Login != nil {
		t.Fatalf("bob's reauth: %v %+v", err, bobs)
	}
	if err := f.deleted(DeleteAccount{P: ann, SSOToken: bobs.ReauthToken}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("ann deleted with bob's token: %v", err)
	}
	res, err := reauth(ann, "subject-1")
	if err != nil || res.ReauthToken == "" {
		t.Fatalf("ann's reauth: %v %+v", err, res)
	}
	if err := f.deleted(DeleteAccount{P: ann, SSOToken: res.ReauthToken}); err != nil {
		t.Fatal(err)
	}
	if list, _ := f.repo.ListUserSSOIdentities(context.Background(), ann.UserID); len(list) != 0 {
		t.Fatalf("%d sign-ins stayed linked", len(list))
	}
	// The identity and its email are free: signing in makes a new account.
	again := f.signedIn(f.sso(idp, person{}, BeginSSO{}, FinishSSO{}))
	if again.UserID == ann.UserID {
		t.Fatal("signed in to the deleted account")
	}
}

func TestAppsSignInAgainToDelete(t *testing.T) {
	f, idp := newSSOFixture(t, nil)
	ann := f.signedIn(f.sso(idp, person{}, BeginSSO{}, FinishSSO{}))
	verifier := secure.Token() + secure.Token()
	appReauth := func() string {
		res, err := f.sso(idp, person{}, BeginSSO{AppChallenge: s256(verifier), Reauth: true}, FinishSSO{})
		if err != nil || res.AppCode == "" || res.Login != nil {
			t.Fatalf("app reauth: %v %+v", err, res)
		}
		return res.AppCode
	}
	if err := f.deleted(DeleteAccount{P: ann, SSOToken: appReauth(), SSOVerifier: secure.Token() + secure.Token()}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("another verifier: %v", err)
	}
	// A code is no sign-in either.
	if _, err := ask[*LoginResult](f, RedeemSSO{Code: appReauth(), Verifier: verifier}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("a reauth code signed in: %v", err)
	}
	if err := f.deleted(DeleteAccount{P: ann, SSOToken: appReauth(), SSOVerifier: verifier}); err != nil {
		t.Fatal(err)
	}
}
