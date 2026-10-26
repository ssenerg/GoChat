package accounts

import (
	"context"
	"strings"
	"time"

	"github.com/gocql/gocql"
	"go.uber.org/zap"

	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
)

var errNoUser = ierrors.NotFound("user not found")

func (w *worker) findUser(ctx context.Context, msg FindUser) (*models.User, error) {
	if !w.Authz.Can(authz.ObjUser, authz.ActFind, string(msg.P.Role)) {
		return nil, ierrors.PermissionDenied()
	}
	id, err := w.lookup(ctx, msg)
	if err == nil {
		var u *models.User
		if u, err = w.Repo.GetUser(ctx, id); err == nil {
			return u, nil
		}
	}
	if ierrors.Is(err, ierrors.CodeNotFound) {
		return nil, errNoUser
	}
	return nil, err
}

// lookup finds the id of the user msg names.
func (w *worker) lookup(ctx context.Context, msg FindUser) (gocql.UUID, error) {
	if msg.UserID != "" {
		if id, ok := ids.Parse(msg.UserID); ok {
			return id, nil
		}
		return gocql.UUID{}, errNoUser
	}
	login := strings.ToLower(strings.TrimSpace(msg.Login))
	switch {
	case login == "" || len(login) > 254:
		return gocql.UUID{}, errNoUser
	case strings.Contains(login, "@"):
		return w.Repo.GetUserIDByEmail(ctx, login)
	default:
		return w.Repo.GetUserIDByUsername(ctx, login)
	}
}

// setDeactivated signs a deactivated user out everywhere at once. Sessions
// it misses, such as one a sign-in racing it creates, are refused anyway:
// authentication rereads the user.
func (w *worker) setDeactivated(ctx context.Context, msg SetDeactivated) (*models.User, error) {
	if !w.Authz.Can(authz.ObjUser, authz.ActDeactivate, string(msg.P.Role)) {
		return nil, ierrors.PermissionDenied()
	}
	id, ok := ids.Parse(msg.UserID)
	if !ok {
		return nil, errNoUser
	}
	if id == msg.P.UserID {
		return nil, ierrors.FailedPrecondition("you cannot deactivate your own account")
	}
	user, err := w.Repo.GetUser(ctx, id)
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return nil, errNoUser
		}
		return nil, err
	}
	if user.Deactivated != msg.Deactivated {
		if err := w.Repo.SetDeactivated(ctx, id, msg.Deactivated, time.Now().UTC()); err != nil {
			return nil, err
		}
		user.Deactivated = msg.Deactivated
		w.Users.Invalidate(id)
		w.Logger.Info("account deactivation changed", zap.String("user", id.String()), zap.Bool("deactivated", msg.Deactivated), zap.String("by", msg.P.UserID.String()))
	}
	// Even when it already was, in case an earlier attempt failed midway.
	if msg.Deactivated {
		hashes, err := w.Repo.DeleteUserSessions(ctx, id, nil)
		w.revoked(id, hashes, true)
		if err != nil {
			return nil, err
		}
		// Their bots stop with them: their sockets close, and their tokens
		// are refused while the owner is deactivated.
		bots, err := w.Repo.ListBotIDs(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, id := range bots {
			bot, err := w.Repo.GetBot(ctx, id)
			if err != nil {
				return nil, err
			}
			w.revoked(id, [][]byte{bot.TokenHash}, true)
		}
	}
	return user, nil
}
