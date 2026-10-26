package scylla

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gocql/gocql"
	"go.uber.org/zap"

	"github.com/ssenerg/gochat/internal/core/ids"
)

// liveRepository opens the migrated keyspace at GOCHAT_TEST_SCYLLA, such as
// 127.0.0.1:9042, or skips.
func liveRepository(t *testing.T) *Repository {
	t.Helper()
	host := os.Getenv("GOCHAT_TEST_SCYLLA")
	if host == "" {
		t.Skip("GOCHAT_TEST_SCYLLA is not set")
	}
	r, err := NewRepository(zap.NewNop(), &Config{
		Hosts: []string{host}, Keyspace: "gochat", Consistency: "local_quorum", SerialConsistency: "local_serial",
		Timeout: 10 * time.Second, ConnectTimeout: 10 * time.Second, NumConns: 1, DisableHostLookup: true, TLS: &TLSConfig{},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Stop)
	return r
}

func TestUpdateLogLive(t *testing.T) {
	r := liveRepository(t)
	ctx := context.Background()
	channel := ids.New()
	if pts, err := r.ChannelPts(ctx, channel); err != nil || pts != 0 {
		t.Fatalf("a new channel's pts %d, %v", pts, err)
	}
	// Across a bucket's end, with the state's write lost for the last one.
	stamp := time.Now().UnixMicro()
	for _, pts := range []int64{4094, 4095, 4096, 4097} {
		e := &LogEntry{Pts: pts, Kind: 0, Event: []byte{byte(pts)}}
		if pts == 4097 {
			e.Kind, e.MessageID, e.Event = 1, ids.NewTime(), nil
		}
		if err := r.AppendChannelUpdate(ctx, channel, e, stamp+pts, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.query(ctx, "UPDATE channel_state USING TIMESTAMP ? SET pts = 4096 WHERE channel_id = ?", stamp+5000, channel).ExecRelease(); err != nil {
		t.Fatal(err)
	}
	if pts, err := r.ChannelPts(ctx, channel); err != nil || pts != 4097 {
		t.Fatalf("pts %d, %v; want the newest entry, 4097", pts, err)
	}
	got, err := r.ChannelUpdates(ctx, channel, 4094, 4097, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Pts != 4095 || got[2].Pts != 4097 || got[2].Kind != 1 || ids.IsZero(got[2].MessageID) {
		t.Fatalf("read %d entries: %+v", len(got), got)
	}
	if got, err := r.ChannelUpdates(ctx, channel, 4094, 4097, 2); err != nil || len(got) != 2 || got[1].Pts != 4096 {
		t.Fatalf("limited read: %+v, %v", got, err)
	}
	all, err := r.ChannelsPts(ctx, []gocql.UUID{channel, ids.New()})
	if err != nil || len(all) != 1 || all[channel] != 4096 {
		t.Fatalf("states %v, %v", all, err)
	}
	if err := r.DeleteChannelUpdates(ctx, channel, 4097); err != nil {
		t.Fatal(err)
	}
	if pts, err := r.ChannelPts(ctx, channel); err != nil || pts != 0 {
		t.Fatalf("pts after deleting %d, %v", pts, err)
	}

	user, chID := ids.New(), ids.New()
	if err := r.AppendAccountUpdate(ctx, user, &LogEntry{Pts: 1, Kind: 1, ChannelID: chID, MessageID: ids.NewTime()}, stamp, time.Hour); err != nil {
		t.Fatal(err)
	}
	acc, err := r.AccountUpdates(ctx, user, 0, 1, 10)
	if err != nil || len(acc) != 1 || acc[0].ChannelID != chID {
		t.Fatalf("account log %+v, %v", acc, err)
	}
}
