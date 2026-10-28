package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
)

type Config interface {
	Validate() error
}

// LoadConfig layers the file name under path, then environment variables
// (<envPrefix>_<KEY_PATH>), over the embedded defaults, and validates the
// result. A missing file is not an error.
func LoadConfig(cfg Config, envPrefix, path, name string, defaultConfig []byte) error {
	if path == "" {
		return errors.New("config path is empty")
	}
	if name == "" {
		return errors.New("config name is empty")
	}

	v := viper.New()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AddConfigPath(path)
	v.SetConfigName(name)
	v.SetConfigType("yaml")
	v.SetEnvPrefix(envPrefix)
	v.AutomaticEnv()
	if err := v.ReadConfig(bytes.NewReader(defaultConfig)); err != nil {
		return err
	}
	if err := v.MergeInConfig(); err != nil {
		_, okViper := errors.AsType[viper.ConfigFileNotFoundError](err)
		_, okFs := errors.AsType[*fs.PathError](err)
		if !okViper && !okFs {
			slog.Error("failed to read config file", "error", err)
			return fmt.Errorf("failed to read config file: %w", err)
		}
		slog.Warn("config file not found, running on embedded defaults and environment variables", "path", path, "name", name)
	} else {
		slog.Info("config file found", "file", v.ConfigFileUsed())
	}
	var metadata mapstructure.Metadata
	if err := v.Unmarshal(cfg, func(dc *mapstructure.DecoderConfig) {
		dc.Metadata = &metadata
		dc.DecodeHook = mapstructure.ComposeDecodeHookFunc(unitlessDurations, jsonLists, dc.DecodeHook)
	}); err != nil {
		slog.Error("failed to unmarshal config", "error", err)
		return fmt.Errorf("failed to unmarshal config: %w", err)
	}
	if len(metadata.Unused) != 0 {
		slog.Error("config has unknown keys", "keys", metadata.Unused)
		return fmt.Errorf("unknown config keys: %s", strings.Join(metadata.Unused, ", "))
	}
	if err := cfg.Validate(); err != nil {
		slog.Error("config is invalid", "error", err)
		return err
	}
	slog.Info("config loaded")
	return nil
}

// jsonLists reads a list of structs given as a string, as an environment
// variable gives it, as JSON.
func jsonLists(from, to reflect.Type, data any) (any, error) {
	if from.Kind() != reflect.String || to.Kind() != reflect.Slice || to.Elem().Kind() != reflect.Struct {
		return data, nil
	}
	s := strings.TrimSpace(data.(string))
	if s == "" {
		return []any{}, nil
	}
	var out []any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("a list needs to be JSON: %w", err)
	}
	return out, nil
}

// unitlessDurations refuses a bare number for a duration: YAML 30 would decode
// as 30ns and pass every positivity check.
func unitlessDurations(from, to reflect.Type, data any) (any, error) {
	if to != reflect.TypeFor[time.Duration]() {
		return data, nil
	}
	switch from.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return nil, fmt.Errorf("duration %v needs a unit, such as %vs", data, data)
	}
	return data, nil
}
