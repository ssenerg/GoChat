package channels

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/convert"
	"github.com/ssenerg/gochat/internal/services/index"
)

// purgeTimeout bounds deleting an erased conversation's messages.
const purgeTimeout = 10 * time.Minute

// shown reports whether m is newer than what its reader cleared.
func shown(m *models.Message, cleared gocql.UUID) bool {
	root := m.ID
	if !ids.IsZero(m.RootID) {
		root = m.RootID
	}
	return ids.IsZero(cleared) || ids.After(root, cleared)
}

// reappear lists a direct conversation again for those who cleared it,
// once a message follows prev, the last they cleared or saw hidden.
func (a *channel) reappear(prev gocql.UUID) {
	for id, cleared := range a.cleared {
		role, ok := a.members[id]
		if !ok || ids.After(prev, cleared) {
			continue
		}
		if _, err := a.joined(id, role); err != nil {
			a.Logger.Warn("failed to list a cleared conversation again", zap.String("channel", a.id.String()), zap.Error(err))
		}
	}
}

// deleteConversation clears the caller's copy of a direct conversation:
// they see none of it until its next message. With Everyone it deletes the
// conversation for both.
func (a *channel) deleteConversation(msg DeleteConversation) (Done, error) {
	if _, err := a.member(msg.P); err != nil {
		return Done{}, err
	}
	if a.ch.Kind != models.ChannelDirect {
		return Done{}, ierrors.FailedPrecondition("only direct conversations can be deleted")
	}
	ctx, cancel := a.ctx()
	defer cancel()
	if msg.Everyone {
		return Done{}, a.erase(ctx, msg.P.UserID)
	}
	cleared := ids.NewTime()
	if err := a.Repo.SetCleared(ctx, msg.P.UserID, a.id, cleared); err != nil {
		return Done{}, err
	}
	a.cleared[msg.P.UserID] = cleared
	if err := a.Repo.ClearMentions(ctx, msg.P.UserID, a.id); err != nil {
		a.Logger.Warn("failed to clear a conversation's mentions", zap.String("channel", a.id.String()), zap.Error(err))
	}
	a.gone(msg.P.UserID, true)
	return Done{}, nil
}

// gone tells userID the conversation is deleted for them.
func (a *channel) gone(userID gocql.UUID, own bool) {
	a.deliver(userID, &pb.Event{Payload: &pb.Event_ChannelLeft{ChannelLeft: &pb.ChannelLeft{
		ChannelId: a.id.String(), TeamId: convert.ID(a.ch.TeamID), Own: own, Deleted: true,
	}}})
}

// erase deletes the conversation for both people: they leave it at once,
// opening it again starts a new one, and its messages go in the
// background.
func (a *channel) erase(ctx context.Context, caller gocql.UUID) error {
	if err := a.Repo.DeleteDirectKey(ctx, scylla.ConversationKey(a.ch.TeamID, a.ch.Name), a.id); err != nil {
		return err
	}
	members, err := a.Repo.ListChannelMembers(ctx, a.id)
	if err != nil {
		return err
	}
	for _, m := range members {
		if err := a.Repo.RemoveChannelMember(ctx, m); err != nil {
			return err
		}
		delete(a.members, m.UserID)
		a.gone(m.UserID, m.UserID == caller)
	}
	a.snapshot()
	people := convert.PeerIDs(a.ch, gocql.UUID{})
	// The purge outlives the request.
	go a.purge(context.WithoutCancel(ctx), people, a.pts)
	return nil
}

// purge deletes an erased conversation's messages, threads, reactions,
// pins, files and search entries, then the conversation. It runs off the
// actor and touches none of its state.
func (a *channel) purge(ctx context.Context, people []gocql.UUID, pts int64) {
	ctx, cancel := context.WithTimeout(ctx, purgeTimeout)
	defer cancel()
	if err := a.purgeAll(ctx, people, pts); err != nil {
		a.Logger.Error("failed to delete an erased conversation", zap.String("channel", a.id.String()), zap.Error(err))
	}
}

func (a *channel) purgeAll(ctx context.Context, people []gocql.UUID, pts int64) error {
	// A conversation's messages are filed for search under each of its
	// people.
	drop := func(m *models.Message) []gocql.UUID {
		a.file(index.Remove{Doc: index.Doc{
			Scopes:  people,
			Posting: scylla.Posting{MessageID: m.ID, ChannelID: a.id, RootID: m.RootID, UserID: m.UserID},
			Content: m.Content,
		}})
		if err := a.Repo.DeleteReactions(ctx, a.id, m.ID); err != nil {
			a.Logger.Warn("failed to delete reactions", zap.String("channel", a.id.String()), zap.Error(err))
		}
		return m.Attachments
	}
	pins, err := a.Repo.ListPins(ctx, a.id, maxPins)
	if err != nil {
		return err
	}
	for _, p := range pins {
		if err := a.Repo.Unpin(ctx, a.id, p.MessageID); err != nil {
			return err
		}
	}
	var before gocql.UUID
	for {
		list, more, err := a.Repo.ListMessages(ctx, a.id, before, 100)
		if err != nil {
			return err
		}
		var atts []gocql.UUID
		for _, m := range list {
			if m.ReplyCount > 0 {
				replies, err := a.Repo.ThreadContents(ctx, a.id, m.ID)
				if err != nil {
					return err
				}
				for _, r := range replies {
					atts = append(atts, drop(r)...)
				}
				if err := a.Repo.DeleteThread(ctx, a.id, m.ID); err != nil {
					return err
				}
				if err := a.Repo.UnfollowThreads(ctx, people, a.id, m.ID); err != nil {
					return err
				}
			}
			atts = append(atts, drop(m)...)
			if err := a.Repo.DeleteMessage(ctx, a.id, m.ID); err != nil {
				return err
			}
		}
		a.dropAttachments(atts)
		if !more || len(list) == 0 {
			break
		}
		before = list[len(list)-1].ID
	}
	for _, id := range people {
		if err := a.Repo.ClearMentions(ctx, id, a.id); err != nil {
			return err
		}
	}
	// The log would replay the conversation's deletions, and its changes.
	if err := a.Repo.DeleteChannelUpdates(ctx, a.id, pts); err != nil {
		return err
	}
	return a.Repo.DeleteChannel(ctx, a.id)
}
