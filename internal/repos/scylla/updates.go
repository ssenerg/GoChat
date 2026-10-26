package scylla

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gocql/gocql"
)

// LogEntry is one update of a channel's or an account's log. A message's
// post or edit is kept as a reference, read as the message is now; any
// other update keeps its event.
type LogEntry struct {
	Pts       int64      `db:"pts"`
	Kind      int        `db:"kind"`
	ChannelID gocql.UUID `db:"channel_id"`
	MessageID gocql.UUID `db:"message_id"`
	RootID    gocql.UUID `db:"root_id"`
	Event     []byte     `db:"event"`
}

// logBucketBits splits a log in partitions of 4096 updates.
const logBucketBits = 12

func logBucket(pts int64) int64 { return pts >> logBucketBits }

// A log's table, the column of its owner and its state table.
type updateLog struct {
	table, owner, state string
}

var (
	channelLog = updateLog{table: "channel_updates", owner: "channel_id", state: "channel_state"}
	accountLog = updateLog{table: "account_updates", owner: "user_id", state: "account_state"}
)

// ChannelPts returns the number of the channel's latest update.
func (r *Repository) ChannelPts(ctx context.Context, channelID gocql.UUID) (int64, error) {
	return r.logPts(ctx, channelLog, channelID)
}

// ChannelsPts returns the numbers of the channels' latest updates; a
// channel with none is left out.
func (r *Repository) ChannelsPts(ctx context.Context, channelIDs []gocql.UUID) (map[gocql.UUID]int64, error) {
	out := make(map[gocql.UUID]int64, len(channelIDs))
	for start := 0; start < len(channelIDs); start += 100 {
		var rows []struct {
			ChannelID gocql.UUID `db:"channel_id"`
			Pts       int64      `db:"pts"`
		}
		err := r.query(ctx, "SELECT channel_id, pts FROM channel_state WHERE channel_id IN ?",
			channelIDs[start:min(start+100, len(channelIDs))]).SelectRelease(&rows)
		if err != nil {
			return nil, wrap(err)
		}
		for _, row := range rows {
			out[row.ChannelID] = row.Pts
		}
	}
	return out, nil
}

// AppendChannelUpdate writes e to the channel's log and its number as the
// channel's state, stamped at stamp so a later number always wins.
func (r *Repository) AppendChannelUpdate(ctx context.Context, channelID gocql.UUID, e *LogEntry, stamp int64, ttl time.Duration) error {
	return r.appendLog(ctx, channelLog, channelID, e, stamp, ttl)
}

// ChannelUpdates returns the channel's updates after after, up to upTo,
// oldest first.
func (r *Repository) ChannelUpdates(ctx context.Context, channelID gocql.UUID, after, upTo int64, limit int) ([]*LogEntry, error) {
	return r.readLog(ctx, channelLog, channelID, after, upTo, limit)
}

// DeleteChannelUpdates deletes the channel's log and state, for a channel
// whose latest update is upTo.
func (r *Repository) DeleteChannelUpdates(ctx context.Context, channelID gocql.UUID, upTo int64) error {
	for b := int64(0); b <= logBucket(upTo); b++ {
		if err := r.query(ctx, "DELETE FROM channel_updates WHERE channel_id = ? AND bucket = ?", channelID, b).ExecRelease(); err != nil {
			return wrap(err)
		}
	}
	return wrap(r.query(ctx, "DELETE FROM channel_state WHERE channel_id = ?", channelID).ExecRelease())
}

// AccountPts returns the number of the account's latest update.
func (r *Repository) AccountPts(ctx context.Context, userID gocql.UUID) (int64, error) {
	return r.logPts(ctx, accountLog, userID)
}

func (r *Repository) AppendAccountUpdate(ctx context.Context, userID gocql.UUID, e *LogEntry, stamp int64, ttl time.Duration) error {
	return r.appendLog(ctx, accountLog, userID, e, stamp, ttl)
}

func (r *Repository) AccountUpdates(ctx context.Context, userID gocql.UUID, after, upTo int64, limit int) ([]*LogEntry, error) {
	return r.readLog(ctx, accountLog, userID, after, upTo, limit)
}

// logPts is the log's state, or the newest entry after it: the entry and
// the state are written together, and the state may be what failed.
func (r *Repository) logPts(ctx context.Context, l updateLog, owner gocql.UUID) (int64, error) {
	var pts int64
	err := r.query(ctx, "SELECT pts FROM "+l.state+" WHERE "+l.owner+" = ?", owner).GetRelease(&pts)
	if err != nil && !errors.Is(err, gocql.ErrNotFound) {
		return 0, wrap(err)
	}
	for b := logBucket(pts); b <= logBucket(pts)+1; b++ {
		var newest int64
		err := r.query(ctx, "SELECT pts FROM "+l.table+" WHERE "+l.owner+" = ? AND bucket = ? ORDER BY pts DESC LIMIT 1",
			owner, b).GetRelease(&newest)
		if errors.Is(err, gocql.ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, wrap(err)
		}
		pts = max(pts, newest)
	}
	return pts, nil
}

func (r *Repository) appendLog(ctx context.Context, l updateLog, owner gocql.UUID, e *LogEntry, stamp int64, ttl time.Duration) error {
	secs := max(int(ttl/time.Second), 1)
	b := r.batch(ctx, false)
	// Absent columns are left out, as they would write tombstones.
	cols, vals := l.owner+", bucket, pts, kind", []any{owner, logBucket(e.Pts), e.Pts, e.Kind}
	if l == accountLog && e.ChannelID != (gocql.UUID{}) {
		cols, vals = cols+", channel_id", append(vals, e.ChannelID)
	}
	if e.MessageID != (gocql.UUID{}) {
		cols, vals = cols+", message_id", append(vals, e.MessageID)
	}
	if e.RootID != (gocql.UUID{}) {
		cols, vals = cols+", root_id", append(vals, e.RootID)
	}
	if len(e.Event) > 0 {
		cols, vals = cols+", event", append(vals, e.Event)
	}
	marks := "?" + strings.Repeat(", ?", len(vals)-1)
	b.Query("INSERT INTO "+l.table+" ("+cols+") VALUES ("+marks+") USING TTL ?", append(vals, secs)...)
	b.Query("UPDATE "+l.state+" USING TIMESTAMP ? SET pts = ? WHERE "+l.owner+" = ?", stamp, e.Pts, owner)
	return r.execBatch(b)
}

func (r *Repository) readLog(ctx context.Context, l updateLog, owner gocql.UUID, after, upTo int64, limit int) ([]*LogEntry, error) {
	cols := "pts, kind, message_id, root_id, event"
	if l == accountLog {
		cols += ", channel_id"
	}
	var out []*LogEntry
	for b := logBucket(after + 1); b <= logBucket(upTo) && len(out) < limit; b++ {
		var rows []*LogEntry
		err := r.query(ctx, "SELECT "+cols+" FROM "+l.table+" WHERE "+l.owner+" = ? AND bucket = ? AND pts > ? AND pts <= ? LIMIT ?",
			owner, b, after, upTo, limit-len(out)).SelectRelease(&rows)
		if err != nil {
			return nil, wrap(err)
		}
		out = append(out, rows...)
	}
	return out, nil
}
