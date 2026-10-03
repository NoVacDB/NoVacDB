// Package server accepts PostgreSQL protocol connections and serves them,
// one goroutine per connection. See docs/design/12-wire-protocol.md.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/executor"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
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
	// DefaultMaxConnections is how many sessions may be open at once.
	// Sessions are goroutines, so the limit is about memory and file
	// descriptors, not processes as in PostgreSQL (whose default is 100).
	DefaultMaxConnections = 1000
)

// ErrServerClosed is returned by Serve after Close or Shutdown.
var ErrServerClosed = errors.New("server: closed")

// Config configures a Server.
type Config struct {
	// DB is the database queries run against. Required.
	DB *executor.DB
	// Logger receives connection events; nil means slog.Default().
	Logger *slog.Logger
	// StartupTimeout bounds the startup handshake; zero means
	// DefaultStartupTimeout.
	StartupTimeout time.Duration
	// ServerVersion is reported as server_version; empty means
	// DefaultServerVersion.
	ServerVersion string
	// MaxConnections bounds the open sessions; zero means
	// DefaultMaxConnections.
	MaxConnections int
	// IdleTimeout ends a session that sends nothing for this long; zero
	// means never, as PostgreSQL's idle_session_timeout.
	IdleTimeout time.Duration
}

// Server serves PostgreSQL protocol connections.
type Server struct {
	cfg Config
	log *slog.Logger
	// ctx is the context queries run with: cancelled by Close.
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	closed    bool
	draining  bool // Shutdown has begun
	listeners map[net.Listener]struct{}
	conns     map[net.Conn]struct{}
	sessions  map[uint32]*liveSession // by process ID
	nextPID   uint32
	wg        sync.WaitGroup

	// afterStartup, if set (by tests, under mu), runs once a session has
	// started.
	afterStartup func()
	// beforeStatement, if set (by tests, under mu), runs with each
	// statement's context just before the statement runs.
	beforeStatement func(ctx context.Context)
}

// liveSession is what the server knows of an open session: its cancel
// key, and how to cancel the statement it is running.
type liveSession struct {
	secret uint32
	mu     sync.Mutex
	cancel context.CancelFunc // of the running statement; nil while idle
}

// New returns a Server.
func New(cfg Config) (*Server, error) {
	if cfg.DB == nil {
		return nil, errors.New("server: Config.DB is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.StartupTimeout == 0 {
		cfg.StartupTimeout = DefaultStartupTimeout
	}
	if cfg.ServerVersion == "" {
		cfg.ServerVersion = DefaultServerVersion
	}
	if cfg.MaxConnections == 0 {
		cfg.MaxConnections = DefaultMaxConnections
	}
	if cfg.MaxConnections < 0 || cfg.IdleTimeout < 0 || cfg.StartupTimeout < 0 {
		return nil, errors.New("server: negative limit in Config")
	}
	var seed [4]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, fmt.Errorf("seeding connection IDs: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		ctx:       ctx,
		cancel:    cancel,
		cfg:       cfg,
		log:       cfg.Logger,
		listeners: map[net.Listener]struct{}{},
		conns:     map[net.Conn]struct{}{},
		sessions:  map[uint32]*liveSession{},
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
			closed := s.closed || s.draining
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

// track registers a connection, unless the server is closed or shutting
// down.
func (s *Server) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.draining {
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

// register admits a new session, unless the server is shutting down or
// the connection limit is reached, and gives it a process ID and cancel
// secret.
func (s *Server) register() (pid uint32, ls *liveSession, err error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, nil, fmt.Errorf("making a cancel key: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining || s.closed {
		return 0, nil, sqlerr.New(sqlerr.CannotConnectNow, "the database system is shutting down")
	}
	if len(s.sessions) >= s.cfg.MaxConnections {
		return 0, nil, sqlerr.New(sqlerr.TooManyConnections, "sorry, too many clients already")
	}
	for {
		pid = s.nextPID
		s.nextPID++
		if s.nextPID >= 1<<31 {
			s.nextPID = 1
		}
		if _, used := s.sessions[pid]; !used {
			break
		}
	}
	ls = &liveSession{secret: binary.BigEndian.Uint32(b[:])}
	s.sessions[pid] = ls
	return pid, ls, nil
}

func (s *Server) unregister(pid uint32) {
	s.mu.Lock()
	delete(s.sessions, pid)
	s.mu.Unlock()
}

// cancelRequest handles a CancelRequest: if the key is a session's, the
// statement it is running, if any, is cancelled. Nothing is answered
// either way, as in PostgreSQL, so a wrong key reveals nothing.
func (s *Server) cancelRequest(pid, secret uint32) {
	s.mu.Lock()
	ls := s.sessions[pid]
	s.mu.Unlock()
	var want [4]byte
	var got [4]byte
	if ls != nil {
		binary.BigEndian.PutUint32(want[:], ls.secret)
	}
	binary.BigEndian.PutUint32(got[:], secret)
	if ls == nil || subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		s.log.Info("cancel request with an unknown key", "pid", pid)
		return
	}
	ls.mu.Lock()
	cancel := ls.cancel
	ls.mu.Unlock()
	if cancel != nil {
		s.log.Info("cancelling a statement", "pid", pid)
		cancel()
	}
}

// isDraining reports whether Shutdown has begun.
func (s *Server) isDraining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

// Shutdown stops the server gracefully (design doc section 2.12): it stops
// accepting connections, ends idle sessions with FATAL 57P01, lets running
// statements finish and their sessions end the same way, and refuses
// sessions still starting with 57P03. When ctx ends first, running
// statements are cancelled and every connection is closed; Shutdown then
// returns ctx's error once all connections have ended.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.draining = true
	var errs []error
	for ln := range s.listeners {
		if err := ln.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	// Wake every connection waiting to read. A session checks for the
	// shutdown after setting its own read deadline, so it cannot miss this.
	for c := range s.conns {
		_ = c.SetReadDeadline(time.Now())
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.log.Warn("shutdown deadline passed: cancelling running statements and closing connections")
		errs = append(errs, ctx.Err())
		_ = s.Close()
		<-done
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	return errors.Join(errs...)
}

// Close stops accepting connections, closes every open connection at once
// (cancelling running statements), and waits for their goroutines to end.
// Shutdown is the graceful way.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.cancel() // running queries stop at their next cancellation point
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
