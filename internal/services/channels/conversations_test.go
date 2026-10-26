package channels

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/users"
)

// teamRepo holds one team's members and records the membership writes.
type teamRepo struct {
	Repo
	team    gocql.UUID
	in      map[gocql.UUID]bool
	members []*models.ChannelMember
	added   []*models.ChannelMember
	removed []*models.ChannelMember
	moved   []gocql.UUID
	teamSet gocql.UUID
}

func (r *teamRepo) GetTeamMember(_ context.Context, teamID, userID gocql.UUID) (*models.TeamMember, error) {
	if teamID == r.team && r.in[userID] {
		return &models.TeamMember{TeamID: teamID, UserID: userID, Role: models.TeamRoleMember}, nil
	}
	return nil, ierrors.NotFound("not a member")
}

func (r *teamRepo) GetUser(_ context.Context, id gocql.UUID) (*models.User, error) {
	return &models.User{ID: id, Username: id.String()[:8]}, nil
}

func (r *teamRepo) GetUsers(_ context.Context, list []gocql.UUID) ([]*models.User, error) {
	out := make([]*models.User, len(list))
	for i, id := range list {
		out[i] = &models.User{ID: id, Username: id.String()[:8]}
	}
	return out, nil
}

func (r *teamRepo) ListChannelMembers(context.Context, gocql.UUID) ([]*models.ChannelMember, error) {
	return slices.Clone(r.members), nil
}

func (r *teamRepo) AddChannelMember(_ context.Context, m *models.ChannelMember) error {
	r.added = append(r.added, m)
	return nil
}

func (r *teamRepo) RemoveChannelMember(_ context.Context, m *models.ChannelMember) error {
	r.removed = append(r.removed, m)
	return nil
}

func (r *teamRepo) MoveChannelMember(_ context.Context, m *models.ChannelMember, teamID gocql.UUID) error {
	if teamID == r.team {
		r.moved = append(r.moved, m.UserID)
	}
	return nil
}

func (r *teamRepo) SetChannelTeam(_ context.Context, _, teamID gocql.UUID) error {
	r.teamSet = teamID
	return nil
}

func conversation(t *testing.T, repo *teamRepo, ch *models.Channel, members ...gocql.UUID) *channel {
	t.Helper()
	engine, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	a := &channel{
		Deps: &Deps{
			Logger: zap.NewNop(), Cfg: &Config{OpTimeout: time.Second}, Repo: logged(repo), UpdatesCfg: testUpdates,
			Users: users.NewCache(repo, 10, time.Minute), Engine: engine, Hub: engine.SpawnFunc(func(*actor.Context) {}, "hub"),
		},
		id: ch.ID, ch: ch, members: map[gocql.UUID]models.ChannelRole{}, typing: map[gocql.UUID]time.Time{},
		posters: map[gocql.UUID]*rate.Limiter{},
	}
	for _, id := range members {
		a.members[id] = models.ChannelRoleMember
		repo.members = append(repo.members, &models.ChannelMember{ChannelID: ch.ID, UserID: id, TeamID: ch.TeamID, Role: models.ChannelRoleMember})
	}
	a.snapshot()
	return a
}

func TestMoveGivesAConversationToATeam(t *testing.T) {
	x, y := ids.New(), ids.New()
	repo := &teamRepo{team: ids.New(), in: map[gocql.UUID]bool{x: true}}
	ch := &models.Channel{ID: ids.New(), TeamID: scylla.NoTeam, Name: x.String() + ":" + y.String(), Kind: models.ChannelDirect}
	a := conversation(t, repo, ch, x, y)
	if _, err := a.move(Move{Target: Target{ChannelID: ch.ID}, TeamID: repo.team}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(repo.moved, []gocql.UUID{x}) || len(repo.removed) != 1 || repo.removed[0].UserID != y || repo.removed[0].TeamID != scylla.NoTeam {
		t.Fatalf("moved %v, removed %+v", repo.moved, repo.removed)
	}
	if repo.teamSet != repo.team || a.ch.TeamID != repo.team {
		t.Fatalf("the conversation's team: stored %s, held %s", repo.teamSet, a.ch.TeamID)
	}
	if _, ok := a.members[y]; ok || len(a.recipients) != 1 {
		t.Fatalf("members %v, recipients %v", a.members, a.recipients)
	}
	if ev := a.posted(&pb.Message{}, nil); ev.TeamId != repo.team.String() || !ev.Conversation {
		t.Fatalf("a message's event: team %q, conversation %v", ev.TeamId, ev.Conversation)
	}
	// Once moved it stays.
	if _, err := a.move(Move{Target: Target{ChannelID: ch.ID}, TeamID: repo.team}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.move(Move{Target: Target{ChannelID: ch.ID}, TeamID: ids.New()}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("a move to another team: %v", err)
	}
	public := conversation(t, &teamRepo{}, &models.Channel{ID: ids.New(), Kind: models.ChannelPublic})
	if _, err := public.move(Move{TeamID: repo.team}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("a channel moved: %v", err)
	}
}

func TestRejoinBringsBackTheTeamsPeople(t *testing.T) {
	x, y, z, w := ids.New(), ids.New(), ids.New(), ids.New()
	repo := &teamRepo{team: ids.New(), in: map[gocql.UUID]bool{x: true, y: true, w: true}}
	ch := &models.Channel{ID: ids.New(), Name: x.String() + ":" + y.String() + ":" + z.String(), Kind: models.ChannelGroup}
	ch.TeamID = repo.team
	a := conversation(t, repo, ch, x)
	// z left the team, w is not one of its people.
	if _, err := a.rejoin(Rejoin{Target: Target{ChannelID: ch.ID}, UserIDs: []gocql.UUID{x, y, z, w}}); err != nil {
		t.Fatal(err)
	}
	if len(repo.added) != 1 || repo.added[0].UserID != y || repo.added[0].TeamID != repo.team {
		t.Fatalf("added %+v", repo.added)
	}
	if _, ok := a.members[y]; !ok || len(a.members) != 2 {
		t.Fatalf("members %v", a.members)
	}
}
