package voiceserver

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// camera plays a browser's camera and screen: on its first offer it puts
// both on the slots, which it answers sendonly, and keeps sending them,
// whether the room forwards them or not, as a browser does. It counts the
// requests for a keyframe each gets.
type camera struct {
	t    *testing.T
	pc   *webrtc.PeerConnection
	mu   sync.Mutex
	mids map[string]string
	plis map[string]int
}

func (cam *camera) attach(c *client, sl *slots) {
	cam.mu.Lock()
	defer cam.mu.Unlock()
	if cam.mids != nil {
		return
	}
	cam.pc, cam.mids, cam.plis = c.pc, map[string]string{}, map[string]int{}
	for source, mid := range map[string]string{sourceCamera: sl.Camera, sourceScreen: sl.Screen} {
		track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, source, "local")
		if err != nil {
			cam.t.Error(err)
			return
		}
		// Pion made the slot sendonly when it took the offer, as the
		// recvonly side's peer; the track keeps it so.
		var slot *webrtc.RTPTransceiver
		for _, tr := range cam.pc.GetTransceivers() {
			if tr.Mid() == mid {
				slot = tr
			}
		}
		if slot == nil || slot.Direction() != webrtc.RTPTransceiverDirectionSendonly {
			cam.t.Errorf("no sendonly slot for the %s at mid %q", source, mid)
			return
		}
		s, err := c.api.NewRTPSender(track, cam.pc.SCTP().Transport())
		if err != nil {
			cam.t.Error(err)
			return
		}
		if err := slot.SetSender(s, track); err != nil {
			cam.t.Error(err)
			return
		}
		cam.mids[source] = mid
		go func() {
			for cam.pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
				if err := track.WriteSample(media.Sample{Data: make([]byte, 600), Duration: 33 * time.Millisecond}); err != nil {
					return
				}
				time.Sleep(33 * time.Millisecond)
			}
		}()
		go func() {
			for {
				pkts, _, err := s.ReadRTCP()
				if err != nil {
					return
				}
				for _, pkt := range pkts {
					if _, ok := pkt.(*rtcp.PictureLossIndication); ok {
						cam.mu.Lock()
						cam.plis[source]++
						cam.mu.Unlock()
					}
				}
			}
		}()
	}
}

// sendsOnSlots names a slot an offer sends something on: the server only
// ever receives on them.
func sendsOnSlots(offer string, sl *slots) string {
	var d sdp.SessionDescription
	if err := d.UnmarshalString(offer); err != nil {
		return err.Error()
	}
	for _, m := range d.MediaDescriptions {
		mid, _ := m.Attribute("mid")
		if mid != sl.Camera && mid != sl.Screen {
			continue
		}
		if _, ok := m.Attribute("recvonly"); !ok {
			return "mid " + mid
		}
	}
	return ""
}

func (cam *camera) pli(source string) int {
	cam.mu.Lock()
	defer cam.mu.Unlock()
	return cam.plis[source]
}

// joinCamera joins a client that sends and shows video, with a camera.
func (s *sfu) joinCamera(session, userID string) (*client, *camera) {
	cam := &camera{t: s.t}
	c, err := s.joinWith(session, userID, true, true, cam.attach)
	if err != nil {
		s.t.Fatal(err)
	}
	return c, cam
}

// stops reports whether a stream stopped coming: packets already on their
// way may still land, however slowly a loaded machine delivers them.
func (c *client) stops(streamID string) bool {
	before := c.heard(streamID)
	time.Sleep(250 * time.Millisecond)
	return c.heard(streamID) == before
}

func (s *sfu) video(session, source string, on bool) {
	s.engine.Send(s.rooms, toRoom{channelID: "room", msg: video{session: session, source: source, on: on}})
}

func TestRoomForwardsCamerasAndScreens(t *testing.T) {
	s := newSFU(t, 3)
	alice, aliceCam := s.joinCamera("s-alice", "alice")
	bob, _ := s.joinCamera("s-bob", "bob")
	// carol's app does not do video: she hears the others and sees nothing.
	carol, err := s.join("s-carol", "carol", true)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "everyone hears alice", func() bool { return bob.heard("alice") > 10 && carol.heard("alice") > 10 })

	// Every offer to one who does video names its slots; to carol, none.
	bob.mu.Lock()
	if len(bob.slots) == 0 || bob.slots[0] == nil || bob.slots[0].Camera == "" || bob.slots[0].Screen == "" {
		t.Errorf("bob's first offer named no slots: %v", bob.slots)
	}
	bob.mu.Unlock()
	carol.mu.Lock()
	for _, sl := range carol.slots {
		if sl != nil {
			t.Errorf("carol was offered slots: %+v", *sl)
		}
	}
	carol.mu.Unlock()

	// Alice's camera comes in from the start, but nobody gets it until she
	// says she sends it.
	time.Sleep(500 * time.Millisecond)
	if bob.heard("alice:camera") != 0 {
		t.Fatal("bob got alice's camera before she turned it on")
	}

	s.video("s-alice", sourceCamera, true)
	eventually(t, "bob sees alice's camera", func() bool { return bob.heard("alice:camera") > 10 })
	eventually(t, "bob gets reports on it", func() bool { return bob.reported("alice:camera") > 0 })
	eventually(t, "the chat server hears alice's camera is on", func() bool { return s.link.sends("s-alice", true, false) })
	eventually(t, "alice is asked for a keyframe for bob", func() bool { return aliceCam.pli(sourceCamera) > 0 })
	if carol.heard("alice:camera") != 0 || alice.heard("alice:camera") != 0 {
		t.Fatal("alice's camera went to carol, who does no video, or back to alice")
	}

	// Bob's browser asking for a keyframe reaches alice.
	plis := aliceCam.pli(sourceCamera)
	time.Sleep(keyframeGap)
	bob.mu.Lock()
	remote := bob.remotes["alice:camera"]
	bob.mu.Unlock()
	if err := bob.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(remote.SSRC())}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob's request reaches alice", func() bool { return aliceCam.pli(sourceCamera) > plis })

	// Her screen goes out beside it.
	s.video("s-alice", sourceScreen, true)
	eventually(t, "bob sees alice's screen", func() bool { return bob.heard("alice:screen") > 10 })
	eventually(t, "the chat server hears alice is live", func() bool { return s.link.sends("s-alice", true, true) })

	// The room forwards two at most here: bob's camera is refused, and
	// goes to nobody.
	s.video("s-bob", sourceCamera, true)
	eventually(t, "bob is told his camera is refused", func() bool {
		bob.mu.Lock()
		defer bob.mu.Unlock()
		return len(bob.refused) == 1 && bob.refused[0] == sourceCamera
	})
	if alice.heard("bob:camera") != 0 || s.link.sends("s-bob", true, false) {
		t.Fatal("a camera past the cap went out")
	}

	// Turning the camera off stops it for bob and leaves the screen.
	s.video("s-alice", sourceCamera, false)
	eventually(t, "bob stops getting alice's camera", func() bool { return bob.stops("alice:camera") })
	eventually(t, "the chat server hears alice's camera is off", func() bool { return s.link.sends("s-alice", false, true) })
	screen := bob.heard("alice:screen")
	eventually(t, "the screen goes on", func() bool { return bob.heard("alice:screen") > screen+10 })

	// And on again, on the transceiver it left, which still sends reports
	// and so answers NACKs: Pion's interceptors carry it.
	camera, reports := bob.heard("alice:camera"), bob.reported("alice:camera")
	s.video("s-alice", sourceCamera, true)
	eventually(t, "bob sees alice's camera again", func() bool { return bob.heard("alice:camera") > camera+10 })
	eventually(t, "bob gets reports on it again", func() bool { return bob.reported("alice:camera") > reports })

	// A video message from one without slots changes nothing.
	s.video("s-carol", sourceCamera, true)

	// Nothing ever went to bob on his own slots, or took another's place.
	bob.mu.Lock()
	if len(bob.slotSends) > 0 || len(bob.swaps) > 0 {
		t.Errorf("offers sent to bob on his slots, %v, or swapped tracks, %v", bob.slotSends, bob.swaps)
	}
	bob.mu.Unlock()

	// When alice leaves, both go.
	s.engine.Send(s.rooms, disconnect{channelID: "room", session: "s-alice", reason: "left"})
	eventually(t, "bob stops getting alice's camera and screen", func() bool {
		return bob.stops("alice:camera") && bob.stops("alice:screen")
	})
	if bob.heard("carol") == 0 {
		t.Fatal("bob lost carol")
	}
}

// gate holds a client's answers while it is closed.
type gate struct {
	mu      sync.Mutex
	held    bool
	open    chan struct{}
	waiting chan struct{}
}

func (g *gate) hold() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held, g.open, g.waiting = true, make(chan struct{}), make(chan struct{}, 1)
}

func (g *gate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held {
		g.held = false
		close(g.open)
	}
}

// pass waits while the gate is held, and says it does.
func (g *gate) pass() {
	g.mu.Lock()
	held, open, waiting := g.held, g.open, g.waiting
	g.mu.Unlock()
	if !held {
		return
	}
	select {
	case waiting <- struct{}{}:
	default:
	}
	<-open
}

func TestAVideoNeverTakesAnothersPlaceUnderAnOffer(t *testing.T) {
	s := newSFU(t, 4)
	alice, _ := s.joinCamera("s-alice", "alice")
	s.joinCamera("s-bob", "bob")
	var g gate
	cam := &camera{t: t}
	carol, err := s.joinWith("s-carol", "carol", true, true, func(c *client, sl *slots) {
		cam.attach(c, sl)
		g.pass()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.release)
	s.video("s-alice", sourceCamera, true)
	eventually(t, "carol sees alice's camera", func() bool { return carol.heard("alice:camera") > 10 })

	// Dave joining sends carol an offer, which she holds while alice's
	// camera goes and bob's comes: bob's must not take the transceiver
	// alice's left before carol hears it go.
	g.hold()
	if _, err := s.join("s-dave", "dave", true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-g.waiting:
	case <-time.After(patience):
		t.Fatal("carol got no offer")
	}
	s.video("s-alice", sourceCamera, false)
	s.video("s-bob", sourceCamera, true)
	eventually(t, "alice sees bob's camera", func() bool { return alice.heard("bob:camera") > 10 })
	g.release()

	eventually(t, "carol is offered bob's camera", func() bool { return carol.offered("bob:camera") })
	carol.mu.Lock()
	swaps := carol.swaps
	carol.mu.Unlock()
	if len(swaps) > 0 {
		t.Fatalf("a track took another's place on carol's connection: %v", swaps)
	}
	eventually(t, "carol sees bob's camera", func() bool { return carol.heard("bob:camera") > 10 })
	eventually(t, "carol gets reports on it", func() bool { return carol.reported("bob:camera") > 0 })
}

// layeredCamera plays a browser that sends its camera in the three layers
// the server asks for: a frame a packet on each, its VP8 descriptor with a
// picture id, the layer's rank after the VP8 header. A layer sends a
// keyframe only when asked for one there.
type layeredCamera struct {
	t    *testing.T
	mu   sync.Mutex
	on   bool
	plis map[string]int
	want map[string]bool
	// full, when movable, sends the full layer on another SSRC when told.
	movable bool
	full    *movingTrack
}

func (cam *layeredCamera) attach(c *client, sl *slots) {
	cam.mu.Lock()
	defer cam.mu.Unlock()
	if cam.on {
		return
	}
	cam.on, cam.plis, cam.want = true, map[string]int{}, map[string]bool{}
	var slot *webrtc.RTPTransceiver
	for _, tr := range c.pc.GetTransceivers() {
		if tr.Mid() == sl.Camera {
			slot = tr
		}
	}
	if slot == nil {
		cam.t.Error("no slot for the camera")
		return
	}
	tracks := make([]layerTrack, len(rids))
	for i, rid := range rids {
		track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, "camera", "local", webrtc.WithRTPStreamID(rid))
		if err != nil {
			cam.t.Error(err)
			return
		}
		tracks[i] = track
		if rid == "f" && cam.movable {
			cam.full = &movingTrack{TrackLocalStaticRTP: track}
			tracks[i] = cam.full
		}
	}
	s, err := c.api.NewRTPSender(tracks[0], c.pc.SCTP().Transport())
	if err != nil {
		cam.t.Error(err)
		return
	}
	// The slot takes a sender of one layer: the others join it there.
	if err := slot.SetSender(s, tracks[0]); err != nil {
		cam.t.Error(err)
		return
	}
	for _, track := range tracks[1:] {
		if err := s.AddEncoding(track); err != nil {
			cam.t.Error(err)
			return
		}
	}
	for _, rid := range rids {
		go func() {
			for {
				pkts, _, err := s.ReadSimulcastRTCP(rid)
				if err != nil {
					return
				}
				for _, pkt := range pkts {
					if _, ok := pkt.(*rtcp.PictureLossIndication); ok {
						cam.mu.Lock()
						cam.plis[rid]++
						cam.want[rid] = true
						cam.mu.Unlock()
					}
				}
			}
		}()
	}
	go func() {
		seq := []uint16{1000, 30000, 65500}
		ts := []uint32{5000, 1 << 31, 4_294_000_000}
		pic := []uint16{100, 20000, 32700}
		for c.pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
			time.Sleep(33 * time.Millisecond)
			// The ids of the extensions that name the mid and the layer, as
			// the server offered them.
			var midID, ridID uint8
			for _, e := range s.GetParameters().HeaderExtensions {
				switch e.URI {
				case sdp.SDESMidURI:
					midID = uint8(e.ID)
				case sdp.SDESRTPStreamIDURI:
					ridID = uint8(e.ID)
				}
			}
			if midID == 0 || ridID == 0 {
				continue
			}
			for i, rid := range rids {
				cam.mu.Lock()
				key := cam.want[rid]
				cam.want[rid] = false
				cam.mu.Unlock()
				hdr := byte(1)
				if key {
					hdr = 0
				}
				payload := append([]byte{0x90, 0x80, 0x80 | byte(pic[i]>>8), byte(pic[i]), hdr, byte(i)}, make([]byte, 200)...)
				pkt := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq[i], Timestamp: ts[i], Marker: true}, Payload: payload}
				if pkt.SetExtension(midID, []byte(sl.Camera)) != nil || pkt.SetExtension(ridID, []byte(rid)) != nil {
					cam.t.Error("could not mark a packet")
					return
				}
				if err := tracks[i].WriteRTP(pkt); err != nil {
					return
				}
				seq[i]++
				ts[i] += 3000
				pic[i] = (pic[i] + 1) & 0x7fff
			}
		}
	}()
}

func (cam *layeredCamera) pli(rid string) int {
	cam.mu.Lock()
	defer cam.mu.Unlock()
	return cam.plis[rid]
}

func (s *sfu) focus(session, stream string) {
	s.engine.Send(s.rooms, toRoom{channelID: "room", msg: focus{session: session, stream: stream}})
}

// layer is the layer of the last frame a client got of a stream, -1 before
// any.
func (c *client) layer(streamID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	fs := c.frames[streamID]
	if len(fs) == 0 {
		return -1
	}
	return fs[len(fs)-1].layer
}

// layers are the layers a client got frames of, in the order they came.
func (c *client) layers(streamID string) []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var got []int
	for _, f := range c.frames[streamID] {
		if len(got) == 0 || got[len(got)-1] != f.layer {
			got = append(got, f.layer)
		}
	}
	return got
}

// switched finds where a client's frames of one layer gave way to
// another's, and says how the stream went on there, if not as one.
func (c *client) switched(streamID string, from, to int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	fs := c.frames[streamID]
	for i := len(fs) - 1; i > 0; i-- {
		a, b := fs[i-1], fs[i]
		if a.layer != from || b.layer != to {
			continue
		}
		switch {
		case !b.key:
			return "not on a keyframe"
		case b.seq != a.seq+1:
			return fmt.Sprintf("sequence %d then %d", a.seq, b.seq)
		case b.ts-a.ts == 0 || b.ts-a.ts >= 1<<31:
			return fmt.Sprintf("timestamp %d then %d", a.ts, b.ts)
		case b.pic != (a.pic+1)&0x7fff:
			return fmt.Sprintf("picture %d then %d", a.pic, b.pic)
		}
		return ""
	}
	return fmt.Sprintf("no switch from layer %d to %d", from, to)
}

func TestAViewerGetsTheLayerThatFitsItsTile(t *testing.T) {
	s := newSFU(t, 4)
	cam := &layeredCamera{t: t}
	alice, err := s.joinAs("s-alice", "alice", true, hello{video: true, simulcast: true, layers: true}, cam.attach)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.joinAs("s-bob", "bob", true, hello{video: true, layers: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// carol's app picks no layers: she gets the best of each.
	carol, err := s.joinWith("s-carol", "carol", true, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob hears alice", func() bool { return bob.heard("alice") > 10 })

	// Only alice said she sends layers: her first offer asks for them, on
	// her camera's slot.
	for _, c := range []*client{alice, bob, carol} {
		c.mu.Lock()
		if layered := c.layered; layered == 0 != (c != alice) {
			t.Errorf("%d offers asked %s for layers", layered, c.userID)
		}
		c.mu.Unlock()
	}

	// Bob shows alice's camera in a tile: he gets the least layer, alone.
	s.video("s-alice", sourceCamera, true)
	eventually(t, "bob sees alice's camera", func() bool { return bob.heard("alice:camera") > 30 })
	if got := bob.layers("alice:camera"); len(got) != 1 || got[0] != 0 {
		t.Fatalf("bob, who shows it small, got layers %v", got)
	}
	eventually(t, "carol gets the full layer", func() bool { return carol.layer("alice:camera") == 2 })

	// His browser asking for a keyframe asks for one of his layer.
	plis := cam.pli("q")
	time.Sleep(keyframeGap)
	bob.mu.Lock()
	remote := bob.remotes["alice:camera"]
	bob.mu.Unlock()
	if err := bob.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(remote.SSRC())}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob's request reaches alice's least layer", func() bool { return cam.pli("q") > plis })

	// He enlarges it: the full layer comes from its next keyframe on, as
	// the same stream.
	s.focus("s-bob", "alice:camera")
	eventually(t, "bob gets the full layer", func() bool { return bob.layer("alice:camera") == 2 })
	if how := bob.switched("alice:camera", 0, 2); how != "" {
		t.Fatalf("switching up: %s", how)
	}
	frames := bob.heard("alice:camera")
	eventually(t, "the full layer goes on", func() bool { return bob.heard("alice:camera") > frames+10 })
	if got := bob.layers("alice:camera"); len(got) != 2 {
		t.Fatalf("bob got layers %v", got)
	}

	// And shrinks it again.
	s.focus("s-bob", "")
	eventually(t, "bob gets the least layer again", func() bool { return bob.layer("alice:camera") == 0 })
	if how := bob.switched("alice:camera", 2, 0); how != "" {
		t.Fatalf("switching down: %s", how)
	}

	// All of it on one track, as Pion's own reports describe it.
	bob.mu.Lock()
	tracks := bob.tracks["alice:camera"]
	bob.mu.Unlock()
	if tracks != 1 {
		t.Fatalf("bob got alice's camera on %d tracks", tracks)
	}
	eventually(t, "bob gets reports on it", func() bool { return bob.reported("alice:camera") > 0 })
}

// layerTrack is a layer of a test camera.
type layerTrack interface {
	webrtc.TrackLocal
	WriteRTP(p *rtp.Packet) error
}

// movingTrack sends its packets on another SSRC once told, as a hostile
// client may: their mid and rid stay, and name a layer the server has.
type movingTrack struct {
	*webrtc.TrackLocalStaticRTP
	mu sync.Mutex
	w  webrtc.TrackLocalWriter
	pt webrtc.PayloadType
	to uint32
}

func (m *movingTrack) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	codec, err := m.TrackLocalStaticRTP.Bind(ctx)
	if err == nil {
		m.mu.Lock()
		m.w, m.pt = ctx.WriteStream(), codec.PayloadType
		m.mu.Unlock()
	}
	return codec, err
}

func (m *movingTrack) move(ssrc uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.to = ssrc
}

func (m *movingTrack) WriteRTP(p *rtp.Packet) error {
	m.mu.Lock()
	w, pt, to := m.w, m.pt, m.to
	m.mu.Unlock()
	if to == 0 || w == nil {
		return m.TrackLocalStaticRTP.WriteRTP(p)
	}
	h := p.Header
	h.SSRC, h.PayloadType = to, uint8(pt)
	_, err := w.WriteRTP(&h, p.Payload)
	return err
}
