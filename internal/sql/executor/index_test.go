package executor

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

// randomValue returns a literal of a column's kind from a small domain, so
// that equal values, NULLs and the special floating-point and timestamp
// values are common.
func randomValue(rng *rand.Rand, kind byte) string {
	if rng.IntN(10) == 0 {
		return "NULL"
	}
	switch kind {
	case 'i':
		return fmt.Sprint(rng.IntN(21) - 10)
	case 'b':
		return fmt.Sprint([]int64{-9223372036854775808, -5, 0, 3, 7, 1 << 40, 9223372036854775807}[rng.IntN(7)])
	case 'c':
		return []string{"0.0", "-0.0", "1.5", "-2.25", "3", "'NaN'", "'Infinity'", "'-Infinity'", "1e300", "-7"}[rng.IntN(10)]
	case 'd':
		return []string{"''", "'a'", "'ab'", "'b'", "'B'", "'a b'", "'é'", "'zz'", "'a%'"}[rng.IntN(9)]
	case 'e':
		return []string{"true", "false"}[rng.IntN(2)]
	}
	return []string{"'2000-01-01 00:00:00+00'", "'1999-12-31 23:59:59.999999+00'", "'infinity'", "'-infinity'", "'2024-02-29 12:00:00+00'", "'0001-01-01 00:00:00+00'"}[rng.IntN(6)]
}

const randomColumns = "abcdef" // kinds: a int, b bigint, c double, d text, e boolean, f timestamptz

func columnKind(col byte) byte { return "ibcdet"[strings.IndexByte(randomColumns, col)] }

// randomConstant is a constant to compare column col with, sometimes of
// another (compatible) type: an integer for a double, a float for an
// integer, an untyped string for anything.
func randomConstant(rng *rand.Rand, col byte) string {
	k := columnKind(col)
	switch rng.IntN(6) {
	case 0:
		if k == 'i' || k == 'b' {
			return []string{"2.5", "-0.5", "1e10", "3.0"}[rng.IntN(4)]
		}
		if k == 'c' {
			return fmt.Sprint(rng.IntN(7) - 3)
		}
	case 1:
		if k == 'i' || k == 'b' {
			return []string{"'4'", "3000000000", "-3000000000", "'-1'"}[rng.IntN(4)]
		}
	}
	return randomValue(rng, k)
}

func randomTerm(rng *rand.Rand) string {
	col := randomColumns[rng.IntN(len(randomColumns))]
	c := string(col)
	if columnKind(col) == 'e' {
		return []string{c, "NOT " + c, c + " = true", c + " IS NULL", "false < " + c}[rng.IntN(5)]
	}
	op := []string{"=", "=", "<", "<=", ">", ">=", "<>"}[rng.IntN(7)]
	switch rng.IntN(8) {
	case 0:
		return fmt.Sprintf("%s BETWEEN %s AND %s", c, randomConstant(rng, col), randomConstant(rng, col))
	case 1:
		return fmt.Sprintf("%s %s %s", randomConstant(rng, col), op, c) // constant first
	case 2:
		return fmt.Sprintf("(%s %s %s OR %s IS NULL)", c, op, randomConstant(rng, col), c)
	case 3:
		return fmt.Sprintf("%s IN (%s, %s)", c, randomConstant(rng, col), randomConstant(rng, col))
	}
	return fmt.Sprintf("%s %s %s", c, op, randomConstant(rng, col))
}

func randomWhere(rng *rand.Rand) string {
	n := 1 + rng.IntN(3)
	terms := make([]string, n)
	for i := range terms {
		terms[i] = randomTerm(rng)
	}
	return strings.Join(terms, " AND ")
}

func sortedRows(t *testing.T, db *DB, sql string) string {
	t.Helper()
	r := strings.Split(rows(t, db, sql), "\n")
	sort.Strings(r)
	return strings.Join(r, "\n")
}

const randomSchema = `CREATE TABLE t (id int PRIMARY KEY, a int, b bigint, c double precision, d text, e boolean, f timestamptz);
	CREATE INDEX ON t (a); CREATE INDEX ON t (b, a); CREATE INDEX ON t (c); CREATE INDEX ON t (d, f);
	CREATE INDEX ON t (e, f); CREATE INDEX ON t (f); CREATE INDEX ON t (d, b, c)`

func fillRandom(t *testing.T, rng *rand.Rand, dbs []*DB, n int) {
	t.Helper()
	for i := range n {
		var vals []string
		for _, col := range []byte(randomColumns) {
			vals = append(vals, randomValue(rng, columnKind(col)))
		}
		sql := fmt.Sprintf("INSERT INTO t VALUES (%d, %s)", i, strings.Join(vals, ", "))
		for _, db := range dbs {
			mustExec(t, db, sql)
		}
	}
}

func TestIndexScansMatchSequentialScans(t *testing.T) {
	seed := testSeed(t)
	rng := rand.New(rand.NewPCG(seed, 45))
	db := newDB(t)
	mustExec(t, db, randomSchema)
	fillRandom(t, rng, []*DB{db}, 600)
	queries := 3000
	if testing.Short() {
		queries = 300
	}
	used := int64(0)
	for range queries {
		where := randomWhere(rng)
		sql := "SELECT * FROM t WHERE " + where
		db.noIndexScans = false
		before := db.indexScans.Load()
		got := sortedRows(t, db, sql)
		used += db.indexScans.Load() - before
		db.noIndexScans = true
		want := sortedRows(t, db, sql)
		if got != want {
			t.Fatalf("seed %d: %s\nindex scan:\n%s\nsequential scan:\n%s", seed, sql, got, want)
		}
	}
	t.Logf("%d of %d queries used an index", used, queries)
	if used < int64(queries)/3 {
		t.Fatalf("only %d of %d queries used an index: the test proves little", used, queries)
	}
}

func TestIndexedWritesMatchSequentialWrites(t *testing.T) {
	// The same random UPDATEs and DELETEs on two databases, one planning
	// index scans and one not, must leave identical tables and indexes.
	seed := testSeed(t)
	rng := rand.New(rand.NewPCG(seed, 46))
	a, b := newDB(t), newDB(t)
	b.noIndexScans = true
	for _, db := range []*DB{a, b} {
		mustExec(t, db, randomSchema)
	}
	fillRandom(t, rng, []*DB{a, b}, 300)
	stmts := 400
	if testing.Short() {
		stmts = 60
	}
	for i := range stmts {
		var sql string
		switch rng.IntN(4) {
		case 0:
			sql = "DELETE FROM t WHERE " + randomWhere(rng)
		case 1:
			var vals []string
			for _, col := range []byte(randomColumns) {
				vals = append(vals, randomValue(rng, columnKind(col)))
			}
			sql = fmt.Sprintf("INSERT INTO t VALUES (%d, %s)", 1000+i, strings.Join(vals, ", "))
		default:
			col := randomColumns[rng.IntN(len(randomColumns))]
			sql = fmt.Sprintf("UPDATE t SET %c = %s WHERE %s", col, randomValue(rng, columnKind(col)), randomWhere(rng))
		}
		ra, erra := a.Exec(bg, sql)
		rb, errb := b.Exec(bg, sql)
		if fmt.Sprint(erra) != fmt.Sprint(errb) || erra == nil && ra[0].Tag != rb[0].Tag {
			t.Fatalf("seed %d: %s: %v / %v", seed, sql, erra, errb)
		}
	}
	all := "SELECT * FROM t"
	if sa, sb := sortedRows(t, a, all), sortedRows(t, b, all); sa != sb {
		t.Fatalf("tables differ:\n%s\n---\n%s", sa, sb)
	}
	checkConsistency(t, a)
	checkConsistency(t, b)
	a.noIndexScans = false
	for _, r := range strings.Split(rows(t, a, "SELECT id, a, b, d FROM t WHERE a IS NOT NULL AND b IS NOT NULL AND d IS NOT NULL"), "\n") {
		if r == "" {
			continue
		}
		f := strings.Split(r, "|")
		q := fmt.Sprintf("SELECT id FROM t WHERE b = %s AND a = %s AND id = %s", f[2], f[1], f[0])
		if got := rows(t, a, q); got != f[0] {
			t.Fatalf("%s: %q", q, got)
		}
	}
}

func TestPlannerReadsOnlyWhatItMust(t *testing.T) {
	// For WHERE clauses an index can answer exactly, the scan must read
	// exactly the matching rows: the best index, the tightest bounds, no
	// NULLs.
	db := newDB(t)
	mustExec(t, db, "CREATE TABLE t (a int, b int, c text); CREATE INDEX i1 ON t (a); CREATE INDEX i2 ON t (a, b); CREATE INDEX i3 ON t (c)")
	for a := range 10 {
		for b := range 10 {
			mustExec(t, db, fmt.Sprintf("INSERT INTO t VALUES (%d, %d, 'c%d'), (NULL, %d, NULL), (%d, NULL, 'x')", a, b, b, b, a))
		}
	}
	cases := []struct {
		where string
		n     int
	}{
		{"a = 3 AND b = 4", 1},
		{"b = 4 AND a = 3", 1},
		{"4 = b AND 3 = a", 1},
		{"a = 3 AND b >= 4", 6},
		{"a = 3 AND b > 4 AND b < 7", 2},
		{"a = 3 AND b BETWEEN 2 AND 3", 2},
		{"a > 3 AND a > 5 AND a <= 8 AND a < 8", 2 * 20},
		{"a >= 5 AND a >= 3 AND a < 9 AND a <= 6", 2 * 20},
		{"5 < a AND 8 > a", 2 * 20},
		{"a >= 8", 2 * 20},
		{"a < 1", 20},
		{"a <= 0", 20},
		{"c = 'c3'", 10},
		{"c >= 'c8'", 20 + 100},
		{"c > 'c8' AND c < 'x'", 10},
		{"a = 3 AND b = 4 AND c = 'nope'", 1},
	}
	for _, c := range cases {
		before := db.rowsRead.Load()
		r := mustExec(t, db, "SELECT * FROM t WHERE "+c.where)[0]
		read := db.rowsRead.Load() - before
		if read != int64(c.n) {
			t.Errorf("WHERE %s: read %d rows, want %d (returned %d)", c.where, read, c.n, len(r.Rows))
		}
	}
	// LIMIT 0 reads nothing, so a WHERE that would fail is never evaluated.
	mustExec(t, db, "INSERT INTO t VALUES (0, 0, 'zero')")
	before := db.rowsRead.Load()
	if got := rows(t, db, "SELECT * FROM t WHERE 1 / a = 1 LIMIT 0"); got != "" || db.rowsRead.Load() != before {
		t.Fatalf("LIMIT 0 read rows: %q", got)
	}
	// LIMIT without ORDER BY stops early, also under DISTINCT.
	before = db.rowsRead.Load()
	if r := mustExec(t, db, "SELECT DISTINCT a FROM t WHERE a IS NOT NULL LIMIT 2")[0]; len(r.Rows) != 2 {
		t.Fatalf("%v", r.Rows)
	} else if n := db.rowsRead.Load() - before; n >= 300 {
		t.Fatalf("DISTINCT ... LIMIT 2 read %d rows", n)
	}
}

func TestPlannerChoosesIndexes(t *testing.T) {
	db := newDB(t)
	// Created in this order: b's index first.
	mustExec(t, db, "CREATE TABLE t (a int, b bigint, c text); CREATE INDEX ib ON t (b); CREATE INDEX ia ON t (a); CREATE INDEX ic ON t (c)")
	for i := range 200 {
		mustExec(t, db, fmt.Sprintf("INSERT INTO t VALUES (%d, %d, 'c%d')", i%10, i, i%40))
	}
	cases := []struct {
		where string
		n     int
	}{
		{"a = 3 AND b > 5", 20},                 // an equality beats a range on an earlier index
		{"b > 5 AND a = 3", 20},                 //
		{"a = 3 AND c = 'c3'", 20},              // a tie goes to the index created first (ia)
		{"c = 'c3' AND b >= 150", 5},            // the equality on ic again
		{"b = 2.5::integer", 1},                 // a cast constant is still a constant
		{"b = 3000000000::bigint::integer", -1}, // a failing cast: left to WHERE, which fails
		{"a = 7::bigint", 200},                  // the column is widened: no index
	}
	for _, c := range cases {
		before := db.rowsRead.Load()
		_, err := db.Exec(bg, "SELECT * FROM t WHERE "+c.where)
		if c.n < 0 {
			if sqlerr.Code(err) != sqlerr.NumericValueOutOfRange {
				t.Errorf("WHERE %s: %v", c.where, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if read := db.rowsRead.Load() - before; read != int64(c.n) {
			t.Errorf("WHERE %s: read %d rows, want %d", c.where, read, c.n)
		}
	}
}
