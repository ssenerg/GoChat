package files

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"
	"go.uber.org/zap"

	pb "github.com/ssenerg/gochat/api/protobuf/gen/gochat/v1"
	"github.com/ssenerg/gochat/internal/actors"
	"github.com/ssenerg/gochat/internal/authz/authztest"
	"github.com/ssenerg/gochat/internal/core/ids"
	"github.com/ssenerg/gochat/internal/core/models"
	ierrors "github.com/ssenerg/gochat/internal/errors"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/channels"
)

// fakeRepo keeps attachments and usage in memory, charging like the store:
// against the user's own quota, or else the limit given.
type fakeRepo struct {
	mu        sync.Mutex
	users     map[gocql.UUID]bool
	used      map[gocql.UUID]int64
	quotas    map[gocql.UUID]int64
	inserted  []*models.Attachment
	failWrite bool
}

func (f *fakeRepo) InsertAttachment(_ context.Context, a *models.Attachment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWrite {
		return ierrors.Internal(errors.New("write failed"))
	}
	f.inserted = append(f.inserted, a)
	return nil
}

func (f *fakeRepo) GetUser(_ context.Context, id gocql.UUID) (*models.User, error) {
	if !f.users[id] {
		return nil, ierrors.NotFound("not found")
	}
	return &models.User{ID: id}, nil
}

func (f *fakeRepo) StorageUsage(_ context.Context, id gocql.UUID) (int64, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.used[id], f.quotas[id], nil
}

func (f *fakeRepo) ChargeStorage(_ context.Context, id gocql.UUID, size, limit int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if q := f.quotas[id]; q > 0 {
		limit = q
	}
	if limit > 0 && f.used[id]+size > limit {
		return false, nil
	}
	f.used[id] += size
	return true, nil
}

func (f *fakeRepo) ReleaseStorage(_ context.Context, id gocql.UUID, size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.used[id] = max(0, f.used[id]-size)
	return nil
}

func (f *fakeRepo) SetStorageQuota(_ context.Context, id gocql.UUID, quota int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quotas[id] = quota
	return nil
}

func (f *fakeRepo) GetAttachment(_ context.Context, id gocql.UUID) (*models.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.inserted {
		if a.ID == id {
			cp := *a
			return &cp, nil
		}
	}
	return nil, ierrors.NotFound("not found")
}

// post marks an upload as carried by a message.
func (f *fakeRepo) post(id gocql.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.inserted {
		if a.ID == id {
			a.MessageID = ids.NewTime()
		}
	}
}
func (f *fakeRepo) DeleteAttachment(context.Context, *models.Attachment) error { return nil }
func (f *fakeRepo) ListPendingUploads(context.Context, int32) ([]*scylla.PendingUpload, error) {
	return nil, nil
}

// DeleteOrphan deletes an unposted upload, giving its space back, as the
// store does.
func (f *fakeRepo) DeleteOrphan(_ context.Context, id gocql.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, a := range f.inserted {
		if a.ID != id {
			continue
		}
		if !ids.IsZero(a.MessageID) {
			return false, nil
		}
		f.inserted = append(f.inserted[:i], f.inserted[i+1:]...)
		if a.Counted {
			f.used[a.UploaderID] = max(0, f.used[a.UploaderID]-a.Size)
		}
		return true, nil
	}
	return true, nil
}

// fakeStorage records the objects deleted.
type fakeStorage struct {
	mu      sync.Mutex
	deleted []string
}

func (s *fakeStorage) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, key)
	return nil
}
func (f *fakeRepo) ForgetPending(context.Context, *scylla.PendingUpload) error { return nil }

// member answers every access check as a channel member.
type member struct{}

func (member) Receive(c *actor.Context) {
	if _, ok := c.Message().(channels.Access); ok {
		actors.Reply(c, &channels.AccessResult{Channel: &models.Channel{Kind: models.ChannelPublic}, Role: models.ChannelRoleMember})
	}
}

type fixture struct {
	t       *testing.T
	repo    *fakeRepo
	storage *fakeStorage
	engine  *actor.Engine
	pid     *actor.PID
}

func newFixture(t *testing.T, quota int64) *fixture {
	t.Helper()
	engine, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	repo := &fakeRepo{users: map[gocql.UUID]bool{}, used: map[gocql.UUID]int64{}, quotas: map[gocql.UUID]int64{}}
	cfg := &Config{Workers: 2, OpTimeout: 5 * time.Second, MaxSize: 1 << 20, Quota: quota, MaxUploads: 4, MaxUserUploads: 2, OrphanAfter: time.Hour, JanitorInterval: time.Minute}
	storage := &fakeStorage{}
	pid := engine.Spawn(Pool(&Deps{
		Logger: zap.NewNop(), Cfg: cfg, Repo: repo, Storage: storage, Authz: authztest.New(t), Engine: engine,
		Channels: engine.Spawn(func() actor.Receiver { return member{} }, "channels"),
	}), "files")
	t.Cleanup(func() { <-engine.Poison(pid).Done() })
	return &fixture{t: t, repo: repo, storage: storage, engine: engine, pid: pid}
}

func ask[T any](f *fixture, msg any) (T, error) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return actors.Ask[T](ctx, f.engine, f.pid, msg)
}

func principal(role models.SystemRole) *models.Principal {
	return &models.Principal{UserID: ids.New(), Role: role}
}

// upload begins and completes an upload of size bytes.
func (f *fixture) upload(p *models.Principal, size int64) (*Upload, error) {
	f.t.Helper()
	up, err := ask[*Upload](f, BeginUpload{P: p, ChannelID: ids.New().String(), Name: "f.bin"})
	if err != nil {
		return nil, err
	}
	if _, err := ask[*pb.Attachment](f, CompleteUpload{P: p, Upload: up, Size: size, ContentType: "application/octet-stream"}); err != nil {
		return up, err
	}
	return up, nil
}

func TestQuotaBoundsUploads(t *testing.T) {
	f := newFixture(t, 1000)
	p := principal(models.SystemRoleUser)
	up, err := f.upload(p, 600)
	if err != nil || up.Room != 1000 {
		t.Fatalf("first upload: %v, room %d", err, up.Room)
	}
	if !f.repo.inserted[0].Counted {
		t.Fatal("the attachment is not marked as counted")
	}
	up, err = f.upload(p, 600)
	if !errors.Is(err, ErrQuota) || up.Room != 400 {
		t.Fatalf("an upload passed the quota: %v, room %d", err, up.Room)
	}
	if _, err := f.upload(p, 400); err != nil {
		t.Fatalf("an upload filling the quota: %v", err)
	}
	if _, err := f.upload(p, 1); !errors.Is(err, ErrQuota) {
		t.Fatalf("an upload began with the quota full: %v", err)
	}
	u, err := ask[*StorageUsage](f, Usage{P: p})
	if err != nil || u.Used != 1000 || u.Quota != 1000 || u.Custom {
		t.Fatalf("usage: %v %+v", err, u)
	}
}

func TestFailedWriteReleasesItsCharge(t *testing.T) {
	f := newFixture(t, 1000)
	p := principal(models.SystemRoleUser)
	f.repo.failWrite = true
	if _, err := f.upload(p, 600); err == nil {
		t.Fatal("a failed write succeeded")
	}
	if used := f.repo.used[p.UserID]; used != 0 {
		t.Fatalf("a failed upload kept %d bytes charged", used)
	}
}

func TestNoQuota(t *testing.T) {
	f := newFixture(t, 0)
	p := principal(models.SystemRoleUser)
	if up, err := f.upload(p, 1<<40); err != nil || up.Room != -1 {
		t.Fatalf("an upload without a quota: %v %+v", err, up)
	}
}

func TestAdminSetsQuotas(t *testing.T) {
	f := newFixture(t, 1000)
	admin, user, other := principal(models.SystemRoleAdmin), principal(models.SystemRoleUser), principal(models.SystemRoleUser)
	f.repo.users[user.UserID] = true
	if _, err := ask[Done](f, SetQuota{P: other, UserID: user.UserID.String(), Quota: 5000}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("a user set a quota: %v", err)
	}
	if _, err := ask[*StorageUsage](f, Usage{P: other, UserID: user.UserID.String()}); !ierrors.Is(err, ierrors.CodePermissionDenied) {
		t.Fatalf("a user read another's usage: %v", err)
	}
	if _, err := ask[Done](f, SetQuota{P: admin, UserID: ids.New().String(), Quota: 5000}); !ierrors.Is(err, ierrors.CodeNotFound) {
		t.Fatalf("a quota for no one: %v", err)
	}
	if _, err := ask[Done](f, SetQuota{P: admin, UserID: user.UserID.String(), Quota: -1}); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
		t.Fatalf("a negative quota: %v", err)
	}
	if _, err := ask[Done](f, SetQuota{P: admin, UserID: user.UserID.String(), Quota: 5000}); err != nil {
		t.Fatal(err)
	}
	u, err := ask[*StorageUsage](f, Usage{P: admin, UserID: user.UserID.String()})
	if err != nil || u.Quota != 5000 || !u.Custom {
		t.Fatalf("usage after a custom quota: %v %+v", err, u)
	}
	if _, err := f.upload(user, 3000); err != nil {
		t.Fatalf("an upload within the custom quota: %v", err)
	}
	if _, err := ask[Done](f, SetQuota{P: admin, UserID: user.UserID.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.upload(user, 1); !errors.Is(err, ErrQuota) {
		t.Fatalf("the default quota did not come back: %v", err)
	}
}

func TestConfigRefusesNegativeQuota(t *testing.T) {
	cfg := Config{Workers: 1, OpTimeout: time.Second, MaxSize: 1 << 20, MaxVoiceDuration: time.Minute, MaxVoiceSize: 1 << 20, MaxUploads: 1, MaxUserUploads: 1, OrphanAfter: time.Hour, JanitorInterval: time.Minute}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Quota = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("a negative quota was accepted")
	}
}

// A batch that failed partway discards what it stored: the caller's
// unposted uploads go and give their space back, while posted ones and
// others' stay.
func TestDiscardFreesOnlyOwnUnpostedUploads(t *testing.T) {
	f := newFixture(t, 1000)
	p, other := principal(models.SystemRoleUser), principal(models.SystemRoleUser)
	mine, err := f.upload(p, 300)
	if err != nil {
		t.Fatal(err)
	}
	posted, err := f.upload(p, 200)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := f.upload(other, 100)
	if err != nil {
		t.Fatal(err)
	}
	f.repo.post(posted.ID)
	if _, err := ask[Done](f, Discard{P: p, AttachmentIDs: []string{mine.ID.String(), posted.ID.String(), theirs.ID.String(), ids.New().String()}}); err != nil {
		t.Fatal(err)
	}
	if u, _ := ask[*StorageUsage](f, Usage{P: p}); u.Used != 200 {
		t.Errorf("used %d after discarding, want 200", u.Used)
	}
	if u, _ := ask[*StorageUsage](f, Usage{P: other}); u.Used != 100 {
		t.Errorf("someone else's upload was discarded: used %d", u.Used)
	}
	if got := f.storage.deleted; len(got) != 1 || got[0] != mine.ObjectKey {
		t.Errorf("objects deleted: %v, want only %s", got, mine.ObjectKey)
	}
	if _, err := ask[Done](f, Discard{P: p, AttachmentIDs: []string{"nope"}}); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
		t.Errorf("a bad id: %v", err)
	}
	many := make([]string, maxDiscard+1)
	for i := range many {
		many[i] = ids.New().String()
	}
	if _, err := ask[Done](f, Discard{P: p, AttachmentIDs: many}); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
		t.Errorf("too many: %v", err)
	}
}
