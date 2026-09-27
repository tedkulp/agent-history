// Package config loads the Collector's collector.toml (collector.md §2.3).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the parsed collector.toml.
type Config struct {
	MachineID      string                  `toml:"machine_id"`
	DisplayName    string                  `toml:"display_name"`
	HubURL         string                  `toml:"hub_url"`
	RescanInterval time.Duration           `toml:"-"`
	RawRescan      string                  `toml:"rescan_interval"`
	LogLevel       string                  `toml:"log_level"`
	Exclude        []string                `toml:"exclude"`
	Sources        map[string]SourceConfig `toml:"sources"`

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
	c.RescanInterval = 10 * time.Minute
	if c.RawRescan != "" {
		d, err := time.ParseDuration(c.RawRescan)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("rescan_interval: %w", err))
		case d < time.Minute:
			errs = append(errs, errors.New("rescan_interval must be at least 1m"))
		default:
			c.RescanInterval = d
		}
	}
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

// header is the comment written at the top of collector.toml.
const header = "# Written by `agent-history init`. Safe to edit; restart the service to apply.\n\n"

// Doc is collector.toml as a generic table, so keys the user added survive
// a rewrite.
type Doc map[string]any

// Table returns the sub-table at key, creating it if absent. A non-table
// value at key is replaced.
func (d Doc) Table(key string) Doc {
	switch t := d[key].(type) {
	case map[string]any:
		return Doc(t)
	case Doc:
		return t
	}
	t := map[string]any{}
	d[key] = t
	return Doc(t)
}

// Edit reads the file at path as a Doc (empty if the file doesn't exist),
// applies edit, and writes it back atomically. Comments in the file are not
// kept; keys and values are.
func Edit(path string, edit func(Doc) error) error {
	d := Doc{}
	if _, err := toml.DecodeFile(path, (*map[string]any)(&d)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("config %s: %w", path, err)
	}
	if err := edit(d); err != nil {
		return err
	}
	var b bytes.Buffer
	b.WriteString(header)
	enc := toml.NewEncoder(&b)
	enc.Indent = ""
	if err := enc.Encode(map[string]any(d)); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeAtomic(path, b.Bytes())
}

func writeAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".collector-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
