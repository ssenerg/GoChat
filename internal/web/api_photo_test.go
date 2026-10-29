package web

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"storj.io/drpc/drpcmetadata"
	"storj.io/drpc/drpcmux"
	"storj.io/drpc/drpcserver"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/client/drpc"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/web/render"
)

// photoCaller is who a session is: their system role, and their role in
// the one team, unspecified when not a member.
type photoCaller struct {
	system pb.SystemRole
	team   pb.TeamRole
}

type photoCallers map[string]photoCaller

func (c photoCallers) of(ctx context.Context) (photoCaller, error) {
	md, _ := drpcmetadata.Get(ctx)
	who, ok := c[md["x-session"]]
	if !ok {
		return who, rpcErr(ierrors.CodeUnauthenticated, "sign in")
	}
	return who, nil
}

type photoAuth struct {
	pb.DRPCAuthServiceUnimplementedServer
	callers photoCallers
}

func (f *photoAuth) Me(ctx context.Context, _ *pb.MeRequest) (*pb.MeResponse, error) {
	who, err := f.callers.of(ctx)
	if err != nil {
		return nil, err
	}
	return &pb.MeResponse{User: &pb.User{Id: "u", Role: who.system}}, nil
}

type photoTeams struct {
	pb.DRPCTeamServiceUnimplementedServer
	callers photoCallers
	set     atomic.Int32
}

func (f *photoTeams) GetTeam(ctx context.Context, req *pb.GetTeamRequest) (*pb.GetTeamResponse, error) {
	who, err := f.callers.of(ctx)
	if err != nil {
		return nil, err
	}
	if who.team == pb.TeamRole_TEAM_ROLE_UNSPECIFIED {
		return nil, rpcErr(ierrors.CodeNotFound, "no such team")
	}
	return &pb.GetTeamResponse{Team: &pb.Team{Id: req.GetId(), Role: who.team}}, nil
}

func (f *photoTeams) SetTeamPhoto(_ context.Context, req *pb.SetTeamPhotoRequest) (*pb.SetTeamPhotoResponse, error) {
	f.set.Add(1)
	return &pb.SetTeamPhotoResponse{Team: &pb.Team{Id: req.TeamId}}, nil
}

type photoUsers struct {
	pb.DRPCUserServiceUnimplementedServer
	set atomic.Int32
}

func (f *photoUsers) SetPhoto(context.Context, *pb.SetPhotoRequest) (*pb.SetPhotoResponse, error) {
	f.set.Add(1)
	return &pb.SetPhotoResponse{User: &pb.User{Id: "u"}}, nil
}

// watchedBody says whether the gateway read the upload.
type watchedBody struct {
	io.Reader
	read atomic.Bool
}

func (b *watchedBody) Read(p []byte) (int, error) {
	b.read.Store(true)
	return b.Reader.Read(p)
}

func TestAPhotoIsReadOnlyForWhoMayChangeIt(t *testing.T) {
	callers := photoCallers{
		strings.Repeat("A", 43): {system: pb.SystemRole_SYSTEM_ROLE_USER, team: pb.TeamRole_TEAM_ROLE_ADMIN},
		strings.Repeat("M", 43): {system: pb.SystemRole_SYSTEM_ROLE_USER, team: pb.TeamRole_TEAM_ROLE_MEMBER},
		strings.Repeat("S", 43): {system: pb.SystemRole_SYSTEM_ROLE_ADMIN},
		strings.Repeat("O", 43): {system: pb.SystemRole_SYSTEM_ROLE_USER},
	}
	teams, users := &photoTeams{callers: callers}, &photoUsers{}
	mux := drpcmux.New()
	for _, err := range []error{
		pb.DRPCRegisterAuthService(mux, &photoAuth{callers: callers}),
		pb.DRPCRegisterTeamService(mux, teams),
		pb.DRPCRegisterUserService(mux, users),
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
	h := NewHandler(zap.NewNop(), &Config{
		PublicURL: "https://chat.test",
		RateLimit: &RateLimitConfig{AuthPerMinute: 6000, AuthBurst: 100, RequestsPerSecond: 100, RequestBurst: 100},
		HTTP:      &HTTPConfig{TransferTimeout: 10 * time.Second, TransferIdleTimeout: 10 * time.Second},
	}, client, r, nil, nil).Routes()

	var picture bytes.Buffer
	if err := png.Encode(&picture, image.NewGray(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatal(err)
	}
	upload := func(path, token string) (int, *pb.ApiError, bool) {
		body := &watchedBody{Reader: bytes.NewReader(picture.Bytes())}
		req := httptest.NewRequest(http.MethodPost, path, body)
		req.Header.Set("Accept", "application/proto")
		req.Header.Set("Authorization", "Bearer "+token)
		req.RemoteAddr = "203.0.113.7:4000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		e := &pb.ApiError{}
		if w.Code != http.StatusOK {
			_ = proto.Unmarshal(w.Body.Bytes(), e)
		}
		return w.Code, e, body.read.Load()
	}
	team := "/api/v2/teams/6f1c1f9e-8a51-4f3a-9d2e-1b7c2a9f0e11/photo"

	// Who may not change it is turned away before the picture is read.
	for _, c := range []struct {
		name, path, token string
		code              uint32
	}{
		{"a made-up token", team, strings.Repeat("X", 43), uint32(ierrors.CodeUnauthenticated)},
		{"a member", team, strings.Repeat("M", 43), uint32(ierrors.CodePermissionDenied)},
		{"someone of another team", team, strings.Repeat("O", 43), uint32(ierrors.CodeNotFound)},
		{"a made-up token's own photo", "/api/v1/photo", strings.Repeat("X", 43), uint32(ierrors.CodeUnauthenticated)},
	} {
		if status, e, read := upload(c.path, c.token); status == http.StatusOK || e.Code != c.code || read {
			t.Errorf("%s: %d %v, read the picture: %v", c.name, status, e, read)
		}
	}
	if teams.set.Load() != 0 || users.set.Load() != 0 {
		t.Fatalf("set %d team photos and %d of people", teams.set.Load(), users.set.Load())
	}

	// A team admin, a system admin and a signed-in person's own photo go
	// on, over a real connection, which an upload's deadlines need.
	srv := httptest.NewServer(h)
	defer srv.Close()
	for _, c := range []struct{ path, token string }{
		{team, strings.Repeat("A", 43)},
		{team, strings.Repeat("S", 43)},
		{"/api/v1/photo", strings.Repeat("M", 43)},
	} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+c.path, bytes.NewReader(picture.Bytes()))
		req.Header.Set("Authorization", "Bearer "+c.token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s by %s: %d %s", c.path, c.token[:1], res.StatusCode, body)
		}
	}
	if teams.set.Load() != 2 || users.set.Load() != 1 {
		t.Fatalf("set %d team photos and %d of people", teams.set.Load(), users.set.Load())
	}
}
