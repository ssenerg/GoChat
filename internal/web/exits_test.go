package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"storj.io/drpc/drpcmetadata"
	"storj.io/drpc/drpcmux"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

// annSession is the one session the exit fakes know.
var annSession = strings.Repeat("S", 43)

func signedInAs(ctx context.Context) bool {
	md, _ := drpcmetadata.Get(ctx)
	return md["x-session"] == annSession
}

// exitAuth answers for ann, who leaves and deletes her account.
type exitAuth struct {
	pb.DRPCAuthServiceUnimplementedServer
	mu                sync.Mutex
	hasPassword, totp bool
	// refuse, when set, is why a deletion fails.
	refuse  string
	deletes []*pb.DeleteAccountRequest
	begun   []*pb.BeginSSORequest
}

func (f *exitAuth) Me(ctx context.Context, _ *pb.MeRequest) (*pb.MeResponse, error) {
	if !signedInAs(ctx) {
		return nil, rpcErr(ierrors.CodeUnauthenticated, "sign in")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return &pb.MeResponse{User: &pb.User{Id: "ann", Username: "ann", TotpEnabled: f.totp}, HasPassword: f.hasPassword}, nil
}

func (f *exitAuth) ListSSOIdentities(context.Context, *pb.ListSSOIdentitiesRequest) (*pb.ListSSOIdentitiesResponse, error) {
	return &pb.ListSSOIdentitiesResponse{Identities: []*pb.SSOIdentity{{ProviderId: "corp", ProviderName: "Corp ID"}}}, nil
}

func (f *exitAuth) DeleteAccount(ctx context.Context, req *pb.DeleteAccountRequest) (*pb.DeleteAccountResponse, error) {
	if !signedInAs(ctx) {
		return nil, rpcErr(ierrors.CodeUnauthenticated, "sign in")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, req)
	if f.refuse != "" {
		return nil, rpcErr(ierrors.CodeFailedPrecondition, f.refuse)
	}
	return &pb.DeleteAccountResponse{}, nil
}

func (f *exitAuth) BeginSSO(_ context.Context, req *pb.BeginSSORequest) (*pb.BeginSSOResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.begun = append(f.begun, req)
	state := secure.Token()
	return &pb.BeginSSOResponse{Url: "https://id.corp.test/auth?state=" + state, State: state}, nil
}

var reauthToken = strings.Repeat("R", 43)

func (f *exitAuth) FinishSSO(ctx context.Context, req *pb.FinishSSORequest) (*pb.FinishSSOResponse, error) {
	if req.Code != "again" || !signedInAs(ctx) {
		return nil, rpcErr(ierrors.CodeFailedPrecondition, "sign in with the Corp ID account linked to yours")
	}
	return &pb.FinishSSOResponse{ProviderName: "Corp ID", Result: &pb.FinishSSOResponse_ReauthToken{ReauthToken: reauthToken}}, nil
}

// exitTeams holds ann's one team, acme.
type exitTeams struct {
	pb.DRPCTeamServiceUnimplementedServer
	mu     sync.Mutex
	role   pb.TeamRole
	refuse string
	left   int
}

func (f *exitTeams) GetTeam(ctx context.Context, _ *pb.GetTeamRequest) (*pb.GetTeamResponse, error) {
	if !signedInAs(ctx) {
		return nil, rpcErr(ierrors.CodeUnauthenticated, "sign in")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return &pb.GetTeamResponse{Team: &pb.Team{Id: "team-1", Name: "acme", DisplayName: "Acme", Role: f.role}}, nil
}

func (f *exitTeams) LeaveTeam(_ context.Context, req *pb.LeaveTeamRequest) (*pb.LeaveTeamResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.TeamId != "team-1" {
		return nil, rpcErr(ierrors.CodeNotFound, "team not found")
	}
	if f.refuse != "" {
		return nil, rpcErr(ierrors.CodeFailedPrecondition, f.refuse)
	}
	f.left++
	return &pb.LeaveTeamResponse{}, nil
}

func (f *exitTeams) set(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

func newExits(t *testing.T) (*browser, *exitAuth, *exitTeams) {
	t.Helper()
	auth, teams := &exitAuth{hasPassword: true}, &exitTeams{role: pb.TeamRole_TEAM_ROLE_MEMBER}
	b := newGateway(t, func(mux *drpcmux.Mux) error {
		if err := pb.DRPCRegisterAuthService(mux, auth); err != nil {
			return err
		}
		return pb.DRPCRegisterTeamService(mux, teams)
	})
	b.signIn(annSession)
	b.hx = true
	return b, auth, teams
}

func TestLeaveTeamFromItsSettings(t *testing.T) {
	b, _, teams := newExits(t)
	page := b.do(http.MethodGet, "/t/acme/settings", nil).Body.String()
	if !strings.Contains(page, `hx-post="/t/acme/leave"`) || strings.Contains(page, "team-photo") {
		t.Fatalf("a member's team settings: %s", page)
	}
	teams.set(func() { teams.role = pb.TeamRole_TEAM_ROLE_ADMIN })
	if page := b.do(http.MethodGet, "/t/acme/settings", nil).Body.String(); !strings.Contains(page, `hx-post="/t/acme/leave"`) || !strings.Contains(page, "team-photo") {
		t.Fatalf("an admin's team settings: %s", page)
	}

	teams.set(func() { teams.refuse = "you are the only admin of Acme: make another member an admin before you leave" })
	w := b.do(http.MethodPost, "/t/acme/leave", url.Values{})
	if w.Code != http.StatusOK || w.Header().Get("HX-Redirect") != "" || !strings.Contains(w.Body.String(), "you are the only admin of Acme") {
		t.Fatalf("a refusal: %d %q %s", w.Code, w.Header().Get("HX-Redirect"), w.Body.String())
	}
	teams.set(func() { teams.refuse = "" })
	if w := b.do(http.MethodPost, "/t/acme/leave", url.Values{}); w.Header().Get("HX-Redirect") != "/" || teams.left != 1 {
		t.Fatalf("leaving: %d %q, left %d", w.Code, w.Header().Get("HX-Redirect"), teams.left)
	}
}

func TestDeleteAccountWithThePassword(t *testing.T) {
	b, auth, _ := newExits(t)
	auth.totp = true
	auth.refuse = "you are the only admin of Acme: make another member an admin before you delete your account"
	w := b.do(http.MethodPost, "/settings/delete", url.Values{"password": {"pw"}, "code": {"123456"}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "you are the only admin of Acme") || !strings.Contains(w.Body.String(), `name="code"`) {
		t.Fatalf("a refusal: %d %s", w.Code, w.Body.String())
	}
	if b.cookies["gochat_session"] == nil {
		t.Fatal("a refusal signed the browser out")
	}
	auth.mu.Lock()
	auth.refuse = ""
	auth.mu.Unlock()
	if w := b.do(http.MethodPost, "/settings/delete", url.Values{"password": {"pw"}, "code": {"123456"}}); w.Header().Get("HX-Redirect") != "/login?deleted=1" {
		t.Fatalf("deleting: %d %q", w.Code, w.Header().Get("HX-Redirect"))
	}
	if got := auth.deletes[1]; got.Password != "pw" || got.Code != "123456" || got.SsoToken != "" {
		t.Fatalf("asked to delete with %+v", got)
	}
	if b.cookies["gochat_session"] != nil {
		t.Fatal("the browser kept the session")
	}
	if page := b.do(http.MethodGet, "/login?deleted=1", nil).Body.String(); !strings.Contains(page, "Your account was deleted.") {
		t.Fatalf("login page: %s", page)
	}
}

func TestDeleteAccountAfterSigningInAgain(t *testing.T) {
	b, auth, _ := newExits(t)
	auth.hasPassword = false
	w := b.do(http.MethodPost, "/settings/delete/sso/corp", url.Values{})
	loc, err := url.Parse(w.Header().Get("HX-Redirect"))
	if err != nil || loc.Host != "id.corp.test" || len(auth.begun) != 1 || !auth.begun[0].Reauth || auth.begun[0].ProviderId != "corp" {
		t.Fatalf("starting: %d %q %+v", w.Code, w.Header().Get("HX-Redirect"), auth.begun)
	}
	b.hx = false
	b.expect(b.do(http.MethodGet, ssoCallback+"?code=again&state="+loc.Query().Get("state"), nil), http.StatusSeeOther, "/settings#delete-account")
	if b.cookies["gochat_sso_reauth"] == nil {
		t.Fatal("no proof of the sign-in")
	}
	b.hx = true
	if w := b.do(http.MethodPost, "/settings/delete", url.Values{}); w.Header().Get("HX-Redirect") != "/login?deleted=1" {
		t.Fatalf("deleting: %d %q %s", w.Code, w.Header().Get("HX-Redirect"), w.Body.String())
	}
	if got := auth.deletes[0]; got.SsoToken != reauthToken || got.Password != "" {
		t.Fatalf("asked to delete with %+v", got)
	}
	if b.cookies["gochat_sso_reauth"] != nil || b.cookies["gochat_session"] != nil {
		t.Fatal("the browser kept the proof or the session")
	}
}

func TestAFailedDeletionSpendsTheSignInAgain(t *testing.T) {
	b, auth, _ := newExits(t)
	auth.hasPassword, auth.refuse = false, "invalid code"
	b.cookies["gochat_sso_reauth"] = &http.Cookie{Name: "gochat_sso_reauth", Value: url.Values{"token": {reauthToken}, "provider": {"Corp ID"}}.Encode()}
	w := b.do(http.MethodPost, "/settings/delete", url.Values{"code": {"1"}})
	if body := w.Body.String(); !strings.Contains(body, "invalid code") || !strings.Contains(body, "Continue with Corp ID") {
		t.Fatalf("a failed deletion: %s", body)
	}
	if b.cookies["gochat_sso_reauth"] != nil {
		t.Fatal("the spent proof stayed")
	}
}

func TestAnAppSignsInAgainToDelete(t *testing.T) {
	b, auth := newFlow(t)
	challenge := strings.Repeat("c", 43)
	state := b.toProvider("/login/sso/corp?app=" + challenge + "&reauth=1")
	if got := auth.begun[0]; got.AppChallenge != challenge || !got.Reauth {
		t.Fatalf("begin: %+v", got)
	}
	b.expect(b.do(http.MethodGet, ssoCallback+"?code=app&state="+state, nil), http.StatusSeeOther, appRedirect+"?code=app-code")
	b.toProvider("/login/sso/corp?app=" + challenge)
	if auth.begun[1].Reauth {
		t.Fatal("a sign-in asked to sign in again")
	}
}
