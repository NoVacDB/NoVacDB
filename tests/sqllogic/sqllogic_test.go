package sqllogic

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSQLLogic runs every testdata/*.test file.
func TestSQLLogic(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.test"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no test files")
	}
	total := 0
	for _, name := range files {
		t.Run(filepath.Base(name), func(t *testing.T) {
			f, err := os.Open(name)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			n, err := (&Runner{Frames: 256}).RunFile(context.Background(), name, f)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				t.Fatal("no records")
			}
			total += n
		})
	}
	t.Logf("%d files, %d records", len(files), total)
}

// TestRunnerCatchesMismatches checks that the runner fails each kind of
// wrong expectation, so passing files mean something.
func TestRunnerCatchesMismatches(t *testing.T) {
	for _, backend := range []struct {
		name string
		open Opener
	}{{"direct", nil}, {"wire", openWire}} {
		t.Run(backend.name, func(t *testing.T) { runnerCatchesMismatches(t, backend.open) })
	}
}

func runnerCatchesMismatches(t *testing.T, open Opener) {
	setup := "statement ok\nCREATE TABLE t (a int, b text)\n\nstatement ok\nINSERT INTO t VALUES (1, 'x'), (2, '')\n\n"
	cases := map[string]string{
		"wrong value":          "query IT rowsort\nSELECT a, b FROM t\n----\n1|x\n2|y\n",
		"empty is not ''":      "query IT rowsort\nSELECT a, b FROM t\n----\n1|x\n2|\n",
		"missing row":          "query I\nSELECT a FROM t ORDER BY a\n----\n1\n",
		"extra row":            "query I\nSELECT a FROM t ORDER BY a\n----\n1\n2\n3\n",
		"wrong order":          "query I nosort\nSELECT a FROM t ORDER BY a\n----\n2\n1\n",
		"wrong types":          "query TT\nSELECT a, b FROM t ORDER BY a\n----\n1|x\n2|(empty)\n",
		"error expected":       "statement error 42P01\nSELECT * FROM t\n",
		"wrong code":           "statement error 42P01\nSELECT * FROM nope2 WHERE nope = 1 AND 1/0 = 1 AND x\n\nstatement error 42703\nSELECT nope FROM nope\n",
		"wrong message":        "statement error 42P01 relation \"other\"\nSELECT * FROM nope\n",
		"statement fails":      "statement ok\nSELECT 1/0\n",
		"query fails":          "query I\nSELECT 1/0\n----\n1\n",
		"wrong tag":            "statement ok INSERT 0 2\nINSERT INTO t VALUES (3, 'z')\n",
		"kept across crash":    "statement ok\nINSERT INTO t VALUES (9, 'nine')\n\ncrash\n\nquery I\nSELECT a FROM t WHERE a = 9\n----\n",
		"two statements query": "query I\nSELECT 1; SELECT 2\n----\n1\n",
	}
	for name, body := range cases {
		_, err := (&Runner{Open: open}).RunFile(context.Background(), name, strings.NewReader(setup+body))
		var f Failure
		if !errors.As(err, &f) {
			t.Errorf("%s: %v, want a Failure", name, err)
		}
	}
	// And the same expectations, written correctly, pass.
	good := setup + "query IT rowsort\nSELECT a, b FROM t\n----\n2|(empty)\n1|x\n\n" +
		"query T valuesort\nSELECT b FROM t\n----\nx\n(empty)\n\n" +
		"statement ok INSERT 0 1\nINSERT INTO t VALUES (3, NULL)\n\n" +
		"statement error 42P01 relation \"nope\" does not exist\nSELECT * FROM nope\n\n" +
		"restart\n\ncrash\n\nquery IT\nSELECT * FROM t WHERE b IS NULL\n----\n3|NULL\n"
	if n, err := (&Runner{Open: open}).RunFile(context.Background(), "good", strings.NewReader(good)); err != nil || n != 9 {
		t.Fatalf("%d records, %v", n, err)
	}
}

func TestBadFiles(t *testing.T) {
	for name, body := range map[string]string{
		"unknown record":    "frobnicate\nSELECT 1\n",
		"no sql":            "statement ok\n\n",
		"bad code":          "statement error 4201\nSELECT\n",
		"no results line":   "query I\nSELECT 1\n",
		"bad type":          "query X\nSELECT 1\n----\n1\n",
		"bad sort":          "query I sideways\nSELECT 1\n----\n1\n",
		"results on stmt":   "statement ok\nSELECT 1\n----\n1\n",
		"restart arguments": "restart now\n",
		"bad statement":     "statement maybe\nSELECT 1\n",
	} {
		if _, err := parse(name, strings.NewReader(body)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}
