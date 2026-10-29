package scylla

import (
	"context"
	"testing"
	"time"

	"github.com/gocql/gocql"

	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

func TestBlocksLive(t *testing.T) {
	r := liveRepository(t)
	ctx := context.Background()
	me, bob, cat := ids.New(), ids.New(), ids.New()
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, id := range []gocql.UUID{bob, cat} {
		if err := r.Block(ctx, &models.Block{UserID: me, BlockedID: id, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := r.ListBlocks(ctx, me, 10)
	if err != nil || len(list) != 2 || !list[0].CreatedAt.Equal(now) {
		t.Fatalf("blocks %+v, %v", list, err)
	}
	if list, _ := r.ListBlocks(ctx, me, 1); len(list) != 1 {
		t.Fatalf("a limit of one listed %d", len(list))
	}
	if list, _ := r.ListBlocks(ctx, bob, 10); len(list) != 0 {
		t.Fatalf("whom bob blocked: %+v", list)
	}
	if err := r.Unblock(ctx, me, bob); err != nil {
		t.Fatal(err)
	}
	if list, _ := r.ListBlocks(ctx, me, 10); len(list) != 1 || list[0].BlockedID != cat {
		t.Fatalf("after unblocking bob: %+v", list)
	}
	if by, err := r.ListBlockers(ctx, cat, 10); err != nil || len(by) != 1 || by[0] != me {
		t.Fatalf("who blocked cat: %v, %v", by, err)
	}
	if by, _ := r.ListBlockers(ctx, bob, 10); len(by) != 0 {
		t.Fatalf("who blocked bob after the unblock: %v", by)
	}
}

func TestCopyBlocksLive(t *testing.T) {
	r := liveRepository(t)
	ctx := context.Background()
	old, gone, them := ids.New(), ids.New(), ids.New()
	stamp := time.Now().UnixMicro()
	// Blocks from before blocked_by; one was undone after the copy read
	// it, so blocked_by holds its delete alone.
	for _, id := range []gocql.UUID{old, gone} {
		if err := r.query(ctx, "INSERT INTO blocks (user_id, blocked_id, created_at) VALUES (?, ?, ?) USING TIMESTAMP ?", id, them, time.Now(), stamp).ExecRelease(); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.query(ctx, "DELETE FROM blocked_by USING TIMESTAMP ? WHERE blocked_id = ? AND user_id = ?", stamp+1, them, gone).ExecRelease(); err != nil {
		t.Fatal(err)
	}
	if n, err := r.CopyBlocks(ctx); err != nil || n < 2 {
		t.Fatalf("copied %d, %v", n, err)
	}
	if by, err := r.ListBlockers(ctx, them, 10); err != nil || len(by) != 1 || by[0] != old {
		t.Fatalf("who blocked them after the copy: %v, %v", by, err)
	}
}

func TestReportsLive(t *testing.T) {
	r := liveRepository(t)
	ctx := context.Background()
	team, other := ids.New(), ids.New()
	file := func(teamID gocql.UUID) *models.Report {
		rep := &models.Report{
			ID: ids.NewTime(), TeamID: teamID, ReporterID: ids.New(), UserID: ids.New(), ChannelID: ids.New(),
			MessageID: ids.NewTime(), Excerpt: "rude", Reason: models.ReportSpam, Details: "again",
		}
		if err := r.CreateReport(ctx, rep); err != nil {
			t.Fatal(err)
		}
		return rep
	}
	a, b, c := file(team), file(team), file(other)
	got, err := r.GetReport(ctx, a.ID)
	if err != nil || got.Excerpt != "rude" || got.Details != "again" || got.TeamID != team || got.Action != "" || !ids.IsZero(got.ResolvedBy) {
		t.Fatalf("report %+v, %v", got, err)
	}
	if _, err := r.GetReport(ctx, ids.NewTime()); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("no report: %v", err)
	}
	open, err := r.ListReports(ctx, team, true, gocql.UUID{}, 10)
	if err != nil || len(open) != 2 || open[0].ID != b.ID || open[1].ID != a.ID {
		t.Fatalf("the team's open reports %+v, %v", open, err)
	}
	if page, _ := r.ListReports(ctx, team, true, b.ID, 10); len(page) != 1 || page[0].ID != a.ID {
		t.Fatalf("the page after %s: %+v", b.ID, page)
	}
	if n, about, err := r.CountOpenReports(ctx, team, 100); err != nil || n != 2 || about[a.UserID] != 1 || about[b.UserID] != 1 {
		t.Fatalf("the team counts %d, %v, %v", n, about, err)
	}
	// Every team's listing holds the others' too, newest first.
	all, err := r.ListReports(ctx, gocql.UUID{}, true, gocql.UUID{}, 3)
	if err != nil || len(all) != 3 || all[0].ID != c.ID {
		t.Fatalf("every team's %+v, %v", all, err)
	}

	// One admin claims a report; another, reading it open, cannot.
	admin, other := ids.New(), ids.New()
	at := time.Now().UTC().Truncate(time.Millisecond)
	fresh := *a
	if ok, err := r.ClaimReport(ctx, &fresh, admin, at); err != nil || !ok {
		t.Fatalf("claiming: %v, %v", ok, err)
	}
	if ok, err := r.ClaimReport(ctx, &fresh, other, at); err != nil || ok {
		t.Fatalf("claiming again: %v, %v", ok, err)
	}
	claimed, _ := r.GetReport(ctx, a.ID)
	if claimed.Action != models.ReportPending || claimed.ResolvedBy != admin || !claimed.ResolvedAt.Equal(at) {
		t.Fatalf("claimed report %+v", claimed)
	}
	// A claim read stale is taken over; its first holder no longer
	// finishes or releases it.
	later := at.Add(time.Minute)
	if ok, err := r.ClaimReport(ctx, claimed, other, later); err != nil || !ok {
		t.Fatalf("taking over: %v, %v", ok, err)
	}
	if ok, err := r.FinishReport(ctx, claimed, models.ReportDeleteMessage); err != nil || ok {
		t.Fatalf("finishing a claim taken over: %v, %v", ok, err)
	}
	if err := r.ReleaseReport(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	held, _ := r.GetReport(ctx, a.ID)
	if held.Action != models.ReportPending || held.ResolvedBy != other {
		t.Fatalf("after a stale holder let go: %+v", held)
	}
	// Its holder releases it, then claims and finishes it.
	if err := r.ReleaseReport(ctx, held); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.GetReport(ctx, a.ID); got.Action != "" || !ids.IsZero(got.ResolvedBy) || !got.ResolvedAt.IsZero() {
		t.Fatalf("released report %+v", got)
	}
	if ok, err := r.ClaimReport(ctx, a, admin, at); err != nil || !ok {
		t.Fatalf("claiming after the release: %v, %v", ok, err)
	}
	mine := *a
	mine.Action, mine.ResolvedBy, mine.ResolvedAt = models.ReportPending, admin, at
	if ok, err := r.FinishReport(ctx, &mine, models.ReportDismissed); err != nil || !ok {
		t.Fatalf("finishing: %v, %v", ok, err)
	}
	got, _ = r.GetReport(ctx, a.ID)
	if got.Action != models.ReportDismissed || got.ResolvedBy != admin || !got.ResolvedAt.Equal(at) {
		t.Fatalf("resolved report %+v", got)
	}
	if open, _ := r.ListReports(ctx, team, true, gocql.UUID{}, 10); len(open) != 1 || open[0].ID != b.ID {
		t.Fatalf("open after resolving: %+v", open)
	}
	if done, _ := r.ListReports(ctx, team, false, gocql.UUID{}, 10); len(done) != 1 || done[0].ID != a.ID {
		t.Fatalf("resolved: %+v", done)
	}
	if done, _ := r.ListReports(ctx, gocql.UUID{}, false, gocql.UUID{}, 1); len(done) != 1 {
		t.Fatalf("every team's resolved: %+v", done)
	}
	if n, _, _ := r.CountOpenReports(ctx, team, 100); n != 1 {
		t.Fatalf("the team counts %d open", n)
	}
	// Moving it again changes nothing.
	if err := r.IndexResolved(ctx, got); err != nil {
		t.Fatal(err)
	}
	if done, _ := r.ListReports(ctx, team, false, gocql.UUID{}, 10); len(done) != 1 {
		t.Fatalf("resolved after moving again: %+v", done)
	}

	// An index row from before it named the person counts from the report.
	legacy := ids.New()
	old := file(legacy)
	if err := r.query(ctx, "UPDATE report_index SET user_id = null WHERE scope = ? AND open = true AND id = ?", legacy, old.ID).ExecRelease(); err != nil {
		t.Fatal(err)
	}
	if n, about, err := r.CountOpenReports(ctx, legacy, 100); err != nil || n != 1 || about[old.UserID] != 1 {
		t.Fatalf("a legacy row counts %d, %v, %v", n, about, err)
	}
}
