package updates

import (
	"errors"
	"time"

	"github.com/ssenerg/gochat/internal/config"
)

type Config struct {
	// Retention is how long updates are kept for clients to catch up.
	Retention time.Duration `mapstructure:"retention"`
	// IdleTimeout is how long an account's sequencer outlives its last
	// update.
	IdleTimeout time.Duration `mapstructure:"idle_timeout"`
	OpTimeout   time.Duration `mapstructure:"op_timeout"`
	// DifferenceLimit bounds the updates one difference returns.
	DifferenceLimit int `mapstructure:"difference_limit"`
}

var _ config.Config = (*Config)(nil)

func (c *Config) Validate() error {
	if c.Retention < time.Hour {
		return errors.New("retention must be at least an hour")
	}
	if c.IdleTimeout < time.Second || c.OpTimeout <= 0 {
		return errors.New("idle_timeout and op_timeout must be positive")
	}
	if c.DifferenceLimit < 1 || c.DifferenceLimit > 1000 {
		return errors.New("difference_limit must be between 1 and 1000")
	}
	return nil
}
