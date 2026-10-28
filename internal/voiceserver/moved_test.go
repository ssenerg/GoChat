//go:build !race

// Pion hands a layer's track a new stream, when it comes on another SSRC,
// without the lock the track's reader takes: the race detector reports it.

package voiceserver

import "testing"

func TestAPeerWhoseLayerMovesIsDropped(t *testing.T) {
	s := newSFU(t, 4)
	cam := &layeredCamera{t: t, movable: true}
	alice, err := s.joinAs("s-alice", "alice", true, hello{video: true, simulcast: true, layers: true}, cam.attach)
	if err != nil {
		t.Fatal(err)
	}
	carol, err := s.joinWith("s-carol", "carol", true, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "carol hears alice", func() bool { return carol.heard("alice") > 10 })
	s.video("s-alice", sourceCamera, true)
	eventually(t, "carol gets the full layer", func() bool { return carol.layer("alice:camera") == 2 })

	// Alice's full layer comes on another SSRC: Pion hands its track over
	// again, and the room lets her go.
	cam.mu.Lock()
	full := cam.full
	cam.mu.Unlock()
	full.move(0x5001)
	eventually(t, "alice is let go", func() bool {
		alice.mu.Lock()
		defer alice.mu.Unlock()
		return alice.closed != nil && alice.closed.code == closeFailed && alice.closed.reason == "a track changed its SSRC"
	})
	eventually(t, "the chat server hears alice left", func() bool { return s.link.has("s-alice") })
	carol.mu.Lock()
	defer carol.mu.Unlock()
	if carol.closed != nil {
		t.Fatalf("carol was let go too: %+v", carol.closed)
	}
}
