package scylla

import (
	"context"
	"time"

	"github.com/gocql/gocql"

	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
)

// blocks holds whom each person blocked, and blocked_by the same the other
// way round. reports holds every report; report_index lists them newest
// first by team, open or resolved, and every team's under the zero scope
// for system admins, each with the person it is about.

// Block records that userID blocked blockedID.
func (r *Repository) Block(ctx context.Context, b *models.Block) error {
	batch := r.batch(ctx, true)
	batch.Query("INSERT INTO blocks (user_id, blocked_id, created_at) VALUES (?, ?, ?)", b.UserID, b.BlockedID, b.CreatedAt)
	batch.Query("INSERT INTO blocked_by (blocked_id, user_id) VALUES (?, ?)", b.BlockedID, b.UserID)
	return r.execBatch(batch)
}

func (r *Repository) Unblock(ctx context.Context, userID, blockedID gocql.UUID) error {
	batch := r.batch(ctx, true)
	batch.Query("DELETE FROM blocks WHERE user_id = ? AND blocked_id = ?", userID, blockedID)
	batch.Query("DELETE FROM blocked_by WHERE blocked_id = ? AND user_id = ?", blockedID, userID)
	return r.execBatch(batch)
}

// ListBlocks returns whom userID blocked, at most limit.
func (r *Repository) ListBlocks(ctx context.Context, userID gocql.UUID, limit int) ([]*models.Block, error) {
	var out []*models.Block
	err := r.query(ctx, "SELECT user_id, blocked_id, created_at FROM blocks WHERE user_id = ? LIMIT ?", userID, limit).SelectRelease(&out)
	return out, wrap(err)
}

// ListBlockers returns who blocked userID, at most limit.
func (r *Repository) ListBlockers(ctx context.Context, userID gocql.UUID, limit int) ([]gocql.UUID, error) {
	var out []gocql.UUID
	err := r.query(ctx, "SELECT user_id FROM blocked_by WHERE blocked_id = ? LIMIT ?", userID, limit).SelectRelease(&out)
	return out, wrap(err)
}

// CopyBlocks fills blocked_by from the blocks made before it, and returns
// how many it copied. Each copy carries its block's write time, so an
// unblock since still wins.
func (r *Repository) CopyBlocks(ctx context.Context) (int, error) {
	q := r.query(ctx, "SELECT user_id, blocked_id, WRITETIME(created_at) FROM blocks")
	defer q.Release()
	iter := q.Iter()
	var (
		userID, blockedID gocql.UUID
		stamp             int64
		n                 int
	)
	for iter.Scan(&userID, &blockedID, &stamp) {
		err := r.query(ctx, "INSERT INTO blocked_by (blocked_id, user_id) VALUES (?, ?) USING TIMESTAMP ?", blockedID, userID, stamp).ExecRelease()
		if err != nil {
			_ = iter.Close()
			return n, wrap(err)
		}
		n++
	}
	return n, wrap(iter.Close())
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
		b.Query("INSERT INTO report_index (scope, open, id, user_id) VALUES (?, true, ?, ?)", scope, rep.ID, rep.UserID)
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

// CountOpenReports counts a scope's open reports, up to limit, and how
// many of them are about each person.
func (r *Repository) CountOpenReports(ctx context.Context, scope gocql.UUID, limit int) (int, map[gocql.UUID]int, error) {
	var rows []struct {
		ID     gocql.UUID `db:"id"`
		UserID gocql.UUID `db:"user_id"`
	}
	err := r.query(ctx, "SELECT id, user_id FROM report_index WHERE scope = ? AND open = true LIMIT ?", scope, limit).SelectRelease(&rows)
	if err != nil {
		return 0, nil, wrap(err)
	}
	about := map[gocql.UUID]int{}
	// Rows from before the index named the person are read from the
	// reports themselves.
	var unnamed []gocql.UUID
	for _, row := range rows {
		if ids.IsZero(row.UserID) {
			unnamed = append(unnamed, row.ID)
		} else {
			about[row.UserID]++
		}
	}
	if len(unnamed) > 0 {
		var reps []*models.Report
		if err := r.query(ctx, "SELECT id, user_id FROM reports WHERE id IN ?", unnamed).SelectRelease(&reps); err != nil {
			return 0, nil, wrap(err)
		}
		for _, rep := range reps {
			about[rep.UserID]++
		}
	}
	return len(rows), about, nil
}

// ClaimReport marks a report pending for by from at, if it is as rep was
// read: open, or pending on a claim the caller found stale. It reports
// whether it did.
func (r *Repository) ClaimReport(ctx context.Context, rep *models.Report, by gocql.UUID, at time.Time) (bool, error) {
	const set = "UPDATE reports SET action = ?, resolved_by = ?, resolved_at = ? WHERE id = ? "
	q := r.query(ctx, set+"IF action = ''", models.ReportPending, by, at, rep.ID)
	if rep.Action != "" {
		q = r.query(ctx, set+"IF action = ? AND resolved_by = ? AND resolved_at = ?",
			models.ReportPending, by, at, rep.ID, rep.Action, rep.ResolvedBy, rep.ResolvedAt)
	}
	applied, err := q.ExecCASRelease()
	return applied, wrap(err)
}

// FinishReport records what the admin holding rep's claim did, and lists
// it as resolved. It reports whether they still held it.
func (r *Repository) FinishReport(ctx context.Context, rep *models.Report, action string) (bool, error) {
	q := r.query(ctx, "UPDATE reports SET action = ? WHERE id = ? IF action = ? AND resolved_by = ? AND resolved_at = ?",
		action, rep.ID, models.ReportPending, rep.ResolvedBy, rep.ResolvedAt)
	applied, err := q.ExecCASRelease()
	if err != nil || !applied {
		return false, wrap(err)
	}
	return true, r.IndexResolved(ctx, rep)
}

// ReleaseReport reopens a report whose claim rep holds.
func (r *Repository) ReleaseReport(ctx context.Context, rep *models.Report) error {
	q := r.query(ctx, "UPDATE reports SET action = '', resolved_by = null, resolved_at = null WHERE id = ? IF action = ? AND resolved_by = ? AND resolved_at = ?",
		rep.ID, models.ReportPending, rep.ResolvedBy, rep.ResolvedAt)
	_, err := q.ExecCASRelease()
	return wrap(err)
}

// IndexResolved moves a resolved report from the open listings to the
// resolved ones. Doing it again repairs a move cut short.
func (r *Repository) IndexResolved(ctx context.Context, rep *models.Report) error {
	b := r.batch(ctx, true)
	for _, scope := range reportScopes(rep.TeamID) {
		b.Query("DELETE FROM report_index WHERE scope = ? AND open = true AND id = ?", scope, rep.ID)
		b.Query("INSERT INTO report_index (scope, open, id, user_id) VALUES (?, false, ?, ?)", scope, rep.ID, rep.UserID)
	}
	return r.execBatch(b)
}
