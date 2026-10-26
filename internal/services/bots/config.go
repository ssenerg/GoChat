package bots

import (
	"errors"
	"time"

	"github.com/ssenerg/gochat/internal/config"
)

type Config struct {
	Workers   int           `mapstructure:"workers"`
	OpTimeout time.Duration `mapstructure:"op_timeout"`
	// MaxPerOwner bounds the bots one person owns.
	MaxPerOwner int `mapstructure:"max_per_owner"`
	// WebhookTimeout bounds one delivery, and WebhookRetryMax the wait
	// between failed ones.
	WebhookTimeout  time.Duration `mapstructure:"webhook_timeout"`
	WebhookRetryMax time.Duration `mapstructure:"webhook_retry_max"`
	// AllowPrivateWebhooks lets webhooks reach loopback and private
	// addresses, and plain http: for development only.
	AllowPrivateWebhooks bool `mapstructure:"allow_private_webhooks"`
}

var _ config.Config = (*Config)(nil)

func (c *Config) Validate() error {
	if c.Workers < 1 {
		return errors.New("workers must be at least 1")
	}
	if c.OpTimeout <= 0 {
		return errors.New("op_timeout must be positive")
	}
	if c.MaxPerOwner < 1 {
		return errors.New("max_per_owner must be at least 1")
	}
	if c.WebhookTimeout <= 0 || c.WebhookRetryMax < time.Second {
		return errors.New("webhook_timeout must be positive and webhook_retry_max at least 1s")
	}
	return nil
}
