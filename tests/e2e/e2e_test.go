// Package e2e tests the novacdb command end to end: the real binary,
// started and stopped with signals, driven over TCP by a minimal protocol
// client written here. See docs/design/12-wire-protocol.md section 7.
package e2e

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/vikrant-choudhary06/NoVacDB/internal/pgwire"
)

// binaryPath is the novacdb command, built once for all tests.
var binaryPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "novacdb-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binaryPath = filepath.Join(dir, "novacdb")
	gotool := filepath.Join(runtime.GOROOT(), "bin", "go")
	if out, err := exec.Command(gotool, "build", "-race", "-o", binaryPath, "../../cmd/novacdb").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building novacdb: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// syncBuffer collects a process's log.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// proc is a running novacdb.
type proc struct {
	t    *testing.T
	cmd  *exec.Cmd
	addr string
	log  *syncBuffer
	done chan struct{}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// start runs novacdb on dir and waits until it listens.
func start(t *testing.T, dir string, args ...string) *proc {
	t.Helper()
	port := freePort(t)
	p := &proc{t: t, addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), log: &syncBuffer{}, done: make(chan struct{})}
	p.cmd = exec.Command(binaryPath, append([]string{"--data-dir", dir, "--listen", "127.0.0.1", "--port", strconv.Itoa(port)}, args...)...)
	p.cmd.Stdout, p.cmd.Stderr = p.log, p.log
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill()
		<-p.done
	})
	deadline := time.Now().Add(60 * time.Second)
	for !strings.Contains(p.log.String(), "msg=listening") {
		select {
		case <-p.done:
			t.Fatalf("novacdb exited:\n%s", p.log.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("novacdb did not start:\n%s", p.log.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	return p
}

// stop sends sig and returns the exit code.
func (p *proc) stop(sig syscall.Signal) int {
	p.t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		p.t.Fatal(err)
	}
	select {
	case <-p.done:
	case <-time.After(60 * time.Second):
		p.t.Fatalf("novacdb did not exit after %v:\n%s", sig, p.log.String())
	}
	// The binary is built with the race detector: a race it found is in
	// the log.
	if strings.Contains(p.log.String(), "DATA RACE") || strings.Contains(p.log.String(), "panic") {
		p.t.Fatalf("novacdb reported a problem:\n%s", p.log.String())
	}
	return p.cmd.ProcessState.ExitCode()
}

// client is a minimal PostgreSQL protocol client.
type client struct {
	t   *testing.T
	c   net.Conn
	rd  *pgwire.Reader
	pid uint32
	key uint32
}

// pgError is an ErrorResponse.
type pgError struct{ severity, code, message string }

func (e *pgError) Error() string { return e.severity + " " + e.code + ": " + e.message }

// codeOf returns the SQLSTATE of an ErrorResponse error, "" for others.
func codeOf(err error) string {
	var pe *pgError
	if errors.As(err, &pe) {
		return pe.code
	}
	return ""
}

// result is one statement's result: its tag and rows in text form, NULL
// as "NULL".
type result struct {
	tag  string
	rows [][]string
}

func connect(t *testing.T, addr string) *client {
	t.Helper()
	c, err := dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.c.Close() })
	c.t = t
	return c
}

// dial connects and completes the startup; errors are returned, not
// fatal, for use from worker goroutines.
func dial(addr string) (*client, error) {
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &client{c: nc, rd: pgwire.NewReader(bufio.NewReader(nc))}
	_ = nc.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := nc.Write(pgwire.EncodeStartup(pgwire.ProtocolVersion30, []pgwire.Param{{Name: "user", Value: "e2e"}})); err != nil {
		_ = nc.Close()
		return nil, err
	}
	for {
		typ, body, err := c.rd.ReadMessage()
		if err != nil {
			_ = nc.Close()
			return nil, err
		}
		switch typ {
		case 'K':
			c.pid, c.key = binary.BigEndian.Uint32(body), binary.BigEndian.Uint32(body[4:])
		case 'E':
			_ = nc.Close()
			return nil, parseError(body)
		case 'Z':
			return c, nil
		}
	}
}

func parseError(body []byte) *pgError {
	e := &pgError{}
	for len(body) > 1 {
		end := bytes.IndexByte(body[1:], 0)
		v := string(body[1 : 1+end])
		switch body[0] {
		case 'S':
			e.severity = v
		case 'C':
			e.code = v
		case 'M':
			e.message = v
		}
		body = body[2+end:]
	}
	return e
}

// roundTrip sends messages and collects results up to ReadyForQuery. The
// error is the first ErrorResponse, or a network error.
func (c *client) roundTrip(msgs ...[]byte) ([]result, error) {
	_ = c.c.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := c.c.Write(bytes.Join(msgs, nil)); err != nil {
		return nil, err
	}
	var out []result
	var cur result
	var qerr error
	for {
		typ, body, err := c.rd.ReadMessage()
		if err != nil {
			return out, err
		}
		switch typ {
		case 'D':
			n := int(binary.BigEndian.Uint16(body))
			body = body[2:]
			row := make([]string, n)
			for i := range row {
				l := int32(binary.BigEndian.Uint32(body))
				body = body[4:]
				if l < 0 {
					row[i] = "NULL"
					continue
				}
				row[i] = string(body[:l])
				body = body[l:]
			}
			cur.rows = append(cur.rows, row)
		case 'C':
			cur.tag = string(bytes.TrimSuffix(body, []byte{0}))
			out = append(out, cur)
			cur = result{}
		case 'E':
			e := parseError(body)
			if qerr == nil {
				qerr = e
			}
			if e.severity == "FATAL" {
				return out, e
			}
		case 'Z':
			return out, qerr
		}
	}
}

// query runs SQL with the simple protocol.
func (c *client) query(sql string) ([]result, error) { return c.roundTrip(pgwire.EncodeQuery(sql)) }

// mustQuery runs SQL that must succeed and returns its last result.
func (c *client) mustQuery(sql string) result {
	c.t.Helper()
	rs, err := c.query(sql)
	if err != nil {
		c.t.Fatalf("%s: %v", sql, err)
	}
	return rs[len(rs)-1]
}

// prepare makes a named statement.
func (c *client) prepare(name, sql string) error {
	_, err := c.roundTrip(pgwire.Parse{Name: name, Query: sql}.Encode(), pgwire.EncodeSync())
	return err
}

// execute runs a named statement with text parameters ("NULL" is NULL).
func (c *client) execute(name string, params ...string) (result, error) {
	b := pgwire.Bind{Statement: name}
	for _, p := range params {
		if p == "NULL" {
			b.Params = append(b.Params, nil)
		} else {
			b.Params = append(b.Params, []byte(p))
		}
	}
	rs, err := c.roundTrip(b.Encode(), pgwire.Execute{}.Encode(), pgwire.EncodeSync())
	if err != nil || len(rs) != 1 {
		return result{}, fmt.Errorf("%d results: %w", len(rs), err)
	}
	return rs[0], nil
}

func rowsText(r result) string {
	lines := make([]string, len(r.rows))
	for i, row := range r.rows {
		lines[i] = strings.Join(row, "|")
	}
	return strings.Join(lines, "\n")
}

// TestMVP is the Phase 5 milestone: a client connects; tables are
// created, filled, queried, updated and deleted from, with both protocols;
// a graceful stop ends idle sessions properly; and the data is all there
// after a restart and after a kill.
func TestMVP(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	p := start(t, dir)
	c := connect(t, p.addr)
	c.mustQuery("CREATE TABLE items (id int PRIMARY KEY, name text NOT NULL, price double precision, tags text, added timestamptz DEFAULT now())")
	c.mustQuery("CREATE INDEX ON items (name)")
	if r := c.mustQuery("INSERT INTO items (id, name, price) VALUES (1, 'apple', 0.5), (2, 'pear', 0.75)"); r.tag != "INSERT 0 2" {
		t.Fatal(r.tag)
	}
	if err := c.prepare("add", "INSERT INTO items (id, name, price, tags) VALUES ($1, $2, $3, $4)"); err != nil {
		t.Fatal(err)
	}
	for i := 3; i <= 50; i++ {
		if r, err := c.execute("add", strconv.Itoa(i), fmt.Sprintf("item %d", i), fmt.Sprint(float64(i)/4), "NULL"); err != nil || r.tag != "INSERT 0 1" {
			t.Fatal(r, err)
		}
	}
	if r := c.mustQuery("UPDATE items SET price = price * 2, tags = 'sale' WHERE id <= 10"); r.tag != "UPDATE 10" {
		t.Fatal(r.tag)
	}
	if r := c.mustQuery("DELETE FROM items WHERE id > 40"); r.tag != "DELETE 10" {
		t.Fatal(r.tag)
	}
	// Errors with their codes, and the session goes on.
	if _, err := c.query("INSERT INTO items (id, name) VALUES (1, 'dup')"); codeOf(err) != "23505" {
		t.Fatal(err)
	}
	if _, err := c.execute("add", "99", "NULL", "1", "x"); err == nil || !strings.Contains(err.Error(), "23502") {
		t.Fatal(err)
	}
	if err := c.prepare("get", "SELECT id, name, price, tags FROM items WHERE name = $1"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"apple":   "1|apple|1|sale",
		"item 10": "10|item 10|5|sale",
		"item 40": "40|item 40|10|NULL",
		"item 41": "",
	}
	checkRows := func(c *client) {
		t.Helper()
		if err := c.prepare("check", "SELECT id, name, price, tags FROM items WHERE name = $1"); err != nil {
			t.Fatal(err)
		}
		for name, row := range want {
			r, err := c.execute("check", name)
			if err != nil || rowsText(r) != row {
				t.Fatalf("%s: %q %v", name, rowsText(r), err)
			}
		}
		r := c.mustQuery("SELECT id FROM items ORDER BY id")
		if len(r.rows) != 40 || r.rows[0][0] != "1" || r.rows[39][0] != "40" {
			t.Fatalf("%d rows", len(r.rows))
		}
	}
	checkRows(c)

	// A graceful stop: the idle session is told, the process exits 0.
	if code := p.stop(syscall.SIGTERM); code != 0 {
		t.Fatalf("exit code %d:\n%s", code, p.log.String())
	}
	if _, err := c.query("SELECT 1"); err == nil {
		t.Fatal("the session survived the shutdown")
	}
	for _, line := range []string{"shutting down", "session ended by an error", "code=57P01", "novacdb stopped"} {
		if !strings.Contains(p.log.String(), line) {
			t.Fatalf("log lacks %q:\n%s", line, p.log.String())
		}
	}

	// Restarted: everything is there.
	p = start(t, dir)
	checkRows(connect(t, p.addr))

	// Killed: everything acknowledged is there after recovery.
	c = connect(t, p.addr)
	c.mustQuery("INSERT INTO items (id, name) VALUES (41, 'after restart')")
	want["after restart"] = "41|after restart|NULL|NULL"
	if code := p.stop(syscall.SIGKILL); code != -1 {
		t.Fatalf("exit code %d after SIGKILL", code)
	}
	p = start(t, dir)
	c = connect(t, p.addr)
	if err := c.prepare("get", "SELECT id, name, price, tags FROM items WHERE name = $1"); err != nil {
		t.Fatal(err)
	}
	for name, row := range want {
		r, err := c.execute("get", name)
		if err != nil || rowsText(r) != row {
			t.Fatalf("after the kill, %s: %q %v", name, rowsText(r), err)
		}
	}
	if code := p.stop(syscall.SIGTERM); code != 0 {
		t.Fatalf("exit code %d", code)
	}
}

// TestKillsUnderLoad kills the server while clients write, again and
// again: after each restart every acknowledged row is there, nothing that
// was never sent is, and the indexes agree with the table.
func TestKillsUnderLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	p := start(t, dir)
	c := connect(t, p.addr)
	c.mustQuery("CREATE TABLE w (id int PRIMARY KEY, worker int NOT NULL, note text UNIQUE)")
	acked := map[int]bool{}
	sent := map[int]bool{}
	var mu sync.Mutex
	for round := range 3 {
		var wg sync.WaitGroup
		stopWriting := make(chan struct{})
		for w := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c, err := dial(p.addr)
				if err != nil {
					return
				}
				defer func() { _ = c.c.Close() }()
				prepared := w%2 == 1 && c.prepare("ins", "INSERT INTO w VALUES ($1, $2, $3)") == nil
				for i := 0; ; i++ {
					select {
					case <-stopWriting:
						return
					default:
					}
					id := round*1_000_000 + w*100_000 + i
					mu.Lock()
					sent[id] = true
					mu.Unlock()
					var err error
					if prepared {
						_, err = c.execute("ins", strconv.Itoa(id), strconv.Itoa(w), "n"+strconv.Itoa(id))
					} else {
						_, err = c.query(fmt.Sprintf("INSERT INTO w VALUES (%d, %d, 'n%d')", id, w, id))
					}
					if err != nil {
						return // killed
					}
					mu.Lock()
					acked[id] = true
					mu.Unlock()
				}
			}()
		}
		time.Sleep(300 * time.Millisecond)
		p.stop(syscall.SIGKILL)
		close(stopWriting)
		wg.Wait()
		p = start(t, dir)
		c = connect(t, p.addr)
		// Every row, by a sequential scan, then each again by its primary
		// key and its unique note: the indexes match the table.
		r := c.mustQuery("SELECT id, note FROM w")
		got := map[int]bool{}
		for _, row := range r.rows {
			id, _ := strconv.Atoi(row[0])
			if row[1] != "n"+row[0] || !sent[id] {
				t.Fatalf("round %d: row %v was never sent", round, row)
			}
			got[id] = true
		}
		for id := range acked {
			if !got[id] {
				t.Fatalf("round %d: acknowledged row %d lost", round, id)
			}
		}
		if err := c.prepare("byid", "SELECT note FROM w WHERE id = $1"); err != nil {
			t.Fatal(err)
		}
		if err := c.prepare("bynote", "SELECT id FROM w WHERE note = $1"); err != nil {
			t.Fatal(err)
		}
		for id := range got {
			a, err1 := c.execute("byid", strconv.Itoa(id))
			b, err2 := c.execute("bynote", "n"+strconv.Itoa(id))
			if err1 != nil || err2 != nil || rowsText(a) != "n"+strconv.Itoa(id) || rowsText(b) != strconv.Itoa(id) {
				t.Fatalf("round %d: row %d through the indexes: %q %q %v %v", round, id, rowsText(a), rowsText(b), err1, err2)
			}
		}
		t.Logf("round %d: %d rows, %d acknowledged", round, len(got), len(acked))
		if len(acked) == 0 {
			t.Fatal("no writes acknowledged")
		}
	}
	if code := p.stop(syscall.SIGTERM); code != 0 {
		t.Fatalf("exit code %d", code)
	}
}

func TestConnectionLimitAndCancelKeys(t *testing.T) {
	p := start(t, filepath.Join(t.TempDir(), "data"), "--max-connections", "3")
	clients := []*client{connect(t, p.addr), connect(t, p.addr), connect(t, p.addr)}
	if _, err := dial(p.addr); codeOf(err) != "53300" {
		t.Fatalf("a fourth session: %v", err)
	}
	// Each session has its own cancel key.
	keys := map[[2]uint32]bool{}
	for _, c := range clients {
		keys[[2]uint32{c.pid, c.key}] = true
	}
	if len(keys) != 3 {
		t.Fatal(keys)
	}
	// One ends; another may start.
	_, _ = clients[0].c.Write([]byte{'X', 0, 0, 0, 4})
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := dial(p.addr)
		if err == nil {
			_ = c.c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if code := p.stop(syscall.SIGINT); code != 0 {
		t.Fatalf("exit code %d", code)
	}
}

// TestPsql runs the milestone's steps with the real psql, when it is
// installed.
func TestPsql(t *testing.T) {
	psql, err := exec.LookPath("psql")
	if err != nil {
		t.Skip("psql is not installed")
	}
	dir := filepath.Join(t.TempDir(), "data")
	run := func(p *proc, script string) string {
		t.Helper()
		host, port, _ := net.SplitHostPort(p.addr)
		cmd := exec.Command(psql, "-X", "-v", "ON_ERROR_STOP=1", "-h", host, "-p", port, "-U", "me", "-d", "shop")
		cmd.Stdin = strings.NewReader(script)
		cmd.Env = append(cmd.Environ(), "PGCONNECT_TIMEOUT=10", "PAGER=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return string(out)
	}
	p := start(t, dir)
	out := run(p, `CREATE TABLE notes (id int PRIMARY KEY, body text NOT NULL, at timestamptz);
INSERT INTO notes VALUES (1, 'first', '2024-01-02 03:04:05+00');
INSERT INTO notes VALUES ($1, $2, $3) \bind 2 second 2024-02-03T04:05:06Z \g
UPDATE notes SET body = upper(body) WHERE id = $1 \bind 1 \g
SELECT id, body, at FROM notes ORDER BY id;
DELETE FROM notes WHERE id = 2;
`)
	for _, want := range []string{"CREATE TABLE", "INSERT 0 1", "UPDATE 1", "DELETE 1",
		"  1 | FIRST  | 2024-01-02 03:04:05+00", "  2 | second | 2024-02-03 04:05:06+00", "(2 rows)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	p.stop(syscall.SIGKILL)
	p = start(t, dir)
	if out := run(p, "SELECT * FROM notes;\n"); !strings.Contains(out, "  1 | FIRST | 2024-01-02 03:04:05+00\n(1 row)") {
		t.Fatalf("after the kill:\n%s", out)
	}
	if code := p.stop(syscall.SIGTERM); code != 0 {
		t.Fatalf("exit code %d", code)
	}
}

// procStatus reads a field of /proc/<pid>/status, in its unit (kB for
// memory); ok is false where there is no /proc.
func procStatus(pid int, field string) (int, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, field+":"); ok {
			n, err := strconv.Atoi(strings.Fields(v)[0])
			return n, err == nil
		}
	}
	return 0, false
}

// TestManySessions opens hundreds of sessions at once against the real
// binary. Each is a goroutine, not a process or thread: they all work,
// the server keeps a handful of OS threads, and each costs it kilobytes
// (WORKFLOW.md problem 12).
func TestManySessions(t *testing.T) {
	const sessions = 500
	p := start(t, filepath.Join(t.TempDir(), "data"), "--max-connections", strconv.Itoa(sessions))
	pid := p.cmd.Process.Pid
	baseRSS, haveProc := procStatus(pid, "VmRSS")
	clients := make([]*client, sessions)
	for i := range clients {
		c, err := dial(p.addr)
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		t.Cleanup(func() { _ = c.c.Close() })
		clients[i] = c
	}
	var wg sync.WaitGroup
	errs := make(chan error, sessions)
	for _, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rs, err := c.query("SELECT 1")
			if err == nil && (len(rs) != 1 || rowsText(rs[0]) != "1") {
				err = fmt.Errorf("%v", rs)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if !haveProc {
		t.Log("no /proc: thread and memory figures not checked")
		return
	}
	threads, _ := procStatus(pid, "Threads")
	rss, _ := procStatus(pid, "VmRSS")
	perSession := (rss - baseRSS) * 1024 / sessions
	t.Logf("%d sessions: %d OS threads, %d kB resident (%d kB at start), about %d bytes each", sessions, threads, rss, baseRSS, perSession)
	// Generous bounds, for the race detector's build too (about 120 kB a
	// session here, 17 kB without it): a thread per session would break
	// the first, and a process per session, as PostgreSQL's backends of
	// megabytes each, the second.
	if threads > 64 {
		t.Fatalf("%d OS threads for %d sessions", threads, sessions)
	}
	if perSession > 1<<20 {
		t.Fatalf("%d bytes per session", perSession)
	}
}
