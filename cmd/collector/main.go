// Command agent-history is the Collector: it ships each Source's Raw records
// from this Machine to the Hub (docs/spec/collector.md).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/tedkulp/agent-history/internal/buildinfo"
	"github.com/tedkulp/agent-history/internal/collector/config"
	"github.com/tedkulp/agent-history/internal/collector/hubclient"
	"github.com/tedkulp/agent-history/internal/collector/reconcile"
	"github.com/tedkulp/agent-history/internal/collector/source"
	"github.com/tedkulp/agent-history/internal/collector/source/claudecode"
	"github.com/tedkulp/agent-history/protocol"
)

const usage = `usage: agent-history <command>

commands:
  run       ship every Source's records to the Hub
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
	// Shipping records that exclude should hold back would leak them, so
	// refuse until exclude filtering exists.
	if len(cfg.Exclude) > 0 {
		return errors.New("exclude is not supported by this Collector yet; remove it from the config to run")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	info := machineInfo(cfg, home)
	var sources []reconcile.Source
	for _, a := range adapters {
		sc := cfg.Sources[a.ID()]
		if !sc.IsEnabled() {
			continue
		}
		root := sc.Root
		if root == "" {
			root = a.DefaultRoot(os.Getenv, home)
		}
		si := protocol.SourceInfo{Source: a.ID(), Root: root, Layouts: []string{}, Detected: a.Detect(root)}
		if v := a.Version(root); v != "" {
			si.Version = &v
		}
		if si.Detected {
			src := reconcile.Source{ID: a.ID()}
			for _, l := range a.Layouts() {
				recs, err := l.Discover(root)
				if err != nil {
					return fmt.Errorf("%s: discovering %s layout: %w", a.ID(), l.Name(), err)
				}
				si.Layouts = append(si.Layouts, l.Name())
				src.Records = append(src.Records, recs...)
			}
			sources = append(sources, src)
			log.Info("source detected", "source", a.ID(), "root", root, "records", len(src.Records))
		} else {
			log.Info("source not detected", "source", a.ID(), "root", root)
		}
		info.Sources = append(info.Sources, si)
	}

	hub, err := hubclient.New(cfg.HubURL, cfg.MachineID, buildinfo.Version, &http.Client{Timeout: 5 * time.Minute})
	if err != nil {
		return err
	}
	start := time.Now()
	res, err := reconcile.Reconcile(ctx, hub, info, sources, log)
	if err != nil {
		return err
	}
	log.Info("reconcile finished", "uploaded", res.Uploaded, "unchanged", res.Unchanged, "deferred", res.Deferred, "failed", res.Failed, "bytes", res.Bytes, "took", time.Since(start).Round(time.Millisecond))
	if res.Failed > 0 {
		return fmt.Errorf("%d records failed to ship", res.Failed)
	}
	return nil
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
