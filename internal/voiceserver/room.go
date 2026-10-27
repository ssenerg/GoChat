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
	// and shows video.
	addPeer struct {
		session, userID, channelID string
		video                      bool
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
	// keyframe asks for a keyframe of a peer's camera or screen.
	keyframe struct {
		session, source string
	}
	// trackAdded says a peer's packets for a source started coming in,
	// relayed to track; ssrc is the peer's.
	trackAdded struct {
		session, source string
		track           *webrtc.TrackLocalStaticRTP
		ssrc            webrtc.SSRC
	}
	trackRemoved struct {
		session, id string
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
	// receives the others'. sending is what it says it sends, and incoming
	// the sources whose packets have come.
	video    bool
	slots    map[string]*webrtc.RTPTransceiver
	sending  map[string]bool
	incoming map[string]*incoming
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

// incoming is a source of a peer whose packets come in.
type incoming struct {
	track *webrtc.TrackLocalStaticRTP
	ssrc  webrtc.SSRC
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

// roomTrack is a peer's voice, camera or screen as the others receive it.
type roomTrack struct {
	local         *webrtc.TrackLocalStaticRTP
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
			r.keyframe(p, msg.source)
		}
	case trackAdded:
		p := r.peers[msg.session]
		if p == nil {
			return
		}
		if msg.source == sourceMic {
			r.tracks[msg.track.ID()] = &roomTrack{local: msg.track, owner: p.session, source: sourceMic}
			r.signal(c)
			return
		}
		p.incoming[msg.source] = &incoming{track: msg.track, ssrc: msg.ssrc, keyframes: rate.NewLimiter(rate.Every(keyframeGap), 1)}
		r.publish(c, p, msg.source)
	case trackRemoved:
		if p := r.peers[msg.session]; p != nil {
			for source, in := range p.incoming {
				if in.track.ID() == msg.id {
					delete(p.incoming, source)
				}
			}
		}
		if t := r.tracks[msg.id]; t != nil && t.owner == msg.session {
			delete(r.tracks, msg.id)
			if p := r.peers[msg.session]; p != nil && t.source != sourceMic {
				r.report(c, p)
			}
			r.signal(c)
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
		video: msg.video, sending: make(map[string]bool), incoming: make(map[string]*incoming),
	}
	// The peer's own voice comes in on this transceiver, and its camera
	// and screen on these.
	recvonly := webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, recvonly); err != nil {
		_ = pc.Close()
		return err
	}
	if p.video {
		p.slots = make(map[string]*webrtc.RTPTransceiver, 2)
		for _, source := range []string{sourceCamera, sourceScreen} {
			t, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, recvonly)
			if err != nil {
				_ = pc.Close()
				return err
			}
			p.slots[source] = t
		}
	}
	self, engine := c.PID(), c.Engine()
	pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand != nil {
			p.candidate(cand.ToJSON())
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		engine.Send(self, pcState{session: p.session, state: s})
	})
	pc.OnTrack(func(remote *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
		forward(engine, self, p.session, p.userID, p.source(recv), remote)
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
// carry per second, which the room repeats to everyone else: Opus tops out
// at 510 kbit/s, and the clients send a camera at about 1 Mbit/s and a
// screen at about 2.5; a peer sending more is flooding the room. Each may
// send a second's worth at once, as a keyframe comes.
var maxRates = map[string]int{sourceMic: 64 << 10, sourceCamera: 320 << 10, sourceScreen: 640 << 10}

// forward relays a peer's voice, camera or screen to the track the others
// receive, until the peer goes. It runs on Pion's goroutine for the track.
func forward(engine *actor.Engine, room *actor.PID, session, userID, source string, remote *webrtc.TrackRemote) {
	if (source == sourceMic) != (remote.Kind() == webrtc.RTPCodecTypeAudio) {
		return
	}
	// The stream id tells receivers whose it is, and what.
	id, stream := "voice-"+session, userID
	if source != sourceMic {
		id, stream = source+"-"+session, userID+":"+source
	}
	local, err := webrtc.NewTrackLocalStaticRTP(remote.Codec().RTPCodecCapability, id, stream)
	if err != nil {
		return
	}
	engine.Send(room, trackAdded{session: session, source: source, track: local, ssrc: remote.SSRC()})
	defer engine.Send(room, trackRemoved{session: session, id: local.ID()})
	limit := rate.NewLimiter(rate.Limit(maxRates[source]), maxRates[source])
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

// publish puts a peer's camera or screen in the room, or takes it out, as
// the peer says it sends it: its packets may come before or after it says
// so, and it is published once both have.
func (r *room) publish(c *actor.Context, p *peer, source string) {
	in := p.incoming[source]
	if in == nil {
		return
	}
	id := in.track.ID()
	_, published := r.tracks[id]
	switch on := p.sending[source]; {
	case on && !published && r.videos() >= r.cfg.MaxVideosPerRoom:
		// Everyone gets every camera and screen: the cap bounds what the
		// server sends.
		p.sending[source] = false
		off := false
		p.out.send(outbound{Op: opVideo, Source: source, On: &off, Reason: fmt.Sprintf("this voice channel shows %d cameras and screens at most", r.cfg.MaxVideosPerRoom)})
	case on && !published:
		r.tracks[id] = &roomTrack{local: in.track, owner: p.session, source: source}
		r.report(c, p)
		r.signal(c)
		r.keyframe(p, source)
	case !on && published:
		delete(r.tracks, id)
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

// keyframe asks a peer for a keyframe of its camera or screen, so those
// who just started receiving it, or lost some, see a picture again.
func (r *room) keyframe(p *peer, source string) {
	in := p.incoming[source]
	if in == nil || !in.keyframes.Allow() {
		return
	}
	if err := p.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(in.ssrc)}}); err != nil {
		r.logger.Debug("failed to ask for a keyframe", zap.String("session", p.session), zap.Error(err))
	}
}

// send adds a track to a peer's connection where Pion's AddTrack puts it:
// on a transceiver of its kind that the last negotiation left sending
// nothing, which the browser has seen stop, or else a new one. Its sender
// is the connection's own, through the interceptors that answer NACKs and
// send reports. A voice goes first on the transceiver the peer's own voice
// comes in on, which browsers answer sendrecv.
//
// A camera or screen must never go on the peer's slots, which AddTrack
// would take as free: the server never takes an offer, so Pion never
// learns that browsers answer them sendonly. For the call, each slot holds
// a stand-in sender that AddTrack passes over, and lets it go after.
func (r *room) send(p *peer, t *roomTrack) (*webrtc.RTPSender, error) {
	if t.source == sourceMic {
		return p.pc.AddTrack(t.local)
	}
	for _, slot := range p.slots {
		stand, err := r.media.api.NewRTPSender(t.local, p.pc.SCTP().Transport())
		if err != nil {
			return nil, err
		}
		err = slot.SetSender(stand, t.local)
		// Back to receiving only, with no sender, before anything is offered.
		defer func() { _ = slot.SetSender(stand, nil) }()
		if err != nil {
			return nil, err
		}
	}
	return p.pc.AddTrack(t.local)
}

// readRTCP reads what a receiver reports on a track it gets, until the
// track leaves it. Pion's interceptors answer its NACKs on the way; its
// requests for a keyframe go to the room, for the track's sender, one per
// askGap at most.
func readRTCP(engine *actor.Engine, room *actor.PID, s *webrtc.RTPSender, want keyframe) {
	asks := rate.NewLimiter(rate.Every(askGap), 1)
	for {
		pkts, _, err := s.ReadRTCP()
		if err != nil {
			return
		}
		if want.source == sourceMic {
			continue
		}
		if slices.ContainsFunc(pkts, wantsKeyframe) && asks.Allow() {
			engine.Send(room, want)
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
				changed = true
			}
		}
		for id, t := range r.tracks {
			if have[id] || t.owner == p.session || (t.source != sourceMic && !p.video) {
				continue
			}
			s, err := r.send(p, t)
			if err != nil {
				return true
			}
			go readRTCP(c.Engine(), c.PID(), s, keyframe{session: t.owner, source: t.source})
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
		p.offer(offer.SDP)
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
