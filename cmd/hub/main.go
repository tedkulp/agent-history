// Command agent-history-hub is the Hub: it stores Raw records shipped by
// Collectors, parses them into Transcripts and serves the Web UI
// (docs/spec/hub.md).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/tedkulp/agent-history/internal/buildinfo"
	"github.com/tedkulp/agent-history/internal/hub/api"
	"github.com/tedkulp/agent-history/internal/hub/backup"
	"github.com/tedkulp/agent-history/internal/hub/config"
	"github.com/tedkulp/agent-history/internal/hub/live"
	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/internal/hub/parser/claudecode"
	"github.com/tedkulp/agent-history/internal/hub/parser/codex"
	"github.com/tedkulp/agent-history/internal/hub/parser/ohmypi"
	"github.com/tedkulp/agent-history/internal/hub/store"
	"github.com/tedkulp/agent-history/internal/hub/web"
	"github.com/tedkulp/agent-history/internal/hub/worker"
	"github.com/tedkulp/agent-history/protocol"
)

const usage = `usage: agent-history-hub <command>

commands:
  serve       run the Hub (flags: agent-history-hub serve --help)
  healthcheck exit 0 if the Hub on AGENT_HISTORY_LISTEN answers /healthz
  backup      write one backup now, next to the running Hub
  reparse     --all | --source <source> | --session <id>
              queue Sessions to be parsed again by the running Hub
  version     print the version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "healthcheck":
		err = healthcheck(os.Args[2:])
	case "backup":
		err = backupNow(os.Args[2:])
	case "reparse":
		err = reparse(os.Args[2:])
	case "version":
		fmt.Println(buildinfo.Version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	if errors.Is(err, flag.ErrHelp) {
		return // the flag set already printed its usage
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-history-hub:", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// parsers holds the Hub's Source parsers.
var parsers = parser.NewRegistry(claudecode.New(), codex.New(), ohmypi.New())

// startup attaches records that now map and queues stale Sessions for a
// re-parse (hub.md §4.1 steps 6 and 7).
func startup(ctx context.Context, st *store.Store, log *slog.Logger) error {
	attached, err := st.MapUnattached(ctx)
	if err != nil {
		return fmt.Errorf("mapping unattached records: %w", err)
	}
	log.Info("mapped unattached records", "attached", attached)
	stale, err := st.EnqueueStale(ctx)
	if err != nil {
		return fmt.Errorf("re-parse check: %w", err)
	}
	for _, src := range slices.Sorted(maps.Keys(stale)) {
		log.Info("re-parse check", "source", src, "parser_version", parsers[src].Version(), "queued", stale[src])
	}
	return nil
}

// side is the database flags of a subcommand that runs in a second process
// next to the live Hub (hub.md §2.2).
type side struct{ data, backupDir *string }

func sideFlags(fs *flag.FlagSet) side {
	return side{
		data:      fs.String("data", envOr("AGENT_HISTORY_DATA", "/data"), "directory holding hub.db"),
		backupDir: fs.String("backup-dir", envOr("AGENT_HISTORY_BACKUP_DIR", "/backups"), "backup target directory"),
	}
}

// open opens the database. The live Hub owns the info log, so this process
// logs only warnings and prints just its result.
func (f side) open(ctx context.Context) (*store.Store, error) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	return store.OpenWith(ctx, *f.data, parsers, store.Options{BackupDir: *f.backupDir})
}

// reparse queues Sessions at re-parse priority from a second process next to
// the live Hub, whose worker picks them up on its next poll (hub.md §2.2).
func reparse(args []string) error {
	fs := flag.NewFlagSet("reparse", flag.ContinueOnError)
	side := sideFlags(fs)
	all := fs.Bool("all", false, "every Session")
	source := fs.String("source", "", "every Session of one Source")
	session := fs.Int64("session", 0, "one Session, by the id in its Transcript URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	picked := 0
	for _, set := range []bool{*all, *source != "", *session != 0} {
		if set {
			picked++
		}
	}
	if picked != 1 || fs.NArg() > 0 {
		return errors.New("usage: agent-history-hub reparse --all | --source <source> | --session <id>")
	}
	if _, ok := parsers[*source]; *source != "" && !ok {
		return fmt.Errorf("no parser for source %q", *source)
	}
	ctx := context.Background()
	st, err := side.open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := st.Reparse(ctx, store.ReparseScope{Source: *source, SessionID: *session})
	if err != nil {
		return err
	}
	sessions := "Sessions"
	if n == 1 {
		sessions = "Session"
	}
	fmt.Printf("queued %d %s for re-parse\n", n, sessions)
	return nil
}

// backupNow writes hub-YYYYMMDD-HHMMSS.db from a second process next to the
// live Hub (hub.md §2.2, §4.8).
func backupNow(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	side := sideFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("usage: agent-history-hub backup [--data <dir>] [--backup-dir <dir>]")
	}
	ctx := context.Background()
	st, err := side.open(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	dst := filepath.Join(*side.backupDir, backup.ManualName(time.Now()))
	if err := st.Backup(ctx, dst); err != nil {
		return err
	}
	fmt.Println(dst)
	return nil
}

// healthcheck exits 0 when the Hub on the configured port answers /healthz
// with 200, for the image's HEALTHCHECK (hub.md §2.2).
func healthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	listen := fs.String("listen", envOr("AGENT_HISTORY_LISTEN", config.DefaultListen), "the Hub's listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	url, err := config.HealthURL(*listen)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return nil
}

func serve(args []string) error {
	cfg, err := config.Parse(args, os.LookupEnv)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.OpenWith(ctx, cfg.Data, parsers, store.Options{BackupDir: cfg.BackupDir})
	if err != nil {
		return err
	}
	defer st.Close()
	if err := startup(ctx, st, log); err != nil {
		return err
	}

	// Background loops stop when bgCtx is cancelled; the deferred shutdown
	// waits for them, then checkpoints before st.Close (hub.md §4.1).
	bgCtx, stopBackground := context.WithCancel(context.Background())
	var bg sync.WaitGroup
	defer func() {
		stopBackground()
		bg.Wait()
		if err := st.Checkpoint(context.Background()); err != nil {
			log.Error("shutdown checkpoint", "err", err)
		}
	}()
	// Live parses tell open Transcript pages to catch up (hub.md §4.5).
	changes := live.New()
	bg.Go(func() { worker.New(st, parsers, log).WithLive(changes).Run(bgCtx) })
	if cfg.BackupAt != nil {
		sched := &backup.Scheduler{Store: st, Dir: cfg.BackupDir, At: *cfg.BackupAt, Keep: cfg.BackupKeep, Log: log}
		bg.Go(func() { sched.Run(bgCtx) })
	}

	mux := http.NewServeMux()
	mux.Handle(protocol.APIPrefix+"/", api.New(st, log, api.Floor{HubVersion: buildinfo.Version, Min: cfg.MinCollectorVersion}))
	mux.Handle("/", web.New(st, changes, log))
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	// Shutdown waits for open requests; closing the broadcaster ends the
	// Transcript pages' event streams.
	srv.RegisterOnShutdown(changes.Close)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	backupAt := "disabled"
	if cfg.BackupAt != nil {
		backupAt = cfg.BackupAt.String()
	}
	log.Info("hub listening", "addr", cfg.Listen, "data", cfg.Data, "version", buildinfo.Version,
		"min_collector_version", cfg.MinCollectorVersion, "backup_dir", cfg.BackupDir, "backup_at", backupAt, "backup_keep", cfg.BackupKeep)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	// In-flight requests get up to 10 s; after that they are cut off.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("requests still running after 10 s, closing them", "err", err)
		srv.Close()
	}
	return nil
}
