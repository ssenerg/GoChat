package directory

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"

	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/hub"
)

func (f *fakeRepo) SetTeamOwner(_ context.Context, _, userID gocql.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.owner = userID
	f.log = append(f.log, "set owner")
	return nil
}

func (f *fakeRepo) ListAllUserChannels(ctx context.Context, userID gocql.UUID) ([]*models.ChannelMember, error) {
	return f.ListUserChannels(ctx, userID, f.team)
}

// deliveries records what the worker tells people, by user.
func deliveries(w *worker) chan hub.Deliver {
	got := make(chan hub.Deliver, 32)
	w.Hub = w.Engine.SpawnFunc(func(c *actor.Context) {
		if d, ok := c.Message().(hub.Deliver); ok {
			got <- d
		}
	}, "deliveries")
	return got
}

func TestLeaveTeam(t *testing.T) {
	repo := newFakeRepo()
	admin := repo.addMember("admin", models.TeamRoleAdmin)
	bob := repo.addMember("bob", models.TeamRoleMember)
	bot := repo.addBot("bob_bot", bob)
	repo.channels[bob] = []gocql.UUID{ids.New(), ids.New()}
	w := newWorker(t, repo)
	got := deliveries(w)
	w.Repo = botTeams{repo}
	if _, err := w.leaveTeam(context.Background(), LeaveTeam{P: principal(bob), TeamID: repo.team.String()}); err != nil {
		t.Fatal(err)
	}
	if _, in := repo.members[bob]; in {
		t.Fatal("bob is still in the team")
	}
	if _, in := repo.members[bot]; in {
		t.Fatal("bob's bot stayed without him")
	}
	if len(repo.channels[bob]) != 0 {
		t.Fatal("bob is still in its channels")
	}
	var left, changed bool
	for !left || !changed {
		select {
		case d := <-got:
			switch {
			case d.Event.GetTeamLeft() != nil:
				left = left || slices.Equal(d.UserIDs, []string{bob.String()})
			case d.Event.GetTeamMembersChanged() != nil:
				ev := d.Event.GetTeamMembersChanged()
				changed = slices.Equal(d.UserIDs, []string{admin.String()}) && ev.UserId == bob.String() && ev.TeamId == repo.team.String()
				if !changed {
					t.Fatalf("told %v of %v", d.UserIDs, ev)
				}
			}
		case <-time.After(time.Second):
			t.Fatalf("bob told he left: %v, the team told: %v", left, changed)
		}
	}
	if _, err := w.leaveTeam(context.Background(), LeaveTeam{P: principal(bob), TeamID: repo.team.String()}); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("leaving again: %v", err)
	}
}

func TestTheLastAdminLeavesOnlyAnEmptyTeam(t *testing.T) {
	repo := newFakeRepo()
	admin := repo.addMember("admin", models.TeamRoleAdmin)
	bob := repo.addMember("bob", models.TeamRoleMember)
	repo.addBot("admin_bot", admin)
	repo.owner = admin
	w := newWorker(t, repo)
	leave := func(id gocql.UUID) error {
		_, err := w.leaveTeam(context.Background(), LeaveTeam{P: principal(id), TeamID: repo.team.String()})
		return err
	}
	err := leave(admin)
	if !ierrors.Is(err, ierrors.CodeFailedPrecondition) || ierrors.As(err).Message != "you are the only admin of Acme: make another member an admin before you leave" {
		t.Fatalf("the last admin left: %v", err)
	}
	if _, in := repo.members[admin]; !in {
		t.Fatal("the refusal removed the admin")
	}
	// Alone with their own bot, they may.
	if err := leave(bob); err != nil {
		t.Fatal(err)
	}
	if err := leave(admin); err != nil {
		t.Fatalf("the last one out: %v", err)
	}
	if len(repo.members) != 0 {
		t.Fatalf("%d stayed", len(repo.members))
	}
	if slices.Contains(repo.log, "set owner") {
		t.Fatal("an empty team got an owner")
	}
}

func TestTheOwnerHandsTheTeamOver(t *testing.T) {
	repo := newFakeRepo()
	owner := repo.addMember("owner", models.TeamRoleAdmin)
	early := repo.addMember("early", models.TeamRoleAdmin)
	late := repo.addMember("late", models.TeamRoleAdmin)
	repo.addMember("bob", models.TeamRoleMember)
	now := time.Now()
	repo.members[early].JoinedAt, repo.members[late].JoinedAt = now.Add(-time.Hour), now
	repo.owner = owner
	w := newWorker(t, repo)
	got := deliveries(w)
	if _, err := w.leaveTeam(context.Background(), LeaveTeam{P: principal(owner), TeamID: repo.team.String()}); err != nil {
		t.Fatal(err)
	}
	if repo.owner != early {
		t.Fatalf("the team went to %v, not the admin who joined first", repo.owner)
	}
	for {
		select {
		case d := <-got:
			if team := d.Event.GetTeamJoined().GetTeam(); team != nil {
				if !slices.Equal(d.UserIDs, []string{early.String()}) || team.OwnerId != early.String() {
					t.Fatalf("told %v the owner is %s", d.UserIDs, team.OwnerId)
				}
				return
			}
		case <-time.After(time.Second):
			t.Fatal("the new owner was not told")
		}
	}
}

func TestLeaveEveryTeam(t *testing.T) {
	repo := newFakeRepo()
	admin := repo.addMember("admin", models.TeamRoleAdmin)
	bob := repo.addMember("bob", models.TeamRoleMember)
	repo.channels[bob] = []gocql.UUID{ids.New()}
	w := newWorker(t, repo)
	w.Repo = botTeams{repo}
	ctx := context.Background()
	// The check refuses the last admin, and leaves no one.
	_, err := w.leaveTeams(ctx, LeaveTeams{P: principal(admin), Check: true})
	if !ierrors.Is(err, ierrors.CodeFailedPrecondition) || !strings.HasSuffix(ierrors.As(err).Message, "before you delete your account") {
		t.Fatalf("checking the last admin: %v", err)
	}
	if _, err := w.leaveTeams(ctx, LeaveTeams{P: principal(bob), Check: true}); err != nil {
		t.Fatal(err)
	}
	if _, in := repo.members[bob]; !in || len(repo.channels[bob]) != 1 {
		t.Fatal("the check left the team")
	}
	if _, err := w.leaveTeams(ctx, LeaveTeams{P: principal(admin)}); !ierrors.Is(err, ierrors.CodeFailedPrecondition) {
		t.Fatalf("the last admin left: %v", err)
	}
	if _, err := w.leaveTeams(ctx, LeaveTeams{P: principal(bob)}); err != nil {
		t.Fatal(err)
	}
	if _, in := repo.members[bob]; in || len(repo.channels[bob]) != 0 {
		t.Fatal("bob stayed")
	}
	// Channels a leave left behind go too, team or none.
	repo.channels[bob] = []gocql.UUID{ids.New()}
	if _, err := w.leaveTeams(ctx, LeaveTeams{P: principal(bob)}); err != nil {
		t.Fatal(err)
	}
	if len(repo.channels[bob]) != 0 {
		t.Fatal("a channel kept bob")
	}
}

func TestABotCannotLeaveATeam(t *testing.T) {
	repo := newFakeRepo()
	bob := repo.addMember("bob", models.TeamRoleMember)
	bot := repo.addBot("bob_bot", bob)
	w := newWorker(t, repo)
	p := principal(bot)
	p.Bot = true
	if _, err := w.leaveTeam(context.Background(), LeaveTeam{P: p, TeamID: repo.team.String()}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("a bot left: %v", err)
	}
}
