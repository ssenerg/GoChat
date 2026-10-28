package voiceserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/ssenerg/gochat/internal/actors"
)

// Messages for a room's actor. Session names one browser's connection.
type (
	// addPeer replies with joined or an error. video says the peer sends
	// and shows video, simulcast that it sends its camera in layers, and
	// layers that it takes small ones of the others' but the one it
	// focuses.
	addPeer struct {
		session, userID, channelID string
		video, simulcast, layers   bool
		out                        *sender
	}
	joined     struct{}
	removePeer struct {
		session string
		code    int
		reason  string
	}
	answer struct {
		session, sdp string
	}
	candidate struct {
		session string
		init    webrtc.ICECandidateInit
	}
	speaking struct {
		session string
		on      bool
	}
	// video says a peer starts or stops sending its camera or screen.
	video struct {
		session, source string
		on              bool
	}
	// keyframe asks for a keyframe of a layer of a peer's camera or
	// screen, or of each when layer is -1.
	keyframe struct {
		session, source string
		layer           int
	}
	// focus says which camera or screen a peer shows big, by stream id.
	focus struct {
		session, stream string
	}
	// trackAdded says a peer's voice started coming in, relayed to track.
	trackAdded struct {
		session string
		track   *webrtc.TrackLocalStaticRTP
	}
	trackRemoved struct {
		session, id string
	}
	// layerAdded says a layer of a peer's camera or screen started coming
	// in from remote, layerRemoved that it stopped.
	layerAdded struct {
		session, source string
		layer           int
		remote          *webrtc.TrackRemote
		ssrc            webrtc.SSRC
	}
	layerRemoved struct {
		session, source string
		layer           int
		remote          *webrtc.TrackRemote
	}
	pcState struct {
		session string
		state   webrtc.PeerConnectionState
	}
	resync struct{}
)

// peerGone tells the rooms actor a peer left a room.
type peerGone struct{ channelID string }

type peer struct {
	session, userID string
	pc              *webrtc.PeerConnection
	out             *sender
	speak           *rate.Limiter
	// video says the peer sends and shows video: it alone has slots, the
	// transceivers its camera and screen come in on, by source, and
	// receives the others'. feeds are its camera and screen as the others
	// get them, sending is what it says it sends, and incoming the layers
	// of each source whose packets have come.
	video    bool
	slots    map[string]*webrtc.RTPTransceiver
	feeds    map[string]*feed
	sending  map[string]bool
	incoming map[string]map[int]*incoming
	// simulcast says it sends its camera in layers. layers says it shows
	// the others' cameras small but the one in focus; views are theirs on
	// its connection, by track id.
	simulcast, layers bool
	focus             string
	views             map[string]*view
	// needOffer is set when the peer's tracks changed while it could not
	// take an offer, or before its first.
	needOffer bool
	// Pion finds candidates on its own goroutine as soon as an offer is
	// set, before the offer is sent: those found before the first offer
	// wait, since a browser drops candidates that come before it.
	mu      sync.Mutex
	offered bool
	early   []webrtc.ICECandidateInit
}

// candidate sends a candidate the peer's connection found, or keeps it
// until the first offer is out.
func (p *peer) candidate(init webrtc.ICECandidateInit) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.offered {
		p.early = append(p.early, init)
		return
	}
	p.out.send(outbound{Op: opCandidate, Candidate: &init})
}

// offer sends an offer, and after the first, the candidates that waited.
// The offer must be the local description already: the slots' mids come
// from it.
func (p *peer) offer(sdp string) {
	msg := outbound{Op: opOffer, SDP: sdp}
	if p.video {
		msg.Slots = &slots{Camera: p.slots[sourceCamera].Mid(), Screen: p.slots[sourceScreen].Mid()}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.out.send(msg)
	if !p.offered {
		p.offered = true
		for i := range p.early {
			p.out.send(outbound{Op: opCandidate, Candidate: &p.early[i]})
		}
		p.early = nil
	}
}

// incoming is a layer of a peer's camera or screen whose packets come in.
type incoming struct {
	remote *webrtc.TrackRemote
	ssrc   webrtc.SSRC
	// keyframes spaces the requests for keyframes the peer gets: one
	// keyframe serves every receiver.
	keyframes *rate.Limiter
}

// keyframeGap is the least time between two requests for a keyframe of
// one camera or screen, and askGap between those one receiver makes: a
// keyframe is large, and one receiver asking often costs everyone.
const (
	keyframeGap = 300 * time.Millisecond
	askGap      = time.Second
)

// roomTrack is a peer's voice, camera or screen as the others receive it:
// a voice as one track for all, a camera or screen as a feed, of which
// each gets a view.
type roomTrack struct {
	local         *webrtc.TrackLocalStaticRTP
	feed          *feed
	owner, source string
}

// room forwards the voice, camera and screen of each peer of one voice
// channel to the others, as an SFU: it neither mixes nor decodes.
type room struct {
	logger    *zap.Logger
	cfg       *Config
	media     *Media
	link      *actor.PID
	rooms     *actor.PID
	channelID string
	peers     map[string]*peer
	// tracks are what the others receive, by id.
	tracks map[string]*roomTrack
	retry  bool
}

func newRoom(logger *zap.Logger, cfg *Config, media *Media, link, rooms *actor.PID, channelID string) actor.Producer {
	return func() actor.Receiver {
		return &room{
			logger: logger, cfg: cfg, media: media, link: link, rooms: rooms, channelID: channelID,
			peers:  make(map[string]*peer),
			tracks: make(map[string]*roomTrack),
		}
	}
}

func (r *room) Receive(c *actor.Context) {
	switch msg := c.Message().(type) {
	case actor.Stopped:
		for _, p := range r.peers {
			r.drop(c, p, closeUnavailable, "the voice server is stopping")
		}
	case addPeer:
		if err := r.add(c, msg); err != nil {
			actors.Reply(c, err)
			c.Send(r.rooms, peerGone{channelID: r.channelID})
			return
		}
		actors.Reply(c, joined{})
	case removePeer:
		if p := r.peers[msg.session]; p != nil {
			r.drop(c, p, msg.code, msg.reason)
			r.signal(c)
		}
	case answer:
		r.answer(c, msg)
	case candidate:
		if p := r.peers[msg.session]; p != nil {
			if err := p.pc.AddICECandidate(msg.init); err != nil {
				r.logger.Debug("bad candidate", zap.String("session", msg.session), zap.Error(err))
			}
		}
	case speaking:
		r.speaking(msg)
	case video:
		if p := r.peers[msg.session]; p != nil && p.slots[msg.source] != nil && p.sending[msg.source] != msg.on {
			p.sending[msg.source] = msg.on
			r.publish(c, p, msg.source)
		}
	case keyframe:
		if p := r.peers[msg.session]; p != nil {
			r.keyframe(p, msg.source, msg.layer)
		}
	case focus:
		if p := r.peers[msg.session]; p != nil && p.layers {
			p.focus = msg.stream
			for _, v := range p.views {
				v.setHigh(v.feed.streamID == p.focus)
			}
		}
	case trackAdded:
		if p := r.peers[msg.session]; p != nil {
			r.tracks[msg.track.ID()] = &roomTrack{local: msg.track, owner: p.session, source: sourceMic}
			r.signal(c)
		}
	case trackRemoved:
		if t := r.tracks[msg.id]; t != nil && t.owner == msg.session {
			delete(r.tracks, msg.id)
			r.signal(c)
		}
	case layerAdded:
		p := r.peers[msg.session]
		if p == nil || p.feeds[msg.source] == nil {
			return
		}
		if p.incoming[msg.source] == nil {
			p.incoming[msg.source] = make(map[int]*incoming)
		}
		p.incoming[msg.source][msg.layer] = &incoming{remote: msg.remote, ssrc: msg.ssrc, keyframes: rate.NewLimiter(rate.Every(keyframeGap), 1)}
		r.publish(c, p, msg.source)
	case layerRemoved:
		if p := r.peers[msg.session]; p != nil {
			r.unlayer(c, p, msg)
		}
	case pcState:
		if p := r.peers[msg.session]; p != nil && (msg.state == webrtc.PeerConnectionStateFailed || msg.state == webrtc.PeerConnectionStateClosed) {
			r.drop(c, p, closeFailed, "the connection failed")
			r.signal(c)
		}
	case resync:
		r.retry = false
		r.signal(c)
	case dropPeers:
		for _, p := range r.peers {
			r.drop(c, p, closeUnavailable, msg.reason)
		}
	}
}

func (r *room) add(c *actor.Context, msg addPeer) error {
	if len(r.peers) >= r.cfg.MaxPeersPerRoom {
		return errFull
	}
	pc, err := r.media.newPeerConnection()
	if err != nil {
		return err
	}
	p := &peer{
		session: msg.session, userID: msg.userID, pc: pc, out: msg.out, speak: rate.NewLimiter(10, 20), needOffer: true,
		video: msg.video, sending: make(map[string]bool), incoming: make(map[string]map[int]*incoming),
		simulcast: msg.video && msg.simulcast, layers: msg.layers, views: make(map[string]*view),
	}
	self, engine := c.PID(), c.Engine()
	// The peer's own voice comes in on this transceiver, and its camera
	// and screen on these.
	recvonly := webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, recvonly); err != nil {
		_ = pc.Close()
		return err
	}
	if p.video {
		p.slots = make(map[string]*webrtc.RTPTransceiver, 2)
		p.feeds = make(map[string]*feed, 2)
		for _, source := range []string{sourceCamera, sourceScreen} {
			t, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, recvonly)
			if err != nil {
				_ = pc.Close()
				return err
			}
			p.slots[source] = t
			p.feeds[source] = newFeed(source+"-"+p.session, p.userID+":"+source, maxRates[source], func(layer int) {
				engine.Send(self, keyframe{session: p.session, source: source, layer: layer})
			})
		}
	}
	pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand != nil {
			p.candidate(cand.ToJSON())
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		engine.Send(self, pcState{session: p.session, state: s})
	})
	pc.OnTrack(func(remote *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
		if source := p.source(recv); source == sourceMic {
			forwardVoice(engine, self, p.session, p.userID, remote)
		} else {
			forwardVideo(engine, self, p.session, source, p.feeds[source], remote)
		}
	})
	r.peers[p.session] = p
	p.out.send(outbound{Op: opReady, UserID: p.userID, ChannelID: r.channelID, Session: p.session, ICEServers: r.media.iceServers(p.userID, time.Now())})
	r.signal(c)
	return nil
}

var errFull = errors.New("the voice channel is full")

// source names what comes in on a receiver: the peer's camera or screen
// on their slots, its voice on the other.
func (p *peer) source(recv *webrtc.RTPReceiver) string {
	for source, t := range p.slots {
		if t.Receiver() == recv {
			return source
		}
	}
	return sourceMic
}

// maxRates bound the bytes a peer's voice, camera and screen may each
// carry per second, a camera's layers together: Opus tops out at 510
// kbit/s, and the clients send a camera at about 1.65 Mbit/s in layers and
// a screen at about 2.5; a peer sending more is flooding the room. Each
// may send a second's worth at once, as a keyframe comes.
var maxRates = map[string]int{sourceMic: 64 << 10, sourceCamera: 320 << 10, sourceScreen: 640 << 10}

// forwardVoice relays a peer's voice to the track the others receive,
// until the peer goes. It runs on Pion's goroutine for the track.
func forwardVoice(engine *actor.Engine, room *actor.PID, session, userID string, remote *webrtc.TrackRemote) {
	if remote.Kind() != webrtc.RTPCodecTypeAudio {
		return
	}
	// The stream id tells receivers whose it is.
	local, err := webrtc.NewTrackLocalStaticRTP(remote.Codec().RTPCodecCapability, "voice-"+session, userID)
	if err != nil {
		return
	}
	engine.Send(room, trackAdded{session: session, track: local})
	defer engine.Send(room, trackRemoved{session: session, id: local.ID()})
	limit := rate.NewLimiter(rate.Limit(maxRates[sourceMic]), maxRates[sourceMic])
	buf := make([]byte, 1500)
	for {
		n, _, err := remote.Read(buf)
		if err != nil {
			return
		}
		if !limit.AllowN(time.Now(), n) {
			continue
		}
		if _, err := local.Write(buf[:n]); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			return
		}
	}
}

// forwardVideo relays a layer of a peer's camera or screen to its feed,
// until the peer goes. It runs on Pion's goroutine for the layer.
func forwardVideo(engine *actor.Engine, room *actor.PID, session, source string, f *feed, remote *webrtc.TrackRemote) {
	layer := layerOf(remote.RID())
	if remote.Kind() != webrtc.RTPCodecTypeVideo || layer < 0 {
		return
	}
	engine.Send(room, layerAdded{session: session, source: source, layer: layer, remote: remote, ssrc: remote.SSRC()})
	defer engine.Send(room, layerRemoved{session: session, source: source, layer: layer, remote: remote})
	buf := make([]byte, 1500)
	for {
		n, _, err := remote.Read(buf)
		if err != nil {
			return
		}
		if f.limit.AllowN(time.Now(), n) {
			f.write(layer, buf[:n])
		}
	}
}

// publish puts a peer's camera or screen in the room, or takes it out, as
// the peer says it sends it: its packets may come before or after it says
// so, and it is published once both have.
func (r *room) publish(c *actor.Context, p *peer, source string) {
	f := p.feeds[source]
	if f == nil || len(p.incoming[source]) == 0 {
		return
	}
	id := f.id
	_, published := r.tracks[id]
	switch on := p.sending[source]; {
	case on && !published && r.videos() >= r.cfg.MaxVideosPerRoom:
		// Everyone gets every camera and screen: the cap bounds what the
		// server sends.
		p.sending[source] = false
		off := false
		p.out.send(outbound{Op: opVideo, Source: source, On: &off, Reason: fmt.Sprintf("this voice channel shows %d cameras and screens at most", r.cfg.MaxVideosPerRoom)})
	case on && !published:
		r.tracks[id] = &roomTrack{feed: f, owner: p.session, source: source}
		r.report(c, p)
		r.signal(c)
		r.keyframe(p, source, -1)
	case !on && published:
		delete(r.tracks, id)
		r.report(c, p)
		r.signal(c)
	}
}

// unlayer takes a layer of a peer's camera or screen out, and the source
// with its last.
func (r *room) unlayer(c *actor.Context, p *peer, msg layerRemoved) {
	in := p.incoming[msg.source]
	if l := in[msg.layer]; l == nil || l.remote != msg.remote {
		return
	}
	delete(in, msg.layer)
	f := p.feeds[msg.source]
	f.forget(msg.layer)
	if len(in) > 0 {
		return
	}
	delete(p.incoming, msg.source)
	if t := r.tracks[f.id]; t != nil && t.owner == p.session {
		delete(r.tracks, f.id)
		r.report(c, p)
		r.signal(c)
	}
}

// videos counts the cameras and screens the room forwards.
func (r *room) videos() int {
	n := 0
	for _, t := range r.tracks {
		if t.source != sourceMic {
			n++
		}
	}
	return n
}

// report tells the chat server which of a peer's camera and screen the
// room forwards, which is what everyone is shown of them.
func (r *room) report(c *actor.Context, p *peer) {
	m := peerMedia{userID: p.userID, channelID: r.channelID, session: p.session}
	for _, t := range r.tracks {
		if t.owner == p.session {
			m.video = m.video || t.source == sourceCamera
			m.stream = m.stream || t.source == sourceScreen
		}
	}
	c.Send(r.link, m)
}

// keyframe asks a peer for a keyframe of a layer of its camera or screen,
// or of each when layer is -1, so those who just started receiving it,
// switched to it, or lost some, see a picture again.
func (r *room) keyframe(p *peer, source string, layer int) {
	for l, in := range p.incoming[source] {
		if (layer >= 0 && l != layer) || !in.keyframes.Allow() {
			continue
		}
		if err := p.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(in.ssrc)}}); err != nil {
			r.logger.Debug("failed to ask for a keyframe", zap.String("session", p.session), zap.Error(err))
		}
	}
}

// send adds a track to a peer's connection where Pion's AddTrack puts it:
// on a transceiver of its kind that the last negotiation left sending
// nothing, which the browser has seen stop, or else a new one. Its sender
// is the connection's own, through the interceptors that answer NACKs and
// send reports. A voice goes first on the transceiver the peer's own voice
// comes in on, which browsers answer sendrecv.
//
// A camera or screen goes as a view of its own, of the layer that fits how
// big the peer shows it, and must never go on the peer's slots, which
// AddTrack would take as free: the server never takes an offer, so Pion
// never learns that browsers answer them sendonly. For the call, each slot
// holds a stand-in sender that AddTrack passes over, and lets it go after.
// ask, for a camera or screen, asks for a keyframe of the view's layer.
func (r *room) send(p *peer, t *roomTrack) (s *webrtc.RTPSender, ask func(), err error) {
	if t.feed == nil {
		s, err = p.pc.AddTrack(t.local)
		return s, nil, err
	}
	v := t.feed.view(!p.layers || t.feed.streamID == p.focus)
	for _, slot := range p.slots {
		stand, err := r.media.api.NewRTPSender(v, p.pc.SCTP().Transport())
		if err != nil {
			return nil, nil, err
		}
		err = slot.SetSender(stand, v)
		// Back to receiving only, with no sender, before anything is offered.
		defer func() { _ = slot.SetSender(stand, nil) }()
		if err != nil {
			return nil, nil, err
		}
	}
	if s, err = p.pc.AddTrack(v); err != nil {
		return nil, nil, err
	}
	p.views[v.ID()] = v
	return s, v.keyframe, nil
}

// readRTCP reads what a receiver reports on a track it gets, until the
// track leaves it. Pion's interceptors answer its NACKs on the way; its
// requests for a keyframe of a camera or screen go to ask, one per askGap
// at most.
func readRTCP(s *webrtc.RTPSender, ask func()) {
	asks := rate.NewLimiter(rate.Every(askGap), 1)
	for {
		pkts, _, err := s.ReadRTCP()
		if err != nil {
			return
		}
		if ask != nil && slices.ContainsFunc(pkts, wantsKeyframe) && asks.Allow() {
			ask()
		}
	}
}

func wantsKeyframe(pkt rtcp.Packet) bool {
	switch pkt.(type) {
	case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
		return true
	}
	return false
}

func (r *room) answer(c *actor.Context, msg answer) {
	p := r.peers[msg.session]
	if p == nil {
		return
	}
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: msg.sdp}); err != nil {
		r.logger.Debug("bad answer", zap.String("session", msg.session), zap.Error(err))
		r.drop(c, p, closeFailed, "the browser's answer was refused")
		r.signal(c)
		return
	}
	if p.needOffer {
		r.signal(c)
	}
}

func (r *room) speaking(msg speaking) {
	p := r.peers[msg.session]
	if p == nil || !p.speak.Allow() {
		return
	}
	on := msg.on
	out := outbound{Op: opSpeaking, UserID: p.userID, Speaking: &on}
	for _, other := range r.peers {
		if other != p {
			other.out.send(out)
		}
	}
}

// drop ends a peer's connection and tells the chat server it left.
func (r *room) drop(c *actor.Context, p *peer, code int, reason string) {
	delete(r.peers, p.session)
	for id, t := range r.tracks {
		if t.owner == p.session {
			delete(r.tracks, id)
		}
	}
	_ = p.pc.Close()
	p.out.close(code, reason)
	c.Send(r.link, peerLeft{userID: p.userID, channelID: r.channelID, session: p.session})
	c.Send(r.rooms, peerGone{channelID: r.channelID})
}

// signal brings every peer's outgoing tracks in line with the others'
// voices, cameras and screens, and offers the change to those it changed.
// A peer in the middle of a negotiation gets its offer once it answers.
func (r *room) signal(c *actor.Context) {
	for range 25 {
		if !r.syncOnce(c) {
			return
		}
	}
	// Pion refused a change: try again in a while rather than spin.
	if !r.retry {
		r.retry = true
		self, engine := c.PID(), c.Engine()
		time.AfterFunc(3*time.Second, func() { engine.Send(self, resync{}) })
	}
}

// syncOnce reports whether it must be run again.
func (r *room) syncOnce(c *actor.Context) bool {
	for _, p := range r.peers {
		if p.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
			r.drop(c, p, closeFailed, "the connection closed")
			return true
		}
		changed := false
		have := make(map[string]bool)
		for _, s := range p.pc.GetSenders() {
			t := s.Track()
			if t == nil {
				continue
			}
			have[t.ID()] = true
			if _, ok := r.tracks[t.ID()]; !ok {
				if err := p.pc.RemoveTrack(s); err != nil {
					return true
				}
				delete(p.views, t.ID())
				changed = true
			}
		}
		for id, t := range r.tracks {
			if have[id] || t.owner == p.session || (t.source != sourceMic && !p.video) {
				continue
			}
			s, ask, err := r.send(p, t)
			if err != nil {
				return true
			}
			go readRTCP(s, ask)
			changed = true
		}
		if !changed && !p.needOffer {
			continue
		}
		if p.pc.SignalingState() != webrtc.SignalingStateStable {
			p.needOffer = true
			continue
		}
		offer, err := p.pc.CreateOffer(nil)
		if err != nil {
			return true
		}
		if err := p.pc.SetLocalDescription(offer); err != nil {
			return true
		}
		p.needOffer = false
		sdp := offer.SDP
		// Pion offers no layers of its own: the first offer asks for them.
		if p.simulcast && p.pc.CurrentRemoteDescription() == nil {
			sdp = askLayers(sdp, p.slots[sourceCamera].Mid())
		}
		p.offer(sdp)
	}
	return false
}

// sender queues messages for one browser's websocket; a browser that
// falls behind is cut off.
type sender struct {
	ch     chan []byte
	closed chan closeFrame
}

type closeFrame struct {
	code   int
	reason string
}

func newSender(queue int) *sender {
	return &sender{ch: make(chan []byte, queue), closed: make(chan closeFrame, 1)}
}

// send is called from the room's actor and Pion's goroutines.
func (s *sender) send(msg outbound) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	select {
	case s.ch <- b:
	default:
		s.close(closeFailed, "the browser is not keeping up")
	}
}

// close asks the websocket's writer to close it, once.
func (s *sender) close(code int, reason string) {
	select {
	case s.closed <- closeFrame{code: code, reason: reason}:
	default:
	}
}
