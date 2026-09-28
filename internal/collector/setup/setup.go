// Package setup implements the Collector's first-run `init` and `set-name`
// (collector.md §4.1).
package setup

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/tedkulp/agent-history/internal/collector/config"
	"github.com/tedkulp/agent-history/internal/collector/source"
	"github.com/tedkulp/agent-history/protocol"
)

// ShellEnvTimeout bounds reading the interactive shell's environment.
const ShellEnvTimeout = 10 * time.Second

// Options configures Init.
type Options struct {
	ConfigPath string
	// HubURL is --hub. It may be empty when the config already has one.
	HubURL string
	// Name is --name; empty keeps the existing display name, or the hostname.
	Name       string
	Offline    bool
	ResetRoots bool

	Home     string
	Hostname string
	Version  string
	// Env looks up the interactive shell's environment (see ShellEnv).
	Env      func(string) string
	Adapters []source.Adapter
	// Register sends PUT /machines/{id} to the Hub at hubURL. Skipped when Offline.
	Register func(ctx context.Context, hubURL, machineID string, info protocol.MachineInfo) error
}

// Init creates or updates collector.toml and registers the Machine with the
// Hub. It returns the loaded config and the info sent to the Hub.
func Init(ctx context.Context, o Options) (*config.Config, protocol.MachineInfo, error) {
	err := config.Edit(o.ConfigPath, func(d config.Doc) error {
		if s, _ := d["machine_id"].(string); s == "" {
			d["machine_id"] = NewUUID()
		}
		switch {
		case o.HubURL != "":
			d["hub_url"] = o.HubURL
		case d["hub_url"] == nil || d["hub_url"] == "":
			return errors.New("--hub is required")
		}
		switch {
		case o.Name != "":
			d["display_name"] = o.Name
		case d["display_name"] == nil || d["display_name"] == "":
			d["display_name"] = ShortHostname(o.Hostname)
		}
		if d["rescan_interval"] == nil {
			d["rescan_interval"] = "10m"
		}
		if d["log_level"] == nil {
			d["log_level"] = "info"
		}
		sources := d.Table("sources")
		for _, a := range o.Adapters {
			sc := sources.Table(a.ID())
			if sc["enabled"] == nil {
				sc["enabled"] = true
			}
			if r, _ := sc["root"].(string); r == "" || o.ResetRoots {
				root := a.DefaultRoot(o.Env, o.Home)
				if !filepath.IsAbs(root) {
					return fmt.Errorf("%s: default root %q is not an absolute path; check the environment variables it comes from", a.ID(), root)
				}
				sc["root"] = root
			}
			if dba, ok := a.(source.DBAdapter); ok {
				// Like the root: kept once set, unless --reset-roots.
				if d, _ := sc["db"].(string); d == "" || o.ResetRoots {
					if db := dba.DefaultDB(o.Env, sc["root"].(string)); db != "" {
						sc["db"] = db
					} else {
						delete(sc, "db")
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, protocol.MachineInfo{}, err
	}
	cfg, err := config.Load(o.ConfigPath)
	if err != nil {
		return nil, protocol.MachineInfo{}, err
	}
	info := MachineInfo(cfg, o.Adapters, o.Hostname, o.Home, o.Version)
	if !o.Offline {
		if err := o.Register(ctx, cfg.HubURL, cfg.MachineID, info); err != nil {
			return nil, protocol.MachineInfo{}, fmt.Errorf("registering with the Hub at %s: %w (use --offline to set up without it)", cfg.HubURL, err)
		}
	}
	return cfg, info, nil
}

// SetName changes display_name in the config at path. The config must exist.
func SetName(path, name string) error {
	if name == "" {
		return errors.New("display name must not be empty")
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no config at %s: run `agent-history init` first", path)
	} else if err != nil {
		return err
	}
	return config.Edit(path, func(d config.Doc) error {
		d["display_name"] = name
		return nil
	})
}

// ShortHostname is the hostname without ".local", the default display name.
func ShortHostname(hostname string) string { return strings.TrimSuffix(hostname, ".local") }

// MachineInfo is the PUT /machines/{id} body for cfg, detecting each
// enabled Source at its configured root.
func MachineInfo(cfg *config.Config, adapters []source.Adapter, hostname, home, version string) protocol.MachineInfo {
	info := protocol.MachineInfo{
		DisplayName:      cfg.DisplayName,
		Hostname:         ShortHostname(hostname),
		OS:               runtime.GOOS,
		Arch:             runtime.GOARCH,
		HomeDir:          home,
		CollectorVersion: version,
		Sources:          []protocol.SourceInfo{},
	}
	if info.DisplayName == "" {
		info.DisplayName = info.Hostname
	}
	for _, a := range adapters {
		sc := cfg.Sources[a.ID()]
		if !sc.IsEnabled() || sc.Root == "" {
			continue
		}
		a = Configure(a, sc)
		si := protocol.SourceInfo{Source: a.ID(), Root: sc.Root, Layouts: []string{}, Detected: a.Detect(sc.Root)}
		if si.Detected {
			if v := a.Version(sc.Root); v != "" {
				si.Version = &v
			}
			for _, l := range a.Layouts() {
				if !source.Present(l, sc.Root) {
					continue
				}
				si.Layouts = append(si.Layouts, l.Name())
			}
		}
		info.Sources = append(info.Sources, si)
	}
	return info
}

// Configure is a with the settings in its [sources.<id>] table beyond the root.
func Configure(a source.Adapter, sc config.SourceConfig) source.Adapter {
	if dba, ok := a.(source.DBAdapter); ok && sc.DB != "" {
		return dba.WithDB(sc.DB)
	}
	return a
}

// ShellEnv reads the environment of the user's interactive shell by running
// `env -0` in `shell -i`, since service managers can't see variables set in
// shell rc files. It fails if the shell errors or takes longer than timeout.
func ShellEnv(ctx context.Context, shell string, timeout time.Duration) (map[string]string, error) {
	if shell == "" {
		return nil, errors.New("SHELL is not set")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-i", "-c", "printf '%s\\0' "+envMarker+"; env -0")
	// An interactive shell may start background jobs that hold stdout open.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s: timed out after %s", shell, timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", shell, err)
	}
	return parseEnv0(out)
}

// envMarker precedes env's output, so anything the shell's rc files print
// first is skipped.
const envMarker = "__AGENT_HISTORY_ENV__"

// parseEnv0 parses the NUL-separated NAME=value entries after envMarker.
func parseEnv0(b []byte) (map[string]string, error) {
	_, after, ok := bytes.Cut(b, []byte(envMarker+"\x00"))
	if !ok {
		return nil, errors.New("no environment in the shell's output")
	}
	env := map[string]string{}
	for _, e := range bytes.Split(after, []byte{0}) {
		if k, v, ok := strings.Cut(string(e), "="); ok && k != "" {
			env[k] = v
		}
	}
	return env, nil
}

// NewUUID returns a random UUID v4.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
