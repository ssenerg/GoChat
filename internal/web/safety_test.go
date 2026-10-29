package web

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
	"storj.io/drpc/drpcmux"
	"storj.io/drpc/drpcserver"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/client/drpc"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/web/render"
)

const (
	testChannel = "11111111-1111-4111-8111-111111111111"
	testMessage = "22222222-2222-1222-8222-222222222222"
	testTeam    = "33333333-3333-4333-8333-333333333333"
	testPeer    = "44444444-4444-4444-8444-444444444444"
	testReport  = "55555555-5555-1555-8555-555555555555"
)

// fakeSafety records what the gateway asks of the safety service.
type fakeSafety struct {
	pb.DRPCSafetyServiceUnimplementedServer
	mu       sync.Mutex
	blocked  map[string]bool
	reports  []*pb.ReportRequest
	resolved []*pb.ResolveReportRequest
}

func (f *fakeSafety) BlockUser(_ context.Context, req *pb.BlockUserRequest) (*pb.BlockUserResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked[req.UserId] = true
	return &pb.BlockUserResponse{}, nil
}

func (f *fakeSafety) UnblockUser(_ context.Context, req *pb.UnblockUserRequest) (*pb.UnblockUserResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.blocked, req.UserId)
	return &pb.UnblockUserResponse{}, nil
}

func (f *fakeSafety) ListBlocked(context.Context, *pb.ListBlockedRequest) (*pb.ListBlockedResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &pb.ListBlockedResponse{}
	for id := range f.blocked {
		out.Blocked = append(out.Blocked, &pb.BlockedUser{User: &pb.User{Id: id, Username: "bob"}})
	}
	return out, nil
}

func (f *fakeSafety) Report(_ context.Context, req *pb.ReportRequest) (*pb.ReportResponse, error) {
	if req.Reason == pb.ReportReason_REPORT_REASON_UNSPECIFIED {
		return nil, rpcErr(ierrors.CodeInvalidArgument, "pick a reason")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, req)
	return &pb.ReportResponse{}, nil
}

func (f *fakeSafety) ResolveReport(_ context.Context, req *pb.ResolveReportRequest) (*pb.ResolveReportResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, req)
	if len(f.resolved) > 1 {
		return nil, rpcErr(ierrors.CodeFailedPrecondition, "an admin already acted on this report")
	}
	return &pb.ResolveReportResponse{Report: &pb.Report{
		Id: req.ReportId, Reason: pb.ReportReason_REPORT_REASON_SPAM, Action: req.Action, MessageId: testMessage,
		User: &pb.User{Id: testPeer, Username: "bob"}, Reporter: &pb.User{Id: "u", Username: "ann"},
		ResolvedBy: &pb.User{Id: "u", Username: "ann", DisplayName: "Ann"}, CreatedAt: timestamppb.Now(), ResolvedAt: timestamppb.Now(),
	}}, nil
}

type fakeChannels struct {
	pb.DRPCChannelServiceUnimplementedServer
}

func (fakeChannels) GetChannel(_ context.Context, req *pb.GetChannelRequest) (*pb.GetChannelResponse, error) {
	return &pb.GetChannelResponse{Channel: &pb.Channel{Id: req.ChannelId, TeamId: testTeam, Type: pb.ChannelType_CHANNEL_TYPE_PUBLIC}}, nil
}

type fakeTeams struct {
	pb.DRPCTeamServiceUnimplementedServer
}

func (fakeTeams) GetTeam(_ context.Context, req *pb.GetTeamRequest) (*pb.GetTeamResponse, error) {
	return &pb.GetTeamResponse{Team: &pb.Team{Id: req.GetId(), Name: "acme", DisplayName: "Acme"}}, nil
}

type fakeUsers struct {
	pb.DRPCUserServiceUnimplementedServer
}

func (fakeUsers) GetUser(_ context.Context, req *pb.GetUserRequest) (*pb.GetUserResponse, error) {
	return &pb.GetUserResponse{User: &pb.User{Id: req.UserId, Username: "bob", DisplayName: "Bob"}}, nil
}

// newSafetyFlow is a browser signed in to a gateway whose server blocks
// and takes reports.
func newSafetyFlow(t *testing.T) (*browser, *fakeSafety) {
	t.Helper()
	auth, safety := &fakeAuth{}, &fakeSafety{blocked: map[string]bool{}}
	mux := drpcmux.New()
	for _, err := range []error{
		pb.DRPCRegisterAuthService(mux, auth),
		pb.DRPCRegisterSafetyService(mux, safety),
		pb.DRPCRegisterChannelService(mux, &fakeChannels{}),
		pb.DRPCRegisterTeamService(mux, &fakeTeams{}),
		pb.DRPCRegisterUserService(mux, &fakeUsers{}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var served sync.WaitGroup
	served.Go(func() { _ = drpcserver.New(mux).Serve(ctx, lis) })
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
		served.Wait()
	})
	r, err := render.New(16)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(zap.NewNop(), &Config{PublicURL: flowOrigin, RateLimit: &RateLimitConfig{AuthPerMinute: 6000, AuthBurst: 100, RequestsPerSecond: 100, RequestBurst: 100}}, client, r, nil, nil)
	b := &browser{t: t, h: h.Routes(), cookies: make(map[string]*http.Cookie)}
	b.do(http.MethodGet, "/login", nil)
	b.expect(b.do(http.MethodPost, "/login", url.Values{"login": {"ann"}, "password": {"pw"}}), http.StatusSeeOther, "/")
	return b, safety
}

func TestReportingAMessageOnTheWeb(t *testing.T) {
	b, safety := newSafetyFlow(t)
	path := "/m/" + testChannel + "/" + testMessage + "/report?root=" + testMessage
	if w := b.do(http.MethodGet, path, nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Report this message") ||
		!strings.Contains(w.Body.String(), `value="harassment"`) {
		t.Fatalf("the form: %d %s", w.Code, w.Body.String())
	}
	// A refusal shows in the form, which keeps what was written.
	w := b.do(http.MethodPost, path, url.Values{"details": {"they keep at it"}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "pick a reason") || !strings.Contains(w.Body.String(), "they keep at it") {
		t.Fatalf("a refused report: %d %s", w.Code, w.Body.String())
	}
	w = b.do(http.MethodPost, path, url.Values{"reason": {"harassment"}, "details": {"they keep at it"}})
	if w.Code != http.StatusOK || w.Header().Get("HX-Trigger") != "closeModal" || !strings.Contains(w.Body.String(), "The admins will look at your report") {
		t.Fatalf("a report: %d %q %s", w.Code, w.Header().Get("HX-Trigger"), w.Body.String())
	}
	if len(safety.reports) != 1 {
		t.Fatalf("%d reports sent", len(safety.reports))
	}
	got := safety.reports[0]
	if got.Reason != pb.ReportReason_REPORT_REASON_HARASSMENT || got.Details != "they keep at it" || got.GetMessage().GetChannelId() != testChannel ||
		got.GetMessage().GetMessageId() != testMessage || got.GetMessage().GetRootId() != testMessage {
		t.Fatalf("reported %v", got)
	}
	if w := b.do(http.MethodGet, "/m/nope/"+testMessage+"/report", nil); w.Code != http.StatusNotFound {
		t.Fatalf("a made-up channel: %d", w.Code)
	}
}

func TestBlockingAndReportingAPersonOnTheWeb(t *testing.T) {
	b, safety := newSafetyFlow(t)
	card := b.do(http.MethodGet, "/u/"+testPeer+"?channel="+testChannel, nil)
	if card.Code != http.StatusOK || !strings.Contains(card.Body.String(), ">Block<") || !strings.Contains(card.Body.String(), "/u/"+testPeer+"/report?team="+testTeam) ||
		!strings.Contains(card.Body.String(), `hx-post="/t/acme/dm"`) {
		t.Fatalf("the card: %d %s", card.Code, card.Body.String())
	}
	w := b.do(http.MethodPost, "/u/"+testPeer+"/block", url.Values{"blocked": {"1"}, "channel": {testChannel}})
	if w.Code != http.StatusOK || !safety.blocked[testPeer] || !strings.Contains(w.Body.String(), ">Unblock<") || strings.Contains(w.Body.String(), "/t/acme/dm") {
		t.Fatalf("blocking from the card: %d %v %s", w.Code, safety.blocked, w.Body.String())
	}
	if w := b.do(http.MethodGet, "/settings/blocked", nil); !strings.Contains(w.Body.String(), "@bob") {
		t.Fatalf("the settings' list: %s", w.Body.String())
	}
	w = b.do(http.MethodPost, "/u/"+testPeer+"/block", url.Values{"blocked": {"0"}, "from": {"settings"}})
	if w.Code != http.StatusOK || safety.blocked[testPeer] || !strings.Contains(w.Body.String(), "You have not blocked anyone") {
		t.Fatalf("unblocking from the settings: %d %s", w.Code, w.Body.String())
	}
	w = b.do(http.MethodPost, "/u/"+testPeer+"/report?team="+testTeam, url.Values{"reason": {"spam"}})
	if w.Code != http.StatusOK || len(safety.reports) != 1 || safety.reports[0].GetUser().GetUserId() != testPeer || safety.reports[0].GetUser().GetTeamId() != testTeam {
		t.Fatalf("reporting the person: %d %v", w.Code, safety.reports)
	}
}

func TestResolvingAReportOnTheWeb(t *testing.T) {
	b, safety := newSafetyFlow(t)
	w := b.do(http.MethodPost, "/reports/"+testReport+"/resolve", url.Values{"action": {"delete"}})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Message deleted by Ann") || len(safety.resolved) != 1 ||
		safety.resolved[0].Action != pb.ReportAction_REPORT_ACTION_DELETE_MESSAGE || safety.resolved[0].ReportId != testReport {
		t.Fatalf("resolving: %d %v %s", w.Code, safety.resolved, w.Body.String())
	}
	// A report another admin acted on says so, and leaves the row.
	w = b.do(http.MethodPost, "/reports/"+testReport+"/resolve", url.Values{"action": {"dismiss"}})
	if w.Code != http.StatusOK || w.Header().Get("HX-Reswap") != "none" || !strings.Contains(w.Body.String(), "already acted") {
		t.Fatalf("resolving again: %d %s", w.Code, w.Body.String())
	}
	// The every-team page is for system admins alone.
	if w := b.do(http.MethodGet, "/admin/reports", nil); w.Code != http.StatusNotFound {
		t.Fatalf("a member opened every team's reports: %d", w.Code)
	}
}

func TestSafetyCallsAreOpenToApps(t *testing.T) {
	for _, name := range []string{"BlockUser", "UnblockUser", "ListBlocked", "Report", "ListReports", "CountReports", "ResolveReport"} {
		if _, ok := apiMethods["gochat.v1.SafetyService/"+name]; !ok {
			t.Errorf("%s is not open to apps", name)
		}
	}
}
