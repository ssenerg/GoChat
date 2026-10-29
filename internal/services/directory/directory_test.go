package directory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthdm/hollywood/actor"
	stringadapter "github.com/casbin/casbin/v2/persist/string-adapter"
	"github.com/gocql/gocql"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/keylock"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/channels"
	"github.com/ssenerg/gochat/internal/services/hub"
	"github.com/ssenerg/gochat/internal/services/users"
)

const policy = `
g, team_admin, team_member
p, system_admin, *, *
p, team_admin, invite, create
p, team_admin, team_member, remove
p, team_admin, team_member, update_role
`

// fakeRepo holds one team. Methods the tests do not need panic.
type fakeRepo struct {
	Repo
	mu       sync.Mutex
	team     gocql.UUID
	owner    gocql.UUID
	users    map[gocql.UUID]*models.User
	members  map[gocql.UUID]*scylla.TeamMemberEntry
	channels map[gocql.UUID][]gocql.UUID
	invites  []*models.Invite
	used     map[string]bool
	// log records the writes, and lag slows reads and writes to widen
	// races.
	log []string
	lag time.Duration
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		team: ids.New(), users: map[gocql.UUID]*models.User{}, members: map[gocql.UUID]*scylla.TeamMemberEntry{},
		channels: map[gocql.UUID][]gocql.UUID{}, used: map[string]bool{},
	}
}

func (f *fakeRepo) addMember(name string, role models.TeamRole) gocql.UUID {
	id := ids.New()
	f.users[id] = &models.User{ID: id, Username: name, Role: models.SystemRoleUser}
	f.members[id] = &scylla.TeamMemberEntry{
		TeamMember: models.TeamMember{TeamID: f.team, UserID: id, Role: role}, Username: name, DisplayName: strings.ToUpper(name),
	}
	return id
}

func (f *fakeRepo) GetUser(_ context.Context, id gocql.UUID) (*models.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := f.users[id]; u != nil {
		return u, nil
	}
	return nil, ierrors.NotFound("user not found")
}

func (f *fakeRepo) GetUsers(_ context.Context, list []gocql.UUID) ([]*models.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*models.User
	for _, id := range list {
		if u := f.users[id]; u != nil {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *fakeRepo) GetTeam(_ context.Context, id gocql.UUID) (*models.Team, error) {
	if id != f.team {
		return nil, ierrors.NotFound("not found")
	}
	return &models.Team{ID: id, CreatedBy: f.owner}, nil
}

func (f *fakeRepo) GetTeamMember(_ context.Context, teamID, userID gocql.UUID) (*models.TeamMember, error) {
	time.Sleep(f.lag)
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.members[userID]; e != nil && teamID == f.team {
		m := e.TeamMember
		return &m, nil
	}
	return nil, ierrors.NotFound("not a member")
}

func (f *fakeRepo) ListTeamMembers(context.Context, gocql.UUID) ([]*scylla.TeamMemberEntry, error) {
	time.Sleep(f.lag)
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*scylla.TeamMemberEntry, 0, len(f.members))
	for _, e := range f.members {
		c := *e
		out = append(out, &c)
	}
	return out, nil
}

func (f *fakeRepo) RemoveTeamMember(_ context.Context, _, userID gocql.UUID) error {
	time.Sleep(f.lag)
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.members, userID)
	f.log = append(f.log, "remove team member")
	return nil
}

func (f *fakeRepo) UpdateTeamMemberRole(_ context.Context, _, userID gocql.UUID, role models.TeamRole) error {
	time.Sleep(f.lag)
	f.mu.Lock()
	defer f.mu.Unlock()
	if e := f.members[userID]; e != nil {
		e.Role = role
	}
	return nil
}

func (f *fakeRepo) ListUserChannels(_ context.Context, userID, _ gocql.UUID) ([]*models.ChannelMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, "list channels")
	var out []*models.ChannelMember
	for _, ch := range f.channels[userID] {
		out = append(out, &models.ChannelMember{ChannelID: ch, UserID: userID, TeamID: f.team})
	}
	return out, nil
}

func (f *fakeRepo) ListTeamInvites(context.Context, gocql.UUID) ([]*models.Invite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.invites), nil
}

func (f *fakeRepo) FindTeamInvites(_ context.Context, _ gocql.UUID, prefix []byte) ([]*models.Invite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*models.Invite
	for _, inv := range f.invites {
		if bytes.HasPrefix(inv.TokenHash, prefix) {
			out = append(out, inv)
		}
	}
	return out, nil
}

func (f *fakeRepo) OpenInvites(_ context.Context, _ gocql.UUID, listed []*models.Invite) ([]*models.Invite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*models.Invite
	for _, inv := range listed {
		if !f.used[string(inv.TokenHash)] {
			out = append(out, inv)
		}
	}
	return out, nil
}

func (f *fakeRepo) RevokeInvite(_ context.Context, _ gocql.UUID, hash []byte) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.used[string(hash)] {
		return false, nil
	}
	f.used[string(hash)] = true
	return true, nil
}

func newWorker(t *testing.T, repo *fakeRepo) *worker {
	t.Helper()
	az, err := authz.New(zap.NewNop(), stringadapter.NewAdapter(policy))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	sink := engine.SpawnFunc(func(*actor.Context) {}, "hub")
	chans := engine.SpawnFunc(func(c *actor.Context) {
		if msg, ok := c.Message().(channels.RemoveMember); ok {
			repo.mu.Lock()
			repo.channels[msg.UserID] = slices.DeleteFunc(repo.channels[msg.UserID], func(id gocql.UUID) bool { return id == msg.ChannelID })
			repo.log = append(repo.log, "leave channel")
			repo.mu.Unlock()
			actors.Reply(c, channels.Done{})
		}
	}, "channels")
	return &worker{Deps: &Deps{
		Logger: zap.NewNop(), Cfg: &Config{OpTimeout: time.Second}, Repo: repo, Authz: az,
		Users: users.NewCache(repo, 100, time.Minute), Engine: engine, Hub: sink, Channels: chans,
		teamLocks: keylock.New(),
	}}
}

func principal(id gocql.UUID) *models.Principal {
	return &models.Principal{UserID: id, Role: models.SystemRoleUser}
}

func TestMembersPage(t *testing.T) {
	repo := newFakeRepo()
	me := repo.addMember("me", models.TeamRoleMember)
	for i := range 249 {
		repo.addMember(fmt.Sprintf("user%03d", i), models.TeamRoleMember)
	}
	w := newWorker(t, repo)
	var names []string
	after := ""
	for range 10 {
		res, err := w.listTeamMembers(context.Background(), ListTeamMembers{P: principal(me), TeamID: repo.team.String(), Limit: 100, After: after})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range res.Members {
			names = append(names, m.User.Username)
		}
		if after = res.Next; after == "" {
			break
		}
	}
	if len(names) != 250 || !slices.IsSorted(names) || slices.Compact(slices.Clone(names)) == nil || len(slices.Compact(slices.Clone(names))) != 250 {
		t.Fatalf("listed %d members, sorted %v", len(names), slices.IsSorted(names))
	}
	// A query pages too, and a full last page has nothing after it.
	res, err := w.listTeamMembers(context.Background(), ListTeamMembers{P: principal(me), TeamID: repo.team.String(), Query: "user1", Limit: 50})
	if err != nil || len(res.Members) != 50 || res.Next != "user149" {
		t.Fatalf("first page of user1: %d members, next %q, %v", len(res.Members), res.Next, err)
	}
	res, err = w.listTeamMembers(context.Background(), ListTeamMembers{P: principal(me), TeamID: repo.team.String(), Query: "user1", Limit: 50, After: res.Next})
	if err != nil || len(res.Members) != 50 || res.Next != "" {
		t.Fatalf("second page of user1: %d members, next %q, %v", len(res.Members), res.Next, err)
	}
}

func addInvites(repo *fakeRepo, by gocql.UUID, n int) {
	start := time.Now().Add(-time.Hour)
	for i := range n {
		hash := sha256.Sum256(fmt.Appendf(nil, "token %d", i))
		repo.invites = append(repo.invites, &models.Invite{
			TokenHash: hash[:], TeamID: repo.team, CreatedBy: by,
			// Pairs share a time, which the id orders.
			CreatedAt: start.Add(time.Duration(i/2) * time.Second), ExpiresAt: start.Add(48 * time.Hour),
		})
	}
}

func TestInvitesPage(t *testing.T) {
	repo := newFakeRepo()
	admin := repo.addMember("admin", models.TeamRoleAdmin)
	addInvites(repo, admin, 260)
	// Used invites are left out, without making a page short.
	for _, inv := range repo.invites[100:110] {
		repo.used[string(inv.TokenHash)] = true
	}
	w := newWorker(t, repo)
	var got []string
	after := ""
	pages := 0
	for {
		res, err := w.listInvites(context.Background(), ListInvites{P: principal(admin), TeamID: repo.team.String(), After: after})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if res.Next != "" && len(res.Invites) != invitePage {
			t.Fatalf("page %d holds %d invites", pages, len(res.Invites))
		}
		for _, s := range res.Invites {
			got = append(got, s.Id+"@"+s.CreatedAt.AsTime().Format(time.RFC3339))
		}
		if after = res.Next; after == "" {
			break
		}
	}
	if len(got) != 250 || pages != 5 {
		t.Fatalf("listed %d invites in %d pages", len(got), pages)
	}
	seen := map[string]bool{}
	for i, g := range got {
		if seen[g] {
			t.Fatalf("listed %s twice", g)
		}
		seen[g] = true
		if i > 0 && strings.Split(got[i-1], "@")[1] < strings.Split(g, "@")[1] {
			t.Fatal("not newest first")
		}
	}
	if _, err := w.listInvites(context.Background(), ListInvites{P: principal(admin), TeamID: repo.team.String(), After: "x"}); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
		t.Fatalf("a bad cursor: %v", err)
	}
}

func TestRevokeAnyInvite(t *testing.T) {
	repo := newFakeRepo()
	admin := repo.addMember("admin", models.TeamRoleAdmin)
	addInvites(repo, admin, 300)
	w := newWorker(t, repo)
	// The last by hash, which no window of 200 reached.
	last := slices.MaxFunc(repo.invites, func(a, b *models.Invite) int { return bytes.Compare(a.TokenHash, b.TokenHash) })
	id := secure.PublicID(last.TokenHash)
	if _, err := w.revokeInvite(context.Background(), RevokeInvite{P: principal(admin), TeamID: repo.team.String(), InviteID: id}); err != nil {
		t.Fatal(err)
	}
	if !repo.used[string(last.TokenHash)] {
		t.Fatal("not revoked")
	}
	if _, err := w.revokeInvite(context.Background(), RevokeInvite{P: principal(admin), TeamID: repo.team.String(), InviteID: id}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("revoking twice: %v", err)
	}
}

func TestTeamKeepsAnAdmin(t *testing.T) {
	repo := newFakeRepo()
	a := repo.addMember("a", models.TeamRoleAdmin)
	b := repo.addMember("b", models.TeamRoleAdmin)
	repo.owner = a
	repo.lag = 5 * time.Millisecond
	w := newWorker(t, repo)
	// Only the owner demotes another admin.
	if _, err := w.updateTeamMemberRole(context.Background(), UpdateTeamMemberRole{
		P: principal(b), TeamID: repo.team.String(), UserID: a.String(), Role: models.TeamRoleMember,
	}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("an admin demoted the owner: %v", err)
	}
	// Both step down at once: one must fail.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, pair := range [][2]gocql.UUID{{a, a}, {b, b}} {
		wg.Go(func() {
			_, errs[i] = w.updateTeamMemberRole(context.Background(), UpdateTeamMemberRole{
				P: principal(pair[0]), TeamID: repo.team.String(), UserID: pair[1].String(), Role: models.TeamRoleMember,
			})
		})
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("both or neither demotion went through: %v, %v", errs[0], errs[1])
	}
	admins := 0
	for _, e := range repo.members {
		if e.Role == models.TeamRoleAdmin {
			admins++
		}
	}
	if admins != 1 {
		t.Fatalf("%d admins left", admins)
	}
	// A system admin outside the team cannot remove its last admin either.
	sys := &models.Principal{UserID: ids.New(), Role: models.SystemRoleAdmin}
	var last gocql.UUID
	for id, e := range repo.members {
		if e.Role == models.TeamRoleAdmin {
			last = id
		}
	}
	if _, err := w.removeTeamMember(context.Background(), RemoveTeamMember{P: sys, TeamID: repo.team.String(), UserID: last.String()}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("removing the last admin: %v", err)
	}
}

func TestRemoveLeavesTheTeamFirst(t *testing.T) {
	repo := newFakeRepo()
	admin := repo.addMember("admin", models.TeamRoleAdmin)
	bob := repo.addMember("bob", models.TeamRoleMember)
	repo.channels[bob] = []gocql.UUID{ids.New(), ids.New()}
	w := newWorker(t, repo)
	if _, err := w.removeTeamMember(context.Background(), RemoveTeamMember{P: principal(admin), TeamID: repo.team.String(), UserID: bob.String()}); err != nil {
		t.Fatal(err)
	}
	want := []string{"remove team member", "list channels", "leave channel", "leave channel"}
	if !slices.Equal(repo.log, want) {
		t.Fatalf("writes %q, want %q", repo.log, want)
	}
	// A removal that stopped before the channels is finished by another.
	repo.channels[bob] = []gocql.UUID{ids.New()}
	repo.log = nil
	if _, err := w.removeTeamMember(context.Background(), RemoveTeamMember{P: principal(admin), TeamID: repo.team.String(), UserID: bob.String()}); err != nil {
		t.Fatal(err)
	}
	if len(repo.channels[bob]) != 0 {
		t.Fatal("the channels were left")
	}
	if _, err := w.removeTeamMember(context.Background(), RemoveTeamMember{P: principal(admin), TeamID: repo.team.String(), UserID: bob.String()}); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("removing a former member: %v", err)
	}
}

func TestARoleChangeReachesThePerson(t *testing.T) {
	repo := newFakeRepo()
	owner := repo.addMember("a", models.TeamRoleAdmin)
	bob := repo.addMember("b", models.TeamRoleMember)
	repo.owner = owner
	w := newWorker(t, repo)
	got := make(chan hub.Deliver, 4)
	w.Hub = w.Engine.SpawnFunc(func(c *actor.Context) {
		if d, ok := c.Message().(hub.Deliver); ok {
			got <- d
		}
	}, "deliveries")
	promote := func() {
		t.Helper()
		if _, err := w.updateTeamMemberRole(context.Background(), UpdateTeamMemberRole{
			P: principal(owner), TeamID: repo.team.String(), UserID: bob.String(), Role: models.TeamRoleAdmin,
		}); err != nil {
			t.Fatal(err)
		}
	}
	promote()
	select {
	case d := <-got:
		team := d.Event.GetTeamJoined().GetTeam()
		if !slices.Equal(d.UserIDs, []string{bob.String()}) || team.GetId() != repo.team.String() || team.GetRole() != pb.TeamRole_TEAM_ROLE_ADMIN {
			t.Fatalf("told %v of %v", d.UserIDs, team)
		}
	case <-time.After(time.Second):
		t.Fatal("bob was not told")
	}
	// The same role again changes nothing, and tells no one.
	promote()
	select {
	case d := <-got:
		t.Fatalf("told %v again", d.UserIDs)
	case <-time.After(100 * time.Millisecond):
	}
}
