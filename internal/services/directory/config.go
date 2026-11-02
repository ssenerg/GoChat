package directory

import (
	"errors"
	"time"

	"github.com/ssenerg/gochat/internal/config"
)

type Config struct {
	Workers   int           `mapstructure:"workers"`
	OpTimeout time.Duration `mapstructure:"op_timeout"`
	InviteTTL time.Duration `mapstructure:"invite_ttl"`
	// MaxInvites caps the invites one person creates per invite_ttl.
	MaxInvites int `mapstructure:"max_invites"`
	// MaxTeamsPerDay caps the teams one person creates a day; system admins
	// create any number.
	MaxTeamsPerDay int `mapstructure:"max_teams_per_day"`
	// MaxBotConversationsPerHour caps the new direct conversations one bot
	// opens an hour.
	MaxBotConversationsPerHour int `mapstructure:"max_bot_conversations_per_hour"`
}

var _ config.Config = (*Config)(nil)

func (c *Config) Validate() error {
	if c.Workers < 1 {
		return errors.New("workers must be at least 1")
	}
	if c.OpTimeout <= 0 {
		return errors.New("op_timeout must be positive")
	}
	if c.InviteTTL < time.Minute || c.InviteTTL > 30*24*time.Hour {
		return errors.New("invite_ttl must be between 1m and 720h")
	}
	if c.MaxInvites < 1 {
		return errors.New("max_invites must be at least 1")
	}
	if c.MaxTeamsPerDay < 1 {
		return errors.New("max_teams_per_day must be at least 1")
	}
	if c.MaxBotConversationsPerHour < 1 {
		return errors.New("max_bot_conversations_per_hour must be at least 1")
	}
	return nil
}
