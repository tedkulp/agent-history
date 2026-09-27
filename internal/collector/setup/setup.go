// Package setup implements the Collector's first-run `init` and `set-name`
// (collector.md §4.1).
package setup

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	// NewID generates a Machine id; nil means a random UUID v4.
	NewID func() string
}

// Init creates or updates collector.toml and registers the Machine with the
// Hub. It returns the loaded config and the info sent to the Hub.
func Init(ctx context.Context, o Options) (*config.Config, protocol.MachineInfo, error) {
	newID := o.NewID
	if newID == nil {
		newID = NewUUID
	}
	err := config.Edit(o.ConfigPath, func(d config.Doc) error {
		if s, _ := d["machine_id"].(string); s == "" {
			d["machine_id"] = newID()
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
			d["display_name"] = DisplayName(o.Hostname)
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
				sc["root"] = a.DefaultRoot(o.Env, o.Home)
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
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no config at %s: run `agent-history init` first", path)
	}
	return config.Edit(path, func(d config.Doc) error {
		d["display_name"] = name
		return nil
	})
}

// DisplayName is the default display name: the hostname without ".local".
func DisplayName(hostname string) string { return strings.TrimSuffix(hostname, ".local") }

// MachineInfo is the PUT /machines/{id} body for cfg, detecting each
// enabled Source at its configured root.
func MachineInfo(cfg *config.Config, adapters []source.Adapter, hostname, home, version string) protocol.MachineInfo {
	info := protocol.MachineInfo{
		DisplayName:      cfg.DisplayName,
		Hostname:         DisplayName(hostname),
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
		si := protocol.SourceInfo{Source: a.ID(), Root: sc.Root, Layouts: []string{}, Detected: a.Detect(sc.Root)}
		if si.Detected {
			if v := a.Version(sc.Root); v != "" {
				si.Version = &v
			}
			for _, l := range a.Layouts() {
				si.Layouts = append(si.Layouts, l.Name())
			}
		}
		info.Sources = append(info.Sources, si)
	}
	return info
}

// ShellEnv reads the environment of the user's interactive shell by running
// `shell -i -c 'env -0'`, since service managers can't see variables set in
// shell rc files. It fails if the shell errors or takes longer than timeout.
func ShellEnv(ctx context.Context, shell string, timeout time.Duration) (map[string]string, error) {
	if shell == "" {
		return nil, errors.New("SHELL is not set")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-i", "-c", "env -0")
	// An interactive shell may start background jobs that hold stdout open.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s: timed out after %s", shell, timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", shell, err)
	}
	return parseEnv0(out), nil
}

// parseEnv0 parses NUL-separated NAME=value entries. Anything an rc file
// printed before env's output is joined to the first entry's name, so only
// the text after the name's last newline is kept.
func parseEnv0(b []byte) map[string]string {
	env := map[string]string{}
	for _, e := range bytes.Split(b, []byte{0}) {
		k, v, ok := strings.Cut(string(e), "=")
		if i := strings.LastIndexByte(k, '\n'); i >= 0 {
			k = k[i+1:]
		}
		if ok && k != "" {
			env[k] = v
		}
	}
	return env
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
