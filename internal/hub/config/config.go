// Package config reads the Hub's settings from env vars and `serve` flags
// (hub.md §2.3). There is no config file; a flag wins over its env var.
package config

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tedkulp/agent-history/protocol"
)

// Config is the Hub's validated settings.
type Config struct {
	Listen     string
	Data       string
	BackupDir  string
	BackupAt   *TimeOfDay // nil disables scheduled backups
	BackupKeep int
	LogLevel   slog.Level
	// MinCollectorVersion is the effective floor: the env value when it
	// raises the compiled-in one, else the compiled-in one.
	MinCollectorVersion string
	// PublicURL is the Hub's base URL for links it hands out, with no
	// trailing slash; "" derives it from each request.
	PublicURL string
}

// TimeOfDay is a container-local wall-clock time.
type TimeOfDay struct{ Hour, Minute int }

func (t TimeOfDay) String() string { return fmt.Sprintf("%02d:%02d", t.Hour, t.Minute) }

// DefaultListen is the listen address when neither AGENT_HISTORY_LISTEN nor
// --listen is set.
const DefaultListen = ":8080"

var levels = map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}

// setting is one env var and its matching flag. parse validates the value
// and stores it in the Config.
type setting struct {
	env, flag, def, usage string
	parse                 func(v string, c *Config) error
}

var settings = []setting{
	{"AGENT_HISTORY_LISTEN", "listen", DefaultListen, "listen address", func(v string, c *Config) error {
		if _, _, err := splitListen(v); err != nil {
			return err
		}
		c.Listen = v
		return nil
	}},
	{"AGENT_HISTORY_DATA", "data", "/data", "directory holding hub.db", func(v string, c *Config) error {
		c.Data = v
		return notEmpty(v)
	}},
	{"AGENT_HISTORY_BACKUP_DIR", "backup-dir", "/backups", "backup target directory", func(v string, c *Config) error {
		c.BackupDir = v
		return notEmpty(v)
	}},
	{"AGENT_HISTORY_BACKUP_AT", "backup-at", "03:00", "daily backup time HH:MM, empty to disable", func(v string, c *Config) error {
		if v == "" {
			return nil
		}
		t, err := time.Parse("15:04", v)
		if err != nil {
			return fmt.Errorf("%q is not a time like 03:00", v)
		}
		c.BackupAt = &TimeOfDay{Hour: t.Hour(), Minute: t.Minute()}
		return nil
	}},
	{"AGENT_HISTORY_BACKUP_KEEP", "backup-keep", "7", "number of scheduled backups kept", func(v string, c *Config) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("%q is not a whole number 0 or more", v)
		}
		c.BackupKeep = n
		return nil
	}},
	{"AGENT_HISTORY_LOG_LEVEL", "log-level", "info", "debug | info | warn | error", func(v string, c *Config) error {
		level, ok := levels[strings.ToLower(v)]
		if !ok {
			return fmt.Errorf("%q is not one of debug, info, warn, error", v)
		}
		c.LogLevel = level
		return nil
	}},
	{"AGENT_HISTORY_MIN_COLLECTOR_VERSION", "min-collector-version", "", "raise the minimum Collector version", func(v string, c *Config) error {
		var err error
		c.MinCollectorVersion, err = protocol.EffectiveMinCollectorVersion(v)
		return err
	}},
	{"AGENT_HISTORY_PUBLIC_URL", "public-url", "", "base URL for links the Hub hands out, such as https://history.example.com", func(v string, c *Config) error {
		if v == "" {
			return nil
		}
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%q is not an http or https URL", v)
		}
		c.PublicURL = strings.TrimRight(v, "/")
		return nil
	}},
}

func notEmpty(v string) error {
	if v == "" {
		return errors.New("must not be empty")
	}
	return nil
}

// Parse reads the settings from lookupEnv (os.LookupEnv in production), then
// from the `serve` flags in args, and validates them. The error names every
// invalid setting: the flag when it was given, else the env var.
func Parse(args []string, lookupEnv func(string) (string, bool)) (Config, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	values := make([]*string, len(settings))
	for i, s := range settings {
		def := s.def
		if v, ok := lookupEnv(s.env); ok {
			def = v
		}
		values[i] = fs.String(s.flag, def, s.usage+" (env "+s.env+")")
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	var c Config
	var errs []error
	for i, s := range settings {
		if err := s.parse(*values[i], &c); err != nil {
			name := s.env
			if set[s.flag] {
				name = "--" + s.flag
			}
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return c, nil
}

// splitListen splits a listen address into host and numeric port.
func splitListen(listen string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(listen)
	if err != nil {
		return "", "", fmt.Errorf("%q is not a host:port listen address", listen)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return "", "", fmt.Errorf("%q needs a numeric port", listen)
	}
	return host, port, nil
}

// HealthURL is the /healthz URL of a Hub listening on listen. An unspecified
// host (":8080", "0.0.0.0", "::") is reached on 127.0.0.1.
func HealthURL(listen string) (string, error) {
	host, port, err := splitListen(listen)
	if err != nil {
		return "", err
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}
