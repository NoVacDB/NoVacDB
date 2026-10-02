package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vikrant-choudhary06/NoVacDB/internal/pgwire"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/executor"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

// startServer serves on a loopback port until the test ends.
func startServer(t *testing.T, cfg Config) (*Server, string) {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.DB == nil {
		cfg.DB = newDB(t)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- s.Serve(ln) }()
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
		if err := <-served; !errors.Is(err, ErrServerClosed) {
			t.Errorf("Serve returned %v", err)
		}
	})
	return s, ln.Addr().String()
}

// newDB opens an in-memory database, closed when the test ends.
func newDB(t *testing.T) *executor.DB {
	t.Helper()
	m := vfs.NewMemFS(1)
	if err := m.MkdirAll("/db"); err != nil {
		t.Fatal(err)
	}
	if err := m.SyncDir("/"); err != nil {
		t.Fatal(err)
	}
	db, err := executor.Open(context.Background(), m, "/db", executor.Options{
		Frames: 256,
		Now:    func() time.Time { return time.Date(2024, 5, 6, 7, 8, 9, 500000000, time.UTC) },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	return db
}

// client is a raw protocol client.
type client struct {
	t  *testing.T
	c  net.Conn
	br *bufio.Reader
}

func dial(t *testing.T, addr string) *client {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(stallLimit))
	t.Cleanup(func() { _ = c.Close() })
	return &client{t: t, c: c, br: bufio.NewReader(c)}
}

// stallLimit is how long a test client waits for progress: each send
// allows this much more for it and the answers it reads, so a long test
// on one connection is not cut short, and a hang still fails.
const stallLimit = 10 * time.Second

func (cl *client) send(b []byte) {
	cl.t.Helper()
	_ = cl.c.SetDeadline(time.Now().Add(stallLimit))
	if _, err := cl.c.Write(b); err != nil {
		cl.t.Fatal(err)
	}
}

// msg is a received message.
type msg struct {
	typ  byte
	body []byte
}

func (cl *client) read() (msg, error) {
	typ, body, err := pgwire.NewReader(cl.br).ReadMessage()
	return msg{typ, body}, err
}

// readUntilReady reads messages up to and including ReadyForQuery.
func (cl *client) readUntilReady() []msg {
	cl.t.Helper()
	var out []msg
	for {
		m, err := cl.read()
		if err != nil {
			cl.t.Fatalf("after %d messages: %v", len(out), err)
		}
		out = append(out, m)
		if m.typ == 'Z' {
			return out
		}
	}
}

// fatal reads a FATAL ErrorResponse and then the end of the connection,
// and returns the error's fields.
func (cl *client) fatal() map[byte]string {
	cl.t.Helper()
	m, err := cl.read()
	if err != nil || m.typ != 'E' {
		cl.t.Fatalf("want an ErrorResponse, got %q %q %v", m.typ, m.body, err)
	}
	f := fields(m.body)
	if f['S'] != "FATAL" || f['V'] != "FATAL" {
		cl.t.Fatalf("severity %q/%q: %v", f['S'], f['V'], f)
	}
	cl.expectClosed()
	return f
}

func (cl *client) expectClosed() {
	cl.t.Helper()
	if b, err := cl.br.ReadByte(); err == nil {
		cl.t.Fatalf("connection still open: read %q", b)
	} else if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "reset") {
		cl.t.Fatalf("waiting for the server to close: %v", err)
	}
}

func fields(body []byte) map[byte]string {
	f := map[byte]string{}
	for len(body) > 1 {
		code := body[0]
		end := bytes.IndexByte(body[1:], 0)
		f[code] = string(body[1 : 1+end])
		body = body[2+end:]
	}
	return f
}

func startup(params ...string) []byte {
	var ps []pgwire.Param
	for i := 0; i < len(params); i += 2 {
		ps = append(ps, pgwire.Param{Name: params[i], Value: params[i+1]})
	}
	return pgwire.EncodeStartup(pgwire.ProtocolVersion30, ps)
}

func message(typ byte, body string) []byte {
	b := append([]byte{typ}, binary.BigEndian.AppendUint32(nil, uint32(len(body)+4))...)
	return append(b, body...)
}

// parameters returns the ParameterStatus messages, in order, as name=value.
func parameters(ms []msg) []string {
	var out []string
	for _, m := range ms {
		if m.typ == 'S' {
			parts := bytes.Split(bytes.TrimSuffix(m.body, []byte{0}), []byte{0})
			out = append(out, string(parts[0])+"="+string(parts[1]))
		}
	}
	return out
}

func msgTypes(ms []msg) string {
	var b []byte
	for _, m := range ms {
		b = append(b, m.typ)
	}
	return string(b)
}

// syncBuffer is a bytes.Buffer safe for concurrent writes and reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestSessionsAreLogged(t *testing.T) {
	var log syncBuffer
	_, addr := startServer(t, Config{Logger: slog.New(slog.NewTextHandler(&log, nil))})
	cl := dial(t, addr)
	cl.send(startup("user", "bob", "search_path", "x")) // no database: it is the user's name
	cl.readUntilReady()
	cl.send(message('X', ""))
	cl.expectClosed()
	cl = dial(t, addr)
	cl.send(startup("user", "eve", "database", "db", "TimeZone", "Mars/Olympus"))
	cl.fatal()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(log.String(), "connection refused") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	for _, want := range []string{"session started", "user=bob", "database=bob", "application_name=\"\"", "accepted and ignored", "settings=search_path", "session ended", "connection refused", "code=0A000"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, log.String())
		}
	}
}

func TestStartupHandshake(t *testing.T) {
	_, addr := startServer(t, Config{})
	cl := dial(t, addr)
	cl.send(startup("user", "alice", "database", "shop", "application_name", "app"))
	ms := cl.readUntilReady()
	if got := msgTypes(ms); got != "RSSSSSSSSSSSSSKZ" {
		t.Fatalf("messages %q", got)
	}
	if !bytes.Equal(ms[0].body, []byte{0, 0, 0, 0}) {
		t.Fatalf("AuthenticationOk body %v", ms[0].body)
	}
	want := []string{
		"application_name=app", "client_encoding=UTF8", "DateStyle=ISO, MDY",
		"default_transaction_read_only=off", "in_hot_standby=off", "integer_datetimes=on",
		"IntervalStyle=postgres", "is_superuser=on", "server_encoding=UTF8",
		"server_version=" + DefaultServerVersion, "session_authorization=alice",
		"standard_conforming_strings=on", "TimeZone=UTC",
	}
	if got := parameters(ms); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("parameters:\n%s", strings.Join(got, "\n"))
	}
	key := ms[len(ms)-2].body
	if len(key) != 8 || binary.BigEndian.Uint32(key) == 0 || binary.BigEndian.Uint32(key) >= 1<<31 {
		t.Fatalf("BackendKeyData %v", key)
	}
	if !bytes.Equal(ms[len(ms)-1].body, []byte{'I'}) {
		t.Fatalf("ReadyForQuery %q", ms[len(ms)-1].body)
	}
	// Terminate ends the session; the server closes the connection.
	cl.send(message('X', ""))
	cl.expectClosed()
}

func TestEncryptionRequests(t *testing.T) {
	_, addr := startServer(t, Config{})
	readN := func(cl *client) {
		t.Helper()
		if b, err := cl.br.ReadByte(); err != nil || b != 'N' {
			t.Fatalf("want N, got %q %v", b, err)
		}
	}
	// SSL, then startup.
	cl := dial(t, addr)
	cl.send(pgwire.EncodeRequest(pgwire.CodeSSLRequest))
	readN(cl)
	cl.send(startup("user", "u"))
	if got := msgTypes(cl.readUntilReady()); got[0] != 'R' {
		t.Fatalf("%q", got)
	}
	// GSS, then SSL, then startup.
	cl = dial(t, addr)
	cl.send(pgwire.EncodeRequest(pgwire.CodeGSSENCRequest))
	readN(cl)
	cl.send(pgwire.EncodeRequest(pgwire.CodeSSLRequest))
	readN(cl)
	cl.send(startup("user", "u"))
	cl.readUntilReady()
	// The same request twice.
	cl = dial(t, addr)
	cl.send(pgwire.EncodeRequest(pgwire.CodeSSLRequest))
	readN(cl)
	cl.send(pgwire.EncodeRequest(pgwire.CodeSSLRequest))
	if f := cl.fatal(); f['C'] != sqlerr.ProtocolViolation {
		t.Fatal(f)
	}
	// Data sent before the answer: refused, without an N.
	cl = dial(t, addr)
	cl.send(append(pgwire.EncodeRequest(pgwire.CodeSSLRequest), startup("user", "u")...))
	if f := cl.fatal(); f['C'] != sqlerr.ProtocolViolation || !strings.Contains(f['M'], "unencrypted data") {
		t.Fatal(f)
	}
	// An encryption request with a body.
	cl = dial(t, addr)
	bad := pgwire.EncodeRequest(pgwire.CodeSSLRequest)
	binary.BigEndian.PutUint32(bad, 12)
	cl.send(append(bad, 0, 0, 0, 0))
	if f := cl.fatal(); f['C'] != sqlerr.ProtocolViolation {
		t.Fatal(f)
	}
}

func TestProtocolVersions(t *testing.T) {
	_, addr := startServer(t, Config{})
	for _, v := range []struct {
		code uint32
		msg  string
	}{
		{2 << 16, "unsupported frontend protocol 2.0: server supports 3.0 to 3.0"},
		{4<<16 | 1, "unsupported frontend protocol 4.1: server supports 3.0 to 3.0"},
		{1234<<16 | 5677, "unsupported frontend protocol 1234.5677"}, // next to CancelRequest's 1234.5678
	} {
		cl := dial(t, addr)
		cl.send(pgwire.EncodeStartup(v.code, []pgwire.Param{{Name: "user", Value: "u"}}))
		if f := cl.fatal(); f['C'] != sqlerr.FeatureNotSupported || !strings.HasPrefix(f['M'], v.msg) {
			t.Errorf("%#x: %v", v.code, f)
		}
	}
	// 3.2 with protocol options: negotiated down to 3.0, options listed.
	cl := dial(t, addr)
	cl.send(pgwire.EncodeStartup(3<<16|2, []pgwire.Param{{Name: "user", Value: "u"}, {Name: "_pq_.a", Value: "1"}, {Name: "_pq_.b", Value: ""}}))
	ms := cl.readUntilReady()
	want := append([]byte{0, 0, 0, 0, 0, 0, 0, 2}, "_pq_.a\x00_pq_.b\x00"...)
	if ms[0].typ != 'v' || !bytes.Equal(ms[0].body, want) || ms[1].typ != 'R' {
		t.Fatalf("%q %q", msgTypes(ms), ms[0].body)
	}
	// 3.0 with an option: listed too. 3.0 alone: no negotiation.
	cl = dial(t, addr)
	cl.send(startup("user", "u", "_pq_.x", "y"))
	if ms := cl.readUntilReady(); ms[0].typ != 'v' {
		t.Fatalf("%q", msgTypes(ms))
	}
	cl = dial(t, addr)
	cl.send(pgwire.EncodeStartup(3<<16|1, []pgwire.Param{{Name: "user", Value: "u"}}))
	if ms := cl.readUntilReady(); ms[0].typ != 'v' || !bytes.Equal(ms[0].body, make([]byte, 8)) {
		t.Fatalf("%q %q", msgTypes(ms), ms[0].body)
	}
}

func TestStartupParameters(t *testing.T) {
	_, addr := startServer(t, Config{})
	long := strings.Repeat("é", 40) // 80 bytes
	cases := []struct {
		params []string
		code   string // "" if accepted
		want   []string
	}{
		{[]string{"user", "u"}, "", []string{"application_name=", "client_encoding=UTF8", "session_authorization=u"}},
		{[]string{"user", "u", "client_encoding", "utf-8"}, "", []string{"client_encoding=UTF8"}},
		{[]string{"user", "u", "client_encoding", "Unicode"}, "", []string{"client_encoding=UTF8"}},
		{[]string{"user", "u", "client_encoding", "SQL_ASCII"}, "", []string{"client_encoding=SQL_ASCII"}},
		{[]string{"user", "u", "client_encoding", "LATIN1"}, sqlerr.InvalidParameterValue, nil},
		{[]string{"user", "u", "application_name", long}, "", []string{"application_name=" + strings.Repeat("é", 31)}},
		{[]string{"user", "u", "application_name", "a", "application_name", "b"}, "", []string{"application_name=b"}},
		{[]string{"user", "u", "DateStyle", "ISO, DMY"}, "", []string{"DateStyle=ISO, MDY"}},
		{[]string{"user", "u", "datestyle", "iso"}, "", nil},
		{[]string{"user", "u", "DateStyle", "Postgres, MDY"}, sqlerr.InvalidParameterValue, nil},
		{[]string{"user", "u", "DateStyle", ""}, sqlerr.InvalidParameterValue, nil},
		{[]string{"user", "u", "TimeZone", "Etc/UTC"}, "", []string{"TimeZone=UTC"}},
		{[]string{"user", "u", "timezone", "gmt"}, "", nil},
		{[]string{"user", "u", "TimeZone", "Europe/Berlin"}, sqlerr.FeatureNotSupported, nil},
		{[]string{"user", "u", "extra_float_digits", "3"}, "", nil},
		{[]string{"user", "u", "extra_float_digits", "-15"}, "", nil},
		{[]string{"user", "u", "extra_float_digits", "4"}, sqlerr.InvalidParameterValue, nil},
		{[]string{"user", "u", "extra_float_digits", "-16"}, sqlerr.InvalidParameterValue, nil},
		{[]string{"user", "u", "extra_float_digits", "two"}, sqlerr.InvalidParameterValue, nil},
		{[]string{"user", "u", "search_path", "public", "statement_timeout", "0", "lock_timeout", "5s"}, "", nil},
		{[]string{"user", "u", "replication", "database"}, sqlerr.FeatureNotSupported, nil},
		{[]string{"user", "u", "work_mem", "4MB"}, sqlerr.UndefinedObject, nil},
		{[]string{"user", "u", "options", "-c DateStyle=ISO --extra-float-digits=2 -cTimeZone=UTC"}, "", nil},
		{[]string{"user", "u", "options", `-c application_name=a\ b`}, "", []string{"application_name=a b"}},
		{[]string{"user", "u", "options", "-c TimeZone=PST8PDT"}, sqlerr.FeatureNotSupported, nil},
		{[]string{"user", "u", "options", "-c work_mem=1"}, sqlerr.UndefinedObject, nil},
		{[]string{"user", "u", "options", "-x"}, sqlerr.SyntaxError, nil},
		{[]string{"user", "u", "options", "-c nameonly"}, sqlerr.SyntaxError, nil},
		{[]string{"user", "u", "options", "-c"}, sqlerr.SyntaxError, nil},
		{[]string{"user", "u", "options", "-c TimeZone=UTC -c"}, sqlerr.SyntaxError, nil},
		{[]string{"user", "u", "options", "-c =x"}, sqlerr.SyntaxError, nil},
		{[]string{"user", "u", "options", "--=x"}, sqlerr.SyntaxError, nil},
		{[]string{"database", "d"}, sqlerr.InvalidAuthorization, nil},
		{[]string{"user", ""}, sqlerr.InvalidAuthorization, nil},
		{nil, sqlerr.InvalidAuthorization, nil},
	}
	for _, c := range cases {
		cl := dial(t, addr)
		cl.send(startup(c.params...))
		if c.code != "" {
			if f := cl.fatal(); f['C'] != c.code {
				t.Errorf("%q: %v, want %s", c.params, f, c.code)
			}
			continue
		}
		got := strings.Join(parameters(cl.readUntilReady()), "\n")
		for _, w := range c.want {
			if !strings.Contains(got+"\n", w+"\n") {
				t.Errorf("%q: parameters lack %q:\n%s", c.params, w, got)
			}
		}
	}
}

func TestSplitOptions(t *testing.T) {
	for in, want := range map[string][]string{
		"":                       nil,
		"  ":                     nil,
		"-c a=b":                 {"-c", "a=b"},
		"  -c   a=b  --x=y ":     {"-c", "a=b", "--x=y"},
		`-c a=b\ c`:              {"-c", "a=b c"},
		`-c a=\\`:                {"-c", `a=\`},
		"-c\ta=b\n--c=d":         {"-c", "a=b", "--c=d"},
		`\ lead`:                 {" lead"},
		`trailing\`:              {"trailing"},
		"-capplication_name=x y": {"-capplication_name=x", "y"},
	} {
		if got := splitOptions(in); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("splitOptions(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAfterStartup(t *testing.T) {
	_, addr := startServer(t, Config{})
	cl := dial(t, addr)
	cl.send(startup("user", "u"))
	cl.readUntilReady()
	// A query, and the session goes on.
	cl.send(message('Q', "SELECT 1\x00"))
	if ms := cl.readUntilReady(); msgTypes(ms) != "TDCZ" {
		t.Fatalf("%q", msgTypes(ms))
	}
	// A malformed Query.
	cl.send(message('Q', "SELECT 1"))
	if f := cl.fatal(); f['C'] != sqlerr.ProtocolViolation {
		t.Fatal(f)
	}
	// An unknown message type.
	cl = dial(t, addr)
	cl.send(startup("user", "u"))
	cl.readUntilReady()
	cl.send(message('p', "x"))
	if f := cl.fatal(); f['C'] != sqlerr.ProtocolViolation || f['M'] != "invalid frontend message type 112" {
		t.Fatal(f)
	}
	// A message over the size limit is refused before its body arrives.
	cl = dial(t, addr)
	cl.send(startup("user", "u"))
	cl.readUntilReady()
	cl.send(append([]byte{'Q'}, binary.BigEndian.AppendUint32(nil, pgwire.MaxMessageSize+1)...))
	if f := cl.fatal(); f['C'] != sqlerr.ProtocolViolation {
		t.Fatal(f)
	}
	// Pipelined: startup and a query in one write.
	cl = dial(t, addr)
	cl.send(append(startup("user", "u"), message('Q', "SELECT 1\x00")...))
	cl.readUntilReady()
	if ms := cl.readUntilReady(); msgTypes(ms) != "TDCZ" {
		t.Fatalf("%q", msgTypes(ms))
	}
}

func TestStartupErrors(t *testing.T) {
	_, addr := startServer(t, Config{})
	length := func(n uint32) []byte { return binary.BigEndian.AppendUint32(nil, n) }
	for name, in := range map[string][]byte{
		"length too small": append(length(4), 0, 3, 0, 0),
		"length too large": length(pgwire.MaxStartupPacket + 1),
		"no terminator":    func() []byte { b := startup("user", "u"); return b[:len(b)-1] }(),
	} {
		cl := dial(t, addr)
		if name == "no terminator" {
			// Fix the length so the packet is complete but malformed.
			binary.BigEndian.PutUint32(in, uint32(len(in)))
		}
		cl.send(in)
		if f := cl.fatal(); f['C'] != sqlerr.ProtocolViolation {
			t.Errorf("%s: %v", name, f)
		}
	}
	// A cancel request before 5.4: closed without a reply.
	cl := dial(t, addr)
	cancel := binary.BigEndian.AppendUint32(nil, 16)
	cancel = binary.BigEndian.AppendUint32(cancel, pgwire.CodeCancelRequest)
	cl.send(append(cancel, 0, 0, 0, 1, 0, 0, 0, 2))
	cl.expectClosed()
}

func TestStartupTimeout(t *testing.T) {
	_, addr := startServer(t, Config{StartupTimeout: 200 * time.Millisecond})
	for _, prefix := range [][]byte{nil, startup("user", "u")[:5], pgwire.EncodeRequest(pgwire.CodeSSLRequest)} {
		cl := dial(t, addr)
		cl.send(prefix)
		start := time.Now()
		if len(prefix) == 8 {
			if b, _ := cl.br.ReadByte(); b != 'N' {
				t.Fatal("no N")
			}
		}
		cl.expectClosed()
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("closed after %v", d)
		}
	}
	// A session that started has no such timeout.
	cl := dial(t, addr)
	cl.send(startup("user", "u"))
	cl.readUntilReady()
	time.Sleep(400 * time.Millisecond)
	cl.send(message('Q', "SELECT 1\x00"))
	cl.readUntilReady()
}

func TestAbruptDisconnects(t *testing.T) {
	// A client that disappears after any byte of the handshake costs
	// nothing: no session is left registered and the server goes on.
	s, addr := startServer(t, Config{})
	conversation := append(pgwire.EncodeRequest(pgwire.CodeSSLRequest), startup("user", "u", "database", "d")...)
	conversation = append(conversation, message('Q', "SELECT 1\x00")...)
	for cut := range len(conversation) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		part := conversation[:cut]
		if cut > 8 {
			// Wait for the N, as a real client would, before the rest.
			_, _ = c.Write(part[:8])
			_, _ = io.ReadFull(c, make([]byte, 1))
			part = part[8:]
		}
		_, _ = c.Write(part)
		_ = c.Close()
	}
	// Every session ended: wait for the server to notice them all.
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		conns, keys := len(s.conns), len(s.sessions)
		s.mu.Unlock()
		if conns == 0 && keys == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connections and %d sessions still registered", conns, keys)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cl := dial(t, addr)
	cl.send(startup("user", "u"))
	cl.readUntilReady()
}

func TestConcurrentHandshakes(t *testing.T) {
	_, addr := startServer(t, Config{})
	const n = 100
	keys := make(chan [2]uint32, n)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("tcp", addr)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(20 * time.Second))
			if _, err := c.Write(startup("user", "u")); err != nil {
				errs <- err
				return
			}
			rd := pgwire.NewReader(bufio.NewReader(c))
			for {
				typ, body, err := rd.ReadMessage()
				if err != nil {
					errs <- err
					return
				}
				if typ == 'K' {
					keys <- [2]uint32{binary.BigEndian.Uint32(body), binary.BigEndian.Uint32(body[4:])}
				}
				if typ == 'Z' {
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	close(keys)
	for err := range errs {
		t.Fatal(err)
	}
	pids, secrets := map[uint32]bool{}, map[uint32]bool{}
	for k := range keys {
		pids[k[0]], secrets[k[1]] = true, true
	}
	if len(pids) != n || len(secrets) < n-2 {
		t.Fatalf("%d distinct process IDs and %d distinct secrets for %d sessions", len(pids), len(secrets), n)
	}
}

func TestPanicIsContained(t *testing.T) {
	s, addr := startServer(t, Config{})
	s.mu.Lock()
	s.afterStartup = func() { panic("boom") }
	s.mu.Unlock()
	cl := dial(t, addr)
	cl.send(startup("user", "u"))
	cl.readUntilReady()
	if f := cl.fatal(); f['C'] != sqlerr.InternalError {
		t.Fatal(f)
	}
	s.mu.Lock()
	s.afterStartup = nil
	s.mu.Unlock()
	cl = dial(t, addr)
	cl.send(startup("user", "u"))
	cl.readUntilReady()
}

func TestCloseEndsSessions(t *testing.T) {
	s, err := New(Config{DB: newDB(t), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- s.Serve(ln) }()
	cl := dial(t, ln.Addr().String())
	cl.send(startup("user", "u"))
	cl.readUntilReady()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-served; !errors.Is(err, ErrServerClosed) {
		t.Fatal(err)
	}
	cl.expectClosed()
	if _, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		t.Fatal("still accepting after Close")
	}
	// Serving after Close fails at once.
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Serve(ln2); !errors.Is(err, ErrServerClosed) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal("second Close:", err)
	}
}

func TestPsqlConnects(t *testing.T) {
	// The real client, when it is installed.
	psql, err := exec.LookPath("psql")
	if err != nil {
		t.Skip("psql is not installed")
	}
	_, addr := startServer(t, Config{})
	host, port, _ := net.SplitHostPort(addr)
	cmd := exec.Command(psql, "-X", "-h", host, "-p", port, "-U", "alice", "-d", "shop", "-c", `\conninfo`)
	cmd.Env = append(cmd.Environ(), "PGCONNECT_TIMEOUT=10", "PGSSLMODE=prefer")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(string(out), `database "shop" as user "alice"`) {
		t.Fatalf("%s", out)
	}
}

func TestProcessIDsWrapAndSkipLiveOnes(t *testing.T) {
	s, err := New(Config{DB: newDB(t)})
	if err != nil {
		t.Fatal(err)
	}
	s.nextPID = 1<<31 - 1
	s.sessions[1] = &liveSession{secret: 7} // a live session holds 1
	a, _, err := s.register()
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.register()
	if err != nil {
		t.Fatal(err)
	}
	if a != 1<<31-1 || b != 2 {
		t.Fatalf("process IDs %d then %d", a, b)
	}
	s.unregister(a)
	if _, ok := s.sessions[a]; ok {
		t.Fatal("unregister kept the key")
	}
}

// flakyListener fails its first Accept with a temporary error.
type flakyListener struct {
	net.Listener
	failed bool
	mu     sync.Mutex
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "too many open files" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func (l *flakyListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	failed := l.failed
	l.failed = true
	l.mu.Unlock()
	if !failed {
		return nil, timeoutErr{}
	}
	return l.Listener.Accept()
}

func TestAcceptRetriesTemporaryErrors(t *testing.T) {
	s, err := New(Config{DB: newDB(t), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- s.Serve(&flakyListener{Listener: ln}) }()
	cl := dial(t, ln.Addr().String())
	cl.send(startup("user", "u"))
	cl.readUntilReady()
	_ = s.Close()
	if err := <-served; !errors.Is(err, ErrServerClosed) {
		t.Fatal(err)
	}
	// A permanent error ends Serve with it.
	s2, _ := New(Config{DB: newDB(t), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln2.Close()
	if err := s2.Serve(ln2); err == nil || errors.Is(err, ErrServerClosed) {
		t.Fatalf("Serve on a closed listener: %v", err)
	}
}
