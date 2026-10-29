package safety

import (
	"context"
	"time"

	"github.com/anthdm/hollywood/actor"
	"go.uber.org/zap"
)

// copyKey holds "done" once blocked_by holds the blocks made before it.
const (
	copyKey     = "blocks.blocked_by.v1"
	copyDone    = "done"
	copyRetry   = 30 * time.Second
	copyTimeout = 10 * time.Minute
)

// CopyBlocks fills blocked_by from the blocks made before it, once, and
// then tells the blocks cache to read who blocked each person from it.
func CopyBlocks(d *Deps) actor.Producer {
	return func() actor.Receiver { return &copier{Deps: d} }
}

type copier struct {
	*Deps
	cancel context.CancelFunc
}

func (c *copier) Receive(ctx *actor.Context) {
	switch ctx.Message().(type) {
	case actor.Started:
		run, cancel := context.WithCancel(context.Background())
		c.cancel = cancel
		go c.run(run)
	case actor.Stopped:
		if c.cancel != nil {
			c.cancel()
		}
	}
}

func (c *copier) run(ctx context.Context) {
	for {
		err := c.copy(ctx)
		if err == nil {
			c.Blocks.Copied()
			return
		}
		if ctx.Err() != nil {
			return
		}
		c.Logger.Warn("failed to copy blocks to blocked_by", zap.Error(err))
		t := time.NewTimer(copyRetry)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return
		}
	}
}

func (c *copier) copy(ctx context.Context) error {
	octx, cancel := context.WithTimeout(ctx, c.Cfg.OpTimeout)
	state, err := c.Repo.GetState(octx, copyKey)
	cancel()
	if err != nil || state == copyDone {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, copyTimeout)
	defer cancel()
	n, err := c.Repo.CopyBlocks(cctx)
	if err != nil {
		return err
	}
	octx, cancel = context.WithTimeout(ctx, c.Cfg.OpTimeout)
	defer cancel()
	if err := c.Repo.SetState(octx, copyKey, copyDone); err != nil {
		return err
	}
	c.Logger.Info("copied blocks to blocked_by", zap.Int("blocks", n))
	return nil
}
