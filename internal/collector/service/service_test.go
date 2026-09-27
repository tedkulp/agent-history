package service

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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
	Exec:    "/Users/ted/.local/share/mise/shims/agent-history",
	Home:    "/Users/ted",
	LogPath: "/Users/ted/.local/state/agent-history/collector.log",
}

func TestPlistGolden(t *testing.T) {
	golden(t, "default.plist", Plist(tedDef))
}

func TestPlistEscapesAndPassesEnv(t *testing.T) {
	d := Definition{
		Exec:    "/Users/a&b/shims/agent-history",
		Home:    "/Users/a&b",
		LogPath: "/Users/a&b/state/collector.log",
		Env:     map[string]string{"XDG_STATE_HOME": "/Users/a&b/state", "MISE_DATA_DIR": "/opt/<mise>"},
	}
	golden(t, "env.plist", Plist(d))
}

func TestUnitGolden(t *testing.T) {
	d := Definition{Exec: "/home/ted/.local/share/mise/shims/agent-history", Home: "/home/ted"}
	golden(t, "default.service", Unit(d))
}

func TestUnitQuotesAndPassesEnv(t *testing.T) {
	d := Definition{
		Exec: "/home/ted/my tools/100%/$bin/agent-history",
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
	return Definition{Exec: "/shim", Home: m.Home, LogPath: filepath.Join(m.Home, "state", "collector.log")}
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
	if err := m.Start(ctx); err == nil || !strings.Contains(err.Error(), "service install") {
		t.Fatalf("start before install: %v, want install guidance", err)
	}
	wantCmds(t, f)
	m.Install(ctx, m.def())
	f.cmds = nil

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
	m.Install(ctx, m.def())
	f.cmds = nil
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

func TestDefaultExec(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	self := filepath.Join(home, ".local", "bin", "agent-history")

	// No mise: the binary's own path.
	if got, err := DefaultExec(getenv, home, self); err != nil || got != self {
		t.Fatalf("no shim: %q, %v; want %q", got, err, self)
	}

	// Run from mise's installs dir with no shim: refused, naming --exec.
	versioned := filepath.Join(home, ".local", "share", "mise", "installs", "github-tedkulp-agent-history", "0.3.0", "agent-history")
	if _, err := DefaultExec(getenv, home, versioned); err == nil || !strings.Contains(err.Error(), "--exec") {
		t.Fatalf("versioned path: %v, want an error naming --exec", err)
	}
	env["MISE_DATA_DIR"] = filepath.Join(home, "mise")
	if _, err := DefaultExec(getenv, home, filepath.Join(home, "mise", "installs", "x", "agent-history")); err == nil {
		t.Fatal("versioned path under MISE_DATA_DIR: want an error")
	}
	if got, err := DefaultExec(getenv, home, versioned); err != nil || got != versioned {
		t.Fatalf("path outside MISE_DATA_DIR's installs: %q, %v", got, err)
	}

	// MISE_DATA_DIR through a symlink, self resolved (as on Linux).
	real := filepath.Join(home, "real-mise")
	os.MkdirAll(filepath.Join(real, "installs", "x"), 0o755)
	os.Symlink(real, filepath.Join(home, "linked-mise"))
	env["MISE_DATA_DIR"] = filepath.Join(home, "linked-mise")
	resolved := filepath.Join(real, "installs", "x", "agent-history")
	os.WriteFile(resolved, nil, 0o755)
	if _, err := DefaultExec(getenv, home, resolved); err == nil {
		t.Fatal("versioned path under a symlinked MISE_DATA_DIR: want an error")
	}

	// A shim present wins over the binary's own path.
	shim := ShimPath(getenv, home)
	os.MkdirAll(filepath.Dir(shim), 0o755)
	os.WriteFile(shim, nil, 0o755)
	if got, err := DefaultExec(getenv, home, self); err != nil || got != shim {
		t.Fatalf("with shim: %q, %v; want %q", got, err, shim)
	}
}

func TestOutdatedUntilReinstalled(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			m, _ := newManager(t, goos)
			if installed, _, err := Outdated(m.Path()); err != nil || installed {
				t.Fatalf("nothing installed: installed=%v err=%v", installed, err)
			}
			// A definition written by a binary with an older template.
			old := Plist(m.def())
			if goos == "linux" {
				old = Unit(m.def())
			}
			cur, prev := strconv.Itoa(TemplateVersion), strconv.Itoa(TemplateVersion-1)
			old = []byte(strings.NewReplacer(
				"service template "+cur+" -->", "service template "+prev+" -->",
				"X-AgentHistoryTemplate="+cur+"\n", "X-AgentHistoryTemplate="+prev+"\n",
			).Replace(string(old)))
			if err := os.MkdirAll(filepath.Dir(m.Path()), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(m.Path(), old, 0o644); err != nil {
				t.Fatal(err)
			}
			if installed, outdated, err := Outdated(m.Path()); err != nil || !installed || !outdated {
				t.Fatalf("older template: installed=%v outdated=%v err=%v", installed, outdated, err)
			}
			if err := m.Install(context.Background(), m.def()); err != nil {
				t.Fatal(err)
			}
			if installed, outdated, err := Outdated(m.Path()); err != nil || !installed || outdated {
				t.Fatalf("after install: installed=%v outdated=%v err=%v", installed, outdated, err)
			}
		})
	}
}

func TestInstalledExecRoundTrips(t *testing.T) {
	dir := t.TempDir()
	for _, exe := range []string{
		"/Users/ted/.local/share/mise/shims/agent-history",
		"/Users/a&b/<shims>/agent-history",
		`/home/ted/my tools/100%/$bin/"q"\agent-history`,
	} {
		d := Definition{Exec: exe, Home: "/h", LogPath: "/h/collector.log"}
		for name, b := range map[string][]byte{"a.plist": Plist(d), "a.service": Unit(d)} {
			p := filepath.Join(dir, name)
			os.WriteFile(p, b, 0o644)
			if got, err := InstalledExec(p); err != nil || got != exe {
				t.Fatalf("%s: exe %q, %v; want %q", name, got, err, exe)
			}
		}
	}
	if _, err := InstalledExec(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing definition: err %v, want ErrNotExist", err)
	}
	bad := filepath.Join(dir, "bad.service")
	os.WriteFile(bad, []byte("[Service]\n"), 0o644)
	if _, err := InstalledExec(bad); err == nil {
		t.Fatal("no error for a definition without ExecStart")
	}
}

func TestExecVersion(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agent-history")
	os.WriteFile(exe, []byte("#!/bin/sh\n[ \"$1\" = version ] && echo 0.4.0\n"), 0o755)
	if v, err := ExecVersion(context.Background(), exe); err != nil || v != "0.4.0" {
		t.Fatalf("version %q, %v", v, err)
	}
	// A manual upgrade replaces the file at the same path.
	next := filepath.Join(dir, "agent-history.new")
	os.WriteFile(next, []byte("#!/bin/sh\n[ \"$1\" = version ] && echo 0.5.0\n"), 0o755)
	if err := os.Rename(next, exe); err != nil {
		t.Fatal(err)
	}
	if v, err := ExecVersion(context.Background(), exe); err != nil || v != "0.5.0" {
		t.Fatalf("after replacing: version %q, %v", v, err)
	}
	failing := filepath.Join(dir, "failing")
	os.WriteFile(failing, []byte("#!/bin/sh\necho boom >&2\nexit 3\n"), 0o755)
	if _, err := ExecVersion(context.Background(), failing); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err %v, want its stderr", err)
	}
	if _, err := ExecVersion(context.Background(), filepath.Join(dir, "missing")); err == nil {
		t.Fatal("no error for a missing binary")
	}
}
