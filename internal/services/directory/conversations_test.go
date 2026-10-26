package directory

import (
	"context"
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
	"github.com/ssenerg/gochat/internal/services/channels"
	"github.com/ssenerg/gochat/internal/services/users"
)

// teamsRepo holds several teams and the conversations opened in them.
type teamsRepo struct {
	Repo
	mu       sync.Mutex
	users    map[gocql.UUID]*models.User
	teams    map[gocql.UUID]map[gocql.UUID]time.Time
	keys     map[string]gocql.UUID
	channels map[gocql.UUID]*models.Channel
	members  map[gocql.UUID][]*models.ChannelMember
	cleared  map[gocql.UUID]map[gocql.UUID]gocql.UUID
}

func newTeamsRepo() *teamsRepo {
	return &teamsRepo{
		users: map[gocql.UUID]*models.User{}, teams: map[gocql.UUID]map[gocql.UUID]time.Time{},
		keys: map[string]gocql.UUID{}, channels: map[gocql.UUID]*models.Channel{}, members: map[gocql.UUID][]*models.ChannelMember{},
	}
}

func (f *teamsRepo) person(name string) gocql.UUID {
	id := ids.New()
	f.users[id] = &models.User{ID: id, Username: name, Role: models.SystemRoleUser}
	return id
}

// join puts people in a team, as of when.
func (f *teamsRepo) join(team gocql.UUID, when time.Time, people ...gocql.UUID) {
	if f.teams[team] == nil {
		f.teams[team] = map[gocql.UUID]time.Time{}
	}
	for _, id := range people {
		f.teams[team][id] = when
	}
}

func (f *teamsRepo) GetUser(_ context.Context, id gocql.UUID) (*models.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := f.users[id]; u != nil {
		return u, nil
	}
	return nil, ierrors.NotFound("user not found")
}

func (f *teamsRepo) GetUsers(_ context.Context, list []gocql.UUID) ([]*models.User, error) {
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

func (f *teamsRepo) GetTeamMember(_ context.Context, teamID, userID gocql.UUID) (*models.TeamMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if at, ok := f.teams[teamID][userID]; ok {
		return &models.TeamMember{TeamID: teamID, UserID: userID, Role: models.TeamRoleMember, JoinedAt: at}, nil
	}
	return nil, ierrors.NotFound("not a member")
}

func (f *teamsRepo) ListUserTeams(_ context.Context, userID gocql.UUID) ([]*models.TeamMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*models.TeamMember
	for team, people := range f.teams {
		if at, ok := people[userID]; ok {
			out = append(out, &models.TeamMember{TeamID: team, UserID: userID, Role: models.TeamRoleMember, JoinedAt: at})
		}
	}
	return out, nil
}

func (f *teamsRepo) ClaimDirectKey(_ context.Context, key string, id gocql.UUID) (gocql.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if holder, ok := f.keys[key]; ok {
		return holder, nil
	}
	f.keys[key] = id
	return id, nil
}

func (f *teamsRepo) CreateDirectChannel(ctx context.Context, key string, ch *models.Channel, members []*models.ChannelMember) (gocql.UUID, error) {
	id, _ := f.ClaimDirectKey(ctx, key, ch.ID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != ch.ID {
		return id, nil
	}
	f.channels[ch.ID] = ch
	f.members[ch.ID] = members
	return ch.ID, nil
}

func (f *teamsRepo) ListUserChannels(_ context.Context, userID, teamID gocql.UUID) ([]*models.ChannelMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*models.ChannelMember
	for _, list := range f.members {
		for _, m := range list {
			if m.UserID == userID && m.TeamID == teamID {
				out = append(out, m)
			}
		}
	}
	return out, nil
}

func (f *teamsRepo) ListChannelMembers(_ context.Context, channelID gocql.UUID) ([]*models.ChannelMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.members[channelID]), nil
}

func (f *teamsRepo) GetChannels(_ context.Context, list []gocql.UUID) ([]*models.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*models.Channel
	for _, id := range list {
		if ch := f.channels[id]; ch != nil {
			out = append(out, ch)
		}
	}
	return out, nil
}

func (f *teamsRepo) ListTeamChannels(context.Context, gocql.UUID) ([]*scylla.TeamChannel, error) {
	return nil, nil
}

func (f *teamsRepo) ListLastReads(context.Context, gocql.UUID) ([]*scylla.ChannelRead, error) {
	return nil, nil
}

func (f *teamsRepo) CountMentions(context.Context, gocql.UUID) (map[gocql.UUID]int, error) {
	return nil, nil
}

func (f *teamsRepo) ListMuted(context.Context, gocql.UUID) (map[gocql.UUID]bool, error) {
	return nil, nil
}

func (f *teamsRepo) ListCleared(_ context.Context, userID gocql.UUID) (map[gocql.UUID]gocql.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cleared[userID], nil
}

// channelLog records what the stand-in channels actor was asked.
type channelLog struct {
	mu      sync.Mutex
	rejoins []channels.Rejoin
	moves   []channels.Move
}

func idOf(s string) gocql.UUID {
	id, _ := ids.Parse(s)
	return id
}

func newTeamsWorker(t *testing.T, repo *teamsRepo) (*worker, *channelLog) {
	t.Helper()
	az, err := authz.New(zap.NewNop(), stringadapter.NewAdapter("p, system_user, direct, create"))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	log := &channelLog{}
	sink := engine.SpawnFunc(func(*actor.Context) {}, "hub")
	chans := engine.SpawnFunc(func(c *actor.Context) {
		log.mu.Lock()
		defer log.mu.Unlock()
		switch msg := c.Message().(type) {
		case channels.View:
			actors.Reply(c, &pb.Channel{Id: msg.ChannelID.String()})
		case channels.Rejoin:
			log.rejoins = append(log.rejoins, msg)
			actors.Reply(c, channels.Done{})
		case channels.Move:
			log.moves = append(log.moves, msg)
			actors.Reply(c, channels.Done{})
		}
	}, "channels")
	return &worker{Deps: &Deps{
		Logger: zap.NewNop(), Cfg: &Config{OpTimeout: time.Second}, Repo: repo, Authz: az,
		Users: users.NewCache(repo, 100, time.Minute), Engine: engine, Hub: sink, Channels: chans,
	}}, log
}

func TestConversationsBelongToTheirTeam(t *testing.T) {
	repo := newTeamsRepo()
	me, bob, carol := repo.person("me"), repo.person("bob"), repo.person("carol")
	acme, beta := ids.New(), ids.New()
	repo.join(acme, time.Now(), me, bob, carol)
	repo.join(beta, time.Now(), me, bob)
	w, _ := newTeamsWorker(t, repo)
	ctx := context.Background()
	open := func(team gocql.UUID, people ...gocql.UUID) (string, error) {
		raw := make([]string, len(people))
		for i, id := range people {
			raw[i] = id.String()
		}
		ch, err := w.openGroup(ctx, OpenGroup{P: principal(me), TeamID: team.String(), UserIDs: raw})
		if err != nil {
			return "", err
		}
		return ch.Id, nil
	}
	inAcme, err := open(acme, bob)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := open(acme, bob); err != nil || again != inAcme {
		t.Fatalf("the same people found another conversation in a team: %s, %s, %v", inAcme, again, err)
	}
	inBeta, err := open(beta, bob)
	if err != nil {
		t.Fatal(err)
	}
	if inBeta == inAcme {
		t.Fatal("another team found the same conversation")
	}
	for id, team := range map[string]gocql.UUID{inAcme: acme, inBeta: beta} {
		ch := repo.channels[idOf(id)]
		if ch.TeamID != team || ch.Kind != models.ChannelDirect {
			t.Fatalf("conversation %s: team %s, kind %s", id, ch.TeamID, ch.Kind)
		}
		for _, m := range repo.members[ch.ID] {
			if m.TeamID != team {
				t.Fatalf("a member of %s is filed under %s", id, m.TeamID)
			}
		}
	}
	// Carol is in acme alone: no conversation with her in beta, nor a group.
	if _, err := open(beta, carol); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("a conversation with someone outside the team: %v", err)
	}
	if _, err := open(beta, bob, carol); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("a group with someone outside the team: %v", err)
	}
	group, err := open(acme, carol, bob)
	if err != nil || repo.channels[idOf(group)].TeamID != acme {
		t.Fatalf("a group in acme: %s, %v", group, err)
	}
	if _, err := w.openGroup(ctx, OpenGroup{P: principal(carol), TeamID: beta.String(), UserIDs: []string{me.String()}}); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("a conversation in a team the caller is not in: %v", err)
	}
	if _, err := w.openGroup(ctx, OpenGroup{P: principal(me), UserIDs: []string{bob.String()}}); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
		t.Fatalf("a conversation in no team: %v", err)
	}
}

func TestTeamListsOnlyItsConversations(t *testing.T) {
	repo := newTeamsRepo()
	me, bob := repo.person("me"), repo.person("bob")
	acme, beta := ids.New(), ids.New()
	repo.join(acme, time.Now(), me, bob)
	repo.join(beta, time.Now(), me, bob)
	w, _ := newTeamsWorker(t, repo)
	ctx := context.Background()
	opened := map[gocql.UUID]string{}
	for _, team := range []gocql.UUID{acme, beta} {
		ch, err := w.openDirect(ctx, OpenDirect{P: principal(me), TeamID: team.String(), UserID: bob.String()})
		if err != nil {
			t.Fatal(err)
		}
		opened[team] = ch.Id
	}
	// One from before teams owned conversations shows in neither.
	legacy := &models.Channel{ID: ids.New(), TeamID: scylla.NoTeam, Name: peopleKey([]gocql.UUID{me, bob}), Kind: models.ChannelDirect}
	repo.channels[legacy.ID] = legacy
	repo.members[legacy.ID] = []*models.ChannelMember{{ChannelID: legacy.ID, UserID: me, TeamID: scylla.NoTeam}}
	for _, team := range []gocql.UUID{acme, beta} {
		for _, who := range []gocql.UUID{me, bob} {
			list, err := w.listChannels(ctx, ListChannels{P: principal(who), TeamID: team.String()})
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 1 || list[0].Id != opened[team] || list[0].GetPeer() == nil {
				t.Fatalf("%s's list in a team: %v, want only %s", repo.users[who].Username, list, opened[team])
			}
		}
	}
	if _, err := w.listChannels(ctx, ListChannels{P: principal(me)}); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
		t.Fatalf("a list of no team: %v", err)
	}
}

func TestReopeningRejoins(t *testing.T) {
	repo := newTeamsRepo()
	me, bob := repo.person("me"), repo.person("bob")
	acme := ids.New()
	repo.join(acme, time.Now(), me, bob)
	w, log := newTeamsWorker(t, repo)
	ctx := context.Background()
	first, err := w.openDirect(ctx, OpenDirect{P: principal(me), TeamID: acme.String(), UserID: bob.String()})
	if err != nil {
		t.Fatal(err)
	}
	log.mu.Lock()
	if len(log.rejoins) != 0 {
		t.Fatal("a new conversation was asked to rejoin people")
	}
	log.mu.Unlock()
	if _, err := w.openDirect(ctx, OpenDirect{P: principal(bob), TeamID: acme.String(), UserID: me.String()}); err != nil {
		t.Fatal(err)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.rejoins) != 1 || log.rejoins[0].ChannelID.String() != first.Id || !slices.Contains(log.rejoins[0].UserIDs, me) || !slices.Contains(log.rejoins[0].UserIDs, bob) {
		t.Fatalf("rejoins %+v", log.rejoins)
	}
}

func TestHomeTeams(t *testing.T) {
	a, b, c := ids.New(), ids.New(), ids.New()
	x, y, z := ids.New(), ids.New(), ids.New()
	day := func(n int) time.Time { return time.Date(2026, 1, n, 0, 0, 0, 0, time.UTC) }
	in := func(team gocql.UUID, n int) *models.TeamMember {
		return &models.TeamMember{TeamID: team, JoinedAt: day(n)}
	}
	cases := []struct {
		name   string
		people map[gocql.UUID][]*models.TeamMember
		want   []gocql.UUID
	}{
		{"shared longest first", map[gocql.UUID][]*models.TeamMember{
			x: {in(a, 5), in(b, 1)}, y: {in(a, 2), in(b, 3)},
		}, []gocql.UUID{b, a}},
		{"most people first", map[gocql.UUID][]*models.TeamMember{
			x: {in(a, 1), in(b, 9)}, y: {in(a, 1), in(b, 9)}, z: {in(b, 9)},
		}, []gocql.UUID{b, a}},
		{"no team of one", map[gocql.UUID][]*models.TeamMember{
			x: {in(a, 1)}, y: {in(b, 1)}, z: {in(c, 1)},
		}, []gocql.UUID{}},
	}
	for _, tc := range cases {
		if got := homeTeams(tc.people); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAdoptGivesOldConversationsATeam(t *testing.T) {
	repo := newTeamsRepo()
	me, bob, carol := repo.person("me"), repo.person("bob"), repo.person("carol")
	older, newer := ids.New(), ids.New()
	repo.join(older, time.Now().Add(-48*time.Hour), me, bob)
	repo.join(newer, time.Now(), me, bob, carol)
	w, log := newTeamsWorker(t, repo)
	a := &adopter{Deps: w.Deps}
	ctx := context.Background()
	legacy := func(people ...gocql.UUID) *models.Channel {
		kind := models.ChannelDirect
		if len(people) > 2 {
			kind = models.ChannelGroup
		}
		ch := &models.Channel{ID: ids.New(), TeamID: scylla.NoTeam, Name: peopleKey(people), Kind: kind}
		for _, id := range people {
			repo.members[ch.ID] = append(repo.members[ch.ID], &models.ChannelMember{ChannelID: ch.ID, UserID: id, TeamID: scylla.NoTeam})
		}
		return ch
	}
	moved := func() channels.Move {
		t.Helper()
		log.mu.Lock()
		defer log.mu.Unlock()
		if len(log.moves) != 1 {
			t.Fatalf("moves %+v", log.moves)
		}
		m := log.moves[0]
		log.moves = nil
		return m
	}

	// A pair goes to the team they have shared longest, a group to the one
	// holding them all.
	direct := legacy(me, bob)
	if err := a.adopt(ctx, direct); err != nil {
		t.Fatal(err)
	}
	if m := moved(); m.ChannelID != direct.ID || m.TeamID != older || repo.keys[scylla.ConversationKey(older, direct.Name)] != direct.ID {
		t.Fatalf("the pair moved %+v", m)
	}
	group := legacy(me, bob, carol)
	if err := a.adopt(ctx, group); err != nil {
		t.Fatal(err)
	}
	if m := moved(); m.TeamID != newer {
		t.Fatalf("the group moved to %s, want %s", m.TeamID, newer)
	}

	// A team where the pair opened a new one since is passed over.
	again := legacy(me, bob)
	repo.keys[scylla.ConversationKey(older, again.Name)] = ids.New()
	if err := a.adopt(ctx, again); err != nil {
		t.Fatal(err)
	}
	if m := moved(); m.TeamID != newer {
		t.Fatalf("the pair moved to %s, want %s", m.TeamID, newer)
	}

	// No team holds two of them, or it belongs to one already.
	stranger := repo.person("stranger")
	for _, ch := range []*models.Channel{legacy(carol, stranger), {ID: ids.New(), TeamID: older, Kind: models.ChannelDirect}, {ID: ids.New(), Kind: models.ChannelPublic}} {
		if err := a.adopt(ctx, ch); err != nil {
			t.Fatal(err)
		}
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.moves) != 0 {
		t.Fatalf("moved %+v", log.moves)
	}
}

func TestClearedConversationsWaitForANewMessage(t *testing.T) {
	repo := newTeamsRepo()
	me, bob := repo.person("me"), repo.person("bob")
	acme := ids.New()
	repo.join(acme, time.Now(), me, bob)
	w, _ := newTeamsWorker(t, repo)
	ctx := context.Background()
	ch, err := w.openDirect(ctx, OpenDirect{P: principal(me), TeamID: acme.String(), UserID: bob.String()})
	if err != nil {
		t.Fatal(err)
	}
	dm := repo.channels[idOf(ch.Id)]
	dm.LastMessageID = ids.NewTime()
	repo.cleared = map[gocql.UUID]map[gocql.UUID]gocql.UUID{me: {dm.ID: ids.NewTime()}}
	listed := func(who gocql.UUID) bool {
		list, err := w.listChannels(ctx, ListChannels{P: principal(who), TeamID: acme.String()})
		if err != nil {
			t.Fatal(err)
		}
		return len(list) == 1
	}
	if listed(me) || !listed(bob) {
		t.Fatal("a clear hid the conversation from the wrong person")
	}
	dm.LastMessageID = ids.NewTime()
	if !listed(me) {
		t.Fatal("a new message did not bring the conversation back")
	}
}

func (r *teamsRepo) ChannelsPts(context.Context, []gocql.UUID) (map[gocql.UUID]int64, error) {
	return nil, nil
}
