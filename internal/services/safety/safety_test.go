package safety

import (
	"context"
	"errors"
	"slices"
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
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/accounts"
	"github.com/ssenerg/gochat/internal/services/blocks"
	"github.com/ssenerg/gochat/internal/services/channels"
	"github.com/ssenerg/gochat/internal/services/directory"
	"github.com/ssenerg/gochat/internal/services/hub"
	"github.com/ssenerg/gochat/internal/services/updates"
	"github.com/ssenerg/gochat/internal/services/users"
)

// fakeRepo keeps people, teams, blocks and reports in memory.
type fakeRepo struct {
	mu      sync.Mutex
	users   map[gocql.UUID]*models.User
	teams   map[gocql.UUID]map[gocql.UUID]models.TeamRole
	blocks  map[gocql.UUID][]*models.Block
	reports map[gocql.UUID]*models.Report
	// listed holds the reports listed open; failIndex fails moving them.
	listed       map[gocql.UUID]bool
	failIndex    bool
	state        map[string]string
	blockerReads int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		users: map[gocql.UUID]*models.User{}, teams: map[gocql.UUID]map[gocql.UUID]models.TeamRole{},
		blocks: map[gocql.UUID][]*models.Block{}, reports: map[gocql.UUID]*models.Report{},
		listed: map[gocql.UUID]bool{}, state: map[string]string{},
	}
}

func (f *fakeRepo) person(name string, role models.SystemRole) gocql.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := ids.New()
	f.users[id] = &models.User{ID: id, Username: name, Role: role}
	return id
}

func (f *fakeRepo) join(team, user gocql.UUID, role models.TeamRole) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.teams[team] == nil {
		f.teams[team] = map[gocql.UUID]models.TeamRole{}
	}
	f.teams[team][user] = role
}

func (f *fakeRepo) GetUser(_ context.Context, id gocql.UUID) (*models.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := f.users[id]; u != nil {
		c := *u
		return &c, nil
	}
	return nil, ierrors.NotFound("not found")
}

func (f *fakeRepo) GetUsers(ctx context.Context, list []gocql.UUID) ([]*models.User, error) {
	var out []*models.User
	for _, id := range list {
		if u, err := f.GetUser(ctx, id); err == nil {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *fakeRepo) Block(_ context.Context, b *models.Block) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := slices.DeleteFunc(f.blocks[b.UserID], func(old *models.Block) bool { return old.BlockedID == b.BlockedID })
	f.blocks[b.UserID] = append(list, b)
	return nil
}

func (f *fakeRepo) Unblock(_ context.Context, userID, blockedID gocql.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocks[userID] = slices.DeleteFunc(f.blocks[userID], func(b *models.Block) bool { return b.BlockedID == blockedID })
	return nil
}

func (f *fakeRepo) ListBlocks(_ context.Context, userID gocql.UUID, limit int) ([]*models.Block, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.blocks[userID]
	return slices.Clone(list[:min(limit, len(list))]), nil
}

func (f *fakeRepo) ListBlockers(_ context.Context, userID gocql.UUID, limit int) ([]gocql.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockerReads++
	var out []gocql.UUID
	for by, list := range f.blocks {
		if slices.ContainsFunc(list, func(b *models.Block) bool { return b.BlockedID == userID }) && len(out) < limit {
			out = append(out, by)
		}
	}
	return out, nil
}

func (f *fakeRepo) CopyBlocks(context.Context) (int, error) { return 0, nil }

func (f *fakeRepo) GetState(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state[key], nil
}

func (f *fakeRepo) SetState(_ context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state[key] = value
	return nil
}

func (f *fakeRepo) CreateReport(_ context.Context, rep *models.Report) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *rep
	f.reports[rep.ID] = &c
	f.listed[rep.ID] = true
	return nil
}

func (f *fakeRepo) GetReport(_ context.Context, id gocql.UUID) (*models.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rep := f.reports[id]; rep != nil {
		c := *rep
		return &c, nil
	}
	return nil, ierrors.NotFound("not found")
}

func (f *fakeRepo) scoped(scope gocql.UUID, open bool) []*models.Report {
	var out []*models.Report
	for _, rep := range f.reports {
		if (ids.IsZero(scope) || rep.TeamID == scope) && f.listed[rep.ID] == open {
			c := *rep
			out = append(out, &c)
		}
	}
	slices.SortFunc(out, func(a, b *models.Report) int { return b.ID.Time().Compare(a.ID.Time()) })
	return out
}

func (f *fakeRepo) ListReports(_ context.Context, scope gocql.UUID, open bool, after gocql.UUID, limit int) ([]*models.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.scoped(scope, open)
	if !ids.IsZero(after) {
		i := slices.IndexFunc(list, func(r *models.Report) bool { return r.ID == after })
		list = list[i+1:]
	}
	return list[:min(limit, len(list))], nil
}

func (f *fakeRepo) CountOpenReports(_ context.Context, scope gocql.UUID, limit int) (int, map[gocql.UUID]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list := f.scoped(scope, true)
	list = list[:min(limit, len(list))]
	about := map[gocql.UUID]int{}
	for _, rep := range list {
		about[rep.UserID]++
	}
	return len(list), about, nil
}

// holds reports whether held is as rep was read.
func holds(held, rep *models.Report) bool {
	return held.Action == rep.Action && held.ResolvedBy == rep.ResolvedBy && held.ResolvedAt.Equal(rep.ResolvedAt)
}

func (f *fakeRepo) ClaimReport(_ context.Context, rep *models.Report, by gocql.UUID, at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	held := f.reports[rep.ID]
	if !holds(held, rep) || !held.Open() {
		return false, nil
	}
	held.Action, held.ResolvedBy, held.ResolvedAt = models.ReportPending, by, at
	return true, nil
}

func (f *fakeRepo) FinishReport(ctx context.Context, rep *models.Report, action string) (bool, error) {
	f.mu.Lock()
	held := f.reports[rep.ID]
	if !holds(held, rep) || held.Action != models.ReportPending {
		f.mu.Unlock()
		return false, nil
	}
	held.Action = action
	f.mu.Unlock()
	return true, f.IndexResolved(ctx, rep)
}

func (f *fakeRepo) ReleaseReport(_ context.Context, rep *models.Report) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if held := f.reports[rep.ID]; holds(held, rep) && held.Action == models.ReportPending {
		held.Action, held.ResolvedBy, held.ResolvedAt = "", gocql.UUID{}, time.Time{}
	}
	return nil
}

func (f *fakeRepo) IndexResolved(_ context.Context, rep *models.Report) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failIndex {
		return ierrors.Internal(errors.New("index unavailable"))
	}
	f.listed[rep.ID] = false
	return nil
}

func (f *fakeRepo) GetTeamMember(_ context.Context, teamID, userID gocql.UUID) (*models.TeamMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if role, ok := f.teams[teamID][userID]; ok {
		return &models.TeamMember{TeamID: teamID, UserID: userID, Role: role}, nil
	}
	return nil, ierrors.NotFound("not a member")
}

func (f *fakeRepo) ListTeamMembers(_ context.Context, teamID gocql.UUID) ([]*scylla.TeamMemberEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*scylla.TeamMemberEntry
	for id, role := range f.teams[teamID] {
		out = append(out, &scylla.TeamMemberEntry{TeamMember: models.TeamMember{TeamID: teamID, UserID: id, Role: role}})
	}
	return out, nil
}

func (f *fakeRepo) ListUserTeams(_ context.Context, userID gocql.UUID) ([]*models.TeamMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*models.TeamMember
	for team, people := range f.teams {
		if role, ok := people[userID]; ok {
			out = append(out, &models.TeamMember{TeamID: team, UserID: userID, Role: role})
		}
	}
	return out, nil
}

func (f *fakeRepo) GetTeams(_ context.Context, list []gocql.UUID) ([]*models.Team, error) {
	out := make([]*models.Team, len(list))
	for i, id := range list {
		out[i] = &models.Team{ID: id, DisplayName: "Acme"}
	}
	return out, nil
}

func (f *fakeRepo) GetChannels(_ context.Context, list []gocql.UUID) ([]*models.Channel, error) {
	out := make([]*models.Channel, len(list))
	for i, id := range list {
		out[i] = &models.Channel{ID: id, DisplayName: "General", Kind: models.ChannelPublic}
	}
	return out, nil
}

// world stands in for the actors the safety service asks and tells.
type world struct {
	mu sync.Mutex
	// messages are those channels.GetMessage finds, with the team of
	// their channel.
	messages map[string]*pb.Message
	teamOf   map[gocql.UUID]gocql.UUID
	deleted  []channels.Delete
	removed  []directory.RemoveTeamMember
	deact    []accounts.SetDeactivated
	pushed   []updates.Push
	toTeam   []hub.Deliver
	toAdmins []hub.ToAdmins
	// failRemove fails removing people from teams.
	failRemove bool
}

func (wd *world) take() (team []hub.Deliver, admins []hub.ToAdmins) {
	wd.mu.Lock()
	defer wd.mu.Unlock()
	team, admins = wd.toTeam, wd.toAdmins
	wd.toTeam, wd.toAdmins = nil, nil
	return team, admins
}

type fixture struct {
	t     *testing.T
	repo  *fakeRepo
	world *world
	w     *worker
}

const policy = `p, system_admin, *, *
p, system_user, report, create
p, team_admin, report, read
p, team_admin, report, resolve`

func newFixture(t *testing.T) *fixture {
	t.Helper()
	az, err := authz.New(zap.NewNop(), stringadapter.NewAdapter(policy))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	repo := newFakeRepo()
	wd := &world{messages: map[string]*pb.Message{}, teamOf: map[gocql.UUID]gocql.UUID{}}
	hubPID := engine.SpawnFunc(func(c *actor.Context) {
		wd.mu.Lock()
		defer wd.mu.Unlock()
		switch msg := c.Message().(type) {
		case hub.Deliver:
			wd.toTeam = append(wd.toTeam, msg)
		case hub.ToAdmins:
			wd.toAdmins = append(wd.toAdmins, msg)
		}
	}, "hub")
	updatesPID := engine.SpawnFunc(func(c *actor.Context) {
		if msg, ok := c.Message().(updates.Push); ok {
			wd.mu.Lock()
			wd.pushed = append(wd.pushed, msg)
			wd.mu.Unlock()
		}
	}, "updates")
	chans := engine.SpawnFunc(func(c *actor.Context) {
		wd.mu.Lock()
		defer wd.mu.Unlock()
		switch msg := c.Message().(type) {
		case channels.GetMessage:
			if m := wd.messages[msg.MessageID]; m != nil && m.ChannelId == msg.ChannelID.String() {
				actors.Reply(c, m)
				return
			}
			actors.Reply(c, ierrors.NotFound("message not found"))
		case channels.Access:
			actors.Reply(c, &channels.AccessResult{Channel: &models.Channel{ID: msg.ChannelID, TeamID: wd.teamOf[msg.ChannelID]}})
		case channels.Delete:
			wd.deleted = append(wd.deleted, msg)
			actors.Reply(c, channels.Done{})
		}
	}, "channels")
	dir := engine.SpawnFunc(func(c *actor.Context) {
		if msg, ok := c.Message().(directory.RemoveTeamMember); ok {
			wd.mu.Lock()
			defer wd.mu.Unlock()
			if wd.failRemove {
				actors.Reply(c, ierrors.Unavailable("the team is busy, try again"))
				return
			}
			wd.removed = append(wd.removed, msg)
			actors.Reply(c, directory.Done{})
		}
	}, "directory")
	accts := engine.SpawnFunc(func(c *actor.Context) {
		if msg, ok := c.Message().(accounts.SetDeactivated); ok {
			wd.mu.Lock()
			wd.deact = append(wd.deact, msg)
			wd.mu.Unlock()
			actors.Reply(c, &models.User{})
		}
	}, "accounts")
	d := &Deps{
		Logger: zap.NewNop(), Cfg: &Config{Workers: 1, OpTimeout: time.Second, MaxBlocks: 3, MaxReports: 3, ReportWindow: time.Hour},
		Repo: repo, Authz: az, Users: users.NewCache(repo, 100, time.Nanosecond), Blocks: blocks.NewCache(repo, 3, 100, time.Minute),
		Engine: engine, Hub: hubPID, Updates: updatesPID, Channels: chans, Directory: dir, Accounts: accts,
	}
	Pool(d)
	return &fixture{t: t, repo: repo, world: wd, w: &worker{Deps: d}}
}

func (f *fixture) principal(id gocql.UUID) *models.Principal {
	f.repo.mu.Lock()
	defer f.repo.mu.Unlock()
	return &models.Principal{UserID: id, Role: f.repo.users[id].Role}
}

// settle waits for the stand-ins to take what was sent them.
func settle() { time.Sleep(50 * time.Millisecond) }

func TestBlocking(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	me := f.repo.person("me", models.SystemRoleUser)
	bob, cat, dan, eve := f.repo.person("bob", models.SystemRoleUser), f.repo.person("cat", models.SystemRoleUser),
		f.repo.person("dan", models.SystemRoleUser), f.repo.person("eve", models.SystemRoleUser)
	block := func(id gocql.UUID, blocked bool) error {
		_, err := f.w.block(ctx, Block{P: f.principal(me), UserID: id.String(), Blocked: blocked})
		return err
	}
	if err := block(me, true); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
		t.Fatalf("blocked myself: %v", err)
	}
	if err := block(ids.New(), true); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("blocked no one: %v", err)
	}
	for _, id := range []gocql.UUID{bob, cat, dan} {
		if err := block(id, true); err != nil {
			t.Fatal(err)
		}
	}
	if blocked, _ := f.w.Blocks.Blocked(ctx, me, bob); !blocked {
		t.Fatal("the cache missed a block")
	}
	// Blocking again changes nothing; past the cap is refused.
	if err := block(bob, true); err != nil {
		t.Fatalf("blocking again: %v", err)
	}
	if err := block(eve, true); !ierrors.Is(err, ierrors.CodeResourceExhausted) {
		t.Fatalf("blocked past the cap: %v", err)
	}
	if err := block(bob, false); err != nil {
		t.Fatal(err)
	}
	if blocked, _ := f.w.Blocks.Blocked(ctx, me, bob); blocked {
		t.Fatal("the cache kept an unblocked person")
	}
	list, err := f.w.listBlocked(ctx, f.principal(me))
	if err != nil || len(list) != 2 {
		t.Fatalf("listed %d blocked, %v", len(list), err)
	}
	// Only the blocker hears of it, in their numbered updates.
	settle()
	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	if len(f.world.pushed) != 5 {
		t.Fatalf("%d updates, want 5: three blocks, one again and an unblock", len(f.world.pushed))
	}
	for _, p := range f.world.pushed {
		if len(p.UserIDs) != 1 || p.UserIDs[0] != me || p.Event.GetBlocksChanged() == nil {
			t.Fatalf("an update went to %v: %v", p.UserIDs, p.Event)
		}
	}
	if last := f.world.pushed[4].Event.GetBlocksChanged(); last.Blocked || last.GetUser().GetId() != bob.String() {
		t.Fatalf("the unblock said %v", last)
	}
}

func TestBlockingPastAStaleCache(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	me, bob, cat, dan := f.repo.person("me", models.SystemRoleUser), f.repo.person("bob", models.SystemRoleUser),
		f.repo.person("cat", models.SystemRoleUser), f.repo.person("dan", models.SystemRoleUser)
	block := func(id gocql.UUID, blocked bool) error {
		_, err := f.w.block(ctx, Block{P: f.principal(me), UserID: id.String(), Blocked: blocked})
		return err
	}
	if err := block(bob, true); err != nil {
		t.Fatal(err)
	}
	// The cache holds one block while three were made, two of them
	// elsewhere.
	if _, err := f.w.Blocks.Of(ctx, me); err != nil {
		t.Fatal(err)
	}
	for _, id := range []gocql.UUID{cat, dan} {
		if err := f.repo.Block(ctx, &models.Block{UserID: me, BlockedID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := block(f.repo.person("eve", models.SystemRoleUser), true); !ierrors.Is(err, ierrors.CodeResourceExhausted) {
		t.Fatalf("blocked past the cap: %v", err)
	}
	// Unblocking someone the cache never saw blocked still unblocks them.
	if err := block(cat, false); err != nil {
		t.Fatal(err)
	}
	if list, _ := f.repo.ListBlocks(ctx, me, 10); len(list) != 2 || slices.ContainsFunc(list, func(b *models.Block) bool { return b.BlockedID == cat }) {
		t.Fatalf("after unblocking cat: %v", list)
	}
}

func TestReporting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	team, channel := ids.New(), ids.New()
	ann, bob, boss := f.repo.person("ann", models.SystemRoleUser), f.repo.person("bob", models.SystemRoleUser), f.repo.person("boss", models.SystemRoleUser)
	root := f.repo.person("root", models.SystemRoleAdmin)
	f.repo.join(team, ann, models.TeamRoleMember)
	f.repo.join(team, bob, models.TeamRoleMember)
	f.repo.join(team, boss, models.TeamRoleAdmin)
	f.world.teamOf[channel] = team
	mine := &pb.Message{Id: ids.NewTime().String(), ChannelId: channel.String(), Content: "mine", Author: &pb.User{Id: ann.String()}}
	rude := &pb.Message{Id: ids.NewTime().String(), ChannelId: channel.String(), Content: "rude words", Author: &pb.User{Id: bob.String()}}
	f.world.messages[mine.Id], f.world.messages[rude.Id] = mine, rude
	report := func(p gocql.UUID, req *pb.ReportRequest) error {
		_, err := f.w.report(ctx, Report{P: f.principal(p), Req: req})
		return err
	}
	ofMessage := func(m *pb.Message, reason pb.ReportReason, details string) *pb.ReportRequest {
		return &pb.ReportRequest{Reason: reason, Details: details, Target: &pb.ReportRequest_Message{Message: &pb.ReportedMessage{ChannelId: m.ChannelId, MessageId: m.Id}}}
	}

	for name, c := range map[string]struct {
		req  *pb.ReportRequest
		code ierrors.Code
	}{
		"no reason":          {ofMessage(rude, pb.ReportReason_REPORT_REASON_UNSPECIFIED, ""), ierrors.CodeInvalidArgument},
		"other without text": {ofMessage(rude, pb.ReportReason_REPORT_REASON_OTHER, " "), ierrors.CodeInvalidArgument},
		"own message":        {ofMessage(mine, pb.ReportReason_REPORT_REASON_SPAM, ""), ierrors.CodeInvalidArgument},
		"unread message":     {ofMessage(&pb.Message{Id: ids.NewTime().String(), ChannelId: channel.String()}, pb.ReportReason_REPORT_REASON_SPAM, ""), ierrors.CodeNotFound},
		"no target":          {&pb.ReportRequest{Reason: pb.ReportReason_REPORT_REASON_SPAM}, ierrors.CodeInvalidArgument},
		"myself": {&pb.ReportRequest{Reason: pb.ReportReason_REPORT_REASON_SPAM, Target: &pb.ReportRequest_User{User: &pb.ReportedUser{
			TeamId: team.String(), UserId: ann.String(),
		}}}, ierrors.CodeInvalidArgument},
		"someone of another team": {&pb.ReportRequest{Reason: pb.ReportReason_REPORT_REASON_SPAM, Target: &pb.ReportRequest_User{User: &pb.ReportedUser{
			TeamId: team.String(), UserId: root.String(),
		}}}, ierrors.CodeNotFound},
	} {
		if err := report(ann, c.req); !ierrors.Is(err, c.code) {
			t.Fatalf("%s: %v, want code %d", name, err, c.code)
		}
	}

	if err := report(ann, ofMessage(rude, pb.ReportReason_REPORT_REASON_HARASSMENT, "again")); err != nil {
		t.Fatal(err)
	}
	if err := report(ann, &pb.ReportRequest{Reason: pb.ReportReason_REPORT_REASON_SPAM, Target: &pb.ReportRequest_User{User: &pb.ReportedUser{
		TeamId: team.String(), UserId: bob.String(),
	}}}); err != nil {
		t.Fatal(err)
	}
	// The team's admins, and system admins, hear how many wait.
	settle()
	toTeam, toAdmins := f.world.take()
	if len(toTeam) != 2 || !slices.Equal(toTeam[1].UserIDs, []string{boss.String()}) || toTeam[1].Event.GetReportsChanged().GetCount() != 2 ||
		toTeam[1].Event.GetReportsChanged().GetTeamId() != team.String() {
		t.Fatalf("the team's admins heard %v", toTeam)
	}
	if len(toAdmins) != 2 || toAdmins[1].Event.GetReportsChanged().GetCount() != 2 || toAdmins[1].Event.GetReportsChanged().GetTeamId() != "" {
		t.Fatalf("system admins heard %v", toAdmins)
	}

	// The third report in the window is the last.
	if err := report(ann, ofMessage(rude, pb.ReportReason_REPORT_REASON_SPAM, "")); err != nil {
		t.Fatal(err)
	}
	if err := report(ann, ofMessage(rude, pb.ReportReason_REPORT_REASON_SPAM, "")); !ierrors.Is(err, ierrors.CodeResourceExhausted) {
		t.Fatalf("reported past the limit: %v", err)
	}

	// A member reads none; the team's admin reads the team's, with the
	// message as it read.
	if _, err := f.w.listReports(ctx, ListReports{P: f.principal(bob), TeamID: team.String()}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("a member listed reports: %v", err)
	}
	if _, err := f.w.listReports(ctx, ListReports{P: f.principal(boss)}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("a team admin listed every team's reports: %v", err)
	}
	res, err := f.w.listReports(ctx, ListReports{P: f.principal(boss), TeamID: team.String()})
	if err != nil || len(res.Reports) != 3 {
		t.Fatalf("the admin listed %v, %v", res, err)
	}
	var ofRude *pb.Report
	for _, r := range res.Reports {
		if r.Reason == pb.ReportReason_REPORT_REASON_HARASSMENT {
			ofRude = r
		}
	}
	if ofRude == nil || ofRude.Excerpt != "rude words" || ofRude.Details != "again" || ofRude.GetUser().GetId() != bob.String() ||
		ofRude.GetReporter().GetId() != ann.String() || ofRude.ChannelName != "General" || ofRude.TeamName != "Acme" {
		t.Fatalf("the report reads %v", ofRude)
	}
	counts, err := f.w.countReports(ctx, f.principal(boss))
	if err != nil || counts.Teams[team.String()] != 3 || counts.All != 0 {
		t.Fatalf("the admin's counts %v, %v", counts, err)
	}
	counts, err = f.w.countReports(ctx, f.principal(root))
	if err != nil || len(counts.Teams) != 0 || counts.All != 3 {
		t.Fatalf("the system admin's counts %v, %v", counts, err)
	}
}

func TestResolvingReports(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	team, channel := ids.New(), ids.New()
	ann, bob, boss := f.repo.person("ann", models.SystemRoleUser), f.repo.person("bob", models.SystemRoleUser), f.repo.person("boss", models.SystemRoleUser)
	root := f.repo.person("root", models.SystemRoleAdmin)
	f.repo.join(team, ann, models.TeamRoleMember)
	f.repo.join(team, bob, models.TeamRoleMember)
	f.repo.join(team, boss, models.TeamRoleAdmin)
	f.world.teamOf[channel] = team
	file := func(teamID gocql.UUID) gocql.UUID {
		rep := &models.Report{ID: ids.NewTime(), TeamID: teamID, ReporterID: ann, UserID: bob, ChannelID: channel, MessageID: ids.NewTime(), Reason: models.ReportSpam}
		if err := f.repo.CreateReport(ctx, rep); err != nil {
			t.Fatal(err)
		}
		return rep.ID
	}
	resolve := func(p gocql.UUID, id gocql.UUID, action pb.ReportAction) (*pb.Report, error) {
		return f.w.resolve(ctx, Resolve{P: f.principal(p), ReportID: id.String(), Action: action})
	}

	r1 := file(team)
	if _, err := resolve(bob, r1, pb.ReportAction_REPORT_ACTION_DISMISS); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("a member acted on a report: %v", err)
	}
	if _, err := resolve(boss, r1, pb.ReportAction_REPORT_ACTION_DEACTIVATE); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("a team admin deactivated an account: %v", err)
	}
	got, err := resolve(boss, r1, pb.ReportAction_REPORT_ACTION_DELETE_MESSAGE)
	if err != nil || got.Action != pb.ReportAction_REPORT_ACTION_DELETE_MESSAGE || got.GetResolvedBy().GetId() != boss.String() || got.ResolvedAt == nil {
		t.Fatalf("deleting the message: %v, %v", got, err)
	}
	if _, err := resolve(boss, r1, pb.ReportAction_REPORT_ACTION_DISMISS); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("acted twice on a report: %v", err)
	}

	r2 := file(team)
	if _, err := resolve(boss, r2, pb.ReportAction_REPORT_ACTION_REMOVE_MEMBER); err != nil {
		t.Fatal(err)
	}
	// A conversation of no team is for system admins alone.
	r3 := file(gocql.UUID{})
	if _, err := resolve(boss, r3, pb.ReportAction_REPORT_ACTION_DISMISS); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("a team admin acted on a report of no team: %v", err)
	}
	if _, err := resolve(root, r3, pb.ReportAction_REPORT_ACTION_REMOVE_MEMBER); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
		t.Fatalf("removed someone from no team: %v", err)
	}
	if _, err := resolve(root, r3, pb.ReportAction_REPORT_ACTION_DEACTIVATE); err != nil {
		t.Fatal(err)
	}

	f.world.mu.Lock()
	defer f.world.mu.Unlock()
	if len(f.world.deleted) != 1 || !f.world.deleted[0].Moderated || f.world.deleted[0].ChannelID != channel || f.world.deleted[0].P.UserID != boss {
		t.Fatalf("deleted %+v", f.world.deleted)
	}
	if len(f.world.removed) != 1 || f.world.removed[0].UserID != bob.String() || f.world.removed[0].TeamID != team.String() || f.world.removed[0].P.UserID != boss {
		t.Fatalf("removed %+v", f.world.removed)
	}
	if len(f.world.deact) != 1 || f.world.deact[0].UserID != bob.String() || !f.world.deact[0].Deactivated || f.world.deact[0].P.UserID != root {
		t.Fatalf("deactivated %+v", f.world.deact)
	}
	f.repo.mu.Lock()
	defer f.repo.mu.Unlock()
	for id, want := range map[gocql.UUID]string{r1: models.ReportDeleteMessage, r2: models.ReportRemoveMember, r3: models.ReportDeactivate} {
		if rep := f.repo.reports[id]; rep.Action != want {
			t.Fatalf("report %s: %q, want %q", id, rep.Action, want)
		}
	}
}

func TestReportsAboutAnAdmin(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	team, channel := ids.New(), ids.New()
	ann, boss, deputy := f.repo.person("ann", models.SystemRoleUser), f.repo.person("boss", models.SystemRoleUser), f.repo.person("deputy", models.SystemRoleUser)
	root, chief := f.repo.person("root", models.SystemRoleAdmin), f.repo.person("chief", models.SystemRoleAdmin)
	f.repo.join(team, ann, models.TeamRoleMember)
	f.repo.join(team, boss, models.TeamRoleAdmin)
	f.repo.join(team, deputy, models.TeamRoleAdmin)
	f.repo.join(team, root, models.TeamRoleMember)
	f.world.teamOf[channel] = team
	file := func(reporter, about gocql.UUID) gocql.UUID {
		rep := &models.Report{ID: ids.NewTime(), TeamID: team, ReporterID: reporter, UserID: about, ChannelID: channel, MessageID: ids.NewTime(), Reason: models.ReportSpam}
		if err := f.repo.CreateReport(ctx, rep); err != nil {
			t.Fatal(err)
		}
		return rep.ID
	}
	ofBoss, ofRoot, byBoss := file(ann, boss), file(ann, root), file(boss, ann)
	listed := func(p gocql.UUID, teamID string) []string {
		t.Helper()
		res, err := f.w.listReports(ctx, ListReports{P: f.principal(p), TeamID: teamID})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range res.Reports {
			out = append(out, r.Id)
		}
		slices.Sort(out)
		return out
	}
	sorted := func(list ...gocql.UUID) []string {
		out := make([]string, len(list))
		for i, id := range list {
			out[i] = id.String()
		}
		slices.Sort(out)
		return out
	}

	// No one reads or counts the reports about themselves; the others do.
	if got := listed(boss, team.String()); !slices.Equal(got, sorted(ofRoot, byBoss)) {
		t.Fatalf("boss read %v", got)
	}
	if got := listed(deputy, team.String()); !slices.Equal(got, sorted(ofBoss, ofRoot, byBoss)) {
		t.Fatalf("deputy read %v", got)
	}
	if got := listed(root, ""); !slices.Equal(got, sorted(ofBoss, byBoss)) {
		t.Fatalf("root read %v", got)
	}
	if got := listed(chief, ""); len(got) != 3 {
		t.Fatalf("chief read %v", got)
	}
	if c, _ := f.w.countReports(ctx, f.principal(boss)); c.Teams[team.String()] != 2 {
		t.Fatalf("boss counts %v", c)
	}
	if c, _ := f.w.countReports(ctx, f.principal(root)); c.All != 2 {
		t.Fatalf("root counts %v", c)
	}

	// Nor act on them, or on those they sent.
	resolve := func(p, id gocql.UUID) error {
		_, err := f.w.resolve(ctx, Resolve{P: f.principal(p), ReportID: id.String(), Action: pb.ReportAction_REPORT_ACTION_DISMISS})
		return err
	}
	if err := resolve(boss, ofBoss); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("boss acted on a report about themselves: %v", err)
	}
	if err := resolve(root, ofRoot); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("root acted on a report about themselves: %v", err)
	}
	if err := resolve(boss, byBoss); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("boss acted on their own report: %v", err)
	}

	// What each admin hears leaves out the reports about them.
	settle()
	f.world.take()
	if err := resolve(deputy, byBoss); err != nil {
		t.Fatal(err)
	}
	settle()
	toTeam, toAdmins := f.world.take()
	heard := map[string]int32{}
	for _, d := range toTeam {
		for _, id := range d.UserIDs {
			heard[id] = d.Event.GetReportsChanged().GetCount()
		}
	}
	if heard[boss.String()] != 1 || heard[deputy.String()] != 2 || len(heard) != 2 {
		t.Fatalf("the team's admins heard %v", heard)
	}
	if len(toAdmins) != 1 || toAdmins[0].Event.GetReportsChanged().GetCount() != 2 ||
		toAdmins[0].Instead[root.String()].GetReportsChanged().GetCount() != 1 {
		t.Fatalf("system admins heard %+v", toAdmins)
	}
}

func TestClaimingReports(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	team, channel := ids.New(), ids.New()
	ann, bob, boss, deputy := f.repo.person("ann", models.SystemRoleUser), f.repo.person("bob", models.SystemRoleUser),
		f.repo.person("boss", models.SystemRoleUser), f.repo.person("deputy", models.SystemRoleUser)
	f.repo.join(team, ann, models.TeamRoleMember)
	f.repo.join(team, bob, models.TeamRoleMember)
	f.repo.join(team, boss, models.TeamRoleAdmin)
	f.repo.join(team, deputy, models.TeamRoleAdmin)
	f.world.teamOf[channel] = team
	file := func() gocql.UUID {
		rep := &models.Report{ID: ids.NewTime(), TeamID: team, ReporterID: ann, UserID: bob, ChannelID: channel, MessageID: ids.NewTime(), Reason: models.ReportSpam}
		if err := f.repo.CreateReport(ctx, rep); err != nil {
			t.Fatal(err)
		}
		return rep.ID
	}
	resolve := func(p, id gocql.UUID, action pb.ReportAction) error {
		_, err := f.w.resolve(ctx, Resolve{P: f.principal(p), ReportID: id.String(), Action: action})
		return err
	}
	report := func(id gocql.UUID) models.Report {
		f.repo.mu.Lock()
		defer f.repo.mu.Unlock()
		return *f.repo.reports[id]
	}

	// An act that fails leaves the report open, for anyone to act on.
	r1 := file()
	f.world.mu.Lock()
	f.world.failRemove = true
	f.world.mu.Unlock()
	if err := resolve(boss, r1, pb.ReportAction_REPORT_ACTION_REMOVE_MEMBER); !ierrors.Is(err, ierrors.CodeUnavailable) {
		t.Fatalf("a failed removal: %v", err)
	}
	if rep := report(r1); rep.Action != "" || !ids.IsZero(rep.ResolvedBy) {
		t.Fatalf("after a failed act the report reads %+v", rep)
	}
	f.world.mu.Lock()
	f.world.failRemove = false
	f.world.mu.Unlock()

	// While one admin acts, the others wait; a claim gone stale is taken
	// over.
	f.repo.mu.Lock()
	held := f.repo.reports[r1]
	held.Action, held.ResolvedBy, held.ResolvedAt = models.ReportPending, deputy, time.Now().UTC().Truncate(time.Millisecond)
	f.repo.mu.Unlock()
	if err := resolve(boss, r1, pb.ReportAction_REPORT_ACTION_DISMISS); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("acted on a report claimed: %v", err)
	}
	res, err := f.w.listReports(ctx, ListReports{P: f.principal(boss), TeamID: team.String()})
	if err != nil || len(res.Reports) != 1 || res.Reports[0].Action != pb.ReportAction_REPORT_ACTION_UNSPECIFIED {
		t.Fatalf("a claimed report lists as %v, %v", res, err)
	}
	f.repo.mu.Lock()
	held.ResolvedAt = held.ResolvedAt.Add(-time.Hour)
	f.repo.mu.Unlock()
	if err := resolve(boss, r1, pb.ReportAction_REPORT_ACTION_DISMISS); err != nil {
		t.Fatalf("taking over a stale claim: %v", err)
	}
	if rep := report(r1); rep.Action != models.ReportDismissed || rep.ResolvedBy != boss {
		t.Fatalf("after taking over: %+v", rep)
	}

	// A resolution whose listing failed to move shows in neither list
	// until the open one repairs it.
	r2 := file()
	f.repo.mu.Lock()
	f.repo.failIndex = true
	f.repo.mu.Unlock()
	if err := resolve(boss, r2, pb.ReportAction_REPORT_ACTION_DISMISS); err != nil {
		t.Fatalf("resolving while the index fails: %v", err)
	}
	f.repo.mu.Lock()
	f.repo.failIndex = false
	f.repo.mu.Unlock()
	if res, _ := f.w.listReports(ctx, ListReports{P: f.principal(boss), TeamID: team.String()}); len(res.Reports) != 0 {
		t.Fatalf("open: %v", res.Reports)
	}
	res, err = f.w.listReports(ctx, ListReports{P: f.principal(boss), TeamID: team.String(), Resolved: true})
	if err != nil || len(res.Reports) != 2 {
		t.Fatalf("resolved after the repair: %v, %v", res, err)
	}

	// Report ids are timeuuids.
	if err := resolve(boss, ids.New(), pb.ReportAction_REPORT_ACTION_DISMISS); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
		t.Fatalf("a random id: %v", err)
	}
	if _, err := f.w.listReports(ctx, ListReports{P: f.principal(boss), TeamID: team.String(), After: ids.New().String()}); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
		t.Fatalf("a random cursor: %v", err)
	}
}

func TestCopyingBlocksOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := &copier{Deps: f.w.Deps}
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	c.run(run)
	if got, _ := f.repo.GetState(ctx, copyKey); got != copyDone {
		t.Fatalf("the copy's state reads %q", got)
	}
	// The cache now reads who blocked someone from blocked_by.
	if _, err := f.w.Blocks.Among(ctx, ids.New(), []gocql.UUID{ids.New()}); err != nil {
		t.Fatal(err)
	}
	f.repo.mu.Lock()
	defer f.repo.mu.Unlock()
	if f.repo.blockerReads != 1 {
		t.Fatalf("read blockers %d times, want once", f.repo.blockerReads)
	}
}
