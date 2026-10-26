package scylla

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/gocql/gocql"
	"go.uber.org/zap"

	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

const channelColumns = "id, team_id, name, display_name, purpose, kind, created_by, created_at, last_message_id, archived_at, voice_disabled"

// NoTeam is the team of conversations from before teams owned them.
var NoTeam = gocql.UUID{}

type TeamChannel struct {
	TeamID    gocql.UUID         `db:"team_id"`
	ChannelID gocql.UUID         `db:"channel_id"`
	Kind      models.ChannelKind `db:"kind"`
}

type ChannelRead struct {
	ChannelID  gocql.UUID `db:"channel_id"`
	LastReadID gocql.UUID `db:"last_read_id"`
}

// CreateChannel claims the channel's name in its team and writes the channel
// with its first member.
func (r *Repository) CreateChannel(ctx context.Context, ch *models.Channel, creator *models.ChannelMember) error {
	ok, err := r.claim(ctx, "INSERT INTO channel_names (team_id, name, channel_id) VALUES (?, ?, ?) IF NOT EXISTS",
		"SELECT channel_id FROM channel_names WHERE team_id = ? AND name = ?", "channel_id", ch.ID, ch.TeamID, ch.Name)
	if err != nil {
		return err
	}
	if !ok {
		return ierrors.AlreadyExists("a channel with this name exists")
	}
	b := r.batch(ctx, true)
	b.Query("INSERT INTO channels (id, team_id, name, display_name, purpose, kind, created_by, created_at, voice_disabled) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		ch.ID, ch.TeamID, ch.Name, ch.DisplayName, ch.Purpose, string(ch.Kind), ch.CreatedBy, ch.CreatedAt, ch.VoiceDisabled)
	b.Query("INSERT INTO team_channels (team_id, channel_id, kind) VALUES (?, ?, ?)", ch.TeamID, ch.ID, string(ch.Kind))
	if creator != nil {
		addMemberQueries(b, creator)
	}
	// A logged batch that timed out is replayed from the batch log, so its
	// claim stays.
	if err := r.execBatch(b); err != nil {
		r.release(err, "DELETE FROM channel_names WHERE team_id = ? AND name = ? IF channel_id = ?", ch.TeamID, ch.Name, ch.ID)
		return err
	}
	return nil
}

// CreateDirectChannel returns the conversation of key, creating it with
// members when it does not exist yet. A key claimed by a creation that never
// completed is completed with the claimed id.
func (r *Repository) CreateDirectChannel(ctx context.Context, key string, ch *models.Channel, members []*models.ChannelMember) (gocql.UUID, error) {
	id, err := r.ClaimDirectKey(ctx, key, ch.ID)
	if err != nil {
		return gocql.UUID{}, err
	}
	if id != ch.ID {
		if _, err := r.GetChannel(ctx, id); !ierrors.Is(err, ierrors.CodeNotFound) {
			return id, err
		}
		ch.ID = id
		for _, m := range members {
			m.ChannelID = id
		}
	}
	b := r.batch(ctx, true)
	b.Query("INSERT INTO channels (id, team_id, name, display_name, purpose, kind, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		ch.ID, ch.TeamID, ch.Name, ch.DisplayName, ch.Purpose, string(ch.Kind), ch.CreatedBy, ch.CreatedAt)
	for _, m := range members {
		addMemberQueries(b, m)
	}
	if err := r.execBatch(b); err != nil {
		return gocql.UUID{}, err
	}
	return ch.ID, nil
}

// ClaimDirectKey gives key to the conversation id unless another holds it,
// returning the holder. A key is a team's id and the sorted ids of the
// conversation's people; the keys of conversations from before teams owned
// them hold the people alone.
func (r *Repository) ClaimDirectKey(ctx context.Context, key string, id gocql.UUID) (gocql.UUID, error) {
	row := make(map[string]any)
	q := r.query(ctx, "INSERT INTO direct_channels (pair, channel_id) VALUES (?, ?) IF NOT EXISTS", key, id)
	applied, err := q.MapScanCAS(row)
	q.Release()
	if err != nil {
		return gocql.UUID{}, wrap(err)
	}
	if applied {
		return id, nil
	}
	holder, _ := row["channel_id"].(gocql.UUID)
	return holder, nil
}

// ConversationKey is the key of a team's conversation of the people a
// conversation's name lists; those from before teams owned them have the
// people alone.
func ConversationKey(teamID gocql.UUID, people string) string {
	if ids.IsZero(teamID) {
		return people
	}
	return teamID.String() + "/" + people
}

// DeleteDirectKey frees key while channelID holds it.
func (r *Repository) DeleteDirectKey(ctx context.Context, key string, channelID gocql.UUID) error {
	_, err := r.query(ctx, "DELETE FROM direct_channels WHERE pair = ? IF channel_id = ?", key, channelID).ExecCASRelease()
	return wrap(err)
}

// DeleteChannel deletes a channel whose messages and members are gone.
func (r *Repository) DeleteChannel(ctx context.Context, id gocql.UUID) error {
	if err := r.query(ctx, "DELETE FROM message_buckets WHERE channel_id = ?", id).ExecRelease(); err != nil {
		return wrap(err)
	}
	return wrap(r.query(ctx, "DELETE FROM channels WHERE id = ?", id).ExecRelease())
}

// SetChannelTeam gives a conversation to a team.
func (r *Repository) SetChannelTeam(ctx context.Context, id, teamID gocql.UUID) error {
	return wrap(r.query(ctx, "UPDATE channels SET team_id = ? WHERE id = ?", teamID, id).ExecRelease())
}

// MoveChannelMember files m's membership under teamID.
func (r *Repository) MoveChannelMember(ctx context.Context, m *models.ChannelMember, teamID gocql.UUID) error {
	if m.TeamID == teamID {
		return nil
	}
	b := r.batch(ctx, true)
	b.Query("INSERT INTO user_channels (user_id, team_id, channel_id, role, joined_at) VALUES (?, ?, ?, ?, ?)",
		m.UserID, teamID, m.ChannelID, string(m.Role), m.JoinedAt)
	b.Query("DELETE FROM user_channels WHERE user_id = ? AND team_id = ? AND channel_id = ?", m.UserID, m.TeamID, m.ChannelID)
	b.Query("UPDATE channel_members SET team_id = ? WHERE channel_id = ? AND user_id = ?", teamID, m.ChannelID, m.UserID)
	return r.execBatch(b)
}

func (r *Repository) UpdateChannelInfo(ctx context.Context, id gocql.UUID, displayName, purpose string) error {
	return wrap(r.query(ctx, "UPDATE channels SET display_name = ?, purpose = ? WHERE id = ?", displayName, purpose, id).ExecRelease())
}

// RenameChannel moves a channel's URL name in its team to name.
func (r *Repository) RenameChannel(ctx context.Context, ch *models.Channel, name string) error {
	ok, err := r.claim(ctx, "INSERT INTO channel_names (team_id, name, channel_id) VALUES (?, ?, ?) IF NOT EXISTS",
		"SELECT channel_id FROM channel_names WHERE team_id = ? AND name = ?", "channel_id", ch.ID, ch.TeamID, name)
	if err != nil {
		return err
	}
	if !ok {
		return ierrors.AlreadyExists("a channel with this name exists")
	}
	if err := r.query(ctx, "UPDATE channels SET name = ? WHERE id = ?", name, ch.ID).ExecRelease(); err != nil {
		r.release(err, "DELETE FROM channel_names WHERE team_id = ? AND name = ? IF channel_id = ?", ch.TeamID, name, ch.ID)
		return wrap(err)
	}
	// The old name, once free, can name another channel.
	if _, err := r.query(ctx, "DELETE FROM channel_names WHERE team_id = ? AND name = ? IF channel_id = ?", ch.TeamID, ch.Name, ch.ID).ExecCASRelease(); err != nil {
		r.logger.Warn("failed to free a channel's old name", zap.Error(err))
	}
	return nil
}

// SetChannelKind makes a channel public or private.
func (r *Repository) SetChannelKind(ctx context.Context, ch *models.Channel, kind models.ChannelKind) error {
	b := r.batch(ctx, true)
	b.Query("UPDATE channels SET kind = ? WHERE id = ?", string(kind), ch.ID)
	b.Query("INSERT INTO team_channels (team_id, channel_id, kind) VALUES (?, ?, ?)", ch.TeamID, ch.ID, string(kind))
	return r.execBatch(b)
}

// SetArchived archives a channel at at, or with a zero at restores it.
func (r *Repository) SetArchived(ctx context.Context, id gocql.UUID, at time.Time) error {
	var v *time.Time
	if !at.IsZero() {
		v = &at
	}
	return wrap(r.query(ctx, "UPDATE channels SET archived_at = ? WHERE id = ?", v, id).ExecRelease())
}

func addMemberQueries(b *gocql.Batch, m *models.ChannelMember) {
	if m.Bot {
		b.Query("INSERT INTO channel_members (channel_id, user_id, team_id, role, joined_at, bot, bot_sees_all) VALUES (?, ?, ?, ?, ?, ?, ?)",
			m.ChannelID, m.UserID, m.TeamID, string(m.Role), m.JoinedAt, true, m.BotSeesAll)
	} else {
		b.Query("INSERT INTO channel_members (channel_id, user_id, team_id, role, joined_at) VALUES (?, ?, ?, ?, ?)",
			m.ChannelID, m.UserID, m.TeamID, string(m.Role), m.JoinedAt)
	}
	b.Query("INSERT INTO user_channels (user_id, team_id, channel_id, role, joined_at) VALUES (?, ?, ?, ?, ?)",
		m.UserID, m.TeamID, m.ChannelID, string(m.Role), m.JoinedAt)
}

func (r *Repository) GetChannel(ctx context.Context, id gocql.UUID) (*models.Channel, error) {
	var ch models.Channel
	if err := r.query(ctx, "SELECT "+channelColumns+" FROM channels WHERE id = ?", id).GetRelease(&ch); err != nil {
		return nil, wrap(err)
	}
	return &ch, nil
}

// GetChannels returns the channels found among ids, at most 100 per call.
func (r *Repository) GetChannels(ctx context.Context, ids []gocql.UUID) ([]*models.Channel, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > 100 {
		return nil, ierrors.InvalidArgument("too many channels")
	}
	var out []*models.Channel
	err := r.query(ctx, "SELECT "+channelColumns+" FROM channels WHERE id IN ?", ids).SelectRelease(&out)
	return out, wrap(err)
}

// SetVoiceDisabled stops a channel's members posting voice messages, or with
// false lets them again.
func (r *Repository) SetVoiceDisabled(ctx context.Context, channelID gocql.UUID, disabled bool) error {
	return wrap(r.query(ctx, "UPDATE channels SET voice_disabled = ? WHERE id = ?", disabled, channelID).ExecRelease())
}

func (r *Repository) ListTeamChannels(ctx context.Context, teamID gocql.UUID) ([]*TeamChannel, error) {
	var out []*TeamChannel
	err := r.query(ctx, "SELECT team_id, channel_id, kind FROM team_channels WHERE team_id = ?", teamID).SelectRelease(&out)
	return out, wrap(err)
}

func (r *Repository) AddChannelMember(ctx context.Context, m *models.ChannelMember) error {
	b := r.batch(ctx, true)
	addMemberQueries(b, m)
	return r.execBatch(b)
}

func (r *Repository) RemoveChannelMember(ctx context.Context, m *models.ChannelMember) error {
	b := r.batch(ctx, true)
	b.Query("DELETE FROM channel_members WHERE channel_id = ? AND user_id = ?", m.ChannelID, m.UserID)
	b.Query("DELETE FROM user_channels WHERE user_id = ? AND team_id = ? AND channel_id = ?", m.UserID, m.TeamID, m.ChannelID)
	// The read mark stays: a delete now would shadow every later mark,
	// which is written at its message's time.
	return r.execBatch(b)
}

func (r *Repository) ListChannelMembers(ctx context.Context, channelID gocql.UUID) ([]*models.ChannelMember, error) {
	var out []*models.ChannelMember
	err := r.query(ctx, "SELECT channel_id, user_id, team_id, role, joined_at, bot, bot_sees_all FROM channel_members WHERE channel_id = ?", channelID).SelectRelease(&out)
	return out, wrap(err)
}

// ListUserChannels returns the user's memberships in teamID.
func (r *Repository) ListUserChannels(ctx context.Context, userID, teamID gocql.UUID) ([]*models.ChannelMember, error) {
	var out []*models.ChannelMember
	err := r.query(ctx, "SELECT channel_id, user_id, team_id, role, joined_at FROM user_channels WHERE user_id = ? AND team_id = ?", userID, teamID).SelectRelease(&out)
	return out, wrap(err)
}

// The last message and last read are written at their message's time, so the
// newest one wins whatever order the writes arrive in.

func (r *Repository) SetLastMessage(ctx context.Context, channelID, messageID gocql.UUID) error {
	return wrap(r.query(ctx, "UPDATE channels USING TIMESTAMP ? SET last_message_id = ? WHERE id = ?",
		messageID.Time().UnixMicro(), messageID, channelID).ExecRelease())
}

// ResetLastMessage moves the last message back, after the newest was
// deleted; a zero id clears it. at outranks the write it replaces.
func (r *Repository) ResetLastMessage(ctx context.Context, channelID, messageID gocql.UUID, at time.Time) error {
	var id *gocql.UUID
	if !ids.IsZero(messageID) {
		id = &messageID
	}
	return wrap(r.query(ctx, "UPDATE channels USING TIMESTAMP ? SET last_message_id = ? WHERE id = ?",
		at.UnixMicro(), id, channelID).ExecRelease())
}

func (r *Repository) SetLastRead(ctx context.Context, userID, channelID, messageID gocql.UUID) error {
	return wrap(r.query(ctx, "UPDATE channel_reads USING TIMESTAMP ? SET last_read_id = ? WHERE user_id = ? AND channel_id = ?",
		messageID.Time().UnixMicro(), messageID, userID, channelID).ExecRelease())
}

func (r *Repository) ListLastReads(ctx context.Context, userID gocql.UUID) ([]*ChannelRead, error) {
	var out []*ChannelRead
	err := r.query(ctx, "SELECT channel_id, last_read_id FROM channel_reads WHERE user_id = ?", userID).SelectRelease(&out)
	return out, wrap(err)
}

func (r *Repository) GetChannelIDByName(ctx context.Context, teamID gocql.UUID, name string) (gocql.UUID, error) {
	var id gocql.UUID
	if err := r.query(ctx, "SELECT channel_id FROM channel_names WHERE team_id = ? AND name = ?", teamID, name).Scan(&id); err != nil {
		return gocql.UUID{}, wrap(err)
	}
	return id, nil
}

// AddMentions records that messageID calls out each of userIDs.
func (r *Repository) AddMentions(ctx context.Context, channelID, messageID gocql.UUID, userIDs []gocql.UUID) error {
	for chunk := range slices.Chunk(userIDs, 50) {
		b := r.batch(ctx, false)
		for _, id := range chunk {
			b.Query("INSERT INTO unread_mentions (user_id, channel_id, message_id) VALUES (?, ?, ?)", id, channelID, messageID)
		}
		if err := r.execBatch(b); err != nil {
			return err
		}
	}
	return nil
}

// ClearMentions forgets a user's mentions in a channel they read, writing
// nothing when there are none.
func (r *Repository) ClearMentions(ctx context.Context, userID, channelID gocql.UUID) error {
	var id gocql.UUID
	err := r.query(ctx, "SELECT message_id FROM unread_mentions WHERE user_id = ? AND channel_id = ? LIMIT 1", userID, channelID).Scan(&id)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil
	}
	if err != nil {
		return wrap(err)
	}
	return wrap(r.query(ctx, "DELETE FROM unread_mentions WHERE user_id = ? AND channel_id = ?", userID, channelID).ExecRelease())
}

// CountMentions counts a user's unread mentions per channel.
func (r *Repository) CountMentions(ctx context.Context, userID gocql.UUID) (map[gocql.UUID]int, error) {
	out := map[gocql.UUID]int{}
	iter := r.query(ctx, "SELECT channel_id FROM unread_mentions WHERE user_id = ? LIMIT 10000", userID).Iter()
	var id gocql.UUID
	for iter.Scan(&id) {
		out[id]++
	}
	return out, wrap(iter.Close())
}

func (r *Repository) SetMuted(ctx context.Context, userID, channelID gocql.UUID, muted bool) error {
	if !muted {
		return wrap(r.query(ctx, "DELETE muted FROM channel_prefs WHERE user_id = ? AND channel_id = ?", userID, channelID).ExecRelease())
	}
	return wrap(r.query(ctx, "INSERT INTO channel_prefs (user_id, channel_id, muted) VALUES (?, ?, true)", userID, channelID).ExecRelease())
}

func (r *Repository) IsMuted(ctx context.Context, userID, channelID gocql.UUID) (bool, error) {
	var muted bool
	err := r.query(ctx, "SELECT muted FROM channel_prefs WHERE user_id = ? AND channel_id = ?", userID, channelID).Scan(&muted)
	if errors.Is(err, gocql.ErrNotFound) {
		return false, nil
	}
	return muted, wrap(err)
}

// SetCleared hides a conversation's messages up to clearedID from a user.
func (r *Repository) SetCleared(ctx context.Context, userID, channelID, clearedID gocql.UUID) error {
	return wrap(r.query(ctx, "UPDATE channel_prefs SET cleared_id = ? WHERE user_id = ? AND channel_id = ?", clearedID, userID, channelID).ExecRelease())
}

// GetCleared returns up to which message a user cleared a conversation,
// zero for none.
func (r *Repository) GetCleared(ctx context.Context, userID, channelID gocql.UUID) (gocql.UUID, error) {
	var id gocql.UUID
	err := r.query(ctx, "SELECT cleared_id FROM channel_prefs WHERE user_id = ? AND channel_id = ?", userID, channelID).Scan(&id)
	if errors.Is(err, gocql.ErrNotFound) {
		return gocql.UUID{}, nil
	}
	return id, wrap(err)
}

// ListCleared returns up to which message a user cleared each conversation
// they cleared.
func (r *Repository) ListCleared(ctx context.Context, userID gocql.UUID) (map[gocql.UUID]gocql.UUID, error) {
	out := map[gocql.UUID]gocql.UUID{}
	iter := r.query(ctx, "SELECT channel_id, cleared_id FROM channel_prefs WHERE user_id = ?", userID).Iter()
	var channelID, clearedID gocql.UUID
	for iter.Scan(&channelID, &clearedID) {
		if !ids.IsZero(clearedID) {
			out[channelID] = clearedID
		}
		clearedID = gocql.UUID{}
	}
	return out, wrap(iter.Close())
}

// ListMuted returns the channels a user muted.
func (r *Repository) ListMuted(ctx context.Context, userID gocql.UUID) (map[gocql.UUID]bool, error) {
	out := map[gocql.UUID]bool{}
	iter := r.query(ctx, "SELECT channel_id, muted FROM channel_prefs WHERE user_id = ?", userID).Iter()
	var id gocql.UUID
	var muted bool
	for iter.Scan(&id, &muted) {
		if muted {
			out[id] = true
		}
	}
	return out, wrap(iter.Close())
}
