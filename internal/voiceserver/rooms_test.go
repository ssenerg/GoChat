package voiceserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anthdm/hollywood/actor"
	"go.uber.org/zap"

	"github.com/ssenerg/gochat/internal/actors"
)

// stopRooms stops the rooms and waits for them, as the server does.
func stopRooms(t *testing.T, engine *actor.Engine, rooms *actor.PID) {
	t.Helper()
	select {
	case <-engine.Poison(rooms).Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the rooms never stopped")
	}
}

// TestRoomsStopAsARoomEmpties stops the rooms just as a room empties and
// is stopped, which once left the rooms waiting on that room forever.
func TestRoomsStopAsARoomEmpties(t *testing.T) {
	cfg := &Config{MaxPeersPerRoom: 0}
	for range 300 {
		engine, err := actor.NewEngine(actor.NewEngineConfig())
		if err != nil {
			t.Fatal(err)
		}
		rooms := engine.Spawn(newRooms(zap.NewNop(), cfg, nil, nil), "rooms")
		// The room turns the peer away, so it empties at once.
		_, err = actors.Ask[joined](context.Background(), engine, rooms, addPeer{session: "s", channelID: "c", out: newSender(1)})
		if !errors.Is(err, errFull) {
			t.Fatalf("join: %v", err)
		}
		stopRooms(t, engine, rooms)
		if engine.Registry.GetPID("room", "c-1") != nil {
			t.Fatal("the room outlived the rooms")
		}
	}
}

func TestStoppingTheRoomsEndsEveryConnection(t *testing.T) {
	s := newSFU(t, 5)
	a, err := s.join("s-a", "a", true)
	if err != nil {
		t.Fatal(err)
	}
	stopRooms(t, s.engine, s.rooms)
	eventually(t, "the peer is closed", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.closed != nil && a.closed.code == closeUnavailable
	})
}
