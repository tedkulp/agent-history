package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "collector.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	p := writeConfig(t, `
machine_id   = "3f6c2a4e-8d1b-4f7a-9c2e-5b0d7e1a9f33"
display_name = "work-laptop"
hub_url      = "http://hub.vpn:8080"
rescan_interval = "15m"
surprise     = 1

[sources.claude-code]
root = "/Users/ted/.claude/projects"

[sources.codex]
enabled = false
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.MachineID != "3f6c2a4e-8d1b-4f7a-9c2e-5b0d7e1a9f33" || c.HubURL != "http://hub.vpn:8080" || c.LogLevel != "info" {
		t.Fatalf("config %+v", c)
	}
	if cc := c.Sources["claude-code"]; !cc.IsEnabled() || cc.Root != "/Users/ted/.claude/projects" {
		t.Fatalf("claude-code %+v", cc)
	}
	if c.Sources["codex"].IsEnabled() {
		t.Fatal("codex should be disabled")
	}
	if c.RescanInterval != 15*time.Minute {
		t.Fatalf("rescan_interval %v", c.RescanInterval)
	}
	if len(c.Unknown) != 1 || c.Unknown[0] != "surprise" {
		t.Fatalf("unknown keys %q", c.Unknown)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"machine_id is required":                       `hub_url = "http://h"`,
		"hub_url is required":                          `machine_id = "m"`,
		"at least 1m":                                  "machine_id = \"m\"\nhub_url = \"http://h\"\nrescan_interval = \"30s\"",
		"must be an absolute path":                     "machine_id = \"m\"\nhub_url = \"http://h\"\n[sources.claude-code]\nroot = \"~/.claude/projects\"",
		"sources.opencode.db must be an absolute path": "machine_id = \"m\"\nhub_url = \"http://h\"\n[sources.opencode]\ndb = \"work.db\"",
	}
	for want, body := range cases {
		_, err := Load(writeConfig(t, body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

func TestDefaultPath(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if got := DefaultPath(getenv, "/home/ted"); got != "/home/ted/.config/agent-history/collector.toml" {
		t.Fatalf("default %q", got)
	}
	env["XDG_CONFIG_HOME"] = "/xdg"
	if got := DefaultPath(getenv, "/home/ted"); got != "/xdg/agent-history/collector.toml" {
		t.Fatalf("XDG %q", got)
	}
}

func TestEditKeepsUserKeys(t *testing.T) {
	p := writeConfig(t, `
machine_id = "m"
hub_url    = "http://h"
surprise   = [1, 2]

[sources.claude-code]
root  = "/r"
extra = "kept"

[mine]
x = true
`)
	err := Edit(p, func(d Doc) error {
		d["display_name"] = "box"
		d.Table("sources").Table("claude-code")["root"] = "/new"
		d.Table("sources").Table("codex")["enabled"] = false
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(b), header) {
		t.Fatalf("missing header:\n%s", b)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.DisplayName != "box" || c.Sources["claude-code"].Root != "/new" || c.Sources["codex"].IsEnabled() {
		t.Fatalf("config %+v", c)
	}
	want := map[string]bool{"surprise": true, "sources.claude-code.extra": true, "mine": true, "mine.x": true}
	for _, k := range c.Unknown {
		delete(want, k)
	}
	if len(want) != 0 {
		t.Fatalf("user keys lost: %v (unknown %q)", want, c.Unknown)
	}
}

func TestEditCreatesFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "b", "collector.toml")
	if err := Edit(p, func(d Doc) error { d["machine_id"] = "m"; d["hub_url"] = "http://h"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", fi.Mode())
	}
}
