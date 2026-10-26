package services

import (
	"errors"
	"fmt"
	"time"

	"github.com/ssenerg/gochat/internal/config"
	"github.com/ssenerg/gochat/internal/services/accounts"
	"github.com/ssenerg/gochat/internal/services/bots"
	"github.com/ssenerg/gochat/internal/services/channels"
	"github.com/ssenerg/gochat/internal/services/directory"
	"github.com/ssenerg/gochat/internal/services/files"
	"github.com/ssenerg/gochat/internal/services/hub"
	"github.com/ssenerg/gochat/internal/services/index"
	"github.com/ssenerg/gochat/internal/services/updates"
	"github.com/ssenerg/gochat/internal/services/voice"
)

type Config struct {
	Accounts  *accounts.Config  `mapstructure:"accounts"`
	Directory *directory.Config `mapstructure:"directory"`
	Channels  *channels.Config  `mapstructure:"channels"`
	Files     *files.Config     `mapstructure:"files"`
	Hub       *hub.Config       `mapstructure:"hub"`
	Search    *index.Config     `mapstructure:"search"`
	Voice     *voice.Config     `mapstructure:"voice"`
	Updates   *updates.Config   `mapstructure:"updates"`
	Bots      *bots.Config      `mapstructure:"bots"`
	Users     *UsersConfig      `mapstructure:"users"`
}

type UsersConfig struct {
	CacheSize int           `mapstructure:"cache_size"`
	CacheTTL  time.Duration `mapstructure:"cache_ttl"`
}

var _ config.Config = (*Config)(nil)

func (c *Config) Validate() error {
	if c.Accounts == nil || c.Directory == nil || c.Channels == nil || c.Files == nil || c.Hub == nil || c.Search == nil || c.Voice == nil || c.Updates == nil || c.Bots == nil || c.Users == nil {
		return errors.New("accounts, directory, channels, files, hub, search, voice, updates, bots and users are required")
	}
	if err := c.Accounts.Validate(); err != nil {
		return fmt.Errorf("accounts.%w", err)
	}
	if err := c.Directory.Validate(); err != nil {
		return fmt.Errorf("directory.%w", err)
	}
	if err := c.Channels.Validate(); err != nil {
		return fmt.Errorf("channels.%w", err)
	}
	if err := c.Files.Validate(); err != nil {
		return fmt.Errorf("files.%w", err)
	}
	if err := c.Hub.Validate(); err != nil {
		return fmt.Errorf("hub.%w", err)
	}
	if err := c.Search.Validate(); err != nil {
		return fmt.Errorf("search.%w", err)
	}
	if err := c.Voice.Validate(); err != nil {
		return fmt.Errorf("voice.%w", err)
	}
	if err := c.Updates.Validate(); err != nil {
		return fmt.Errorf("updates.%w", err)
	}
	if err := c.Bots.Validate(); err != nil {
		return fmt.Errorf("bots.%w", err)
	}
	if c.Users.CacheSize < 1 || c.Users.CacheTTL <= 0 {
		return errors.New("users.cache_size and users.cache_ttl must be positive")
	}
	return nil
}
