package voiceserver

import (
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// rids are the layers a camera comes in, as simulcast sends them, from the
// smallest picture to the full one. A screen, or a camera from an app that
// sends one layer, comes in one, with no rid.
var rids = []string{"q", "h", "f"}

// layerOf ranks a rid among the layers; -1 for one the server never asked
// for.
func layerOf(rid string) int {
	if rid == "" {
		return 0
	}
	return slices.Index(rids, rid)
}

// stale is how long a layer may bring no frame before viewers leave it: a
// sender pauses its larger layers when its network is short.
const stale = time.Second

// askLayers asks, in an offer, for the camera's layers on its slot. Pion
// cannot offer to receive simulcast, but takes a browser's answer to such
// an offer, and asks for the same layers in every offer after it.
func askLayers(offer, mid string) string {
	var d sdp.SessionDescription
	if d.UnmarshalString(offer) != nil {
		return offer
	}
	for _, m := range d.MediaDescriptions {
		if v, _ := m.Attribute(sdp.AttrKeyMID); v != mid {
			continue
		}
		if _, ok := m.Attribute("simulcast"); ok {
			return offer
		}
		for _, rid := range rids {
			m.WithValueAttribute("rid", rid+" recv")
		}
		m.WithValueAttribute("simulcast", "recv "+strings.Join(rids, ";"))
		b, err := d.Marshal()
		if err != nil {
			return offer
		}
		return string(b)
	}
	return offer
}

// feed is a peer's camera or screen as the others receive it. Each viewer
// gets one layer of it, on a view of its own.
type feed struct {
	id, streamID string
	// ask asks the sender for a keyframe of a layer.
	ask func(layer int)
	// seen is when each layer last brought a frame, in unix nanoseconds.
	seen  [3]atomic.Int64
	mu    sync.RWMutex
	views map[*view]struct{}
}

func newFeed(id, streamID string, ask func(layer int)) *feed {
	return &feed{id: id, streamID: streamID, ask: ask, views: make(map[*view]struct{})}
}

// view makes the feed's view for one viewer, which shows it big when high
// is set.
func (f *feed) view(high bool) *view {
	return &view{feed: f, high: high, layer: -1}
}

// write sends a packet of a layer to the viewers on it. It runs on the
// layer's own goroutine.
func (f *feed) write(layer int, buf []byte) {
	var pkt rtp.Packet
	if pkt.Unmarshal(buf) != nil {
		return
	}
	now := time.Now()
	// Padding alone carries no picture.
	if len(pkt.Payload) > 0 {
		f.seen[layer].Store(now.UnixNano())
	}
	d := parseVP8(pkt.Payload)
	f.mu.RLock()
	defer f.mu.RUnlock()
	for v := range f.views {
		v.write(layer, &pkt, d, now)
	}
}

// pick is the layer a viewer gets: the best that comes for a big tile, the
// least for a small one; -1 while none comes.
func (f *feed) pick(high bool, now time.Time) int {
	got := -1
	for i := range f.seen {
		if now.UnixNano()-f.seen[i].Load() >= int64(stale) {
			continue
		}
		got = i
		if !high {
			break
		}
	}
	return got
}

// forget says a layer stopped coming.
func (f *feed) forget(layer int) {
	f.seen[layer].Store(0)
}

func (f *feed) add(v *view) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.views[v] = struct{}{}
}

func (f *feed) remove(v *view) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.views, v)
}

// view is a feed on one viewer's connection: one layer at a time, switched
// on a keyframe, its sequence numbers, timestamps and picture ids rewritten
// so the viewer sees one stream throughout.
type view struct {
	feed *feed
	mu   sync.Mutex
	out  webrtc.TrackLocalWriter
	ssrc uint32
	pt   uint8
	high bool
	// layer is the one forwarded, -1 until the first keyframe.
	layer int
	// What the layer's packets lose on the way out: out = in - off.
	seqOff uint16
	tsOff  uint32
	picOff uint16
	tl0Off uint8
	// The newest packet sent, and when.
	sent           bool
	lastSeq        uint16
	lastTS         uint32
	lastPic        uint16
	lastTL0        uint8
	hasPic, hasTL0 bool
	lastAt         time.Time
	asked          time.Time
	first          uint16
	fresh          bool
	payload        []byte
}

// write forwards a packet of a layer, if it is the viewer's.
func (v *view) write(layer int, pkt *rtp.Packet, d vp8Desc, now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.out == nil {
		return
	}
	seq := pkt.SequenceNumber
	switch {
	case layer != v.layer:
		if layer != v.feed.pick(v.high, now) {
			return
		}
		if !d.keyframe {
			if now.Sub(v.asked) >= askGap {
				v.asked = now
				v.feed.ask(layer)
			}
			return
		}
		v.switchTo(layer, pkt, d, now)
	case v.fresh:
		// Packets of the new layer from before the keyframe belong to
		// frames the viewer never got the start of.
		if seqBefore(seq, v.first) {
			return
		}
		if seq-v.first > 512 {
			v.fresh = false
		}
	}
	h := pkt.Header
	h.SSRC, h.PayloadType = v.ssrc, v.pt
	h.SequenceNumber -= v.seqOff
	h.Timestamp -= v.tsOff
	// The sender's extensions mean nothing on the viewer's connection, its
	// mid least of all.
	h.Extension, h.ExtensionProfile, h.Extensions = false, 0, nil
	payload := pkt.Payload
	pic, tl0 := (d.picture(payload)-v.picOff)&d.mask(), d.tl0Index(payload)-v.tl0Off
	if (v.picOff != 0 && d.pic >= 0) || (v.tl0Off != 0 && d.tl0 >= 0) {
		v.payload = append(v.payload[:0], payload...)
		payload = v.payload
		d.setPicture(payload, pic)
		d.setTL0Index(payload, tl0)
	}
	if !v.sent || seqBefore(v.lastSeq, h.SequenceNumber) {
		v.sent, v.lastSeq, v.lastTS, v.lastAt = true, h.SequenceNumber, h.Timestamp, now
		if d.pic >= 0 {
			v.lastPic, v.hasPic = pic, true
		}
		if d.tl0 >= 0 {
			v.lastTL0, v.hasTL0 = tl0, true
		}
	}
	_, _ = v.out.WriteRTP(&h, payload)
}

// switchTo starts forwarding a layer from its keyframe pkt, where the last
// one left off.
func (v *view) switchTo(layer int, pkt *rtp.Packet, d vp8Desc, now time.Time) {
	if v.sent {
		v.seqOff = pkt.SequenceNumber - v.lastSeq - 1
		v.tsOff = pkt.Timestamp - v.lastTS - ticks(now.Sub(v.lastAt))
		if d.pic >= 0 && v.hasPic {
			v.picOff = d.picture(pkt.Payload) - v.lastPic - 1
		}
		if d.tl0 >= 0 && v.hasTL0 {
			v.tl0Off = d.tl0Index(pkt.Payload) - v.lastTL0 - 1
		}
	}
	v.layer = layer
	v.first, v.fresh = pkt.SequenceNumber, true
}

// ticks is a time in the 90 kHz clock of video, one tick at least.
func ticks(d time.Duration) uint32 {
	ms := d.Milliseconds()
	if ms < 1 {
		return 1
	}
	if ms > 60_000 {
		ms = 60_000
	}
	return uint32(ms) * 90
}

// seqBefore reports whether sequence number a comes before b.
func seqBefore(a, b uint16) bool {
	return a != b && b-a < 1<<15
}

// keyframe asks for a keyframe of the layer the viewer gets, as its
// browser asks.
func (v *view) keyframe() {
	v.mu.Lock()
	layer := v.layer
	v.mu.Unlock()
	if layer >= 0 {
		v.feed.ask(layer)
	}
}

// setHigh says whether the viewer shows the feed big.
func (v *view) setHigh(high bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.high != high {
		v.high, v.asked = high, time.Time{}
	}
}

// A view is a track Pion sends: these make it one.

func (v *view) ID() string                { return v.feed.id }
func (v *view) RID() string               { return "" }
func (v *view) StreamID() string          { return v.feed.streamID }
func (v *view) Kind() webrtc.RTPCodecType { return webrtc.RTPCodecTypeVideo }

func (v *view) Bind(ctx webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	for _, c := range ctx.CodecParameters() {
		if strings.EqualFold(c.MimeType, vp8.MimeType) {
			v.mu.Lock()
			v.out, v.ssrc, v.pt = ctx.WriteStream(), uint32(ctx.SSRC()), uint8(c.PayloadType)
			v.mu.Unlock()
			v.feed.add(v)
			return c, nil
		}
	}
	return webrtc.RTPCodecParameters{}, webrtc.ErrUnsupportedCodec
}

func (v *view) Unbind(webrtc.TrackLocalContext) error {
	v.feed.remove(v)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.out = nil
	return nil
}

// vp8Desc is what a VP8 payload descriptor says: whether the packet starts
// a keyframe, and where its picture id and TL0PICIDX are, -1 when absent.
type vp8Desc struct {
	keyframe bool
	pic, tl0 int
	// wide says the picture id has 15 bits, not 7.
	wide bool
}

func parseVP8(b []byte) vp8Desc {
	none := vp8Desc{pic: -1, tl0: -1}
	if len(b) == 0 {
		return none
	}
	d, i := none, 1
	if b[0]&0x80 != 0 {
		if len(b) < 2 {
			return none
		}
		x := b[1]
		i = 2
		if x&0x80 != 0 && i < len(b) {
			d.pic, d.wide = i, b[i]&0x80 != 0
			i++
			if d.wide {
				i++
			}
		}
		if x&0x40 != 0 {
			d.tl0 = i
			i++
		}
		if x&0x30 != 0 {
			i++
		}
	}
	// The VP8 payload header follows: its first bit is 0 on a keyframe.
	if i >= len(b) {
		return none
	}
	d.keyframe = b[0]&0x10 != 0 && b[0]&0x07 == 0 && b[i]&0x01 == 0
	return d
}

func (d vp8Desc) picture(b []byte) uint16 {
	switch {
	case d.pic < 0:
		return 0
	case d.wide:
		return uint16(b[d.pic]&0x7f)<<8 | uint16(b[d.pic+1])
	}
	return uint16(b[d.pic] & 0x7f)
}

// mask is the range of the picture id.
func (d vp8Desc) mask() uint16 {
	if d.wide {
		return 0x7fff
	}
	return 0x7f
}

func (d vp8Desc) setPicture(b []byte, id uint16) {
	switch {
	case d.pic < 0:
	case d.wide:
		b[d.pic], b[d.pic+1] = 0x80|byte(id>>8&0x7f), byte(id&0xff)
	default:
		b[d.pic] = byte(id & 0x7f)
	}
}

func (d vp8Desc) tl0Index(b []byte) uint8 {
	if d.tl0 < 0 {
		return 0
	}
	return b[d.tl0]
}

func (d vp8Desc) setTL0Index(b []byte, idx uint8) {
	if d.tl0 >= 0 {
		b[d.tl0] = idx
	}
}
