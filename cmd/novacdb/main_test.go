package main

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vikrant-choudhary06/NoVacDB/internal/pgwire"
)

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    config
		wantErr error
	}{
		{"defaults", nil, config{dataDir: defaultDataDir, listen: defaultListen, port: defaultPort}, nil},
		{"custom values", []string{"--data-dir", "/x", "--listen", "0.0.0.0", "--port", "6000"}, config{dataDir: "/x", listen: "0.0.0.0", port: 6000}, nil},
		{"every interface", []string{"--listen", ""}, config{dataDir: defaultDataDir, listen: "", port: defaultPort}, nil},
		{"minimum port", []string{"--port", "1"}, config{dataDir: defaultDataDir, listen: defaultListen, port: 1}, nil},
		{"maximum port", []string{"--port", "65535"}, config{dataDir: defaultDataDir, listen: defaultListen, port: 65535}, nil},
		{"port zero", []string{"--port", "0"}, config{}, ErrInvalidPort},
		{"port too large", []string{"--port", "65536"}, config{}, ErrInvalidPort},
		{"negative port", []string{"--port", "-1"}, config{}, ErrInvalidPort},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFlags(tc.args, &bytes.Buffer{})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("config = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseFlagsRejectsGarbage(t *testing.T) {
	for _, args := range [][]string{{"--port", "abc"}, {"--nope"}} {
		if _, err := parseFlags(args, &bytes.Buffer{}); err == nil {
			t.Errorf("parseFlags(%v) succeeded, want error", args)
		}
	}
}

func TestRun(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
	}{
		{"help", []string{"-h"}, 0, "data-dir"},
		{"bad port", []string{"--port", "0"}, 2, "port must be"},
		{"bad flag", []string{"--nope"}, 2, "error:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if code := run(tc.args, &out, nil); code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d (output: %s)", code, tc.wantCode, out.String())
			}
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Fatalf("output %q does not contain %q", out.String(), tc.wantOut)
			}
		})
	}
}

// freePort returns a TCP port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// syncBuffer is a bytes.Buffer safe for the server's logger and the test.
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

// handshake connects and completes a startup, returning the message types
// received up to ReadyForQuery.
func handshake(t *testing.T, addr string) string {
	t.Helper()
	var c net.Conn
	var err error
	for range 200 { // the server may still be starting
		if c, err = net.Dial("tcp", addr); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write(pgwire.EncodeStartup(pgwire.ProtocolVersion30, []pgwire.Param{{Name: "user", Value: "me"}})); err != nil {
		t.Fatal(err)
	}
	rd := pgwire.NewReader(bufio.NewReader(c))
	var types []byte
	for {
		typ, _, err := rd.ReadMessage()
		if err != nil {
			t.Fatalf("after %q: %v", types, err)
		}
		types = append(types, typ)
		if typ == 'Z' {
			return string(types)
		}
	}
}

func TestRunServesUntilStopped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	for round := range 2 { // the second run reopens the same data
		port := freePort(t)
		stop := make(chan struct{})
		var out syncBuffer
		done := make(chan int, 1)
		go func() {
			done <- run([]string{"--data-dir", dir, "--listen", "127.0.0.1", "--port", strconv.Itoa(port)}, &out, stop)
		}()
		got := handshake(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if got != "RSSSSSSSSSSSSSKZ" {
			t.Fatalf("round %d: handshake messages %q", round, got)
		}
		close(stop)
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("round %d: exit code %d\n%s", round, code, out.String())
			}
		case <-time.After(30 * time.Second):
			t.Fatal("run did not stop")
		}
		for _, want := range []string{"listening", "session started", "shutting down", "novacdb stopped"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("round %d: log lacks %q:\n%s", round, want, out.String())
			}
		}
		if strings.Contains(out.String(), "level=WARN") {
			t.Fatalf("round %d: unexpected warning:\n%s", round, out.String())
		}
	}
}

func TestRunFailures(t *testing.T) {
	// A port already in use: the database opens, listening fails, exit 1.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	var out syncBuffer
	if code := run([]string{"--data-dir", t.TempDir(), "--listen", "127.0.0.1", "--port", port}, &out, nil); code != 1 || !strings.Contains(out.String(), "listening failed") {
		t.Fatalf("busy port: code %d\n%s", code, out.String())
	}
	// A data directory that cannot be created.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out = syncBuffer{}
	if code := run([]string{"--data-dir", filepath.Join(file, "data")}, &out, nil); code != 1 {
		t.Fatalf("bad data dir: code %d\n%s", code, out.String())
	}
}

func TestListeningBeyondThisMachineWarns(t *testing.T) {
	for host, loopback := range map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "": false, "0.0.0.0": false, "192.0.2.1": false} {
		if got := loopbackOnly(host); got != loopback {
			t.Errorf("loopbackOnly(%q) = %v", host, got)
		}
	}
}
