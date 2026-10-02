package executor

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

// exprGen makes random expressions over the fuzz schema's columns, of
// every kind the binder handles. Expressions are mostly of the type asked
// for; a few are deliberately of another, to exercise type errors.
type exprGen struct{ rng *rand.Rand }

// Generator types.
const (
	gInt = iota
	gBig
	gFloat
	gText
	gBool
	gTS
	gTypes
)

var genColumn = [gTypes][]string{{"id", "t.id"}, {"a"}, {"f"}, {"s", "t.s"}, {"b"}, {"ts"}}

var genLiteral = [gTypes][]string{
	{"0", "1", "-7", "2147483647", "-2147483648", "'10'"},
	{"9223372036854775807", "5000000000", "-1::bigint", "'-3'"},
	{"1.5", "-0.0", "1e308", "'NaN'", "'-Infinity'", "0.1"},
	{"'x'", "''", "'one'", "'O%'", "'%o%'", "'_w_'", "'é'"},
	{"true", "false", "'t'", "'off'"},
	{"'2024-01-01'", "'infinity'", "'1999-12-31 23:59:59.5+00'", "'2000-01-01 00:00:00+05'"},
}

var genTypeName = [gTypes]string{"integer", "bigint", "double precision", "text", "boolean", "timestamptz"}

func (g *exprGen) numeric() int { return g.rng.IntN(3) }

func (g *exprGen) expr(t, depth int) string {
	r := g.rng
	if r.IntN(25) == 0 {
		t = r.IntN(gTypes) // a deliberate type error, sometimes harmless
	}
	if depth <= 0 || r.IntN(4) == 0 {
		switch r.IntN(10) {
		case 0:
			return "NULL"
		case 1, 2, 3, 4:
			c := genColumn[t]
			return c[r.IntN(len(c))]
		}
		l := genLiteral[t]
		return l[r.IntN(len(l))]
	}
	d := depth - 1
	switch r.IntN(5) {
	case 0:
		return fmt.Sprintf("CASE WHEN %s THEN %s ELSE %s END", g.expr(gBool, d), g.expr(t, d), g.expr(t, d))
	case 1:
		fn := []string{"coalesce", "greatest", "least", "nullif"}[r.IntN(4)]
		return fmt.Sprintf("%s(%s, %s)", fn, g.expr(t, d), g.expr(t, d))
	case 2:
		// From a type with a cast to t (any to and from text, numbers
		// among themselves, integer to and from boolean).
		from := []int{gText, t}[r.IntN(2)]
		switch {
		case t <= gFloat:
			from = []int{gInt, gBig, gFloat, gText}[r.IntN(4)]
		case t == gText:
			from = r.IntN(gTypes)
		case t == gBool && r.IntN(2) == 0:
			from = gInt
		}
		return fmt.Sprintf("CAST(%s AS %s)", g.expr(from, d), genTypeName[t])
	}
	switch t {
	case gInt, gBig, gFloat:
		switch r.IntN(5) {
		case 0:
			return fmt.Sprintf("(- %s)", g.expr(t, d))
		case 1:
			return fmt.Sprintf("abs(%s)", g.expr(t, d))
		case 2:
			if t == gInt {
				return fmt.Sprintf("length(%s)", g.expr(gText, d))
			}
		}
		ops := "+-*/"
		if t != gFloat {
			ops += "%"
		}
		op := ops[r.IntN(len(ops))]
		return fmt.Sprintf("(%s %c %s)", g.expr(t, d), op, g.expr(t, d))
	case gText:
		switch r.IntN(3) {
		case 0:
			return fmt.Sprintf("%s(%s)", []string{"lower", "upper"}[r.IntN(2)], g.expr(gText, d))
		case 1:
			return fmt.Sprintf("(%s || %s)", g.expr(gText, d), g.expr(r.IntN(gTypes), d))
		}
		return fmt.Sprintf("(%s || %s)", g.expr(gText, d), g.expr(gText, d))
	case gTS:
		if r.IntN(2) == 0 {
			return "now()"
		}
		return g.expr(gTS, 0)
	}
	// Boolean.
	u := r.IntN(gTypes)
	switch r.IntN(9) {
	case 0:
		return fmt.Sprintf("(%s %s %s)", g.expr(gBool, d), []string{"AND", "OR"}[r.IntN(2)], g.expr(gBool, d))
	case 1:
		return fmt.Sprintf("(NOT %s)", g.expr(gBool, d))
	case 2:
		tests := []string{"IS NULL", "IS NOT NULL", "IS TRUE", "IS NOT FALSE", "IS UNKNOWN"}
		test := tests[r.IntN(len(tests))]
		if test == "IS NULL" || test == "IS NOT NULL" {
			return fmt.Sprintf("(%s %s)", g.expr(u, d), test)
		}
		return fmt.Sprintf("(%s %s)", g.expr(gBool, d), test)
	case 3:
		return fmt.Sprintf("(%s IS %sDISTINCT FROM %s)", g.expr(u, d), []string{"", "NOT "}[r.IntN(2)], g.expr(u, d))
	case 4:
		return fmt.Sprintf("(%s %sBETWEEN %s AND %s)", g.expr(u, d), []string{"", "NOT "}[r.IntN(2)], g.expr(u, d), g.expr(u, d))
	case 5:
		return fmt.Sprintf("(%s %sIN (%s, %s))", g.expr(u, d), []string{"", "NOT "}[r.IntN(2)], g.expr(u, d), g.expr(u, d))
	case 6:
		return fmt.Sprintf("(%s %s %s)", g.expr(gText, d), []string{"LIKE", "NOT LIKE", "ILIKE"}[r.IntN(3)], g.expr(gText, d))
	}
	ops := []string{"=", "<>", "<", "<=", ">", ">="}
	if u <= gFloat && r.IntN(2) == 0 {
		return fmt.Sprintf("(%s %s %s)", g.expr(u, d), ops[r.IntN(len(ops))], g.expr(g.numeric(), d)) // mixed numerics
	}
	return fmt.Sprintf("(%s %s %s)", g.expr(u, d), ops[r.IntN(len(ops))], g.expr(u, d))
}

// lit is a literal of type t (or NULL).
func (g *exprGen) lit(t int) string {
	if g.rng.IntN(8) == 0 {
		return "NULL"
	}
	l := genLiteral[t]
	return l[g.rng.IntN(len(l))]
}

func (g *exprGen) any(depth int) string { return g.expr(g.rng.IntN(gTypes), depth) }

// checkErr fails the test for errors that blame the database rather than
// the query.
func checkErr(t *testing.T, sql string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	var se *sqlerr.Error
	if !errors.As(err, &se) {
		t.Fatalf("%s: error %T %v", sql, err, err)
	}
	switch se.Code {
	case sqlerr.InternalError, sqlerr.DataCorrupted, sqlerr.IOError:
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestRandomStatements(t *testing.T) {
	seed := testSeed(t)
	rng := rand.New(rand.NewPCG(seed, 47))
	g := &exprGen{rng}
	a, b := newDB(t), newDB(t)
	b.noIndexScans = true
	for _, db := range []*DB{a, b} {
		mustExec(t, db, fuzzSchema)
	}
	n := 4000
	if testing.Short() {
		n = 500
	}
	var oks, errs, planDependent int
	codes := map[string]int{}
	for i := range n {
		var sql string
		switch rng.IntN(6) {
		case 0, 1, 2:
			sql = fmt.Sprintf("SELECT %s, %s FROM t WHERE %s", g.any(3), g.any(2), g.expr(gBool, 3))
			switch rng.IntN(3) {
			case 0:
				sql += fmt.Sprintf(" ORDER BY %s DESC, 1", g.any(2))
			case 1:
				sql = strings.Replace(sql, "SELECT", "SELECT DISTINCT", 1) + " ORDER BY 2, 1 LIMIT 3"
			}
		case 3:
			col := rng.IntN(gTypes)
			sql = fmt.Sprintf("UPDATE t SET %s = %s WHERE %s", genColumn[col][0], g.expr(col, 2), g.expr(gBool, 2))
		case 4:
			sql = fmt.Sprintf("INSERT INTO t (id, a, f, s, b) VALUES (%d, %s, %s, %s, %s)", 100+i, g.lit(gBig), g.lit(gFloat), g.lit(gText), g.lit(gBool))
		default:
			sql = "DELETE FROM t WHERE " + g.expr(gBool, 3)
		}
		ra, erra := a.Exec(bg, sql)
		rb, errb := b.Exec(bg, sql)
		checkErr(t, sql, erra)
		// Which rows an expression is evaluated for, and in what order,
		// depends on the plan, in PostgreSQL too: an index scan reads a
		// subset of the rows in another order. So where the sequential
		// scan meets an error that depends on row data (class 22 or 23),
		// the index plan may succeed or meet another such error first.
		// Anything else must agree.
		ca, cb := sqlerr.Code(erra), sqlerr.Code(errb)
		if ca != cb && (!rowError(cb) || ca != "" && !rowError(ca)) {
			t.Fatalf("seed %d: %s: index plans give %v, sequential scans %v", seed, sql, erra, errb)
		}
		if ca != cb {
			// The two databases now differ; make b match a again.
			planDependent++
			if erra == nil && !strings.HasPrefix(sql, "SELECT") {
				b.noIndexScans = false
				if _, err := b.Exec(bg, sql); err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
				b.noIndexScans = true
			}
			continue
		}
		if erra != nil {
			errs++
			codes[sqlerr.Code(erra)+" "+sqlerr.From(erra).Message[:min(40, len(sqlerr.From(erra).Message))]]++
			continue
		}
		oks++
		// Compared as multisets: rows that sort equal may come in either
		// order, and so may differ under LIMIT.
		fa, fb := sortLines(format(ra[0])), sortLines(format(rb[0]))
		if fa != fb && !strings.Contains(sql, "LIMIT") {
			t.Fatalf("seed %d: %s:\nindex plans:\n%s\nsequential scans:\n%s", seed, sql, fa, fb)
		}
	}
	checkConsistency(t, a)
	checkConsistency(t, b)
	if sa, sb := sortedRows(t, a, "SELECT * FROM t"), sortedRows(t, b, "SELECT * FROM t"); sa != sb {
		t.Fatalf("tables differ:\n%s\n---\n%s", sa, sb)
	}
	t.Logf("%d statements succeeded, %d failed with query errors, %d errors depended on the plan: %v", oks, errs, planDependent, codes)
	if oks < n/3 {
		t.Fatalf("only %d of %d statements succeeded: the generator is too often wrong to test much", oks, n)
	}
}

// rowError reports whether an error code depends on the data of a row.
func rowError(code string) bool {
	return strings.HasPrefix(code, "22") || strings.HasPrefix(code, "23")
}

func sortLines(s string) string {
	lines := strings.Split(s, "\n")
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
