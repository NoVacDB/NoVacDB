package executor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

func TestBasicStatements(t *testing.T) {
	db := newDB(t)
	steps := []struct{ sql, want string }{
		{"CREATE TABLE users (id bigint PRIMARY KEY, name text NOT NULL, email text UNIQUE, age integer DEFAULT 18)", "CREATE TABLE"},
		{"INSERT INTO users VALUES (1, 'ann', 'ann@x', 30), (2, 'bob', NULL, DEFAULT)", "INSERT 0 2"},
		{"INSERT INTO users (name, id) VALUES ('cy', 3)", "INSERT 0 1"},
		{"SELECT * FROM users ORDER BY id", "SELECT 3\n1|ann|ann@x|30\n2|bob|NULL|18\n3|cy|NULL|18"},
		{"SELECT name, age + 1 AS next FROM users WHERE age > 20", "SELECT 1\nann|31"},
		{"UPDATE users SET age = age * 2 WHERE name <> 'ann'", "UPDATE 2"},
		{"SELECT id, age FROM users ORDER BY age DESC, id", "SELECT 3\n2|36\n3|36\n1|30"},
		{"DELETE FROM users WHERE id = 2", "DELETE 1"},
	}
	for _, s := range steps {
		if got := query(t, db, s.sql); got != s.want {
			t.Fatalf("%s\n got %q\nwant %q", s.sql, got, s.want)
		}
	}
	if got := rows(t, db, "SELECT id FROM users ORDER BY 1"); got != "1\n3" {
		t.Fatalf("after delete: %q", got)
	}
	r := mustExec(t, db, "SELECT id AS x, upper(name), 1 + 1, CAST(age AS text), true, now() FROM users LIMIT 1")[0]
	var names []string
	for _, c := range r.Columns {
		names = append(names, c.Name+":"+c.Type.String())
	}
	want := "x:bigint upper:text ?column?:integer age:text bool:boolean now:timestamp with time zone"
	if strings.Join(names, " ") != want {
		t.Fatalf("columns %v", names)
	}
	if got := r.Rows[0][5].String(); got != "2024-05-06 07:08:09.5+00" {
		t.Fatalf("now() = %s", got)
	}
	// An empty select list still makes a SELECT result: no columns, but
	// non-nil, and one empty row per row, so a client gets them all.
	r = mustExec(t, db, "SELECT FROM users")[0]
	if r.Columns == nil || len(r.Columns) != 0 || len(r.Rows) != 2 || r.Tag != "SELECT 2" {
		t.Fatalf("%+v", r)
	}
	if r = mustExec(t, db, "SELECT FROM users WHERE false")[0]; r.Columns == nil || r.Tag != "SELECT 0" {
		t.Fatalf("%+v", r)
	}
}

func TestSelectColumnLimit(t *testing.T) {
	db := newDB(t)
	list := func(n int) string { return strings.Repeat("1, ", n-1) + "1" }
	r := mustExec(t, db, "SELECT "+list(MaxSelectColumns))[0]
	if len(r.Columns) != MaxSelectColumns || len(r.Rows[0]) != MaxSelectColumns {
		t.Fatalf("%d columns", len(r.Columns))
	}
	// One more, or many more, is an error at the first target past the
	// limit: the protocol's column count is 16 bits.
	for _, n := range []int{MaxSelectColumns + 1, 70000} {
		sql := "SELECT " + list(n)
		e := expectErr(t, db, sql, sqlerr.TooManyColumns)
		if e.Position != len("SELECT ")+3*MaxSelectColumns+1 {
			t.Fatalf("%d columns: %v at %d", n, e, e.Position)
		}
	}
	// Stars count every column they expand to.
	cols := make([]string, 1000)
	for i := range cols {
		cols[i] = fmt.Sprintf("c%d int", i)
	}
	mustExec(t, db, "CREATE TABLE wide ("+strings.Join(cols, ", ")+")")
	mustExec(t, db, "INSERT INTO wide (c0) VALUES (1)")
	if r := mustExec(t, db, "SELECT *, "+list(MaxSelectColumns-1000)+" FROM wide")[0]; len(r.Columns) != MaxSelectColumns {
		t.Fatalf("%d columns", len(r.Columns))
	}
	if e := expectErr(t, db, "SELECT *, * FROM wide", sqlerr.TooManyColumns); e.Position != len("SELECT *, ")+1 {
		t.Fatalf("%v at %d", e, e.Position)
	}
	expectErr(t, db, "SELECT *, "+list(MaxSelectColumns-999)+" FROM wide", sqlerr.TooManyColumns)
	// A star that ends exactly at the limit, and one that passes it by one.
	if r := mustExec(t, db, "SELECT "+list(MaxSelectColumns-1000)+", * FROM wide")[0]; len(r.Columns) != MaxSelectColumns {
		t.Fatalf("%d columns", len(r.Columns))
	}
	sql := "SELECT " + list(MaxSelectColumns-999) + ", * FROM wide"
	if e := expectErr(t, db, sql, sqlerr.TooManyColumns); e.Position != strings.Index(sql, "*")+1 {
		t.Fatalf("%v at %d", e, e.Position)
	}
}

func TestDDLStatements(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "CREATE TABLE t (a int, b text)")
	r := mustExec(t, db, "CREATE TABLE IF NOT EXISTS t (x int)")[0]
	if r.Tag != "CREATE TABLE" || len(r.Notices) != 1 || r.Notices[0] != (Notice{sqlerr.DuplicateTable, `relation "t" already exists, skipping`}) {
		t.Fatalf("%+v", r)
	}
	expectErr(t, db, "CREATE TABLE t (x int)", sqlerr.DuplicateTable)
	mustExec(t, db, "CREATE INDEX ON t (a, b)")
	mustExec(t, db, "CREATE UNIQUE INDEX tb ON t (b)")
	if !db.cat.Exists("t_a_b_idx") || !db.cat.Exists("tb") {
		t.Fatal("indexes missing")
	}
	expectErr(t, db, "DROP TABLE tb", sqlerr.WrongObjectType)
	expectErr(t, db, "DROP INDEX t", sqlerr.WrongObjectType)
	expectErr(t, db, "DROP INDEX nope", sqlerr.UndefinedObject)
	expectErr(t, db, "DROP TABLE nope", sqlerr.UndefinedTable)
	if r := mustExec(t, db, "DROP TABLE IF EXISTS nope; DROP INDEX IF EXISTS nope"); r[0].Notices[0] != (Notice{"00000", `table "nope" does not exist, skipping`}) || r[1].Notices[0] != (Notice{"00000", `index "nope" does not exist, skipping`}) {
		t.Fatalf("%+v %+v", r[0], r[1])
	}
	mustExec(t, db, "DROP INDEX tb")
	mustExec(t, db, "DROP TABLE t")
	if len(db.cat.Tables()) != 0 || db.cat.Exists("t_a_b_idx") {
		t.Fatal("drop left objects behind")
	}
	expectErr(t, db, "CREATE TABLE p (a int PRIMARY KEY, b int, PRIMARY KEY (b))", sqlerr.InvalidTableDefinition)
	expectErr(t, db, "CREATE TABLE p (a int NULL NOT NULL)", sqlerr.SyntaxError)
	expectErr(t, db, "CREATE TABLE p (a int DEFAULT b)", sqlerr.FeatureNotSupported)
	expectErr(t, db, "CREATE TABLE p (a int DEFAULT 'x')", sqlerr.InvalidTextRepresentation)
	expectErr(t, db, "CREATE TABLE p (a int DEFAULT true)", sqlerr.DatatypeMismatch)
	expectErr(t, db, "CREATE TABLE novac_x (a int)", sqlerr.ReservedName)
	if len(db.cat.Tables()) != 0 {
		t.Fatal("a failed CREATE TABLE left a table")
	}
}

func TestValuesPersist(t *testing.T) {
	m := newFS(t)
	db := openDB(t, m, Options{})
	mustExec(t, db, `CREATE TABLE t (i int PRIMARY KEY, b bigint, f double precision, s text, ok boolean, ts timestamptz DEFAULT now());
		INSERT INTO t VALUES (1, -9223372036854775808, -0.5, 'it''s', true, '2001-02-03 04:05:06.789+02'), (2, NULL, 'NaN', '', NULL, DEFAULT)`)
	want := "1|-9223372036854775808|-0.5|it's|t|2001-02-03 02:05:06.789+00\n2|NULL|NaN||NULL|2024-05-06 07:08:09.5+00"
	if got := rows(t, db, "SELECT * FROM t ORDER BY i"); got != want {
		t.Fatalf("got\n%s", got)
	}
	if err := db.Close(bg); err != nil {
		t.Fatal(err)
	}
	db = openDB(t, m, Options{})
	defer func() { _ = db.Close(bg) }()
	if got := rows(t, db, "SELECT * FROM t ORDER BY i"); got != want {
		t.Fatalf("after reopening:\n%s", got)
	}
	if _, err := db.Exec(bg, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
}
