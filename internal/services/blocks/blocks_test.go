package blocks

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gocql/gocql"

	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
)

// counted holds whom each person blocked, and counts the reads.
type counted struct {
	blocks          map[gocql.UUID][]gocql.UUID
	lists, blockers int
}

func (c *counted) ListBlocks(_ context.Context, userID gocql.UUID, _ int) ([]*models.Block, error) {
	c.lists++
	var out []*models.Block
	for _, id := range c.blocks[userID] {
		out = append(out, &models.Block{UserID: userID, BlockedID: id})
	}
	return out, nil
}

func (c *counted) ListBlockers(_ context.Context, userID gocql.UUID, _ int) ([]gocql.UUID, error) {
	c.blockers++
	var out []gocql.UUID
	for by, list := range c.blocks {
		if slices.Contains(list, userID) {
			out = append(out, by)
		}
	}
	return out, nil
}

func TestAmong(t *testing.T) {
	ann, bob, cat, dan := ids.New(), ids.New(), ids.New(), ids.New()
	repo := &counted{blocks: map[gocql.UUID][]gocql.UUID{bob: {ann}, dan: {ann, cat}}}
	c := NewCache(repo, 10, 10, time.Minute)
	ctx := context.Background()
	want := Set{bob: true, dan: true}

	// Until blocked_by is filled, each person's blocks are read.
	got, err := c.Among(ctx, ann, []gocql.UUID{bob, cat, dan})
	if err != nil || len(got) != 2 || !got[bob] || !got[dan] {
		t.Fatalf("before the copy: %v, %v, want %v", got, err, want)
	}
	if repo.lists != 3 || repo.blockers != 0 {
		t.Fatalf("before the copy read %d lists and %d blockers", repo.lists, repo.blockers)
	}

	// Then one read answers, cached until someone blocks or unblocks her.
	c.Copied()
	for range 2 {
		if got, _ := c.Among(ctx, ann, []gocql.UUID{bob, cat, dan}); len(got) != 2 || !got[bob] || !got[dan] {
			t.Fatalf("after the copy: %v, want %v", got, want)
		}
	}
	if repo.blockers != 1 {
		t.Fatalf("after the copy read blockers %d times, want once", repo.blockers)
	}
	repo.blocks[cat] = []gocql.UUID{ann}
	c.Invalidate(cat, ann)
	if got, _ := c.Among(ctx, ann, []gocql.UUID{bob, cat, dan}); len(got) != 3 {
		t.Fatalf("after cat blocked her: %v", got)
	}
	if repo.blockers != 2 {
		t.Fatalf("read blockers %d times, want twice", repo.blockers)
	}
}
