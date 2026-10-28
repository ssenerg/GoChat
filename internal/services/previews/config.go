package previews

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/net/idna"

	"github.com/ssenerg/gochat/internal/config"
	"github.com/ssenerg/gochat/internal/core/photo"
	"github.com/ssenerg/gochat/internal/core/unfurl"
)

type Config struct {
	// Enabled has the server preview the first link of each message.
	Enabled bool `mapstructure:"enabled"`
	// Workers bounds the previews made at once, Queue those waiting:
	// links past it go without.
	Workers int `mapstructure:"workers"`
	Queue   int `mapstructure:"queue"`
	// Timeout bounds fetching a page, and again its picture, redirects
	// included.
	Timeout      time.Duration `mapstructure:"timeout"`
	MaxRedirects int           `mapstructure:"max_redirects"`
	// MaxPageSize is how much of a page is read, MaxImageSize the largest
	// picture taken.
	MaxPageSize  int64 `mapstructure:"max_page_size"`
	MaxImageSize int64 `mapstructure:"max_image_size"`
	// Ports are those a link may name.
	Ports []int `mapstructure:"ports"`
	// CacheTTL is how long a link's preview is kept for the next message
	// with it, FailureTTL how long a link that gave none is left alone.
	CacheTTL   time.Duration `mapstructure:"cache_ttl"`
	FailureTTL time.Duration `mapstructure:"failure_ttl"`
	// AllowHosts, when not empty, are the only hosts previewed; DenyHosts
	// are never fetched from. A name covers its subdomains.
	AllowHosts []string `mapstructure:"allow_hosts"`
	DenyHosts  []string `mapstructure:"deny_hosts"`
	UserAgent  string   `mapstructure:"user_agent"`
}

var _ config.Config = (*Config)(nil)

func (c *Config) Validate() error {
	if c.Workers < 1 || c.Workers > 64 || c.Queue < 1 {
		return errors.New("workers must be between 1 and 64, and queue at least 1")
	}
	if c.Timeout <= 0 || c.Timeout > time.Minute {
		return errors.New("timeout must be positive and at most 1m")
	}
	if c.MaxRedirects < 0 || c.MaxRedirects > 10 {
		return errors.New("max_redirects must be between 0 and 10")
	}
	if c.MaxPageSize < 4096 || c.MaxPageSize > 16<<20 {
		return errors.New("max_page_size must be between 4KiB and 16MiB")
	}
	if c.MaxImageSize < 1024 || c.MaxImageSize > photo.MaxUpload {
		return fmt.Errorf("max_image_size must be between 1KiB and %d", photo.MaxUpload)
	}
	if len(c.Ports) == 0 {
		return errors.New("ports must name at least one port")
	}
	for _, p := range c.Ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("ports: %d is not a port", p)
		}
	}
	if c.CacheTTL < time.Minute || c.FailureTTL < time.Second {
		return errors.New("cache_ttl must be at least 1m and failure_ttl at least 1s")
	}
	for _, list := range []*[]string{&c.AllowHosts, &c.DenyHosts} {
		for i, h := range *list {
			ascii, err := idna.Lookup.ToASCII(strings.TrimSuffix(strings.TrimSpace(h), "."))
			if err != nil || ascii == "" || strings.ContainsAny(ascii, "/:@") {
				return fmt.Errorf("allow_hosts and deny_hosts take host names: %q is not one", h)
			}
			(*list)[i] = ascii
		}
	}
	if strings.TrimSpace(c.UserAgent) == "" {
		return errors.New("user_agent is required")
	}
	return nil
}

// Hosts is the policy of AllowHosts and DenyHosts.
func (c *Config) Hosts() unfurl.Hosts {
	return unfurl.Hosts{Allow: c.AllowHosts, Deny: c.DenyHosts}
}
