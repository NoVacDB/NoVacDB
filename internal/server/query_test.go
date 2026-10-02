package server

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

// openSession dials and completes a startup.
func openSession(t *testing.T, addr string) *client {
	t.Helper()
	cl := dial(t, addr)
	cl.send(startup("user", "u"))
	cl.readUntilReady()
	return cl
}

// query sends one Query message and returns the messages up to
// ReadyForQuery.
func (cl *client) query(sql string) []msg {
	cl.t.Helper()
	cl.send(message('Q', sql+"\x00"))
	ms := cl.readUntilReady()
	if last := ms[len(ms)-1]; !bytes.Equal(last.body, []byte{'I'}) {
		cl.t.Fatalf("ReadyForQuery status %q", last.body)
	}
	return ms
}

// column is a decoded RowDescription field.
type column struct {
	name   string
	table  uint32
	attr   uint16
	oid    uint32
	size   int16
	typmod int32
	format uint16
}

func rowDescription(t *testing.T, body []byte) []column {
	t.Helper()
	n := int(binary.BigEndian.Uint16(body))
	body = body[2:]
	out := make([]column, n)
	for i := range out {
		end := bytes.IndexByte(body, 0)
		c := column{name: string(body[:end])}
		body = body[end+1:]
		c.table = binary.BigEndian.Uint32(body)
		c.attr = binary.BigEndian.Uint16(body[4:])
		c.oid = binary.BigEndian.Uint32(body[6:])
		c.size = int16(binary.BigEndian.Uint16(body[10:]))
		c.typmod = int32(binary.BigEndian.Uint32(body[12:]))
		c.format = binary.BigEndian.Uint16(body[16:])
		body = body[18:]
		out[i] = c
	}
	if len(body) != 0 {
		t.Fatalf("%d bytes after the fields", len(body))
	}
	return out
}

// dataRow decodes a DataRow; NULL is "NULL".
func dataRow(t *testing.T, body []byte) []string {
	t.Helper()
	n := int(binary.BigEndian.Uint16(body))
	body = body[2:]
	out := make([]string, n)
	for i := range out {
		l := int32(binary.BigEndian.Uint32(body))
		body = body[4:]
		if l < 0 {
			out[i] = "NULL"
			continue
		}
		out[i] = string(body[:l])
		body = body[l:]
	}
	if len(body) != 0 {
		t.Fatalf("%d bytes after the values", len(body))
	}
	return out
}

func tag(m msg) string { return string(bytes.TrimSuffix(m.body, []byte{0})) }

func TestQueryResults(t *testing.T) {
	_, addr := startServer(t, Config{})
	cl := openSession(t, addr)
	ms := cl.query(`CREATE TABLE t (i int, b bigint, f double precision, s text, o boolean, ts timestamptz);
		INSERT INTO t VALUES (1, 9223372036854775807, 0.1, 'it''s', true, '2024-01-02 03:04:05.5+00'), (NULL, NULL, NULL, '', NULL, NULL)`)
	if msgTypes(ms) != "CCZ" || tag(ms[0]) != "CREATE TABLE" || tag(ms[1]) != "INSERT 0 2" {
		t.Fatalf("%q", msgTypes(ms))
	}
	ms = cl.query("SELECT i, b, f, s, o, ts, i + 1 AS next, upper(s) FROM t ORDER BY i")
	if msgTypes(ms) != "TDDCZ" || tag(ms[3]) != "SELECT 2" {
		t.Fatalf("%q", msgTypes(ms))
	}
	cols := rowDescription(t, ms[0].body)
	want := []column{
		{"i", 0, 0, 23, 4, -1, 0}, {"b", 0, 0, 20, 8, -1, 0}, {"f", 0, 0, 701, 8, -1, 0},
		{"s", 0, 0, 25, -1, -1, 0}, {"o", 0, 0, 16, 1, -1, 0}, {"ts", 0, 0, 1184, 8, -1, 0},
		{"next", 0, 0, 23, 4, -1, 0}, {"upper", 0, 0, 25, -1, -1, 0},
	}
	if fmt.Sprint(cols) != fmt.Sprint(want) {
		t.Fatalf("columns\n%v\nwant\n%v", cols, want)
	}
	if got := strings.Join(dataRow(t, ms[1].body), "|"); got != "1|9223372036854775807|0.1|it's|t|2024-01-02 03:04:05.5+00|2|IT'S" {
		t.Fatalf("row %q", got)
	}
	// NULLs are length -1, the empty string length 0.
	if got := dataRow(t, ms[2].body); strings.Join(got, "|") != "NULL|NULL|NULL||NULL|NULL|NULL|" {
		t.Fatalf("row %q", got)
	}
	// An empty string in the first row is still not NULL.
	if ms := cl.query("SELECT '', NULL"); msgTypes(ms) != "TDCZ" || strings.Join(dataRow(t, ms[1].body), "|") != "|NULL" {
		t.Fatalf("%q %q", msgTypes(ms), dataRow(t, ms[1].body))
	}
	// A SELECT with no rows still describes its columns.
	if ms := cl.query("SELECT s FROM t WHERE false"); msgTypes(ms) != "TCZ" || tag(ms[1]) != "SELECT 0" {
		t.Fatalf("%q", msgTypes(ms))
	}
	// An empty select list, as PostgreSQL allows: a RowDescription of no
	// columns and one empty DataRow per row.
	ms = cl.query("SELECT FROM t")
	if msgTypes(ms) != "TDDCZ" || tag(ms[3]) != "SELECT 2" {
		t.Fatalf("%q", msgTypes(ms))
	}
	if cols := rowDescription(t, ms[0].body); len(cols) != 0 {
		t.Fatalf("columns %v", cols)
	}
	if got := dataRow(t, ms[1].body); len(got) != 0 {
		t.Fatalf("row %q", got)
	}
	// Tags of the other statements.
	for sql, want := range map[string]string{
		"UPDATE t SET i = 5 WHERE i = 1": "UPDATE 1", "DELETE FROM t WHERE i IS NULL": "DELETE 1",
		"CREATE INDEX ti ON t (i)": "CREATE INDEX", "DROP INDEX ti": "DROP INDEX",
	} {
		if ms := cl.query(sql); msgTypes(ms) != "CZ" || tag(ms[0]) != want {
			t.Errorf("%s: %q %q", sql, msgTypes(ms), tag(ms[0]))
		}
	}
}

func TestQueryErrorsAndNotices(t *testing.T) {
	_, addr := startServer(t, Config{})
	cl := openSession(t, addr)
	// An error stops the string; the statements before it stay done.
	ms := cl.query("CREATE TABLE t (a int PRIMARY KEY); INSERT INTO t VALUES (1); INSERT INTO t VALUES (1); INSERT INTO t VALUES (2)")
	if msgTypes(ms) != "CCEZ" {
		t.Fatalf("%q", msgTypes(ms))
	}
	f := fields(ms[2].body)
	if f['S'] != "ERROR" || f['V'] != "ERROR" || f['C'] != sqlerr.UniqueViolation ||
		f['M'] != `duplicate key value violates unique constraint "t_pkey"` || f['D'] != "Key (a)=(1) already exists." {
		t.Fatalf("%v", f)
	}
	if ms := cl.query("SELECT a FROM t ORDER BY a"); msgTypes(ms) != "TDCZ" {
		t.Fatalf("the statement after the error ran: %q", msgTypes(ms))
	}
	// Positions count characters in the whole string.
	ms = cl.query("SELECT 'é'; SELECT nope FROM t")
	if msgTypes(ms) != "TDCEZ" || fields(ms[3].body)['P'] != "20" || fields(ms[3].body)['C'] != sqlerr.UndefinedColumn {
		t.Fatalf("%q %v", msgTypes(ms), fields(ms[3].body))
	}
	if ms := cl.query("SELECT 1 +"); msgTypes(ms) != "EZ" || fields(ms[0].body)['C'] != sqlerr.SyntaxError || fields(ms[0].body)['P'] == "" {
		t.Fatalf("%q %v", msgTypes(ms), fields(ms[0].body))
	}
	if ms := cl.query("SELECT 1 + true"); fields(ms[0].body)['H'] == "" {
		t.Fatalf("no hint: %v", fields(ms[0].body))
	}
	// Notices come before their statement's CommandComplete.
	ms = cl.query("CREATE TABLE IF NOT EXISTS t (x int); DROP TABLE IF EXISTS nope")
	if msgTypes(ms) != "NCNCZ" {
		t.Fatalf("%q", msgTypes(ms))
	}
	n1, n2 := fields(ms[0].body), fields(ms[2].body)
	if n1['S'] != "NOTICE" || n1['C'] != "42P07" || n1['M'] != `relation "t" already exists, skipping` || n2['C'] != "00000" {
		t.Fatalf("%v %v", n1, n2)
	}
	// Empty strings.
	for _, sql := range []string{"", " ", ";", " ; ;", "-- just a comment", "/* c */"} {
		if ms := cl.query(sql); msgTypes(ms) != "IZ" {
			t.Errorf("%q: %q", sql, msgTypes(ms))
		}
	}
	// A query too long for the parser.
	if ms := cl.query("SELECT " + strings.Repeat(" ", 1<<20)); fields(ms[0].body)['C'] != sqlerr.ProgramLimitExceeded {
		t.Fatalf("%v", fields(ms[0].body))
	}
}

func TestOtherMessages(t *testing.T) {
	_, addr := startServer(t, Config{})
	cl := openSession(t, addr)
	// A function call.
	cl.send(message('F', "\x00\x00\x00\x01"))
	if ms := cl.readUntilReady(); msgTypes(ms) != "EZ" || fields(ms[0].body)['C'] != sqlerr.FeatureNotSupported {
		t.Fatalf("%q", msgTypes(ms))
	}
	// Copy messages outside a COPY are ignored.
	cl.send(message('d', "data"))
	cl.send(message('c', ""))
	cl.send(message('f', "why\x00"))
	if ms := cl.query("SELECT 1"); msgTypes(ms) != "TDCZ" {
		t.Fatalf("%q", msgTypes(ms))
	}
	// Sync alone.
	cl.send(message('S', ""))
	if ms := cl.readUntilReady(); msgTypes(ms) != "Z" {
		t.Fatalf("%q", msgTypes(ms))
	}
}

func TestLargeResults(t *testing.T) {
	_, addr := startServer(t, Config{})
	cl := openSession(t, addr)
	cl.query("CREATE TABLE big (id int PRIMARY KEY, pad text)")
	const rows = 20000
	for start := 0; start < rows; start += 1000 {
		var vals []string
		for i := start; i < start+1000; i++ {
			vals = append(vals, fmt.Sprintf("(%d, '%0100d')", i, i))
		}
		if ms := cl.query("INSERT INTO big VALUES " + strings.Join(vals, ", ")); tag(ms[0]) != "INSERT 0 1000" {
			t.Fatalf("%q", tag(ms[0]))
		}
	}
	// About 2.5 MB of rows: sent in many writes, all of them in order.
	ms := cl.query("SELECT id, pad FROM big ORDER BY id")
	if len(ms) != rows+3 || tag(ms[len(ms)-2]) != fmt.Sprintf("SELECT %d", rows) {
		t.Fatalf("%d messages", len(ms))
	}
	for i, m := range ms[1 : rows+1] {
		if got := dataRow(t, m.body); got[0] != fmt.Sprint(i) || got[1] != fmt.Sprintf("%0100d", i) {
			t.Fatalf("row %d: %q", i, got)
		}
	}
	// Clients that hang up in the middle of the result: the server's
	// writes fail, its sessions end (Close in the cleanup waits for them),
	// and the other session is unaffected.
	for range 4 {
		quitter := openSession(t, addr)
		quitter.send(message('Q', "SELECT id, pad FROM big\x00"))
		if m, err := quitter.read(); err != nil || m.typ != 'T' {
			t.Fatalf("%q %v", m.typ, err)
		}
		_ = quitter.c.(*net.TCPConn).SetLinger(0)
		_ = quitter.c.Close()
	}
	if ms := cl.query(fmt.Sprintf("SELECT pad FROM big WHERE id = %d", rows-1)); msgTypes(ms) != "TDCZ" || dataRow(t, ms[1].body)[0] != fmt.Sprintf("%0100d", rows-1) {
		t.Fatalf("%q", msgTypes(ms))
	}
}

func TestConcurrentSessions(t *testing.T) {
	_, addr := startServer(t, Config{})
	setup := openSession(t, addr)
	setup.query("CREATE TABLE acct (id int PRIMARY KEY, bal bigint NOT NULL)")
	for i := range 10 {
		setup.query(fmt.Sprintf("INSERT INTO acct VALUES (%d, 100)", i))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cl := openSession(t, addr)
			for i := range 100 {
				if w%2 == 0 {
					from, to := (w+i)%10, (w+i+3)%10
					ms := cl.query(fmt.Sprintf("UPDATE acct SET bal = bal + CASE id WHEN %d THEN -1 ELSE 1 END WHERE id IN (%d, %d)", from, from, to))
					if tag(ms[0]) != "UPDATE 2" {
						errs <- fmt.Errorf("transfer: %q", tag(ms[0]))
						return
					}
					continue
				}
				ms := cl.query("SELECT bal FROM acct")
				total := 0
				for _, m := range ms {
					if m.typ == 'D' {
						var v int
						_, _ = fmt.Sscan(dataRow(t, m.body)[0], &v)
						total += v
					}
				}
				if total != 1000 {
					errs <- fmt.Errorf("a reader saw a total of %d", total)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestPipelinedQueries(t *testing.T) {
	// Several queries in one write: answered in order, each with its own
	// ReadyForQuery.
	_, addr := startServer(t, Config{})
	cl := openSession(t, addr)
	var b []byte
	for i := range 50 {
		b = append(b, message('Q', fmt.Sprintf("SELECT %d\x00", i))...)
	}
	cl.send(b)
	for i := range 50 {
		ms := cl.readUntilReady()
		if msgTypes(ms) != "TDCZ" || dataRow(t, ms[1].body)[0] != fmt.Sprint(i) {
			t.Fatalf("query %d: %q", i, msgTypes(ms))
		}
	}
}

func TestNewRequiresDB(t *testing.T) {
	if s, err := New(Config{}); err == nil || s != nil {
		t.Fatalf("New without a DB: %v, %v", s, err)
	}
}

func TestPsqlQueries(t *testing.T) {
	// The real client, when it is installed: aligned output, errors with a
	// caret under the position, notices, and several commands in one run.
	psql, err := exec.LookPath("psql")
	if err != nil {
		t.Skip("psql is not installed")
	}
	_, addr := startServer(t, Config{})
	host, port, _ := net.SplitHostPort(addr)
	cmd := exec.Command(psql, "-X", "-h", host, "-p", port, "-U", "alice", "-d", "shop",
		"-c", "CREATE TABLE t (id int PRIMARY KEY, name text, ok bool)",
		"-c", "CREATE TABLE IF NOT EXISTS t (id int)",
		"-c", "INSERT INTO t VALUES (1, 'one', true), (2, NULL, false)",
		"-c", "SELECT id, name, ok FROM t ORDER BY id",
		"-c", "SELECT nope FROM t")
	cmd.Env = append(cmd.Environ(), "PGCONNECT_TIMEOUT=10", "PGSSLMODE=prefer", "PAGER=", "PGCLIENTENCODING=UTF8")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("want exit status 1 from the failing command, got %v: %s", err, out)
	}
	for _, want := range []string{
		"CREATE TABLE\n",
		`NOTICE:  relation "t" already exists, skipping`,
		"INSERT 0 2\n",
		" id | name | ok \n----+------+----\n  1 | one  | t\n  2 |      | f\n(2 rows)\n",
		`ERROR:  column "nope" does not exist`,
		"LINE 1: SELECT nope FROM t\n               ^",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
