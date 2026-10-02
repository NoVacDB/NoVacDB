package executor

import (
	"errors"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
)

const fuzzSchema = `CREATE TABLE t (id int PRIMARY KEY, a bigint, f double precision, s text UNIQUE, b boolean, ts timestamptz DEFAULT now());
	CREATE INDEX ON t (a, f); CREATE INDEX ON t (ts);
	INSERT INTO t VALUES (1, 10, 1.5, 'one', true, '2024-01-01'), (2, NULL, 'NaN', 'two', false, NULL),
		(3, -5, -0.0, NULL, NULL, 'infinity'), (4, 10, 2.5, 'four', true, '1999-12-31 23:59:59.5+00')`

// FuzzExec runs arbitrary SQL against a small database. Whatever happens,
// nothing panics, every error is a *sqlerr.Error that blames the query
// (never an internal error, corruption or I/O failure), and the indexes
// still match their tables.
func FuzzExec(f *testing.F) {
	for _, s := range []string{
		"SELECT * FROM t WHERE a = 10 AND f > 1 ORDER BY s DESC NULLS LAST LIMIT 2",
		"UPDATE t SET id = id + 1, s = s || 'x' WHERE b",
		"DELETE FROM t WHERE ts < now() OR a IS NULL",
		"INSERT INTO t (id, s) VALUES (9, 'nine'), (10, NULL)",
		"CREATE TABLE u (x int UNIQUE, y text DEFAULT 'd'); INSERT INTO u DEFAULT VALUES; DROP TABLE u",
		"SELECT DISTINCT b, count FROM t",
		"SELECT CASE WHEN a > 0 THEN 'p' ELSE s END, coalesce(f, 0) FROM t ORDER BY 1",
		"CREATE UNIQUE INDEX ON t (b); DROP INDEX t_s_key",
		"SELECT greatest(a, id), least(f, 1e10), nullif(s, 'one'), length(s), upper(s) FROM t WHERE s LIKE '%o%'",
		"UPDATE t SET a = a * 9223372036854775807",
		"SELECT ts::text, a::double precision / 0 FROM t",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		db := openDB(t, newFS(t), Options{Frames: 64})
		defer func() { _ = db.Close(bg) }()
		mustExec(t, db, fuzzSchema)
		_, err := db.Exec(bg, sql)
		if err != nil {
			var se *sqlerr.Error
			if !errors.As(err, &se) {
				t.Fatalf("%q: error %T %v", sql, err, err)
			}
			switch se.Code {
			case sqlerr.InternalError, sqlerr.DataCorrupted, sqlerr.IOError:
				t.Fatalf("%q: %v", sql, err)
			}
		}
		checkConsistency(t, db)
		// The database still answers.
		if _, err := db.Exec(bg, "SELECT * FROM t"); err != nil && sqlerr.Code(err) != sqlerr.UndefinedTable {
			t.Fatalf("after %q: %v", sql, err)
		}
	})
}
