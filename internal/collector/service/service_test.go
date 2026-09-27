package service

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s differs from golden file (run with -update to rewrite):\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

var tedDef = Definition{
	Shim:    "/Users/ted/.local/share/mise/shims/agent-history",
	Home:    "/Users/ted",
	LogPath: "/Users/ted/.local/state/agent-history/collector.log",
}

func TestPlistGolden(t *testing.T) {
	golden(t, "default.plist", Plist(tedDef))
}

func TestPlistEscapesAndPassesEnv(t *testing.T) {
	d := Definition{
		Shim:    "/Users/a&b/shims/agent-history",
		Home:    "/Users/a&b",
		LogPath: "/Users/a&b/state/collector.log",
		Env:     map[string]string{"XDG_STATE_HOME": "/Users/a&b/state", "MISE_DATA_DIR": "/opt/<mise>"},
	}
	golden(t, "env.plist", Plist(d))
}

func TestUnitGolden(t *testing.T) {
	d := Definition{Shim: "/home/ted/.local/share/mise/shims/agent-history", Home: "/home/ted"}
	golden(t, "default.service", Unit(d))
}

func TestUnitQuotesAndPassesEnv(t *testing.T) {
	d := Definition{
		Shim: "/home/ted/my tools/100%/$bin/agent-history",
		Home: "/home/ted",
		Env:  map[string]string{"XDG_CONFIG_HOME": "/home/ted/my config", "MISE_DATA_DIR": `/home/ted/"mise"`},
	}
	golden(t, "env.service", Unit(d))
}

func TestInstalledVersion(t *testing.T) {
	dir := t.TempDir()
	for name, b := range map[string][]byte{"a.plist": Plist(tedDef), "a.service": Unit(tedDef)} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, b, 0o644)
		if v, err := InstalledVersion(p); err != nil || v != TemplateVersion {
			t.Fatalf("%s: version %d, %v; want %d", name, v, err, TemplateVersion)
		}
	}
	old := filepath.Join(dir, "old.plist")
	os.WriteFile(old, []byte("<plist></plist>"), 0o644)
	if v, err := InstalledVersion(old); err != nil || v != 0 {
		t.Fatalf("unmarked definition: version %d, %v; want 0", v, err)
	}
	if _, err := InstalledVersion(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing definition: err %v, want ErrNotExist", err)
	}
}

func TestShimPath(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if got := ShimPath(getenv, "/home/ted"); got != "/home/ted/.local/share/mise/shims/agent-history" {
		t.Fatalf("default shim %q", got)
	}
	env["MISE_DATA_DIR"] = "/opt/mise"
	if got := ShimPath(getenv, "/home/ted"); got != "/opt/mise/shims/agent-history" {
		t.Fatalf("MISE_DATA_DIR shim %q", got)
	}
}

func TestDefinitionEnv(t *testing.T) {
	env := map[string]string{"XDG_CONFIG_HOME": "/c", "PATH": "/bin", "CLAUDE_CONFIG_DIR": "/opt/claude"}
	got := DefinitionEnv(func(k string) string { return env[k] })
	if len(got) != 1 || got["XDG_CONFIG_HOME"] != "/c" {
		t.Fatalf("env %v: want only XDG_CONFIG_HOME", got)
	}
}

// fakeRunner records commands and fails the ones listed in fail.
type fakeRunner struct {
	cmds []string
	fail map[string]error
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	f.cmds = append(f.cmds, cmd)
	for prefix, err := range f.fail {
		if strings.HasPrefix(cmd, prefix) {
			return []byte("failed"), err
		}
	}
	return []byte("ok"), nil
}

func newManager(t *testing.T, goos string) (*Manager, *fakeRunner) {
	t.Helper()
	f := &fakeRunner{fail: map[string]error{}}
	home := t.TempDir()
	return &Manager{
		OS:     goos,
		Home:   home,
		UID:    501,
		User:   "ted",
		Getenv: func(string) string { return "" },
		Run:    f.run,
	}, f
}

func (m *Manager) def() Definition {
	return Definition{Shim: "/shim", Home: m.Home, LogPath: filepath.Join(m.Home, "state", "collector.log")}
}

func wantCmds(t *testing.T, f *fakeRunner, want ...string) {
	t.Helper()
	if strings.Join(f.cmds, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands:\n%s\nwant:\n%s", strings.Join(f.cmds, "\n"), strings.Join(want, "\n"))
	}
	f.cmds = nil
}

func TestLaunchdInstallIsIdempotent(t *testing.T) {
	m, f := newManager(t, "darwin")
	path := filepath.Join(m.Home, "Library", "LaunchAgents", "com.tedkulp.agent-history.plist")
	if m.Path() != path {
		t.Fatalf("path %q, want %q", m.Path(), path)
	}
	f.fail["launchctl bootout"] = errors.New("not loaded")
	for range 2 {
		if err := m.Install(context.Background(), m.def()); err != nil {
			t.Fatal(err)
		}
		wantCmds(t, f,
			"launchctl bootout gui/501/com.tedkulp.agent-history",
			"launchctl bootstrap gui/501 "+path)
	}
	b, _ := os.ReadFile(path)
	if string(b) != string(Plist(m.def())) {
		t.Fatalf("plist not written:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(m.Home, "state")); err != nil {
		t.Fatalf("log directory not created: %v", err)
	}
}

func TestLaunchdInstallReportsBootstrapFailure(t *testing.T) {
	m, f := newManager(t, "darwin")
	bootstrapRetryDelay = 0
	f.fail["launchctl bootstrap"] = errors.New("exit status 5")
	err := m.Install(context.Background(), m.def())
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("err %v, want the launchctl output", err)
	}
}

func TestLaunchdControl(t *testing.T) {
	m, f := newManager(t, "darwin")
	target := "gui/501/com.tedkulp.agent-history"
	ctx := context.Background()

	m.Start(ctx)
	wantCmds(t, f, "launchctl print "+target, "launchctl kickstart "+target)
	f.fail["launchctl print"] = errors.New("not loaded")
	m.Start(ctx)
	wantCmds(t, f, "launchctl print "+target, "launchctl bootstrap gui/501 "+m.Path())
	delete(f.fail, "launchctl print")

	m.Stop(ctx)
	wantCmds(t, f, "launchctl bootout "+target)
	m.Restart(ctx)
	wantCmds(t, f, "launchctl print "+target, "launchctl kickstart -k "+target)
	out, _ := m.Status(ctx)
	wantCmds(t, f, "launchctl print "+target)
	if out != "ok" {
		t.Fatalf("status %q", out)
	}
}

func TestLaunchdUninstallKeepsConfigAndState(t *testing.T) {
	m, f := newManager(t, "darwin")
	ctx := context.Background()
	m.Install(ctx, m.def())
	f.cmds = nil
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	wantCmds(t, f, "launchctl bootout gui/501/com.tedkulp.agent-history")
	if _, err := os.Stat(m.Path()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("plist still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.Home, "state")); err != nil {
		t.Fatalf("state removed: %v", err)
	}
	// Uninstalling again is not an error.
	f.fail["launchctl bootout"] = errors.New("not loaded")
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSystemdInstallIsIdempotent(t *testing.T) {
	m, f := newManager(t, "linux")
	path := filepath.Join(m.Home, ".config", "systemd", "user", "agent-history.service")
	if m.Path() != path {
		t.Fatalf("path %q, want %q", m.Path(), path)
	}
	for range 2 {
		if err := m.Install(context.Background(), m.def()); err != nil {
			t.Fatal(err)
		}
		wantCmds(t, f,
			"systemctl --user daemon-reload",
			"systemctl --user enable agent-history.service",
			"systemctl --user restart agent-history.service")
	}
	b, _ := os.ReadFile(path)
	if string(b) != string(Unit(m.def())) {
		t.Fatalf("unit not written:\n%s", b)
	}
}

func TestSystemdPathHonoursXDGConfigHome(t *testing.T) {
	m, _ := newManager(t, "linux")
	m.Getenv = func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return "/xdg"
		}
		return ""
	}
	if m.Path() != "/xdg/systemd/user/agent-history.service" {
		t.Fatalf("path %q", m.Path())
	}
}

func TestSystemdControlAndUninstall(t *testing.T) {
	m, f := newManager(t, "linux")
	ctx := context.Background()
	m.Start(ctx)
	m.Stop(ctx)
	m.Restart(ctx)
	f.fail["systemctl --user status"] = errors.New("exit status 3")
	out, err := m.Status(ctx)
	if err != nil || out != "failed" {
		t.Fatalf("status of a stopped unit: %q, %v; want its output and no error", out, err)
	}
	wantCmds(t, f,
		"systemctl --user start agent-history.service",
		"systemctl --user stop agent-history.service",
		"systemctl --user restart agent-history.service",
		"systemctl --user status agent-history.service")

	m.Install(ctx, m.def())
	f.cmds = nil
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	wantCmds(t, f,
		"systemctl --user disable --now agent-history.service",
		"systemctl --user daemon-reload")
	if _, err := os.Stat(m.Path()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unit still present: %v", err)
	}
}

func TestEnableLinger(t *testing.T) {
	m, f := newManager(t, "linux")
	if err := m.EnableLinger(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantCmds(t, f, "loginctl enable-linger ted")
	f.fail["loginctl"] = errors.New("denied")
	if err := m.EnableLinger(context.Background()); err == nil {
		t.Fatal("want an error when loginctl fails")
	}
}

func TestUnsupportedOS(t *testing.T) {
	m, _ := newManager(t, "windows")
	if err := m.Install(context.Background(), m.def()); err == nil {
		t.Fatal("want an error on an unsupported OS")
	}
}

func TestCheckShim(t *testing.T) {
	dir := t.TempDir()
	err := CheckShim(filepath.Join(dir, "agent-history"))
	if err == nil || !strings.Contains(err.Error(), "mise use -g github:tedkulp/agent-history") {
		t.Fatalf("missing shim: %v, want install guidance", err)
	}
	shim := filepath.Join(dir, "agent-history")
	os.WriteFile(shim, nil, 0o755)
	if err := CheckShim(shim); err != nil {
		t.Fatal(err)
	}
}
