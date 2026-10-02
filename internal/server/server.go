// Package server accepts PostgreSQL protocol connections and serves them,
// one goroutine per connection. See docs/design/12-wire-protocol.md.
package server

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/executor"
)

// Defaults for Config.
const (
	// DefaultServerVersion is what server_version reports: PostgreSQL 16's
	// number first, because drivers decide what SQL to send from it
	// (design doc section 2.3), then what the server really is.
	DefaultServerVersion = "16.0 (NoVacDB 0.5)"
	// DefaultStartupTimeout is how long a client may take from connecting
	// to finishing the startup handshake (PostgreSQL's
	// authentication_timeout).
	DefaultStartupTimeout = 60 * time.Second
)

// ErrServerClosed is returned by Serve after Close.
var ErrServerClosed = errors.New("server: closed")

// Config configures a Server.
type Config struct {
	// DB is the database queries run against (from Step 5.2).
	DB *executor.DB
	// Logger receives connection events; nil means slog.Default().
	Logger *slog.Logger
	// StartupTimeout bounds the startup handshake; zero means
	// DefaultStartupTimeout.
	StartupTimeout time.Duration
	// ServerVersion is reported as server_version; empty means
	// DefaultServerVersion.
	ServerVersion string
}

// Server serves PostgreSQL protocol connections.
type Server struct {
	cfg Config
	log *slog.Logger

	mu        sync.Mutex
	closed    bool
	listeners map[net.Listener]struct{}
	conns     map[net.Conn]struct{}
	keys      map[uint32]uint32 // process ID -> cancel secret, of open sessions
	nextPID   uint32
	wg        sync.WaitGroup

	// afterStartup, if set (by tests, under mu), runs once a session has
	// started.
	afterStartup func()
}

// New returns a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.StartupTimeout == 0 {
		cfg.StartupTimeout = DefaultStartupTimeout
	}
	if cfg.ServerVersion == "" {
		cfg.ServerVersion = DefaultServerVersion
	}
	var seed [4]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, fmt.Errorf("seeding connection IDs: %w", err)
	}
	return &Server{
		cfg:       cfg,
		log:       cfg.Logger,
		listeners: map[net.Listener]struct{}{},
		conns:     map[net.Conn]struct{}{},
		keys:      map[uint32]uint32{},
		// Process IDs are connection numbers: positive 31-bit values,
		// starting anywhere so they do not look like a fresh server's.
		nextPID: binary.BigEndian.Uint32(seed[:])%(1<<30) + 1,
	}, nil
}

// Serve accepts connections on ln until Close, serving each in its own
// goroutine. It returns ErrServerClosed after Close, or the listener's
// error.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ln.Close()
		return ErrServerClosed
	}
	s.listeners[ln] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.listeners, ln)
		s.mu.Unlock()
	}()
	backoff := 5 * time.Millisecond
	for {
		c, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return ErrServerClosed
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				// Out of file descriptors and the like: wait and retry.
				s.log.Warn("accepting a connection failed; retrying", "err", err)
				time.Sleep(backoff)
				backoff = min(2*backoff, time.Second)
				continue
			}
			return fmt.Errorf("accepting connections: %w", err)
		}
		backoff = 5 * time.Millisecond
		if !s.track(c) {
			_ = c.Close()
			return ErrServerClosed
		}
		go s.handle(c)
	}
}

// track registers a connection, unless the server is closed.
func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[c] = struct{}{}
	s.wg.Add(1)
	return true
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	s.wg.Done()
}

// register gives a new session a process ID and cancel secret.
func (s *Server) register() (pid, secret uint32, err error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, 0, fmt.Errorf("making a cancel key: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		pid = s.nextPID
		s.nextPID++
		if s.nextPID >= 1<<31 {
			s.nextPID = 1
		}
		if _, used := s.keys[pid]; !used {
			break
		}
	}
	secret = binary.BigEndian.Uint32(b[:])
	s.keys[pid] = secret
	return pid, secret, nil
}

func (s *Server) unregister(pid uint32) {
	s.mu.Lock()
	delete(s.keys, pid)
	s.mu.Unlock()
}

// Close stops accepting connections, closes every open connection, and
// waits for their goroutines to end. (Graceful shutdown, which lets
// running statements finish, is Step 5.4.)
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	var errs []error
	for ln := range s.listeners {
		if err := ln.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return errors.Join(errs...)
}
