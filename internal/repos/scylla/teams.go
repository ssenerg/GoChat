package scylla

import (
	"bytes"
	"context"
	"slices"
	"time"

	"github.com/gocql/gocql"
	"go.uber.org/zap"

	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

const (
	teamWritten = "id, name, display_name, description, created_by, created_at"
	teamColumns = teamWritten + ", photo"
)

// TeamMemberEntry is a team member with the profile fields kept beside it for
// listing and search.
type TeamMemberEntry struct {
	models.TeamMember
	Username    string `db:"username"`
	DisplayName string `db:"display_name"`
}

// NewTeam is a team as it is created: its creator, an admin, in each of its
// first channels.
type NewTeam struct {
	Team                  *models.Team
	Creator               *models.TeamMember
	Username, DisplayName string
	Channels              []*models.Channel
}

// CreateTeam claims the team's name, then writes the rest in one logged
// batch, so a team never exists without its creator and channels. The
// names of a new team's channels cannot be taken yet: they need no claim.
func (r *Repository) CreateTeam(ctx context.Context, n *NewTeam) error {
	t, m := n.Team, n.Creator
	ok, err := r.claim(ctx, "INSERT INTO team_names (name, team_id) VALUES (?, ?) IF NOT EXISTS",
		"SELECT team_id FROM team_names WHERE name = ?", "team_id", t.ID, t.Name)
	if err != nil {
		return err
	}
	if !ok {
		return ierrors.AlreadyExists("team name is taken")
	}
	b := r.batch(ctx, true)
	b.Query("INSERT INTO teams ("+teamWritten+") VALUES (?, ?, ?, ?, ?, ?)",
		t.ID, t.Name, t.DisplayName, t.Description, t.CreatedBy, t.CreatedAt)
	b.Query("INSERT INTO team_members (team_id, user_id, role, username, display_name, joined_at) VALUES (?, ?, ?, ?, ?, ?)",
		m.TeamID, m.UserID, string(m.Role), n.Username, n.DisplayName, m.JoinedAt)
	b.Query("INSERT INTO user_teams (user_id, team_id, role, joined_at) VALUES (?, ?, ?, ?)",
		m.UserID, m.TeamID, string(m.Role), m.JoinedAt)
	for _, ch := range n.Channels {
		b.Query("INSERT INTO channel_names (team_id, name, channel_id) VALUES (?, ?, ?)", ch.TeamID, ch.Name, ch.ID)
		b.Query("INSERT INTO channels (id, team_id, name, display_name, purpose, kind, created_by, created_at, voice_disabled) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			ch.ID, ch.TeamID, ch.Name, ch.DisplayName, ch.Purpose, string(ch.Kind), ch.CreatedBy, ch.CreatedAt, ch.VoiceDisabled)
		b.Query("INSERT INTO team_channels (team_id, channel_id, kind) VALUES (?, ?, ?)", ch.TeamID, ch.ID, string(ch.Kind))
		addMemberQueries(b, &models.ChannelMember{ChannelID: ch.ID, UserID: m.UserID, TeamID: t.ID, Role: models.ChannelRoleAdmin, JoinedAt: m.JoinedAt})
	}
	// A logged batch that timed out is replayed from the batch log, so its
	// claim stays.
	if err := r.execBatch(b); err != nil {
		r.release(err, "DELETE FROM team_names WHERE name = ? IF team_id = ?", t.Name, t.ID)
		return err
	}
	return nil
}

func (r *Repository) GetTeam(ctx context.Context, id gocql.UUID) (*models.Team, error) {
	var t models.Team
	if err := r.query(ctx, "SELECT "+teamColumns+" FROM teams WHERE id = ?", id).GetRelease(&t); err != nil {
		return nil, wrap(err)
	}
	return &t, nil
}

// SetTeamPhoto records the version of the team's photo, empty for none.
func (r *Repository) SetTeamPhoto(ctx context.Context, id gocql.UUID, version string) error {
	return wrap(r.query(ctx, "UPDATE teams SET photo = ? WHERE id = ?", nullable(version), id).ExecRelease())
}

func (r *Repository) GetTeamIDByName(ctx context.Context, name string) (gocql.UUID, error) {
	var id gocql.UUID
	if err := r.query(ctx, "SELECT team_id FROM team_names WHERE name = ?", name).Scan(&id); err != nil {
		return gocql.UUID{}, wrap(err)
	}
	return id, nil
}

func (r *Repository) GetTeams(ctx context.Context, ids []gocql.UUID) ([]*models.Team, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var out []*models.Team
	err := r.query(ctx, "SELECT "+teamColumns+" FROM teams WHERE id IN ?", ids).SelectRelease(&out)
	return out, wrap(err)
}

// Team memberships are only written conditionally, so concurrent role
// changes and removals are ordered by the cluster: a role update can never
// bring back a removed member. user_teams decides access and is written last
// on joining and first on leaving.

func (r *Repository) AddTeamMember(ctx context.Context, m *models.TeamMember, username, displayName string) error {
	_, err := r.query(ctx, "INSERT INTO team_members (team_id, user_id, role, username, display_name, joined_at) VALUES (?, ?, ?, ?, ?, ?) IF NOT EXISTS",
		m.TeamID, m.UserID, string(m.Role), username, displayName, m.JoinedAt).ExecCASRelease()
	if err != nil {
		return wrap(err)
	}
	_, err = r.query(ctx, "INSERT INTO user_teams (user_id, team_id, role, joined_at) VALUES (?, ?, ?, ?) IF NOT EXISTS",
		m.UserID, m.TeamID, string(m.Role), m.JoinedAt).ExecCASRelease()
	return wrap(err)
}

func (r *Repository) GetTeamMember(ctx context.Context, teamID, userID gocql.UUID) (*models.TeamMember, error) {
	var m models.TeamMember
	err := r.query(ctx, "SELECT team_id, user_id, role, joined_at FROM user_teams WHERE user_id = ? AND team_id = ?", userID, teamID).GetRelease(&m)
	if err != nil {
		return nil, wrap(err)
	}
	return &m, nil
}

func (r *Repository) ListTeamMembers(ctx context.Context, teamID gocql.UUID) ([]*TeamMemberEntry, error) {
	var out []*TeamMemberEntry
	err := r.query(ctx, "SELECT team_id, user_id, role, username, display_name, joined_at FROM team_members WHERE team_id = ?", teamID).SelectRelease(&out)
	return out, wrap(err)
}

func (r *Repository) ListUserTeams(ctx context.Context, userID gocql.UUID) ([]*models.TeamMember, error) {
	var out []*models.TeamMember
	err := r.query(ctx, "SELECT team_id, user_id, role, joined_at FROM user_teams WHERE user_id = ?", userID).SelectRelease(&out)
	return out, wrap(err)
}

// SetTeamOwner makes userID the team's owner, its creator as far as the
// rules go.
func (r *Repository) SetTeamOwner(ctx context.Context, teamID, userID gocql.UUID) error {
	return wrap(r.query(ctx, "UPDATE teams SET created_by = ? WHERE id = ?", userID, teamID).ExecRelease())
}

func (r *Repository) RemoveTeamMember(ctx context.Context, teamID, userID gocql.UUID) error {
	if _, err := r.query(ctx, "DELETE FROM user_teams WHERE user_id = ? AND team_id = ? IF EXISTS", userID, teamID).ExecCASRelease(); err != nil {
		return wrap(err)
	}
	_, err := r.query(ctx, "DELETE FROM team_members WHERE team_id = ? AND user_id = ? IF EXISTS", teamID, userID).ExecCASRelease()
	return wrap(err)
}

// UpdateTeamMemberRole changes the role of a member, NotFound when the user is
// not one.
func (r *Repository) UpdateTeamMemberRole(ctx context.Context, teamID, userID gocql.UUID, role models.TeamRole) error {
	applied, err := r.query(ctx, "UPDATE user_teams SET role = ? WHERE user_id = ? AND team_id = ? IF EXISTS", string(role), userID, teamID).ExecCASRelease()
	if err != nil {
		return wrap(err)
	}
	if !applied {
		return ierrors.NotFound("not a member of the team")
	}
	_, err = r.query(ctx, "UPDATE team_members SET role = ? WHERE team_id = ? AND user_id = ? IF EXISTS", string(role), teamID, userID).ExecCASRelease()
	return wrap(err)
}

// UpdateMemberDisplayName refreshes the copy of a display name kept with each
// of the user's team memberships.
func (r *Repository) UpdateMemberDisplayName(ctx context.Context, userID gocql.UUID, name string) error {
	teams, err := r.ListUserTeams(ctx, userID)
	if err != nil {
		return err
	}
	for _, t := range teams {
		if err := r.query(ctx, "UPDATE team_members SET display_name = ? WHERE team_id = ? AND user_id = ? IF EXISTS",
			name, t.TeamID, userID).Exec(); err != nil {
			return wrap(err)
		}
	}
	return nil
}

const inviteColumns = "token_hash, team_id, created_by, created_at, expires_at, used_by, used_at"

// CreateInvite writes the invite and lists it with its team's open invites.
func (r *Repository) CreateInvite(ctx context.Context, inv *models.Invite) error {
	b := r.batch(ctx, true)
	b.Query("INSERT INTO invites (token_hash, team_id, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?) USING TTL ?",
		inv.TokenHash, inv.TeamID, inv.CreatedBy, inv.CreatedAt, inv.ExpiresAt, ttlSeconds(inv.ExpiresAt, inv.CreatedAt))
	listTeamInvite(b, inv, inv.CreatedAt)
	return r.execBatch(b)
}

func listTeamInvite(b *gocql.Batch, inv *models.Invite, now time.Time) {
	b.Query("INSERT INTO team_invites (team_id, token_hash, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?) USING TTL ?",
		inv.TeamID, inv.TokenHash, inv.CreatedBy, inv.CreatedAt, inv.ExpiresAt, ttlSeconds(inv.ExpiresAt, now))
}

func (r *Repository) GetInvite(ctx context.Context, hash []byte) (*models.Invite, error) {
	var inv models.Invite
	err := r.query(ctx, "SELECT "+inviteColumns+" FROM invites WHERE token_hash = ?", hash).GetRelease(&inv)
	if err != nil {
		return nil, wrap(err)
	}
	return &inv, nil
}

const teamInviteColumns = "team_id, token_hash, created_by, created_at, expires_at"

// ListTeamInvites returns the invites listed for the team and not expired,
// in no order. Some may have been used: OpenInvites tells.
func (r *Repository) ListTeamInvites(ctx context.Context, teamID gocql.UUID) ([]*models.Invite, error) {
	var listed []*models.Invite
	err := r.query(ctx, "SELECT "+teamInviteColumns+" FROM team_invites WHERE team_id = ?", teamID).SelectRelease(&listed)
	if err != nil {
		return nil, wrap(err)
	}
	now := time.Now()
	return slices.DeleteFunc(listed, func(inv *models.Invite) bool { return !now.Before(inv.ExpiresAt) }), nil
}

// FindTeamInvites returns the team's listed invites whose token hash starts
// with prefix.
func (r *Repository) FindTeamInvites(ctx context.Context, teamID gocql.UUID, prefix []byte) ([]*models.Invite, error) {
	var listed []*models.Invite
	q := "SELECT " + teamInviteColumns + " FROM team_invites WHERE team_id = ? AND token_hash >= ?"
	args := []any{teamID, prefix}
	if end := prefixEnd(prefix); end != nil {
		q += " AND token_hash < ?"
		args = append(args, end)
	}
	if err := r.query(ctx, q, args...).SelectRelease(&listed); err != nil {
		return nil, wrap(err)
	}
	return slices.DeleteFunc(listed, func(inv *models.Invite) bool { return !bytes.HasPrefix(inv.TokenHash, prefix) }), nil
}

// prefixEnd is the first blob after every blob starting with prefix, nil
// when there is none.
func prefixEnd(prefix []byte) []byte {
	end := bytes.Clone(prefix)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

// OpenInvites keeps the invites of teamID that are neither used, revoked
// nor expired. The team's list is cleaned as invites are used, but the
// invites themselves decide.
func (r *Repository) OpenInvites(ctx context.Context, teamID gocql.UUID, listed []*models.Invite) ([]*models.Invite, error) {
	open := make(map[string]bool, len(listed))
	now := time.Now()
	for chunk := range slices.Chunk(listed, 100) {
		hashes := make([][]byte, len(chunk))
		for i, inv := range chunk {
			hashes[i] = inv.TokenHash
		}
		var current []*models.Invite
		if err := r.query(ctx, "SELECT "+inviteColumns+" FROM invites WHERE token_hash IN ?", hashes).SelectRelease(&current); err != nil {
			return nil, wrap(err)
		}
		for _, inv := range current {
			if inv.TeamID == teamID && ids.IsZero(inv.UsedBy) && now.Before(inv.ExpiresAt) {
				open[string(inv.TokenHash)] = true
			}
		}
	}
	out := make([]*models.Invite, 0, len(listed))
	for _, inv := range listed {
		if open[string(inv.TokenHash)] {
			out = append(out, inv)
		}
	}
	return out, nil
}

// RevokeInvite deletes an unused invite of teamID, reporting false when it was
// used or is gone already. It leaves the team's list either way.
func (r *Repository) RevokeInvite(ctx context.Context, teamID gocql.UUID, hash []byte) (bool, error) {
	applied, err := r.query(ctx, "DELETE FROM invites WHERE token_hash = ? IF used_by = null AND team_id = ?", hash, teamID).ExecCASRelease()
	if err != nil {
		return false, wrap(err)
	}
	return applied, wrap(r.query(ctx, "DELETE FROM team_invites WHERE team_id = ? AND token_hash = ?", teamID, hash).ExecRelease())
}

// ConsumeInvite marks the invite used by userID, reporting false when someone
// else used it or it has expired. The team_id condition keeps an expired,
// absent row from being recreated by the update.
func (r *Repository) ConsumeInvite(ctx context.Context, inv *models.Invite, userID gocql.UUID, now time.Time) (bool, error) {
	if !now.Before(inv.ExpiresAt) {
		return false, nil
	}
	row := make(map[string]any)
	q := r.query(ctx, "UPDATE invites USING TTL ? SET used_by = ?, used_at = ? WHERE token_hash = ? IF used_by = null AND team_id = ?",
		ttlSeconds(inv.ExpiresAt, now), userID, now, inv.TokenHash, inv.TeamID)
	applied, err := q.MapScanCAS(row)
	q.Release()
	if err != nil {
		return false, wrap(err)
	}
	if applied {
		if err := r.query(ctx, "DELETE FROM team_invites WHERE team_id = ? AND token_hash = ?", inv.TeamID, inv.TokenHash).ExecRelease(); err != nil {
			r.logger.Warn("failed to drop a used invite from its team's list", zap.Error(err))
		}
	}
	return applied || row["used_by"] == userID, nil
}

// SpendInvite deletes an invite userID used to join.
func (r *Repository) SpendInvite(ctx context.Context, hash []byte, userID gocql.UUID) error {
	_, err := r.query(ctx, "DELETE FROM invites WHERE token_hash = ? IF used_by = ?", hash, userID).ExecCASRelease()
	return wrap(err)
}

// ReleaseInvite undoes a consumption whose join could not complete.
func (r *Repository) ReleaseInvite(ctx context.Context, inv *models.Invite, userID gocql.UUID, now time.Time) error {
	applied, err := r.query(ctx, "UPDATE invites USING TTL ? SET used_by = null, used_at = null WHERE token_hash = ? IF used_by = ?",
		ttlSeconds(inv.ExpiresAt, now), inv.TokenHash, userID).ExecCASRelease()
	if err != nil || !applied {
		return wrap(err)
	}
	b := r.batch(ctx, false)
	listTeamInvite(b, inv, now)
	return r.execBatch(b)
}
