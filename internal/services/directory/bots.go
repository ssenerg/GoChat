package directory

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/channels"
	"github.com/ssenerg/gochat/internal/services/convert"
	"github.com/ssenerg/gochat/internal/services/voice"
)

// AddBot adds a bot P owns to a team P may invite people to. The bot joins
// no channel: people add it where they want it.
type AddBot struct {
	P      *models.Principal
	BotID  gocql.UUID
	TeamID string
}

func (w *worker) addBot(ctx context.Context, msg AddBot) (Done, error) {
	teamID, err := parseID(msg.TeamID, "team")
	if err != nil {
		return Done{}, err
	}
	m, err := w.membership(ctx, teamID, msg.P.UserID)
	if err != nil {
		return Done{}, err
	}
	if !w.Authz.Can(authz.ObjInvite, authz.ActCreate, string(m.Role), string(msg.P.Role)) {
		return Done{}, ierrors.PermissionDenied()
	}
	bot, err := w.Users.Get(ctx, msg.BotID)
	if err != nil || !bot.Bot || bot.BotOwner != msg.P.UserID || bot.Deactivated {
		return Done{}, ierrors.NotFound("bot not found")
	}
	if _, err := w.Repo.GetTeamMember(ctx, teamID, bot.ID); err == nil {
		return Done{}, nil
	} else if !ierrors.Is(err, ierrors.CodeNotFound) {
		return Done{}, err
	}
	team, err := w.Repo.GetTeam(ctx, teamID)
	if err != nil {
		return Done{}, err
	}
	member := &models.TeamMember{TeamID: teamID, UserID: bot.ID, Role: models.TeamRoleMember, JoinedAt: time.Now().UTC()}
	if err := w.Repo.AddTeamMember(ctx, member, bot.Username, bot.DisplayName); err != nil {
		return Done{}, err
	}
	w.deliver(&pb.Event{Payload: &pb.Event_TeamJoined{TeamJoined: &pb.TeamJoined{Team: convert.Team(team, models.TeamRoleMember)}}}, bot.ID)
	return Done{}, nil
}

// RemoveBot takes a bot being deleted out of its teams and their
// channels, so no channel keeps writing it updates.
type RemoveBot struct {
	P     *models.Principal
	BotID gocql.UUID
}

func (w *worker) removeBot(ctx context.Context, msg RemoveBot) (Done, error) {
	teams, err := w.Repo.ListUserTeams(ctx, msg.BotID)
	if err != nil {
		return Done{}, err
	}
	for _, m := range teams {
		if err := w.Repo.RemoveTeamMember(ctx, m.TeamID, msg.BotID); err != nil {
			return Done{}, err
		}
		if err := w.leaveChannels(ctx, msg.P, m.TeamID, msg.BotID); err != nil {
			return Done{}, err
		}
	}
	return Done{}, nil
}

// dropBots takes the bots of someone who left a team out of it with them:
// their owner no longer reads it through them.
func (w *worker) dropBots(ctx context.Context, p *models.Principal, teamID, owner gocql.UUID) error {
	bots, err := w.Repo.ListBotIDs(ctx, owner)
	if err != nil {
		return err
	}
	for _, bot := range bots {
		if _, err := w.Repo.GetTeamMember(ctx, teamID, bot); ierrors.Is(err, ierrors.CodeNotFound) {
			continue
		} else if err != nil {
			return err
		}
		if err := w.Repo.RemoveTeamMember(ctx, teamID, bot); err != nil {
			return err
		}
		if err := w.leaveChannels(ctx, p, teamID, bot); err != nil {
			return err
		}
		w.Logger.Info("removed a bot with its owner", zap.String("bot", bot.String()), zap.String("team", teamID.String()))
	}
	return nil
}

// leaveChannels takes target out of the team's channels after it left the
// team, and tells it.
func (w *worker) leaveChannels(ctx context.Context, p *models.Principal, teamID, target gocql.UUID) error {
	memberships, err := w.Repo.ListUserChannels(ctx, target, teamID)
	if err != nil {
		return err
	}
	for _, cm := range memberships {
		req := channels.RemoveMember{Target: channels.Target{P: p, ChannelID: cm.ChannelID}, UserID: target, System: true}
		if _, err := actors.Ask[channels.Done](ctx, w.Engine, w.Channels, req); err != nil {
			return err
		}
	}
	w.Engine.Send(w.Voice, voice.DropUser{UserID: target, TeamID: teamID})
	w.deliver(&pb.Event{Payload: &pb.Event_TeamLeft{TeamLeft: &pb.TeamLeft{TeamId: teamID.String()}}}, target)
	return nil
}
