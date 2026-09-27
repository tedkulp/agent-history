// Command agent-history is the Collector: it ships each Source's Raw records
// from this Machine to the Hub (docs/spec/collector.md).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/tedkulp/agent-history/internal/buildinfo"
	"github.com/tedkulp/agent-history/internal/collector/cache"
	"github.com/tedkulp/agent-history/internal/collector/config"
	"github.com/tedkulp/agent-history/internal/collector/exclude"
	"github.com/tedkulp/agent-history/internal/collector/hubclient"
	"github.com/tedkulp/agent-history/internal/collector/runner"
	"github.com/tedkulp/agent-history/internal/collector/source"
	"github.com/tedkulp/agent-history/internal/collector/source/claudecode"
	"github.com/tedkulp/agent-history/internal/collector/state"
	"github.com/tedkulp/agent-history/protocol"
)

const usage = `usage: agent-history <command>

commands:
  run       ship every Source's records to the Hub and keep it current
  version   print the version
`

// adapters are the Sources this Collector can read.
var adapters = []source.Adapter{claudecode.Adapter{}}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = run()
	case "version":
		fmt.Println(buildinfo.Version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-history:", err)
		os.Exit(1)
	}
}

func run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := state.DefaultDir(os.Getenv, home)
	unlock, err := state.Lock(dir)
	if err != nil {
		return err
	}
	defer unlock()

	cfg, err := config.Load(config.DefaultPath(os.Getenv, home))
	if err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToUpper(cfg.LogLevel))); err != nil {
		return fmt.Errorf("invalid log_level %q", cfg.LogLevel)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	for _, k := range cfg.Unknown {
		log.Warn("unknown config key ignored", "key", k)
	}
	excludeFilter, err := exclude.New(cfg.Exclude)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var sources []runner.Source
	for _, a := range adapters {
		sc := cfg.Sources[a.ID()]
		if !sc.IsEnabled() {
			continue
		}
		root := sc.Root
		if root == "" {
			root = a.DefaultRoot(os.Getenv, home)
		}
		sources = append(sources, runner.Source{Adapter: a, Root: root})
	}

	hub, err := hubclient.New(cfg.HubURL, cfg.MachineID, buildinfo.Version, &http.Client{Timeout: 5 * time.Minute})
	if err != nil {
		return err
	}
	log.Info("collector starting", "version", buildinfo.Version, "hub", cfg.HubURL, "state_dir", dir)
	return runner.Run(ctx, runner.Config{
		Hub:            hub,
		Info:           machineInfo(cfg, home),
		Sources:        sources,
		Cache:          cache.Load(filepath.Join(dir, "cache.json"), cfg.HubURL, log),
		Log:            log,
		Exclude:        excludeFilter,
		RescanInterval: cfg.RescanInterval,
	})
}

func machineInfo(cfg *config.Config, home string) protocol.MachineInfo {
	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	name := cfg.DisplayName
	if name == "" {
		name = host
	}
	return protocol.MachineInfo{
		DisplayName:      name,
		Hostname:         host,
		OS:               runtime.GOOS,
		Arch:             runtime.GOARCH,
		HomeDir:          home,
		CollectorVersion: buildinfo.Version,
		Sources:          []protocol.SourceInfo{},
	}
}
