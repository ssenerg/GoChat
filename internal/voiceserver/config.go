package voiceserver

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/ssenerg/gochat/internal/config"
	"github.com/ssenerg/gochat/internal/utils"
)

type Config struct {
	// ID names this voice server to the chat server; the host name when
	// empty.
	ID string `mapstructure:"id"`
	// PublicURL is the signaling websocket as browsers reach it.
	PublicURL string `mapstructure:"public_url"`
	// AllowedOrigins are the web gateways' origins, whose pages may open the
	// signaling websocket.
	AllowedOrigins []string    `mapstructure:"allowed_origins"`
	HTTP           *HTTPConfig `mapstructure:"http"`
	ICE            *ICEConfig  `mapstructure:"ice"`
	// MaxPeersPerRoom bounds one voice channel's talk on this server, and
	// MaxVideosPerRoom the cameras and screens it forwards, each to every
	// other peer.
	MaxPeersPerRoom  int           `mapstructure:"max_peers_per_room"`
	MaxVideosPerRoom int           `mapstructure:"max_videos_per_room"`
	IdentifyTimeout  time.Duration `mapstructure:"identify_timeout"`
	PingInterval     time.Duration `mapstructure:"ping_interval"`
	WriteTimeout     time.Duration `mapstructure:"write_timeout"`
	// ReadLimit bounds one signaling message, SendQueue how many may wait
	// for a slow browser.
	ReadLimit int64 `mapstructure:"read_limit"`
	// MaxUnidentified bounds the connections waiting to identify, which
	// anyone may open; MessageRate and MessageBurst how fast a peer may
	// signal.
	MaxUnidentified int           `mapstructure:"max_unidentified"`
	MessageRate     float64       `mapstructure:"message_rate"`
	MessageBurst    int           `mapstructure:"message_burst"`
	SendQueue       int           `mapstructure:"send_queue"`
	ReconnectMin    time.Duration `mapstructure:"reconnect_min"`
	ReconnectMax    time.Duration `mapstructure:"reconnect_max"`
}

type HTTPConfig struct {
	Address           string        `mapstructure:"address"`
	ReadHeaderTimeout time.Duration `mapstructure:"read_header_timeout"`
	ShutdownTimeout   time.Duration `mapstructure:"shutdown_timeout"`
}

// ICEConfig is where the media flows: every peer shares one UDP port, and
// one TCP port for browsers that cannot use UDP.
type ICEConfig struct {
	UDPPort int `mapstructure:"udp_port"`
	// TCPPort 0 turns ICE over TCP off.
	TCPPort int `mapstructure:"tcp_port"`
	// PublicIPs are the addresses browsers reach the ports at, announced
	// in place of the host's own, which behind NAT or in a container they
	// cannot reach.
	PublicIPs []string    `mapstructure:"public_ips"`
	TURN      *TURNConfig `mapstructure:"turn"`
}

// TURNConfig hands browsers short-lived credentials for a TURN server that
// shares Secret (coturn's use-auth-secret), for networks that block even
// the TCP port.
type TURNConfig struct {
	URLs   []string      `mapstructure:"urls"`
	Secret string        `mapstructure:"secret"`
	TTL    time.Duration `mapstructure:"ttl"`
}

var _ config.Config = (*Config)(nil)

func (c *Config) Validate() error {
	u, err := url.Parse(c.PublicURL)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return errors.New("public_url must be a ws(s) URL, such as wss://chat.example.com/rtc")
	}
	if len(c.AllowedOrigins) == 0 {
		return errors.New("allowed_origins must name the web gateway's origin")
	}
	for _, o := range c.AllowedOrigins {
		if u, err := url.Parse(o); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("allowed_origins: %q is not an http(s) origin", o)
		}
	}
	if c.HTTP == nil || c.ICE == nil || c.ICE.TURN == nil {
		return errors.New("http, ice and ice.turn are required")
	}
	if err := utils.ValidateAddress(c.HTTP.Address); err != nil {
		return fmt.Errorf("http.address: %w", err)
	}
	if c.HTTP.ReadHeaderTimeout <= 0 || c.HTTP.ShutdownTimeout <= 0 {
		return errors.New("http timeouts must be positive")
	}
	if c.ICE.UDPPort < 1 || c.ICE.UDPPort > 65535 || c.ICE.TCPPort < 0 || c.ICE.TCPPort > 65535 {
		return errors.New("ice.udp_port must be a port, and ice.tcp_port a port or 0")
	}
	for _, ip := range c.ICE.PublicIPs {
		if net.ParseIP(ip) == nil {
			return fmt.Errorf("ice.public_ips: %q is not an IP address", ip)
		}
	}
	t := c.ICE.TURN
	if len(t.URLs) > 0 && (len(t.Secret) < 16 || t.TTL < time.Minute) {
		return errors.New("ice.turn needs a secret of at least 16 characters and a ttl of at least 1m")
	}
	for _, raw := range t.URLs {
		if u, err := url.Parse(raw); err != nil || (u.Scheme != "turn" && u.Scheme != "turns") {
			return fmt.Errorf("ice.turn.urls: %q is not a turn: or turns: URL", raw)
		}
	}
	if c.MaxPeersPerRoom < 2 {
		return errors.New("max_peers_per_room must be at least 2")
	}
	if c.MaxVideosPerRoom < 0 {
		return errors.New("max_videos_per_room must be 0, for no video, or more")
	}
	if c.IdentifyTimeout <= 0 || c.PingInterval <= 0 || c.WriteTimeout <= 0 || c.ReconnectMin <= 0 || c.ReconnectMax < c.ReconnectMin {
		return errors.New("timeouts must be positive, and reconnect_max at least reconnect_min")
	}
	if c.MaxUnidentified < 1 || c.MessageRate <= 0 || c.MessageBurst < 1 {
		return errors.New("max_unidentified, message_rate and message_burst must be positive")
	}
	if c.ReadLimit < 16<<10 || c.SendQueue < 8 {
		return errors.New("read_limit must be at least 16KiB and send_queue at least 8")
	}
	return nil
}
