// Command agent-history is the Collector: it ships each Source's Raw records
// from this Machine to the Hub (docs/spec/collector.md).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tedkulp/agent-history/internal/buildinfo"
	"github.com/tedkulp/agent-history/internal/collector/cache"
	"github.com/tedkulp/agent-history/internal/collector/config"
	"github.com/tedkulp/agent-history/internal/collector/exclude"
	"github.com/tedkulp/agent-history/internal/collector/hubclient"
	"github.com/tedkulp/agent-history/internal/collector/runner"
	"github.com/tedkulp/agent-history/internal/collector/setup"
	"github.com/tedkulp/agent-history/internal/collector/source"
	"github.com/tedkulp/agent-history/internal/collector/source/claudecode"
	"github.com/tedkulp/agent-history/internal/collector/state"
	"github.com/tedkulp/agent-history/protocol"
)

const usage = `usage: agent-history <command>

commands:
  init --hub <url> [--name <display>] [--offline] [--reset-roots]
            set up this Machine: write collector.toml and register with the Hub
  run       ship every Source's records to the Hub and keep it current
  set-name <display>
            change this Machine's display name
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
	case "init":
		err = initCmd(os.Args[2:])
	case "run":
		err = run()
	case "set-name":
		err = setName(os.Args[2:])
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
		Info:           setup.MachineInfo(cfg, adapters, hostname(), home, buildinfo.Version),
		Sources:        sources,
		Cache:          cache.Load(filepath.Join(dir, "cache.json"), cfg.HubURL, log),
		Log:            log,
		Exclude:        excludeFilter,
		RescanInterval: cfg.RescanInterval,
	})
}

func initCmd(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	hubURL := fs.String("hub", "", "base URL of the Hub, e.g. http://hub.vpn:8080")
	name := fs.String("name", "", "display name (default: the hostname)")
	offline := fs.Bool("offline", false, "don't register with the Hub")
	resetRoots := fs.Bool("reset-roots", false, "replace configured Source roots with the defaults")
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("init: unexpected argument %q", fs.Arg(0))
	}
	home, path, err := configPath()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	getenv := os.Getenv
	if env, err := setup.ShellEnv(ctx, os.Getenv("SHELL"), setup.ShellEnvTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "agent-history: reading the shell environment: %v; using this process's environment\n", err)
	} else {
		getenv = func(k string) string { return env[k] }
	}
	cfg, info, err := setup.Init(ctx, setup.Options{
		ConfigPath: path,
		HubURL:     *hubURL,
		Name:       *name,
		Offline:    *offline,
		ResetRoots: *resetRoots,
		Home:       home,
		Hostname:   hostname(),
		Version:    buildinfo.Version,
		Env:        getenv,
		Adapters:   adapters,
		Register: func(ctx context.Context, hubURL, machineID string, info protocol.MachineInfo) error {
			hub, err := hubclient.New(hubURL, machineID, buildinfo.Version, &http.Client{Timeout: 30 * time.Second})
			if err != nil {
				return err
			}
			return hub.PutMachine(ctx, info)
		},
	})
	if err != nil {
		return err
	}
	for _, k := range cfg.Unknown {
		fmt.Fprintf(os.Stderr, "agent-history: warning: unknown config key %q ignored\n", k)
	}
	fmt.Printf("agent-history %s   machine %s   %q\n", buildinfo.Version, cfg.MachineID, cfg.DisplayName)
	fmt.Printf("Config   %s\n", path)
	if *offline {
		fmt.Printf("Hub      %s  not registered (--offline)\n", cfg.HubURL)
	} else {
		fmt.Printf("Hub      %s  registered\n", cfg.HubURL)
	}
	fmt.Println()
	for _, a := range adapters {
		if !cfg.Sources[a.ID()].IsEnabled() {
			fmt.Printf("%-12s disabled\n", a.ID())
		}
	}
	for _, si := range info.Sources {
		state := "not detected"
		if si.Detected {
			state = "detected"
		}
		fmt.Printf("%-12s %-14s %s\n", si.Source, state, si.Root)
	}
	return nil
}

func setName(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: agent-history set-name <display>")
	}
	_, path, err := configPath()
	if err != nil {
		return err
	}
	return setup.SetName(path, args[0])
}

// configPath returns the home directory and the collector.toml path.
func configPath() (home, path string, err error) {
	home, err = os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	return home, config.DefaultPath(os.Getenv, home), nil
}

// hostname is the Machine's hostname, or empty if it can't be read.
func hostname() string {
	h, _ := os.Hostname()
	return h
}
