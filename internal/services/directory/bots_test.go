package directory

import (
	"context"
	"slices"
	"testing"

	"github.com/gocql/gocql"

	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

func (f *fakeRepo) ListBotIDs(_ context.Context, owner gocql.UUID) ([]gocql.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []gocql.UUID
	for id, u := range f.users {
		if u.Bot && u.BotOwner == owner {
			out = append(out, id)
		}
	}
	return out, nil
}

// addBot makes a bot of owner, a member of the team.
func (f *fakeRepo) addBot(name string, owner gocql.UUID) gocql.UUID {
	id := f.addMember(name, models.TeamRoleMember)
	f.users[id].Bot, f.users[id].BotOwner = true, owner
	return id
}

func TestBotsLeaveTheTeamWithTheirOwner(t *testing.T) {
	repo := newFakeRepo()
	admin := repo.addMember("admin", models.TeamRoleAdmin)
	bob := repo.addMember("bob", models.TeamRoleMember)
	bot := repo.addBot("bob_bot", bob)
	other := repo.addBot("admin_bot", admin)
	repo.channels[bot] = []gocql.UUID{ids.New()}
	w := newWorker(t, repo)
	if _, err := w.removeTeamMember(context.Background(), RemoveTeamMember{P: principal(admin), TeamID: repo.team.String(), UserID: bob.String()}); err != nil {
		t.Fatal(err)
	}
	if _, in := repo.members[bot]; in {
		t.Fatal("the bot stayed in the team without its owner")
	}
	if _, in := repo.members[other]; !in {
		t.Fatal("another owner's bot left the team")
	}
	if n := len(slices.DeleteFunc(slices.Clone(repo.log), func(s string) bool { return s != "leave channel" })); n != 1 {
		t.Fatalf("the bot left %d channels, want 1: %q", n, repo.log)
	}
}

// botTeams lists the one team's memberships.
type botTeams struct{ *fakeRepo }

func (f botTeams) ListUserTeams(_ context.Context, userID gocql.UUID) ([]*models.TeamMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, in := f.members[userID]; !in {
		return nil, nil
	}
	return []*models.TeamMember{{TeamID: f.team, UserID: userID}}, nil
}

func TestADeletedBotLeavesItsTeams(t *testing.T) {
	repo := newFakeRepo()
	bob := repo.addMember("bob", models.TeamRoleMember)
	bot := repo.addBot("bob_bot", bob)
	repo.channels[bot] = []gocql.UUID{ids.New(), ids.New()}
	w := newWorker(t, repo)
	w.Repo = botTeams{repo}
	if _, err := w.removeBot(context.Background(), RemoveBot{P: principal(bob), BotID: bot}); err != nil {
		t.Fatal(err)
	}
	if _, in := repo.members[bot]; in {
		t.Fatal("the bot stayed in the team")
	}
	if n := len(slices.DeleteFunc(slices.Clone(repo.log), func(s string) bool { return s != "leave channel" })); n != 2 {
		t.Fatalf("the bot left %d channels, want 2: %q", n, repo.log)
	}
}

func TestOnlyTheOwnerAddsTheirBot(t *testing.T) {
	repo := newFakeRepo()
	admin := repo.addMember("admin", models.TeamRoleAdmin)
	bob := repo.addMember("bob", models.TeamRoleMember)
	bot := repo.addBot("admin_bot", admin)
	delete(repo.members, bot)
	w := newWorker(t, repo)
	// Bob may not invite people, nor is the bot his.
	if _, err := w.addBot(context.Background(), AddBot{P: principal(bob), BotID: bot, TeamID: repo.team.String()}); err == nil {
		t.Fatal("a member added someone else's bot")
	}
	if _, in := repo.members[bot]; in {
		t.Fatal("the bot joined")
	}
	if _, err := w.addBot(context.Background(), AddBot{P: principal(admin), BotID: ids.New(), TeamID: repo.team.String()}); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("adding no bot: %v", err)
	}
}
