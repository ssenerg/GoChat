package voiceserver

import "github.com/pion/webrtc/v4"

// The signaling protocol between a browser and the voice server, one JSON
// object per websocket message, as Discord's voice gateway uses opcodes.
//
// The browser sends identify first, with the token the chat server gave
// it, and answers the offers the server makes; the server never takes an
// offer, so the two never offer at once. Both send ICE candidates as they
// find them. speaking says whether the browser hears its own voice, and
// the server tells the others.
//
// A browser that identifies with video set also sends its camera and its
// screen. Every offer then names, in slots, the mids of the two
// transceivers they go out on: the browser answers them sendonly, puts
// its camera or screen on them with replaceTrack, and says with a video
// message when it starts or stops, so turning one on or off never needs a
// new offer. The others receive each as a video stream whose id is the
// sender's user id, a colon and the source, as "u123:camera"; a voice's
// stream id is the user id alone. A room forwards a few cameras and
// screens at most: past them, the server answers a video message with its
// own, on false and a reason, and the browser stops that source.
const (
	opIdentify  = "identify"
	opReady     = "ready"
	opOffer     = "offer"
	opAnswer    = "answer"
	opCandidate = "candidate"
	opSpeaking  = "speaking"
	opVideo     = "video"
)

// The sources a peer sends, as stream ids and video messages name them.
const (
	sourceMic    = "mic"
	sourceCamera = "camera"
	sourceScreen = "screen"
)

// The close codes a browser may see.
const (
	closeIdentify     = 4001 // no valid identify in time
	closeDisconnected = 4002 // the chat server ended the talk; the reason says why
	closeFull         = 4003 // the voice channel is full
	closeUnavailable  = 4004 // the voice server cannot serve now
	closeFailed       = 4005 // the connection failed
)

type inbound struct {
	Op        string                   `json:"op"`
	Token     string                   `json:"token,omitempty"`
	SDP       string                   `json:"sdp,omitempty"`
	Candidate *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	Speaking  bool                     `json:"speaking,omitempty"`
	// Video, on identify, says the browser sends and shows video.
	Video bool `json:"video,omitempty"`
	// Source and On, on video, say which the browser starts or stops.
	Source string `json:"source,omitempty"`
	On     bool   `json:"on,omitempty"`
}

type outbound struct {
	Op         string                   `json:"op"`
	UserID     string                   `json:"user_id,omitempty"`
	ChannelID  string                   `json:"channel_id,omitempty"`
	Session    string                   `json:"session,omitempty"`
	ICEServers []iceServer              `json:"ice_servers,omitempty"`
	SDP        string                   `json:"sdp,omitempty"`
	Slots      *slots                   `json:"slots,omitempty"`
	Candidate  *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	Speaking   *bool                    `json:"speaking,omitempty"`
	// Source, On and Reason, on video, refuse a camera or screen.
	Source string `json:"source,omitempty"`
	On     *bool  `json:"on,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// slots are the mids of the transceivers a browser sends its camera and
// its screen on.
type slots struct {
	Camera string `json:"camera"`
	Screen string `json:"screen"`
}

// iceServer is an RTCIceServer as browsers take it.
type iceServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}
