package accounts

import (
	"context"
	"strings"
	"time"

	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/secure"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/convert"
	"github.com/ssenerg/gochat/internal/services/hub"
)

// DeletedName is what others see of a deleted account.
const DeletedName = "Deleted user"

var errOnlyAdmin = ierrors.FailedPrecondition("you are this server's only system admin, so you cannot delete your account")

func (w *worker) deleteAccount(ctx context.Context, msg DeleteAccount) (Done, error) {
	if msg.P.Bot {
		return Done{}, ierrors.PermissionDenied()
	}
	user, err := w.reauthenticate(ctx, msg)
	if err != nil {
		return Done{}, err
	}
	if user.TOTPEnabled {
		if strings.TrimSpace(msg.Code) == "" {
			return Done{}, ierrors.InvalidArgument("enter a code from your authenticator app, or a recovery code")
		}
		ok, err := w.checkSecondFactor(ctx, user, msg.Code, time.Now())
		if err != nil {
			return Done{}, err
		}
		if !ok {
			w.failures.Add("reauth:" + user.ID.String())
			return Done{}, ierrors.InvalidArgument("invalid code")
		}
	}
	return Done{}, w.keepAnAdmin(ctx, user)
}

// reauthenticate proves the caller owns the account: by its password, or
// for one without, by a sign-in at its provider that just finished.
func (w *worker) reauthenticate(ctx context.Context, msg DeleteAccount) (*models.User, error) {
	user, err := w.Repo.GetUser(ctx, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	if user.PasswordHash != "" {
		return w.verifyPassword(ctx, msg.P, msg.Password)
	}
	if msg.SSOToken == "" {
		return nil, ierrors.FailedPrecondition("this account has no password: sign in again with single sign-on to delete it")
	}
	t, err := w.takeTicket(ctx, msg.SSOToken, models.SSOTicketReauth)
	if err != nil {
		return nil, err
	}
	// A ticket is spent either way, so a stolen one is no use to guess with.
	v := msg.SSOVerifier
	if t.UserID != user.ID || (t.Challenge != "" && (len(v) < 43 || len(v) > 128 || !secure.Equal(s256(v), t.Challenge))) {
		w.Logger.Warn("sso reauth refused", zap.String("user", user.ID.String()))
		return nil, errSSOExpired
	}
	return user, nil
}

// keepAnAdmin refuses to delete the last active system admin.
func (w *worker) keepAnAdmin(ctx context.Context, user *models.User) error {
	if user.Role != models.SystemRoleAdmin {
		return nil
	}
	admins, err := w.Repo.ListSystemAdmins(ctx)
	if err != nil {
		return err
	}
	for _, id := range admins {
		if id != user.ID {
			return nil
		}
	}
	return errOnlyAdmin
}

// eraseAccount deletes the account for good. Its messages keep its row,
// under DeletedName. Once the row is written what follows is best effort,
// since no one can sign in to try again.
func (w *worker) eraseAccount(ctx context.Context, msg EraseAccount) (Done, error) {
	user, err := w.Repo.GetUser(ctx, msg.P.UserID)
	if err != nil {
		return Done{}, err
	}
	if user.DeletedAt.IsZero() {
		if err := w.erase(ctx, user); err != nil {
			return Done{}, err
		}
	}
	w.Users.Invalidate(user.ID)
	linked, err := w.Repo.ListUserSSOIdentities(ctx, user.ID)
	for _, id := range linked {
		if err == nil {
			err = w.Repo.UnlinkSSOIdentity(ctx, id)
		}
	}
	if err != nil {
		w.Logger.Warn("failed to unlink a deleted account's sign-ins", zap.String("user", user.ID.String()), zap.Error(err))
	}
	hashes, err := w.Repo.DeleteUserSessions(ctx, user.ID, nil)
	w.revoked(user.ID, hashes, true)
	if err != nil {
		w.Logger.Warn("failed to end a deleted account's sessions", zap.String("user", user.ID.String()), zap.Error(err))
	}
	if u, err := w.Repo.GetUser(ctx, user.ID); err == nil {
		w.Engine.Send(w.Hub, hub.About{UserID: u.ID.String(), Event: &pb.Event{Payload: &pb.Event_ProfileChanged{
			ProfileChanged: &pb.ProfileChanged{User: convert.PublicUser(u)},
		}}})
	}
	w.Logger.Info("account deleted", zap.String("user", user.ID.String()))
	return Done{}, nil
}

// erase writes the deletion, holding the system admins' changes for one.
func (w *worker) erase(ctx context.Context, user *models.User) error {
	if user.Role == models.SystemRoleAdmin {
		unlock, err := w.admins.Lock(ctx, "system")
		if err != nil {
			return ierrors.Unavailable("busy, try again")
		}
		defer unlock()
		if err := w.keepAnAdmin(ctx, user); err != nil {
			return err
		}
	}
	return w.Repo.EraseUser(ctx, user, DeletedName, time.Now().UTC())
}
