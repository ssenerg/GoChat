package scylla

import (
	"context"
	"errors"
	"time"

	"github.com/gocql/gocql"
)

// link_previews caches the preview of each link, by the hash of its URL,
// for a while: an empty one says the page has none.

// GetLinkPreview returns the cached preview of a link, and whether the
// cache holds one.
func (r *Repository) GetLinkPreview(ctx context.Context, urlHash []byte) ([]byte, bool, error) {
	var preview []byte
	err := r.query(ctx, "SELECT preview FROM link_previews WHERE url_hash = ?", urlHash).Scan(&preview)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, wrap(err)
	}
	return preview, true, nil
}

// PutLinkPreview caches a link's preview, empty for none, for ttl.
func (r *Repository) PutLinkPreview(ctx context.Context, urlHash, preview []byte, ttl time.Duration) error {
	if preview == nil {
		preview = []byte{}
	}
	return wrap(r.query(ctx, "INSERT INTO link_previews (url_hash, preview) VALUES (?, ?) USING TTL ?",
		urlHash, preview, max(int(ttl/time.Second), 1)).ExecRelease())
}
