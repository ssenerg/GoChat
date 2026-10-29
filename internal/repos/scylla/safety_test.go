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
	if n, err := r.CountOpenReports(ctx, team, 100); err != nil || n != 2 {
		t.Fatalf("the team counts %d, %v", n, err)
	}
	// Every team's listing holds the others' too, newest first.
	all, err := r.ListReports(ctx, gocql.UUID{}, true, gocql.UUID{}, 3)
	if err != nil || len(all) != 3 || all[0].ID != c.ID {
		t.Fatalf("every team's %+v, %v", all, err)
	}

	at := time.Now().UTC().Truncate(time.Millisecond)
	admin := ids.New()
	if ok, err := r.ResolveReport(ctx, a, models.ReportDismissed, admin, at); err != nil || !ok {
		t.Fatalf("resolving: %v, %v", ok, err)
	}
	if ok, err := r.ResolveReport(ctx, a, models.ReportDeleteMessage, ids.New(), at); err != nil || ok {
		t.Fatalf("resolving again: %v, %v", ok, err)
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
	if n, _ := r.CountOpenReports(ctx, team, 100); n != 1 {
		t.Fatalf("the team counts %d open", n)
	}
	// A report of no team is listed once, for all.
	none := file(gocql.UUID{})
	if ok, err := r.ResolveReport(ctx, none, models.ReportDeactivate, admin, at); err != nil || !ok {
		t.Fatalf("resolving a report of no team: %v, %v", ok, err)
	}
}
