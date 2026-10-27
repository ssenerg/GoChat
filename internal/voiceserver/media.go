package voiceserver

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // coturn's use-auth-secret signs credentials with HMAC-SHA1
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

// Media is what every peer connection of this server shares: the codecs,
// and the ports the media flows through.
type Media struct {
	api     *webrtc.API
	closers []io.Closer
	turn    *TURNConfig
}

// opus is the codec voices come in: as Discord sends them.
var opus = webrtc.RTPCodecParameters{
	RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1",
	},
	PayloadType: 111,
}

// vp8 is the codec cameras and screens come in: every browser and both
// apps' WebRTC encode and decode it, so a forwarded stream plays wherever
// it goes. Receivers ask for a keyframe with a PLI or a FIR, and the
// interceptors add NACK and transport-wide congestion control.
var vp8 = webrtc.RTPCodecParameters{
	RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeVP8, ClockRate: 90000,
		RTCPFeedback: []webrtc.RTCPFeedback{{Type: "ccm", Parameter: "fir"}},
	},
	PayloadType: 96,
}

// NewMedia opens the ICE ports. A server with a public address answers
// browsers' checks without making its own (ICE lite), as SFUs do.
func NewMedia(cfg *ICEConfig) (*Media, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(opus, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if err := m.RegisterCodec(vp8, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	// Pion's default interceptors but the simulcast header extensions: the
	// server forwards packets as they come, and a sender's mid in them would
	// steer the receivers' browsers to the wrong transceiver.
	registry := &interceptor.Registry{}
	if err := webrtc.ConfigureNack(m, registry); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureRTCPReports(registry); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureStatsInterceptor(registry); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureTWCCSender(m, registry); err != nil {
		return nil, err
	}
	media := &Media{turn: cfg.TURN}
	se := webrtc.SettingEngine{}
	se.SetLite(true)
	// Browsers find the server by the addresses it announces, never by name.
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	udp, err := ice.NewMultiUDPMuxFromPort(cfg.UDPPort)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on udp port %d: %w", cfg.UDPPort, err)
	}
	media.closers = append(media.closers, udp)
	se.SetICEUDPMux(udp)
	networks := []webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6}
	if cfg.TCPPort > 0 {
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.TCPPort))
		if err != nil {
			media.Close()
			return nil, fmt.Errorf("failed to listen on tcp port %d: %w", cfg.TCPPort, err)
		}
		tcp := webrtc.NewICETCPMux(nil, ln, 8)
		media.closers = append(media.closers, tcp)
		se.SetICETCPMux(tcp)
		networks = append(networks, webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6)
	}
	se.SetNetworkTypes(networks)
	if len(cfg.PublicIPs) > 0 {
		if err := se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External: cfg.PublicIPs, AsCandidateType: webrtc.ICECandidateTypeHost, Mode: webrtc.ICEAddressRewriteReplace,
		}); err != nil {
			media.Close()
			return nil, err
		}
	} else {
		// Without a public address, the host's own ones are all there is,
		// loopback included for a browser on this machine.
		se.SetIncludeLoopbackCandidate(true)
	}
	media.api = webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(registry), webrtc.WithSettingEngine(se))
	return media, nil
}

func (m *Media) newPeerConnection() (*webrtc.PeerConnection, error) {
	return m.api.NewPeerConnection(webrtc.Configuration{})
}

// iceServers are the TURN servers a browser may relay through, with
// credentials that expire.
func (m *Media) iceServers(userID string, now time.Time) []iceServer {
	if m.turn == nil || len(m.turn.URLs) == 0 {
		return nil
	}
	user := strconv.FormatInt(now.Add(m.turn.TTL).Unix(), 10) + ":" + userID
	mac := hmac.New(sha1.New, []byte(m.turn.Secret))
	mac.Write([]byte(user))
	return []iceServer{{URLs: m.turn.URLs, Username: user, Credential: base64.StdEncoding.EncodeToString(mac.Sum(nil))}}
}

func (m *Media) Close() {
	for _, c := range m.closers {
		_ = c.Close()
	}
}
