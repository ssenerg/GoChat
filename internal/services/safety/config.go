package safety

import (
	"errors"
	"time"

	"github.com/ssenerg/gochat/internal/config"
)

type Config struct {
	Workers   int           `mapstructure:"workers"`
	OpTimeout time.Duration `mapstructure:"op_timeout"`
	// MaxBlocks caps the people one person blocks.
	MaxBlocks int `mapstructure:"max_blocks"`
	// MaxReports caps the reports one person sends per report_window.
	MaxReports   int           `mapstructure:"max_reports"`
	ReportWindow time.Duration `mapstructure:"report_window"`
}

var _ config.Config = (*Config)(nil)

func (c *Config) Validate() error {
	if c.Workers < 1 {
		return errors.New("workers must be at least 1")
	}
	if c.OpTimeout <= 0 {
		return errors.New("op_timeout must be positive")
	}
	if c.MaxBlocks < 1 || c.MaxBlocks > 10000 {
		return errors.New("max_blocks must be between 1 and 10000")
	}
	if c.MaxReports < 1 || c.ReportWindow < time.Minute {
		return errors.New("max_reports must be at least 1, and report_window at least 1m")
	}
	return nil
}
