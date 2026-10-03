package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/vikrant-choudhary06/NoVacDB/internal/server"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/executor"
	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

const (
	// defaultPort is 5433 rather than PostgreSQL's 5432 so NoVacDB can run
	// side by side with a local PostgreSQL install.
	defaultPort = 5433
	// defaultDataDir is where the database files live.
	defaultDataDir = "./data"
	// defaultListen is the address to listen on: this machine only, as
	// PostgreSQL's default, because authentication is trust (design doc
	// 12 section 2.3).
	defaultListen = "localhost"
	// maxPort is the largest valid TCP port number.
	maxPort = 65535
	// defaultShutdownTimeout is how long running statements may take to
	// finish once a shutdown begins.
	defaultShutdownTimeout = 30 * time.Second
)

// Errors for invalid flag values.
var (
	ErrInvalidPort           = errors.New("port must be between 1 and 65535")
	ErrInvalidMaxConnections = errors.New("max-connections must be at least 1")
	ErrNegativeDuration      = errors.New("durations must not be negative")
)

// config holds the parsed command-line options.
type config struct {
	dataDir         string
	listen          string
	port            int
	maxConnections  int
	idleTimeout     time.Duration
	shutdownTimeout time.Duration
}

// parseFlags parses args (without the program name) into a config. Usage and
// parse errors are written to out so tests can capture them.
func parseFlags(args []string, out io.Writer) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("novacdb", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&cfg.dataDir, "data-dir", defaultDataDir, "directory that holds the database files")
	fs.StringVar(&cfg.listen, "listen", defaultListen, "address to listen on (empty: every interface)")
	fs.IntVar(&cfg.port, "port", defaultPort, "TCP port to listen on")
	fs.IntVar(&cfg.maxConnections, "max-connections", server.DefaultMaxConnections, "most sessions open at once")
	fs.DurationVar(&cfg.idleTimeout, "idle-timeout", 0, "end sessions idle for this long (0: never)")
	fs.DurationVar(&cfg.shutdownTimeout, "shutdown-timeout", defaultShutdownTimeout,
		"on SIGINT or SIGTERM, how long running statements may take to finish (a second signal stops at once)")
	if err := fs.Parse(args); err != nil {
		return config{}, fmt.Errorf("parsing flags: %w", err)
	}
	switch {
	case cfg.port < 1 || cfg.port > maxPort:
		return config{}, fmt.Errorf("checking --port=%d: %w", cfg.port, ErrInvalidPort)
	case cfg.maxConnections < 1:
		return config{}, fmt.Errorf("checking --max-connections=%d: %w", cfg.maxConnections, ErrInvalidMaxConnections)
	case cfg.idleTimeout < 0 || cfg.shutdownTimeout < 0:
		return config{}, fmt.Errorf("checking --idle-timeout and --shutdown-timeout: %w", ErrNegativeDuration)
	}
	return cfg, nil
}

// run is the testable body of main. It opens the database, serves
// connections until stop is closed (main closes it on SIGINT or SIGTERM),
// shuts down gracefully, unless force is closed too (a second signal),
// then closes the database and returns the process exit code.
func run(args []string, stderr io.Writer, stop, force <-chan struct{}) int {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 2
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))
	logger.Info("novacdb starting", "data_dir", cfg.dataDir, "listen", cfg.listen, "port", cfg.port)
	ctx := context.Background()
	fsys := vfs.OSFS{}
	if err := fsys.MkdirAll(cfg.dataDir); err != nil {
		logger.Error("creating the data directory failed", "err", err)
		return 1
	}
	db, err := executor.Open(ctx, fsys, cfg.dataDir, executor.Options{Logger: logger})
	if err != nil {
		logger.Error("opening the database failed", "err", err)
		return 1
	}
	code := serve(cfg, db, logger, stop, force)
	if err := db.Close(ctx); err != nil {
		logger.Error("closing the database failed", "err", err)
		code = 1
	}
	logger.Info("novacdb stopped")
	return code
}

// serve listens and serves until stop is closed or serving fails, then
// shuts the server down (design doc 12 section 2.12).
func serve(cfg config, db *executor.DB, logger *slog.Logger, stop, force <-chan struct{}) int {
	addr := net.JoinHostPort(cfg.listen, strconv.Itoa(cfg.port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error("listening failed", "addr", addr, "err", err)
		return 1
	}
	if !loopbackOnly(cfg.listen) {
		logger.Warn("listening beyond this machine with trust authentication: anyone who can reach the port can connect", "listen", cfg.listen)
	}
	srv, err := server.New(server.Config{DB: db, Logger: logger, MaxConnections: cfg.maxConnections, IdleTimeout: cfg.idleTimeout})
	if err != nil {
		_ = ln.Close()
		logger.Error("starting the server failed", "err", err)
		return 1
	}
	logger.Info("listening", "addr", ln.Addr().String())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	code := 0
	select {
	case <-stop:
		logger.Info("shutting down: waiting for running statements", "timeout", cfg.shutdownTimeout)
	case err := <-served:
		logger.Error("serving failed", "err", err)
		code = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.shutdownTimeout)
	defer cancel()
	go func() {
		select {
		case <-force:
			logger.Info("stopping at once")
			cancel()
		case <-ctx.Done():
		}
	}()
	if err := srv.Shutdown(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		logger.Warn("closing the listener", "err", err)
	}
	return code
}

// loopbackOnly reports whether host names only this machine: localhost, or
// addresses that all resolve to loopback. An empty host (every interface)
// does not.
func loopbackOnly(host string) bool {
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return false
		}
	}
	return true
}

func main() {
	stop, force := make(chan struct{}), make(chan struct{})
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		close(stop)
		<-sigs
		close(force)
	}()
	os.Exit(run(os.Args[1:], os.Stderr, stop, force))
}
