package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"path"
	"testing"
	"time"

	"github.com/vikrant-choudhary06/NoVacDB/internal/btree"
	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

// stmtRow is a heap row and B+Tree key tagged with its statement number.
func stmtRow(stmt, i int) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(stmt))
	b = binary.BigEndian.AppendUint32(b, uint32(i))
	return append(b, bytes.Repeat([]byte{byte(stmt)}, 300)...)
}

// countByStatement reads a heap and a tree and counts rows and keys per
// statement number.
func countByStatement(t *testing.T, h *storage.Heap, tr *btree.Tree) (map[int]int, map[int]int) {
	t.Helper()
	rows, keys := map[int]int{}, map[int]int{}
	s := h.Scan()
	for {
		_, d, ok, err := s.Next(bg)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		rows[int(binary.BigEndian.Uint32(d))]++
	}
	it := tr.Scan(btree.Bound{}, btree.Bound{})
	for {
		k, _, ok, err := it.Next(bg)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		keys[int(binary.BigEndian.Uint32(k))]++
	}
	if _, err := tr.Check(bg); err != nil {
		t.Fatal(err)
	}
	return rows, keys
}

// runStatement inserts n tagged rows into the heap and the tree inside one
// statement group, and returns the commit error (or the first failure).
func runStatement(e *Engine, h *storage.Heap, tr *btree.Tree, stmt, n int) error {
	if err := e.BeginStatement(bg); err != nil {
		return err
	}
	for i := range n {
		r := stmtRow(stmt, i)
		if _, err := h.Insert(bg, r); err != nil {
			return err
		}
		if err := tr.Insert(bg, r[:8+rand.IntN(200)], nil); err != nil {
			return err
		}
	}
	_, err := e.CommitStatement(bg)
	return err
}

func setupHeapAndTree(t *testing.T, e *Engine) (*storage.Heap, *btree.Tree) {
	t.Helper()
	if err := e.BeginStatement(bg); err != nil {
		t.Fatal(err)
	}
	h, err := e.CreateHeap(bg)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := e.CreateBTree(bg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CommitStatement(bg); err != nil {
		t.Fatal(err)
	}
	return h, tr
}

func TestCommittedStatementSurvivesAndUncommittedIsDiscarded(t *testing.T) {
	m := newFS(t)
	e := mustEngine(t, m, EngineOptions{Frames: 64})
	h, tr := setupHeapAndTree(t, e)
	if err := runStatement(e, h, tr, 1, 30); err != nil {
		t.Fatal(err)
	}
	// Statement 2 is left open, and its records are made durable.
	if err := e.BeginStatement(bg); err != nil {
		t.Fatal(err)
	}
	for i := range 30 {
		if _, err := h.Insert(bg, stmtRow(2, i)); err != nil {
			t.Fatal(err)
		}
		if err := tr.Insert(bg, stmtRow(2, i), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.w.Flush(bg); err != nil {
		t.Fatal(err)
	}
	m.Crash(vfs.CrashOptions{TearLast: true})

	e2 := mustEngine(t, m, EngineOptions{Frames: 64})
	defer func() { _ = e2.Close(bg) }()
	rec := e2.Recovery()
	if rec.DiscardedStatements != 1 || rec.Discarded < 60 {
		t.Fatalf("recovery %+v: want the open statement's records discarded", rec)
	}
	h2, err := e2.OpenHeap(bg, h.FirstPage())
	if err != nil {
		t.Fatal(err)
	}
	tr2, err := e2.OpenBTree(bg, tr.Root())
	if err != nil {
		t.Fatal(err)
	}
	rows, keys := countByStatement(t, h2, tr2)
	if rows[1] != 30 || keys[1] != 30 || rows[2] != 0 || keys[2] != 0 {
		t.Fatalf("rows %v keys %v", rows, keys)
	}
}

// diskLSNs returns the highest page LSN in the data file as stored.
func maxDiskLSN(t *testing.T, m *vfs.MemFS) uint64 {
	t.Helper()
	raw := readFile(t, m, path.Join(dbDir, DataFileName))
	var hi uint64
	for off := storage.FirstDataPage * storage.PageSize; off+storage.PageSize <= len(raw); off += storage.PageSize {
		hi = max(hi, storage.PageLSN(raw[off:off+storage.PageSize]))
	}
	return hi
}

func TestStatementPagesNeverReachDiskBeforeCommit(t *testing.T) {
	m := newFS(t)
	e := mustEngine(t, m, EngineOptions{Frames: 12})
	h, tr := setupHeapAndTree(t, e)
	for s := 1; s <= 3; s++ {
		if err := runStatement(e, h, tr, s, 8); err != nil {
			t.Fatal(err)
		}
	}
	// Committed work in another heap, which the statement below does not
	// touch, leaves dirty pages that eviction may and must write.
	if err := e.BeginStatement(bg); err != nil {
		t.Fatal(err)
	}
	other, err := e.CreateHeap(bg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 150 {
		if _, err := other.Insert(bg, stmtRow(100, i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.CommitStatement(bg); err != nil {
		t.Fatal(err)
	}
	before := e.Pool().Stats().Writes
	if err := e.BeginStatement(bg); err != nil {
		t.Fatal(err)
	}
	horizon := e.horizon.Load()
	n := 0
	for ; err == nil && n < 1000; n++ {
		r := stmtRow(9, n)
		if _, err = h.Insert(bg, r); err == nil {
			err = tr.Insert(bg, r, nil)
		}
		if hi := maxDiskLSN(t, m); hi > horizon {
			t.Fatalf("after %d inserts a page with lsn %d reached disk; the statement began at %d", n, hi, horizon)
		}
	}
	if !errors.Is(err, storage.ErrNoFreeFrames) {
		t.Fatalf("a statement larger than the pool: %v after %d rows", err, n)
	}
	if e.Pool().Stats().Writes == before {
		t.Fatalf("no page was evicted during the statement (%d rows, %+v): the test proves nothing", n, e.Pool().Stats())
	}
	// The statement cannot finish; abandoning and reopening discards it.
	if err := e.Abandon(); err != nil {
		t.Fatal(err)
	}
	e2 := mustEngine(t, m, EngineOptions{Frames: 12})
	defer func() { _ = e2.Close(bg) }()
	h2, _ := e2.OpenHeap(bg, h.FirstPage())
	tr2, _ := e2.OpenBTree(bg, tr.Root())
	rows, keys := countByStatement(t, h2, tr2)
	if rows[9] != 0 || keys[9] != 0 || rows[3] != 8 || keys[3] != 8 {
		t.Fatalf("rows %v keys %v", rows, keys)
	}
}

func TestCheckpointWaitsForStatement(t *testing.T) {
	m := newFS(t)
	e := mustEngine(t, m, EngineOptions{Frames: 32})
	defer func() { _ = e.Close(bg) }()
	h, tr := setupHeapAndTree(t, e)
	if err := e.BeginStatement(bg); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Insert(bg, stmtRow(1, 0)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.Checkpoint(bg)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("checkpoint ran during a statement: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tr.Insert(bg, stmtRow(1, 0), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CommitStatement(bg); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("checkpoint after the statement: %v", err)
	}
}

func TestStatementMisuse(t *testing.T) {
	m := newFS(t)
	e := mustEngine(t, m, EngineOptions{Frames: 8})
	if _, err := e.CommitStatement(bg); !errors.Is(err, ErrStatement) {
		t.Fatalf("commit without begin: %v", err)
	}
	if err := e.BeginStatement(bg); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(bg); !errors.Is(err, ErrStatement) {
		t.Fatalf("close with a statement open: %v", err)
	}
	if err := e.BeginStatement(bg); !errors.Is(err, ErrClosed) {
		t.Fatalf("begin after close: %v", err)
	}
	if err := e.Abandon(); !errors.Is(err, ErrClosed) {
		t.Fatalf("abandon after close: %v", err)
	}
	// An abandoned engine is closed too.
	ea := mustEngine(t, newFS(t), EngineOptions{Frames: 8})
	if err := ea.Abandon(); err != nil {
		t.Fatal(err)
	}
	if err := ea.Abandon(); !errors.Is(err, ErrClosed) {
		t.Fatalf("abandon twice: %v", err)
	}
	if err := ea.Close(bg); !errors.Is(err, ErrClosed) {
		t.Fatalf("close after abandon: %v", err)
	}
	if err := ea.BeginStatement(bg); !errors.Is(err, ErrClosed) {
		t.Fatalf("begin after abandon: %v", err)
	}
	// The open statement was never committed: reopening discards it.
	e2 := mustEngine(t, m, EngineOptions{Frames: 8})
	if e2.Recovery().DiscardedStatements != 1 {
		t.Fatalf("%+v", e2.Recovery())
	}
	if err := e2.Close(bg); err != nil {
		t.Fatal(err)
	}
}

func TestAbandonedStatementStaysDiscarded(t *testing.T) {
	// An abandoned group, then a reopen whose end-of-recovery checkpoint
	// appends its record but fails to write the control file: the next
	// recovery starts before the group again and must still discard it,
	// although a checkpoint record, not a begin, follows it.
	m := newFS(t)
	e := mustEngine(t, m, EngineOptions{Frames: 32})
	h, tr := setupHeapAndTree(t, e)
	if err := runStatement(e, h, tr, 1, 5); err != nil {
		t.Fatal(err)
	}
	if err := e.BeginStatement(bg); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if _, err := h.Insert(bg, stmtRow(2, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.w.Flush(bg); err != nil {
		t.Fatal(err)
	}
	if err := e.Abandon(); err != nil {
		t.Fatal(err)
	}
	m.InjectError(vfs.Fault{Op: vfs.OpRename, Name: path.Join(dbDir, ControlFileName+".tmp")})
	if _, err := OpenEngine(bg, m, dbDir, EngineOptions{Frames: 32}); err == nil {
		t.Fatal("open succeeded although the control file could not be written")
	}
	m.ClearFaults()
	e2 := mustEngine(t, m, EngineOptions{Frames: 32})
	defer func() { _ = e2.Close(bg) }()
	h2, _ := e2.OpenHeap(bg, h.FirstPage())
	tr2, _ := e2.OpenBTree(bg, tr.Root())
	rows, keys := countByStatement(t, h2, tr2)
	if rows[1] != 5 || keys[1] != 5 || rows[2] != 0 {
		t.Fatalf("rows %v keys %v (recovery %+v)", rows, keys, e2.Recovery())
	}
}

func TestMismatchedCommitIsCorrupt(t *testing.T) {
	cases := map[string][][2]any{
		"commit without begin":  {{RecordStmtCommit, uint64(32)}},
		"commit of no begin":    {{RecordStmtCommit, uint64(0)}},
		"commit of wrong begin": {{RecordStmtBegin, nil}, {RecordStmtCommit, uint64(12345)}},
		"commit payload short":  {{RecordStmtBegin, nil}, {RecordStmtCommit, []byte{1}}},
	}
	for name, recs := range cases {
		t.Run(name, func(t *testing.T) {
			m := newFS(t)
			e := mustEngine(t, m, EngineOptions{Frames: 8})
			for _, r := range recs {
				var p []byte
				switch v := r[1].(type) {
				case uint64:
					p = binary.LittleEndian.AppendUint64(nil, v)
				case []byte:
					p = v
				}
				if _, err := e.w.Append(bg, r[0].(RecordType), p); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.w.Flush(bg); err != nil {
				t.Fatal(err)
			}
			m.Crash(vfs.CrashOptions{})
			if _, err := OpenEngine(bg, m, dbDir, EngineOptions{Frames: 8}); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("open: %v", err)
			}
		})
	}
}

func TestRecoveryFindsUncommittedGroups(t *testing.T) {
	// Logs of group records alone (B: begin, C: commit of the latest begin,
	// K: checkpoint record), appended after a fresh engine's own records.
	// Discarded counts every record of the dropped groups, their begin
	// records included, up to the record that ends each group.
	cases := []struct {
		log               string
		groups, discarded int
		corrupt           bool // a commit with no open group
	}{
		{"", 0, 0, false},
		{"BC", 0, 0, false},
		{"BCBC", 0, 0, false},
		{"B", 1, 1, false},
		{"BCB", 1, 1, false},
		{"BB", 2, 2, false},
		{"BBC", 1, 1, false},
		{"BKBC", 1, 1, false},
		{"BBBC", 2, 2, false},
		{"BKBKB", 3, 3, false},
		{"BCBBCB", 2, 2, false},
		{"KBK", 1, 1, false},
		{"BCC", 0, 0, true},
		{"BKC", 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.log, func(t *testing.T) {
			m := newFS(t)
			e := mustEngine(t, m, EngineOptions{Frames: 8})
			var begin LSN
			for _, r := range c.log {
				var err error
				switch r {
				case 'B':
					begin, err = e.w.Append(bg, RecordStmtBegin, nil)
				case 'C':
					_, err = e.w.Append(bg, RecordStmtCommit, binary.LittleEndian.AppendUint64(nil, uint64(begin)))
				case 'K':
					_, err = e.w.Append(bg, RecordCheckpoint, []byte("not a real checkpoint"))
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := e.w.Flush(bg); err != nil {
				t.Fatal(err)
			}
			m.Crash(vfs.CrashOptions{})
			if c.corrupt {
				if _, err := OpenEngine(bg, m, dbDir, EngineOptions{Frames: 8}); !errors.Is(err, ErrCorrupt) {
					t.Fatalf("open: %v", err)
				}
				return
			}
			e2 := mustEngine(t, m, EngineOptions{Frames: 8})
			defer func() { _ = e2.Close(bg) }()
			rec := e2.Recovery()
			if rec.DiscardedStatements != c.groups || rec.Discarded != c.discarded {
				t.Fatalf("recovery %+v: want %d groups and %d records discarded", rec, c.groups, c.discarded)
			}
		})
	}
}

func TestStatementsAreAllOrNothingAcrossCrashes(t *testing.T) {
	base := testSeed(t)
	runs := 60
	if testing.Short() {
		runs = 10
	}
	var discarded, torn, partialCommits int
	for run := range runs {
		seed := base + uint64(run)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 31))
			m := vfs.NewMemFS(seed)
			_ = m.MkdirAll("/db")
			_ = m.SyncDir("/")
			opts := EngineOptions{Frames: 24 + rng.IntN(40), WAL: Options{SegmentSize: []int64{8192, 1 << 20}[rng.IntN(2)]}}
			e := mustEngine(t, m, opts)
			h, tr := setupHeapAndTree(t, e)
			acked := map[int]int{} // statement -> rows
			stmt := 0
			for cycle := range 3 {
				for range 1 + rng.IntN(12) {
					stmt++
					n := 1 + rng.IntN(12)
					if rng.IntN(4) == 0 {
						if _, err := e.Checkpoint(bg); err != nil {
							t.Fatal(err)
						}
					}
					if err := runStatement(e, h, tr, stmt, n); err != nil {
						t.Fatalf("statement %d: %v", stmt, err)
					}
					acked[stmt] = n
				}
				// A statement in flight when the crash comes.
				stmt++
				inFlight := stmt
				if err := e.BeginStatement(bg); err != nil {
					t.Fatal(err)
				}
				for i := range rng.IntN(15) {
					if _, err := h.Insert(bg, stmtRow(inFlight, i)); err != nil {
						t.Fatal(err)
					}
					if err := tr.Insert(bg, stmtRow(inFlight, i), nil); err != nil {
						t.Fatal(err)
					}
				}
				if rng.IntN(2) == 0 {
					_ = e.w.Flush(bg) // its records durable, the commit not
				}
				opt := vfs.CrashOptions{TearLast: rng.IntN(2) == 0}
				if opt.TearLast {
					torn++
				}
				m.Crash(opt)
				e = mustEngine(t, m, opts)
				if e.Recovery().DiscardedStatements > 0 {
					discarded++
				}
				var err error
				if h, err = e.OpenHeap(bg, h.FirstPage()); err != nil {
					t.Fatal(err)
				}
				if tr, err = e.OpenBTree(bg, tr.Root()); err != nil {
					t.Fatal(err)
				}
				rows, keys := countByStatement(t, h, tr)
				for s, n := range acked {
					if rows[s] != n || keys[s] != n {
						t.Fatalf("cycle %d: acknowledged statement %d has %d rows and %d keys, want %d", cycle, s, rows[s], keys[s], n)
					}
				}
				if rows[inFlight] != 0 || keys[inFlight] != 0 {
					partialCommits++
					t.Fatalf("cycle %d: uncommitted statement %d left %d rows and %d keys", cycle, inFlight, rows[inFlight], keys[inFlight])
				}
				for s := range rows {
					if _, ok := acked[s]; !ok {
						t.Fatalf("rows from statement %d, which never committed", s)
					}
				}
			}
			if err := e.Close(bg); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Logf("%d runs: %d recoveries discarded a statement, %d torn crashes", runs, discarded, torn)
	if runs >= 60 && (discarded < runs/2 || torn < runs/2) {
		t.Fatalf("too few discards (%d) or torn crashes (%d)", discarded, torn)
	}
}
