package scylla

import (
	"context"
	"errors"
	"time"

	"github.com/gocql/gocql"

	"github.com/ssenerg/gochat/internal/core/models"
)

// push_devices holds each session's device, push_tokens who holds each
// device token. Both expire with the session.

const pushColumns = "user_id, session_hash, platform, device_token, sandbox, created_at"

// PutPushDevice stores d until the session's expiry.
func (r *Repository) PutPushDevice(ctx context.Context, d *models.PushDevice, expires time.Time) error {
	ttl := ttlSeconds(expires, d.CreatedAt)
	b := r.batch(ctx, true)
	b.Query("INSERT INTO push_devices ("+pushColumns+") VALUES (?, ?, ?, ?, ?, ?) USING TTL ?",
		d.UserID, d.SessionHash, d.Platform, d.Token, d.Sandbox, d.CreatedAt, ttl)
	b.Query("INSERT INTO push_tokens (platform, device_token, user_id, session_hash) VALUES (?, ?, ?, ?) USING TTL ?",
		d.Platform, d.Token, d.UserID, d.SessionHash, ttl)
	return r.execBatch(b)
}

// GetPushDevice returns a session's device, or NotFound.
func (r *Repository) GetPushDevice(ctx context.Context, userID gocql.UUID, sessionHash []byte) (*models.PushDevice, error) {
	var d models.PushDevice
	err := r.query(ctx, "SELECT "+pushColumns+" FROM push_devices WHERE user_id = ? AND session_hash = ?", userID, sessionHash).GetRelease(&d)
	if err != nil {
		return nil, wrap(err)
	}
	return &d, nil
}

func (r *Repository) ListPushDevices(ctx context.Context, userID gocql.UUID) ([]*models.PushDevice, error) {
	var out []*models.PushDevice
	err := r.query(ctx, "SELECT "+pushColumns+" FROM push_devices WHERE user_id = ?", userID).SelectRelease(&out)
	return out, wrap(err)
}

// PushTokenHolder returns the session holding a device token; a zero user
// for none.
func (r *Repository) PushTokenHolder(ctx context.Context, platform, token string) (gocql.UUID, []byte, error) {
	var (
		userID gocql.UUID
		hash   []byte
	)
	err := r.query(ctx, "SELECT user_id, session_hash FROM push_tokens WHERE platform = ? AND device_token = ?", platform, token).Scan(&userID, &hash)
	if errors.Is(err, gocql.ErrNotFound) {
		return gocql.UUID{}, nil, nil
	}
	return userID, hash, wrap(err)
}

// DeletePushDevice forgets a session's device and who holds its token.
func (r *Repository) DeletePushDevice(ctx context.Context, d *models.PushDevice) error {
	b := r.batch(ctx, true)
	b.Query("DELETE FROM push_devices WHERE user_id = ? AND session_hash = ?", d.UserID, d.SessionHash)
	b.Query("DELETE FROM push_tokens WHERE platform = ? AND device_token = ?", d.Platform, d.Token)
	return r.execBatch(b)
}

// DeletePushToken forgets who holds a device token, for a session whose
// token changed.
func (r *Repository) DeletePushToken(ctx context.Context, platform, token string) error {
	return wrap(r.query(ctx, "DELETE FROM push_tokens WHERE platform = ? AND device_token = ?", platform, token).ExecRelease())
}
