package scylla

import (
	"context"
	"time"

	"github.com/gocql/gocql"

	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
)

// blocks holds whom each person blocked. reports holds every report;
// report_index lists them newest first by team, open or resolved, and
// every team's under the zero scope for system admins.

// Block records that userID blocked blockedID.
func (r *Repository) Block(ctx context.Context, b *models.Block) error {
	return wrap(r.query(ctx, "INSERT INTO blocks (user_id, blocked_id, created_at) VALUES (?, ?, ?)",
		b.UserID, b.BlockedID, b.CreatedAt).ExecRelease())
}

func (r *Repository) Unblock(ctx context.Context, userID, blockedID gocql.UUID) error {
	return wrap(r.query(ctx, "DELETE FROM blocks WHERE user_id = ? AND blocked_id = ?", userID, blockedID).ExecRelease())
}

// ListBlocks returns whom userID blocked, at most limit.
func (r *Repository) ListBlocks(ctx context.Context, userID gocql.UUID, limit int) ([]*models.Block, error) {
	var out []*models.Block
	err := r.query(ctx, "SELECT user_id, blocked_id, created_at FROM blocks WHERE user_id = ? LIMIT ?", userID, limit).SelectRelease(&out)
	return out, wrap(err)
}

const reportColumns = "id, team_id, reporter_id, user_id, channel_id, message_id, root_id, excerpt, reason, details, action, resolved_by, resolved_at"

// reportScopes are the listings a report of teamID shows in.
func reportScopes(teamID gocql.UUID) []gocql.UUID {
	if ids.IsZero(teamID) {
		return []gocql.UUID{{}}
	}
	return []gocql.UUID{teamID, {}}
}

// CreateReport stores an open report.
func (r *Repository) CreateReport(ctx context.Context, rep *models.Report) error {
	b := r.batch(ctx, true)
	b.Query("INSERT INTO reports (id, team_id, reporter_id, user_id, channel_id, message_id, root_id, excerpt, reason, details, action) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '')",
		rep.ID, rep.TeamID, rep.ReporterID, rep.UserID, rep.ChannelID, rep.MessageID, rep.RootID, rep.Excerpt, rep.Reason, rep.Details)
	for _, scope := range reportScopes(rep.TeamID) {
		b.Query("INSERT INTO report_index (scope, open, id) VALUES (?, true, ?)", scope, rep.ID)
	}
	return r.execBatch(b)
}

// GetReport returns a report, or NotFound.
func (r *Repository) GetReport(ctx context.Context, id gocql.UUID) (*models.Report, error) {
	var rep models.Report
	if err := r.query(ctx, "SELECT "+reportColumns+" FROM reports WHERE id = ?", id).GetRelease(&rep); err != nil {
		return nil, wrap(err)
	}
	return &rep, nil
}

// ListReports returns the open or resolved reports of a scope, a team or
// the zero one for all, newest first: at most limit, before the report
// after names when it is set.
func (r *Repository) ListReports(ctx context.Context, scope gocql.UUID, open bool, after gocql.UUID, limit int) ([]*models.Report, error) {
	q := r.query(ctx, "SELECT id FROM report_index WHERE scope = ? AND open = ? LIMIT ?", scope, open, limit)
	if !ids.IsZero(after) {
		q = r.query(ctx, "SELECT id FROM report_index WHERE scope = ? AND open = ? AND id < ? LIMIT ?", scope, open, after, limit)
	}
	var list []gocql.UUID
	if err := q.SelectRelease(&list); err != nil {
		return nil, wrap(err)
	}
	if len(list) == 0 {
		return nil, nil
	}
	var found []*models.Report
	if err := r.query(ctx, "SELECT "+reportColumns+" FROM reports WHERE id IN ?", list).SelectRelease(&found); err != nil {
		return nil, wrap(err)
	}
	byID := make(map[gocql.UUID]*models.Report, len(found))
	for _, rep := range found {
		byID[rep.ID] = rep
	}
	out := make([]*models.Report, 0, len(list))
	for _, id := range list {
		if rep := byID[id]; rep != nil {
			out = append(out, rep)
		}
	}
	return out, nil
}

// CountOpenReports counts a scope's open reports, up to limit.
func (r *Repository) CountOpenReports(ctx context.Context, scope gocql.UUID, limit int) (int, error) {
	var list []gocql.UUID
	err := r.query(ctx, "SELECT id FROM report_index WHERE scope = ? AND open = true LIMIT ?", scope, limit).SelectRelease(&list)
	return len(list), wrap(err)
}

// ResolveReport records what an admin did about an open report, and
// reports whether it was still open: two admins acting at once record one.
func (r *Repository) ResolveReport(ctx context.Context, rep *models.Report, action string, by gocql.UUID, at time.Time) (bool, error) {
	q := r.query(ctx, "UPDATE reports SET action = ?, resolved_by = ?, resolved_at = ? WHERE id = ? IF action = ''", action, by, at, rep.ID)
	applied, err := q.ExecCASRelease()
	if err != nil {
		return false, wrap(err)
	}
	if !applied {
		return false, nil
	}
	b := r.batch(ctx, true)
	for _, scope := range reportScopes(rep.TeamID) {
		b.Query("DELETE FROM report_index WHERE scope = ? AND open = true AND id = ?", scope, rep.ID)
		b.Query("INSERT INTO report_index (scope, open, id) VALUES (?, false, ?)", scope, rep.ID)
	}
	return true, r.execBatch(b)
}
