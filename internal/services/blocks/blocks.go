// Package blocks is a read-through cache of whom each person blocked,
// shared by the actors that keep blocked people from reaching them.
package blocks

import (
	"context"
	"time"

	"github.com/gocql/gocql"

	"github.com/ssenerg/gochat/internal/core/cache"
	"github.com/ssenerg/gochat/internal/core/models"
)

type Repo interface {
	ListBlocks(ctx context.Context, userID gocql.UUID, limit int) ([]*models.Block, error)
}

// Set is the people one person blocked: read only.
type Set map[gocql.UUID]bool

type Cache struct {
	repo  Repo
	limit int
	cache *cache.Cache[gocql.UUID, Set]
}

// NewCache caches the sets of up to capacity people, each at most limit
// people long, as the cap on blocks keeps them.
func NewCache(repo Repo, limit, capacity int, ttl time.Duration) *Cache {
	return &Cache{repo: repo, limit: limit, cache: cache.New[gocql.UUID, Set](capacity, ttl)}
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

func (c *Cache) Invalidate(userID gocql.UUID) {
	if c != nil {
		c.cache.Delete(userID)
	}
}
