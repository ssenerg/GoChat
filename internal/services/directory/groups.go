package directory

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/gocql/gocql"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/channels"
	"github.com/ssenerg/gochat/internal/services/convert"
)

// maxGroup bounds the people of a group conversation, its starter included.
const maxGroup = 8

// peopleKey names a conversation by its people's sorted ids.
func peopleKey(people []gocql.UUID) string {
	keys := make([]string, len(people))
	for i, id := range people {
		keys[i] = id.String()
	}
	slices.Sort(keys)
	return strings.Join(keys, ":")
}

func (w *worker) openDirect(ctx context.Context, msg OpenDirect) (*pb.Channel, error) {
	return w.openGroup(ctx, OpenGroup{P: msg.P, TeamID: msg.TeamID, UserIDs: []string{msg.UserID}})
}

// openGroup opens the conversation of the caller and the people they chose
// in a team they all belong to: a direct one with one person, a group one
// with more. The same people find the same conversation in a team, and
// another one in each other team. A bot opens direct conversations with
// people alone, and only so many new ones an hour.
func (w *worker) openGroup(ctx context.Context, msg OpenGroup) (*pb.Channel, error) {
	if !w.Authz.Can(authz.ObjDirect, authz.ActCreate, string(msg.P.Role)) {
		return nil, ierrors.PermissionDenied()
	}
	teamID, err := parseID(msg.TeamID, "team")
	if err != nil {
		return nil, err
	}
	members := []gocql.UUID{msg.P.UserID}
	for _, raw := range msg.UserIDs {
		id, err := parseID(raw, "user")
		if err != nil {
			return nil, err
		}
		if !slices.Contains(members, id) {
			members = append(members, id)
		}
	}
	switch {
	case len(members) == 1:
		return nil, ierrors.InvalidArgument("pick someone to talk to")
	case len(members) > maxGroup:
		return nil, ierrors.InvalidArgument("a group conversation holds at most 8 people")
	case msg.P.Bot && len(members) > 2:
		return nil, ierrors.InvalidArgument("bots are not part of group conversations")
	}
	if _, err := w.membership(ctx, teamID, msg.P.UserID); err != nil {
		return nil, err
	}
	bots := map[gocql.UUID]bool{msg.P.UserID: msg.P.Bot}
	for _, id := range members[1:] {
		bot, err := w.reachable(ctx, teamID, id)
		if err != nil {
			return nil, err
		}
		if bot && msg.P.Bot {
			return nil, ierrors.InvalidArgument("bots do not open conversations with bots")
		}
		bots[id] = bot
		if err := w.mayReach(ctx, msg.P.UserID, id, kindOf(len(members))); err != nil {
			return nil, err
		}
	}
	kind, display := kindOf(len(members)), "Group"
	if kind == models.ChannelDirect {
		display = "Direct"
	}
	// A bot talks with one person at a time.
	if kind == models.ChannelGroup && slices.Contains(slices.Collect(maps.Values(bots)), true) {
		return nil, ierrors.InvalidArgument("bots are not part of group conversations")
	}
	now := time.Now().UTC()
	ch := &models.Channel{
		ID: ids.New(), TeamID: teamID, Name: peopleKey(members), DisplayName: display, Kind: kind,
		CreatedBy: msg.P.UserID, CreatedAt: now,
	}
	cms := make([]*models.ChannelMember, len(members))
	for i, id := range members {
		// A bot sees everything of a conversation it is in.
		cms[i] = &models.ChannelMember{ChannelID: ch.ID, UserID: id, TeamID: teamID, Role: models.ChannelRoleMember, JoinedAt: now, Bot: bots[id], BotSeesAll: bots[id]}
	}
	key := scylla.ConversationKey(teamID, ch.Name)
	if msg.P.Bot {
		if err := w.botMayOpen(ctx, msg.P.UserID, key); err != nil {
			return nil, err
		}
	}
	chID, err := w.Repo.CreateDirectChannel(ctx, key, ch, cms)
	if err != nil {
		return nil, err
	}
	if msg.P.Bot && chID == ch.ID {
		w.botOpened.Add(msg.P.UserID.String())
	}
	target := channels.Target{P: msg.P, ChannelID: chID}
	if chID != ch.ID {
		// Removals from the team took people out of it.
		if _, err := actors.Ask[channels.Done](ctx, w.Engine, w.Channels, channels.Rejoin{Target: target, UserIDs: members}); err != nil {
			return nil, err
		}
	}
	view, err := actors.Ask[*pb.Channel](ctx, w.Engine, w.Channels, channels.View{Target: target})
	if err != nil {
		return nil, err
	}
	if chID == ch.ID {
		if people, err := w.Users.GetMany(ctx, members); err == nil {
			for _, id := range members {
				w.deliver(&pb.Event{Payload: &pb.Event_ChannelJoined{ChannelJoined: &pb.ChannelJoined{
					Channel: convert.Conversation(ch, models.ChannelRoleMember, gocql.UUID{}, id, people),
				}}}, id)
			}
		}
	}
	return view, nil
}

// botMayOpen refuses bot the conversation of key once it opened its new
// ones for the hour; one it has already is free.
func (w *worker) botMayOpen(ctx context.Context, bot gocql.UUID, key string) error {
	if !w.botOpened.Blocked(bot.String()) {
		return nil
	}
	if _, err := w.Repo.DirectKeyHolder(ctx, key); err == nil {
		return nil
	} else if !ierrors.Is(err, ierrors.CodeNotFound) {
		return err
	}
	return ierrors.ResourceExhausted("the bot opened too many conversations this hour, try again later")
}

// kindOf is the kind of a conversation of n people.
func kindOf(n int) models.ChannelKind {
	if n == 2 {
		return models.ChannelDirect
	}
	return models.ChannelGroup
}

// mayReach refuses a conversation with target to someone target blocked,
// in the words a post to them is refused. Who blocked target may open
// their direct conversation, to read it, but not start a group with them.
func (w *worker) mayReach(ctx context.Context, userID, target gocql.UUID, k models.ChannelKind) error {
	if w.Blocks == nil {
		return nil
	}
	if theirs, err := w.Blocks.Blocked(ctx, target, userID); err != nil || theirs {
		if err == nil {
			err = channels.ErrNotTaking
		}
		return err
	}
	if k == models.ChannelDirect {
		return nil
	}
	if mine, err := w.Blocks.Blocked(ctx, userID, target); err != nil || mine {
		if err == nil {
			err = ierrors.FailedPrecondition("you blocked someone you picked: unblock them first")
		}
		return err
	}
	return nil
}

// reachable checks target is an active member of the team.
func (w *worker) reachable(ctx context.Context, teamID, target gocql.UUID) (bool, error) {
	peer, err := w.Users.Get(ctx, target)
	if err != nil || peer.Deactivated {
		return false, ierrors.NotFound("user not found")
	}
	if _, err := w.Repo.GetTeamMember(ctx, teamID, target); err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return false, ierrors.NotFound("user not found")
		}
		return false, err
	}
	return peer.Bot, nil
}
