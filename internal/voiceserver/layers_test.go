package voiceserver

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// recorder is a viewer's connection: it keeps what a view sends.
type recorder struct {
	mu   sync.Mutex
	sent []rtp.Packet
}

func (w *recorder) WriteRTP(h *rtp.Header, payload []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sent = append(w.sent, rtp.Packet{Header: *h, Payload: append([]byte(nil), payload...)})
	return len(payload), nil
}

func (w *recorder) Write(b []byte) (int, error) { return len(b), nil }

func (w *recorder) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.sent)
}

func (w *recorder) last() rtp.Packet {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sent[len(w.sent)-1]
}

// layerStream plays one layer as a browser sends it: a frame per packet,
// numbered on its own, with a 15-bit picture id and a TL0PICIDX, and the
// layer's rank after the VP8 header.
type layerStream struct {
	rank int
	seq  uint16
	ts   uint32
	pic  uint16
	tl0  uint8
}

func (l *layerStream) next(key bool) []byte {
	b := l.packet(l.seq, key)
	l.seq++
	l.ts += 3000
	l.pic = (l.pic + 1) & 0x7fff
	l.tl0++
	return b
}

func (l *layerStream) packet(seq uint16, key bool) []byte {
	hdr := byte(1)
	if key {
		hdr = 0
	}
	p := rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 100, SequenceNumber: seq, Timestamp: l.ts, SSRC: 99, Marker: true},
		Payload: []byte{0x90, 0xc0, 0x80 | byte(l.pic>>8), byte(l.pic), l.tl0, hdr, byte(l.rank)},
	}
	// As a browser marks its packets with its mid.
	if err := p.SetExtension(4, []byte("1")); err != nil {
		panic(err)
	}
	b, err := p.Marshal()
	if err != nil {
		panic(err)
	}
	return b
}

// continuous fails unless each packet sent follows the one before as one
// stream: the next sequence number, a later timestamp, the next picture.
func continuous(t *testing.T, sent []rtp.Packet) {
	t.Helper()
	for i := 1; i < len(sent); i++ {
		a, b := sent[i-1], sent[i]
		d := parseVP8(a.Payload)
		e := parseVP8(b.Payload)
		if b.SequenceNumber != a.SequenceNumber+1 || b.Timestamp-a.Timestamp == 0 || b.Timestamp-a.Timestamp >= 1<<31 {
			t.Fatalf("packet %d: sequence %d then %d, timestamp %d then %d", i, a.SequenceNumber, b.SequenceNumber, a.Timestamp, b.Timestamp)
		}
		if e.picture(b.Payload) != (d.picture(a.Payload)+1)&0x7fff || e.tl0Index(b.Payload) != d.tl0Index(a.Payload)+1 {
			t.Fatalf("packet %d: picture %d then %d", i, d.picture(a.Payload), e.picture(b.Payload))
		}
		if b.SSRC != 7 || b.PayloadType != 96 || b.Extension || len(b.Extensions) > 0 {
			t.Fatalf("packet %d went out as %+v", i, b.Header)
		}
	}
}

func TestAViewSwitchesLayersOnKeyframesAsOneStream(t *testing.T) {
	var asked []int
	f := newFeed("camera-s", "u:camera", func(layer int) { asked = append(asked, layer) })
	w := &recorder{}
	v := f.view(false)
	v.out, v.ssrc, v.pt = w, 7, 96
	f.add(v)
	// Each layer numbers its packets and frames its own way: the full one
	// is about to wrap them all.
	low := &layerStream{rank: 0, seq: 100, ts: 1000, pic: 500, tl0: 10}
	full := &layerStream{rank: 2, seq: 65533, ts: 4_294_960_000, pic: 32766, tl0: 254}

	// Nothing goes out before a keyframe of the least layer, which a small
	// tile shows, and the view asks for one.
	f.write(0, low.next(false))
	f.write(2, full.next(true))
	if w.count() != 0 || len(asked) != 1 || asked[0] != 0 {
		t.Fatalf("sent %d packets and asked for %v before a keyframe", w.count(), asked)
	}
	f.write(0, low.next(true))
	for range 5 {
		f.write(2, full.next(false))
		f.write(0, low.next(false))
	}
	if w.count() != 6 {
		t.Fatalf("sent %d packets of the least layer, not 6", w.count())
	}

	// Shown big, the view keeps the least layer until the full one's
	// keyframe, which it asks for.
	v.setHigh(true)
	f.write(2, full.next(false))
	f.write(0, low.next(false))
	if w.count() != 7 || asked[len(asked)-1] != 2 {
		t.Fatalf("sent %d packets and asked for %v while waiting for a keyframe", w.count(), asked)
	}
	before := full.seq - 1
	f.write(2, full.next(true))
	f.write(0, low.next(false))
	f.write(2, full.next(false))
	// A packet of the full layer from before its keyframe is late for it.
	f.write(2, full.packet(before, false))
	if w.count() != 9 || parseVP8(w.last().Payload).keyframe || w.last().Payload[6] != 2 {
		t.Fatalf("sent %d packets, the last %v", w.count(), w.last().Payload)
	}

	// And small again, from the least layer's next keyframe.
	v.setHigh(false)
	f.write(0, low.next(false))
	f.write(0, low.next(true))
	f.write(2, full.next(false))
	f.write(0, low.next(false))
	if w.count() != 11 || w.last().Payload[6] != 0 {
		t.Fatalf("sent %d packets, the last %v", w.count(), w.last().Payload)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	continuous(t, w.sent)
}

func TestABigViewFallsBackWhenTheFullLayerStops(t *testing.T) {
	f := newFeed("camera-s", "u:camera", func(int) {})
	w := &recorder{}
	v := f.view(true)
	v.out, v.ssrc, v.pt = w, 7, 96
	f.add(v)
	low := &layerStream{rank: 0, seq: 1, pic: 1}
	full := &layerStream{rank: 2, seq: 40000, ts: 1 << 30, pic: 9000}
	f.write(2, full.next(true))
	f.write(0, low.next(true))
	f.write(2, full.next(false))
	// The sender's network got short: it paused the full layer a while ago.
	f.seen[2].Store(time.Now().Add(-2 * stale).UnixNano())
	f.write(0, low.next(true))
	f.write(0, low.next(false))
	if w.count() != 4 || w.last().Payload[6] != 0 {
		t.Fatalf("sent %d packets, the last %v", w.count(), w.last().Payload)
	}
	continuous(t, w.sent)

	// Gone from the feed, a view is sent nothing.
	f.remove(v)
	f.write(0, low.next(true))
	if w.count() != 4 {
		t.Fatal("a view out of the feed was sent a packet")
	}
}

func TestParseVP8(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    []byte
		want vp8Desc
	}{
		{"keyframe, no extension", []byte{0x10, 0x00}, vp8Desc{keyframe: true, pic: -1, tl0: -1}},
		{"delta frame", []byte{0x10, 0x01}, vp8Desc{pic: -1, tl0: -1}},
		{"later partition", []byte{0x11, 0x00}, vp8Desc{pic: -1, tl0: -1}},
		{"not the start", []byte{0x00, 0x00}, vp8Desc{pic: -1, tl0: -1}},
		{"7-bit picture id", []byte{0x90, 0x80, 0x05, 0x00}, vp8Desc{keyframe: true, pic: 2, tl0: -1}},
		{"15-bit picture id, TL0PICIDX, TID", []byte{0x90, 0xe0, 0x81, 0x02, 0x07, 0x40, 0x00}, vp8Desc{keyframe: true, pic: 2, tl0: 4, wide: true}},
		{"no VP8 header", []byte{0x90, 0x80, 0x81, 0x02}, vp8Desc{pic: -1, tl0: -1}},
		{"empty", nil, vp8Desc{pic: -1, tl0: -1}},
	} {
		if got := parseVP8(tc.b); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
	b := []byte{0x90, 0xc0, 0xff, 0xff, 0x09, 0x00}
	d := parseVP8(b)
	if d.picture(b) != 0x7fff || d.tl0Index(b) != 9 {
		t.Fatalf("read picture %d, TL0PICIDX %d", d.picture(b), d.tl0Index(b))
	}
	d.setPicture(b, 0x1234)
	d.setTL0Index(b, 3)
	if b[2] != 0x92 || b[3] != 0x34 || b[4] != 3 {
		t.Fatalf("wrote %x", b)
	}
}

func TestAskLayersAsksOnTheCameraSlotOnly(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	for range 2 {
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatal(err)
		}
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	asked := askLayers(offer.SDP, "0")
	sections := strings.Split(asked, "m=video")
	if len(sections) != 3 {
		t.Fatalf("the offer has %d video sections", len(sections)-1)
	}
	want := "a=rid:q recv\r\na=rid:h recv\r\na=rid:f recv\r\na=simulcast:recv q;h;f\r\n"
	if !strings.Contains(sections[1], want) || strings.Contains(sections[2], "simulcast") {
		t.Fatalf("layers asked for wrongly:\n%s", asked)
	}
	if again := askLayers(asked, "0"); again != asked {
		t.Fatal("layers asked for twice")
	}
	if none := askLayers(offer.SDP, "9"); none != offer.SDP {
		t.Fatal("an offer without the slot changed")
	}
}

func TestTogglingTheFocusAsksNoMoreThanTheThrottle(t *testing.T) {
	var asked []int
	f := newFeed("camera-s", "u:camera", func(layer int) { asked = append(asked, layer) })
	w := &recorder{}
	v := f.view(false)
	v.out, v.ssrc, v.pt = w, 7, 96
	f.add(v)
	low := &layerStream{rank: 0, seq: 1, pic: 1}
	full := &layerStream{rank: 2, seq: 40000, ts: 1 << 30, pic: 9000}
	f.write(0, low.next(true))
	f.write(2, full.next(false))
	// A viewer flips its focus as fast as it may: the first flip asks for
	// the full layer's keyframe at once, the others within the throttle.
	for i := range 20 {
		v.setHigh(i%2 == 0)
		f.write(2, full.next(false))
		f.write(0, low.next(false))
	}
	if len(asked) != 1 || asked[0] != 2 {
		t.Fatalf("asked for keyframes of %v", asked)
	}
}
