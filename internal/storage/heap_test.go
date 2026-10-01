package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"slices"
	"sync"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

func newHeapEnv(t testing.TB, frames int) (*env, *Heap) {
	t.Helper()
	e := newEnv(t, frames, nil)
	h, err := CreateHeap(bg, e.bp)
	if err != nil {
		t.Fatalf("CreateHeap: %v", err)
	}
	return e, h
}

// reopen flushes everything, closes the pool and disk manager, reopens the
// file with a fresh pool, and reopens the heap from its first page.
func reopen(t testing.TB, e *env, h *Heap, frames int) (*env, *Heap) {
	t.Helper()
	if err := e.bp.Close(bg); err != nil {
		t.Fatalf("closing pool: %v", err)
	}
	if err := e.dm.Close(); err != nil {
		t.Fatalf("closing disk manager: %v", err)
	}
	dm := mustOpenDM(t, e.fs, dbName)
	spy := &spyStore{PageStore: dm}
	bp, err := NewBufferPool(spy, Options{Frames: frames})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := OpenHeap(bg, bp, h.FirstPage())
	if err != nil {
		t.Fatalf("OpenHeap: %v", err)
	}
	return &env{bp: bp, spy: spy, dm: dm, fs: e.fs}, h2
}

// scanAll returns every row of the heap, failing on duplicate RIDs.
func scanAll(t testing.TB, h *Heap) map[RID][]byte {
	t.Helper()
	got := map[RID][]byte{}
	s := h.Scan()
	for {
		rid, data, ok, err := s.Next(bg)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if !ok {
			return got
		}
		if _, dup := got[rid]; dup {
			t.Fatalf("scan returned %s twice", rid)
		}
		got[rid] = data
	}
}

func sameRows(t testing.TB, got, want map[RID][]byte, ctx string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d rows, want %d", ctx, len(got), len(want))
	}
	for rid, w := range want {
		g, ok := got[rid]
		if !ok {
			t.Fatalf("%s: row %s missing", ctx, rid)
		}
		if !bytes.Equal(g, w) {
			t.Fatalf("%s: row %s content differs (len %d vs %d)", ctx, rid, len(g), len(w))
		}
	}
}

// checkFSM verifies that, single-threaded, every free-space hint is exact and
// the page index is consistent.
func (h *Heap) checkFSM(t testing.TB) {
	t.Helper()
	h.mu.Lock()
	pages := slices.Clone(h.pages)
	idx := h.index
	h.mu.Unlock()
	if len(idx) != len(pages) {
		t.Fatalf("index has %d entries for %d pages", len(idx), len(pages))
	}
	for i, p := range pages {
		if idx[p.id] != i {
			t.Fatalf("index[%d] = %d, want %d", p.id, idx[p.id], i)
		}
		err := withPage(bg, h.bp, p.id, false, func(sp *SlottedPage) (bool, error) {
			if err := sp.Validate(); err != nil {
				return false, err
			}
			if got := sp.FreeSpace(); got != p.free {
				t.Fatalf("page %d: hint %d, actual %d", p.id, p.free, got)
			}
			wantNext := uint64(0)
			if i+1 < len(pages) {
				wantNext = pages[i+1].id
			}
			if sp.NextPage() != wantNext {
				t.Fatalf("page %d: next %d, chain says %d", p.id, sp.NextPage(), wantNext)
			}
			return false, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func rowBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.UintN(256))
	}
	return b
}

func TestHeapInsertGet(t *testing.T) {
	e, h := newHeapEnv(t, 8)
	rng := rand.New(rand.NewPCG(1, 1))
	want := map[RID][]byte{}
	for i := range 50 {
		d := rowBytes(rng, 1+i*7)
		rid, err := h.Insert(bg, d)
		if err != nil {
			t.Fatal(err)
		}
		want[rid] = d
	}
	for rid, d := range want {
		got, err := h.Get(bg, rid)
		if err != nil || !bytes.Equal(got, d) {
			t.Fatalf("Get(%s): %v", rid, err)
		}
	}
	// Get returns a copy: changing it must not change the heap.
	for rid, d := range want {
		got, _ := h.Get(bg, rid)
		got[0] ^= 0xFF
		again, _ := h.Get(bg, rid)
		if !bytes.Equal(again, d) {
			t.Fatal("Get result aliases heap storage")
		}
		break
	}
	sameRows(t, scanAll(t, h), want, "scan")
	h.checkFSM(t)
	e.bp.checkInvariants(t, 0)
}

func TestHeapEmpty(t *testing.T) {
	e, h := newHeapEnv(t, 4)
	if rows := scanAll(t, h); len(rows) != 0 {
		t.Fatalf("empty heap scanned %d rows", len(rows))
	}
	if h.NumPages() != 1 {
		t.Fatalf("NumPages = %d", h.NumPages())
	}
	if _, err := h.Get(bg, RID{Page: h.FirstPage(), Slot: 0}); !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("Get on empty heap err = %v", err)
	}
	e.bp.checkInvariants(t, 0)
}

func TestHeapRejectsBadTuplesAndRIDs(t *testing.T) {
	e, h := newHeapEnv(t, 4)
	rid, err := h.Insert(bg, []byte("row"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Insert(bg, nil); !errors.Is(err, ErrEmptyTuple) {
		t.Errorf("Insert(nil) err = %v", err)
	}
	if _, err := h.Insert(bg, make([]byte, MaxTupleSize+1)); !errors.Is(err, ErrTupleTooLarge) {
		t.Errorf("oversize err = %v", err)
	}
	if _, err := h.Update(bg, rid, nil); !errors.Is(err, ErrEmptyTuple) {
		t.Errorf("Update(nil) err = %v", err)
	}
	if _, err := h.Update(bg, rid, make([]byte, MaxTupleSize+1)); !errors.Is(err, ErrTupleTooLarge) {
		t.Errorf("Update oversize err = %v", err)
	}
	other := e.newStamped(t, 1) // a heap-type page that is not part of this heap
	for _, bad := range []RID{{Page: 0}, {Page: 1}, {Page: other}, {Page: 1 << 40}} {
		if _, err := h.Get(bg, bad); !errors.Is(err, ErrInvalidRID) {
			t.Errorf("Get(%s) err = %v", bad, err)
		}
		if err := h.Delete(bg, bad); !errors.Is(err, ErrInvalidRID) {
			t.Errorf("Delete(%s) err = %v", bad, err)
		}
		if _, err := h.Update(bg, bad, []byte{1}); !errors.Is(err, ErrInvalidRID) {
			t.Errorf("Update(%s) err = %v", bad, err)
		}
	}
	for _, bad := range []RID{{Page: rid.Page, Slot: rid.Slot + 1}, {Page: rid.Page, Slot: 65535}} {
		if _, err := h.Get(bg, bad); !errors.Is(err, ErrSlotNotFound) {
			t.Errorf("Get(%s) err = %v", bad, err)
		}
		if err := h.Delete(bg, bad); !errors.Is(err, ErrSlotNotFound) {
			t.Errorf("Delete(%s) err = %v", bad, err)
		}
		if _, err := h.Update(bg, bad, []byte{1}); !errors.Is(err, ErrSlotNotFound) {
			t.Errorf("Update(%s) err = %v", bad, err)
		}
	}
	e.bp.checkInvariants(t, 0)
}

func TestHeapGrowsAcrossPages(t *testing.T) {
	e, h := newHeapEnv(t, 4)
	const rowSize, rows = 1000, 100
	want := map[RID][]byte{}
	for i := range rows {
		d := bytes.Repeat([]byte{byte(i)}, rowSize)
		rid, err := h.Insert(bg, d)
		if err != nil {
			t.Fatal(err)
		}
		want[rid] = d
	}
	perPage := slotAreaSize / (rowSize + slotSize)
	if wantPages := (rows + perPage - 1) / perPage; h.NumPages() != wantPages {
		t.Fatalf("NumPages = %d, want %d (%d rows per page)", h.NumPages(), wantPages, perPage)
	}
	sameRows(t, scanAll(t, h), want, "scan")
	h.checkFSM(t)
	e.bp.checkInvariants(t, 0)
}

func TestHeapMaxSizeRowsOnePerPage(t *testing.T) {
	_, h := newHeapEnv(t, 4)
	want := map[RID][]byte{}
	for i := range 6 {
		d := bytes.Repeat([]byte{byte(i + 1)}, MaxTupleSize)
		rid, err := h.Insert(bg, d)
		if err != nil {
			t.Fatal(err)
		}
		want[rid] = d
	}
	if h.NumPages() != 6 {
		t.Fatalf("NumPages = %d, want 6", h.NumPages())
	}
	sameRows(t, scanAll(t, h), want, "scan")
	h.checkFSM(t)
}

func TestHeapDeleteReusesSpaceAndSlots(t *testing.T) {
	_, h := newHeapEnv(t, 4)
	var rids []RID
	for range 30 {
		rid, err := h.Insert(bg, bytes.Repeat([]byte{1}, 2000))
		if err != nil {
			t.Fatal(err)
		}
		rids = append(rids, rid)
	}
	pages := h.NumPages()
	for _, rid := range rids[:10] {
		if err := h.Delete(bg, rid); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Get(bg, rid); !errors.Is(err, ErrSlotNotFound) {
			t.Fatalf("deleted row still readable: %v", err)
		}
		if err := h.Delete(bg, rid); !errors.Is(err, ErrSlotNotFound) {
			t.Fatalf("double delete err = %v", err)
		}
	}
	for range 10 {
		if _, err := h.Insert(bg, bytes.Repeat([]byte{2}, 2000)); err != nil {
			t.Fatal(err)
		}
	}
	if h.NumPages() != pages {
		t.Fatalf("heap grew from %d to %d pages although 10 rows were freed", pages, h.NumPages())
	}
	h.checkFSM(t)
}

func TestHeapUpdate(t *testing.T) {
	_, h := newHeapEnv(t, 6)
	small, err := h.Insert(bg, []byte("small"))
	if err != nil {
		t.Fatal(err)
	}
	// Fill the rest of the page with a neighbour so growth cannot fit there.
	neighbour, err := h.Insert(bg, bytes.Repeat([]byte{9}, h.pages[0].free-slotSize))
	if err != nil {
		t.Fatal(err)
	}
	if neighbour.Page != small.Page {
		t.Fatal("setup: neighbour on another page")
	}

	// Smaller or equal: always in place, same RID.
	for _, d := range [][]byte{[]byte("tiny"), []byte("fivee"), []byte("x")} {
		got, err := h.Update(bg, small, d)
		if err != nil || got != small {
			t.Fatalf("shrinking update: %s, %v", got, err)
		}
	}
	// Grows beyond the page: moves, returns a new RID, old RID is gone.
	big := bytes.Repeat([]byte{7}, 500)
	moved, err := h.Update(bg, small, big)
	if err != nil {
		t.Fatal(err)
	}
	if moved == small || moved.Page == small.Page {
		t.Fatalf("row did not move: %s -> %s", small, moved)
	}
	if _, err := h.Get(bg, small); !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("old RID still readable: %v", err)
	}
	if got, err := h.Get(bg, moved); err != nil || !bytes.Equal(got, big) {
		t.Fatalf("moved row: %v", err)
	}
	// The neighbour is untouched, and the scan sees each row exactly once.
	rows := scanAll(t, h)
	if len(rows) != 2 || !bytes.Equal(rows[moved], big) {
		t.Fatalf("scan after move: %d rows", len(rows))
	}
	// Update of a dead RID.
	if _, err := h.Update(bg, small, []byte("z")); !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("update dead row err = %v", err)
	}
	h.checkFSM(t)
}

func TestHeapScanOrderAndConcurrentAppend(t *testing.T) {
	_, h := newHeapEnv(t, 4)
	for i := range 40 {
		if _, err := h.Insert(bg, bytes.Repeat([]byte{byte(i)}, 700)); err != nil {
			t.Fatal(err)
		}
	}
	// Rows added while a scan is in progress, on pages the scan has not
	// reached yet, are seen; the scan never repeats a row or fails.
	s := h.Scan()
	seen := map[RID]bool{}
	for i := 0; ; i++ {
		rid, _, ok, err := s.Next(bg)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		if seen[rid] {
			t.Fatalf("row %s seen twice", rid)
		}
		seen[rid] = true
		if i == 5 {
			for range 10 {
				if _, err := h.Insert(bg, bytes.Repeat([]byte{0xEE}, 3000)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if len(seen) < 40 {
		t.Fatalf("scan saw only %d rows", len(seen))
	}
	// A finished scanner stays finished.
	if _, _, ok, err := s.Next(bg); ok || err != nil {
		t.Fatalf("Next after end: ok=%v err=%v", ok, err)
	}
}

func TestOpenHeapAfterReopen(t *testing.T) {
	frames := 6
	e, h := newHeapEnv(t, frames)
	rng := rand.New(rand.NewPCG(testSeed(t), 9))
	want := map[RID][]byte{}
	for range 300 {
		d := rowBytes(rng, 1+rng.IntN(1500))
		rid, err := h.Insert(bg, d)
		if err != nil {
			t.Fatal(err)
		}
		want[rid] = d
	}
	pages := h.NumPages()
	e, h = reopen(t, e, h, frames)
	if h.NumPages() != pages {
		t.Fatalf("reopened with %d pages, had %d", h.NumPages(), pages)
	}
	sameRows(t, scanAll(t, h), want, "after reopen")
	h.checkFSM(t)
	// The reopened heap keeps working: insert, then reopen once more.
	rid, err := h.Insert(bg, []byte("after reopen"))
	if err != nil {
		t.Fatal(err)
	}
	want[rid] = []byte("after reopen")
	_, h = reopen(t, e, h, frames)
	sameRows(t, scanAll(t, h), want, "after second reopen")
}

func TestOpenHeapRejectsDamage(t *testing.T) {
	build := func(t *testing.T) (*env, *Heap) {
		e, h := newHeapEnv(t, 8)
		for range 12 {
			if _, err := h.Insert(bg, bytes.Repeat([]byte{5}, 3000)); err != nil {
				t.Fatal(err)
			}
		}
		if h.NumPages() < 4 {
			t.Fatalf("setup: only %d pages", h.NumPages())
		}
		if err := e.bp.FlushAll(bg); err != nil {
			t.Fatal(err)
		}
		if err := e.dm.Sync(bg); err != nil {
			t.Fatal(err)
		}
		return e, h
	}
	reload := func(t *testing.T, e *env, first uint64) error {
		bp, err := NewBufferPool(e.dm, Options{Frames: 8}) // cold pool over the same file
		if err != nil {
			t.Fatal(err)
		}
		_, err = OpenHeap(bg, bp, first)
		return err
	}
	// rewrite replaces page id with a sealed copy edited by f.
	rewrite := func(t *testing.T, e *env, id uint64, f func(buf []byte)) {
		buf := readRawFile(t, e.fs, id)
		f(buf)
		if err := Seal(buf); err != nil {
			t.Fatal(err)
		}
		writeRawFile(t, e.fs, id, buf)
	}

	t.Run("bad checksum", func(t *testing.T) {
		e, h := build(t)
		raw := readRawFile(t, e.fs, h.pages[1].id)
		raw[500] ^= 1
		writeRawFile(t, e.fs, h.pages[1].id, raw)
		if err := reload(t, e, h.FirstPage()); !errors.Is(err, ErrChecksum) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("cycle", func(t *testing.T) {
		e, h := build(t)
		last := h.pages[len(h.pages)-1].id
		rewrite(t, e, last, func(b []byte) { binary.LittleEndian.PutUint64(b[heapOffNext:], h.pages[1].id) })
		if err := reload(t, e, h.FirstPage()); !errors.Is(err, ErrCorruptHeap) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("self loop", func(t *testing.T) {
		e, h := build(t)
		rewrite(t, e, h.FirstPage(), func(b []byte) { binary.LittleEndian.PutUint64(b[heapOffNext:], h.FirstPage()) })
		if err := reload(t, e, h.FirstPage()); !errors.Is(err, ErrCorruptHeap) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("chain reaches a non-heap page", func(t *testing.T) {
		e, h := build(t)
		other, err := e.dm.Allocate(bg)
		if err != nil {
			t.Fatal(err)
		}
		page := make([]byte, PageSize)
		if err := InitPage(page, Header{ID: other, Type: PageTypeBTreeLeaf}); err != nil {
			t.Fatal(err)
		}
		if err := e.dm.WritePage(bg, other, page); err != nil {
			t.Fatal(err)
		}
		last := h.pages[len(h.pages)-1].id
		rewrite(t, e, last, func(b []byte) { binary.LittleEndian.PutUint64(b[heapOffNext:], other) })
		if err := reload(t, e, h.FirstPage()); !errors.Is(err, ErrCorruptPage) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("chain reaches an unwritten page", func(t *testing.T) {
		e, h := build(t)
		other, err := e.dm.Allocate(bg)
		if err != nil {
			t.Fatal(err)
		}
		last := h.pages[len(h.pages)-1].id
		rewrite(t, e, last, func(b []byte) { binary.LittleEndian.PutUint64(b[heapOffNext:], other) })
		if err := reload(t, e, h.FirstPage()); !errors.Is(err, ErrZeroPage) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("structurally corrupt page with valid checksum", func(t *testing.T) {
		e, h := build(t)
		rewrite(t, e, h.pages[2].id, func(b []byte) { binary.LittleEndian.PutUint16(b[heapOffNumSlots:], 60000) })
		if err := reload(t, e, h.FirstPage()); !errors.Is(err, ErrCorruptPage) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("overlapping tuples with valid checksum", func(t *testing.T) {
		e, h := build(t)
		rewrite(t, e, h.pages[1].id, func(b []byte) {
			// slot 1 now points into slot 0's tuple
			off0 := binary.LittleEndian.Uint16(b[slotsOffset:])
			binary.LittleEndian.PutUint16(b[slotsOffset+slotSize:], off0+10)
		})
		if err := reload(t, e, h.FirstPage()); !errors.Is(err, ErrCorruptPage) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("first page invalid", func(t *testing.T) {
		e, _ := build(t)
		for _, first := range []uint64{0, 1, 9999} {
			if err := reload(t, e, first); err == nil {
				t.Errorf("OpenHeap(%d) succeeded", first)
			}
		}
	})
	t.Run("healthy heap opens", func(t *testing.T) {
		e, h := build(t)
		if err := reload(t, e, h.FirstPage()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestHeapFaultsLeaveNoPins(t *testing.T) {
	e, h := newHeapEnv(t, 3)
	rid, err := h.Insert(bg, []byte("row"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.bp.FlushAll(bg); err != nil {
		t.Fatal(err)
	}
	for range 5 { // evict the heap page
		e.newStamped(t, 0)
	}
	e.spy.set(&e.spy.failRead, errBoom)
	if _, err := h.Get(bg, rid); !errors.Is(err, errBoom) {
		t.Fatalf("Get err = %v", err)
	}
	if _, err := h.Insert(bg, []byte("x")); !errors.Is(err, errBoom) {
		t.Fatalf("Insert err = %v", err)
	}
	if err := h.Delete(bg, rid); !errors.Is(err, errBoom) {
		t.Fatalf("Delete err = %v", err)
	}
	if _, err := h.Update(bg, rid, []byte("y")); !errors.Is(err, errBoom) {
		t.Fatalf("Update err = %v", err)
	}
	if _, _, _, err := h.Scan().Next(bg); !errors.Is(err, errBoom) {
		t.Fatalf("Scan err = %v", err)
	}
	if _, err := OpenHeap(bg, e.bp, h.FirstPage()); !errors.Is(err, errBoom) {
		t.Fatalf("OpenHeap err = %v", err)
	}
	e.bp.checkInvariants(t, 0)
	e.spy.set(&e.spy.failRead, nil)
	if got, err := h.Get(bg, rid); err != nil || string(got) != "row" {
		t.Fatalf("heap unusable after faults: %v", err)
	}
}

func TestHeapGrowFailures(t *testing.T) {
	e, h := newHeapEnv(t, 3)
	fill := bytes.Repeat([]byte{1}, MaxTupleSize)
	if _, err := h.Insert(bg, fill); err != nil { // first page full
		t.Fatal(err)
	}
	pages := h.NumPages()

	e.spy.set(&e.spy.failAlloc, errBoom)
	if _, err := h.Insert(bg, fill); !errors.Is(err, errBoom) {
		t.Fatalf("allocation failure err = %v", err)
	}
	e.spy.set(&e.spy.failAlloc, nil)

	// A failure while linking the new page (reading the tail) must not leave
	// a dangling page in the heap.
	if err := e.bp.FlushAll(bg); err != nil {
		t.Fatal(err)
	}
	e.newStamped(t, 1)
	e.newStamped(t, 2)
	e.newStamped(t, 3) // the tail is evicted from the pool
	e.spy.set(&e.spy.failRead, errBoom)
	if _, err := h.Insert(bg, fill); !errors.Is(err, errBoom) {
		t.Fatalf("link failure err = %v", err)
	}
	e.spy.set(&e.spy.failRead, nil)
	if h.NumPages() != pages {
		t.Fatalf("failed grow changed page count: %d -> %d", pages, h.NumPages())
	}
	e.bp.checkInvariants(t, 0)
	if _, err := h.Insert(bg, fill); err != nil {
		t.Fatalf("heap unusable after failed grow: %v", err)
	}
	h.checkFSM(t)
}

func TestHeapCancelledContext(t *testing.T) {
	_, h := newHeapEnv(t, 4)
	rid, _ := h.Insert(bg, []byte("a"))
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := h.Insert(ctx, []byte("b")); !errors.Is(err, context.Canceled) {
		t.Errorf("Insert err = %v", err)
	}
	if _, err := h.Get(ctx, rid); !errors.Is(err, context.Canceled) {
		t.Errorf("Get err = %v", err)
	}
	if err := h.Delete(ctx, rid); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete err = %v", err)
	}
	if _, _, _, err := h.Scan().Next(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Scan err = %v", err)
	}
}

// --- model-based heap test ---------------------------------------------------

func heapRowSize(rng *rand.Rand) int {
	switch r := rng.IntN(100); {
	case r < 50:
		return 1 + rng.IntN(100)
	case r < 85:
		return 1 + rng.IntN(1500)
	case r < 97:
		return 1 + rng.IntN(4000)
	default:
		return MaxTupleSize - rng.IntN(30)
	}
}

func runHeapModel(t *testing.T, seed uint64, steps int) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, seed+21))
	frames := 3 + rng.IntN(5)
	e, h := newHeapEnv(t, frames)
	model := map[RID][]byte{}
	pick := func() (RID, bool) {
		if len(model) == 0 {
			return RID{}, false
		}
		rids := make([]RID, 0, len(model))
		for r := range model {
			rids = append(rids, r)
		}
		slices.SortFunc(rids, func(a, b RID) int {
			if a.Page != b.Page {
				return int(a.Page) - int(b.Page)
			}
			return int(a.Slot) - int(b.Slot)
		})
		return rids[rng.IntN(len(rids))], true
	}
	for step := range steps {
		ctx := fmt.Sprintf("seed %d step %d", seed, step)
		switch op := rng.IntN(100); {
		case op < 35:
			d := rowBytes(rng, heapRowSize(rng))
			rid, err := h.Insert(bg, d)
			if err != nil {
				t.Fatalf("%s: Insert: %v", ctx, err)
			}
			if _, dup := model[rid]; dup {
				t.Fatalf("%s: Insert returned live RID %s", ctx, rid)
			}
			model[rid] = d
		case op < 60:
			rid, ok := pick()
			if !ok {
				continue
			}
			d := rowBytes(rng, heapRowSize(rng))
			old := model[rid]
			newRID, err := h.Update(bg, rid, d)
			if err != nil {
				t.Fatalf("%s: Update(%s): %v", ctx, rid, err)
			}
			if len(d) <= len(old) && newRID != rid {
				t.Fatalf("%s: shrinking update moved the row %s -> %s", ctx, rid, newRID)
			}
			if newRID != rid {
				if _, taken := model[newRID]; taken {
					t.Fatalf("%s: Update moved row onto live RID %s", ctx, newRID)
				}
				if _, err := h.Get(bg, rid); !errors.Is(err, ErrSlotNotFound) {
					t.Fatalf("%s: old RID %s still readable (%v)", ctx, rid, err)
				}
				delete(model, rid)
			}
			model[newRID] = d
		case op < 80:
			rid, ok := pick()
			if !ok {
				continue
			}
			if err := h.Delete(bg, rid); err != nil {
				t.Fatalf("%s: Delete(%s): %v", ctx, rid, err)
			}
			delete(model, rid)
			if _, err := h.Get(bg, rid); !errors.Is(err, ErrSlotNotFound) {
				t.Fatalf("%s: deleted row readable: %v", ctx, err)
			}
		case op < 92:
			if rid, ok := pick(); ok {
				got, err := h.Get(bg, rid)
				if err != nil || !bytes.Equal(got, model[rid]) {
					t.Fatalf("%s: Get(%s): %v", ctx, rid, err)
				}
			}
			// A RID the model does not hold must not resolve.
			probe := RID{Page: h.FirstPage(), Slot: uint16(rng.IntN(200))}
			if _, live := model[probe]; !live {
				if _, err := h.Get(bg, probe); !errors.Is(err, ErrSlotNotFound) {
					t.Fatalf("%s: Get(%s) err = %v for a row not in the model", ctx, probe, err)
				}
			}
		case op < 98:
			sameRows(t, scanAll(t, h), model, ctx+" scan")
			h.checkFSM(t)
		default:
			e, h = reopen(t, e, h, frames)
			sameRows(t, scanAll(t, h), model, ctx+" after reopen")
		}
		e.bp.checkInvariants(t, 0)
	}
	sameRows(t, scanAll(t, h), model, fmt.Sprintf("seed %d final", seed))
	h.checkFSM(t)
}

func TestHeapModelBased(t *testing.T) {
	base := testSeed(t)
	runs, steps := 25, 700
	if testing.Short() {
		runs = 4
	}
	for i := range runs {
		runHeapModel(t, base+uint64(i), steps)
	}
}

// --- concurrency --------------------------------------------------------------

// retryBusy retries while the pool is momentarily out of unpinned frames.
func retryBusy[T any](fn func() (T, error)) (T, error) {
	for {
		v, err := fn()
		if !errors.Is(err, ErrNoFreeFrames) {
			return v, err
		}
		runtime.Gosched()
	}
}

// concRow builds a row whose bytes after the 9-byte prefix all equal the low
// byte of its version, so any torn or mixed read is detectable.
func concRow(owner byte, version uint64, size int) []byte {
	b := bytes.Repeat([]byte{byte(version)}, max(size, 9))
	b[0] = owner
	binary.LittleEndian.PutUint64(b[1:], version)
	return b
}

func checkConcRow(t testing.TB, b []byte, owners int) {
	t.Helper()
	if len(b) < 9 || int(b[0]) >= owners {
		t.Errorf("malformed row: len %d owner %d", len(b), b[0])
		return
	}
	version := binary.LittleEndian.Uint64(b[1:])
	for i, c := range b[9:] {
		if c != byte(version) {
			t.Errorf("torn row: byte %d is %d, version %d", 9+i, c, version)
			return
		}
	}
}

func runHeapConcurrent(t *testing.T, frames, writers, scanners, opsPer int) {
	t.Helper()
	e, h := newHeapEnv(t, frames)
	models := make([]map[RID][]byte, writers)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var scanWG sync.WaitGroup
	for range scanners {
		scanWG.Add(1)
		go func() {
			defer scanWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := h.Scan()
				for {
					_, data, ok, err := retryRow(s)
					if err != nil {
						t.Errorf("scan: %v", err)
						return
					}
					if !ok {
						break
					}
					checkConcRow(t, data, writers)
				}
			}
		}()
	}
	for w := range writers {
		models[w] = map[RID][]byte{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w), 77))
			m := models[w]
			var version uint64
			for range opsPer {
				var rids []RID
				for r := range m {
					rids = append(rids, r)
				}
				slices.SortFunc(rids, func(a, b RID) int {
					if a.Page != b.Page {
						return int(a.Page) - int(b.Page)
					}
					return int(a.Slot) - int(b.Slot)
				})
				version++
				switch op := rng.IntN(10); {
				case op < 4 || len(rids) == 0:
					d := concRow(byte(w), version, 9+rng.IntN(heapRowSize(rng)))
					d = d[:min(len(d), MaxTupleSize)]
					rid, err := retryBusy(func() (RID, error) { return h.Insert(bg, d) })
					if err != nil {
						t.Errorf("insert: %v", err)
						return
					}
					if _, dup := m[rid]; dup {
						t.Errorf("insert returned RID %s that this writer already owns", rid)
						return
					}
					m[rid] = d
				case op < 7:
					rid := rids[rng.IntN(len(rids))]
					d := concRow(byte(w), version, 9+rng.IntN(heapRowSize(rng)))
					d = d[:min(len(d), MaxTupleSize)]
					nr, err := retryBusy(func() (RID, error) { return h.Update(bg, rid, d) })
					if err != nil {
						t.Errorf("update: %v", err)
						return
					}
					delete(m, rid)
					m[nr] = d
				case op < 9:
					rid := rids[rng.IntN(len(rids))]
					if _, err := retryBusy(func() (struct{}, error) { return struct{}{}, h.Delete(bg, rid) }); err != nil {
						t.Errorf("delete: %v", err)
						return
					}
					delete(m, rid)
				default:
					rid := rids[rng.IntN(len(rids))]
					got, err := retryBusy(func() ([]byte, error) { return h.Get(bg, rid) })
					if err != nil || !bytes.Equal(got, m[rid]) {
						t.Errorf("get %s: %v (own row changed or lost)", rid, err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	scanWG.Wait()
	if t.Failed() {
		return
	}
	want := map[RID][]byte{}
	for _, m := range models {
		for rid, d := range m {
			if _, dup := want[rid]; dup {
				t.Fatalf("two writers own RID %s", rid)
			}
			want[rid] = d
		}
	}
	sameRows(t, scanAll(t, h), want, "final scan")
	h.checkFSM(t)
	e.bp.checkInvariants(t, 0)
}

// retryRow is Scanner.Next with the busy-pool retry.
func retryRow(s *Scanner) (RID, []byte, bool, error) {
	for {
		rid, d, ok, err := s.Next(bg)
		if !errors.Is(err, ErrNoFreeFrames) {
			return rid, d, ok, err
		}
		runtime.Gosched()
	}
}

func TestHeapConcurrent(t *testing.T)         { runHeapConcurrent(t, 16, 4, 2, 250) }
func TestHeapConcurrentTinyPool(t *testing.T) { runHeapConcurrent(t, 6, 4, 1, 150) }

// Many goroutines read and rewrite the same rows. Rewrites keep the size, so
// they happen in place and RIDs stay valid; readers must never see a torn row.
func TestHeapConcurrentSharedRows(t *testing.T) {
	e, h := newHeapEnv(t, 8)
	const rows, workers, iters, size = 40, 6, 400, 600
	rids := make([]RID, rows)
	for i := range rids {
		rid, err := h.Insert(bg, concRow(0, 0, size))
		if err != nil {
			t.Fatal(err)
		}
		rids[i] = rid
	}
	var wg sync.WaitGroup
	var versions [rows]uint64
	var vmu sync.Mutex
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w), 31))
			for range iters {
				i := rng.IntN(rows)
				if rng.IntN(2) == 0 {
					vmu.Lock()
					versions[i]++
					v := versions[i]
					vmu.Unlock()
					// Writers race: only the final bytes matter, and they
					// must be one writer's complete row.
					got, err := retryBusy(func() (RID, error) { return h.Update(bg, rids[i], concRow(byte(w), v, size)) })
					if err != nil || got != rids[i] {
						t.Errorf("in-place update of %s: moved to %s, %v", rids[i], got, err)
						return
					}
				} else {
					d, err := retryBusy(func() ([]byte, error) { return h.Get(bg, rids[i]) })
					if err != nil {
						t.Errorf("get: %v", err)
						return
					}
					checkConcRow(t, d, workers)
				}
			}
		}()
	}
	wg.Wait()
	rows2 := scanAll(t, h)
	if len(rows2) != rows {
		t.Fatalf("%d rows after the run, want %d", len(rows2), rows)
	}
	for _, d := range rows2 {
		checkConcRow(t, d, workers)
	}
	h.checkFSM(t)
	e.bp.checkInvariants(t, 0)
}

// The heap must work with the smallest pools: it never needs more than one
// page pinned at a time.
func TestHeapWithOneAndTwoFrames(t *testing.T) {
	for _, frames := range []int{1, 2} {
		t.Run(fmt.Sprintf("frames=%d", frames), func(t *testing.T) {
			e, h := newHeapEnv(t, frames)
			rng := rand.New(rand.NewPCG(uint64(frames), 4))
			want := map[RID][]byte{}
			for range 200 {
				d := rowBytes(rng, 1+rng.IntN(2500))
				rid, err := h.Insert(bg, d)
				if err != nil {
					t.Fatal(err)
				}
				want[rid] = d
			}
			for rid, d := range want { // update half so some rows move
				if rng.IntN(2) == 0 {
					nd := rowBytes(rng, 1+rng.IntN(3000))
					nr, err := h.Update(bg, rid, nd)
					if err != nil {
						t.Fatal(err)
					}
					delete(want, rid)
					want[nr] = nd
					_ = d
				}
			}
			sameRows(t, scanAll(t, h), want, "scan")
			h.checkFSM(t)
			e.bp.checkInvariants(t, 0)
			_, h = reopen(t, e, h, frames)
			sameRows(t, scanAll(t, h), want, "after reopen")
		})
	}
}

// --- crash --------------------------------------------------------------------

// With a pool big enough that nothing is evicted, the only pages that reach
// disk are the ones a checkpoint (FlushAll + Sync) writes. After a crash the
// heap must therefore equal the last checkpoint exactly.
func runHeapCrash(t *testing.T, seed uint64) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, seed+5))
	m := vfs.NewMemFS(seed)
	if err := m.MkdirAll("/db"); err != nil {
		t.Fatal(err)
	}
	if err := m.SyncDir("/"); err != nil {
		t.Fatal(err)
	}
	dm := mustCreate(t, m, dbName)
	bp, err := NewBufferPool(dm, Options{Frames: 512})
	if err != nil {
		t.Fatal(err)
	}
	h, err := CreateHeap(bg, bp)
	if err != nil {
		t.Fatal(err)
	}
	first := h.FirstPage()
	if err := bp.FlushAll(bg); err != nil {
		t.Fatal(err)
	}
	if err := dm.Sync(bg); err != nil {
		t.Fatal(err)
	}

	model := map[RID][]byte{}
	checkpoint := map[RID][]byte{}
	for range 20 + rng.IntN(120) {
		switch op := rng.IntN(100); {
		case op < 50:
			d := rowBytes(rng, heapRowSize(rng))
			rid, err := h.Insert(bg, d)
			if err != nil {
				t.Fatal(err)
			}
			model[rid] = d
		case op < 70 && len(model) > 0:
			for rid := range model { // any row
				d := rowBytes(rng, heapRowSize(rng))
				nr, err := h.Update(bg, rid, d)
				if err != nil {
					t.Fatal(err)
				}
				delete(model, rid)
				model[nr] = d
				break
			}
		case op < 85 && len(model) > 0:
			for rid := range model {
				if err := h.Delete(bg, rid); err != nil {
					t.Fatal(err)
				}
				delete(model, rid)
				break
			}
		default: // checkpoint
			if err := bp.FlushAll(bg); err != nil {
				t.Fatal(err)
			}
			if err := dm.Sync(bg); err != nil {
				t.Fatal(err)
			}
			checkpoint = map[RID][]byte{}
			for rid, d := range model {
				checkpoint[rid] = d
			}
		}
	}

	m.Crash(vfs.CrashOptions{TearLast: rng.IntN(2) == 0})
	dm2 := mustOpenDM(t, m, dbName)
	bp2, err := NewBufferPool(dm2, Options{Frames: 512})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := OpenHeap(bg, bp2, first)
	if err != nil {
		t.Fatalf("seed %d: OpenHeap after crash: %v", seed, err)
	}
	sameRows(t, scanAll(t, h2), checkpoint, fmt.Sprintf("seed %d after crash", seed))
	h2.checkFSM(t)
}

func TestHeapCrashRecoversLastCheckpoint(t *testing.T) {
	base := testSeed(t)
	runs := 150
	if testing.Short() {
		runs = 20
	}
	for i := range runs {
		runHeapCrash(t, base+uint64(i))
	}
}

// --- benchmarks ----------------------------------------------------------------

func BenchmarkHeapInsert(b *testing.B) {
	e := newEnv(b, 256, nil)
	h, err := CreateHeap(bg, e.bp)
	if err != nil {
		b.Fatal(err)
	}
	row := bytes.Repeat([]byte{1}, 100)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := h.Insert(bg, row); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHeapScan(b *testing.B) {
	e := newEnv(b, 256, nil)
	h, err := CreateHeap(bg, e.bp)
	if err != nil {
		b.Fatal(err)
	}
	row := bytes.Repeat([]byte{1}, 100)
	for range 5000 {
		if _, err := h.Insert(bg, row); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := h.Scan()
		n := 0
		for {
			_, _, ok, err := s.Next(bg)
			if err != nil {
				b.Fatal(err)
			}
			if !ok {
				break
			}
			n++
		}
		if n != 5000 {
			b.Fatalf("scanned %d rows", n)
		}
	}
}
