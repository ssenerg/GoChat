// Package blocks is a read-through cache of whom each person blocked, and
// who blocked them, shared by the actors that keep blocked people from
// reaching them.
package blocks

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/gocql/gocql"

	"github.com/ssenerg/gochat/internal/core/cache"
	"github.com/ssenerg/gochat/internal/core/models"
)

type Repo interface {
	ListBlocks(ctx context.Context, userID gocql.UUID, limit int) ([]*models.Block, error)
	ListBlockers(ctx context.Context, userID gocql.UUID, limit int) ([]gocql.UUID, error)
}

// MaxBlockers bounds who blocked one person, as read: past it, a few more
// of them may still be notified.
const MaxBlockers = 50000

// Set is a set of people: read only.
type Set map[gocql.UUID]bool

type Cache struct {
	repo  Repo
	limit int
	cache *cache.Cache[gocql.UUID, Set]
	by    *cache.Cache[gocql.UUID, Set]
	// copied is set once blocked_by holds the blocks made before it.
	copied atomic.Bool
}

// NewCache caches the sets of up to capacity people, each at most limit
// people long, as the cap on blocks keeps them.
func NewCache(repo Repo, limit, capacity int, ttl time.Duration) *Cache {
	return &Cache{
		repo: repo, limit: limit,
		cache: cache.New[gocql.UUID, Set](capacity, ttl),
		by:    cache.New[gocql.UUID, Set](capacity, ttl),
	}
}

// Of returns whom userID blocked.
func (c *Cache) Of(ctx context.Context, userID gocql.UUID) (Set, error) {
	if c == nil {
		return nil, nil
	}
	if s, ok := c.cache.Get(userID); ok {
		return s, nil
	}
	list, err := c.repo.ListBlocks(ctx, userID, c.limit)
	if err != nil {
		return nil, err
	}
	s := make(Set, len(list))
	for _, b := range list {
		s[b.BlockedID] = true
	}
	c.cache.Set(userID, s)
	return s, nil
}

// Blocked reports whether userID blocked other.
func (c *Cache) Blocked(ctx context.Context, userID, other gocql.UUID) (bool, error) {
	s, err := c.Of(ctx, userID)
	return s[other], err
}

// Either reports whether a or b blocked the other.
func (c *Cache) Either(ctx context.Context, a, b gocql.UUID) (bool, error) {
	if blocked, err := c.Blocked(ctx, a, b); err != nil || blocked {
		return blocked, err
	}
	return c.Blocked(ctx, b, a)
}

// Among returns those of userIDs who blocked author: one read of who
// blocked author, or until their copy is done one of each person's blocks.
func (c *Cache) Among(ctx context.Context, author gocql.UUID, userIDs []gocql.UUID) (Set, error) {
	out := Set{}
	if c == nil || len(userIDs) == 0 {
		return out, nil
	}
	if !c.copied.Load() {
		for _, id := range userIDs {
			blocked, err := c.Blocked(ctx, id, author)
			if err != nil {
				return nil, err
			}
			if blocked {
				out[id] = true
			}
		}
		return out, nil
	}
	s, ok := c.by.Get(author)
	if !ok {
		list, err := c.repo.ListBlockers(ctx, author, MaxBlockers)
		if err != nil {
			return nil, err
		}
		s = make(Set, len(list))
		for _, id := range list {
			s[id] = true
		}
		c.by.Set(author, s)
	}
	for _, id := range userIDs {
		if s[id] {
			out[id] = true
		}
	}
	return out, nil
}

// Copied tells the cache that who blocked each person is all read from
// blocked_by now.
func (c *Cache) Copied() {
	if c != nil {
		c.copied.Store(true)
	}
}

// Invalidate forgets whom userID blocked and who blocked other, after
// userID blocked or unblocked other.
func (c *Cache) Invalidate(userID, other gocql.UUID) {
	if c != nil {
		c.cache.Delete(userID)
		c.by.Delete(other)
	}
}
