package server

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vikrant-choudhary06/NoVacDB/internal/pgwire"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

// openKeyed opens a session and returns its cancel key.
func openKeyed(t *testing.T, addr string) (cl *client, pid, secret uint32) {
	t.Helper()
	cl = dial(t, addr)
	cl.send(startup("user", "u"))
	for _, m := range cl.readUntilReady() {
		if m.typ == 'K' {
			pid, secret = binary.BigEndian.Uint32(m.body), binary.BigEndian.Uint32(m.body[4:])
		}
	}
	if pid == 0 {
		t.Fatal("no BackendKeyData")
	}
	return cl, pid, secret
}

// sendCancel sends a CancelRequest on a new connection, which the server
// closes without a reply.
func sendCancel(t *testing.T, addr string, pid, secret uint32) {
	t.Helper()
	cl := dial(t, addr)
	b := binary.BigEndian.AppendUint32(nil, 16)
	b = binary.BigEndian.AppendUint32(b, pgwire.CodeCancelRequest)
	b = binary.BigEndian.AppendUint32(b, pid)
	cl.send(binary.BigEndian.AppendUint32(b, secret))
	cl.expectClosed()
}

// blockStatements makes every statement wait, after it is registered for
// cancelling, until its context ends or release is closed; running gets
// one value per statement that starts waiting.
func blockStatements(s *Server) (running chan struct{}, release chan struct{}) {
	running, release = make(chan struct{}, 16), make(chan struct{})
	s.mu.Lock()
	s.beforeStatement = func(ctx context.Context) {
		running <- struct{}{}
		select {
		case <-ctx.Done():
		case <-release:
		}
	}
	s.mu.Unlock()
	return running, release
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func sessionCount(s *Server) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

func TestConnectionLimit(t *testing.T) {
	s, addr := startServer(t, Config{MaxConnections: 2})
	a := openSession(t, addr)
	openSession(t, addr)
	// The third is refused as PostgreSQL refuses it.
	c := dial(t, addr)
	c.send(startup("user", "u"))
	if f := c.fatal(); f['C'] != sqlerr.TooManyConnections || f['M'] != "sorry, too many clients already" {
		t.Fatal(f)
	}
	// Cancel requests are not sessions: they still get through.
	sendCancel(t, addr, 1, 1)
	// Once one ends, there is room again.
	a.send(message('X', ""))
	a.expectClosed()
	waitFor(t, "the session to end", func() bool { return sessionCount(s) == 1 })
	if ms := openSession(t, addr).query("SELECT 1"); msgTypes(ms) != "TDCZ" {
		t.Fatalf("%q", msgTypes(ms))
	}
}

func TestCancelRequest(t *testing.T) {
	s, addr := startServer(t, Config{})
	cl, pid, secret := openKeyed(t, addr)
	cl.query("CREATE TABLE t (id int)")
	running, release := blockStatements(s)
	defer close(release)

	// A cancel with the right key ends the running statement with 57014;
	// the session goes on.
	cl.send(message('Q', "SELECT 1\x00"))
	<-running
	sendCancel(t, addr, pid, secret)
	ms := cl.readUntilReady()
	if msgTypes(ms) != "EZ" || fields(ms[0].body)['C'] != sqlerr.QueryCanceled ||
		fields(ms[0].body)['M'] != "canceling statement due to user request" {
		t.Fatalf("%q %v", msgTypes(ms), fields(ms[0].body))
	}

	// A wrong secret, or another session's process ID, cancels nothing.
	other, otherPID, _ := openKeyed(t, addr)
	defer func() { _ = other }()
	cl.send(message('Q', "SELECT 2\x00"))
	<-running
	sendCancel(t, addr, pid, secret^1)
	sendCancel(t, addr, otherPID, secret)
	sendCancel(t, addr, 0, 0)
	time.Sleep(50 * time.Millisecond)
	release <- struct{}{} // not cancelled: the statement still waits; let it go
	if ms := cl.readUntilReady(); msgTypes(ms) != "TDCZ" {
		t.Fatalf("%q", msgTypes(ms))
	}

	// A cancel while idle does nothing, and is not remembered for the
	// next statement.
	s.mu.Lock()
	s.beforeStatement = nil
	s.mu.Unlock()
	sendCancel(t, addr, pid, secret)
	if ms := cl.query("SELECT 3"); msgTypes(ms) != "TDCZ" {
		t.Fatalf("%q", msgTypes(ms))
	}

	// The extended protocol: a cancelled Execute is an error that skips
	// to Sync.
	cl.sends(parseMsg("", "INSERT INTO t VALUES ($1)"), pgwire.EncodeSync())
	cl.readUntilReady()
	running, release2 := blockStatements(s)
	defer close(release2)
	cl.sends(bindText("", "", nil, "1"), execMsg("", 0), bindText("", "", nil, "2"), execMsg("", 0), pgwire.EncodeSync())
	<-running
	sendCancel(t, addr, pid, secret)
	if ms := cl.readUntilReady(); msgTypes(ms) != "2EZ" || errCode(t, ms[1]) != sqlerr.QueryCanceled {
		t.Fatalf("%q", msgTypes(ms))
	}
	s.mu.Lock()
	s.beforeStatement = nil
	s.mu.Unlock()
	// Nothing was inserted: the statement was cancelled before it began.
	if ms := cl.query("SELECT id FROM t"); msgTypes(ms) != "TCZ" {
		t.Fatalf("%q", msgTypes(ms))
	}
	// Malformed cancel requests are ignored too.
	c := dial(t, addr)
	c.send(binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 12), pgwire.CodeCancelRequest))
	c.send([]byte{1, 2, 3, 4})
	c.expectClosed()
}

func TestShutdown(t *testing.T) {
	var logs syncBuffer
	s, addr := startServer(t, Config{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	idle := openSession(t, addr)
	busy := openSession(t, addr)
	busy.query("CREATE TABLE t (id int)")
	running, release := blockStatements(s)
	busy.send(message('Q', "INSERT INTO t VALUES (1); SELECT id FROM t\x00"))
	<-running
	// A connection still in its startup, once the server has accepted it
	// (until then it is the kernel's, and closing the listener resets it).
	starting := dial(t, addr)
	waitFor(t, "the connection to be accepted", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.conns) == 3
	})

	shut := make(chan error, 1)
	go func() { shut <- s.Shutdown(context.Background()) }()
	// The idle session ends at once, with PostgreSQL's message.
	if f := idle.fatal(); f['C'] != sqlerr.AdminShutdown || f['M'] != "terminating connection due to administrator command" {
		t.Fatal(f)
	}
	// So does the one starting.
	if f := starting.fatal(); f['C'] != sqlerr.CannotConnectNow {
		t.Fatal(f)
	}
	// No new connections.
	waitFor(t, "the listener to close", func() bool {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = c.Close()
		}
		return err != nil
	})
	// The running statement finishes, the rest of its query too, and its
	// results arrive before the session ends.
	select {
	case err := <-shut:
		t.Fatalf("Shutdown returned %v with a statement running", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	ms := busy.readN(4)
	if msgTypes(ms) != "CTDC" || tag(ms[0]) != "INSERT 0 1" || dataRow(t, ms[2].body)[0] != "1" {
		t.Fatalf("%q", msgTypes(ms))
	}
	if m, err := busy.read(); err != nil || m.typ != 'Z' {
		t.Fatalf("%q %v", m.typ, err)
	}
	if f := busy.fatal(); f['C'] != sqlerr.AdminShutdown {
		t.Fatal(f)
	}
	if err := <-shut; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatal(logs.String())
	}
	// Shutdown and Close after Shutdown do nothing more.
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownDeadline(t *testing.T) {
	s, addr := startServer(t, Config{})
	busy := openSession(t, addr)
	running, release := blockStatements(s)
	defer close(release)
	busy.send(message('Q', "SELECT 1\x00"))
	<-running
	// The statement outlives the deadline: it is cancelled and the
	// connection closed.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	// Whatever the client got, the connection is over.
	_ = busy.c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(busy.br); err != nil && !strings.Contains(err.Error(), "reset") {
		t.Fatal(err)
	}
}

func TestShutdownWithManySessions(t *testing.T) {
	// Sessions busy with real statements, idle sessions and new
	// connections, all while Shutdown runs: everyone ends, nothing hangs.
	s, addr := startServer(t, Config{})
	setup := openSession(t, addr)
	setup.query("CREATE TABLE t (id int PRIMARY KEY)")
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("tcp", addr)
			if err != nil {
				return // the listener may already be closed
			}
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(20 * time.Second))
			if _, err := c.Write(startup("user", "u")); err != nil {
				return
			}
			rd := pgwire.NewReader(bufio.NewReader(c))
			for i := 0; ; i++ {
				if i > 0 || w%2 == 0 {
					if _, err := c.Write(pgwire.EncodeQuery("INSERT INTO t VALUES (" + strconv.Itoa(w*100000+i) + ")")); err != nil {
						return
					}
				}
				for {
					typ, body, err := rd.ReadMessage()
					if err != nil {
						return // closed after its FATAL
					}
					if typ == 'E' && fields(body)['S'] == "FATAL" {
						if code := fields(body)['C']; code != sqlerr.AdminShutdown && code != sqlerr.CannotConnectNow {
							t.Errorf("FATAL %v", fields(body))
						}
						return
					}
					if typ == 'Z' {
						break
					}
				}
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}

func TestIdleTimeout(t *testing.T) {
	_, addr := startServer(t, Config{IdleTimeout: 200 * time.Millisecond})
	cl := openSession(t, addr)
	// Activity keeps it open past the timeout...
	for range 6 {
		time.Sleep(60 * time.Millisecond)
		if ms := cl.query("SELECT 1"); msgTypes(ms) != "TDCZ" {
			t.Fatalf("%q", msgTypes(ms))
		}
	}
	// ... and silence ends it, as PostgreSQL's idle_session_timeout does.
	start := time.Now()
	if f := cl.fatal(); f['C'] != sqlerr.IdleSessionTimeout || f['M'] != "terminating connection due to idle-session timeout" {
		t.Fatal(f)
	}
	if waited := time.Since(start); waited < 150*time.Millisecond {
		t.Fatalf("ended after %v", waited)
	}
}

func TestNewRejectsNegativeLimits(t *testing.T) {
	for _, cfg := range []Config{{MaxConnections: -1}, {IdleTimeout: -1}, {StartupTimeout: -1}} {
		cfg.DB = newDB(t)
		if _, err := New(cfg); err == nil {
			t.Errorf("%+v accepted", cfg)
		}
	}
}

func TestRegisterRefusesWhileDraining(t *testing.T) {
	// A startup packet read just as Shutdown begins: the session is
	// refused with 57P03, not started.
	s, err := New(Config{DB: newDB(t), MaxConnections: 5})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.draining = true
	s.mu.Unlock()
	if _, _, err := s.register(); sqlerr.Code(err) != sqlerr.CannotConnectNow {
		t.Fatal(err)
	}
	if sessionCount(s) != 0 {
		t.Fatal("registered")
	}
}

func TestIdleTimeoutIsOnlyForWaiting(t *testing.T) {
	// A message that takes longer than the idle timeout to arrive is not
	// idleness: the session reads it whole and answers.
	_, addr := startServer(t, Config{IdleTimeout: 150 * time.Millisecond})
	cl := openSession(t, addr)
	msg := message('Q', "SELECT 'slow'\x00")
	for i, b := range msg {
		cl.send([]byte{b})
		if i < 3 {
			time.Sleep(100 * time.Millisecond) // 300ms in all, twice the timeout
		}
	}
	if ms := cl.readUntilReady(); msgTypes(ms) != "TDCZ" || dataRow(t, ms[1].body)[0] != "slow" {
		t.Fatalf("%q", msgTypes(ms))
	}
	// Then idle: ended.
	if f := cl.fatal(); f['C'] != sqlerr.IdleSessionTimeout {
		t.Fatal(f)
	}
}
