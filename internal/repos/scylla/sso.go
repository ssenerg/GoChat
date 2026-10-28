package scylla

import (
	"context"
	"time"

	"github.com/gocql/gocql"

	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

const (
	ssoFlowColumns     = "state_hash, provider, nonce, verifier, redirect_uri, link_user, app_challenge, expires_at"
	ssoTicketColumns   = "token_hash, kind, user_id, challenge, provider, issuer, subject, email, expires_at"
	ssoIdentityColumns = "issuer, subject, user_id, provider, email, linked_at"
)

func (r *Repository) CreateSSOFlow(ctx context.Context, f *models.SSOFlow, now time.Time) error {
	var linkUser *gocql.UUID
	if !ids.IsZero(f.LinkUser) {
		linkUser = &f.LinkUser
	}
	return wrap(r.query(ctx, "INSERT INTO sso_flows ("+ssoFlowColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?) USING TTL ?",
		f.StateHash, f.Provider, f.Nonce, f.Verifier, f.RedirectURI, linkUser, nullable(f.AppChallenge), f.ExpiresAt,
		ttlSeconds(f.ExpiresAt, now)).ExecRelease())
}

// ConsumeSSOFlow deletes the flow and returns it, NotFound when it is gone
// or another call took it first.
func (r *Repository) ConsumeSSOFlow(ctx context.Context, hash []byte) (*models.SSOFlow, error) {
	var f models.SSOFlow
	if err := r.query(ctx, "SELECT "+ssoFlowColumns+" FROM sso_flows WHERE state_hash = ?", hash).GetRelease(&f); err != nil {
		return nil, wrap(err)
	}
	applied, err := r.query(ctx, "DELETE FROM sso_flows WHERE state_hash = ? IF EXISTS", hash).ExecCASRelease()
	if err != nil {
		return nil, wrap(err)
	}
	if !applied {
		return nil, ierrors.NotFound("not found")
	}
	return &f, nil
}

func (r *Repository) CreateSSOTicket(ctx context.Context, t *models.SSOTicket, now time.Time) error {
	var userID *gocql.UUID
	if !ids.IsZero(t.UserID) {
		userID = &t.UserID
	}
	return wrap(r.query(ctx, "INSERT INTO sso_tickets ("+ssoTicketColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) USING TTL ?",
		t.TokenHash, t.Kind, userID, nullable(t.Challenge), t.Provider, nullable(t.Issuer), nullable(t.Subject), nullable(t.Email),
		t.ExpiresAt, ttlSeconds(t.ExpiresAt, now)).ExecRelease())
}

func (r *Repository) GetSSOTicket(ctx context.Context, hash []byte) (*models.SSOTicket, error) {
	var t models.SSOTicket
	if err := r.query(ctx, "SELECT "+ssoTicketColumns+" FROM sso_tickets WHERE token_hash = ?", hash).GetRelease(&t); err != nil {
		return nil, wrap(err)
	}
	return &t, nil
}

// ConsumeSSOTicket deletes the ticket, reporting whether this call is the
// one that did.
func (r *Repository) ConsumeSSOTicket(ctx context.Context, hash []byte) (bool, error) {
	applied, err := r.query(ctx, "DELETE FROM sso_tickets WHERE token_hash = ? IF EXISTS", hash).ExecCASRelease()
	return applied, wrap(err)
}

func (r *Repository) GetSSOIdentity(ctx context.Context, issuer, subject string) (gocql.UUID, error) {
	var id gocql.UUID
	if err := r.query(ctx, "SELECT user_id FROM sso_identities WHERE issuer = ? AND subject = ?", issuer, subject).Scan(&id); err != nil {
		return gocql.UUID{}, wrap(err)
	}
	return id, nil
}

// LinkSSOIdentity claims the identity for its user, reporting false when
// someone else holds it, then lists it under the user and the issuer.
func (r *Repository) LinkSSOIdentity(ctx context.Context, id *models.SSOIdentity) (bool, error) {
	ok, err := r.claim(ctx, "INSERT INTO sso_identities (issuer, subject, user_id) VALUES (?, ?, ?) IF NOT EXISTS",
		"SELECT user_id FROM sso_identities WHERE issuer = ? AND subject = ?", "user_id", id.UserID, id.Issuer, id.Subject)
	if err != nil || !ok {
		return false, err
	}
	b := r.batch(ctx, true)
	b.Query("INSERT INTO user_sso_identities (user_id, issuer, subject, provider, email, linked_at) VALUES (?, ?, ?, ?, ?, ?)",
		id.UserID, id.Issuer, id.Subject, id.Provider, nullable(id.Email), id.LinkedAt)
	b.Query("INSERT INTO sso_issuer_users (issuer, user_id, subject, provider, email, linked_at) VALUES (?, ?, ?, ?, ?, ?)",
		id.Issuer, id.UserID, id.Subject, id.Provider, nullable(id.Email), id.LinkedAt)
	if err := r.execBatch(b); err != nil {
		r.release(err, "DELETE FROM sso_identities WHERE issuer = ? AND subject = ? IF user_id = ?", id.Issuer, id.Subject, id.UserID)
		return false, err
	}
	return true, nil
}

// UnlinkSSOIdentity drops the identity's listings, then frees it.
func (r *Repository) UnlinkSSOIdentity(ctx context.Context, id *models.SSOIdentity) error {
	b := r.batch(ctx, true)
	b.Query("DELETE FROM user_sso_identities WHERE user_id = ? AND issuer = ? AND subject = ?", id.UserID, id.Issuer, id.Subject)
	b.Query("DELETE FROM sso_issuer_users WHERE issuer = ? AND user_id = ? AND subject = ?", id.Issuer, id.UserID, id.Subject)
	if err := r.execBatch(b); err != nil {
		return err
	}
	_, err := r.query(ctx, "DELETE FROM sso_identities WHERE issuer = ? AND subject = ? IF user_id = ?", id.Issuer, id.Subject, id.UserID).ExecCASRelease()
	return wrap(err)
}

func (r *Repository) ListUserSSOIdentities(ctx context.Context, userID gocql.UUID) ([]*models.SSOIdentity, error) {
	var out []*models.SSOIdentity
	err := r.query(ctx, "SELECT "+ssoIdentityColumns+" FROM user_sso_identities WHERE user_id = ?", userID).SelectRelease(&out)
	return out, wrap(err)
}

// ListIssuerSSOIdentities lists the identities of an issuer by user, from
// the first user after after, or from the start when it is zero.
func (r *Repository) ListIssuerSSOIdentities(ctx context.Context, issuer string, after gocql.UUID, limit int) ([]*models.SSOIdentity, error) {
	stmt, args := "SELECT "+ssoIdentityColumns+" FROM sso_issuer_users WHERE issuer = ? LIMIT ?", []any{issuer, limit}
	if !ids.IsZero(after) {
		stmt, args = "SELECT "+ssoIdentityColumns+" FROM sso_issuer_users WHERE issuer = ? AND user_id > ? LIMIT ?", []any{issuer, after, limit}
	}
	var out []*models.SSOIdentity
	err := r.query(ctx, stmt, args...).SelectRelease(&out)
	return out, wrap(err)
}
