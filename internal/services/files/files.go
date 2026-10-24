// Package files decides who may upload to a channel and download from it.
// The bytes themselves stream between the transport and the object store.
package files

import (
	"context"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/authz"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/core/validate"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/channels"
	"github.com/ssenerg/gochat/internal/services/convert"
)

type Repo interface {
	InsertAttachment(ctx context.Context, a *models.Attachment) error
	GetAttachment(ctx context.Context, id gocql.UUID) (*models.Attachment, error)
	DeleteAttachment(ctx context.Context, a *models.Attachment) error
	ListPendingUploads(ctx context.Context, day int32) ([]*scylla.PendingUpload, error)
	DeleteOrphan(ctx context.Context, id gocql.UUID) (bool, error)
	ForgetPending(ctx context.Context, p *scylla.PendingUpload) error
	GetUser(ctx context.Context, id gocql.UUID) (*models.User, error)
	StorageUsage(ctx context.Context, userID gocql.UUID) (used, quota int64, err error)
	ChargeStorage(ctx context.Context, userID gocql.UUID, size, limit int64) (bool, error)
	ReleaseStorage(ctx context.Context, userID gocql.UUID, size int64) error
	SetStorageQuota(ctx context.Context, userID gocql.UUID, quota int64) error
}

type Storage interface {
	Delete(ctx context.Context, key string) error
}

type Deps struct {
	Logger   *zap.Logger
	Cfg      *Config
	Repo     Repo
	Storage  Storage
	Authz    *authz.Authorizer
	Engine   *actor.Engine
	Channels *actor.PID
}

func Pool(d *Deps) actor.Producer {
	return actors.NewPool(d.Cfg.Workers, func() actor.Receiver { return &worker{Deps: d} },
		actor.WithMiddleware(actors.Recover(d.Logger)))
}

type worker struct{ *Deps }

func (w *worker) Receive(c *actor.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), w.Cfg.OpTimeout)
	defer cancel()
	var (
		out any
		err error
	)
	switch msg := c.Message().(type) {
	case BeginUpload:
		out, err = w.begin(ctx, msg)
	case CompleteUpload:
		out, err = w.complete(ctx, msg)
	case OpenDownload:
		out, err = w.open(ctx, msg)
	case Usage:
		out, err = w.usage(ctx, msg)
	case SetQuota:
		out, err = w.setQuota(ctx, msg)
	case Discard:
		out, err = w.discard(ctx, msg)
	default:
		return
	}
	if err != nil {
		if e := ierrors.As(err); e.Code == ierrors.CodeInternal {
			w.Logger.Error("files request failed", e.Fields()...)
		}
		actors.Reply(c, err)
		return
	}
	actors.Reply(c, out)
}

func (w *worker) access(ctx context.Context, p *models.Principal, channelID gocql.UUID) (*channels.AccessResult, error) {
	return actors.Ask[*channels.AccessResult](ctx, w.Engine, w.Channels, channels.Access{Target: channels.Target{P: p, ChannelID: channelID}})
}

func (w *worker) begin(ctx context.Context, msg BeginUpload) (*Upload, error) {
	channelID, ok := ids.Parse(msg.ChannelID)
	if !ok {
		return nil, ierrors.InvalidArgument("invalid channel id")
	}
	acc, err := w.access(ctx, msg.P, channelID)
	if err != nil {
		return nil, err
	}
	if acc.Role == "" {
		return nil, ierrors.NotFound("channel not found")
	}
	if acc.Channel.Kind == models.ChannelVoice {
		return nil, ierrors.FailedPrecondition("voice channels carry no messages")
	}
	if !w.Authz.Can(authz.ObjFile, authz.ActUpload, string(acc.Role), string(msg.P.Role)) {
		return nil, ierrors.PermissionDenied()
	}
	room, err := w.room(ctx, msg.P.UserID)
	if err != nil {
		return nil, err
	}
	if room == 0 {
		return nil, ErrQuota
	}
	up := &Upload{ID: ids.New(), ChannelID: channelID, Name: validate.Filename(msg.Name), Voice: msg.Voice, Room: room}
	up.ObjectKey = "attachments/" + channelID.String() + "/" + up.ID.String()
	if msg.Voice {
		if acc.Channel.VoiceDisabled {
			return nil, ierrors.FailedPrecondition("voice messages are turned off in this channel")
		}
		if time.Duration(msg.DurationMS)*time.Millisecond > w.Cfg.MaxVoiceDuration {
			return nil, ierrors.InvalidArgument("the voice message is too long")
		}
		up.DurationMS = int(msg.DurationMS)
	}
	return up, nil
}

// room is how many more bytes userID may store, -1 for no limit.
func (w *worker) room(ctx context.Context, userID gocql.UUID) (int64, error) {
	used, quota, err := w.Repo.StorageUsage(ctx, userID)
	if err != nil {
		return 0, err
	}
	if quota <= 0 {
		quota = w.Cfg.Quota
	}
	if quota <= 0 {
		return -1, nil
	}
	return max(0, quota-used), nil
}

func (w *worker) complete(ctx context.Context, msg CompleteUpload) (*pb.Attachment, error) {
	u := msg.Upload
	att := &models.Attachment{
		ID:          u.ID,
		ChannelID:   u.ChannelID,
		UploaderID:  msg.P.UserID,
		Name:        u.Name,
		ContentType: msg.ContentType,
		Size:        msg.Size,
		ObjectKey:   u.ObjectKey,
		CreatedAt:   time.Now().UTC(),
		Counted:     true,
		Voice:       u.Voice,
		DurationMS:  u.DurationMS,
	}
	// Charged first, so uploads at once cannot pass the quota together.
	ok, err := w.Repo.ChargeStorage(ctx, msg.P.UserID, msg.Size, w.Cfg.Quota)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrQuota
	}
	if err := w.Repo.InsertAttachment(ctx, att); err != nil {
		// A row that may exist is left to the janitor, which releases it.
		if !scylla.Uncertain(err) {
			w.release(att)
		}
		return nil, err
	}
	return convert.Attachment(att), nil
}

func (w *worker) release(att *models.Attachment) {
	ctx, cancel := context.WithTimeout(context.Background(), w.Cfg.OpTimeout)
	defer cancel()
	if err := w.Repo.ReleaseStorage(ctx, att.UploaderID, att.Size); err != nil {
		w.Logger.Warn("failed to release storage", zap.String("user", att.UploaderID.String()), zap.Error(err))
	}
}

// owner is whose storage a call is about: the caller's, or for someone who
// may manage any, that of raw.
func (w *worker) owner(p *models.Principal, raw string) (gocql.UUID, error) {
	if raw == "" {
		return p.UserID, nil
	}
	id, ok := ids.Parse(raw)
	if !ok {
		return gocql.UUID{}, ierrors.InvalidArgument("invalid user id")
	}
	if id != p.UserID && !w.Authz.Can(authz.ObjStorage, authz.ActManageAny, string(p.Role)) {
		return gocql.UUID{}, ierrors.PermissionDenied()
	}
	return id, nil
}

func (w *worker) usage(ctx context.Context, msg Usage) (*StorageUsage, error) {
	userID, err := w.owner(msg.P, msg.UserID)
	if err != nil {
		return nil, err
	}
	used, quota, err := w.Repo.StorageUsage(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := &StorageUsage{Used: used, Quota: w.Cfg.Quota}
	if quota > 0 {
		out.Quota, out.Custom = quota, true
	}
	return out, nil
}

// maxQuota bounds the quota one user may get, a petabyte.
const maxQuota = 1 << 50

func (w *worker) setQuota(ctx context.Context, msg SetQuota) (Done, error) {
	if !w.Authz.Can(authz.ObjStorage, authz.ActManageAny, string(msg.P.Role)) {
		return Done{}, ierrors.PermissionDenied()
	}
	if msg.Quota < 0 || msg.Quota > maxQuota {
		return Done{}, ierrors.InvalidArgument("the quota is out of range")
	}
	id, ok := ids.Parse(msg.UserID)
	if !ok {
		return Done{}, errNoUser
	}
	if _, err := w.Repo.GetUser(ctx, id); err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return Done{}, errNoUser
		}
		return Done{}, err
	}
	if err := w.Repo.SetStorageQuota(ctx, id, msg.Quota); err != nil {
		return Done{}, err
	}
	w.Logger.Info("storage quota set", zap.String("user", id.String()), zap.Int64("quota", msg.Quota), zap.String("by", msg.P.UserID.String()))
	return Done{}, nil
}

var errNoFile = ierrors.NotFound("file not found")

// discard deletes the caller's unposted uploads among msg's. Others, and
// uploads a message carries, are left alone; the row goes before the
// object, as in the janitor, so a post racing it keeps its file or fails.
func (w *worker) discard(ctx context.Context, msg Discard) (Done, error) {
	if len(msg.AttachmentIDs) > maxDiscard {
		return Done{}, ierrors.InvalidArgument("too many uploads")
	}
	for _, raw := range msg.AttachmentIDs {
		id, ok := ids.Parse(raw)
		if !ok {
			return Done{}, ierrors.InvalidArgument("invalid attachment id")
		}
		att, err := w.Repo.GetAttachment(ctx, id)
		if ierrors.Is(err, ierrors.CodeNotFound) {
			continue
		}
		if err != nil {
			return Done{}, err
		}
		if att.UploaderID != msg.P.UserID || !ids.IsZero(att.MessageID) {
			continue
		}
		orphan, err := w.Repo.DeleteOrphan(ctx, id)
		if err != nil {
			return Done{}, err
		}
		if !orphan {
			continue
		}
		if err := w.Storage.Delete(ctx, att.ObjectKey); err != nil {
			// The janitor finds it on the pending list.
			w.Logger.Warn("failed to delete a discarded upload", zap.String("attachment", id.String()), zap.Error(err))
			continue
		}
		if err := w.Repo.ForgetPending(ctx, &scylla.PendingUpload{ID: att.ID, ObjectKey: att.ObjectKey, CreatedAt: att.CreatedAt}); err != nil {
			w.Logger.Warn("failed to update the pending list", zap.Error(err))
		}
	}
	return Done{}, nil
}

func (w *worker) open(ctx context.Context, msg OpenDownload) (*models.Attachment, error) {
	id, ok := ids.Parse(msg.AttachmentID)
	if !ok {
		return nil, errNoFile
	}
	att, err := w.Repo.GetAttachment(ctx, id)
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) {
			return nil, errNoFile
		}
		return nil, err
	}
	if ids.IsZero(att.MessageID) && att.UploaderID != msg.P.UserID {
		return nil, errNoFile
	}
	acc, err := w.access(ctx, msg.P, att.ChannelID)
	if err != nil {
		if ierrors.Is(err, ierrors.CodeNotFound) || ierrors.Is(err, ierrors.CodePermissionDenied) {
			return nil, errNoFile
		}
		return nil, err
	}
	if acc.Role == "" || !w.Authz.Can(authz.ObjMessage, authz.ActRead, string(acc.Role), string(msg.P.Role)) {
		return nil, errNoFile
	}
	return att, nil
}

type sweep struct{}

// Janitor deletes uploads never posted within OrphanAfter.
func Janitor(d *Deps) actor.Producer {
	return func() actor.Receiver { return &janitor{Deps: d} }
}

type janitor struct {
	*Deps
	repeater *actor.SendRepeater
	running  bool
}

func (j *janitor) Receive(c *actor.Context) {
	switch c.Message().(type) {
	case actor.Started:
		r := c.SendRepeat(c.PID(), sweep{}, j.Cfg.JanitorInterval)
		j.repeater = &r
	case actor.Stopped:
		if j.repeater != nil {
			j.repeater.Stop()
		}
	case sweep:
		if j.running {
			return
		}
		j.running = true
		pid, engine := c.PID(), c.Engine()
		go func() {
			defer engine.Send(pid, swept{})
			j.sweep()
		}()
	case swept:
		j.running = false
	}
}

type swept struct{}

func (j *janitor) sweep() {
	ctx, cancel := context.WithTimeout(context.Background(), j.Cfg.JanitorInterval)
	defer cancel()
	cutoff := time.Now().Add(-j.Cfg.OrphanAfter)
	last := scylla.PendingDay(cutoff)
	removed := 0
	for day := last - 7; day <= last; day++ {
		pending, err := j.Repo.ListPendingUploads(ctx, day)
		if err != nil {
			j.Logger.Warn("janitor failed to list uploads", zap.Error(err))
			return
		}
		for _, p := range pending {
			if p.CreatedAt.After(cutoff) {
				continue
			}
			// The row goes first, and only while unposted, so a post racing
			// the janitor either keeps its file or fails.
			orphan, err := j.Repo.DeleteOrphan(ctx, p.ID)
			if err != nil {
				j.Logger.Warn("janitor failed to delete upload", zap.Error(err))
				continue
			}
			if orphan {
				if err := j.Storage.Delete(ctx, p.ObjectKey); err != nil {
					j.Logger.Warn("janitor failed to delete object", zap.Error(err))
					continue
				}
				removed++
			}
			if err := j.Repo.ForgetPending(ctx, p); err != nil {
				j.Logger.Warn("janitor failed to update the pending list", zap.Error(err))
			}
		}
	}
	if removed > 0 {
		j.Logger.Info("removed orphaned uploads", zap.Int("count", removed))
	}
}
