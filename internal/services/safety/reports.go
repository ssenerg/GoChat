package safety

import (
	"context"
	"fmt"
	"time"

	"github.com/gocql/gocql"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/validate"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/accounts"
	"github.com/ssenerg/gochat/internal/services/channels"
	"github.com/ssenerg/gochat/internal/services/convert"
	"github.com/ssenerg/gochat/internal/services/directory"
)

const (
	maxDetails  = 500
	excerptSize = 1000
	reportPage  = 50
)

var reasons = map[pb.ReportReason]string{
	pb.ReportReason_REPORT_REASON_SPAM:       models.ReportSpam,
	pb.ReportReason_REPORT_REASON_HARASSMENT: models.ReportHarassment,
	pb.ReportReason_REPORT_REASON_HATE:       models.ReportHate,
	pb.ReportReason_REPORT_REASON_SEXUAL:     models.ReportSexual,
	pb.ReportReason_REPORT_REASON_VIOLENCE:   models.ReportViolence,
	pb.ReportReason_REPORT_REASON_OTHER:      models.ReportOther,
}

var reportActions = map[pb.ReportAction]string{
	pb.ReportAction_REPORT_ACTION_DISMISS:        models.ReportDismissed,
	pb.ReportAction_REPORT_ACTION_DELETE_MESSAGE: models.ReportDeleteMessage,
	pb.ReportAction_REPORT_ACTION_REMOVE_MEMBER:  models.ReportRemoveMember,
	pb.ReportAction_REPORT_ACTION_DEACTIVATE:     models.ReportDeactivate,
}

func (w *worker) report(ctx context.Context, msg Report) (Done, error) {
	if !w.Authz.Can(authz.ObjReport, authz.ActCreate, string(msg.P.Role)) {
		return Done{}, ierrors.PermissionDenied()
	}
	key := msg.P.UserID.String()
	if w.reported.Blocked(key) {
		return Done{}, ierrors.ResourceExhausted("you sent too many reports, try again later")
	}
	req := msg.Req
	reason, ok := reasons[req.GetReason()]
	if !ok {
		return Done{}, ierrors.InvalidArgument("pick a reason")
	}
	details, err := validate.Text("details", req.GetDetails(), maxDetails)
	if err != nil {
		return Done{}, err
	}
	if reason == models.ReportOther && details == "" {
		return Done{}, ierrors.InvalidArgument("say what is wrong")
	}
	rep := &models.Report{ID: ids.NewTime(), ReporterID: msg.P.UserID, Reason: reason, Details: details}
	switch t := req.GetTarget().(type) {
	case *pb.ReportRequest_Message:
		err = w.aboutMessage(ctx, msg.P, t.Message, rep)
	case *pb.ReportRequest_User:
		err = w.aboutUser(ctx, msg.P, t.User, rep)
	default:
		err = ierrors.InvalidArgument("name the message or the person you report")
	}
	if err != nil {
		return Done{}, err
	}
	if rep.UserID == msg.P.UserID {
		return Done{}, ierrors.InvalidArgument("you cannot report yourself")
	}
	if err := w.Repo.CreateReport(ctx, rep); err != nil {
		return Done{}, err
	}
	w.reported.Add(key)
	w.Logger.Info("report filed", zap.String("report", rep.ID.String()), zap.String("team", rep.TeamID.String()), zap.String("reason", reason))
	w.announce(ctx, rep.TeamID)
	return Done{}, nil
}

// aboutMessage fills rep from a message the caller can read.
func (w *worker) aboutMessage(ctx context.Context, p *models.Principal, ref *pb.ReportedMessage, rep *models.Report) error {
	channelID, err := parseID(ref.GetChannelId(), "channel")
	if err != nil {
		return err
	}
	target := channels.Target{P: p, ChannelID: channelID}
	m, err := actors.Ask[*pb.Message](ctx, w.Engine, w.Channels, channels.GetMessage{Target: target, MessageID: ref.GetMessageId(), RootID: ref.GetRootId()})
	if err != nil {
		return err
	}
	acc, err := actors.Ask[*channels.AccessResult](ctx, w.Engine, w.Channels, channels.Access{Target: target})
	if err != nil {
		return err
	}
	rep.TeamID, rep.ChannelID = acc.Channel.TeamID, channelID
	rep.MessageID, _ = ids.Parse(m.GetId())
	rep.RootID, _ = ids.Parse(m.GetRootId())
	rep.UserID, _ = ids.Parse(m.GetAuthor().GetId())
	rep.Excerpt = excerpt(m)
	return nil
}

// excerpt is what a message says, as an admin reads it in the report.
func excerpt(m *pb.Message) string {
	text := m.GetContent()
	if r := []rune(text); len(r) > excerptSize {
		text = string(r[:excerptSize]) + "…"
	}
	switch n := len(m.GetAttachments()); {
	case n == 1 && text == "":
		return "[a file: " + m.Attachments[0].GetName() + "]"
	case n > 0 && text == "":
		return fmt.Sprintf("[%d files]", n)
	case n > 0:
		return text + fmt.Sprintf("\n[%d files]", n)
	}
	return text
}

// aboutUser fills rep from a person of a team the caller is in.
func (w *worker) aboutUser(ctx context.Context, p *models.Principal, ref *pb.ReportedUser, rep *models.Report) error {
	teamID, err := parseID(ref.GetTeamId(), "team")
	if err != nil {
		return err
	}
	userID, err := parseID(ref.GetUserId(), "user")
	if err != nil {
		return err
	}
	if _, err := w.Repo.GetTeamMember(ctx, teamID, p.UserID); err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return ierrors.NotFound("team not found")
		}
		return err
	}
	if _, err := w.Repo.GetTeamMember(ctx, teamID, userID); err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return ierrors.NotFound("user not found")
		}
		return err
	}
	rep.TeamID, rep.UserID = teamID, userID
	return nil
}

var errNoReport = ierrors.NotFound("report not found")

// moderates reports whether p acts on the reports of teamID: its admins
// and system admins, who alone act on those of no team.
func (w *worker) moderates(ctx context.Context, p *models.Principal, teamID gocql.UUID) (bool, error) {
	if p.Role == models.SystemRoleAdmin {
		return true, nil
	}
	if ids.IsZero(teamID) {
		return false, nil
	}
	m, err := w.Repo.GetTeamMember(ctx, teamID, p.UserID)
	if ierrors.Is(err, ierrors.CodeNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return w.Authz.Can(authz.ObjReport, authz.ActRead, string(m.Role)), nil
}

func (w *worker) listReports(ctx context.Context, msg ListReports) (*pb.ListReportsResponse, error) {
	var scope gocql.UUID
	if msg.TeamID != "" {
		var err error
		if scope, err = parseID(msg.TeamID, "team"); err != nil {
			return nil, err
		}
	}
	ok, err := w.moderates(ctx, msg.P, scope)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ierrors.PermissionDenied()
	}
	var after gocql.UUID
	if msg.After != "" {
		if after, err = parseTimeID(msg.After, "report"); err != nil {
			return nil, err
		}
	}
	list, err := w.Repo.ListReports(ctx, scope, !msg.Resolved, after, reportPage)
	if err != nil {
		return nil, err
	}
	out := &pb.ListReportsResponse{}
	if len(list) == reportPage {
		out.Next = list[len(list)-1].ID.String()
	}
	shown := list[:0]
	for _, rep := range list {
		if rep.Open() == msg.Resolved {
			// Resolved but still listed open: its move was cut short.
			if !msg.Resolved {
				w.reindex(ctx, rep)
			}
			continue
		}
		// No one sees the reports about themselves.
		if rep.UserID != msg.P.UserID {
			shown = append(shown, rep)
		}
	}
	out.Reports, err = w.render(ctx, shown)
	return out, err
}

// reindex redoes a resolved report's move out of the open listings.
func (w *worker) reindex(ctx context.Context, rep *models.Report) {
	if err := w.Repo.IndexResolved(ctx, rep); err != nil {
		w.Logger.Warn("failed to list a resolved report as resolved", zap.String("report", rep.ID.String()), zap.Error(err))
	}
}

func (w *worker) countReports(ctx context.Context, p *models.Principal) (*pb.CountReportsResponse, error) {
	out := &pb.CountReportsResponse{Teams: map[string]int32{}}
	teams, err := w.Repo.ListUserTeams(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	for _, m := range teams {
		if !w.Authz.Can(authz.ObjReport, authz.ActRead, string(m.Role)) {
			continue
		}
		n, about, err := w.Repo.CountOpenReports(ctx, m.TeamID, maxCounted)
		if err != nil {
			return nil, err
		}
		out.Teams[m.TeamID.String()] = int32(n - about[p.UserID]) //nolint:gosec // at most maxCounted
	}
	if p.Role == models.SystemRoleAdmin {
		n, about, err := w.Repo.CountOpenReports(ctx, gocql.UUID{}, maxCounted)
		if err != nil {
			return nil, err
		}
		out.All = int32(n - about[p.UserID]) //nolint:gosec // at most maxCounted
	}
	return out, nil
}

func (w *worker) resolve(ctx context.Context, msg Resolve) (*pb.Report, error) {
	id, err := parseTimeID(msg.ReportID, "report")
	if err != nil {
		return nil, err
	}
	action, ok := reportActions[msg.Action]
	if !ok {
		return nil, ierrors.InvalidArgument("pick what to do")
	}
	rep, err := w.Repo.GetReport(ctx, id)
	if ierrors.Is(err, ierrors.CodeNotFound) {
		return nil, errNoReport
	}
	if err != nil {
		return nil, err
	}
	if ok, err := w.moderates(ctx, msg.P, rep.TeamID); err != nil || !ok || rep.UserID == msg.P.UserID {
		if err == nil {
			err = errNoReport
		}
		return nil, err
	}
	if rep.ReporterID == msg.P.UserID {
		return nil, ierrors.New(ierrors.CodePermissionDenied, "another admin acts on a report you sent")
	}
	if !rep.Open() {
		w.reindex(ctx, rep)
		return nil, errResolved
	}
	if rep.Action == models.ReportPending && time.Since(rep.ResolvedAt) < w.claimFor() {
		return nil, errClaimed
	}
	// Claimed first, so two admins never both act; a claim its admin
	// never settled goes stale after claimFor.
	claim := time.Now().UTC().Truncate(time.Millisecond)
	if ok, err := w.Repo.ClaimReport(ctx, rep, msg.P.UserID, claim); err != nil || !ok {
		if err == nil {
			err = errClaimed
		}
		return nil, err
	}
	rep.Action, rep.ResolvedBy, rep.ResolvedAt = models.ReportPending, msg.P.UserID, claim
	// The claim is settled even when the act ran out the request's time.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.Cfg.OpTimeout)
	defer cancel()
	if err := w.act(ctx, msg.P, rep, msg.Action); err != nil {
		if rerr := w.Repo.ReleaseReport(sctx, rep); rerr != nil {
			w.Logger.Warn("failed to release a report's claim", zap.String("report", rep.ID.String()), zap.Error(rerr))
		}
		return nil, err
	}
	held, err := w.Repo.FinishReport(sctx, rep, action)
	switch {
	case err != nil && held:
		// Resolved, but still listed open until a listing repairs it.
		w.Logger.Warn("failed to list a resolved report as resolved", zap.String("report", rep.ID.String()), zap.Error(err))
	case err != nil:
		return nil, err
	case !held:
		return nil, errClaimed
	}
	rep.Action = action
	w.Logger.Info("report resolved", zap.String("report", rep.ID.String()), zap.String("action", action), zap.String("by", msg.P.UserID.String()))
	w.announce(sctx, rep.TeamID)
	out, err := w.render(sctx, []*models.Report{rep})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

var (
	errResolved = ierrors.FailedPrecondition("an admin already acted on this report")
	errClaimed  = ierrors.FailedPrecondition("another admin is acting on this report")
)

// claimFor is how long an admin's claim on a report holds: past the time
// their act may take.
func (w *worker) claimFor() time.Duration {
	return max(time.Minute, 3*w.Cfg.OpTimeout)
}

// act does what the admin chose. Its object being gone already counts as
// done.
func (w *worker) act(ctx context.Context, p *models.Principal, rep *models.Report, action pb.ReportAction) error {
	var err error
	switch action {
	case pb.ReportAction_REPORT_ACTION_DELETE_MESSAGE:
		if ids.IsZero(rep.MessageID) {
			return ierrors.InvalidArgument("the report names no message")
		}
		root := ""
		if !ids.IsZero(rep.RootID) {
			root = rep.RootID.String()
		}
		_, err = actors.Ask[channels.Done](ctx, w.Engine, w.Channels, channels.Delete{
			Target: channels.Target{P: p, ChannelID: rep.ChannelID}, MessageID: rep.MessageID.String(), RootID: root, Moderated: true,
		})
	case pb.ReportAction_REPORT_ACTION_REMOVE_MEMBER:
		if ids.IsZero(rep.TeamID) {
			return ierrors.InvalidArgument("the conversation belongs to no team")
		}
		_, err = actors.Ask[directory.Done](ctx, w.Engine, w.Directory, directory.RemoveTeamMember{
			P: p, TeamID: rep.TeamID.String(), UserID: rep.UserID.String(),
		})
	case pb.ReportAction_REPORT_ACTION_DEACTIVATE:
		if p.Role != models.SystemRoleAdmin {
			return ierrors.New(ierrors.CodePermissionDenied, "only system admins deactivate accounts")
		}
		_, err = actors.Ask[*models.User](ctx, w.Engine, w.Accounts, accounts.SetDeactivated{
			P: p, UserID: rep.UserID.String(), Deactivated: true,
		})
	}
	if ierrors.Is(err, ierrors.CodeNotFound) {
		return nil
	}
	return err
}

// render shows reports to an admin, with the people, teams and channels
// they name.
func (w *worker) render(ctx context.Context, list []*models.Report) ([]*pb.Report, error) {
	var userIDs, teamIDs, channelIDs []gocql.UUID
	for _, rep := range list {
		userIDs = append(userIDs, rep.ReporterID, rep.UserID)
		if !rep.Open() {
			userIDs = append(userIDs, rep.ResolvedBy)
		}
		if !ids.IsZero(rep.TeamID) {
			teamIDs = append(teamIDs, rep.TeamID)
		}
		if !ids.IsZero(rep.ChannelID) {
			channelIDs = append(channelIDs, rep.ChannelID)
		}
	}
	people, err := w.Users.GetMany(ctx, userIDs)
	if err != nil {
		return nil, err
	}
	teamNames := map[gocql.UUID]string{}
	if len(teamIDs) > 0 {
		teams, err := w.Repo.GetTeams(ctx, teamIDs)
		if err != nil {
			return nil, err
		}
		for _, t := range teams {
			teamNames[t.ID] = t.DisplayName
		}
	}
	channelNames := map[gocql.UUID]string{}
	if len(channelIDs) > 0 {
		chs, err := w.Repo.GetChannels(ctx, channelIDs)
		if err != nil {
			return nil, err
		}
		for _, ch := range chs {
			if !ch.Kind.Conversation() {
				channelNames[ch.ID] = ch.DisplayName
			}
		}
	}
	out := make([]*pb.Report, 0, len(list))
	for _, rep := range list {
		r := &pb.Report{
			Id: rep.ID.String(), TeamId: convert.ID(rep.TeamID), TeamName: teamNames[rep.TeamID],
			Reporter: convert.PublicUser(people[rep.ReporterID]), User: convert.PublicUser(people[rep.UserID]),
			ChannelId: convert.ID(rep.ChannelID), MessageId: convert.ID(rep.MessageID), RootId: convert.ID(rep.RootID),
			Excerpt: rep.Excerpt, Reason: reasonOf(rep.Reason), Details: rep.Details, CreatedAt: convert.Time(rep.ID.Time()),
			ChannelName: channelNames[rep.ChannelID],
		}
		if !rep.Open() {
			r.Action = actionOf(rep.Action)
			r.ResolvedBy = convert.PublicUser(people[rep.ResolvedBy])
			r.ResolvedAt = convert.Time(rep.ResolvedAt)
		}
		out = append(out, r)
	}
	return out, nil
}

func reasonOf(s string) pb.ReportReason {
	for k, v := range reasons {
		if v == s {
			return k
		}
	}
	return pb.ReportReason_REPORT_REASON_UNSPECIFIED
}

func actionOf(s string) pb.ReportAction {
	for k, v := range reportActions {
		if v == s {
			return k
		}
	}
	return pb.ReportAction_REPORT_ACTION_UNSPECIFIED
}
