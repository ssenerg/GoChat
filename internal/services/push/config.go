package push

import (
	"errors"
	"os"
	"time"

	"github.com/ssenerg/gochat/internal/config"
)

type Config struct {
	Workers   int           `mapstructure:"workers"`
	OpTimeout time.Duration `mapstructure:"op_timeout"`
	// SendTimeout bounds one push to a provider.
	SendTimeout time.Duration `mapstructure:"send_timeout"`
	// Excerpts puts what a message says in its push, cut short as in-app
	// notices show it; without, a push says "New message" alone.
	Excerpts bool        `mapstructure:"excerpts"`
	FCM      *FCMConfig  `mapstructure:"fcm"`
	APNs     *APNsConfig `mapstructure:"apns"`
}

// FCMConfig turns on Firebase Cloud Messaging, for Android, with a service
// account's JSON key: Credentials holds it, or CredentialsFile names it.
type FCMConfig struct {
	Credentials     string `mapstructure:"credentials"`
	CredentialsFile string `mapstructure:"credentials_file"`
}

// APNsConfig turns on APNs, for iOS, with a token signing key (.p8): Key
// holds it, or KeyFile names it.
type APNsConfig struct {
	Key     string `mapstructure:"key"`
	KeyFile string `mapstructure:"key_file"`
	KeyID   string `mapstructure:"key_id"`
	TeamID  string `mapstructure:"team_id"`
	// Topic is the app's bundle id.
	Topic string `mapstructure:"topic"`
}

var _ config.Config = (*Config)(nil)

func (c *Config) Validate() error {
	if c.Workers < 1 {
		return errors.New("workers must be at least 1")
	}
	if c.OpTimeout <= 0 || c.SendTimeout <= 0 {
		return errors.New("op_timeout and send_timeout must be positive")
	}
	if c.FCM == nil || c.APNs == nil {
		return errors.New("fcm and apns are required")
	}
	if c.FCM.Credentials != "" && c.FCM.CredentialsFile != "" {
		return errors.New("fcm takes credentials or credentials_file, not both")
	}
	if c.APNs.Key != "" && c.APNs.KeyFile != "" {
		return errors.New("apns takes key or key_file, not both")
	}
	if c.APNs.Enabled() && (c.APNs.KeyID == "" || c.APNs.TeamID == "" || c.APNs.Topic == "") {
		return errors.New("apns needs key_id, team_id and topic with its key")
	}
	return nil
}

func (c *FCMConfig) Enabled() bool  { return c.Credentials != "" || c.CredentialsFile != "" }
func (c *APNsConfig) Enabled() bool { return c.Key != "" || c.KeyFile != "" }

// secret is a value given inline or in a file.
func secret(inline, file string) ([]byte, error) {
	if inline != "" {
		return []byte(inline), nil
	}
	return os.ReadFile(file) //nolint:gosec // the operator's own path
}
