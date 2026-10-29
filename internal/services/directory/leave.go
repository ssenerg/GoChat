package directory

import (
	"context"
	"slices"
	"time"

	"github.com/gocql/gocql"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/channels"
	"github.com/ssenerg/gochat/internal/services/convert"
)

// LeaveTeam takes P out of a team, as a removal would.
type LeaveTeam struct {
	P      *models.Principal
	TeamID string
}

// LeaveTeams takes P out of every team and channel, for an account being
// deleted, replying with the []gocql.UUID of the members of the teams it
// left. With Check it leaves none and fails as leaving would.
type LeaveTeams struct {
	P     *models.Principal
	Check bool
}

func (w *worker) leaveTeam(ctx context.Context, msg LeaveTeam) (Done, error) {
	if msg.P.Bot {
		return Done{}, ierrors.PermissionDenied()
	}
	teamID, err := parseID(msg.TeamID, "team")
	if err != nil {
		return Done{}, err
	}
	if err := w.quit(ctx, msg.P, teamID, "before you leave"); err != nil {
		return Done{}, err
	}
	w.Logger.Info("left a team", zap.String("team", teamID.String()), zap.String("user", msg.P.UserID.String()))
	// Out of the team, they can no longer finish this themselves.
	if _, err := w.afterLeaving(ctx, msg.P, teamID, msg.P.UserID); err != nil {
		w.Logger.Warn("failed to take a leaver out of the team's channels", zap.String("team", teamID.String()), zap.Error(err))
		go w.finishLeaving(msg.P, teamID)
	}
	return Done{}, nil
}

// finishLeaving retries afterLeaving for a leave that stopped short.
func (w *worker) finishLeaving(p *models.Principal, teamID gocql.UUID) {
	for _, wait := range leaveRetries {
		time.Sleep(wait)
		ctx, cancel := context.WithTimeout(context.Background(), w.Cfg.OpTimeout)
		_, err := w.afterLeaving(ctx, p, teamID, p.UserID)
		cancel()
		if err == nil {
			return
		}
		w.Logger.Warn("failed to take a leaver out of the team's channels", zap.String("team", teamID.String()), zap.Error(err))
	}
}

var leaveRetries = []time.Duration{time.Second, 10 * time.Second, time.Minute}

const beforeDeleting = "before you delete your account"

// leaveAllTimeout bounds leaving every team, more than one operation's.
const leaveAllTimeout = 90 * time.Second

func (w *worker) leaveTeams(ctx context.Context, msg LeaveTeams) ([]gocql.UUID, error) {
	teams, err := w.Repo.ListUserTeams(ctx, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	if msg.Check {
		for _, m := range teams {
			if _, err := w.successor(ctx, m.TeamID, m, beforeDeleting); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaveAllTimeout)
	defer cancel()
	seen := map[gocql.UUID]bool{}
	var stayed []gocql.UUID
	for _, m := range teams {
		err := w.quit(ctx, msg.P, m.TeamID, beforeDeleting)
		if ierrors.Is(err, ierrors.CodeNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		members, err := w.afterLeaving(ctx, msg.P, m.TeamID, msg.P.UserID)
		if err != nil {
			return nil, err
		}
		for _, id := range members {
			if !seen[id] {
				seen[id] = true
				stayed = append(stayed, id)
			}
		}
	}
	// What a leave that stopped short left, and conversations of no team.
	rest, err := w.Repo.ListAllUserChannels(ctx, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	for _, cm := range rest {
		req := channels.RemoveMember{Target: channels.Target{P: msg.P, ChannelID: cm.ChannelID}, UserID: msg.P.UserID, System: true}
		if _, err := actors.Ask[channels.Done](ctx, w.Engine, w.Channels, req); err != nil {
			return nil, err
		}
	}
	return stayed, nil
}

// quit takes p out of the team under its lock. A leave that stopped short
// of the channels may be finished.
func (w *worker) quit(ctx context.Context, p *models.Principal, teamID gocql.UUID, before string) error {
	unlock, err := w.lockTeam(ctx, teamID)
	if err != nil {
		return err
	}
	defer unlock()
	m, err := w.membership(ctx, teamID, p.UserID)
	if ierrors.Is(err, ierrors.CodeNotFound) {
		in, err := w.Repo.ListUserChannels(ctx, p.UserID, teamID)
		if err != nil {
			return err
		}
		if len(in) == 0 {
			return errNoTeam
		}
		return nil
	}
	if err != nil {
		return err
	}
	heir, err := w.successor(ctx, teamID, m, before)
	if err != nil {
		return err
	}
	if err := w.handOver(ctx, teamID, p.UserID, heir); err != nil {
		return err
	}
	return w.Repo.RemoveTeamMember(ctx, teamID, p.UserID)
}

// successor checks m may leave its team, under the team's lock: its last
// active admin only once no one active is in it, its own bots aside. It
// returns the active admin who joined first of those who stay, nil for
// none.
func (w *worker) successor(ctx context.Context, teamID gocql.UUID, m *models.TeamMember, before string) (*scylla.TeamMemberEntry, error) {
	entries, err := w.Repo.ListTeamMembers(ctx, teamID)
	if err != nil {
		return nil, err
	}
	bots, err := w.Repo.ListBotIDs(ctx, m.UserID)
	if err != nil {
		return nil, err
	}
	var admins []*scylla.TeamMemberEntry
	var others []gocql.UUID
	for _, e := range entries {
		if e.UserID == m.UserID || slices.Contains(bots, e.UserID) {
			continue
		}
		others = append(others, e.UserID)
		if e.Role == models.TeamRoleAdmin {
			admins = append(admins, e)
		}
	}
	slices.SortFunc(admins, func(a, b *scylla.TeamMemberEntry) int { return a.JoinedAt.Compare(b.JoinedAt) })
	var heir *scylla.TeamMemberEntry
	for _, e := range admins {
		u, err := w.Users.Get(ctx, e.UserID)
		if err != nil {
			return nil, err
		}
		if !u.Deactivated {
			heir = e
			break
		}
	}
	if m.Role == models.TeamRoleAdmin && heir == nil && len(others) > 0 {
		// Rare: the last active admin, so whether anyone active stays.
		people, err := w.Users.GetMany(ctx, others)
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(others, func(id gocql.UUID) bool { u := people[id]; return u != nil && !u.Deactivated }) {
			return nil, nil
		}
		team, err := w.Repo.GetTeam(ctx, teamID)
		if err != nil {
			return nil, err
		}
		return nil, ierrors.FailedPrecondition("you are the only admin of " + team.DisplayName + ": make another member an admin " + before)
	}
	return heir, nil
}

// handOver makes heir the team's owner when userID, its owner, leaves; the
// owner alone may change other admins.
func (w *worker) handOver(ctx context.Context, teamID, userID gocql.UUID, heir *scylla.TeamMemberEntry) error {
	if heir == nil {
		return nil
	}
	team, err := w.Repo.GetTeam(ctx, teamID)
	if err != nil {
		return err
	}
	if team.CreatedBy != userID {
		return nil
	}
	if err := w.Repo.SetTeamOwner(ctx, teamID, heir.UserID); err != nil {
		return err
	}
	team.CreatedBy = heir.UserID
	w.deliver(&pb.Event{Payload: &pb.Event_TeamJoined{TeamJoined: &pb.TeamJoined{Team: convert.Team(team, heir.Role)}}}, heir.UserID)
	w.Logger.Info("team handed over", zap.String("team", teamID.String()), zap.String("to", heir.UserID.String()))
	return nil
}

// afterLeaving takes target, out of the team now, out of its channels and
// conversations with its bots, and tells the members who stay, whom it
// returns.
func (w *worker) afterLeaving(ctx context.Context, p *models.Principal, teamID, target gocql.UUID) ([]gocql.UUID, error) {
	// The channels go after the team: a join racing the removal then finds
	// no team, or joined in time to be listed here.
	if err := w.leaveChannels(ctx, p, teamID, target); err != nil {
		return nil, err
	}
	if err := w.dropBots(ctx, p, teamID, target); err != nil {
		return nil, err
	}
	go w.recheckWatches(context.WithoutCancel(ctx), target)
	members, err := w.Repo.ListTeamMembers(ctx, teamID)
	if err != nil {
		w.Logger.Warn("failed to tell a team someone left", zap.String("team", teamID.String()), zap.Error(err))
		return nil, nil
	}
	to := make([]gocql.UUID, len(members))
	for i, m := range members {
		to[i] = m.UserID
	}
	ev := &pb.TeamMembersChanged{TeamId: teamID.String(), UserId: target.String()}
	w.deliver(&pb.Event{Payload: &pb.Event_TeamMembersChanged{TeamMembersChanged: ev}}, to...)
	return to, nil
}
