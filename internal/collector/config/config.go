// Package config loads the Collector's collector.toml (collector.md §2.3).
package config

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config is the parsed collector.toml.
type Config struct {
	MachineID   string                  `toml:"machine_id"`
	DisplayName string                  `toml:"display_name"`
	HubURL      string                  `toml:"hub_url"`
	LogLevel    string                  `toml:"log_level"`
	Exclude     []string                `toml:"exclude"`
	Sources     map[string]SourceConfig `toml:"sources"`

	// Unknown lists keys in the file that the Collector doesn't know.
	// The caller logs them at warn.
	Unknown []string `toml:"-"`
}

// SourceConfig is one [sources.<id>] table.
type SourceConfig struct {
	Enabled *bool  `toml:"enabled"`
	Root    string `toml:"root"`
}

// IsEnabled reports whether the Source is enabled; it defaults to true.
func (s SourceConfig) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// DefaultPath is $XDG_CONFIG_HOME/agent-history/collector.toml, defaulting
// to ~/.config/agent-history/collector.toml on every OS.
func DefaultPath(getenv func(string) string, home string) string {
	base := getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "agent-history", "collector.toml")
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	for _, k := range md.Undecoded() {
		c.Unknown = append(c.Unknown, k.String())
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	var errs []error
	if c.MachineID == "" {
		errs = append(errs, errors.New("machine_id is required"))
	}
	if c.HubURL == "" {
		errs = append(errs, errors.New("hub_url is required"))
	}
	for id, s := range c.Sources {
		if s.Root != "" && !filepath.IsAbs(s.Root) {
			errs = append(errs, fmt.Errorf("sources.%s.root must be an absolute path", id))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}
