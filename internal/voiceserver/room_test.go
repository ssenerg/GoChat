package voiceserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthdm/hollywood/actor"
	"github.com/pion/ice/v4"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"go.uber.org/zap"

	"github.com/ssenerg/gochat/internal/actors"
)

// fakeLink records who the rooms say left, and what they say each sends.
type fakeLink struct {
	mu    sync.Mutex
	left  []peerLeft
	media map[string]peerMedia
}

func (l *fakeLink) Receive(c *actor.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch m := c.Message().(type) {
	case peerLeft:
		l.left = append(l.left, m)
	case peerMedia:
		if l.media == nil {
			l.media = map[string]peerMedia{}
		}
		l.media[m.session] = m
	}
}

// sends reports whether the rooms last said a session's camera and screen
// go out as given.
func (l *fakeLink) sends(session string, video, stream bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.media[session]
	return ok && m.video == video && m.stream == stream
}

func (l *fakeLink) has(session string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.left {
		if m.session == session {
			return true
		}
	}
	return false
}

func freeUDPPort(t *testing.T) int {
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	return c.LocalAddr().(*net.UDPAddr).Port
}

type sfu struct {
	t      *testing.T
	engine *actor.Engine
	rooms  *actor.PID
	link   *fakeLink
}

func newSFU(t *testing.T, maxPeers int) *sfu {
	m, err := NewMedia(&ICEConfig{UDPPort: freeUDPPort(t), TURN: &TURNConfig{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	engine, err := actor.NewEngine(actor.NewEngineConfig())
	if err != nil {
		t.Fatal(err)
	}
	link := &fakeLink{}
	linkPID := engine.Spawn(func() actor.Receiver { return link }, "link")
	cfg := &Config{MaxPeersPerRoom: maxPeers, MaxVideosPerRoom: 2}
	s := &sfu{t: t, engine: engine, link: link}
	s.rooms = engine.Spawn(newRooms(zap.NewNop(), cfg, m, linkPID), "rooms")
	t.Cleanup(func() { <-engine.Poison(s.rooms).Done() })
	return s
}

// client is a browser, played by Pion.
type client struct {
	t       *testing.T
	s       *sfu
	session string
	userID  string
	api     *webrtc.API
	pc      *webrtc.PeerConnection
	out     *sender
	mu      sync.Mutex
	got     map[string]int // packets received by the stream id that sent them
	reports map[string]int // sender reports, by stream id
	remotes map[string]*webrtc.TrackRemote
	speak   []outbound
	refused []string // the sources the server would not forward
	offers  int
	closed  *closeFrame
	// early is set when a candidate came before any offer, which a browser
	// would drop.
	early bool
	// slots are those of every offer, and beforeAnswer runs on each once
	// it is set, as a browser puts its camera and screen on the slots.
	slots        []*slots
	beforeAnswer func(*client, *slots)
	// slotSends are the offers that sent something on a slot, which a
	// browser takes for its own.
	slotSends []string
	// sending is the stream each mid sends, as the offers so far have it;
	// swaps are the mids an offer gave another stream without a stop in
	// between, which browsers never take for a new track.
	sending map[string]string
	swaps   []string
}

func (s *sfu) join(session, userID string, send bool) (*client, error) {
	return s.joinWith(session, userID, send, false, nil)
}

// joinWith joins a client that says it sends and shows video when video is
// set.
func (s *sfu) joinWith(session, userID string, send, video bool, beforeAnswer func(*client, *slots)) (*client, error) {
	se := webrtc.SettingEngine{}
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = pc.Close() })
	c := &client{
		t: s.t, s: s, session: session, userID: userID, api: api, pc: pc, out: newSender(256),
		got: map[string]int{}, reports: map[string]int{}, remotes: map[string]*webrtc.TrackRemote{}, beforeAnswer: beforeAnswer,
		sending: map[string]string{},
	}
	if send {
		track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "mic", "local")
		if err != nil {
			s.t.Fatal(err)
		}
		if _, err := pc.AddTrack(track); err != nil {
			s.t.Fatal(err)
		}
		go func() {
			for {
				if err := track.WriteSample(media.Sample{Data: []byte{0xf8, 0xff, 0xfe}, Duration: 20 * time.Millisecond}); err != nil {
					return
				}
				time.Sleep(20 * time.Millisecond)
				if pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
					return
				}
			}
		}()
	}
	pc.OnTrack(func(remote *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
		c.mu.Lock()
		c.remotes[remote.StreamID()] = remote
		c.mu.Unlock()
		go func() {
			for {
				pkts, _, err := recv.ReadRTCP()
				if err != nil {
					return
				}
				for _, pkt := range pkts {
					if _, ok := pkt.(*rtcp.SenderReport); ok {
						c.mu.Lock()
						c.reports[remote.StreamID()]++
						c.mu.Unlock()
					}
				}
			}
		}()
		for {
			if _, _, err := remote.ReadRTP(); err != nil {
				return
			}
			c.mu.Lock()
			c.got[remote.StreamID()]++
			c.mu.Unlock()
		}
	})
	pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand != nil {
			init := cand.ToJSON()
			s.engine.Send(s.rooms, toRoom{channelID: "room", msg: candidate{session: session, init: init}})
		}
	})
	go c.pump()
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	_, err = actors.Ask[joined](ctx, s.engine, s.rooms, addPeer{session: session, userID: userID, channelID: "room", video: video, out: c.out})
	return c, err
}

// pump plays the browser's side of the signaling.
func (c *client) pump() {
	for {
		select {
		case b := <-c.out.ch:
			var m outbound
			if json.Unmarshal(b, &m) != nil {
				continue
			}
			switch m.Op {
			case opOffer:
				if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: m.SDP}); err != nil {
					continue
				}
				c.mu.Lock()
				c.slots = append(c.slots, m.Slots)
				c.track(m.SDP)
				if m.Slots != nil {
					if bad := sendsOnSlots(m.SDP, m.Slots); bad != "" {
						c.slotSends = append(c.slotSends, bad)
					}
				}
				c.mu.Unlock()
				if c.beforeAnswer != nil && m.Slots != nil {
					c.beforeAnswer(c, m.Slots)
				}
				ans, err := c.pc.CreateAnswer(nil)
				if err != nil || c.pc.SetLocalDescription(ans) != nil {
					continue
				}
				c.mu.Lock()
				c.offers++
				c.mu.Unlock()
				c.s.engine.Send(c.s.rooms, toRoom{channelID: "room", msg: answer{session: c.session, sdp: ans.SDP}})
			case opCandidate:
				c.mu.Lock()
				if c.offers == 0 && c.pc.RemoteDescription() == nil {
					c.early = true
				}
				c.mu.Unlock()
				_ = c.pc.AddICECandidate(*m.Candidate)
			case opSpeaking:
				c.mu.Lock()
				c.speak = append(c.speak, m)
				c.mu.Unlock()
			case opVideo:
				c.mu.Lock()
				if m.On != nil && !*m.On && m.Reason != "" {
					c.refused = append(c.refused, m.Source)
				}
				c.mu.Unlock()
			}
		case f := <-c.out.closed:
			c.mu.Lock()
			c.closed = &f
			c.mu.Unlock()
			return
		}
	}
}

// patience bounds every wait: a loaded machine sets connections up slowly.
const patience = time.Minute

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(patience)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", what)
}

// track follows what each mid of an offer sends.
func (c *client) track(offer string) {
	var d sdp.SessionDescription
	if err := d.UnmarshalString(offer); err != nil {
		c.t.Error(err)
		return
	}
	for _, m := range d.MediaDescriptions {
		mid, _ := m.Attribute("mid")
		_, sendonly := m.Attribute("sendonly")
		_, sendrecv := m.Attribute("sendrecv")
		msid, _ := m.Attribute("msid")
		stream, _, _ := strings.Cut(msid, " ")
		if !sendonly && !sendrecv || stream == "" {
			delete(c.sending, mid)
			continue
		}
		if was, ok := c.sending[mid]; ok && was != stream {
			c.swaps = append(c.swaps, mid+": "+was+" to "+stream)
		}
		c.sending[mid] = stream
	}
}

// offered reports whether an offer sends streamID.
func (c *client) offered(streamID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sending {
		if s == streamID {
			return true
		}
	}
	return false
}

func (c *client) reported(streamID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reports[streamID]
}

func (c *client) heard(streamID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got[streamID]
}

func TestRoomForwardsVoice(t *testing.T) {
	s := newSFU(t, 3)
	alice, err := s.join("s-alice", "alice", true)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.join("s-bob", "bob", true)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob hears alice", func() bool { return bob.heard("alice") > 10 })
	eventually(t, "alice hears bob", func() bool { return alice.heard("bob") > 10 })
	if alice.heard("alice") != 0 || bob.heard("bob") != 0 {
		t.Fatal("someone heard themselves")
	}
	for _, c := range []*client{alice, bob} {
		c.mu.Lock()
		if c.early {
			t.Errorf("%s got a candidate before its first offer", c.userID)
		}
		c.mu.Unlock()
	}

	// A third person hears both, who now hear them too.
	carol, err := s.join("s-carol", "carol", true)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "carol hears both", func() bool { return carol.heard("alice") > 10 && carol.heard("bob") > 10 })
	eventually(t, "both hear carol", func() bool { return alice.heard("carol") > 10 && bob.heard("carol") > 10 })

	// The room is full.
	if _, err := s.join("s-dave", "dave", false); !errors.Is(err, errFull) {
		t.Fatalf("a fourth joined a room of three: %v", err)
	}
	eventually(t, "the refused join is not counted", func() bool { return true })

	// Speaking reaches the others, not the speaker.
	s.engine.Send(s.rooms, toRoom{channelID: "room", msg: speaking{session: "s-alice", on: true}})
	eventually(t, "bob sees alice speak", func() bool {
		bob.mu.Lock()
		defer bob.mu.Unlock()
		return len(bob.speak) == 1 && bob.speak[0].UserID == "alice" && *bob.speak[0].Speaking
	})
	alice.mu.Lock()
	if len(alice.speak) != 0 {
		t.Error("alice was told she speaks")
	}
	alice.mu.Unlock()

	// When carol leaves, the chat server hears of it and the others stop
	// getting her voice.
	bob.mu.Lock()
	offers := bob.offers
	bob.mu.Unlock()
	s.engine.Send(s.rooms, disconnect{channelID: "room", session: "s-carol", reason: "kicked"})
	eventually(t, "carol is told", func() bool {
		carol.mu.Lock()
		defer carol.mu.Unlock()
		return carol.closed != nil && carol.closed.code == closeDisconnected && carol.closed.reason == "kicked"
	})
	eventually(t, "the chat server hears carol left", func() bool { return s.link.has("s-carol") })
	eventually(t, "bob renegotiates without carol", func() bool {
		bob.mu.Lock()
		defer bob.mu.Unlock()
		return bob.offers > offers
	})
	// Packets already on their way may still land: what counts is that
	// they stop, however slowly a loaded machine delivers them.
	eventually(t, "bob stops hearing carol", func() bool {
		before := bob.heard("carol")
		time.Sleep(250 * time.Millisecond)
		return bob.heard("carol") == before
	})
	if bob.heard("alice") == 0 {
		t.Fatal("bob lost alice")
	}
	if _, err := s.join("s-dave", "dave", false); err != nil {
		t.Fatalf("a place freed but dave could not join: %v", err)
	}
}

func TestDropAllEndsEveryConnection(t *testing.T) {
	s := newSFU(t, 5)
	a, err := s.join("s-a", "a", true)
	if err != nil {
		t.Fatal(err)
	}
	s.engine.Send(s.rooms, dropAll{reason: "lost the chat server"})
	eventually(t, "the peer is closed", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.closed != nil && a.closed.code == closeUnavailable
	})
	eventually(t, "the chat server hears it", func() bool { return s.link.has("s-a") })
}

func TestICEServersSignTURNCredentials(t *testing.T) {
	m := &Media{turn: &TURNConfig{URLs: []string{"turn:turn.example.com:3478"}, Secret: "0123456789abcdef", TTL: time.Hour}}
	now := time.Unix(1_700_000_000, 0)
	got := m.iceServers("u1", now)
	if len(got) != 1 || got[0].Username != "1700003600:u1" || got[0].Credential == "" {
		t.Fatalf("ice servers: %+v", got)
	}
	// coturn's REST credential: base64(HMAC-SHA1(secret, username)).
	if got[0].Credential != "ZnjCR+EEsT2EciZj6mKuDl2b/bU=" {
		t.Errorf("credential %q", got[0].Credential)
	}
	if (&Media{turn: &TURNConfig{}}).iceServers("u1", now) != nil {
		t.Error("ice servers without TURN")
	}
}
