// Command agent-history is the Collector: it ships each Source's Raw records
// from this Machine to the Hub (docs/spec/collector.md).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/tedkulp/agent-history/internal/buildinfo"
	"github.com/tedkulp/agent-history/internal/collector/cache"
	"github.com/tedkulp/agent-history/internal/collector/config"
	"github.com/tedkulp/agent-history/internal/collector/control"
	"github.com/tedkulp/agent-history/internal/collector/drift"
	"github.com/tedkulp/agent-history/internal/collector/exclude"
	"github.com/tedkulp/agent-history/internal/collector/hubclient"
	"github.com/tedkulp/agent-history/internal/collector/logfile"
	"github.com/tedkulp/agent-history/internal/collector/reconcile"
	"github.com/tedkulp/agent-history/internal/collector/runner"
	"github.com/tedkulp/agent-history/internal/collector/service"
	"github.com/tedkulp/agent-history/internal/collector/setup"
	"github.com/tedkulp/agent-history/internal/collector/source"
	"github.com/tedkulp/agent-history/internal/collector/source/claudecode"
	"github.com/tedkulp/agent-history/internal/collector/source/codex"
	"github.com/tedkulp/agent-history/internal/collector/source/ohmypi"
	"github.com/tedkulp/agent-history/internal/collector/state"
	"github.com/tedkulp/agent-history/internal/collector/status"
	"github.com/tedkulp/agent-history/protocol"
)

const usage = `usage: agent-history <command>

commands:
  init --hub <url> [--name <display>] [--offline] [--reset-roots] [--exec <path>]
            set up this Machine: write collector.toml, register with the Hub,
            and install and start the service
  run       ship every Source's records to the Hub and keep it current
  status    show what the Collector is doing
  sync      reconcile every record with the Hub now
  set-name <display>
            change this Machine's display name
  service install [--exec <path>] | uninstall | start | stop | restart | status
            manage the launchd / systemd user service that runs the Collector
  version   print the version
`

// adapters are the Sources this Collector can read.
var adapters = []source.Adapter{claudecode.Adapter{}, codex.Adapter{}, ohmypi.Adapter{}}

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
	case "status":
		err = statusCmd(os.Args[2:])
	case "sync":
		err = syncCmd(os.Args[2:])
	case "set-name":
		err = setName(os.Args[2:])
	case "service":
		err = serviceCmd(os.Args[2:])
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
	rotateLog := logRotation(dir, log)
	rotateLog()
	for _, k := range cfg.Unknown {
		log.Warn("unknown config key ignored", "key", k)
	}
	rc, err := runnerConfig(cfg, home, dir, log)
	if err != nil {
		return err
	}
	rc.OnRescan = rotateLog
	svcPath := newServiceManager(os.Getenv, home).Path()
	if installed, outdated, err := service.Outdated(svcPath); err == nil && installed && outdated {
		log.Warn("service definition outdated, run `agent-history service install`")
	}
	rc.CheckVersion = execVersionCheck(svcPath, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ln, err := control.Listen(socketPath(home))
	if err != nil {
		return fmt.Errorf("opening the control socket: %w", err)
	}
	rc.Control = runner.NewControl()
	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := control.Serve(ctx, ln, socketService{rc.Control, cfg}); err != nil {
			log.Error("control socket", "err", err)
		}
	}()

	log.Info("collector starting", "version", buildinfo.Version, "hub", cfg.HubURL, "state_dir", dir)
	err = runner.Run(ctx, rc)
	stop()
	<-served
	return err
}

// logRotation returns what rotates collector.log on macOS, where launchd
// points stderr at it and never rotates it (collector.md §4.10). It does
// nothing elsewhere, or when stderr is not that file.
func logRotation(dir string, log *slog.Logger) func() {
	if runtime.GOOS != "darwin" {
		return func() {}
	}
	path := filepath.Join(dir, "collector.log")
	r := logfile.Stderr(path)
	if r == nil {
		log.Info("stderr isn't the log file, not rotating it", "path", path)
		return func() {}
	}
	return func() {
		if rotated, err := r.Rotate(); err != nil {
			log.Warn("rotating the log", "path", r.Path, "err", err)
		} else if rotated {
			log.Info("rotated the log", "path", r.Path)
		}
	}
}

// execVersionCheck runs the binary the installed service runs with
// `version`, so the runner can restart into a new version after an upgrade
// replaces it (collector.md §4.9). Without an installed service there is
// nothing to restart it, so there is no check.
func execVersionCheck(svcPath string, log *slog.Logger) func(context.Context) (string, error) {
	exe, err := service.InstalledExec(svcPath)
	if errors.Is(err, fs.ErrNotExist) {
		log.Info("no service installed, not checking for new versions")
		return nil
	}
	if err != nil {
		log.Warn("reading the service definition, not checking for new versions", "err", err)
		return nil
	}
	return func(ctx context.Context) (string, error) { return service.ExecVersion(ctx, exe) }
}

// runnerConfig sets up a Run, or a one-shot sync, from cfg.
func runnerConfig(cfg *config.Config, home, dir string, log *slog.Logger) (runner.Config, error) {
	excludeFilter, err := exclude.New(cfg.Exclude)
	if err != nil {
		return runner.Config{}, err
	}
	var sources []runner.Source
	for _, a := range adapters {
		if sc := cfg.Sources[a.ID()]; sc.IsEnabled() {
			sources = append(sources, runner.Source{Adapter: a, Root: sourceRoot(a, sc, home)})
		}
	}
	hub, err := hubclient.New(cfg.HubURL, cfg.MachineID, buildinfo.Version, &http.Client{Timeout: 5 * time.Minute})
	if err != nil {
		return runner.Config{}, err
	}
	return runner.Config{
		Hub:            hub,
		Info:           setup.MachineInfo(cfg, adapters, hostname(), home, buildinfo.Version),
		Sources:        sources,
		Cache:          cache.Load(filepath.Join(dir, "cache.json"), cfg.HubURL, log),
		Log:            log,
		Exclude:        excludeFilter,
		RescanInterval: cfg.RescanInterval,
	}, nil
}

// sourceRoot is the configured root, or the adapter's default.
func sourceRoot(a source.Adapter, sc config.SourceConfig, home string) string {
	if sc.Root != "" {
		return sc.Root
	}
	return a.DefaultRoot(os.Getenv, home)
}

func socketPath(home string) string {
	return filepath.Join(state.DefaultDir(os.Getenv, home), control.SocketName)
}

// socketService answers the control socket from a running Collector.
type socketService struct {
	ctl *runner.Control
	cfg *config.Config
}

func (b socketService) Status(ctx context.Context) (status.Report, error) {
	rep, err := b.ctl.Status(ctx)
	if err != nil {
		return rep, err
	}
	rep.MachineID, rep.HubURL = b.cfg.MachineID, b.cfg.HubURL
	// The runner knows only the enabled Sources.
	var all []status.Source
	for _, a := range adapters {
		sc := b.cfg.Sources[a.ID()]
		if !sc.IsEnabled() {
			all = append(all, status.Source{ID: a.ID(), Root: sc.Root})
			continue
		}
		for _, s := range rep.Sources {
			if s.ID == a.ID() {
				all = append(all, s)
			}
		}
	}
	rep.Sources = all
	return rep, nil
}

func (b socketService) Sync(ctx context.Context, progress func(string)) (string, error) {
	res, err := b.ctl.Sync(ctx, progress)
	if err != nil {
		return "", err
	}
	return summary(res), nil
}

func (b socketService) SetName(ctx context.Context, name string) error {
	return b.ctl.SetName(ctx, name)
}

func summary(r reconcile.Result) string {
	s := fmt.Sprintf("synced: %d uploaded, %d replaced, %d unchanged, %d failed", r.Uploaded, r.Replaced, r.Unchanged, r.Failed)
	if r.Mismatched > 0 {
		s += fmt.Sprintf(", %d mismatched", r.Mismatched)
	}
	return s
}

func statusCmd(args []string) error {
	if len(args) > 0 {
		return errors.New("usage: agent-history status")
	}
	home, path, err := configPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rep, err := control.NewClient(socketPath(home)).Status(ctx)
	if errors.Is(err, control.ErrNotRunning) {
		rep = localReport(ctx, cfg, home)
	} else if err != nil {
		return fmt.Errorf("asking the service: %w", err)
	}
	if r := rep.Hub.Reachable; r != nil && *r && !rep.Hub.UpgradeRequired {
		rep.Hub.Parsing = hubHealth(ctx, cfg)
	}
	if rep.ServiceInstalled, rep.ServiceOutdated, err = service.Outdated(newServiceManager(os.Getenv, home).Path()); err != nil {
		// Still print the rest of the report.
		fmt.Fprintf(os.Stderr, "agent-history: reading the service definition: %v\n", err)
		rep.ServiceInstalled = true
	}
	status.Format(os.Stdout, rep, time.Now())
	return nil
}

// hubHealth asks the Hub for this Machine's parse health, or returns nil
// when it can't tell; the rest of the report says why.
func hubHealth(ctx context.Context, cfg *config.Config) *protocol.Health {
	hub, err := hubclient.New(cfg.HubURL, cfg.MachineID, buildinfo.Version, &http.Client{Timeout: 5 * time.Second})
	if err != nil {
		return nil
	}
	h, err := hub.Health(ctx)
	if err != nil {
		return nil
	}
	return &h
}

// localReport is what status can tell without a running service: config,
// detected Sources and whether the Hub answers.
func localReport(ctx context.Context, cfg *config.Config, home string) status.Report {
	rep := status.Report{
		Version:     buildinfo.Version,
		MachineID:   cfg.MachineID,
		DisplayName: cfg.DisplayName,
		HubURL:      cfg.HubURL,
	}
	if rep.DisplayName == "" {
		rep.DisplayName = setup.ShortHostname(hostname())
	}
	now := time.Now()
	hub, err := hubclient.New(cfg.HubURL, cfg.MachineID, buildinfo.Version, &http.Client{Timeout: 5 * time.Second})
	if err == nil {
		err = hub.Ping(ctx)
	}
	var se *hubclient.StatusError
	var tooOld *hubclient.TooOldError
	if errors.As(err, &tooOld) {
		rep.Hub.SetUpgradeRequired(tooOld.MinVersion)
	} else {
		reachable := err == nil || errors.As(err, &se) && se.StatusCode < 500
		rep.Hub.Reachable = &reachable
		if err != nil {
			rep.Hub.LastError = status.NewFailure(err, now)
		}
	}
	for _, a := range adapters {
		sc := cfg.Sources[a.ID()]
		s := status.Source{ID: a.ID(), Root: sourceRoot(a, sc, home), Enabled: sc.IsEnabled()}
		if s.Enabled {
			s.Detected = a.Detect(s.Root)
		}
		if s.Detected {
			for _, l := range a.Layouts() {
				recs, err := l.Discover(s.Root)
				if err != nil {
					s.LastError = status.NewFailure(fmt.Errorf("discovering %s records: %w", l.Name(), err), now)
					continue
				}
				s.Layouts = append(s.Layouts, l.Name())
				s.Records += len(recs)
			}
			drift.Scan(a, s.Root).Report(&s)
		}
		rep.Sources = append(rep.Sources, s)
	}
	return rep
}

func syncCmd(args []string) error {
	if len(args) > 0 {
		return errors.New("usage: agent-history sync")
	}
	home, path, err := configPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	progress := func(line string) { fmt.Println(line) }

	// The service owns uploads while it runs, so a sync goes through it.
	done, err := control.NewClient(socketPath(home)).Sync(ctx, progress)
	if !errors.Is(err, control.ErrNotRunning) {
		if err == nil {
			fmt.Println(done)
		}
		return err
	}

	fmt.Println("service not running, syncing directly")
	dir := state.DefaultDir(os.Getenv, home)
	unlock, err := state.Lock(dir)
	if errors.Is(err, state.ErrLocked) {
		return errors.New("another agent-history holds collector.lock: the service is starting or another sync is running; try again")
	} else if err != nil {
		return err
	}
	defer unlock()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	rc, err := runnerConfig(cfg, home, dir, log)
	if err != nil {
		return err
	}
	res, err := runner.Once(ctx, rc, progress)
	if err != nil {
		return err
	}
	fmt.Println(summary(res))
	return nil
}

func initCmd(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	hubURL := fs.String("hub", "", "base URL of the Hub, e.g. http://hub.vpn:8080")
	name := fs.String("name", "", "display name (default: the hostname)")
	offline := fs.Bool("offline", false, "don't register with the Hub")
	resetRoots := fs.Bool("reset-roots", false, "replace configured Source roots with the defaults")
	exe := execFlag(fs)
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
	fmt.Println()

	// The service uses this process's environment, the same one the config
	// path came from, so it reads the config init just wrote.
	m := newServiceManager(os.Getenv, home)
	if err := installService(ctx, m, os.Getenv, home, *exe); err != nil {
		return fmt.Errorf("installing the service: %w", err)
	}
	fmt.Printf("Service  %s  installed and started\n", m.Path())
	if runtime.GOOS == "linux" {
		if err := m.EnableLinger(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "agent-history: %v\nTo keep the service running after logout, run:\n  loginctl enable-linger %s\n", err, m.User)
		}
	}
	return nil
}

func serviceCmd(args []string) error {
	const usage = "usage: agent-history service install [--exec <path>] | uninstall | start | stop | restart | status"
	if len(args) == 0 {
		return errors.New(usage)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	m := newServiceManager(os.Getenv, home)
	sub, rest := args[0], args[1:]
	if sub != "install" && len(rest) > 0 {
		return fmt.Errorf("service %s: unexpected argument %q", sub, rest[0])
	}
	switch sub {
	case "install":
		fs := flag.NewFlagSet("service install", flag.ContinueOnError)
		exe := execFlag(fs)
		if err := fs.Parse(rest); errors.Is(err, flag.ErrHelp) {
			return nil
		} else if err != nil {
			return err
		}
		if fs.NArg() > 0 {
			return fmt.Errorf("service install: unexpected argument %q", fs.Arg(0))
		}
		if err := installService(ctx, m, os.Getenv, home, *exe); err != nil {
			return err
		}
		fmt.Printf("installed and started %s\n", m.Path())
		return nil
	case "uninstall":
		return m.Uninstall(ctx)
	case "start":
		return m.Start(ctx)
	case "stop":
		return m.Stop(ctx)
	case "restart":
		return m.Restart(ctx)
	case "status":
		out, err := m.Status(ctx)
		if err != nil {
			return err
		}
		fmt.Print(strings.TrimRight(out, "\n") + "\n")
		return nil
	default:
		return errors.New(usage)
	}
}

// execFlag defines --exec on fs, plus --shim, its old name, kept working for
// one release. It replaces fs.Usage with one that leaves --shim out.
func execFlag(fs *flag.FlagSet) *string {
	exe := fs.String("exec", "", "path the service runs (default: the mise shim if installed, else this binary)")
	fs.StringVar(exe, "shim", "", "")
	fs.Usage = func() {
		shown := flag.NewFlagSet(fs.Name(), flag.ContinueOnError)
		shown.SetOutput(fs.Output())
		fs.VisitAll(func(f *flag.Flag) {
			if f.Name != "shim" {
				shown.Var(f.Value, f.Name, f.Usage)
			}
		})
		fmt.Fprintf(fs.Output(), "Usage of %s:\n", fs.Name())
		shown.PrintDefaults()
	}
	return exe
}

// installService writes the service definition, running exe (see
// service.DefaultExec when empty), and starts it.
func installService(ctx context.Context, m *service.Manager, getenv func(string) string, home, exe string) error {
	if exe == "" {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("finding this binary: %w; pass --exec <path>", err)
		}
		if exe, err = service.DefaultExec(getenv, home, self); err != nil {
			return err
		}
	} else {
		abs, err := filepath.Abs(exe)
		if err != nil {
			return err
		}
		exe = abs
	}
	return m.Install(ctx, service.Definition{
		Exec:    exe,
		Home:    home,
		LogPath: filepath.Join(state.DefaultDir(getenv, home), "collector.log"),
		Env:     service.DefinitionEnv(getenv),
	})
}

func newServiceManager(getenv func(string) string, home string) *service.Manager {
	name := getenv("USER")
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	return &service.Manager{
		OS:     runtime.GOOS,
		Home:   home,
		UID:    os.Getuid(),
		User:   name,
		Getenv: getenv,
		Run:    service.Exec,
	}
}

func setName(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: agent-history set-name <display>")
	}
	home, path, err := configPath()
	if err != nil {
		return err
	}
	name := args[0]
	if err := setup.SetName(path, name); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch err := control.NewClient(socketPath(home)).SetName(ctx, name); {
	case errors.Is(err, control.ErrNotRunning):
		fmt.Printf("display name set to %q; the service isn't running, so the Hub gets it when the service next starts\n", name)
	case err != nil:
		return fmt.Errorf("display name saved to config, but sending it to the Hub failed: %w", err)
	default:
		fmt.Printf("display name set to %q\n", name)
	}
	return nil
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
