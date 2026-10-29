// Package safety keeps people apart from those they block, and takes the
// reports people make of messages and people to the admins who act on
// them. It runs as a pool of workers.
package safety

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/keylock"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/window"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/blocks"
	"github.com/ssenerg/gochat/internal/services/convert"
	"github.com/ssenerg/gochat/internal/services/hub"
	"github.com/ssenerg/gochat/internal/services/updates"
	"github.com/ssenerg/gochat/internal/services/users"
)

type Repo interface {
	Block(ctx context.Context, b *models.Block) error
	Unblock(ctx context.Context, userID, blockedID gocql.UUID) error
	ListBlocks(ctx context.Context, userID gocql.UUID, limit int) ([]*models.Block, error)
	CopyBlocks(ctx context.Context) (int, error)
	GetState(ctx context.Context, key string) (string, error)
	SetState(ctx context.Context, key, value string) error

	CreateReport(ctx context.Context, rep *models.Report) error
	GetReport(ctx context.Context, id gocql.UUID) (*models.Report, error)
	ListReports(ctx context.Context, scope gocql.UUID, open bool, after gocql.UUID, limit int) ([]*models.Report, error)
	CountOpenReports(ctx context.Context, scope gocql.UUID, limit int) (int, map[gocql.UUID]int, error)
	ClaimReport(ctx context.Context, rep *models.Report, by gocql.UUID, at time.Time) (bool, error)
	FinishReport(ctx context.Context, rep *models.Report, action string) (bool, error)
	ReleaseReport(ctx context.Context, rep *models.Report) error
	IndexResolved(ctx context.Context, rep *models.Report) error

	GetTeamMember(ctx context.Context, teamID, userID gocql.UUID) (*models.TeamMember, error)
	ListTeamMembers(ctx context.Context, teamID gocql.UUID) ([]*scylla.TeamMemberEntry, error)
	ListUserTeams(ctx context.Context, userID gocql.UUID) ([]*models.TeamMember, error)
	GetTeams(ctx context.Context, ids []gocql.UUID) ([]*models.Team, error)
	GetChannels(ctx context.Context, ids []gocql.UUID) ([]*models.Channel, error)
}

type Deps struct {
	Logger *zap.Logger
	Cfg    *Config
	Repo   Repo
	Authz  *authz.Authorizer
	Users  *users.Cache
	Blocks *blocks.Cache
	Engine *actor.Engine
	Hub    *actor.PID
	// Updates numbers what happens to each account; Channels, Directory
	// and Accounts carry out what admins do about reports.
	Updates   *actor.PID
	Channels  *actor.PID
	Directory *actor.PID
	Accounts  *actor.PID
	reported  *window.Counter
	// locks makes one person's blocks change one at a time.
	locks *keylock.Locks
}

func Pool(d *Deps) actor.Producer {
	d.reported = window.New(d.Cfg.MaxReports, d.Cfg.ReportWindow)
	d.locks = keylock.New()
	return actors.NewPool(d.Cfg.Workers, func() actor.Receiver { return &worker{Deps: d} },
		actor.WithMiddleware(actors.Recover(d.Logger)))
}

type worker struct{ *Deps }

func (w *worker) Receive(c *actor.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), w.Cfg.OpTimeout)
	defer cancel()
	var (
		out any
		err error
	)
	switch msg := c.Message().(type) {
	case Block:
		out, err = w.block(ctx, msg)
	case ListBlocked:
		out, err = w.listBlocked(ctx, msg.P)
	case Report:
		out, err = w.report(ctx, msg)
	case ListReports:
		out, err = w.listReports(ctx, msg)
	case CountReports:
		out, err = w.countReports(ctx, msg.P)
	case Resolve:
		out, err = w.resolve(ctx, msg)
	default:
		return
	}
	if err != nil {
		if e := ierrors.As(err); e.Code == ierrors.CodeInternal {
			w.Logger.Error("safety request failed", e.Fields()...)
		}
		actors.Reply(c, err)
		return
	}
	actors.Reply(c, out)
}

func parseID(s, what string) (gocql.UUID, error) {
	id, ok := ids.Parse(s)
	if !ok {
		return gocql.UUID{}, ierrors.InvalidArgument("invalid " + what + " id")
	}
	return id, nil
}

// parseTimeID parses the timeuuid of a report.
func parseTimeID(s, what string) (gocql.UUID, error) {
	id, ok := ids.ParseTime(s)
	if !ok {
		return gocql.UUID{}, ierrors.InvalidArgument("invalid " + what + " id")
	}
	return id, nil
}

// deliver numbers ev in the updates of userID and sends it to them.
func (w *worker) deliver(ev *pb.Event, userID gocql.UUID) {
	if w.Updates == nil {
		w.Engine.Send(w.Hub, hub.Deliver{UserIDs: []string{userID.String()}, Event: ev})
		return
	}
	w.Engine.Send(w.Updates, updates.Push{UserIDs: []gocql.UUID{userID}, Event: ev})
}

func (w *worker) block(ctx context.Context, msg Block) (Done, error) {
	target, err := parseID(msg.UserID, "user")
	if err != nil {
		return Done{}, err
	}
	if target == msg.P.UserID {
		return Done{}, ierrors.InvalidArgument("you cannot block yourself")
	}
	u, err := w.Users.Get(ctx, target)
	if ierrors.Is(err, ierrors.CodeNotFound) {
		return Done{}, ierrors.NotFound("user not found")
	}
	if err != nil {
		return Done{}, err
	}
	unlock, err := w.locks.Lock(ctx, msg.P.UserID.String())
	if err != nil {
		return Done{}, ierrors.Unavailable("busy, try again")
	}
	defer unlock()
	// Written even when it seems done already: the cache may be stale.
	if msg.Blocked {
		list, err := w.Repo.ListBlocks(ctx, msg.P.UserID, w.Cfg.MaxBlocks)
		if err != nil {
			return Done{}, err
		}
		if len(list) >= w.Cfg.MaxBlocks && !slices.ContainsFunc(list, func(b *models.Block) bool { return b.BlockedID == target }) {
			return Done{}, ierrors.ResourceExhausted(fmt.Sprintf("you can block up to %d people: unblock someone first", w.Cfg.MaxBlocks))
		}
		err = w.Repo.Block(ctx, &models.Block{UserID: msg.P.UserID, BlockedID: target, CreatedAt: time.Now().UTC()})
	} else {
		err = w.Repo.Unblock(ctx, msg.P.UserID, target)
	}
	w.Blocks.Invalidate(msg.P.UserID, target)
	if err != nil {
		return Done{}, err
	}
	w.deliver(&pb.Event{Payload: &pb.Event_BlocksChanged{BlocksChanged: &pb.BlocksChanged{
		User: convert.PublicUser(u), Blocked: msg.Blocked,
	}}}, msg.P.UserID)
	return Done{}, nil
}

func (w *worker) listBlocked(ctx context.Context, p *models.Principal) ([]*pb.BlockedUser, error) {
	list, err := w.Repo.ListBlocks(ctx, p.UserID, w.Cfg.MaxBlocks)
	if err != nil {
		return nil, err
	}
	userIDs := make([]gocql.UUID, len(list))
	for i, b := range list {
		userIDs[i] = b.BlockedID
	}
	people, err := w.Users.GetMany(ctx, userIDs)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(list, func(a, b *models.Block) int { return b.CreatedAt.Compare(a.CreatedAt) })
	out := make([]*pb.BlockedUser, 0, len(list))
	for _, b := range list {
		if u := people[b.BlockedID]; u != nil {
			out = append(out, &pb.BlockedUser{User: convert.PublicUser(u), BlockedAt: convert.Time(b.CreatedAt)})
		}
	}
	return out, nil
}

// teamAdmins are the people who act on a team's reports, bots left out.
func (w *worker) teamAdmins(ctx context.Context, teamID gocql.UUID) ([]string, error) {
	entries, err := w.Repo.ListTeamMembers(ctx, teamID)
	if err != nil {
		return nil, err
	}
	var adminIDs []gocql.UUID
	for _, e := range entries {
		if w.Authz.Can(authz.ObjReport, authz.ActResolve, string(e.Role)) {
			adminIDs = append(adminIDs, e.UserID)
		}
	}
	people, err := w.Users.GetMany(ctx, adminIDs)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(adminIDs))
	for _, id := range adminIDs {
		if u := people[id]; u != nil && !u.Bot {
			out = append(out, id.String())
		}
	}
	return out, nil
}

// maxCounted bounds the open reports counted for a badge.
const maxCounted = 1000

// announce tells admins how many of their reports are open: a team's
// admins the team's, system admins every team's, each leaving out those
// about themselves.
func (w *worker) announce(ctx context.Context, teamID gocql.UUID) {
	event := func(teamID string, n int) *pb.Event {
		return &pb.Event{Payload: &pb.Event_ReportsChanged{ReportsChanged: &pb.ReportsChanged{
			TeamId: teamID, Count: int32(n), //nolint:gosec // at most maxCounted
		}}}
	}
	if !ids.IsZero(teamID) {
		n, about, err := w.Repo.CountOpenReports(ctx, teamID, maxCounted)
		var admins []string
		if err == nil {
			admins, err = w.teamAdmins(ctx, teamID)
		}
		if err != nil {
			w.Logger.Warn("failed to tell a team's admins of its reports", zap.String("team", teamID.String()), zap.Error(err))
		} else {
			var rest []string
			for _, id := range admins {
				uid, _ := ids.Parse(id)
				if mine := about[uid]; mine > 0 {
					w.Engine.Send(w.Hub, hub.Deliver{UserIDs: []string{id}, Event: event(teamID.String(), n-mine)})
				} else {
					rest = append(rest, id)
				}
			}
			if len(rest) > 0 {
				w.Engine.Send(w.Hub, hub.Deliver{UserIDs: rest, Event: event(teamID.String(), n)})
			}
		}
	}
	n, about, err := w.Repo.CountOpenReports(ctx, gocql.UUID{}, maxCounted)
	if err != nil {
		w.Logger.Warn("failed to tell system admins of the reports", zap.Error(err))
		return
	}
	instead := make(map[string]*pb.Event, len(about))
	for id, mine := range about {
		instead[id.String()] = event("", n-mine)
	}
	w.Engine.Send(w.Hub, hub.ToAdmins{Event: event("", n), Instead: instead})
}
