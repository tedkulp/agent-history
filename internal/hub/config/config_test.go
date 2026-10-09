package config

import (
	"log/slog"
	"strings"
	"testing"
)

func env(kv map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := kv[k]
		return v, ok
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse(nil, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Listen:              ":8080",
		Data:                "/data",
		BackupDir:           "/backups",
		BackupAt:            &TimeOfDay{Hour: 3, Minute: 0},
		BackupKeep:          7,
		LogLevel:            slog.LevelInfo,
		MinCollectorVersion: c.MinCollectorVersion,
	}
	if *c.BackupAt != *want.BackupAt {
		t.Errorf("BackupAt = %v, want %v", c.BackupAt, want.BackupAt)
	}
	c.BackupAt, want.BackupAt = nil, nil
	if c != want {
		t.Errorf("got %+v, want %+v", c, want)
	}
	if c.MinCollectorVersion == "" {
		t.Error("MinCollectorVersion is empty; want the compiled-in floor")
	}
}

func TestEnvSetsEveryValue(t *testing.T) {
	c, err := Parse(nil, env(map[string]string{
		"AGENT_HISTORY_LISTEN":                "127.0.0.1:9000",
		"AGENT_HISTORY_DATA":                  "/srv/data",
		"AGENT_HISTORY_BACKUP_DIR":            "/srv/backups",
		"AGENT_HISTORY_BACKUP_AT":             "23:45",
		"AGENT_HISTORY_BACKUP_KEEP":           "0",
		"AGENT_HISTORY_LOG_LEVEL":             "debug",
		"AGENT_HISTORY_MIN_COLLECTOR_VERSION": "99.0.0",
		"AGENT_HISTORY_PUBLIC_URL":            "https://history.example.com/",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:9000" || c.Data != "/srv/data" || c.BackupDir != "/srv/backups" ||
		*c.BackupAt != (TimeOfDay{23, 45}) || c.BackupKeep != 0 || c.LogLevel != slog.LevelDebug ||
		c.MinCollectorVersion != "99.0.0" || c.PublicURL != "https://history.example.com" {
		t.Errorf("got %+v (BackupAt %v)", c, c.BackupAt)
	}
}

func TestFlagWinsOverEnv(t *testing.T) {
	c, err := Parse([]string{
		"--listen", ":9999", "--data", "/flag/data", "--backup-dir", "/flag/backups",
		"--backup-at", "04:30", "--backup-keep", "3", "--log-level", "warn",
		"--min-collector-version", "98.0.0",
	}, env(map[string]string{
		"AGENT_HISTORY_LISTEN":                ":1111",
		"AGENT_HISTORY_DATA":                  "/env/data",
		"AGENT_HISTORY_BACKUP_DIR":            "/env/backups",
		"AGENT_HISTORY_BACKUP_AT":             "not a time",
		"AGENT_HISTORY_BACKUP_KEEP":           "-1",
		"AGENT_HISTORY_LOG_LEVEL":             "loud",
		"AGENT_HISTORY_MIN_COLLECTOR_VERSION": "nope",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9999" || c.Data != "/flag/data" || c.BackupDir != "/flag/backups" ||
		*c.BackupAt != (TimeOfDay{4, 30}) || c.BackupKeep != 3 || c.LogLevel != slog.LevelWarn ||
		c.MinCollectorVersion != "98.0.0" {
		t.Errorf("got %+v (BackupAt %v)", c, c.BackupAt)
	}
}

func TestEmptyBackupAtDisablesScheduledBackups(t *testing.T) {
	c, err := Parse(nil, env(map[string]string{"AGENT_HISTORY_BACKUP_AT": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if c.BackupAt != nil {
		t.Errorf("BackupAt = %v, want nil", c.BackupAt)
	}
}

func TestInvalidValuesNameTheSetting(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"env time", nil, map[string]string{"AGENT_HISTORY_BACKUP_AT": "25:00"}, `AGENT_HISTORY_BACKUP_AT: "25:00"`},
		{"flag time", []string{"--backup-at", "3am"}, nil, `--backup-at: "3am"`},
		{"negative keep", nil, map[string]string{"AGENT_HISTORY_BACKUP_KEEP": "-2"}, `AGENT_HISTORY_BACKUP_KEEP: "-2"`},
		{"non-numeric keep", []string{"--backup-keep", "seven"}, nil, `--backup-keep: "seven"`},
		{"log level", nil, map[string]string{"AGENT_HISTORY_LOG_LEVEL": "loud"}, `AGENT_HISTORY_LOG_LEVEL: "loud"`},
		{"semver", nil, map[string]string{"AGENT_HISTORY_MIN_COLLECTOR_VERSION": "1.2"}, `AGENT_HISTORY_MIN_COLLECTOR_VERSION: minimum Collector version "1.2"`},
		{"listen", nil, map[string]string{"AGENT_HISTORY_LISTEN": "8080"}, `AGENT_HISTORY_LISTEN: "8080"`},
		{"listen port", []string{"--listen", ":http"}, nil, `--listen: ":http"`},
		{"empty data", nil, map[string]string{"AGENT_HISTORY_DATA": ""}, `AGENT_HISTORY_DATA: must not be empty`},
		{"public url", nil, map[string]string{"AGENT_HISTORY_PUBLIC_URL": "history.example.com"}, `AGENT_HISTORY_PUBLIC_URL: "history.example.com"`},
		{"public url scheme", []string{"--public-url", "ftp://h"}, nil, `--public-url: "ftp://h"`},
		{"stray arg", []string{"extra"}, nil, `unexpected argument "extra"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.args, env(tc.env))
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestHealthURL(t *testing.T) {
	cases := map[string]string{
		":8080":          "http://127.0.0.1:8080/healthz",
		"0.0.0.0:9000":   "http://127.0.0.1:9000/healthz",
		"[::]:9000":      "http://127.0.0.1:9000/healthz",
		"127.0.0.1:7000": "http://127.0.0.1:7000/healthz",
		"10.1.2.3:7000":  "http://10.1.2.3:7000/healthz",
		"[::1]:7000":     "http://[::1]:7000/healthz",
		"localhost:7000": "http://localhost:7000/healthz",
	}
	for listen, want := range cases {
		if got, err := HealthURL(listen); err != nil || got != want {
			t.Errorf("HealthURL(%q) = %q, %v; want %q", listen, got, err, want)
		}
	}
	if _, err := HealthURL("8080"); err == nil {
		t.Error("HealthURL(\"8080\"): no error")
	}
}

func TestLogLevelIgnoresCase(t *testing.T) {
	c, err := Parse([]string{"--log-level", "WARN"}, env(nil))
	if err != nil || c.LogLevel != slog.LevelWarn {
		t.Errorf("got %v, %v; want warn", c.LogLevel, err)
	}
}

func TestEveryInvalidSettingIsReported(t *testing.T) {
	_, err := Parse([]string{"--backup-keep", "-1"}, env(map[string]string{"AGENT_HISTORY_BACKUP_AT": "noon"}))
	if err == nil || !strings.Contains(err.Error(), "--backup-keep") || !strings.Contains(err.Error(), "AGENT_HISTORY_BACKUP_AT") {
		t.Errorf("error %v does not name both settings", err)
	}
}
