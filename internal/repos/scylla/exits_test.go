package scylla

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gocql/gocql"

	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

func liveUser(t *testing.T, r *Repository, role models.SystemRole) *models.User {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	name := "exit" + ids.New().String()[:8]
	u := &models.User{
		ID: ids.New(), Username: name, Email: name + "@example.test", DisplayName: "Exit", PasswordHash: "hash",
		Role: role, CreatedAt: now, UpdatedAt: now,
	}
	if err := r.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestEraseUserLive(t *testing.T) {
	r := liveRepository(t)
	ctx := context.Background()
	u := liveUser(t, r, models.SystemRoleUser)
	if err := r.SetCustomStatus(ctx, u.ID, "coffee", "away", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetUserPhoto(ctx, u.ID, "v1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.ReplaceRecoveryCodes(ctx, u.ID, [][]byte{{1}, {2}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := r.EraseUser(ctx, u, "Deleted user", now); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "" || got.Email != "" || got.DisplayName != "Deleted user" || got.PasswordHash != "" || got.Photo != "" ||
		got.StatusEmoji != "" || got.StatusText != "" || !got.Deactivated || !got.DeletedAt.Equal(now) || got.Role != u.Role || !got.CreatedAt.Equal(u.CreatedAt) {
		t.Fatalf("left of the account: %+v", got)
	}
	if ok, err := r.ConsumeRecoveryCode(ctx, u.ID, []byte{1}); err != nil || ok {
		t.Fatalf("a recovery code outlived the account: %v %v", ok, err)
	}
	for _, lookup := range []func() (gocql.UUID, error){
		func() (gocql.UUID, error) { return r.GetUserIDByUsername(ctx, u.Username) },
		func() (gocql.UUID, error) { return r.GetUserIDByEmail(ctx, u.Email) },
	} {
		if _, err := lookup(); !ierrors.Is(err, ierrors.CodeNotFound) {
			t.Fatalf("a deleted account's name or email still names it: %v", err)
		}
	}
	// Someone takes them, and erasing the old account again frees nothing.
	again := &models.User{ID: ids.New(), Username: u.Username, Email: u.Email, DisplayName: "Again", Role: models.SystemRoleUser, CreatedAt: now, UpdatedAt: now}
	if err := r.CreateUser(ctx, again); err != nil {
		t.Fatalf("taking a deleted account's name: %v", err)
	}
	if err := r.EraseUser(ctx, u, "Deleted user", now); err != nil {
		t.Fatal(err)
	}
	if id, err := r.GetUserIDByUsername(ctx, u.Username); err != nil || id != again.ID {
		t.Fatalf("the name went from its new owner: %v %v", id, err)
	}
}

func TestListSystemAdminsLive(t *testing.T) {
	r := liveRepository(t)
	ctx := context.Background()
	admin := liveUser(t, r, models.SystemRoleAdmin)
	user := liveUser(t, r, models.SystemRoleUser)
	gone := liveUser(t, r, models.SystemRoleAdmin)
	if err := r.EraseUser(ctx, gone, "Deleted user", time.Now()); err != nil {
		t.Fatal(err)
	}
	list, err := r.ListSystemAdmins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(list, admin.ID) || slices.Contains(list, user.ID) || slices.Contains(list, gone.ID) {
		t.Fatalf("admins %v: want %s, not %s or %s", list, admin.ID, user.ID, gone.ID)
	}
}

func TestTeamOwnerAndEveryChannelLive(t *testing.T) {
	r := liveRepository(t)
	ctx := context.Background()
	owner, heir := liveUser(t, r, models.SystemRoleUser), liveUser(t, r, models.SystemRoleUser)
	now := time.Now().UTC()
	team := &models.Team{ID: ids.New(), Name: "exit-" + ids.New().String()[:8], DisplayName: "Exit", CreatedBy: owner.ID, CreatedAt: now}
	if err := r.CreateTeam(ctx, &NewTeam{
		Team: team, Creator: &models.TeamMember{TeamID: team.ID, UserID: owner.ID, Role: models.TeamRoleAdmin, JoinedAt: now},
		Username: owner.Username, DisplayName: owner.DisplayName,
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetTeamOwner(ctx, team.ID, heir.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := r.GetTeam(ctx, team.ID); err != nil || got.CreatedBy != heir.ID {
		t.Fatalf("owner %v, %v", got, err)
	}

	// Channels of two teams and a conversation of none.
	var want []gocql.UUID
	for _, teamID := range []gocql.UUID{team.ID, ids.New()} {
		ch := &models.Channel{ID: ids.New(), TeamID: teamID, Name: "general", DisplayName: "General", Kind: models.ChannelPublic, CreatedBy: owner.ID, CreatedAt: now}
		if err := r.CreateChannel(ctx, ch, &models.ChannelMember{ChannelID: ch.ID, UserID: owner.ID, TeamID: teamID, Role: models.ChannelRoleAdmin, JoinedAt: now}); err != nil {
			t.Fatal(err)
		}
		want = append(want, ch.ID)
	}
	dm := &models.Channel{ID: ids.New(), Kind: models.ChannelDirect, CreatedBy: owner.ID, CreatedAt: now}
	members := []*models.ChannelMember{
		{ChannelID: dm.ID, UserID: owner.ID, Role: models.ChannelRoleMember, JoinedAt: now},
		{ChannelID: dm.ID, UserID: heir.ID, Role: models.ChannelRoleMember, JoinedAt: now},
	}
	id, err := r.CreateDirectChannel(ctx, "exit:"+ids.New().String(), dm, members)
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, id)
	got, err := r.ListAllUserChannels(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	var listed []gocql.UUID
	for _, m := range got {
		listed = append(listed, m.ChannelID)
	}
	slices.SortFunc(listed, func(a, b gocql.UUID) int { return slices.Compare(a[:], b[:]) })
	slices.SortFunc(want, func(a, b gocql.UUID) int { return slices.Compare(a[:], b[:]) })
	if !slices.Equal(listed, want) {
		t.Fatalf("listed %v, want %v", listed, want)
	}
}

func TestSSOFlowKeepsReauthLive(t *testing.T) {
	r := liveRepository(t)
	ctx := context.Background()
	now := time.Now()
	for _, reauth := range []bool{true, false} {
		f := &models.SSOFlow{StateHash: []byte(ids.New().String()), Provider: "idp", Nonce: "n", Verifier: []byte("v"), RedirectURI: "https://x.test/cb", ExpiresAt: now.Add(time.Minute), Reauth: reauth}
		if err := r.CreateSSOFlow(ctx, f, now); err != nil {
			t.Fatal(err)
		}
		got, err := r.ConsumeSSOFlow(ctx, f.StateHash)
		if err != nil || got.Reauth != reauth {
			t.Fatalf("reauth %v came back as %+v, %v", reauth, got, err)
		}
	}
}
