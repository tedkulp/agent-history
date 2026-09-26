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
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tedkulp/agent-history/internal/buildinfo"
	"github.com/tedkulp/agent-history/internal/hub/api"
	"github.com/tedkulp/agent-history/internal/hub/parser"
	"github.com/tedkulp/agent-history/internal/hub/parser/claudecode"
	"github.com/tedkulp/agent-history/internal/hub/store"
	"github.com/tedkulp/agent-history/internal/hub/web"
	"github.com/tedkulp/agent-history/internal/hub/worker"
	"github.com/tedkulp/agent-history/protocol"
)

const usage = `usage: agent-history-hub <command>

commands:
  serve     run the Hub
  version   print the version
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
	case "version":
		fmt.Println(buildinfo.Version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
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

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", envOr("AGENT_HISTORY_LISTEN", ":8080"), "listen address")
	data := fs.String("data", envOr("AGENT_HISTORY_DATA", "/data"), "directory holding hub.db")
	logLevel := fs.String("log-level", envOr("AGENT_HISTORY_LOG_LEVEL", "info"), "debug | info | warn | error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToUpper(*logLevel))); err != nil {
		return fmt.Errorf("invalid log level %q", *logLevel)
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	parsers := parser.NewRegistry(claudecode.New())
	st, err := store.Open(ctx, *data, parsers)
	if err != nil {
		return err
	}
	defer st.Close()

	workerCtx, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() {
		worker.New(st, parsers, log).Run(workerCtx)
		close(workerDone)
	}()
	defer func() { stopWorker(); <-workerDone }()

	mux := http.NewServeMux()
	mux.Handle(protocol.APIPrefix+"/", api.New(st, log))
	mux.Handle("/", web.New(st, log))
	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("hub listening", "addr", *listen, "data", *data, "version", buildinfo.Version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
