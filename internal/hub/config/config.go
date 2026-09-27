// Package config reads the Hub's settings from env vars and `serve` flags
// (hub.md §2.3). There is no config file; a flag wins over its env var.
package config

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"strconv"
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
}

// TimeOfDay is a container-local wall-clock time.
type TimeOfDay struct{ Hour, Minute int }

func (t TimeOfDay) String() string { return fmt.Sprintf("%02d:%02d", t.Hour, t.Minute) }

// setting is one env var and its matching flag.
type setting struct {
	env, flag, def, usage string
	value                 *string
}

// name is how an error refers to the setting: the flag when it was given,
// else the env var.
func (s setting) name(set map[string]bool) string {
	if set[s.flag] {
		return "--" + s.flag
	}
	return s.env
}

// Parse reads the settings from lookupEnv (os.LookupEnv in production), then
// from the `serve` flags in args, and validates them.
func Parse(args []string, lookupEnv func(string) (string, bool)) (Config, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var listen, data, backupDir, backupAt, backupKeep, logLevel, minVersion string
	settings := []setting{
		{"AGENT_HISTORY_LISTEN", "listen", ":8080", "listen address", &listen},
		{"AGENT_HISTORY_DATA", "data", "/data", "directory holding hub.db", &data},
		{"AGENT_HISTORY_BACKUP_DIR", "backup-dir", "/backups", "backup target directory", &backupDir},
		{"AGENT_HISTORY_BACKUP_AT", "backup-at", "03:00", "daily backup time HH:MM, empty to disable", &backupAt},
		{"AGENT_HISTORY_BACKUP_KEEP", "backup-keep", "7", "number of scheduled backups kept", &backupKeep},
		{"AGENT_HISTORY_LOG_LEVEL", "log-level", "info", "debug | info | warn | error", &logLevel},
		{"AGENT_HISTORY_MIN_COLLECTOR_VERSION", "min-collector-version", "", "raise the minimum Collector version", &minVersion},
	}
	for _, s := range settings {
		def := s.def
		if v, ok := lookupEnv(s.env); ok {
			def = v
		}
		fs.StringVar(s.value, s.flag, def, s.usage+" (env "+s.env+")")
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	invalid := func(s setting, why string) error {
		return fmt.Errorf("%s: %q %s", s.name(set), *s.value, why)
	}

	c := Config{Listen: listen, Data: data, BackupDir: backupDir}
	if _, port, err := net.SplitHostPort(listen); err != nil {
		return Config{}, invalid(settings[0], "is not a host:port listen address")
	} else if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return Config{}, invalid(settings[0], "needs a numeric port")
	}
	for _, s := range settings[1:3] {
		if *s.value == "" {
			return Config{}, fmt.Errorf("%s: must not be empty", s.name(set))
		}
	}
	if backupAt != "" {
		t, err := time.Parse("15:04", backupAt)
		if err != nil {
			return Config{}, invalid(settings[3], "is not a time like 03:00")
		}
		c.BackupAt = &TimeOfDay{Hour: t.Hour(), Minute: t.Minute()}
	}
	keep, err := strconv.Atoi(backupKeep)
	if err != nil || keep < 0 {
		return Config{}, invalid(settings[4], "is not a whole number 0 or more")
	}
	c.BackupKeep = keep
	levels := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}
	level, ok := levels[logLevel]
	if !ok {
		return Config{}, invalid(settings[5], "is not one of debug, info, warn, error")
	}
	c.LogLevel = level
	c.MinCollectorVersion, err = protocol.EffectiveMinCollectorVersion(minVersion)
	if err != nil {
		return Config{}, invalid(settings[6], "is not a semantic version like 0.4.0")
	}
	return c, nil
}

// HealthURL is the /healthz URL of a Hub listening on listen. An unspecified
// host (":8080", "0.0.0.0", "::") is reached on 127.0.0.1.
func HealthURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		host, port = "", "8080"
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz"
}
