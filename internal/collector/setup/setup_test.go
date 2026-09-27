package setup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tedkulp/agent-history/internal/collector/config"
	"github.com/tedkulp/agent-history/internal/collector/hubclient"
	"github.com/tedkulp/agent-history/internal/collector/source"
	"github.com/tedkulp/agent-history/internal/collector/source/claudecode"
	"github.com/tedkulp/agent-history/protocol"
)

type fixture struct {
	opts       Options
	registered []protocol.MachineInfo
	ids        []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	home := t.TempDir()
	f.opts = Options{
		ConfigPath: filepath.Join(home, ".config", "agent-history", "collector.toml"),
		HubURL:     "http://hub.vpn:8080",
		Home:       home,
		Hostname:   "work-laptop.local",
		Version:    "0.3.1",
		Env:        func(string) string { return "" },
		Adapters:   []source.Adapter{claudecode.Adapter{}},
		Register: func(_ context.Context, hubURL, machineID string, info protocol.MachineInfo) error {
			f.ids = append(f.ids, machineID)
			f.registered = append(f.registered, info)
			return nil
		},
	}
	return f
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestInitWritesConfigAndRegisters(t *testing.T) {
	f := newFixture(t)
	cfg, _, err := Init(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if !uuidV4.MatchString(cfg.MachineID) {
		t.Fatalf("machine_id %q is not a UUID v4", cfg.MachineID)
	}
	wantRoot := filepath.Join(f.opts.Home, ".claude", "projects")
	if cfg.HubURL != "http://hub.vpn:8080" || cfg.DisplayName != "work-laptop" || cfg.Sources["claude-code"].Root != wantRoot {
		t.Fatalf("config %+v", cfg)
	}
	if len(f.registered) != 1 || f.ids[0] != cfg.MachineID {
		t.Fatalf("registered %v as %v", f.registered, f.ids)
	}
	info := f.registered[0]
	if info.DisplayName != "work-laptop" || info.Hostname != "work-laptop" || info.CollectorVersion != "0.3.1" || info.HomeDir != f.opts.Home {
		t.Fatalf("info %+v", info)
	}
	// The root doesn't exist: still written, and reported not detected.
	if len(info.Sources) != 1 || info.Sources[0].Detected || info.Sources[0].Root != wantRoot {
		t.Fatalf("sources %+v", info.Sources)
	}
	if _, err := os.Stat(wantRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root should not exist: %v", err)
	}
}

func TestInitDetectsExistingRoot(t *testing.T) {
	f := newFixture(t)
	root := filepath.Join(f.opts.Home, ".claude", "projects")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Init(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	si := f.registered[0].Sources[0]
	if !si.Detected || len(si.Layouts) != 1 || si.Layouts[0] != "jsonl" {
		t.Fatalf("source %+v", si)
	}
}

func TestInitRerunKeepsIDAndUserKeys(t *testing.T) {
	f := newFixture(t)
	first, _, err := Init(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.opts.ConfigPath)
	edited := strings.Replace(string(b), `rescan_interval = "10m"`, `rescan_interval = "30m"`+"\nmy_note = \"hi\"", 1)
	edited = "exclude = [\"/secret/**\"]\n" + edited
	edited = strings.Replace(edited, "[sources.claude-code]", "[sources.claude-code]\n  root = \"/custom/projects\"", 1)
	edited = regexp.MustCompile(`(?m)^\s*root = ".*\.claude/projects"\n`).ReplaceAllString(edited, "")
	if err := os.WriteFile(f.opts.ConfigPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	f.opts.HubURL = ""
	second, _, err := Init(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.MachineID != first.MachineID {
		t.Fatalf("machine_id changed: %q -> %q", first.MachineID, second.MachineID)
	}
	if second.RescanInterval != 30*time.Minute || len(second.Exclude) != 1 || second.HubURL != "http://hub.vpn:8080" {
		t.Fatalf("user edits lost: %+v", second)
	}
	if len(second.Unknown) != 1 || second.Unknown[0] != "my_note" {
		t.Fatalf("unknown %q", second.Unknown)
	}
	if got := second.Sources["claude-code"].Root; got != "/custom/projects" {
		t.Fatalf("existing root replaced: %q", got)
	}

	f.opts.ResetRoots = true
	third, _, err := Init(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := third.Sources["claude-code"].Root; got != filepath.Join(f.opts.Home, ".claude", "projects") {
		t.Fatalf("--reset-roots root %q", got)
	}
}

func TestInitName(t *testing.T) {
	f := newFixture(t)
	f.opts.Name = "desk"
	cfg, _, err := Init(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DisplayName != "desk" {
		t.Fatalf("display_name %q", cfg.DisplayName)
	}
	f.opts.Name = ""
	if cfg, _, _ = Init(context.Background(), f.opts); cfg.DisplayName != "desk" {
		t.Fatalf("re-run replaced display_name: %q", cfg.DisplayName)
	}
}

func TestInitRequiresHub(t *testing.T) {
	f := newFixture(t)
	f.opts.HubURL = ""
	if _, _, err := Init(context.Background(), f.opts); err == nil || !strings.Contains(err.Error(), "--hub") {
		t.Fatalf("err = %v", err)
	}
}

func TestInitUnreachableHub(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	f := newFixture(t)
	f.opts.HubURL = url
	f.opts.Register = func(ctx context.Context, hubURL, machineID string, info protocol.MachineInfo) error {
		c, err := hubclient.New(hubURL, machineID, "0.3.1", &http.Client{Timeout: time.Second})
		if err != nil {
			return err
		}
		return c.PutMachine(ctx, info)
	}
	_, _, err := Init(context.Background(), f.opts)
	if err == nil || !strings.Contains(err.Error(), "registering with the Hub at "+url) || !strings.Contains(err.Error(), "--offline") {
		t.Fatalf("err = %v", err)
	}

	f.opts.Offline = true
	cfg, _, err := Init(context.Background(), f.opts)
	if err != nil {
		t.Fatalf("--offline: %v", err)
	}
	if cfg.HubURL != url {
		t.Fatalf("hub_url %q", cfg.HubURL)
	}
}

func TestInitClaudeConfigDirFromShellRC(t *testing.T) {
	home := t.TempDir()
	rc := filepath.Join(home, "rc")
	if err := os.WriteFile(rc, []byte("echo welcome to my shell\nexport CLAUDE_CONFIG_DIR=/opt/claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A fake interactive shell that sources an rc file, like `zsh -i` does.
	shell := filepath.Join(home, "fakesh")
	script := "#!/bin/sh\n[ \"$1\" = -i ] && . " + rc + "\nshift\nshift\nexec sh -c \"$1\"\n"
	if err := os.WriteFile(shell, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	env, err := ShellEnv(context.Background(), shell, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	f.opts.Env = func(k string) string { return env[k] }
	cfg, _, err := Init(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Sources["claude-code"].Root; got != "/opt/claude/projects" {
		t.Fatalf("root %q", got)
	}
}

func TestShellEnvTimeout(t *testing.T) {
	shell := filepath.Join(t.TempDir(), "slowsh")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := ShellEnv(context.Background(), shell, 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

func TestParseEnv0(t *testing.T) {
	env := parseEnv0([]byte("motd line\nA=1\x00B=two\nlines\x00C=\x00junk\x00"))
	if env["A"] != "1" || env["B"] != "two\nlines" || env["C"] != "" || len(env) != 3 {
		t.Fatalf("env %q", env)
	}
}

func TestSetName(t *testing.T) {
	f := newFixture(t)
	if err := SetName(f.opts.ConfigPath, "new"); err == nil || !strings.Contains(err.Error(), "init") {
		t.Fatalf("missing config: err = %v", err)
	}
	if _, _, err := Init(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if err := SetName(f.opts.ConfigPath, "renamed"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f.opts.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DisplayName != "renamed" {
		t.Fatalf("display_name %q", cfg.DisplayName)
	}
}

func TestDisplayName(t *testing.T) {
	for in, want := range map[string]string{"box.local": "box", "box": "box", "box.lan": "box.lan"} {
		if got := DisplayName(in); got != want {
			t.Errorf("DisplayName(%q) = %q", in, got)
		}
	}
}
