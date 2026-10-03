package executor

import (
	"errors"
	"strings"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

func TestExpressions(t *testing.T) {
	db := newDB(t)
	// Each expression's value as SELECT prints it, as PostgreSQL 16 gives
	// it (with DateStyle ISO and TimeZone UTC).
	cases := []struct{ expr, want string }{
		// arithmetic and type resolution
		{"1 + 2 * 3", "7"},
		{"7 / 2", "3"},
		{"-7 / 2", "-3"},
		{"-7 % 3", "-1"},
		{"7.0 / 2", "3.5"},
		{"2 ^ 10", "1024"},
		{"2 ^ 0.5", "1.4142135623730951"},
		{"1 + 2.5", "3.5"},
		{"2147483647 + 1::bigint", "2147483648"},
		{"9223372036854775807", "9223372036854775807"},
		{"-(-5)", "5"},
		{"+3", "3"},
		{"'5' + 1", "6"},
		{"1 + '2.5'::double precision", "3.5"},
		{"'abc' || 'def'", "abcdef"},
		{"'n=' || 42", "n=42"},
		{"1.5 || 'x'", "1.5x"},
		{"'x' || NULL", "NULL"},
		{"'t' || true", "ttrue"},
		{"true::text", "true"},
		// comparison and logic
		{"1 < 2", "t"},
		{"2 <= 2", "t"},
		{"1 = 1.0", "t"},
		{"'a' < 'b'", "t"},
		{"'B' < 'a'", "t"},
		{"1 <> NULL", "NULL"},
		{"NULL = NULL", "NULL"},
		{"true AND NULL", "NULL"},
		{"false AND NULL", "f"},
		{"true OR NULL", "t"},
		{"NOT NULL::boolean", "NULL"},
		{"NOT true", "f"},
		{"'NaN'::double precision = 'NaN'::double precision", "t"},
		{"'NaN'::double precision > 1e308", "t"},
		{"-0.0 = 0.0", "t"},
		{"'2024-01-01'::timestamptz < '2024-01-01 00:00:01+00'", "t"},
		{"'yes' = true", "t"},
		// IS tests, IN, BETWEEN, LIKE
		{"NULL IS NULL", "t"},
		{"1 IS NOT NULL", "t"},
		{"NULL::boolean IS UNKNOWN", "t"},
		{"true IS TRUE", "t"},
		{"NULL::boolean IS NOT FALSE", "t"},
		{"NULL IS DISTINCT FROM NULL", "f"},
		{"1 IS DISTINCT FROM NULL", "t"},
		{"1 IS NOT DISTINCT FROM 1.0", "t"},
		{"2 IN (1, 2, 3)", "t"},
		{"4 IN (1, 2, 3)", "f"},
		{"4 IN (1, NULL)", "NULL"},
		{"1 IN (1, NULL)", "t"},
		{"4 NOT IN (1, NULL)", "NULL"},
		{"4 NOT IN (1, 2)", "t"},
		{"2 BETWEEN 1 AND 3", "t"},
		{"5 BETWEEN 1 AND 3", "f"},
		{"5 NOT BETWEEN 1 AND 3", "t"},
		{"NULL BETWEEN 1 AND 3", "NULL"},
		{"0 NOT BETWEEN 1 AND NULL", "t"},
		{"'hello' LIKE 'h%o'", "t"},
		{"'hello' LIKE 'h_llo'", "t"},
		{"'hello' LIKE 'H%'", "f"},
		{"'hello' ILIKE 'H%'", "t"},
		{"'hello' NOT LIKE '%x%'", "t"},
		{"'50%' LIKE '50\\%'", "t"},
		{"NULL LIKE 'a'", "NULL"},
		// CASE
		{"CASE WHEN 1 > 2 THEN 'a' WHEN 2 > 1 THEN 'b' END", "b"},
		{"CASE WHEN false THEN 1 END", "NULL"},
		{"CASE 2 WHEN 1 THEN 'one' WHEN 2 THEN 'two' ELSE 'many' END", "two"},
		{"CASE 3 WHEN 1 THEN 'one' ELSE 'many' END", "many"},
		{"CASE WHEN true THEN 1 ELSE 2.5 END", "1"},
		{"CASE NULL WHEN NULL THEN 'x' ELSE 'y' END", "y"},
		// casts
		{"CAST('42' AS integer)", "42"},
		{"'  42  '::bigint", "42"},
		{"2.5::integer", "2"},
		{"3.5::integer", "4"},
		{"-2.5::integer", "-2"},
		{"(-2.5)::integer", "-2"},
		{"1::boolean", "t"},
		{"true::integer", "1"},
		{"42::text", "42"},
		{"1e300::text", "1e+300"},
		{"0.1::text", "0.1"},
		{"100000000000000000000.0::text", "1e+20"},
		{"'Infinity'::double precision", "Infinity"},
		{"'-infinity'::timestamptz", "-infinity"},
		{"'2024-02-29T12:00:00Z'::timestamptz", "2024-02-29 12:00:00+00"},
		{"'2024-02-29 12:00:00.1234565+05:30'::timestamptz", "2024-02-29 06:30:00.123456+00"},
		{"'on'::boolean", "t"},
		{"'0x1F'::integer", "31"},
		{"'1_000'::integer", "1000"},
		// functions
		{"lower('AbC')", "abc"},
		{"upper('straße')", "STRAßE"},
		{"length('héllo')", "5"},
		{"length('')", "0"},
		{"abs(-5)", "5"},
		{"abs(-5.5)", "5.5"},
		{"abs(-9223372036854775807)", "9223372036854775807"},
		{"coalesce(NULL, 2, 3)", "2"},
		{"coalesce(NULL, NULL)", "NULL"},
		{"coalesce(NULL, 1, 2.5)", "1"},
		{"nullif(1, 1)", "NULL"},
		{"nullif(1, 2)", "1"},
		{"greatest(1, 5, 3)", "5"},
		{"least(1, NULL, -3)", "-3"},
		{"greatest(NULL, NULL)", "NULL"},
		{"greatest(1, 2.5)", "2.5"},
		{"least('b', 'a')", "a"},
		{"now()", "2024-05-06 07:08:09.5+00"},
		{"NOW()", "2024-05-06 07:08:09.5+00"},
		// short circuit, as PostgreSQL's executor
		{"false AND 1/0 = 1", "f"},
		{"true OR 1/0 = 1", "t"},
		{"CASE WHEN true THEN 1 ELSE 1/0 END", "1"},
		{"coalesce(1, 1/0)", "1"},
	}
	for _, c := range cases {
		if got := rows(t, db, "SELECT "+c.expr); got != c.want {
			t.Errorf("SELECT %s = %q, want %q", c.expr, got, c.want)
		}
	}
}

func TestExpressionErrors(t *testing.T) {
	db := newDB(t)
	mustExec(t, db, "CREATE TABLE t (a int, b text, c boolean)")
	// Each statement's error code, and the text its error position points
	// at (empty: no position).
	cases := []struct{ sql, code, at string }{
		{"SELECT 1/0", sqlerr.DivisionByZero, ""},
		{"SELECT 5 % 0", sqlerr.DivisionByZero, ""},
		{"SELECT 1.0/0", sqlerr.DivisionByZero, ""},
		{"SELECT 2147483647 + 1", sqlerr.NumericValueOutOfRange, ""},
		{"SELECT -(-9223372036854775807 - 1)", sqlerr.NumericValueOutOfRange, ""},
		{"SELECT 1e308 * 10", sqlerr.NumericValueOutOfRange, ""},
		{"SELECT 0 ^ -1", "2201F", ""},
		{"SELECT 'abc' + 1", sqlerr.InvalidTextRepresentation, "'abc'"},
		{"SELECT 1 + 'x'", sqlerr.InvalidTextRepresentation, "'x'"},
		{"SELECT 'a' + 'b'", sqlerr.AmbiguousFunction, "'a'"},
		{"SELECT -'1'", sqlerr.AmbiguousFunction, "-'1'"},
		{"SELECT 1 + true", sqlerr.UndefinedFunction, "1 + true"},
		{"SELECT 'x'::text = 1", sqlerr.UndefinedFunction, "'x'"},
		{"SELECT -true", sqlerr.UndefinedFunction, "-true"},
		{"SELECT 1.5 % 1", sqlerr.UndefinedFunction, "1.5"},
		{"SELECT 1 || 2", sqlerr.UndefinedFunction, "1 || 2"},
		{"SELECT 1 AND true", sqlerr.DatatypeMismatch, "1 AND"},
		{"SELECT NOT 5", sqlerr.DatatypeMismatch, "5"},
		{"SELECT 1 LIKE 'a'", sqlerr.UndefinedFunction, "1 LIKE"},
		{"SELECT 5 IS TRUE", sqlerr.DatatypeMismatch, "5 IS"},
		{"SELECT true::timestamptz", sqlerr.CannotCoerce, "true::"},
		{"SELECT 'x'::boolean", sqlerr.InvalidTextRepresentation, "'x'"},
		{"SELECT '2024-13-01'::timestamptz", sqlerr.DatetimeFieldOverflow, "'2024"},
		{"SELECT 3000000000::integer", sqlerr.NumericValueOutOfRange, ""},
		{"SELECT 'NaN'::double precision::integer", sqlerr.NumericValueOutOfRange, ""},
		{"SELECT foo(1)", sqlerr.UndefinedFunction, "foo(1)"},
		{"SELECT lower(1)", sqlerr.UndefinedFunction, "lower(1)"},
		{"SELECT lower('a', 'b')", sqlerr.UndefinedFunction, "lower('a'"},
		{"SELECT now(1)", sqlerr.UndefinedFunction, "now(1)"},
		{"SELECT abs('1')", sqlerr.AmbiguousFunction, "abs('1')"},
		{"SELECT abs(true)", sqlerr.UndefinedFunction, "abs(true)"},
		{"SELECT coalesce(1, 'x')", sqlerr.InvalidTextRepresentation, "'x'"},
		{"SELECT coalesce(1, true)", sqlerr.DatatypeMismatch, "true"},
		{"SELECT CASE WHEN true THEN 1 ELSE 'a'::text END", sqlerr.DatatypeMismatch, "'a'::text"},
		{"SELECT CASE WHEN 1 THEN 1 END", sqlerr.DatatypeMismatch, "1 THEN"},
		{"SELECT nullif(1, true)", sqlerr.UndefinedFunction, "nullif"},
		{"SELECT abs(-2147483647 - 1)", sqlerr.NumericValueOutOfRange, ""},
		{"SELECT $1", sqlerr.UndefinedParameter, "$1"},
		{"SELECT a FROM t WHERE nope = 1", sqlerr.UndefinedColumn, "nope"},
		{"SELECT x.a FROM t", sqlerr.UndefinedTable, "x.a"},
		{"SELECT t.nope FROM t", sqlerr.UndefinedColumn, "t.nope"},
		{"SELECT t.a FROM t AS u", sqlerr.UndefinedTable, "t.a"},
		{"SELECT a", sqlerr.UndefinedColumn, "a"},
		{"SELECT *", sqlerr.SyntaxError, "*"},
		{"SELECT x.* FROM t", sqlerr.UndefinedTable, "x.*"},
		{"SELECT a FROM nope", sqlerr.UndefinedTable, "nope"},
		{"SELECT a FROM t WHERE a", sqlerr.DatatypeMismatch, "a"},
		{"SELECT a FROM t WHERE 'maybe'", sqlerr.InvalidTextRepresentation, "'maybe'"},
		{"SELECT a FROM t WHERE b = 1", sqlerr.UndefinedFunction, "b = 1"},
		{"SELECT a FROM t WHERE c LIKE 'x'", sqlerr.UndefinedFunction, "c LIKE"},
		{"SELECT DEFAULT", sqlerr.SyntaxError, "DEFAULT"},
	}
	for _, c := range cases {
		e := expectErrSoft(t, db, c.sql, c.code)
		if e == nil {
			continue
		}
		at := ""
		if e.Position > 0 {
			at = string([]rune(c.sql)[e.Position-1:])
		}
		if c.at == "" && e.Position != 0 || c.at != "" && !strings.HasPrefix(at, c.at) {
			t.Errorf("%s: error %v points at %q, want %q", c.sql, e, at, c.at)
		}
	}
}

// expectErrSoft is expectErr reporting with t.Errorf.
func expectErrSoft(t *testing.T, db *DB, sql, code string) *sqlerr.Error {
	t.Helper()
	_, err := db.Exec(bg, sql)
	var se *sqlerr.Error
	if !errors.As(err, &se) || se.Code != code {
		t.Errorf("%s: %v, want code %s", sql, err, code)
		return nil
	}
	return se
}
