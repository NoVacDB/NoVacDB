package parser

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/ast"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

// printAll parses sql and prints its statements joined by "; ".
func printAll(t *testing.T, sql string) string {
	t.Helper()
	stmts, err := Parse(sql)
	if err != nil {
		t.Fatalf("Parse(%q): %v", sql, err)
	}
	parts := make([]string, len(stmts))
	for i, s := range stmts {
		parts[i] = s.String()
	}
	return strings.Join(parts, "; ")
}

func TestParseStatements(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{" ; ;; -- nothing\n", ""},
		{"select 1", "SELECT 1"},
		{"SELECT 1; SELECT 2;", "SELECT 1; SELECT 2"},
		{"SELECT", "SELECT "},
		{"select from t", "SELECT  FROM t"},
		{"SELECT a, b AS c, d e, f AS select FROM t", `SELECT a, b AS c, d AS e, f AS "select" FROM t`},
		{"SELECT * FROM t", "SELECT * FROM t"},
		{"SELECT t.*, x.y FROM t x", "SELECT t.*, x.y FROM t AS x"},
		{"SELECT DISTINCT a FROM t", "SELECT DISTINCT a FROM t"},
		{"SELECT ALL a FROM t", "SELECT a FROM t"},
		{"SELECT a FROM t WHERE a = 1 AND b <> 2 OR NOT c",
			"SELECT a FROM t WHERE a = 1 AND b <> 2 OR NOT c"},
		{"SELECT a FROM t ORDER BY a, b DESC, c ASC NULLS FIRST, d DESC NULLS LAST LIMIT 10 OFFSET 5",
			"SELECT a FROM t ORDER BY a, b DESC, c NULLS FIRST, d DESC NULLS LAST LIMIT 10 OFFSET 5"},
		{"SELECT a FROM t OFFSET 2 ROWS LIMIT ALL", "SELECT a FROM t LIMIT NULL OFFSET 2"},
		{`SELECT "A", "select", "x""y", "key", key2 FROM "My Table"`,
			`SELECT "A", "select", "x""y", "key", key2 FROM "My Table"`},
		{"INSERT INTO t VALUES (1, 'a'), (2, DEFAULT)", "INSERT INTO t VALUES (1, 'a'), (2, DEFAULT)"},
		{"INSERT INTO t (a, b) VALUES (1, NULL)", "INSERT INTO t (a, b) VALUES (1, NULL)"},
		{"INSERT INTO t DEFAULT VALUES", "INSERT INTO t DEFAULT VALUES"},
		{"UPDATE t SET a = a + 1, b = DEFAULT WHERE c", "UPDATE t SET a = a + 1, b = DEFAULT WHERE c"},
		{"UPDATE t AS x SET a = 1", "UPDATE t AS x SET a = 1"},
		{"UPDATE t x SET a = 1", "UPDATE t AS x SET a = 1"},
		{"DELETE FROM t", "DELETE FROM t"},
		{"DELETE FROM t AS x WHERE x.a IS NULL", "DELETE FROM t AS x WHERE x.a IS NULL"},
		{"CREATE TABLE t (a int, b bigint NOT NULL, c double precision DEFAULT 1.5, d text NULL, e boolean, f timestamptz)",
			"CREATE TABLE t (a integer, b bigint NOT NULL, c double precision DEFAULT 1.5, d text NULL, e boolean, f timestamptz)"},
		{"create table if not exists t (id int8 primary key, u int4 unique, x float8, y float, z float(53), w bool, v timestamp with time zone, s integer)",
			"CREATE TABLE IF NOT EXISTS t (id bigint PRIMARY KEY, u integer UNIQUE, x double precision, y double precision, z double precision, w boolean, v timestamptz, s integer)"},
		{"CREATE TABLE t (a int, b int, PRIMARY KEY (a, b), UNIQUE (b))",
			"CREATE TABLE t (a integer, b integer, PRIMARY KEY (a, b), UNIQUE (b))"},
		{"CREATE TABLE t ()", "CREATE TABLE t ()"},
		{"CREATE TABLE t (a int DEFAULT 0 NOT NULL, b int DEFAULT -1 PRIMARY KEY)",
			"CREATE TABLE t (a integer NOT NULL DEFAULT 0, b integer PRIMARY KEY DEFAULT -1)"},
		{"CREATE TABLE t (a boolean DEFAULT 1 < 2)", "CREATE TABLE t (a boolean DEFAULT 1 < 2)"},
		{"CREATE TABLE t (a boolean DEFAULT (b AND c))", "CREATE TABLE t (a boolean DEFAULT (b AND c))"},
		{"CREATE TABLE key (text text, \"Index\" int)", `CREATE TABLE "key" (text text, "Index" integer)`},
		{"CREATE TABLE values (between int, integer text, timestamp bool)",
			`CREATE TABLE "values" ("between" integer, "integer" text, "timestamp" boolean)`},
		{`CREATE TABLE t (a "int4", b "text", c "float8", d "timestamptz", e "bool", f "int8", g float(25), h float(53))`,
			"CREATE TABLE t (a integer, b text, c double precision, d timestamptz, e boolean, f bigint, g double precision, h double precision)"},
		{"DROP TABLE t", "DROP TABLE t"},
		{"DROP TABLE IF EXISTS t", "DROP TABLE IF EXISTS t"},
		{"CREATE INDEX i ON t (a, b)", "CREATE INDEX i ON t (a, b)"},
		{"CREATE UNIQUE INDEX IF NOT EXISTS i ON t (a)", "CREATE UNIQUE INDEX IF NOT EXISTS i ON t (a)"},
		{"CREATE INDEX ON t (a)", "CREATE INDEX ON t (a)"},
		{"DROP INDEX i", "DROP INDEX i"},
		{"DROP INDEX IF EXISTS i", "DROP INDEX IF EXISTS i"},
	}
	for _, c := range cases {
		if got := printAll(t, c.in); got != c.want {
			t.Errorf("Parse(%q)\n got  %s\n want %s", c.in, got, c.want)
		}
	}
}

// tree prints an expression fully parenthesised, so tests can see its
// structure (String prints only the parentheses precedence needs).
func tree(e ast.Expr) string {
	paren := func(s string) string { return "(" + s + ")" }
	list := func(es []ast.Expr) string {
		parts := make([]string, len(es))
		for i, x := range es {
			parts[i] = tree(x)
		}
		return strings.Join(parts, ", ")
	}
	switch e := e.(type) {
	case *ast.Binary:
		return paren(tree(e.L) + " " + e.Op + " " + tree(e.R))
	case *ast.Unary:
		return paren(e.Op + " " + tree(e.X))
	case *ast.Is:
		not := ""
		if e.Not {
			not = "NOT "
		}
		return paren(tree(e.X) + " IS " + not + [...]string{"NULL", "TRUE", "FALSE", "UNKNOWN"}[e.Test])
	case *ast.IsDistinct:
		not := ""
		if e.Not {
			not = "NOT "
		}
		return paren(tree(e.L) + " IS " + not + "DISTINCT FROM " + tree(e.R))
	case *ast.Between:
		not := ""
		if e.Not {
			not = "NOT "
		}
		return paren(tree(e.X) + " " + not + "BETWEEN " + tree(e.Lo) + " AND " + tree(e.Hi))
	case *ast.In:
		not := ""
		if e.Not {
			not = "NOT "
		}
		return paren(tree(e.X) + " " + not + "IN (" + list(e.List) + ")")
	case *ast.Like:
		op := "LIKE"
		if e.CaseInsensitive {
			op = "ILIKE"
		}
		if e.Not {
			op = "NOT " + op
		}
		return paren(tree(e.X) + " " + op + " " + tree(e.Pattern))
	case *ast.Cast:
		return "CAST(" + tree(e.X) + " AS " + e.Type.String() + ")"
	case *ast.FuncCall:
		return ast.QuoteIdent(e.Name) + "(" + list(e.Args) + ")"
	case *ast.Case:
		out := "CASE "
		if e.Operand != nil {
			out += tree(e.Operand) + " "
		}
		for _, w := range e.Whens {
			out += "WHEN " + tree(w.Cond) + " THEN " + tree(w.Result) + " "
		}
		if e.Else != nil {
			out += "ELSE " + tree(e.Else) + " "
		}
		return out + "END"
	case *ast.IntegerLit:
		if e.Value < 0 {
			return paren(e.String())
		}
	case *ast.FloatLit:
		if strings.HasPrefix(e.Text, "-") {
			return paren(e.String())
		}
	}
	return e.String()
}

func exprString(t *testing.T, sql string) string {
	t.Helper()
	e, err := ParseExpr(sql)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", sql, err)
	}
	return tree(e)
}

func TestParseExpressions(t *testing.T) {
	cases := []struct{ in, want string }{
		// Precedence and associativity.
		{"1 + 2 * 3", "(1 + (2 * 3))"},
		{"1 - 2 - 3", "((1 - 2) - 3)"},
		{"2 ^ 3 ^ 2", "((2 ^ 3) ^ 2)"},
		{"-2 ^ 2", "((-2) ^ 2)"},
		{"- a ^ 2", "((- a) ^ 2)"}, // unary minus binds tighter than ^, as in PostgreSQL
		{"a * b % c / d", "(((a * b) % c) / d)"},
		{"a || b || c", "((a || b) || c)"},
		{"a + b || c", "((a + b) || c)"},
		{"a || b = c", "((a || b) = c)"},
		{"a = b AND c = d OR e", "(((a = b) AND (c = d)) OR e)"},
		{"a OR b AND c", "(a OR (b AND c))"},
		{"NOT a AND b", "((NOT a) AND b)"},
		{"NOT NOT a", "(NOT (NOT a))"},
		{"NOT a = b", "(NOT (a = b))"},
		{"a = b IS NULL", "((a = b) IS NULL)"},
		{"a IS NOT NULL AND b IS TRUE", "((a IS NOT NULL) AND (b IS TRUE))"},
		{"a IS FALSE IS NOT UNKNOWN", "((a IS FALSE) IS NOT UNKNOWN)"},
		{"a ISNULL OR b NOTNULL", "((a IS NULL) OR (b IS NOT NULL))"},
		{"a IS DISTINCT FROM b + 1", "(a IS DISTINCT FROM (b + 1))"},
		{"a IS NOT DISTINCT FROM NULL", "(a IS NOT DISTINCT FROM NULL)"},
		{"a < b + 1", "(a < (b + 1))"},
		{"a BETWEEN 1 AND 2 AND b", "((a BETWEEN 1 AND 2) AND b)"},
		{"a NOT BETWEEN b + 1 AND c * 2", "(a NOT BETWEEN (b + 1) AND (c * 2))"},
		{"a BETWEEN 1 AND 2 = true", "((a BETWEEN 1 AND 2) = TRUE)"},
		{"a IN (1, 2, b)", "(a IN (1, 2, b))"},
		{"a NOT IN (1)", "(a NOT IN (1))"},
		{"a LIKE 'x%' AND b NOT ILIKE c", "((a LIKE 'x%') AND (b NOT ILIKE c))"},
		{"a || 'b' LIKE c", "((a || 'b') LIKE c)"},
		// Literals and folding.
		{"-1", "(-1)"},
		{"- 1", "(-1)"},
		{"- -1", "1"},
		{"+1", "(+ 1)"},
		{"-1.5", "(-1.5)"},
		{"- -1.5e3", "1.5e3"},
		{"-(2)", "(-2)"},
		{"-9223372036854775808", "(-9223372036854775808)"},
		{"-0x8000000000000000", "(-9223372036854775808)"},
		{"9223372036854775807", "9223372036854775807"},
		{"0x1F + 0o17 + 0b11 + 1_000", "(((31 + 15) + 3) + 1000)"},
		{"1_000.5 + .5 + 1e3", "((1000.5 + .5) + 1e3)"},
		{"'it''s' || E'\\n'", "('it''s' || '\n')"},
		{"TRUE AND FALSE OR NULL", "((TRUE AND FALSE) OR NULL)"},
		{"$1 + $2", "($1 + $2)"},
		// Casts.
		{"a::int", "CAST(a AS integer)"},
		{"a::int::text", "CAST(CAST(a AS integer) AS text)"},
		{"-a::int", "(- CAST(a AS integer))"},
		{"CAST(a + 1 AS bigint)", "CAST((a + 1) AS bigint)"},
		{"timestamptz '2024-01-01 00:00:00+00'", "CAST('2024-01-01 00:00:00+00' AS timestamptz)"},
		{"timestamp with time zone 'x'", "CAST('x' AS timestamptz)"},
		{"double precision '1.5'", "CAST('1.5' AS double precision)"},
		{"int '5' + 1", "(CAST('5' AS integer) + 1)"},
		{"bool 't'", "CAST('t' AS boolean)"},
		// Columns and functions.
		{"t.a + \"T\".\"B\"", `(t.a + "T"."B")`},
		{"t.select", `t."select"`},
		{"lower(a) || upper('x')", "(lower(a) || upper('x'))"},
		{"coalesce(a, b, 1)", `"coalesce"(a, b, 1)`},
		{"now()", "now()"},
		{"left(a, 2)", `"left"(a, 2)`},
		{"CASE WHEN a THEN 1 WHEN b THEN 2 ELSE 3 END", "CASE WHEN a THEN 1 WHEN b THEN 2 ELSE 3 END"},
		{"CASE a WHEN 1 THEN 'x' END", "CASE a WHEN 1 THEN 'x' END"},
		{"(a)", "a"},
	}
	for _, c := range cases {
		if got := exprString(t, c.in); got != c.want {
			t.Errorf("ParseExpr(%q)\n got  %s\n want %s", c.in, got, c.want)
		}
	}
}

func TestParseLiteralValues(t *testing.T) {
	e, _ := ParseExpr("-9223372036854775808")
	if lit, ok := e.(*ast.IntegerLit); !ok || lit.Value != math.MinInt64 {
		t.Fatalf("min int64: %#v", e)
	}
	e, _ = ParseExpr("0b1111")
	if lit, ok := e.(*ast.IntegerLit); !ok || lit.Value != 15 {
		t.Fatalf("0b1111: %#v", e)
	}
	e, _ = ParseExpr("-1.5e2")
	if lit, ok := e.(*ast.FloatLit); !ok || lit.Value != -150 {
		t.Fatalf("-1.5e2: %#v", e)
	}
	e, _ = ParseExpr("1e-300")
	if lit, ok := e.(*ast.FloatLit); !ok || lit.Value != 1e-300 {
		t.Fatalf("1e-300: %#v", e)
	}
	e, _ = ParseExpr("0.0")
	if lit, ok := e.(*ast.FloatLit); !ok || lit.Value != 0 {
		t.Fatalf("0.0: %#v", e)
	}
}

func TestParsePositions(t *testing.T) {
	sql := "SELECT a,  b + 1 FROM tbl WHERE ü = 'x'"
	stmts, err := Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	s := stmts[0].(*ast.Select)
	if s.Pos() != 0 || s.Targets[1].Pos() != 11 || s.Targets[1].Expr.Pos() != 11 || s.From.Pos() != 22 {
		t.Fatalf("positions: %d %d %d %d", s.Pos(), s.Targets[1].Pos(), s.Targets[1].Expr.Pos(), s.From.Pos())
	}
	w := s.Where.(*ast.Binary)
	if w.Pos() != 32 || w.R.Pos() != 37 {
		t.Fatalf("where positions: %d %d", w.Pos(), w.R.Pos())
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		in   string
		code string
		pos  int
		msg  string
	}{
		{"SELEC 1", sqlerr.SyntaxError, 1, `syntax error at or near "SELEC"`},
		{"SELECT 1 +", sqlerr.SyntaxError, 11, "syntax error at end of input"},
		{"SELECT (1", sqlerr.SyntaxError, 10, "syntax error at end of input"},
		{"SELECT 1 2", sqlerr.SyntaxError, 10, `at or near "2"`},
		{"SELECT a, FROM t", sqlerr.SyntaxError, 11, `at or near "FROM"`},
		{"SELECT a FROM t WHERE", sqlerr.SyntaxError, 22, "end of input"},
		{"SELECT a < b < c", sqlerr.SyntaxError, 14, `at or near "<"`},
		{"SELECT a = b = c", sqlerr.SyntaxError, 14, `at or near "="`},
		{"SELECT a BETWEEN 1 AND 2 BETWEEN 3 AND 4", sqlerr.SyntaxError, 26, `at or near "BETWEEN"`},
		{"SELECT a IN (1) IN (2)", sqlerr.SyntaxError, 17, `at or near "IN"`},
		{"SELECT a LIKE b NOT LIKE c", sqlerr.SyntaxError, 17, `at or near "NOT"`},
		{"SELECT a IS", sqlerr.SyntaxError, 12, "end of input"},
		{"SELECT < 1", sqlerr.SyntaxError, 8, `at or near "<"`},
		{"SELECT a @ b", sqlerr.UndefinedFunction, 10, "operator does not exist: @"},
		{"SELECT ~a", sqlerr.UndefinedFunction, 8, "operator does not exist: ~"},
		{"SELECT a !== b", sqlerr.UndefinedFunction, 10, "operator does not exist: !=="},
		{"SELECT 9223372036854775808", sqlerr.NumericValueOutOfRange, 8, "out of range for type bigint"},
		{"SELECT -9223372036854775809", sqlerr.NumericValueOutOfRange, 9, "out of range"},
		{"SELECT -9223372036854775808::bigint", sqlerr.NumericValueOutOfRange, 9, "out of range"},
		{"SELECT 1e400", sqlerr.NumericValueOutOfRange, 8, "out of range for type double precision"},
		{"SELECT 1e-400", sqlerr.NumericValueOutOfRange, 8, "out of range for type double precision"},
		{"SELECT * FROM select", sqlerr.SyntaxError, 15, `at or near "select"`},
		{"CREATE TABLE user (a int)", sqlerr.SyntaxError, 14, `at or near "user"`},
		{"CREATE TABLE t (a int,)", sqlerr.SyntaxError, 23, `at or near ")"`},
		{"CREATE TABLE t (a)", sqlerr.SyntaxError, 18, `at or near ")"`},
		{"CREATE TABLE t (a foo)", sqlerr.UndefinedObject, 19, `type "foo" does not exist`},
		{"CREATE TABLE t (a varchar(10))", sqlerr.FeatureNotSupported, 19, "type varchar is not supported yet"},
		{"CREATE TABLE t (a timestamp)", sqlerr.FeatureNotSupported, 19, "timestamp without time zone"},
		{"CREATE TABLE t (a timestamp without time zone)", sqlerr.FeatureNotSupported, 19, "timestamp without time zone"},
		{"CREATE TABLE t (a int[])", sqlerr.FeatureNotSupported, 22, "array types"},
		{"CREATE TABLE t (a float(10))", sqlerr.FeatureNotSupported, 19, "type real"},
		{"CREATE TABLE t (a float(24))", sqlerr.FeatureNotSupported, 19, "type real"},
		{`CREATE TABLE t (a "integer")`, sqlerr.UndefinedObject, 19, `type "integer" does not exist`},
		{`CREATE TABLE t (a "Int4")`, sqlerr.UndefinedObject, 19, `type "Int4" does not exist`},
		{"CREATE TABLE t (a float(54))", sqlerr.InvalidParameterValue, 25, "between 1 and 53"},
		{"CREATE TABLE t (a double)", sqlerr.SyntaxError, 25, `at or near ")"`},
		{"CREATE TABLE t (a int NULL NOT NULL)", sqlerr.SyntaxError, 17, "conflicting NULL/NOT NULL"},
		{"CREATE TABLE t (a int CHECK (a > 0))", sqlerr.FeatureNotSupported, 23, "CHECK"},
		{"CREATE TABLE t (a int, FOREIGN KEY (a) REFERENCES u (b))", sqlerr.FeatureNotSupported, 24, "FOREIGN constraints"},
		{"CREATE TABLE t (a int PRIMARY)", sqlerr.SyntaxError, 30, `at or near ")"`},
		{"CREATE VIEW v", sqlerr.SyntaxError, 8, `at or near "VIEW"`},
		{"DROP VIEW v", sqlerr.SyntaxError, 6, `at or near "VIEW"`},
		{"DROP TABLE a, b", sqlerr.FeatureNotSupported, 13, "several tables"},
		{"CREATE INDEX CONCURRENTLY i ON t (a)", sqlerr.FeatureNotSupported, 14, "CONCURRENTLY"},
		{"CREATE INDEX i ON t USING hash (a)", sqlerr.FeatureNotSupported, 21, "access methods"},
		{"CREATE INDEX IF NOT EXISTS ON t (a)", sqlerr.SyntaxError, 28, `at or near "ON"`},
		{"INSERT INTO t SELECT 1", sqlerr.FeatureNotSupported, 15, "INSERT ... SELECT"},
		{"INSERT INTO t VALUES (1) RETURNING a", sqlerr.FeatureNotSupported, 26, "RETURNING"},
		{"INSERT INTO t VALUES ()", sqlerr.SyntaxError, 23, `at or near ")"`},
		{"INSERT t VALUES (1)", sqlerr.SyntaxError, 8, `at or near "t"`},
		{"SELECT a FROM t, u", sqlerr.FeatureNotSupported, 16, "joins"},
		{"SELECT a FROM t JOIN u ON true", sqlerr.FeatureNotSupported, 17, "joins"},
		{"SELECT a FROM s.t", sqlerr.FeatureNotSupported, 16, "schema-qualified"},
		{"SELECT a FROM t GROUP BY a", sqlerr.FeatureNotSupported, 17, "GROUP"},
		{"SELECT count(*) FROM t", sqlerr.FeatureNotSupported, 14, "aggregate"},
		{"SELECT (SELECT 1)", sqlerr.FeatureNotSupported, 9, "subqueries"},
		{"SELECT a IN (SELECT 1)", sqlerr.FeatureNotSupported, 14, "subqueries"},
		{"SELECT EXISTS (SELECT 1)", sqlerr.FeatureNotSupported, 8, "EXISTS"},
		{"SELECT (1, 2)", sqlerr.FeatureNotSupported, 10, "row constructors"},
		{"SELECT 1 UNION SELECT 2", sqlerr.FeatureNotSupported, 10, "UNION"},
		{"SELECT current_timestamp", sqlerr.FeatureNotSupported, 8, "CURRENT_TIMESTAMP"},
		{"SELECT a FROM t ORDER BY a NULLS", sqlerr.SyntaxError, 33, "end of input"},
		{"SELECT a FROM t LIMIT 1 LIMIT 2", sqlerr.SyntaxError, 25, `at or near "LIMIT"`},
		{"SELECT t.* + 1", sqlerr.SyntaxError, 12, `at or near "+"`},
		{"SELECT a + t.*", sqlerr.SyntaxError, 14, `at or near "*"`},
		{"SELECT CASE END", sqlerr.SyntaxError, 13, `at or near "END"`},
		{"SELECT CASE WHEN a THEN b", sqlerr.SyntaxError, 26, "end of input"},
		{"SELECT CAST(a int)", sqlerr.SyntaxError, 15, `at or near "int"`},
		{"UPDATE t SET (a, b) = (1, 2)", sqlerr.FeatureNotSupported, 14, "multiple-column"},
		{"UPDATE t SET a 1", sqlerr.SyntaxError, 16, `at or near "1"`},
		{"UPDATE t SET a = 1 FROM u", sqlerr.FeatureNotSupported, 20, "UPDATE ... FROM"},
		{"DELETE t", sqlerr.SyntaxError, 8, `at or near "t"`},
		{"DELETE FROM t USING u", sqlerr.FeatureNotSupported, 15, "USING"},
		{"SELECT a LIKE b ESCAPE 'x'", sqlerr.FeatureNotSupported, 17, "ESCAPE"},
		{"SELECT 1; SELEC 2", sqlerr.SyntaxError, 11, `at or near "SELEC"`},
		{"SELECT 1 SELECT 2", sqlerr.SyntaxError, 10, `at or near "SELECT"`},
		{"'abc", sqlerr.SyntaxError, 1, "unterminated quoted string"},
	}
	for _, c := range cases {
		_, err := Parse(c.in)
		var e *sqlerr.Error
		if !errors.As(err, &e) {
			t.Errorf("Parse(%q) = %v, want an error", c.in, err)
			continue
		}
		if e.Code != c.code || e.Position != c.pos || !strings.Contains(e.Message, c.msg) {
			t.Errorf("Parse(%q) = %q code %s at %d; want %s at %d containing %q", c.in, e.Message, e.Code, e.Position, c.code, c.pos, c.msg)
		}
	}
}

func TestParseHints(t *testing.T) {
	cases := map[string]string{
		"CREATE TABLE t (a timestamp)": "timestamptz",
		"CREATE TABLE t (a varchar)":   "Use text",
		"SELECT * FROM t WHERE a = (1": `")"`,
		"CREATE TABLE order (a int)":   "reserved word",
		"SELECT 99999999999999999999":  "numeric type",
		"SELECT a ~~ b":                "Supported operators",
	}
	for sql, want := range cases {
		_, err := Parse(sql)
		if e := sqlerr.From(err); e == nil || !strings.Contains(e.Hint, want) {
			t.Errorf("Parse(%q): hint %q, want it to mention %q (error %v)", sql, e.Hint, want, err)
		}
	}
}

func TestParseLimits(t *testing.T) {
	deep := strings.Repeat("(", MaxDepth) + "1" + strings.Repeat(")", MaxDepth)
	if _, err := ParseExpr(deep); sqlerr.Code(err) != sqlerr.StatementTooComplex {
		t.Fatalf("%d parentheses: %v", MaxDepth, err)
	}
	ok := strings.Repeat("(", MaxDepth-1) + "1" + strings.Repeat(")", MaxDepth-1)
	if _, err := ParseExpr(ok); err != nil {
		t.Fatalf("%d parentheses: %v", MaxDepth-1, err)
	}
	for _, prefix := range []string{"- ", "NOT ", "CASE WHEN TRUE THEN "} {
		deep := strings.Repeat(prefix, MaxDepth+1) + "1"
		if _, err := ParseExpr(deep); sqlerr.Code(err) != sqlerr.StatementTooComplex {
			t.Fatalf("deep %q: %v", prefix, err)
		}
	}
	// Long flat expressions are fine: no recursion per operator.
	flat := "1" + strings.Repeat(" + 1", 50000)
	if _, err := ParseExpr(flat); err != nil {
		t.Fatalf("long flat expression: %v", err)
	}
}

// stripPositions zeroes every position field (P, TypeP) in a syntax tree,
// so trees parsed from different texts can be compared.
func stripPositions(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			stripPositions(v.Elem())
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			stripPositions(v.Index(i))
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if (f.Name == "P" || f.Name == "TypeP") && f.Type.Kind() == reflect.Int {
				v.Field(i).SetInt(0)
				continue
			}
			stripPositions(v.Field(i))
		}
	}
}

// checkRoundTrip checks that every statement that parses prints to text
// that parses back to the same tree.
func checkRoundTrip(t *testing.T, sql string) {
	t.Helper()
	stmts, err := Parse(sql)
	if err != nil {
		return
	}
	for _, s := range stmts {
		printed := s.String()
		again, err := Parse(printed)
		if err != nil {
			t.Fatalf("%q printed as %q, which does not parse: %v", sql, printed, err)
		}
		if len(again) != 1 {
			t.Fatalf("%q printed as %q, which parses as %d statements", sql, printed, len(again))
		}
		want, got := s, again[0]
		stripPositions(reflect.ValueOf(want))
		stripPositions(reflect.ValueOf(got))
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("%q printed as %q, which parses to a different tree", sql, printed)
		}
	}
}

func TestPrintMinimalParentheses(t *testing.T) {
	cases := []struct{ in, want string }{
		{"(a + b) * c", "(a + b) * c"},
		{"a + (b * c)", "a + b * c"},
		{"a - (b - c)", "a - (b - c)"},
		{"(a - b) - c", "a - b - c"},
		{"(NOT a) = b", "(NOT a) = b"},
		{"NOT (a AND b)", "NOT (a AND b)"},
		{"NOT (a OR b) AND c", "NOT (a OR b) AND c"},
		{"NOT (a = b)", "NOT a = b"},
		{"(a = b) = c", "(a = b) = c"},
		{"a = (b = c)", "a = (b = c)"},
		{"(a IS NULL) IS NULL", "a IS NULL IS NULL"},
		{"(NOT a) IS NULL", "(NOT a) IS NULL"},
		{"- (a ^ 2)", "- (a ^ 2)"},
		{"(-a) ^ 2", "- a ^ 2"},
		{"2 ^ -2", "2 ^ -2"},
		{"a - -1", "a - -1"},
		{"- (NOT a)", "- (NOT a)"},
		{"(a BETWEEN 1 AND 2) BETWEEN c AND d", "(a BETWEEN 1 AND 2) BETWEEN c AND d"},
		{"a BETWEEN (b AND c) AND d", "a BETWEEN (b AND c) AND d"},
		{"(a OR b) AND c", "(a OR b) AND c"},
		{"a || (b || c)", "a || (b || c)"},
		{"(a + b) || c", "a + b || c"},
		{"a IS DISTINCT FROM (b IS NULL)", "a IS DISTINCT FROM (b IS NULL)"},
		{"a IN ((NOT b), c)", "a IN (NOT b, c)"},
	}
	for _, c := range cases {
		e, err := ParseExpr(c.in)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", c.in, err)
		}
		if got := e.String(); got != c.want {
			t.Errorf("%q prints as %q, want %q", c.in, got, c.want)
		}
		again, err := ParseExpr(e.String())
		if err != nil || tree(again) != tree(e) {
			t.Errorf("%q: printed form %q reparses as %v, %v", c.in, e.String(), again, err)
		}
	}
}

func TestPrintingDoesNotDeepen(t *testing.T) {
	// Deep but valid expressions must still parse after printing, or a
	// stored DEFAULT expression could fail to load.
	for _, sql := range []string{
		"SELECT " + "1" + strings.Repeat(" + 1", 5000),
		"SELECT " + strings.Repeat("- ", MaxDepth-1) + "a",
		"SELECT " + strings.Repeat("NOT ", MaxDepth-1) + "a",
		"SELECT " + strings.Repeat("(", MaxDepth-1) + "a" + strings.Repeat(")", MaxDepth-1),
		"SELECT " + strings.Repeat("a + (", MaxDepth-2) + "a" + strings.Repeat(")", MaxDepth-2),
		"SELECT 0" + strings.Repeat("+", 900) + "u0",
	} {
		if _, err := Parse(sql); err != nil {
			t.Fatalf("input does not parse: %v", err)
		}
		checkRoundTrip(t, sql)
	}
}

func TestRoundTrip(t *testing.T) {
	for _, sql := range []string{
		"SELECT -9223372036854775808, - - 5, -(-1.5), +x, - +x, x - -1",
		"SELECT a AS \"FROM\", b \"order\" FROM \"select\" AS \"where\"",
		"SELECT CASE WHEN a BETWEEN -1 AND 1 THEN 'it''s' END, $1::text FROM t WHERE NOT (a IS NULL)",
		"UPDATE \"set\" AS \"x\" SET \"key\" = -1 WHERE coalesce(a, 1) IN (1, -2)",
		"CREATE TABLE \"t t\" (\"a\"\"b\" int DEFAULT -1, PRIMARY KEY (\"a\"\"b\"))",
		"SELECT E'\\x01\\n', 'ü', \"ü\"",
	} {
		checkRoundTrip(t, sql)
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"SELECT a, b + 1 AS c FROM t WHERE a BETWEEN 1 AND 2 OR b IN (1, 2) ORDER BY 1 DESC NULLS LAST LIMIT 5",
		"INSERT INTO t (a, b) VALUES (1, 'x'), (DEFAULT, NULL)",
		"UPDATE t SET a = -a WHERE b IS NOT DISTINCT FROM $1",
		"DELETE FROM t WHERE a LIKE 'x%' AND NOT b",
		"CREATE TABLE t (id bigint PRIMARY KEY, v text NOT NULL DEFAULT 'x', UNIQUE (v))",
		"CREATE UNIQUE INDEX i ON t (a, b); DROP INDEX i; DROP TABLE IF EXISTS t",
		"SELECT CASE a WHEN 1 THEN 2 ELSE 3 END, CAST(a AS text), a::bigint, timestamptz '2024-01-01'",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		_, err := Parse(sql)
		if err != nil {
			var e *sqlerr.Error
			if !errors.As(err, &e) || len(e.Code) != 5 {
				t.Fatalf("bad error %v", err)
			}
			if e.Position < 0 || e.Position > len([]rune(sql))+1 {
				t.Fatalf("position %d outside the query", e.Position)
			}
			return
		}
		checkRoundTrip(t, sql)
	})
}
