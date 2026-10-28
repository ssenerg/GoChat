// Package actors holds what every hollywood actor of the application shares:
// request and reply, a worker pool, children, panic recovery and event
// logging.
package actors

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anthdm/hollywood/actor"

	ierrors "github.com/ssenerg/gochat/internal/errors"
)

const askPrefix = "ask/"

// DefaultTimeout bounds an Ask whose context carries no deadline.
const DefaultTimeout = 10 * time.Second

var askSeq atomic.Uint64

// replyProc receives exactly one reply. Hollywood's own Response draws its id
// from a 31-bit random number, and two requests sharing one would see each
// other's replies; this id is a process-wide counter instead.
type replyProc struct {
	pid *actor.PID
	ch  chan any
}

func (r *replyProc) Start()                  {}
func (r *replyProc) PID() *actor.PID         { return r.pid }
func (r *replyProc) Invoke([]actor.Envelope) {}
func (r *replyProc) Shutdown()               {}
func (r *replyProc) Send(_ *actor.PID, msg any, _ *actor.PID) {
	select {
	case r.ch <- msg:
	default:
	}
}

// Ask sends msg to pid and waits for its reply, which is either a T or an
// error.
func Ask[T any](ctx context.Context, e *actor.Engine, pid *actor.PID, msg any) (T, error) {
	var zero T
	if pid == nil {
		return zero, ierrors.Unavailable("service unavailable")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	rp := &replyProc{
		pid: actor.NewPID(e.Address(), askPrefix+strconv.FormatUint(askSeq.Add(1), 36)),
		ch:  make(chan any, 1),
	}
	e.SpawnProc(rp)
	defer e.Registry.Remove(rp.pid)
	e.SendWithSender(pid, msg, rp.pid)

	select {
	case v := <-rp.ch:
		if err, ok := v.(error); ok {
			return zero, err
		}
		t, ok := v.(T)
		if !ok {
			return zero, ierrors.Internal(fmt.Errorf("unexpected reply %T to %T", v, msg))
		}
		return t, nil
	case <-ctx.Done():
		return zero, &ierrors.Error{Code: ierrors.CodeDeadlineExceeded, Message: "request timed out", Cause: ctx.Err()}
	}
}

// IsAsk reports whether pid waits for a reply from Ask.
func IsAsk(pid *actor.PID) bool {
	return pid != nil && strings.HasPrefix(pid.ID, askPrefix)
}

// Reply answers the current message when its sender waits for one.
func Reply(c *actor.Context, v any) {
	if IsAsk(c.Sender()) {
		c.Respond(v)
	}
}
