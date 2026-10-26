// Package channels runs one actor per active channel. A channel's actor owns
// its member list and serializes the writes to it, so messages get their
// order and every permission check sees the membership it acts on.
package channels

import (
	"context"
	"strconv"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"

	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/updates"
	"github.com/ssenerg/gochat/internal/services/users"
)

type Repo interface {
	GetChannel(ctx context.Context, id gocql.UUID) (*models.Channel, error)
	UpdateChannelInfo(ctx context.Context, id gocql.UUID, displayName, purpose string) error
	RenameChannel(ctx context.Context, ch *models.Channel, name string) error
	SetChannelKind(ctx context.Context, ch *models.Channel, kind models.ChannelKind) error
	SetArchived(ctx context.Context, id gocql.UUID, at time.Time) error
	TouchThread(ctx context.Context, followers []gocql.UUID, channelID, rootID, teamID gocql.UUID, at time.Time, reader gocql.UUID) error
	MarkThreadRead(ctx context.Context, userID, channelID, rootID gocql.UUID, at time.Time) error
	GetThreadFollow(ctx context.Context, userID, channelID, rootID gocql.UUID) (bool, error)
	UnfollowThreads(ctx context.Context, userIDs []gocql.UUID, channelID, rootID gocql.UUID) error
	SetPinned(ctx context.Context, m *models.Message, pinned bool, by gocql.UUID) error
	Unpin(ctx context.Context, channelID, messageID gocql.UUID) error
	ListPins(ctx context.Context, channelID gocql.UUID, limit int) ([]*scylla.Pin, error)
	SetSaved(ctx context.Context, userID gocql.UUID, m *models.Message, teamID gocql.UUID, saved bool) error
	SavedAmong(ctx context.Context, userID gocql.UUID, messageIDs []gocql.UUID) (map[gocql.UUID]bool, error)
	ListChannelMembers(ctx context.Context, channelID gocql.UUID) ([]*models.ChannelMember, error)
	AddChannelMember(ctx context.Context, m *models.ChannelMember) error
	RemoveChannelMember(ctx context.Context, m *models.ChannelMember) error
	MoveChannelMember(ctx context.Context, m *models.ChannelMember, teamID gocql.UUID) error
	SetChannelTeam(ctx context.Context, id, teamID gocql.UUID) error
	SetCleared(ctx context.Context, userID, channelID, clearedID gocql.UUID) error
	GetCleared(ctx context.Context, userID, channelID gocql.UUID) (gocql.UUID, error)
	DeleteDirectKey(ctx context.Context, key string, channelID gocql.UUID) error
	DeleteChannel(ctx context.Context, id gocql.UUID) error
	GetTeamMember(ctx context.Context, teamID, userID gocql.UUID) (*models.TeamMember, error)
	InsertMessage(ctx context.Context, m *models.Message, newBucket bool) error
	GetMessage(ctx context.Context, channelID, id gocql.UUID) (*models.Message, error)
	UpdateMessageContent(ctx context.Context, channelID, id gocql.UUID, content string, at time.Time) error
	DeleteMessage(ctx context.Context, channelID, id gocql.UUID) error
	InsertReply(ctx context.Context, m *models.Message) error
	GetReply(ctx context.Context, channelID, rootID, id gocql.UUID) (*models.Message, error)
	UpdateReplyContent(ctx context.Context, channelID, rootID, id gocql.UUID, content string, at time.Time) error
	DeleteReply(ctx context.Context, channelID, rootID, id gocql.UUID) error
	ListReplies(ctx context.Context, channelID, rootID, before gocql.UUID, limit int) ([]*models.Message, bool, error)
	ThreadContents(ctx context.Context, channelID, rootID gocql.UUID) ([]*models.Message, error)
	AddReaction(ctx context.Context, channelID, messageID gocql.UUID, emoji string, userID gocql.UUID) error
	RemoveReaction(ctx context.Context, channelID, messageID gocql.UUID, emoji string, userID gocql.UUID) error
	DeleteReactions(ctx context.Context, channelID, messageID gocql.UUID) error
	ListReactions(ctx context.Context, channelID gocql.UUID, messageIDs []gocql.UUID) ([]*scylla.Reaction, error)
	DeleteThread(ctx context.Context, channelID, rootID gocql.UUID) error
	SetThreadSummary(ctx context.Context, root *models.Message, count int, last time.Time, participant gocql.UUID) error
	ListMessages(ctx context.Context, channelID, before gocql.UUID, limit int) ([]*models.Message, bool, error)
	ListMessagesAfter(ctx context.Context, channelID, after gocql.UUID, limit int) ([]*models.Message, bool, error)
	LastMessageBucket(ctx context.Context, channelID gocql.UUID) (int32, bool, error)
	SetLastMessage(ctx context.Context, channelID, messageID gocql.UUID) error
	ResetLastMessage(ctx context.Context, channelID, messageID gocql.UUID, at time.Time) error
	SetLastRead(ctx context.Context, userID, channelID, messageID gocql.UUID) error
	GetUserIDByUsername(ctx context.Context, username string) (gocql.UUID, error)
	AddMentions(ctx context.Context, channelID, messageID gocql.UUID, userIDs []gocql.UUID) error
	ClearMentions(ctx context.Context, userID, channelID gocql.UUID) error
	SetMuted(ctx context.Context, userID, channelID gocql.UUID, muted bool) error
	SetVoiceDisabled(ctx context.Context, channelID gocql.UUID, disabled bool) error
	IsMuted(ctx context.Context, userID, channelID gocql.UUID) (bool, error)
	ListLastReads(ctx context.Context, userID gocql.UUID) ([]*scylla.ChannelRead, error)
	GetAttachments(ctx context.Context, ids []gocql.UUID) ([]*models.Attachment, error)
	ClaimAttachment(ctx context.Context, a *models.Attachment, messageID gocql.UUID) (bool, error)
	UnclaimAttachment(ctx context.Context, a *models.Attachment, messageID gocql.UUID) error
	DeleteAttachment(ctx context.Context, a *models.Attachment) error
	PendObject(ctx context.Context, id gocql.UUID, key string, at time.Time) error
	ChannelPts(ctx context.Context, channelID gocql.UUID) (int64, error)
	GetBot(ctx context.Context, id gocql.UUID) (*models.Bot, error)
	SetMarkup(ctx context.Context, channelID, rootID, id gocql.UUID, markup []byte) error
	AppendChannelUpdate(ctx context.Context, channelID gocql.UUID, e *scylla.LogEntry, stamp int64, ttl time.Duration) error
	ChannelUpdates(ctx context.Context, channelID gocql.UUID, after, upTo int64, limit int) ([]*scylla.LogEntry, error)
	DeleteChannelUpdates(ctx context.Context, channelID gocql.UUID, upTo int64) error
}

type Storage interface {
	Delete(ctx context.Context, key string) error
}

type Deps struct {
	Logger  *zap.Logger
	Cfg     *Config
	Repo    Repo
	Storage Storage
	Authz   *authz.Authorizer
	Users   *users.Cache
	Engine  *actor.Engine
	Hub     *actor.PID
	// Search files messages for search; nil leaves them out.
	Search *actor.PID
	// Updates numbers what happens to each account.
	Updates    *actor.PID
	UpdatesCfg *updates.Config
	// Callbacks holds the button presses bots have yet to answer.
	Callbacks *actor.PID
}

// passivate asks to stop an idle channel actor that has handled requests.
type passivate struct {
	id      gocql.UUID
	pid     *actor.PID
	handled uint64
}

type child struct {
	pid    *actor.PID
	routed uint64
}

type manager struct {
	*Deps
	children map[gocql.UUID]*child
	gen      uint64
}

func New(d *Deps) actor.Producer {
	return func() actor.Receiver {
		return &manager{Deps: d, children: make(map[gocql.UUID]*child)}
	}
}

func (m *manager) Receive(c *actor.Context) {
	switch msg := c.Message().(type) {
	case passivate:
		// A child stops only once it has handled every request routed to it;
		// a successor must never run beside it.
		if ch, ok := m.children[msg.id]; ok && ch.pid.Equals(msg.pid) && ch.routed == msg.handled {
			delete(m.children, msg.id)
			c.Engine().Poison(ch.pid)
		}
	case Request:
		id := msg.channel()
		ch, ok := m.children[id]
		// A child that exhausted its restarts is gone from the context.
		if !ok || c.Child(ch.pid.ID) == nil {
			m.gen++
			ch = &child{pid: c.SpawnChild(newChannel(m.Deps, id), "channel",
				actor.WithID(id.String()+"."+strconv.FormatUint(m.gen, 36)),
				actor.WithInboxSize(256),
				actor.WithMiddleware(actors.Recover(m.Logger)))}
			m.children[id] = ch
		}
		ch.routed++
		c.Engine().SendWithSender(ch.pid, msg, c.Sender())
	}
}
