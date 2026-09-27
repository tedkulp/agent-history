// Package service writes and controls the Collector's per-user service
// definition: a launchd agent on macOS, a systemd user unit on Linux
// (collector.md §4.8). The definition always runs the mise shim, never a
// versioned install path, so it survives `mise upgrade`.
package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// TemplateVersion is written into every definition. Bump it whenever the
// templates change, so `status` can report an outdated installed definition.
const TemplateVersion = 1

const (
	// Label is the launchd job label.
	Label = "com.tedkulp.agent-history"
	// UnitName is the systemd user unit.
	UnitName = "agent-history.service"
)

// passedEnv are the variables copied into the definition when set, because
// they move the config, state or mise directories and a service manager
// doesn't see the shell's environment.
var passedEnv = []string{"MISE_DATA_DIR", "XDG_CONFIG_HOME", "XDG_STATE_HOME"}

// Definition is what a service definition is rendered from.
type Definition struct {
	// Shim is the absolute path of the mise shim the service runs.
	Shim string
	Home string
	// LogPath receives stdout and stderr on macOS; systemd uses journald.
	LogPath string
	// Env holds extra variables beyond HOME (see DefinitionEnv).
	Env map[string]string
}

// ShimPath is $MISE_DATA_DIR/shims/agent-history, defaulting to
// ~/.local/share/mise/shims/agent-history.
func ShimPath(getenv func(string) string, home string) string {
	base := getenv("MISE_DATA_DIR")
	if base == "" {
		base = filepath.Join(home, ".local", "share", "mise")
	}
	return filepath.Join(base, "shims", "agent-history")
}

// CheckShim fails with install guidance if the shim doesn't exist.
func CheckShim(shim string) error {
	if _, err := os.Stat(shim); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no mise shim at %s: install the Collector with `mise use -g github:tedkulp/agent-history`, or pass --shim <path>", shim)
	} else if err != nil {
		return err
	}
	return nil
}

// DefinitionEnv returns the variables from getenv that the service needs to
// find the same config, state and mise directories as the shell.
func DefinitionEnv(getenv func(string) string) map[string]string {
	env := map[string]string{}
	for _, k := range passedEnv {
		if v := getenv(k); v != "" {
			env[k] = v
		}
	}
	return env
}

// envList is HOME followed by d.Env in key order.
func (d Definition) envList() [][2]string {
	list := [][2]string{{"HOME", d.Home}}
	keys := make([]string, 0, len(d.Env))
	for k := range d.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		list = append(list, [2]string{k, d.Env[k]})
	}
	return list
}

// Plist renders the launchd agent.
func Plist(d Definition) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- agent-history service template %d -->
<plist version="1.0">
<dict>
  <key>Label</key>              <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>run</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
`, TemplateVersion, Label, xmlEscape(d.Shim))
	for _, kv := range d.envList() {
		fmt.Fprintf(&b, "    <key>%s</key>%s<string>%s</string>\n", kv[0], pad(kv[0], 17), xmlEscape(kv[1]))
	}
	fmt.Fprintf(&b, `  </dict>
  <key>RunAtLoad</key>          <true/>
  <key>KeepAlive</key>          <true/>
  <key>ThrottleInterval</key>   <integer>10</integer>
  <key>StandardOutPath</key>    <string>%s</string>
  <key>StandardErrorPath</key>  <string>%s</string>
</dict>
</plist>
`, xmlEscape(d.LogPath), xmlEscape(d.LogPath))
	return b.Bytes()
}

// pad aligns values after a <key> element for readability.
func pad(key string, width int) string {
	return strings.Repeat(" ", max(1, width-len(key)))
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Unit renders the systemd user unit.
func Unit(d Definition) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, `[Unit]
Description=Agent History Collector
X-AgentHistoryTemplate=%d

[Service]
ExecStart=%s run
`, TemplateVersion, strings.ReplaceAll(systemdQuote(d.Shim), "$", "$$"))
	for _, kv := range d.envList() {
		fmt.Fprintf(&b, "Environment=%s\n", systemdQuote(kv[0]+"="+kv[1]))
	}
	b.WriteString(`Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`)
	return b.Bytes()
}

// systemdQuote escapes % specifiers and double-quotes s when it holds
// characters systemd would otherwise split on or interpret.
func systemdQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if !strings.ContainsAny(s, " \t\"'\\;") {
		return s
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

var versionMarker = regexp.MustCompile(`(?:<!-- agent-history service template |^X-AgentHistoryTemplate=)(\d+)`)

// InstalledVersion reads the template version of the definition at path. It
// returns 0 for a definition without a marker, and an error wrapping
// fs.ErrNotExist when none is installed.
func InstalledVersion(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := versionMarker.FindStringSubmatch(sc.Text()); m != nil {
			return strconv.Atoi(m[1])
		}
	}
	return 0, sc.Err()
}

// RunFunc runs a command and returns its combined output.
type RunFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// Exec runs commands for real.
func Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Manager installs and controls the service on one OS.
type Manager struct {
	// OS is runtime.GOOS: "darwin" or "linux".
	OS     string
	Home   string
	UID    int
	User   string
	Getenv func(string) string
	Run    RunFunc
}

// Path is where the service definition lives.
func (m *Manager) Path() string {
	if m.OS == "darwin" {
		return filepath.Join(m.Home, "Library", "LaunchAgents", Label+".plist")
	}
	base := m.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(m.Home, ".config")
	}
	return filepath.Join(base, "systemd", "user", UnitName)
}

func (m *Manager) supported() error {
	if m.OS != "darwin" && m.OS != "linux" {
		return fmt.Errorf("services are not supported on %s", m.OS)
	}
	return nil
}

// cmd runs a command, folding its output into the error.
func (m *Manager) cmd(ctx context.Context, name string, args ...string) error {
	out, err := m.Run(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

func (m *Manager) domain() string { return "gui/" + strconv.Itoa(m.UID) }
func (m *Manager) target() string { return m.domain() + "/" + Label }

func (m *Manager) systemctl(ctx context.Context, args ...string) error {
	return m.cmd(ctx, "systemctl", append([]string{"--user"}, args...)...)
}

// Install writes the definition and (re)loads it, starting the service. It
// is idempotent: a second install rewrites the file and reloads.
func (m *Manager) Install(ctx context.Context, d Definition) error {
	if err := m.supported(); err != nil {
		return err
	}
	var body []byte
	if m.OS == "darwin" {
		// launchd opens the log itself and doesn't create its directory.
		if err := os.MkdirAll(filepath.Dir(d.LogPath), 0o700); err != nil {
			return err
		}
		body = Plist(d)
	} else {
		body = Unit(d)
	}
	if err := os.MkdirAll(filepath.Dir(m.Path()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(m.Path(), body, 0o644); err != nil {
		return err
	}
	if m.OS == "darwin" {
		// Unloading fails when the job isn't loaded, which is fine.
		m.Run(ctx, "launchctl", "bootout", m.target())
		return m.bootstrap(ctx)
	}
	if err := m.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if err := m.systemctl(ctx, "enable", UnitName); err != nil {
		return err
	}
	// restart rather than `enable --now`, which leaves an already running
	// service on the old definition.
	return m.systemctl(ctx, "restart", UnitName)
}

// bootstrapRetryDelay spaces bootstrap attempts: launchd finishes a bootout
// asynchronously, and bootstrapping too soon fails with an I/O error.
var bootstrapRetryDelay = 500 * time.Millisecond

func (m *Manager) bootstrap(ctx context.Context) error {
	var err error
	for i := range 5 {
		if err = m.cmd(ctx, "launchctl", "bootstrap", m.domain(), m.Path()); err == nil || i == 4 {
			break
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(bootstrapRetryDelay):
		}
	}
	return err
}

// installed fails with guidance when there is no definition to control.
func (m *Manager) installed() error {
	if err := m.supported(); err != nil {
		return err
	}
	if _, err := os.Stat(m.Path()); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("service not installed: run `agent-history service install`")
	} else if err != nil {
		return err
	}
	return nil
}

// loaded reports whether launchd has the job.
func (m *Manager) loaded(ctx context.Context) bool {
	_, err := m.Run(ctx, "launchctl", "print", m.target())
	return err == nil
}

// Uninstall stops the service and removes its definition. Config and state
// are kept. Uninstalling when nothing is installed is not an error.
func (m *Manager) Uninstall(ctx context.Context) error {
	if err := m.supported(); err != nil {
		return err
	}
	// Stopping fails when the service isn't loaded, which is fine.
	if m.OS == "darwin" {
		m.Run(ctx, "launchctl", "bootout", m.target())
	} else {
		m.Run(ctx, "systemctl", "--user", "disable", "--now", UnitName)
	}
	if err := os.Remove(m.Path()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if m.OS == "linux" {
		return m.systemctl(ctx, "daemon-reload")
	}
	return nil
}

// Start starts the installed service.
func (m *Manager) Start(ctx context.Context) error {
	if err := m.installed(); err != nil {
		return err
	}
	if m.OS == "linux" {
		return m.systemctl(ctx, "start", UnitName)
	}
	if m.loaded(ctx) {
		return m.cmd(ctx, "launchctl", "kickstart", m.target())
	}
	return m.bootstrap(ctx)
}

// Stop stops the service until the next Start, login or boot.
func (m *Manager) Stop(ctx context.Context) error {
	if err := m.supported(); err != nil {
		return err
	}
	if m.OS == "linux" {
		return m.systemctl(ctx, "stop", UnitName)
	}
	return m.cmd(ctx, "launchctl", "bootout", m.target())
}

// Restart restarts the service, starting it if it isn't running.
func (m *Manager) Restart(ctx context.Context) error {
	if err := m.installed(); err != nil {
		return err
	}
	if m.OS == "linux" {
		return m.systemctl(ctx, "restart", UnitName)
	}
	if !m.loaded(ctx) {
		return m.bootstrap(ctx)
	}
	return m.cmd(ctx, "launchctl", "kickstart", "-k", m.target())
}

// Status returns the service manager's report. A stopped or missing service
// is reported in the output, not as an error.
func (m *Manager) Status(ctx context.Context) (string, error) {
	if err := m.supported(); err != nil {
		return "", err
	}
	var out []byte
	var err error
	if m.OS == "linux" {
		out, err = m.Run(ctx, "systemctl", "--user", "status", UnitName)
	} else {
		out, err = m.Run(ctx, "launchctl", "print", m.target())
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err != nil {
			return "service not loaded", nil
		}
	}
	var exitErr *exec.ExitError
	if err != nil && len(out) == 0 && !errors.As(err, &exitErr) {
		return "", err
	}
	return string(out), nil
}

// EnableLinger keeps the user's systemd instance, and so the service,
// running after logout (Linux only).
func (m *Manager) EnableLinger(ctx context.Context) error {
	return m.cmd(ctx, "loginctl", "enable-linger", m.User)
}
