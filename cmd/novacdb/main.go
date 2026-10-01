package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
)

const (
	// defaultPort is 5433 rather than PostgreSQL's 5432 so NoVacDB can run
	// side by side with a local PostgreSQL install.
	defaultPort = 5433
	// defaultDataDir is where database files will live once storage exists.
	defaultDataDir = "./data"
	// maxPort is the largest valid TCP port number.
	maxPort = 65535
)

// ErrInvalidPort is returned when --port is outside the valid TCP range.
var ErrInvalidPort = errors.New("port must be between 1 and 65535")

// config holds the parsed command-line options.
type config struct {
	dataDir string
	port    int
}

// parseFlags parses args (without the program name) into a config. Usage and
// parse errors are written to out so tests can capture them.
func parseFlags(args []string, out io.Writer) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("novacdb", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&cfg.dataDir, "data-dir", defaultDataDir, "directory that holds the database files")
	fs.IntVar(&cfg.port, "port", defaultPort, "TCP port to listen on")
	if err := fs.Parse(args); err != nil {
		return config{}, fmt.Errorf("parsing flags: %w", err)
	}
	if cfg.port < 1 || cfg.port > maxPort {
		return config{}, fmt.Errorf("checking --port=%d: %w", cfg.port, ErrInvalidPort)
	}
	return cfg, nil
}

// run is the testable body of main. It returns the process exit code.
func run(args []string, stderr io.Writer) int {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 2
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))
	logger.Info("novacdb starting", "data_dir", cfg.dataDir, "port", cfg.port)
	// Nothing to serve yet: storage, SQL and the wire protocol come in later
	// steps. Exit cleanly rather than pretending to listen.
	logger.Info("server not implemented yet; exiting")
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}
