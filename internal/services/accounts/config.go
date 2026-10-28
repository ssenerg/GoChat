package accounts

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ssenerg/gochat/internal/config"
	"github.com/ssenerg/gochat/internal/core/password"
	"github.com/ssenerg/gochat/internal/core/secure"
)

type Config struct {
	Workers           int             `mapstructure:"workers"`
	OpTimeout         time.Duration   `mapstructure:"op_timeout"`
	Password          password.Params `mapstructure:"password"`
	MinPasswordLength int             `mapstructure:"min_password_length"`
	SessionTTL        time.Duration   `mapstructure:"session_ttl"`
	SessionCacheTTL   time.Duration   `mapstructure:"session_cache_ttl"`
	SessionCacheSize  int             `mapstructure:"session_cache_size"`
	// LastSeenInterval is how often a session in use records it.
	LastSeenInterval time.Duration `mapstructure:"last_seen_interval"`
	// SessionIdleTimeout ends a session unused for this long; zero keeps
	// sessions until session_ttl.
	SessionIdleTimeout time.Duration `mapstructure:"session_idle_timeout"`
	MFATTL             time.Duration `mapstructure:"mfa_ttl"`
	MFAMaxAttempts     int           `mapstructure:"mfa_max_attempts"`
	TOTPIssuer         string        `mapstructure:"totp_issuer"`
	// SecretKey seals TOTP secrets at rest: 32 bytes, base64.
	SecretKey  string `mapstructure:"secret_key"`
	OpenSignup bool   `mapstructure:"open_signup"`
	// SetupToken must be given to create the first account. Without one, no
	// account can be created unless AllowOpenBootstrap lets the first
	// sign-up become the admin.
	SetupToken         string        `mapstructure:"setup_token"`
	AllowOpenBootstrap bool          `mapstructure:"allow_open_bootstrap"`
	LoginFailures      int           `mapstructure:"login_failures"`
	LoginWindow        time.Duration `mapstructure:"login_window"`
	SSO                SSOConfig     `mapstructure:"sso"`
}

// SSOConfig lets people sign in with identity providers over OpenID
// Connect; without providers it is off.
type SSOConfig struct {
	// FlowTTL bounds a sign-in from leaving for the provider to finishing.
	FlowTTL time.Duration `mapstructure:"flow_ttl"`
	// Timeout bounds each request to a provider.
	Timeout   time.Duration `mapstructure:"timeout"`
	Providers []SSOProvider `mapstructure:"providers"`
}

type SSOProvider struct {
	// ID names the provider in URLs, such as google.
	ID string `mapstructure:"id"`
	// Name is what the button says: "Sign in with <name>".
	Name   string `mapstructure:"name"`
	Issuer string `mapstructure:"issuer"`
	// ClientID and the secret, or the file holding it, of GoChat's client
	// at the provider.
	ClientID         string `mapstructure:"client_id"`
	ClientSecret     string `mapstructure:"client_secret"`
	ClientSecretFile string `mapstructure:"client_secret_file"`
	// Scopes are asked for beside openid, email and profile.
	Scopes []string `mapstructure:"scopes"`
	// AllowedDomains, when set, admits only verified emails at them.
	AllowedDomains []string `mapstructure:"allowed_domains"`
}

var (
	ssoIDRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	ssoDomainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
)

func (c *SSOConfig) Validate() error {
	if len(c.Providers) == 0 {
		return nil
	}
	if c.FlowTTL < time.Minute || c.FlowTTL > time.Hour {
		return errors.New("flow_ttl must be between 1m and 1h")
	}
	if c.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	seen := make(map[string]bool)
	for i := range c.Providers {
		p := &c.Providers[i]
		// The gateway's own paths under /login/sso/ take these.
		if !ssoIDRe.MatchString(p.ID) || seen[p.ID] || p.ID == "link" || p.ID == "callback" {
			return fmt.Errorf("providers: %q is not a unique id of lowercase letters, digits and '-'", p.ID)
		}
		seen[p.ID] = true
		if err := p.validate(); err != nil {
			return fmt.Errorf("providers.%s.%w", p.ID, err)
		}
	}
	return nil
}

func (p *SSOProvider) validate() error {
	if n := len(p.Name); n == 0 || n > 64 {
		return errors.New("name must be 1-64 characters")
	}
	u, err := url.Parse(p.Issuer)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "https" && (u.Scheme != "http" || !loopback(u.Hostname()))) {
		return errors.New("issuer must be an https URL")
	}
	if p.ClientID == "" {
		return errors.New("client_id is required")
	}
	if (p.ClientSecret == "") == (p.ClientSecretFile == "") {
		return errors.New("client_secret or client_secret_file is required, not both")
	}
	for i, d := range p.AllowedDomains {
		d = strings.ToLower(strings.TrimSpace(d))
		if !ssoDomainRe.MatchString(d) {
			return fmt.Errorf("allowed_domains: %q is not a domain", d)
		}
		p.AllowedDomains[i] = d
	}
	return nil
}

// loopback lets an issuer on this machine, as in tests, go without TLS.
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var _ config.Config = (*Config)(nil)

func (c *Config) Validate() error {
	if c.Workers < 1 {
		return errors.New("workers must be at least 1")
	}
	if c.OpTimeout <= 0 {
		return errors.New("op_timeout must be positive")
	}
	if err := c.Password.Validate(); err != nil {
		return fmt.Errorf("password.%w", err)
	}
	if c.MinPasswordLength < 8 {
		return errors.New("min_password_length must be at least 8")
	}
	if c.SessionTTL < time.Minute || c.MFATTL < 30*time.Second {
		return errors.New("session_ttl and mfa_ttl are too short")
	}
	if c.SessionCacheTTL <= 0 || c.SessionCacheSize < 1 {
		return errors.New("session_cache_ttl and session_cache_size must be positive")
	}
	if c.LastSeenInterval < time.Minute {
		return errors.New("last_seen_interval must be at least 1m")
	}
	// Use is recorded every last_seen_interval at most: a shorter timeout
	// would end sessions in use.
	if c.SessionIdleTimeout != 0 && c.SessionIdleTimeout < 2*(c.LastSeenInterval+c.SessionCacheTTL) {
		return errors.New("session_idle_timeout must be zero or at least twice last_seen_interval and session_cache_ttl together")
	}
	if c.MFAMaxAttempts < 1 {
		return errors.New("mfa_max_attempts must be at least 1")
	}
	if c.TOTPIssuer == "" {
		return errors.New("totp_issuer is required")
	}
	if _, err := secure.DecodeKey(c.SecretKey); err != nil {
		return fmt.Errorf("secret_key: %w", err)
	}
	if c.LoginFailures < 1 || c.LoginWindow <= 0 {
		return errors.New("login_failures and login_window must be positive")
	}
	if err := c.SSO.Validate(); err != nil {
		return fmt.Errorf("sso.%w", err)
	}
	return nil
}
