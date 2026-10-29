// Package push reaches people's apps while they are closed, through FCM on
// Android and APNs on iOS: it keeps each session's device token and pushes
// what the apps would show a notice for, to people with no connection open.
package push

import (
	"context"
	"regexp"
	"slices"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/services/users"
)

type Repo interface {
	PutPushDevice(ctx context.Context, d *models.PushDevice, expires time.Time) error
	GetPushDevice(ctx context.Context, userID gocql.UUID, sessionHash []byte) (*models.PushDevice, error)
	ListPushDevices(ctx context.Context, userID gocql.UUID) ([]*models.PushDevice, error)
	PushTokenHolder(ctx context.Context, platform, token string) (gocql.UUID, []byte, error)
	DeletePushDevice(ctx context.Context, d *models.PushDevice) error
	ForgetPushToken(ctx context.Context, d *models.PushDevice) error
	DeletePushToken(ctx context.Context, platform, token string) error

	GetSession(ctx context.Context, hash []byte) (*models.Session, error)
	GetUserIDByUsername(ctx context.Context, username string) (gocql.UUID, error)
	IsMuted(ctx context.Context, userID, channelID gocql.UUID) (bool, error)
}

type Deps struct {
	Logger *zap.Logger
	Cfg    *Config
	Repo   Repo
	Users  *users.Cache
	Engine *actor.Engine
	Hub    *actor.PID
	// Providers holds those turned on, by platform.
	Providers map[string]Provider
	// IdleTimeout is the sessions': a device whose session went unused
	// that long gets no more pushes.
	IdleTimeout time.Duration

	fan *fanOut
}

// Register replaces the device of P's session; it replies with Registered.
type Register struct {
	P        *models.Principal
	Platform pb.PushPlatform
	Token    string
	Sandbox  bool
}

// Registered says whether the platform's pushes are on.
type Registered bool

// Unregister forgets the device of P's session; it replies with Done.
type Unregister struct{ P *models.Principal }

// Forget drops the devices of revoked sessions, or with All every device
// of the user.
type Forget struct {
	UserID gocql.UUID
	Hashes [][]byte
	All    bool
}

type Done struct{}

func Pool(d *Deps) actor.Producer {
	d.fan = newFanOut()
	return actors.NewPool(d.Cfg.Workers, func() actor.Receiver { return &worker{Deps: d} },
		actor.WithMiddleware(actors.Recover(d.Logger)))
}

type worker struct{ *Deps }

func (w *worker) Receive(c *actor.Context) {
	var (
		out any
		err error
	)
	switch msg := c.Message().(type) {
	case Posted:
		w.posted(msg)
		return
	case Forget:
		ctx, cancel := context.WithTimeout(context.Background(), w.Cfg.OpTimeout)
		w.forget(ctx, msg)
		cancel()
		return
	case Register:
		ctx, cancel := context.WithTimeout(context.Background(), w.Cfg.OpTimeout)
		out, err = w.register(ctx, msg)
		cancel()
	case Unregister:
		ctx, cancel := context.WithTimeout(context.Background(), w.Cfg.OpTimeout)
		out, err = w.unregister(ctx, msg.P)
		cancel()
	default:
		return
	}
	if err != nil {
		if e := ierrors.As(err); e.Code == ierrors.CodeInternal {
			w.Logger.Error("push request failed", e.Fields()...)
		}
		actors.Reply(c, err)
		return
	}
	actors.Reply(c, out)
}

var (
	// FCM's registration tokens, and APNs device tokens in hexadecimal.
	fcmTokenRe  = regexp.MustCompile(`^[A-Za-z0-9_:.-]+$`)
	apnsTokenRe = regexp.MustCompile(`^[0-9a-f]+$`)
)

// validToken reports whether token is of platform's form.
func validToken(platform, token string) bool {
	switch platform {
	case models.PushFCM:
		return len(token) >= 20 && len(token) <= 4096 && fcmTokenRe.MatchString(token)
	case models.PushAPNs:
		return len(token)%2 == 0 && len(token) >= 64 && len(token) <= 200 && apnsTokenRe.MatchString(token)
	}
	return false
}

func platformOf(p pb.PushPlatform) string {
	switch p {
	case pb.PushPlatform_PUSH_PLATFORM_FCM:
		return models.PushFCM
	case pb.PushPlatform_PUSH_PLATFORM_APNS:
		return models.PushAPNs
	}
	return ""
}

func (w *worker) register(ctx context.Context, msg Register) (Registered, error) {
	if msg.P.Bot {
		return false, ierrors.PermissionDenied()
	}
	platform := platformOf(msg.Platform)
	if !validToken(platform, msg.Token) {
		return false, ierrors.InvalidArgument("invalid device token")
	}
	if w.Providers[platform] == nil {
		return false, nil
	}
	sandbox := platform == models.PushAPNs && msg.Sandbox
	// The token moves from any other session, such as the device's last
	// sign-in, so one device never hears of two.
	userID, hash, err := w.Repo.PushTokenHolder(ctx, platform, msg.Token)
	if err != nil {
		return false, err
	}
	if hash != nil && (userID != msg.P.UserID || string(hash) != string(msg.P.SessionHash)) {
		held, err := w.Repo.GetPushDevice(ctx, userID, hash)
		switch {
		case ierrors.Is(err, ierrors.CodeNotFound):
		case err != nil:
			return false, err
		case held.Platform == platform && held.Token == msg.Token:
			if err := w.Repo.ForgetPushToken(ctx, held); err != nil {
				return false, err
			}
		}
	}
	if err := w.makeRoom(ctx, msg.P); err != nil {
		return false, err
	}
	old, err := w.Repo.GetPushDevice(ctx, msg.P.UserID, msg.P.SessionHash)
	switch {
	case ierrors.Is(err, ierrors.CodeNotFound):
	case err != nil:
		return false, err
	case old.Platform != platform || old.Token != msg.Token:
		if err := w.Repo.DeletePushToken(ctx, old.Platform, old.Token); err != nil {
			return false, err
		}
	}
	d := &models.PushDevice{
		UserID: msg.P.UserID, SessionHash: msg.P.SessionHash, Platform: platform,
		Token: msg.Token, Sandbox: sandbox, CreatedAt: time.Now().UTC(),
	}
	if err := w.Repo.PutPushDevice(ctx, d, msg.P.ExpiresAt); err != nil {
		return false, err
	}
	return true, nil
}

// maxDevices bounds a person's devices: each push goes to every one.
const maxDevices = 10

// makeRoom forgets the person's oldest devices but the session's own, so
// that with its own they have maxDevices at most.
func (w *worker) makeRoom(ctx context.Context, p *models.Principal) error {
	list, err := w.Repo.ListPushDevices(ctx, p.UserID)
	if err != nil {
		return err
	}
	others := slices.DeleteFunc(list, func(d *models.PushDevice) bool { return string(d.SessionHash) == string(p.SessionHash) })
	extra := len(others) - (maxDevices - 1)
	if extra <= 0 {
		return nil
	}
	slices.SortFunc(others, func(a, b *models.PushDevice) int { return a.CreatedAt.Compare(b.CreatedAt) })
	for _, d := range others[:extra] {
		if err := w.Repo.DeletePushDevice(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

func (w *worker) unregister(ctx context.Context, p *models.Principal) (Done, error) {
	return Done{}, w.drop(ctx, p.UserID, p.SessionHash)
}

// drop forgets a session's device, if it has one.
func (w *worker) drop(ctx context.Context, userID gocql.UUID, hash []byte) error {
	d, err := w.Repo.GetPushDevice(ctx, userID, hash)
	if ierrors.Is(err, ierrors.CodeNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return w.Repo.DeletePushDevice(ctx, d)
}

func (w *worker) forget(ctx context.Context, msg Forget) {
	var err error
	if msg.All {
		var list []*models.PushDevice
		if list, err = w.Repo.ListPushDevices(ctx, msg.UserID); err == nil {
			for _, d := range list {
				if err = w.Repo.DeletePushDevice(ctx, d); err != nil {
					break
				}
			}
		}
	} else {
		for _, h := range msg.Hashes {
			if err = w.drop(ctx, msg.UserID, h); err != nil {
				break
			}
		}
	}
	if err != nil {
		w.Logger.Warn("failed to forget the devices of revoked sessions", zap.String("user", msg.UserID.String()), zap.Error(err))
	}
}
