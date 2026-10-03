package executor

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/types"
)

func mustPrepare(t testing.TB, db *DB, sql string, declared ...types.Type) *Prepared {
	t.Helper()
	p, err := db.Prepare(bg, sql, declared)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return p
}

func mustRun(t testing.TB, db *DB, p *Prepared, vals ...types.Value) string {
	t.Helper()
	r, err := db.ExecPrepared(bg, p, vals)
	if err != nil {
		t.Fatalf("%s %v: %v", p.sql, vals, err)
	}
	return format(r)
}

func code(err error) string {
	var se *sqlerr.Error
	if errors.As(err, &se) {
		return se.Code
	}
	return fmt.Sprint(err)
}

func typeList(ts []types.Type) string {
	s := make([]string, len(ts))
	for i, t := range ts {
		s[i] = t.String()
	}
	return strings.Join(s, ",")
}

func TestPrepareInfersParameterTypes(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "CREATE TABLE t (id int PRIMARY KEY, big bigint, f double precision, s text, b boolean, ts timestamptz)")
	cases := []struct {
		sql, params, cols string
	}{
		{"INSERT INTO t VALUES ($1, $2, $3, $4, $5, $6)", "integer,bigint,double precision,text,boolean,timestamp with time zone", ""},
		{"INSERT INTO t (s, id) VALUES ($1, $2)", "text,integer", ""},
		{"SELECT id, s FROM t WHERE id = $1", "integer", "id:integer s:text"},
		{"SELECT * FROM t WHERE big > $1 AND s LIKE $2", "bigint,text", "id:integer big:bigint f:double precision s:text b:boolean ts:timestamp with time zone"},
		{"SELECT $1", "text", "?column?:text"},
		{"SELECT $1::int + 1 AS n", "integer", "n:integer"},
		{"SELECT $1 + 1", "integer", "?column?:integer"},
		{"SELECT $1 || 'x', $2 = $3", "text,text,text", "?column?:text ?column?:boolean"},
		{"SELECT $1 IS NULL", "text", "?column?:boolean"},
		{"SELECT f FROM t WHERE $1 AND f < $2", "boolean,double precision", "f:double precision"},
		{"SELECT id FROM t LIMIT $1 OFFSET $2", "bigint,bigint", "id:integer"},
		{"SELECT id FROM t WHERE id IN ($1, $2) ORDER BY id", "integer,integer", "id:integer"},
		{"SELECT coalesce($1, big) FROM t", "bigint", "coalesce:bigint"},
		{"UPDATE t SET s = $2, f = f * $3 WHERE id = $1", "integer,text,double precision", ""},
		{"DELETE FROM t WHERE ts < $1", "timestamp with time zone", ""},
		// A later number makes the earlier ones exist, typed text when
		// nothing decides.
		{"SELECT id FROM t WHERE id = $3", "text,text,integer", "id:integer"},
		// DDL is not looked at: it has no parameters.
		{"CREATE TABLE u (a int)", "", ""},
	}
	for _, c := range cases {
		p := mustPrepare(t, db, c.sql)
		var cols []string
		for _, col := range p.Columns {
			cols = append(cols, col.Name+":"+col.Type.String())
		}
		if got := typeList(p.ParamTypes); got != c.params {
			t.Errorf("%s: parameters %q, want %q", c.sql, got, c.params)
		}
		if got := strings.Join(cols, " "); got != c.cols {
			t.Errorf("%s: columns %q, want %q", c.sql, got, c.cols)
		}
		if (p.Columns != nil) != strings.HasPrefix(c.sql, "SELECT") {
			t.Errorf("%s: columns %v", c.sql, p.Columns)
		}
	}
	// Preparing changed nothing: u was not created, t is empty.
	expectErr(t, db, "SELECT * FROM u", sqlerr.UndefinedTable)
	if got := query(t, db, "SELECT * FROM t"); got != "SELECT 0" {
		t.Fatal(got)
	}
}

func TestPrepareDeclaredTypes(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "CREATE TABLE t (id int PRIMARY KEY, big bigint)")
	// Declared types win over inference; Unknown leaves one to inference;
	// declared parameters the query does not use still count.
	p := mustPrepare(t, db, "SELECT id FROM t WHERE big = $2", types.Int8, types.Unknown, types.Bool)
	if got := typeList(p.ParamTypes); got != "bigint,bigint,boolean" {
		t.Fatal(got)
	}
	p = mustPrepare(t, db, "SELECT $1", types.Int4)
	if got := typeList(p.ParamTypes) + " " + p.Columns[0].Type.String(); got != "integer integer" {
		t.Fatal(got)
	}
	// A declared type the context cannot take is the context's error.
	if _, err := db.Prepare(bg, "SELECT id FROM t WHERE id = $1", []types.Type{types.Bool}); code(err) != sqlerr.UndefinedFunction {
		t.Fatal(err)
	}
	// Into a column: the assignment casts of INSERT.
	p = mustPrepare(t, db, "INSERT INTO t VALUES ($1, $2)", types.Int8, types.Int4)
	if got := mustRun(t, db, p, types.NewInt8(7), types.NewInt4(70)); got != "INSERT 0 1" {
		t.Fatal(got)
	}
	if _, err := db.ExecPrepared(bg, p, []types.Value{types.NewInt8(1 << 40), types.NewInt4(1)}); code(err) != sqlerr.NumericValueOutOfRange {
		t.Fatal(err)
	}
	if got := rows(t, db, "SELECT * FROM t"); got != "7|70" {
		t.Fatal(got)
	}
}

func TestExecPrepared(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "CREATE TABLE t (id int PRIMARY KEY, s text, f double precision)")
	ins := mustPrepare(t, db, "INSERT INTO t VALUES ($1, $2, $3)")
	for i := range 100 {
		if got := mustRun(t, db, ins, types.NewInt4(int32(i)), types.NewText(fmt.Sprintf("s%d", i)), types.NewFloat8(float64(i)/2)); got != "INSERT 0 1" {
			t.Fatal(got)
		}
	}
	// NULL parameters, and values that would need quoting in SQL text.
	if got := mustRun(t, db, ins, types.NewInt4(100), types.Null(types.Text), types.Null(types.Float8)); got != "INSERT 0 1" {
		t.Fatal(got)
	}
	if got := mustRun(t, db, ins, types.NewInt4(101), types.NewText("it's; DROP TABLE t; --"), types.NewFloat8(0)); got != "INSERT 0 1" {
		t.Fatal(got)
	}
	sel := mustPrepare(t, db, "SELECT s, f FROM t WHERE id = $1")
	for _, c := range []struct {
		id   int32
		want string
	}{{5, "SELECT 1\ns5|2.5"}, {100, "SELECT 1\nNULL|NULL"}, {101, "SELECT 1\nit's; DROP TABLE t; --|0"}, {999, "SELECT 0"}} {
		before := db.indexScans.Load()
		if got := mustRun(t, db, sel, types.NewInt4(c.id)); got != c.want {
			t.Fatalf("id %d: %q", c.id, got)
		}
		// A parameter is a constant to the planner: the primary key is used.
		if db.indexScans.Load() != before+1 {
			t.Fatalf("id %d: no index scan", c.id)
		}
	}
	if got := mustRun(t, db, sel, types.Null(types.Int4)); got != "SELECT 0" {
		t.Fatal(got)
	}
	// LIMIT and OFFSET from parameters; NULL is no limit.
	page := mustPrepare(t, db, "SELECT id FROM t ORDER BY id LIMIT $1 OFFSET $2")
	if got := mustRun(t, db, page, types.NewInt8(3), types.NewInt8(10)); got != "SELECT 3\n10\n11\n12" {
		t.Fatal(got)
	}
	if got := mustRun(t, db, page, types.Null(types.Int8), types.NewInt8(100)); got != "SELECT 2\n100\n101" {
		t.Fatal(got)
	}
	if _, err := db.ExecPrepared(bg, page, []types.Value{types.NewInt8(-1), types.NewInt8(0)}); code(err) != sqlerr.InvalidRowCountInLimit {
		t.Fatal(err)
	}
	upd := mustPrepare(t, db, "UPDATE t SET f = f + $2 WHERE id < $1")
	if got := mustRun(t, db, upd, types.NewInt4(10), types.NewFloat8(1000)); got != "UPDATE 10" {
		t.Fatal(got)
	}
	del := mustPrepare(t, db, "DELETE FROM t WHERE f > $1")
	if got := mustRun(t, db, del, types.NewFloat8(999)); got != "DELETE 10" {
		t.Fatal(got)
	}
	// Errors when run: a constraint, and the wrong number or types of
	// values.
	if _, err := db.ExecPrepared(bg, ins, []types.Value{types.NewInt4(50), types.Null(types.Text), types.Null(types.Float8)}); code(err) != sqlerr.UniqueViolation {
		t.Fatal(err)
	}
	if _, err := db.ExecPrepared(bg, sel, nil); code(err) != sqlerr.ProtocolViolation {
		t.Fatal(err)
	}
	if _, err := db.ExecPrepared(bg, sel, []types.Value{types.NewText("1")}); code(err) != sqlerr.InternalError {
		t.Fatal(err)
	}
	// The same prepared statement from many goroutines.
	errs := make(chan error, 8)
	for g := range 8 {
		go func() {
			for i := range 50 {
				r, err := db.ExecPrepared(bg, sel, []types.Value{types.NewInt4(int32(10 + (g*50+i)%90))})
				if err == nil && len(r.Rows) != 1 {
					err = fmt.Errorf("%d rows", len(r.Rows))
				}
				if err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrepareErrors(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "CREATE TABLE t (id int)")
	for _, c := range []struct{ sql, code string }{
		{"SELECT 1; SELECT 2", sqlerr.SyntaxError},
		{"SELECT nope FROM t", sqlerr.UndefinedColumn},
		{"SELECT * FROM missing WHERE id = $1", sqlerr.UndefinedTable},
		{"SELECT $1 + $2", sqlerr.AmbiguousFunction},
		{"SELECT -$1", sqlerr.AmbiguousFunction},
		{"SELECT id FROM t WHERE id = $1 AND $1 = true", sqlerr.UndefinedFunction},
		{"SELECT $0", sqlerr.UndefinedParameter},
		{"SELECT $65536", sqlerr.UndefinedParameter},
		{"SELEC 1", sqlerr.SyntaxError},
	} {
		if _, err := db.Prepare(bg, c.sql, nil); code(err) != c.code {
			t.Errorf("%s: %v, want %s", c.sql, err, c.code)
		}
	}
	if _, err := db.Prepare(bg, "SELECT 1", make([]types.Type, MaxParams+1)); code(err) != sqlerr.ProgramLimitExceeded {
		t.Fatal(err)
	}
	// The most parameters a statement can have.
	p := mustPrepare(t, db, "SELECT $65535")
	if len(p.ParamTypes) != MaxParams {
		t.Fatal(len(p.ParamTypes))
	}
	// Empty: no statement, no result.
	p = mustPrepare(t, db, " ; -- nothing")
	if !p.Empty() || p.Columns != nil {
		t.Fatal(p)
	}
	if r, err := db.ExecPrepared(bg, p, nil); r != nil || err != nil {
		t.Fatal(r, err)
	}
	// Parameters in DDL are an error when it runs, as nothing binds them.
	p = mustPrepare(t, db, "CREATE TABLE u (a int DEFAULT $1)")
	if _, err := db.ExecPrepared(bg, p, nil); code(err) != sqlerr.UndefinedParameter {
		t.Fatal(err)
	}
	// Parameters in the simple protocol are an error too.
	expectErr(t, db, "SELECT $1", sqlerr.UndefinedParameter)
}

func TestPreparedAfterSchemaChanges(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "CREATE TABLE t (id int, s text)")
	mustExec(t, db, "INSERT INTO t VALUES (1, 'a')")
	sel := mustPrepare(t, db, "SELECT * FROM t WHERE id = $1")
	ins := mustPrepare(t, db, "INSERT INTO t VALUES ($1, $2)")
	// Same columns again: still fine.
	mustExec(t, db, "DROP TABLE t")
	mustExec(t, db, "CREATE TABLE t (id int, s text)")
	if got := mustRun(t, db, ins, types.NewInt4(2), types.NewText("b")); got != "INSERT 0 1" {
		t.Fatal(got)
	}
	if got := mustRun(t, db, sel, types.NewInt4(2)); got != "SELECT 1\n2|b" {
		t.Fatal(got)
	}
	// Different result columns: refused rather than sent under the old
	// description.
	mustExec(t, db, "DROP TABLE t")
	mustExec(t, db, "CREATE TABLE t (id int, s text, extra bigint)")
	if _, err := db.ExecPrepared(bg, sel, []types.Value{types.NewInt4(2)}); code(err) != sqlerr.FeatureNotSupported {
		t.Fatal(err)
	}
	// The same number of columns, of another type: refused too.
	mustExec(t, db, "DROP TABLE t")
	mustExec(t, db, "CREATE TABLE t (id int, s bigint)")
	if _, err := db.ExecPrepared(bg, sel, []types.Value{types.NewInt4(2)}); code(err) != sqlerr.FeatureNotSupported {
		t.Fatal(err)
	}
	// Gone: the table's error.
	mustExec(t, db, "DROP TABLE t")
	if _, err := db.ExecPrepared(bg, ins, []types.Value{types.NewInt4(3), types.NewText("c")}); code(err) != sqlerr.UndefinedTable {
		t.Fatal(err)
	}
	// A column whose type changed under a parameter: bound again, with
	// the assignment cast PostgreSQL would use (any type into text)...
	mustExec(t, db, "CREATE TABLE t (id text, s text)")
	if got := mustRun(t, db, ins, types.NewInt4(3), types.NewText("c")); got != "INSERT 0 1" {
		t.Fatal(got)
	}
	if got := rows(t, db, "SELECT id || '!' FROM t"); got != "3!" {
		t.Fatal(got)
	}
	// ... or its error where there is none.
	mustExec(t, db, "DROP TABLE t")
	mustExec(t, db, "CREATE TABLE t (id boolean, s text)")
	if _, err := db.ExecPrepared(bg, ins, []types.Value{types.NewInt4(3), types.NewText("c")}); code(err) != sqlerr.DatatypeMismatch {
		t.Fatal(err)
	}
}
