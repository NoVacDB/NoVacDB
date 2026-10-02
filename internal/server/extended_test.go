package server

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vikrant-choudhary06/NoVacDB/internal/pgwire"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/types"
)

// sends writes several messages at once, as drivers pipeline them.
func (cl *client) sends(msgs ...[]byte) {
	cl.t.Helper()
	cl.send(bytes.Join(msgs, nil))
}

// readN reads n messages.
func (cl *client) readN(n int) []msg {
	cl.t.Helper()
	out := make([]msg, n)
	for i := range out {
		m, err := cl.read()
		if err != nil {
			cl.t.Fatalf("message %d: %v", i, err)
		}
		out[i] = m
	}
	return out
}

func parseMsg(name, query string, oids ...uint32) []byte {
	return pgwire.Parse{Name: name, Query: query, ParamTypes: oids}.Encode()
}

// bindText binds text parameters ("NULL" is NULL) with result formats.
func bindText(portal, stmt string, results []int16, params ...string) []byte {
	b := pgwire.Bind{Portal: portal, Statement: stmt, ResultFormats: results}
	for _, p := range params {
		if p == "NULL" {
			b.Params = append(b.Params, nil)
		} else {
			b.Params = append(b.Params, []byte(p))
		}
	}
	return b.Encode()
}

func execMsg(portal string, max int32) []byte {
	return pgwire.Execute{Portal: portal, MaxRows: max}.Encode()
}

// paramOIDs decodes a ParameterDescription.
func paramOIDs(body []byte) []uint32 {
	n := int(binary.BigEndian.Uint16(body))
	out := make([]uint32, n)
	for i := range out {
		out[i] = binary.BigEndian.Uint32(body[2+4*i:])
	}
	return out
}

// binaryRow decodes a DataRow's raw values; NULL is nil.
func binaryRow(t *testing.T, body []byte) [][]byte {
	t.Helper()
	n := int(binary.BigEndian.Uint16(body))
	body = body[2:]
	out := make([][]byte, n)
	for i := range out {
		l := int32(binary.BigEndian.Uint32(body))
		body = body[4:]
		if l >= 0 {
			out[i] = body[:l]
			body = body[l:]
		}
	}
	if len(body) != 0 {
		t.Fatalf("%d bytes left", len(body))
	}
	return out
}

func errCode(t *testing.T, m msg) string {
	t.Helper()
	if m.typ != 'E' {
		t.Fatalf("want an ErrorResponse, got %q %q", m.typ, m.body)
	}
	f := fields(m.body)
	if f['S'] != "ERROR" {
		t.Fatalf("severity %q", f['S'])
	}
	return f['C']
}

func TestExtendedQueryFlow(t *testing.T) {
	_, addr := startServer(t, Config{})
	cl := openSession(t, addr)
	cl.query("CREATE TABLE t (id int PRIMARY KEY, s text)")

	// Parse with inferred types, Describe the statement, then Bind and
	// Execute it three times, all in one pipeline.
	cl.sends(parseMsg("", "INSERT INTO t VALUES ($1, $2)"), pgwire.EncodeDescribe('S', ""),
		bindText("", "", nil, "1", "one"), execMsg("", 0),
		bindText("", "", nil, "2", "NULL"), execMsg("", 0),
		bindText("", "", nil, "3", ""), execMsg("", 0),
		pgwire.EncodeSync())
	ms := cl.readUntilReady()
	if msgTypes(ms) != "1tn2C2C2CZ" {
		t.Fatalf("%q", msgTypes(ms))
	}
	if got := paramOIDs(ms[1].body); fmt.Sprint(got) != "[23 25]" {
		t.Fatal(got)
	}
	if tag(ms[4]) != "INSERT 0 1" {
		t.Fatal(tag(ms[4]))
	}

	// A named statement: its description, then a portal with a binary
	// first column and text second, fetched two rows at a time.
	cl.sends(parseMsg("sel", "SELECT id, s FROM t WHERE id >= $1 ORDER BY id"), pgwire.EncodeDescribe('S', "sel"),
		bindText("p", "sel", []int16{1, 0}, "1"), pgwire.EncodeDescribe('P', "p"),
		execMsg("p", 2), execMsg("p", 2), execMsg("p", 2), pgwire.EncodeSync())
	ms = cl.readUntilReady()
	if msgTypes(ms) != "1tT2TDDsDCCZ" {
		t.Fatalf("%q", msgTypes(ms))
	}
	if got := paramOIDs(ms[1].body); fmt.Sprint(got) != "[23]" {
		t.Fatal(got)
	}
	// The statement's description has text formats; the portal's has its
	// own.
	if cols := rowDescription(t, ms[2].body); cols[0].format != 0 || cols[1].format != 0 || cols[0].oid != 23 {
		t.Fatal(cols)
	}
	if cols := rowDescription(t, ms[4].body); cols[0].format != 1 || cols[1].format != 0 {
		t.Fatal(cols)
	}
	r := binaryRow(t, ms[5].body)
	if !bytes.Equal(r[0], []byte{0, 0, 0, 1}) || string(r[1]) != "one" {
		t.Fatalf("%v", r)
	}
	if r := binaryRow(t, ms[6].body); r[1] != nil {
		t.Fatalf("NULL came back as %q", r[1])
	}
	if r := binaryRow(t, ms[8].body); r[1] == nil || len(r[1]) != 0 {
		t.Fatalf("the empty string came back as %v", r[1])
	}
	// The last fetch's tag counts its own rows; a finished portal returns
	// nothing more.
	if tag(ms[9]) != "SELECT 1" || tag(ms[10]) != "SELECT 0" {
		t.Fatal(tag(ms[9]), tag(ms[10]))
	}

	// The named statement survives Sync; its portals do not.
	cl.sends(execMsg("p", 0), pgwire.EncodeSync())
	if ms := cl.readUntilReady(); msgTypes(ms) != "EZ" || errCode(t, ms[0]) != sqlerr.InvalidCursorName {
		t.Fatalf("%q", msgTypes(ms))
	}
	cl.sends(bindText("", "sel", nil, "3"), execMsg("", 0), pgwire.EncodeSync())
	if ms := cl.readUntilReady(); msgTypes(ms) != "2DCZ" || dataRow(t, ms[1].body)[0] != "3" {
		t.Fatalf("%q", msgTypes(ms))
	}

	// Flush sends what is ready without ending the sequence.
	cl.sends(bindText("", "sel", nil, "2"), execMsg("", 1), pgwire.EncodeFlush())
	if ms := cl.readN(3); msgTypes(ms) != "2Ds" {
		t.Fatalf("%q", msgTypes(ms))
	}
	cl.sends(execMsg("", 0), pgwire.EncodeSync())
	if ms := cl.readUntilReady(); msgTypes(ms) != "DCZ" || tag(ms[1]) != "SELECT 1" {
		t.Fatalf("%q", msgTypes(ms))
	}

	// Close, then the statement is gone; closing what does not exist is
	// not an error.
	cl.sends(pgwire.EncodeClose('S', "sel"), pgwire.EncodeClose('S', "nope"), pgwire.EncodeClose('P', "nope"),
		bindText("", "sel", nil, "1"), pgwire.EncodeSync())
	ms = cl.readUntilReady()
	if msgTypes(ms) != "333EZ" || errCode(t, ms[3]) != sqlerr.InvalidSQLStatementName {
		t.Fatalf("%q", msgTypes(ms))
	}

	// An empty statement, and statements without results.
	cl.sends(parseMsg("", "  "), pgwire.EncodeDescribe('S', ""), bindText("", "", nil), pgwire.EncodeDescribe('P', ""), execMsg("", 0),
		parseMsg("", "CREATE TABLE IF NOT EXISTS t (id int)"), bindText("", "", nil), execMsg("", 0), pgwire.EncodeSync())
	ms = cl.readUntilReady()
	if msgTypes(ms) != "1tn2nI12NCZ" || tag(ms[9]) != "CREATE TABLE" || fields(ms[8].body)['C'] != sqlerr.DuplicateTable {
		t.Fatalf("%q", msgTypes(ms))
	}

	// A simple query replaces the unnamed statement.
	cl.sends(parseMsg("", "SELECT 1"), pgwire.EncodeSync())
	cl.readUntilReady()
	cl.query("SELECT 2")
	cl.sends(bindText("", "", nil), pgwire.EncodeSync())
	if ms := cl.readUntilReady(); msgTypes(ms) != "EZ" || errCode(t, ms[0]) != sqlerr.InvalidSQLStatementName {
		t.Fatalf("%q", msgTypes(ms))
	}
}

func TestExtendedErrors(t *testing.T) {
	_, addr := startServer(t, Config{})
	cl := openSession(t, addr)
	cl.query("CREATE TABLE t (id int PRIMARY KEY)")
	cl.sends(parseMsg("ins", "INSERT INTO t VALUES ($1)"), parseMsg("sel", "SELECT id FROM t WHERE id = $1"), pgwire.EncodeSync())
	cl.readUntilReady()

	cases := []struct {
		name string
		msgs [][]byte
		code string
	}{
		{"no such statement", [][]byte{bindText("", "nope", nil)}, sqlerr.InvalidSQLStatementName},
		{"no unnamed statement", [][]byte{pgwire.EncodeDescribe('S', "")}, sqlerr.InvalidSQLStatementName},
		{"statement exists", [][]byte{parseMsg("ins", "SELECT 1")}, sqlerr.DuplicatePreparedStatement},
		{"two statements", [][]byte{parseMsg("", "SELECT 1; SELECT 2")}, sqlerr.SyntaxError},
		{"syntax", [][]byte{parseMsg("", "SELEC 1")}, sqlerr.SyntaxError},
		{"unknown table", [][]byte{parseMsg("", "SELECT * FROM nope")}, sqlerr.UndefinedTable},
		{"unsupported type", [][]byte{parseMsg("", "SELECT $1", 1700)}, sqlerr.FeatureNotSupported},
		{"too few values", [][]byte{bindText("", "ins", nil)}, sqlerr.ProtocolViolation},
		{"too many values", [][]byte{bindText("", "ins", nil, "1", "2")}, sqlerr.ProtocolViolation},
		{"bad value", [][]byte{bindText("", "ins", nil, "x")}, sqlerr.InvalidTextRepresentation},
		{"value out of range", [][]byte{bindText("", "ins", nil, "99999999999")}, sqlerr.NumericValueOutOfRange},
		{"result formats", [][]byte{bindText("", "sel", []int16{0, 0}, "1")}, sqlerr.ProtocolViolation},
		{"format code", [][]byte{bindText("", "sel", []int16{2}, "1")}, sqlerr.InvalidParameterValue},
		{"param format code", [][]byte{pgwire.Bind{Statement: "ins", ParamFormats: []int16{3}, Params: [][]byte{[]byte("1")}}.Encode()}, sqlerr.InvalidParameterValue},
		{"no such portal", [][]byte{execMsg("nope", 0)}, sqlerr.InvalidCursorName},
		{"describe no portal", [][]byte{pgwire.EncodeDescribe('P', "nope")}, sqlerr.InvalidCursorName},
		{"portal exists", [][]byte{bindText("p", "sel", nil, "1"), bindText("p", "sel", nil, "1")}, sqlerr.DuplicateCursor},
		{"run twice", [][]byte{bindText("", "ins", nil, "5"), execMsg("", 0), execMsg("", 0)}, sqlerr.ObjectNotInPrerequisiteState},
		{"constraint", [][]byte{bindText("", "ins", nil, "5"), execMsg("", 0)}, sqlerr.UniqueViolation},
		{"malformed bind", [][]byte{message('B', "\x00")}, sqlerr.ProtocolViolation},
		{"malformed describe", [][]byte{message('D', "X\x00")}, sqlerr.ProtocolViolation},
		{"malformed parse", [][]byte{message('P', "s\x00q")}, sqlerr.ProtocolViolation},
		{"malformed execute", [][]byte{message('E', "\x00\x00")}, sqlerr.ProtocolViolation},
		{"malformed close", [][]byte{message('C', "Q\x00")}, sqlerr.ProtocolViolation},
	}
	for _, c := range cases {
		// The error, then everything up to Sync is skipped (a Parse that
		// would succeed and a query that would fail again), then the
		// session works.
		msgs := append(append([][]byte(nil), c.msgs...), parseMsg("later", "SELECT 1"), execMsg("nope", 0), pgwire.EncodeSync())
		cl.sends(msgs...)
		ms := cl.readUntilReady()
		var errs []msg
		for _, m := range ms {
			if m.typ == 'E' {
				errs = append(errs, m)
			}
		}
		if len(errs) != 1 || errCode(t, errs[0]) != c.code {
			t.Errorf("%s: %q %v", c.name, msgTypes(ms), fields(errs[0].body))
		}
		if ms := cl.query("SELECT 1"); msgTypes(ms) != "TDCZ" {
			t.Fatalf("%s: %q", c.name, msgTypes(ms))
		}
	}
	// Nothing after the errors ran: no statement "later".
	cl.sends(pgwire.EncodeDescribe('S', "later"), pgwire.EncodeSync())
	if ms := cl.readUntilReady(); errCode(t, ms[0]) != sqlerr.InvalidSQLStatementName {
		t.Fatal(fields(ms[0].body))
	}
	if got := cl.query("SELECT id FROM t"); msgTypes(got) != "TDCZ" || dataRow(t, got[1].body)[0] != "5" {
		t.Fatalf("%q", msgTypes(got))
	}
	// Bind's message names the statement as PostgreSQL does.
	cl.sends(bindText("", "ins", nil), pgwire.EncodeSync())
	if ms := cl.readUntilReady(); fields(ms[0].body)['M'] != `bind message supplies 0 parameters, but prepared statement "ins" requires 1` {
		t.Fatal(fields(ms[0].body)['M'])
	}
}

// binary values for each type, as PostgreSQL's send functions write them.
func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func be64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

func TestExtendedBinaryValues(t *testing.T) {
	_, addr := startServer(t, Config{})
	cl := openSession(t, addr)
	cl.query("CREATE TABLE v (i int, b bigint, f double precision, s text, o boolean, ts timestamptz)")
	ts := types.TimestampFromTime(time.Date(2024, 1, 2, 3, 4, 5, 500_000_000, time.UTC))
	row := [][]byte{be32(uint32(0xffffff85)), be64(1 << 40), be64(math.Float64bits(-0.25)), []byte("héllo"), {1}, be64(uint64(ts))}
	cl.sends(parseMsg("ins", "INSERT INTO v VALUES ($1, $2, $3, $4, $5, $6)"),
		pgwire.Bind{Statement: "ins", ParamFormats: []int16{1}, Params: row}.Encode(), execMsg("", 0),
		pgwire.Bind{Statement: "ins", ParamFormats: []int16{1}, Params: make([][]byte, 6)}.Encode(), execMsg("", 0),
		pgwire.EncodeSync())
	if ms := cl.readUntilReady(); msgTypes(ms) != "12C2CZ" {
		t.Fatalf("%q %v", msgTypes(ms), fields(ms[len(ms)-2].body))
	}
	// The text forms show the values arrived intact.
	ms := cl.query("SELECT * FROM v WHERE i IS NOT NULL")
	if got := strings.Join(dataRow(t, ms[1].body), "|"); got != "-123|1099511627776|-0.25|héllo|t|2024-01-02 03:04:05.5+00" {
		t.Fatal(got)
	}
	// Binary results give back the same bytes; NULLs stay NULL.
	cl.sends(parseMsg("", "SELECT * FROM v ORDER BY i NULLS LAST"), bindText("", "", []int16{1}), execMsg("", 0), pgwire.EncodeSync())
	ms = cl.readUntilReady()
	if msgTypes(ms) != "12DDCZ" {
		t.Fatalf("%q", msgTypes(ms))
	}
	if got := binaryRow(t, ms[2].body); fmt.Sprint(got) != fmt.Sprint(row) {
		t.Fatalf("%v\nwant %v", got, row)
	}
	if got := binaryRow(t, ms[3].body); fmt.Sprint(got) != fmt.Sprint(make([][]byte, 6)) {
		t.Fatal(got)
	}
	// A parameter declared unknown is inferred, as with 0.
	cl.sends(parseMsg("", "SELECT i FROM v WHERE i = $1", 705), pgwire.EncodeDescribe('S', ""), pgwire.EncodeSync())
	if ms := cl.readUntilReady(); msgTypes(ms) != "1tTZ" || fmt.Sprint(paramOIDs(ms[1].body)) != "[23]" {
		t.Fatalf("%q", msgTypes(ms))
	}
	// Declared smallint, real and varchar parameters are read in their own
	// binary forms, and reported as declared.
	cl.sends(parseMsg("", "SELECT $1 + 1, $2 * 2, $3 || '!'", 21, 700, 1043), pgwire.EncodeDescribe('S', ""),
		pgwire.Bind{ParamFormats: []int16{1}, Params: [][]byte{{0xff, 0xfe}, be32(math.Float32bits(1.5)), []byte("vc")}}.Encode(),
		execMsg("", 0), pgwire.EncodeSync())
	ms = cl.readUntilReady()
	if msgTypes(ms) != "1tT2DCZ" || fmt.Sprint(paramOIDs(ms[1].body)) != "[21 700 1043]" {
		t.Fatalf("%q", msgTypes(ms))
	}
	if got := strings.Join(dataRow(t, ms[4].body), "|"); got != "-1|3|vc!" {
		t.Fatal(got)
	}
	// Infinity timestamps, and the edges of the range.
	minTS := uint64(minTimestamp)
	for _, c := range []struct {
		v    uint64
		want string
	}{{math.MaxInt64, "infinity"}, {1 << 63, "-infinity"}, {minTS, "0001-01-01 00:00:00+00"}, {uint64(maxTimestamp), "10000-12-31 23:59:59.999999+00"}} {
		cl.sends(parseMsg("", "SELECT $1::timestamptz"), pgwire.Bind{ParamFormats: []int16{1}, Params: [][]byte{be64(c.v)}}.Encode(),
			execMsg("", 0), pgwire.EncodeSync())
		ms := cl.readUntilReady()
		if msgTypes(ms) != "12DCZ" || dataRow(t, ms[2].body)[0] != c.want {
			t.Errorf("%d: %q %v", int64(c.v), msgTypes(ms), ms)
		}
	}
	// Bad binary values.
	bad := []struct {
		oid  uint32
		data []byte
		code string
	}{
		{23, []byte{0, 0, 1}, sqlerr.InvalidBinaryRepresentation},
		{20, be32(1), sqlerr.InvalidBinaryRepresentation},
		{701, be32(1), sqlerr.InvalidBinaryRepresentation},
		{700, be64(1), sqlerr.InvalidBinaryRepresentation},
		{21, be32(1), sqlerr.InvalidBinaryRepresentation},
		{16, []byte{}, sqlerr.InvalidBinaryRepresentation},
		{1184, be32(1), sqlerr.InvalidBinaryRepresentation},
		{1184, be64(minTS - 1), sqlerr.DatetimeFieldOverflow},
		{1184, be64(uint64(maxTimestamp + 1)), sqlerr.DatetimeFieldOverflow},
		{25, []byte{0xff}, sqlerr.CharacterNotInRepertoire},
		{25, []byte("a\x00b"), sqlerr.CharacterNotInRepertoire},
	}
	for _, c := range bad {
		cl.sends(parseMsg("", "SELECT $1", c.oid), pgwire.Bind{ParamFormats: []int16{1}, Params: [][]byte{c.data}}.Encode(), pgwire.EncodeSync())
		if ms := cl.readUntilReady(); msgTypes(ms) != "1EZ" || errCode(t, ms[1]) != c.code {
			t.Errorf("%d %x: %q %v", c.oid, c.data, msgTypes(ms), fields(ms[1].body))
		}
	}
	// Text values are checked as text too.
	for _, data := range [][]byte{{'a', 0xc3}, []byte("x\x00")} {
		cl.sends(parseMsg("", "SELECT $1"), pgwire.Bind{Params: [][]byte{data}}.Encode(), pgwire.EncodeSync())
		if ms := cl.readUntilReady(); msgTypes(ms) != "1EZ" || errCode(t, ms[1]) != sqlerr.CharacterNotInRepertoire {
			t.Errorf("%x: %q", data, msgTypes(ms))
		}
	}
}

func TestExtendedLargeResultInChunks(t *testing.T) {
	_, addr := startServer(t, Config{})
	cl := openSession(t, addr)
	cl.query("CREATE TABLE big (id int PRIMARY KEY, pad text)")
	const rows = 5000
	// The first 500 rows through a prepared INSERT, 500 Bind and Execute
	// pairs in one pipeline; the rest in multi-row INSERTs.
	const piped = 500
	load := [][]byte{parseMsg("ins", "INSERT INTO big VALUES ($1, $2)")}
	for i := range piped {
		load = append(load, bindText("", "ins", nil, fmt.Sprint(i), fmt.Sprintf("%0200d", i)), execMsg("", 0))
	}
	cl.sends(append(load, pgwire.EncodeSync())...)
	if ms := cl.readUntilReady(); len(ms) != 2+2*piped || ms[len(ms)-2].typ != 'C' {
		t.Fatalf("%d messages", len(ms))
	}
	for start := piped; start < rows; start += 1500 {
		var vals []string
		for i := start; i < min(start+1500, rows); i++ {
			vals = append(vals, fmt.Sprintf("(%d, '%0200d')", i, i))
		}
		if ms := cl.query("INSERT INTO big VALUES " + strings.Join(vals, ", ")); ms[0].typ != 'C' {
			t.Fatal(fields(ms[0].body))
		}
	}
	// Fetch in chunks of 777 rows, pipelined, in binary.
	msgs := [][]byte{parseMsg("", "SELECT id, pad FROM big ORDER BY id"), bindText("c", "", []int16{1})}
	chunks := rows/777 + 1
	for range chunks {
		msgs = append(msgs, execMsg("c", 777))
	}
	cl.sends(append(msgs, pgwire.EncodeSync())...)
	ms := cl.readUntilReady()
	next, suspended := 0, 0
	for _, m := range ms[2 : len(ms)-1] {
		switch m.typ {
		case 'D':
			r := binaryRow(t, m.body)
			if int(binary.BigEndian.Uint32(r[0])) != next || string(r[1]) != fmt.Sprintf("%0200d", next) {
				t.Fatalf("row %d: %v", next, r[0])
			}
			next++
		case 's':
			if next%777 != 0 {
				t.Fatalf("suspended after %d rows", next)
			}
			suspended++
		case 'C':
			if tag(m) != fmt.Sprintf("SELECT %d", rows%777) {
				t.Fatal(tag(m))
			}
		default:
			t.Fatalf("%q", m.typ)
		}
	}
	if next != rows || suspended != chunks-1 {
		t.Fatalf("%d rows, %d suspensions", next, suspended)
	}
}

func TestExtendedConcurrentSessions(t *testing.T) {
	_, addr := startServer(t, Config{})
	setup := openSession(t, addr)
	setup.query("CREATE TABLE c (id int PRIMARY KEY, n bigint NOT NULL)")
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cl := openSession(t, addr)
			cl.sends(parseMsg("ins", "INSERT INTO c VALUES ($1, $2)"), parseMsg("get", "SELECT n FROM c WHERE id = $1"), pgwire.EncodeSync())
			cl.readUntilReady()
			for i := range 50 {
				id := fmt.Sprint(w*1000 + i)
				cl.sends(bindText("", "ins", nil, id, id), execMsg("", 0), bindText("", "get", nil, id), execMsg("", 0), pgwire.EncodeSync())
				ms := cl.readUntilReady()
				if msgTypes(ms) != "2C2DCZ" || dataRow(t, ms[3].body)[0] != id {
					errs <- fmt.Sprintf("worker %d row %d: %q", w, i, msgTypes(ms))
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
