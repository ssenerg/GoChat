package channels

import (
	"context"
	"testing"
	"time"

	stringadapter "github.com/casbin/casbin/v2/persist/string-adapter"
	"github.com/gocql/gocql"
	"go.uber.org/zap"

	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/users"
)

// leavingRepo answers that the user belongs to the team until they join a
// channel: a removal from the team racing the join.
type leavingRepo struct {
	Repo
	joined, removed bool
}

func (r *leavingRepo) GetTeamMember(_ context.Context, teamID, userID gocql.UUID) (*models.TeamMember, error) {
	if r.joined {
		return nil, ierrors.NotFound("not a member")
	}
	return &models.TeamMember{TeamID: teamID, UserID: userID, Role: models.TeamRoleMember}, nil
}

func (r *leavingRepo) GetUser(_ context.Context, id gocql.UUID) (*models.User, error) {
	return &models.User{ID: id, Username: "someone"}, nil
}

func (r *leavingRepo) GetUsers(ctx context.Context, list []gocql.UUID) ([]*models.User, error) {
	out := make([]*models.User, len(list))
	for i, id := range list {
		out[i], _ = r.GetUser(ctx, id)
	}
	return out, nil
}

func (r *leavingRepo) AddChannelMember(context.Context, *models.ChannelMember) error {
	r.joined = true
	return nil
}

func (r *leavingRepo) RemoveChannelMember(context.Context, *models.ChannelMember) error {
	r.removed = true
	return nil
}

func TestJoinUndoneWhenTheTeamLeft(t *testing.T) {
	az, err := authz.New(zap.NewNop(), stringadapter.NewAdapter("p, team_member, channel, join"))
	if err != nil {
		t.Fatal(err)
	}
	repo := &leavingRepo{}
	a := &channel{
		Deps: &Deps{
			Logger: zap.NewNop(), Cfg: &Config{OpTimeout: time.Second}, Repo: logged(repo), UpdatesCfg: testUpdates, Authz: az,
			Users: users.NewCache(repo, 10, time.Minute),
		},
		id: ids.New(), ch: &models.Channel{TeamID: ids.New(), Kind: models.ChannelPublic},
		members: map[gocql.UUID]models.ChannelRole{}, typing: map[gocql.UUID]time.Time{},
	}
	user := ids.New()
	_, err = a.join(Join{Target: Target{P: &models.Principal{UserID: user, Role: models.SystemRoleUser}, ChannelID: a.id}})
	if !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("join after leaving the team: %v", err)
	}
	if !repo.removed {
		t.Fatal("the membership was not undone")
	}
	if _, ok := a.members[user]; ok {
		t.Fatal("still a member")
	}
}
