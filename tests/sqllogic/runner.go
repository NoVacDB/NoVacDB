// Package sqllogic runs SQL logic test files against NoVacDB: scripts of
// statements and queries with the results they must give, in a small
// dialect of SQLite's sqllogictest format. The runner uses only the
// standard library. See docs/design/11-sql-logic-tests.md.
package sqllogic

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/executor"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/types"
	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

// Now is the time now() returns in every test file.
var Now = time.Date(2024, 5, 6, 7, 8, 9, 500000000, time.UTC)

const dbDir = "/db"

// record is one entry of a test file.
type record struct {
	line int    // where the record starts
	kind string // statement, query, restart, crash
	// statement and query
	sql string
	// statement: "ok", or an expected error code and message prefix
	wantErr  string
	wantMsg  string
	wantTag  string // statement ok with an expected command tag
	types    string // query: one letter per column
	sortMode string // query: nosort, rowsort or valuesort
	want     []string
}

// Failure is a mismatch between a test file and NoVacDB.
type Failure struct {
	File string
	Line int
	Msg  string
}

func (f Failure) Error() string { return fmt.Sprintf("%s:%d: %s", f.File, f.Line, f.Msg) }

// parse reads a test file.
func parse(name string, r io.Reader) ([]record, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var lines []string
	for sc.Scan() {
		lines = append(lines, strings.TrimRight(sc.Text(), "\r"))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	bad := func(i int, format string, args ...any) error {
		return Failure{name, i + 1, fmt.Sprintf(format, args...)}
	}
	var recs []record
	for i := 0; i < len(lines); {
		line := lines[i]
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			i++
			continue
		}
		rec := record{line: i + 1}
		f := strings.Fields(line)
		rec.kind = f[0]
		i++
		switch rec.kind {
		case "restart", "crash":
			if len(f) != 1 {
				return nil, bad(i-1, "%s takes no arguments", rec.kind)
			}
			recs = append(recs, rec)
			continue
		case "statement":
			switch {
			case len(f) == 2 && f[1] == "ok":
			case len(f) >= 3 && f[1] == "ok":
				rec.wantTag = strings.Join(f[2:], " ")
			case len(f) >= 3 && f[1] == "error":
				rec.wantErr = f[2]
				if len(rec.wantErr) != 5 {
					return nil, bad(i-1, "error code %q is not a SQLSTATE", rec.wantErr)
				}
				// The message prefix is the rest of the line, as written.
				_, after, _ := strings.Cut(line, rec.wantErr)
				rec.wantMsg = strings.TrimSpace(after)
			default:
				return nil, bad(i-1, "expected \"statement ok [tag]\" or \"statement error CODE [message]\"")
			}
		case "query":
			if len(f) < 2 || len(f) > 3 {
				return nil, bad(i-1, "expected \"query TYPES [nosort|rowsort|valuesort]\"")
			}
			rec.types = f[1]
			for _, c := range rec.types {
				if !strings.ContainsRune("ITRBD", c) {
					return nil, bad(i-1, "unknown column type %q (use I, T, R, B or D)", c)
				}
			}
			rec.sortMode = "nosort"
			if len(f) == 3 {
				rec.sortMode = f[2]
				if rec.sortMode != "nosort" && rec.sortMode != "rowsort" && rec.sortMode != "valuesort" {
					return nil, bad(i-1, "unknown sort mode %q", rec.sortMode)
				}
			}
		default:
			return nil, bad(i-1, "unknown record %q", rec.kind)
		}
		// The SQL runs to a blank line, or to ---- in a query.
		var sql []string
		for i < len(lines) && strings.TrimSpace(lines[i]) != "" && lines[i] != "----" {
			sql = append(sql, lines[i])
			i++
		}
		if len(sql) == 0 {
			return nil, bad(rec.line-1, "%s without SQL", rec.kind)
		}
		rec.sql = strings.Join(sql, "\n")
		if rec.kind == "query" {
			if i >= len(lines) || lines[i] != "----" {
				return nil, bad(rec.line-1, "query without a ---- line before its results")
			}
			i++
			for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
				rec.want = append(rec.want, lines[i])
				i++
			}
		} else if i < len(lines) && lines[i] == "----" {
			return nil, bad(i, "a statement has no results")
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// typeLetter is the column type letter of a SQL type.
func typeLetter(t types.Type) byte {
	switch t {
	case types.Int4, types.Int8:
		return 'I'
	case types.Float8:
		return 'R'
	case types.Bool:
		return 'B'
	case types.TimestampTZ:
		return 'D'
	}
	return 'T'
}

// formatValue prints a value as result lines show it: NULL, (empty) for
// the empty string, otherwise PostgreSQL's text form.
func formatValue(v types.Value) string {
	switch {
	case v.Null:
		return "NULL"
	case v.T == types.Text && v.S == "":
		return "(empty)"
	}
	return types.Format(v)
}

// result renders a query result as lines: one row per line with values
// separated by "|", or one value per line under valuesort.
func result(r *executor.Result, sortMode string) []string {
	var lines []string
	for _, row := range r.Rows {
		vals := make([]string, len(row))
		for i, v := range row {
			vals[i] = formatValue(v)
		}
		if sortMode == "valuesort" {
			lines = append(lines, vals...)
		} else {
			lines = append(lines, strings.Join(vals, "|"))
		}
	}
	if sortMode != "nosort" {
		sort.Strings(lines)
	}
	return lines
}

// Runner runs test files, each against a fresh database on a MemFS.
type Runner struct {
	// Frames is the buffer pool size; zero takes the executor's default.
	Frames int
}

// RunFile runs one test file and returns how many records passed. It
// stops at the first failure, which it returns as a Failure (or a parse
// error): later records usually depend on earlier ones.
func (rn *Runner) RunFile(ctx context.Context, name string, r io.Reader) (records int, err error) {
	recs, err := parse(name, r)
	if err != nil {
		return 0, err
	}
	m := vfs.NewMemFS(1)
	if err := m.MkdirAll(dbDir); err != nil {
		return 0, err
	}
	if err := m.SyncDir("/"); err != nil {
		return 0, err
	}
	opts := executor.Options{
		Frames: rn.Frames,
		Now:    func() time.Time { return Now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	db, err := executor.Open(ctx, m, dbDir, opts)
	if err != nil {
		return 0, err
	}
	defer func() {
		if db != nil {
			_ = db.Close(ctx)
		}
	}()
	for _, rec := range recs {
		fail := func(format string, args ...any) error {
			return Failure{name, rec.line, fmt.Sprintf(format, args...)}
		}
		switch rec.kind {
		case "restart", "crash":
			if rec.kind == "restart" {
				if err := db.Close(ctx); err != nil {
					return records, fail("closing: %v", err)
				}
			} else {
				m.Crash(vfs.CrashOptions{TearLast: true}) // the old db is gone with its handles
			}
			db = nil
			if db, err = executor.Open(ctx, m, dbDir, opts); err != nil {
				return records, fail("reopening: %v", err)
			}
		case "statement":
			rs, err := db.Exec(ctx, rec.sql)
			if rec.wantErr != "" {
				var se *sqlerr.Error
				switch {
				case err == nil:
					return records, fail("statement succeeded; want error %s", rec.wantErr)
				case !errors.As(err, &se):
					return records, fail("error %v is not a SQL error", err)
				case se.Code != rec.wantErr:
					return records, fail("got error %s %q; want %s", se.Code, se.Message, rec.wantErr)
				case !strings.HasPrefix(se.Message, rec.wantMsg):
					return records, fail("got message %q; want it to begin %q", se.Message, rec.wantMsg)
				}
				break
			}
			if err != nil {
				return records, fail("statement failed: %v", err)
			}
			if rec.wantTag != "" {
				if len(rs) == 0 || rs[len(rs)-1].Tag != rec.wantTag {
					var got string
					if len(rs) > 0 {
						got = rs[len(rs)-1].Tag
					}
					return records, fail("command tag %q; want %q", got, rec.wantTag)
				}
			}
		case "query":
			rs, err := db.Exec(ctx, rec.sql)
			if err != nil {
				return records, fail("query failed: %v", err)
			}
			if len(rs) != 1 {
				return records, fail("%d results; a query must be one statement", len(rs))
			}
			r := rs[0]
			got := make([]byte, len(r.Columns))
			for i, c := range r.Columns {
				got[i] = typeLetter(c.Type)
			}
			if string(got) != rec.types {
				return records, fail("column types %s; want %s", got, rec.types)
			}
			lines := result(r, rec.sortMode)
			want := rec.want
			if rec.sortMode != "nosort" {
				want = append([]string(nil), want...)
				sort.Strings(want)
			}
			if strings.Join(lines, "\n") != strings.Join(want, "\n") {
				return records, fail("results differ:\n%s\n---- want\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
			}
		}
		records++
	}
	return records, nil
}
