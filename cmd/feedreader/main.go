// Command feedreader runs the single-user feed reader: the HTTP UI and the
// polling scheduler, sharing one SQLite database.
//
// Usage:
//
//	feedreader [-config <file>] [-version]
//
// Configuration comes from FR_* environment variables, optionally preceded by
// KEY=VALUE lines in the -config file; the environment wins. See .env.example.
//
// SIGINT or SIGTERM stops the process gracefully: in-flight HTTP requests and
// fetches are drained (each bounded by a timeout), then the database is
// closed. A second signal during the drain terminates the process
// immediately.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/sched"
	"github.com/giacomomicoli/feedreader/internal/store"
	"github.com/giacomomicoli/feedreader/internal/web"
)

// HTTP server limits. They bound slow or abusive clients; the UI itself only
// sends small forms.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 64 << 10

	// requestTimeout is the deadline of every handler's context. Adding a
	// source fetches remote pages; the deadline makes such a handler give
	// up and answer with an inline error before writeTimeout silently cuts
	// the connection.
	requestTimeout = writeTimeout - 10*time.Second

	// shutdownTimeout bounds the graceful drain of in-flight HTTP requests.
	shutdownTimeout = 15 * time.Second

	// schedStopTimeout bounds the wait for the scheduler once it has been
	// told to stop. It is a budget of its own, not what the HTTP drain left
	// over: a slow request can use all of shutdownTimeout, and the
	// scheduler only needs a moment because its fetches are cancelled.
	schedStopTimeout = 5 * time.Second

	// dataDirPerm is used when the data directory has to be created.
	dataDirPerm = 0o750
)

// exitUsage is the exit status for command-line usage errors.
const exitUsage = 2

// usageError reports bad command-line arguments. The flag package has
// already printed the problem and the usage text when it is returned.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

// options are the parsed command-line flags.
type options struct {
	configFile  string
	showVersion bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop() // restore the default action: a second signal kills the process
	}()
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err == nil {
		return
	}
	var ue usageError
	if errors.As(err, &ue) {
		os.Exit(exitUsage)
	}
	fmt.Fprintf(os.Stderr, "%s: %v\n", config.AppName, err)
	os.Exit(1)
}

// run parses args, loads the configuration and serves until ctx is done.
// It returns nil after a clean shutdown.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	opts, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if opts.showVersion {
		_, err := fmt.Fprintf(stdout, "%s %s\n", config.AppName, config.Version)
		return err
	}
	cfg, err := config.Load(opts.configFile)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)
	return serve(ctx, cfg, log)
}

// parseFlags parses the command line. -h yields flag.ErrHelp; anything
// invalid yields a usageError after printing the usage text to stderr.
func parseFlags(args []string, stderr io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet(config.AppName, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.configFile, "config", "", "read `file` (KEY=VALUE lines) before the FR_* environment variables")
	fs.BoolVar(&opts.showVersion, "version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s [-config <file>] [-version]\n\n", config.AppName)
		fmt.Fprintf(fs.Output(), "Self-hosted single-user feed reader. Settings come from FR_* environment\n")
		fmt.Fprintf(fs.Output(), "variables (see .env.example); the environment overrides the config file.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, usageError{err}
	}
	if fs.NArg() > 0 {
		err := fmt.Errorf("unexpected argument %q", fs.Arg(0))
		fmt.Fprintln(fs.Output(), err)
		fs.Usage()
		return opts, usageError{err}
	}
	return opts, nil
}

// serve wires store, fetcher, scheduler, resolver and web UI, then runs the
// scheduler and the HTTP server until ctx is done or the server fails.
func serve(ctx context.Context, cfg config.Config, log *slog.Logger) (err error) {
	if err := os.MkdirAll(cfg.DataDir, dataDirPerm); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	st, err := store.OpenSQLite(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close database: %w", cerr))
		}
	}()

	fetcher := fetch.New(cfg)
	scheduler := sched.New(st, fetcher, cfg, log.With("component", "sched"))
	handler, err := web.New(web.Deps{
		Config:       cfg,
		Store:        st,
		Sched:        scheduler,
		Resolver:     resolve.New(scheduler.ResolveFetcher()),
		Log:          log.With("component", "web"),
		AllowedHosts: cfg.AllowedHosts,
	})
	if err != nil {
		return fmt.Errorf("build web UI: %w", err)
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := newHTTPServer(handler, log)

	// The scheduler outlives ctx until the HTTP server has drained, so
	// requests still in flight can trigger refreshes.
	schedCtx, stopSched := context.WithCancel(context.WithoutCancel(ctx))
	defer stopSched()
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		if err := scheduler.Run(schedCtx); err != nil {
			log.Error("scheduler failed", "err", err)
		}
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	log.Info("feedreader started", "version", config.Version, "listen", "http://"+ln.Addr().String(),
		"db", cfg.DBPath())

	select {
	case <-ctx.Done():
		log.Info("shutting down", "reason", context.Cause(ctx))
	case err = <-serveErr:
		err = fmt.Errorf("http server: %w", err)
		log.Error("http server failed", "err", err)
	}
	err = errors.Join(err, shutdown(srv, stopSched, schedDone, shutdownTimeout, schedStopTimeout))
	if err == nil {
		log.Info("feedreader stopped")
	}
	return err
}

// newHTTPServer returns the server with its timeouts and header limit.
func newHTTPServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           withRequestTimeout(h, requestTimeout),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(log.With("component", "http").Handler(), slog.LevelWarn),
	}
}

// withRequestTimeout gives every request context a deadline of d.
func withRequestTimeout(h http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// shutdown drains the HTTP server for up to httpBudget (then closes it),
// and only then stops the scheduler, so requests still in flight can trigger
// refreshes. It waits up to schedBudget, counted from the stop, for the
// scheduler's in-flight fetches to finish.
func shutdown(srv *http.Server, stopSched context.CancelFunc, schedDone <-chan struct{}, httpBudget, schedBudget time.Duration) error {
	var errs []error
	ctx, cancel := context.WithTimeout(context.Background(), httpBudget)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("http shutdown: %w", err))
		_ = srv.Close()
	}

	stopSched()
	timer := time.NewTimer(schedBudget)
	defer timer.Stop()
	select {
	case <-schedDone:
	case <-timer.C:
		errs = append(errs, fmt.Errorf("scheduler did not stop within %s", schedBudget))
	}
	return errors.Join(errs...)
}
