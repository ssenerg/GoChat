package channels

import (
	"context"
	"slices"

	"github.com/gocql/gocql"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/convert"
	"github.com/ssenerg/gochat/internal/services/push"
)

// posted is the event of a message posted, with whom its thread notifies.
func (a *channel) posted(m *pb.Message, participants []string) *pb.MessagePosted {
	return &pb.MessagePosted{
		Message: m, ParticipantIds: participants,
		TeamId: convert.ID(a.ch.TeamID), Conversation: a.ch.Kind.Conversation(),
	}
}

// push hands a posted message to push, for the closed apps of whom it
// calls for.
func (a *channel) push(m *pb.Message, participants []string) {
	if a.Push == nil {
		return
	}
	name := ""
	if !a.ch.Kind.Conversation() {
		name = a.ch.DisplayName
	}
	a.Engine.Send(a.Push, push.Posted{
		Message: m, TeamID: a.ch.TeamID, Conversation: a.ch.Kind.Conversation(),
		ChannelName: name, Members: a.recipients, Participants: participants,
	})
}

// inTeam reports whether userID belongs to teamID.
func (a *channel) inTeam(ctx context.Context, teamID, userID gocql.UUID) (bool, error) {
	_, err := a.Repo.GetTeamMember(ctx, teamID, userID)
	if ierrors.Is(err, ierrors.CodeNotFound) {
		return false, nil
	}
	return err == nil, err
}

// rejoin brings back the conversation's people a removal from its team took
// out, once they are in the team again.
func (a *channel) rejoin(msg Rejoin) (Done, error) {
	if !a.ch.Kind.Conversation() || ids.IsZero(a.ch.TeamID) {
		return Done{}, nil
	}
	people := convert.PeerIDs(a.ch, gocql.UUID{})
	ctx, cancel := a.ctx()
	defer cancel()
	for _, id := range msg.UserIDs {
		if _, ok := a.members[id]; ok || !slices.Contains(people, id) {
			continue
		}
		in, err := a.inTeam(ctx, a.ch.TeamID, id)
		if err != nil {
			return Done{}, err
		}
		if !in {
			continue
		}
		if err := a.addTeamMember(ctx, id); err != nil {
			if ierrors.Is(err, ierrors.CodeNotFound) {
				continue
			}
			return Done{}, err
		}
		if _, err := a.joined(id, models.ChannelRoleMember); err != nil {
			return Done{}, err
		}
	}
	return Done{}, nil
}

// move gives a conversation of no team to msg.TeamID. Its people outside
// the team leave it; a move cut short is finished by the next.
func (a *channel) move(msg Move) (Done, error) {
	if !a.ch.Kind.Conversation() {
		return Done{}, ierrors.FailedPrecondition("only conversations move")
	}
	if a.ch.TeamID == msg.TeamID {
		return Done{}, nil
	}
	if !ids.IsZero(a.ch.TeamID) {
		return Done{}, ierrors.FailedPrecondition("the conversation belongs to a team")
	}
	ctx, cancel := a.ctx()
	defer cancel()
	members, err := a.Repo.ListChannelMembers(ctx, a.id)
	if err != nil {
		return Done{}, err
	}
	for _, m := range members {
		in, err := a.inTeam(ctx, msg.TeamID, m.UserID)
		if err != nil {
			return Done{}, err
		}
		if in {
			if err := a.Repo.MoveChannelMember(ctx, m, msg.TeamID); err != nil {
				return Done{}, err
			}
			continue
		}
		if err := a.Repo.RemoveChannelMember(ctx, m); err != nil {
			return Done{}, err
		}
		delete(a.members, m.UserID)
		a.snapshot()
		a.left(m.UserID, false)
	}
	if err := a.Repo.SetChannelTeam(ctx, a.id, msg.TeamID); err != nil {
		return Done{}, err
	}
	a.ch.TeamID = msg.TeamID
	return Done{}, nil
}
