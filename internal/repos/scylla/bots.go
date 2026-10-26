package scylla

import (
	"context"
	"time"

	"github.com/gocql/gocql"

	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

const botColumns = "id, owner_id, description, sees_all, commands, token_hash, created_at, webhook_url, webhook_secret, delivered_pts, webhook_error, webhook_error_at"

// CreateBot claims the bot's username, then writes its user, the bot and
// its token. A bot has no email, password or second factor.
func (r *Repository) CreateBot(ctx context.Context, u *models.User, b *models.Bot) error {
	ok, err := r.claim(ctx, "INSERT INTO usernames (username, user_id) VALUES (?, ?) IF NOT EXISTS",
		"SELECT user_id FROM usernames WHERE username = ?", "user_id", u.ID, u.Username)
	if err != nil {
		return err
	}
	if !ok {
		return ierrors.AlreadyExists("username is taken")
	}
	batch := r.batch(ctx, true)
	batch.Query("INSERT INTO users (id, username, display_name, system_role, deactivated, created_at, updated_at, bot, bot_owner) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		u.ID, u.Username, u.DisplayName, string(u.Role), false, u.CreatedAt, u.UpdatedAt, true, u.BotOwner)
	batch.Query("INSERT INTO bots (id, owner_id, description, sees_all, token_hash, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		b.ID, b.OwnerID, b.Description, b.SeesAll, b.TokenHash, b.CreatedAt)
	batch.Query("INSERT INTO user_bots (owner_id, bot_id) VALUES (?, ?)", b.OwnerID, b.ID)
	batch.Query("INSERT INTO bot_tokens (token_hash, bot_id) VALUES (?, ?)", b.TokenHash, b.ID)
	if err := r.execBatch(batch); err != nil {
		r.release(err, "DELETE FROM usernames WHERE username = ? IF user_id = ?", u.Username, u.ID)
		return err
	}
	return nil
}

func (r *Repository) GetBot(ctx context.Context, id gocql.UUID) (*models.Bot, error) {
	var b models.Bot
	if err := r.query(ctx, "SELECT "+botColumns+" FROM bots WHERE id = ?", id).GetRelease(&b); err != nil {
		return nil, wrap(err)
	}
	return &b, nil
}

// GetBots returns the bots found among ids, at most 100 per call.
func (r *Repository) GetBots(ctx context.Context, ids []gocql.UUID) ([]*models.Bot, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > 100 {
		return nil, ierrors.InvalidArgument("too many bots")
	}
	var out []*models.Bot
	err := r.query(ctx, "SELECT "+botColumns+" FROM bots WHERE id IN ?", ids).SelectRelease(&out)
	return out, wrap(err)
}

// ListBotIDs returns the ids of the bots owner owns.
func (r *Repository) ListBotIDs(ctx context.Context, owner gocql.UUID) ([]gocql.UUID, error) {
	var out []gocql.UUID
	err := r.query(ctx, "SELECT bot_id FROM user_bots WHERE owner_id = ?", owner).SelectRelease(&out)
	return out, wrap(err)
}

// BotOfToken returns the bot a token's hash names.
func (r *Repository) BotOfToken(ctx context.Context, hash []byte) (gocql.UUID, error) {
	var id gocql.UUID
	if err := r.query(ctx, "SELECT bot_id FROM bot_tokens WHERE token_hash = ?", hash).Scan(&id); err != nil {
		return gocql.UUID{}, wrap(err)
	}
	return id, nil
}

func (r *Repository) UpdateBot(ctx context.Context, id gocql.UUID, description string, seesAll bool) error {
	return wrap(r.query(ctx, "UPDATE bots SET description = ?, sees_all = ? WHERE id = ?", description, seesAll, id).ExecRelease())
}

// ReplaceBotToken swaps a bot's token for another.
func (r *Repository) ReplaceBotToken(ctx context.Context, id gocql.UUID, old, hash []byte) error {
	b := r.batch(ctx, true)
	b.Query("DELETE FROM bot_tokens WHERE token_hash = ?", old)
	b.Query("INSERT INTO bot_tokens (token_hash, bot_id) VALUES (?, ?)", hash, id)
	b.Query("UPDATE bots SET token_hash = ? WHERE id = ?", hash, id)
	return r.execBatch(b)
}

// DeleteBot turns a bot off: its token stops working, its user is
// deactivated and its owner no longer lists it. Its messages stay.
func (r *Repository) DeleteBot(ctx context.Context, b *models.Bot) error {
	batch := r.batch(ctx, true)
	batch.Query("DELETE FROM bot_tokens WHERE token_hash = ?", b.TokenHash)
	batch.Query("DELETE FROM user_bots WHERE owner_id = ? AND bot_id = ?", b.OwnerID, b.ID)
	batch.Query("UPDATE users SET deactivated = true, updated_at = ? WHERE id = ?", time.Now().UTC(), b.ID)
	batch.Query("DELETE webhook_url, webhook_secret FROM bots WHERE id = ?", b.ID)
	return r.execBatch(batch)
}

func (r *Repository) SetBotCommands(ctx context.Context, id gocql.UUID, commands []byte) error {
	return wrap(r.query(ctx, "UPDATE bots SET commands = ? WHERE id = ?", commands, id).ExecRelease())
}

// SetWebhook sets the bot's webhook, or with an empty url removes it.
func (r *Repository) SetWebhook(ctx context.Context, id gocql.UUID, url, secret string) error {
	if url == "" {
		return wrap(r.query(ctx, "DELETE webhook_url, webhook_secret, webhook_error, webhook_error_at FROM bots WHERE id = ?", id).ExecRelease())
	}
	return wrap(r.query(ctx, "UPDATE bots SET webhook_url = ?, webhook_secret = ?, webhook_error = null, webhook_error_at = null WHERE id = ?",
		url, secret, id).ExecRelease())
}

// SetDelivered records the last update the bot handled.
func (r *Repository) SetDelivered(ctx context.Context, id gocql.UUID, pts int64) error {
	return wrap(r.query(ctx, "UPDATE bots SET delivered_pts = ? WHERE id = ?", pts, id).ExecRelease())
}

// SetWebhookError records why a delivery failed, or with an empty reason
// that one succeeded.
func (r *Repository) SetWebhookError(ctx context.Context, id gocql.UUID, reason string, at time.Time) error {
	if reason == "" {
		return wrap(r.query(ctx, "DELETE webhook_error, webhook_error_at FROM bots WHERE id = ?", id).ExecRelease())
	}
	return wrap(r.query(ctx, "UPDATE bots SET webhook_error = ?, webhook_error_at = ? WHERE id = ?", reason, at, id).ExecRelease())
}

// ListWebhooks returns the bots with a webhook.
func (r *Repository) ListWebhooks(ctx context.Context) ([]*models.Bot, error) {
	var out []*models.Bot
	iter := r.query(ctx, "SELECT "+botColumns+" FROM bots").Iter()
	var b models.Bot
	for iter.StructScan(&b) {
		if b.WebhookURL != "" {
			c := b
			out = append(out, &c)
		}
		b = models.Bot{}
	}
	return out, wrap(iter.Close())
}
