package scylla

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/gocql/gocql"
	"github.com/scylladb/gocqlx/v3"
	"go.uber.org/zap"

	ierrors "github.com/ssenerg/gochat/internal/errors"
)

type Repository struct {
	logger  *zap.Logger
	session gocqlx.Session
	// serial is the consistency of reads that must see every conditional
	// write, including one still in progress.
	serial gocql.Consistency
}

// NewRepository connects to Scylla, waiting for it until ctx ends.
func NewRepository(ctx context.Context, logger *zap.Logger, cfg *Config) (*Repository, error) {
	session, err := Open(ctx, logger, cfg)
	if err != nil {
		return nil, err
	}
	serial, _ := parseSerial(cfg.SerialConsistency)
	return &Repository{logger: logger, session: session, serial: serial}, nil
}

func (r *Repository) Stop() {
	r.session.Close()
	r.logger.Info("session closed")
}

func (r *Repository) Ping(ctx context.Context) error {
	var now int64
	if err := r.session.ContextQuery(ctx, "SELECT toUnixTimestamp(now()) FROM system.local", nil).Scan(&now); err != nil {
		return err
	}
	return nil
}

func (r *Repository) query(ctx context.Context, stmt string, args ...any) *gocqlx.Queryx {
	return r.session.ContextQuery(ctx, stmt, nil).Bind(args...)
}

// batch groups writes; logged batches are atomic across partitions.
func (r *Repository) batch(ctx context.Context, logged bool) *gocql.Batch {
	kind := gocql.UnloggedBatch
	if logged {
		kind = gocql.LoggedBatch
	}
	return r.session.Session.Batch(kind).WithContext(ctx)
}

func (r *Repository) execBatch(b *gocql.Batch) error {
	return wrap(r.session.Session.ExecuteBatch(b))
}

// claim inserts a row IF NOT EXISTS, owner bound last, and reports whether
// the row's column holds owner: an earlier attempt that applied counts. A
// write of unknown outcome is settled with a serial read of check.
func (r *Repository) claim(ctx context.Context, insert, check, column string, owner any, keys ...any) (bool, error) {
	row := make(map[string]any)
	q := r.query(ctx, insert, slices.Concat(keys, []any{owner})...)
	applied, err := q.MapScanCAS(row)
	q.Release()
	if err == nil {
		return applied || row[column] == owner, nil
	}
	if !uncertain(err) {
		return false, wrap(err)
	}
	// The caller's deadline may be what failed the write.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	row = make(map[string]any)
	q = r.query(cctx, check, keys...)
	q.Consistency(r.serial)
	rerr := q.MapScan(row)
	q.Release()
	switch {
	case rerr == nil:
		return row[column] == owner, nil
	case errors.Is(rerr, gocql.ErrNotFound):
		return false, wrap(err)
	default:
		return false, wrap(rerr)
	}
}

// Uncertain reports whether a failed write may still have been applied.
func Uncertain(err error) bool {
	return uncertain(err)
}

func uncertain(err error) bool {
	var qe *gocql.QueryError
	if errors.As(err, &qe) && qe.PotentiallyExecuted() {
		return true
	}
	var wt *gocql.RequestErrWriteTimeout
	var cu *gocql.RequestErrCASWriteUnknown
	return errors.As(err, &wt) || errors.As(err, &cu) ||
		errors.Is(err, gocql.ErrTimeoutNoResponse) || errors.Is(err, context.DeadlineExceeded)
}

// compensate runs an undo of a failed write, unless the write may have
// applied: undoing then could leave its effects without their guard.
func (r *Repository) compensate(err error, undo func(ctx context.Context)) {
	if uncertain(err) {
		r.logger.Warn("write of unknown outcome left as is", zap.Error(err))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	undo(ctx)
}

func wrap(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gocql.ErrNotFound):
		return ierrors.NotFound("not found")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &ierrors.Error{Code: ierrors.CodeDeadlineExceeded, Message: "request timed out", Cause: err}
	default:
		return ierrors.Internal(err)
	}
}
