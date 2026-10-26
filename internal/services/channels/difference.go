package channels

import (
	"context"

	"github.com/anthdm/hollywood/actor"
	"github.com/gocql/gocql"

	"github.com/ssenerg/gochat/internal/core/models"
	"github.com/ssenerg/gochat/internal/repos/scylla"
	"github.com/ssenerg/gochat/internal/services/updates"
)

// difference replays the channel's updates after msg.Pts to a member, its
// messages as they are now and none of those the member cleared.
func (a *channel) difference(c *actor.Context, msg Difference) error {
	if err := a.canRead(msg.P); err != nil {
		return err
	}
	limit := a.UpdatesCfg.DifferenceLimit
	if msg.Limit > 0 && msg.Limit < limit {
		limit = msg.Limit
	}
	from, upTo, cleared := msg.Pts, a.pts, a.cleared[msg.P.UserID]
	r := &updates.Resolver{
		Messages: a.Repo,
		Render:   Render(a.Deps),
		Channel:  a.id,
		Shown: func(id, root gocql.UUID) bool {
			return shown(&models.Message{ID: id, RootID: root}, cleared)
		},
	}
	a.async(c, func(ctx context.Context) (any, error) {
		return updates.Read(ctx, from, upTo, limit, func(after, upTo int64, limit int) ([]*scylla.LogEntry, error) {
			return a.Repo.ChannelUpdates(ctx, a.id, after, upTo, limit)
		}, r.Resolve)
	})
	return nil
}
