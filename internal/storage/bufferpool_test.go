package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

var errBoom = errors.New("boom")

// spyStore wraps a real PageStore, counting calls and failing on demand.
type spyStore struct {
	PageStore
	mu                                       sync.Mutex
	reads, writes, allocs, frees             int
	failRead, failWrite, failAlloc, failFree error
	onWrite                                  func() // runs inside the next WritePage, then is cleared
}

func (s *spyStore) ReadPage(ctx context.Context, id uint64, buf []byte) error {
	s.mu.Lock()
	s.reads++
	err := s.failRead
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.PageStore.ReadPage(ctx, id, buf)
}

func (s *spyStore) WritePage(ctx context.Context, id uint64, buf []byte) error {
	s.mu.Lock()
	s.writes++
	err := s.failWrite
	hook := s.onWrite
	s.onWrite = nil
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return err
	}
	return s.PageStore.WritePage(ctx, id, buf)
}

func (s *spyStore) Allocate(ctx context.Context) (uint64, error) {
	s.mu.Lock()
	s.allocs++
	err := s.failAlloc
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return s.PageStore.Allocate(ctx)
}

func (s *spyStore) Free(ctx context.Context, id uint64) error {
	s.mu.Lock()
	s.frees++
	err := s.failFree
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.PageStore.Free(ctx, id)
}

func (s *spyStore) set(field *error, err error) {
	s.mu.Lock()
	*field = err
	s.mu.Unlock()
}

func (s *spyStore) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

type env struct {
	bp  *BufferPool
	spy *spyStore
	dm  *DiskManager
	fs  *vfs.MemFS
}

func newEnv(t testing.TB, frames int, lsn func() uint64) *env {
	t.Helper()
	m := newMem(t)
	dm := mustCreate(t, m, dbName)
	spy := &spyStore{PageStore: dm}
	bp, err := NewBufferPool(spy, Options{Frames: frames, FlushedLSN: lsn})
	if err != nil {
		t.Fatal(err)
	}
	return &env{bp: bp, spy: spy, dm: dm, fs: m}
}

// fillPayload sets every payload byte to stamp.
func fillPayload(r *PageRef, stamp byte) {
	d := r.Data()
	for i := HeaderSize; i < PageSize; i++ {
		d[i] = stamp
	}
}

// payloadStamp returns the stamp if every payload byte is equal.
func payloadStamp(t testing.TB, d []byte) byte {
	t.Helper()
	s := d[HeaderSize]
	for i := HeaderSize; i < PageSize; i++ {
		if d[i] != s {
			t.Fatalf("payload not uniform: byte %d is %d, first is %d", i, d[i], s)
		}
	}
	return s
}

// newStamped creates a page with the given stamp and unpins it dirty.
func (e *env) newStamped(t testing.TB, stamp byte) uint64 {
	t.Helper()
	r, err := e.bp.NewPage(bg, PageTypeHeap)
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	fillPayload(r, stamp)
	id := r.ID()
	if err := r.Unpin(true); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) fetch(t testing.TB, id uint64) *PageRef {
	t.Helper()
	r, err := e.bp.FetchPage(bg, id)
	if err != nil {
		t.Fatalf("FetchPage(%d): %v", id, err)
	}
	return r
}

// checkInvariants verifies the pool's internal consistency.
func (bp *BufferPool) checkInvariants(t testing.TB, wantPins int) {
	t.Helper()
	bp.mu.Lock()
	defer bp.mu.Unlock()
	inFree := map[*frame]bool{}
	for _, f := range bp.free {
		if inFree[f] {
			t.Fatal("frame twice on free list")
		}
		inFree[f] = true
		if f.valid || f.pinCount != 0 {
			t.Fatal("free frame is valid or pinned")
		}
	}
	inTable := map[*frame]bool{}
	pins := 0
	for id, f := range bp.table {
		if !f.valid || f.pageID != id {
			t.Fatalf("table entry %d points to frame holding %d (valid=%v)", id, f.pageID, f.valid)
		}
		if inTable[f] {
			t.Fatalf("frame mapped by two page ids (second: %d)", id)
		}
		if inFree[f] {
			t.Fatalf("frame for page %d is also free", id)
		}
		inTable[f] = true
		pins += f.pinCount
	}
	if len(inTable)+len(inFree) != len(bp.frames) {
		t.Fatalf("%d resident + %d free != %d frames", len(inTable), len(inFree), len(bp.frames))
	}
	if pins != wantPins {
		t.Fatalf("pin count total %d, want %d", pins, wantPins)
	}
}

func (bp *BufferPool) resident() []uint64 {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	var ids []uint64
	for id := range bp.table {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func TestNewBufferPoolValidation(t *testing.T) {
	m := newMem(t)
	dm := mustCreate(t, m, dbName)
	for _, n := range []int{0, -1} {
		if _, err := NewBufferPool(dm, Options{Frames: n}); !errors.Is(err, ErrBadPoolSize) {
			t.Errorf("Frames=%d err = %v", n, err)
		}
	}
	if _, err := NewBufferPool(nil, Options{Frames: 1}); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := NewBufferPool(dm, Options{Frames: 1}); err != nil {
		t.Errorf("one frame rejected: %v", err)
	}
}

func TestNewPageFetchRoundTrip(t *testing.T) {
	e := newEnv(t, 4, nil)
	r, err := e.bp.NewPage(bg, PageTypeBTreeLeaf)
	if err != nil {
		t.Fatal(err)
	}
	h, err := DecodeHeader(r.Data())
	if err != nil || h.ID != r.ID() || h.Type != PageTypeBTreeLeaf {
		t.Fatalf("new page header = %+v, %v", h, err)
	}
	if len(r.Data()) != PageSize {
		t.Fatalf("Data len %d", len(r.Data()))
	}
	fillPayload(r, 9)
	id := r.ID()
	if err := r.Unpin(true); err != nil {
		t.Fatal(err)
	}
	r2 := e.fetch(t, id)
	if payloadStamp(t, r2.Data()) != 9 {
		t.Fatal("content lost")
	}
	if st := e.bp.Stats(); st.Hits != 1 || st.Misses != 0 {
		t.Fatalf("stats = %+v", st)
	}
	_ = r2.Unpin(false)
	e.bp.checkInvariants(t, 0)
}

func TestNewPageRejectsBadTypes(t *testing.T) {
	e := newEnv(t, 2, nil)
	for _, typ := range []PageType{PageTypeInvalid, PageTypeFileHeader, PageTypeFree, 77} {
		if _, err := e.bp.NewPage(bg, typ); !errors.Is(err, ErrBadPageType) {
			t.Errorf("type %d err = %v", typ, err)
		}
	}
	if e.spy.allocs != 0 {
		t.Fatal("bad type still allocated a page")
	}
}

func TestAllFramesPinned(t *testing.T) {
	e := newEnv(t, 2, nil)
	a, err := e.bp.NewPage(bg, PageTypeHeap)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.bp.NewPage(bg, PageTypeHeap)
	if err != nil {
		t.Fatal(err)
	}
	allocs := e.spy.allocs
	if _, err := e.bp.NewPage(bg, PageTypeHeap); !errors.Is(err, ErrNoFreeFrames) {
		t.Fatalf("NewPage err = %v", err)
	}
	if e.spy.allocs != allocs {
		t.Fatal("NewPage allocated although no frame was available")
	}
	if _, err := e.bp.FetchPage(bg, 50); !errors.Is(err, ErrNoFreeFrames) { // not resident
		t.Fatalf("FetchPage err = %v", err)
	}
	e.bp.checkInvariants(t, 2)
	_ = a.Unpin(true)
	c, err := e.bp.NewPage(bg, PageTypeHeap)
	if err != nil {
		t.Fatalf("after unpin: %v", err)
	}
	_ = b.Unpin(true)
	_ = c.Unpin(true)
	e.bp.checkInvariants(t, 0)
}

func TestSingleFramePool(t *testing.T) {
	e := newEnv(t, 1, nil)
	a := e.newStamped(t, 1)
	b := e.newStamped(t, 2) // evicts a, writing it
	r := e.fetch(t, a)      // evicts b
	if payloadStamp(t, r.Data()) != 1 {
		t.Fatal("page a lost")
	}
	_ = r.Unpin(false)
	r = e.fetch(t, b)
	if payloadStamp(t, r.Data()) != 2 {
		t.Fatal("page b lost")
	}
	_ = r.Unpin(false)
}

func TestEvictionWritesDirtyPagesOnly(t *testing.T) {
	e := newEnv(t, 2, nil)
	a := e.newStamped(t, 1)
	e.newStamped(t, 2)
	if w := e.spy.writeCount(); w != 0 {
		t.Fatalf("writes before any eviction = %d", w)
	}
	e.newStamped(t, 3) // evicts one dirty page
	if w := e.spy.writeCount(); w != 1 {
		t.Fatalf("writes after one dirty eviction = %d", w)
	}
	if st := e.bp.Stats(); st.Evictions != 1 || st.Writes != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if err := e.bp.FlushAll(bg); err != nil {
		t.Fatal(err)
	}
	base := e.spy.writeCount()
	// Everything resident is clean now; fetching other pages evicts them
	// without writing.
	for _, id := range []uint64{a, a + 1, a + 2, a} {
		r := e.fetch(t, id)
		_ = r.Unpin(false)
	}
	if w := e.spy.writeCount(); w != base {
		t.Fatalf("clean evictions wrote %d pages", w-base)
	}
	// The evicted dirty page's content really reached the store.
	buf := make([]byte, PageSize)
	if err := e.dm.ReadPage(bg, a, buf); err != nil || payloadStamp(t, buf) != 1 {
		t.Fatalf("page a in store: %v", err)
	}
}

func TestClockGivesSecondChance(t *testing.T) {
	e := newEnv(t, 3, nil)
	p1 := e.newStamped(t, 1) // frame 0
	p2 := e.newStamped(t, 2) // frame 1
	p3 := e.newStamped(t, 3) // frame 2
	p4 := e.newStamped(t, 4) // all ref bits set: hand clears them, evicts frame 0 (p1)
	if got := e.bp.resident(); !slices.Equal(got, []uint64{p2, p3, p4}) {
		t.Fatalf("resident = %v, want %v", got, []uint64{p2, p3, p4})
	}
	// Touch p2 so it gets its reference bit back; p3's bit is still clear.
	r := e.fetch(t, p2)
	_ = r.Unpin(false)
	e.newStamped(t, 5) // hand at frame 1 (p2, referenced -> spared), then frame 2 (p3) is evicted
	got := e.bp.resident()
	if slices.Contains(got, p3) || !slices.Contains(got, p2) {
		t.Fatalf("resident = %v: p3 should be evicted and p2 spared", got)
	}
	_ = p1
}

func TestPinnedPagesNeverEvicted(t *testing.T) {
	e := newEnv(t, 3, nil)
	keep := e.newStamped(t, 7)
	pinned := e.fetch(t, keep)
	for i := range 20 {
		e.newStamped(t, byte(i))
	}
	if !slices.Contains(e.bp.resident(), keep) {
		t.Fatal("pinned page was evicted")
	}
	if payloadStamp(t, pinned.Data()) != 7 {
		t.Fatal("pinned page changed")
	}
	_ = pinned.Unpin(false)
	e.bp.checkInvariants(t, 0)
}

func TestSamePageMultiplePins(t *testing.T) {
	e := newEnv(t, 2, nil)
	id := e.newStamped(t, 5)
	a, b := e.fetch(t, id), e.fetch(t, id)
	if &a.Data()[0] != &b.Data()[0] {
		t.Fatal("two pins of one page see different memory")
	}
	e.bp.checkInvariants(t, 2)
	_ = a.Unpin(false)
	e.bp.checkInvariants(t, 1)
	if err := e.bp.DeletePage(bg, id); !errors.Is(err, ErrPagePinned) {
		t.Fatalf("delete with one pin left err = %v", err)
	}
	_ = b.Unpin(false)
	if err := e.bp.DeletePage(bg, id); err != nil {
		t.Fatal(err)
	}
}

func TestDoubleUnpin(t *testing.T) {
	e := newEnv(t, 2, nil)
	id := e.newStamped(t, 1)
	a, b := e.fetch(t, id), e.fetch(t, id)
	if err := a.Unpin(false); err != nil {
		t.Fatal(err)
	}
	if err := a.Unpin(true); !errors.Is(err, ErrAlreadyUnpinned) {
		t.Fatalf("err = %v", err)
	}
	e.bp.checkInvariants(t, 1) // b's pin is intact
	_ = b.Unpin(false)
	e.bp.checkInvariants(t, 0)
}

func TestDeletePage(t *testing.T) {
	e := newEnv(t, 3, nil)
	// A dirty page that never reached disk is dropped without a write.
	dirty := e.newStamped(t, 1)
	if err := e.bp.DeletePage(bg, dirty); err != nil {
		t.Fatal(err)
	}
	if w := e.spy.writeCount(); w != 0 {
		t.Fatalf("delete wrote %d pages", w)
	}
	if slices.Contains(e.bp.resident(), dirty) || e.spy.frees != 1 {
		t.Fatal("page still resident or not freed in store")
	}
	e.bp.checkInvariants(t, 0)

	// A non-resident page.
	other := e.newStamped(t, 2)
	if err := e.bp.FlushAll(bg); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		e.newStamped(t, 0) // push `other` out
	}
	if err := e.bp.DeletePage(bg, other); err != nil {
		t.Fatalf("non-resident delete: %v", err)
	}
	// Deleted IDs are reused by the next NewPage, with fresh content.
	r, err := e.bp.NewPage(bg, PageTypeHeap)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID() != other && r.ID() != dirty {
		t.Fatalf("new page id %d reuses neither %d nor %d", r.ID(), other, dirty)
	}
	_ = r.Unpin(false)

	// Pinned page, and a store failure.
	p := e.newStamped(t, 3)
	pin := e.fetch(t, p)
	if err := e.bp.DeletePage(bg, p); !errors.Is(err, ErrPagePinned) {
		t.Fatalf("pinned delete err = %v", err)
	}
	_ = pin.Unpin(false)
	e.spy.set(&e.spy.failFree, errBoom)
	if err := e.bp.DeletePage(bg, p); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if !slices.Contains(e.bp.resident(), p) {
		t.Fatal("page left the pool although Free failed")
	}
	e.bp.checkInvariants(t, 0)
}

func TestFlushPage(t *testing.T) {
	e := newEnv(t, 3, nil)
	id := e.newStamped(t, 4)
	if err := e.bp.FlushPage(bg, id); err != nil {
		t.Fatal(err)
	}
	if w := e.spy.writeCount(); w != 1 {
		t.Fatalf("writes = %d", w)
	}
	if err := e.bp.FlushPage(bg, id); err != nil { // clean: no-op
		t.Fatal(err)
	}
	if err := e.bp.FlushPage(bg, 9999); err != nil { // not resident: no-op
		t.Fatal(err)
	}
	if w := e.spy.writeCount(); w != 1 {
		t.Fatalf("no-op flushes wrote: %d", w)
	}
	// Flushing a pinned page works while the pin is held.
	r := e.fetch(t, id)
	r.Lock()
	fillPayload(r, 6)
	r.Unlock()
	_ = r.Unpin(true)
	r = e.fetch(t, id)
	if err := e.bp.FlushPage(bg, id); err != nil {
		t.Fatal(err)
	}
	_ = r.Unpin(false)
	buf := make([]byte, PageSize)
	if err := e.dm.ReadPage(bg, id, buf); err != nil || payloadStamp(t, buf) != 6 {
		t.Fatalf("store has stale page: %v", err)
	}
}

// A change made while an older copy of the page is being written must not be
// lost: the page has to stay dirty so a later flush writes the newer content.
func TestChangeDuringFlushIsNotLost(t *testing.T) {
	e := newEnv(t, 3, nil)
	id := e.newStamped(t, 1)
	e.spy.mu.Lock()
	e.spy.onWrite = func() {
		r := e.fetch(t, id)
		r.Lock()
		fillPayload(r, 2)
		r.Unlock()
		_ = r.Unpin(true)
	}
	e.spy.mu.Unlock()
	if err := e.bp.FlushPage(bg, id); err != nil {
		t.Fatal(err)
	}
	if err := e.bp.FlushAll(bg); err != nil { // must write the newer version
		t.Fatal(err)
	}
	buf := make([]byte, PageSize)
	if err := e.dm.ReadPage(bg, id, buf); err != nil || payloadStamp(t, buf) != 2 {
		t.Fatalf("newer content lost (err %v)", err)
	}
}

func TestFlushFailureKeepsPageDirty(t *testing.T) {
	e := newEnv(t, 3, nil)
	id := e.newStamped(t, 4)
	e.spy.set(&e.spy.failWrite, errBoom)
	if err := e.bp.FlushPage(bg, id); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if err := e.bp.FlushAll(bg); !errors.Is(err, errBoom) {
		t.Fatalf("FlushAll err = %v", err)
	}
	e.spy.set(&e.spy.failWrite, nil)
	if err := e.bp.FlushAll(bg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, PageSize)
	if err := e.dm.ReadPage(bg, id, buf); err != nil || payloadStamp(t, buf) != 4 {
		t.Fatalf("page lost after failed flush: %v", err)
	}
	e.bp.checkInvariants(t, 0)
}

func TestFlushAllJoinsErrors(t *testing.T) {
	e := newEnv(t, 4, nil)
	e.newStamped(t, 1)
	e.newStamped(t, 2)
	e.spy.set(&e.spy.failWrite, errBoom)
	err := e.bp.FlushAll(bg)
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if w := e.spy.writeCount(); w != 2 {
		t.Fatalf("FlushAll stopped after first failure: %d write attempts", w)
	}
}

func TestWriteBackFailureKeepsVictim(t *testing.T) {
	e := newEnv(t, 1, nil)
	a := e.newStamped(t, 1)
	e.spy.set(&e.spy.failWrite, errBoom)
	if _, err := e.bp.NewPage(bg, PageTypeHeap); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v", err)
	}
	if got := e.bp.resident(); !slices.Equal(got, []uint64{a}) {
		t.Fatalf("resident = %v", got)
	}
	e.spy.set(&e.spy.failWrite, nil)
	e.newStamped(t, 2) // now evicts a successfully
	r := e.fetch(t, a)
	if payloadStamp(t, r.Data()) != 1 {
		t.Fatal("page a lost across failed write-back")
	}
	_ = r.Unpin(false)
	e.bp.checkInvariants(t, 0)
}

func TestReadFailureDoesNotLeakFrame(t *testing.T) {
	e := newEnv(t, 2, nil)
	a := e.newStamped(t, 1)
	b := e.newStamped(t, 2)
	if err := e.bp.FlushAll(bg); err != nil {
		t.Fatal(err)
	}
	e.newStamped(t, 3)
	e.newStamped(t, 4) // a and b are out of the pool now
	e.spy.set(&e.spy.failRead, errBoom)
	for range 5 { // more failures than frames: a leak would exhaust the pool
		if _, err := e.bp.FetchPage(bg, a); !errors.Is(err, errBoom) {
			t.Fatalf("err = %v", err)
		}
	}
	e.bp.checkInvariants(t, 0)
	e.spy.set(&e.spy.failRead, nil)
	r := e.fetch(t, a)
	if payloadStamp(t, r.Data()) != 1 {
		t.Fatal("wrong content")
	}
	_ = r.Unpin(false)
	_ = b
}

func TestAllocateAndFreeFailures(t *testing.T) {
	e := newEnv(t, 2, nil)
	e.spy.set(&e.spy.failAlloc, errBoom)
	for range 4 {
		if _, err := e.bp.NewPage(bg, PageTypeHeap); !errors.Is(err, errBoom) {
			t.Fatalf("err = %v", err)
		}
	}
	e.bp.checkInvariants(t, 0)
	e.spy.set(&e.spy.failAlloc, nil)
	e.newStamped(t, 1)
}

func TestFetchUnwrittenAndCorruptPages(t *testing.T) {
	e := newEnv(t, 2, nil)
	id, err := e.dm.Allocate(bg) // allocated, never written
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := e.bp.FetchPage(bg, id); !errors.Is(err, ErrZeroPage) {
			t.Fatalf("err = %v, want ErrZeroPage", err)
		}
	}
	if _, err := e.bp.FetchPage(bg, 1); !errors.Is(err, ErrInvalidPageID) {
		t.Fatalf("header page err = %v", err)
	}
	e.bp.checkInvariants(t, 0)

	good := e.newStamped(t, 3)
	if err := e.bp.FlushAll(bg); err != nil {
		t.Fatal(err)
	}
	raw := readRawFile(t, e.fs, good)
	raw[200] ^= 0xFF
	writeRawFile(t, e.fs, good, raw)
	for range 3 {
		e.newStamped(t, 0) // evict `good`
	}
	if _, err := e.bp.FetchPage(bg, good); !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v, want ErrChecksum", err)
	}
	e.bp.checkInvariants(t, 0)
}

func TestCancelledContext(t *testing.T) {
	e := newEnv(t, 2, nil)
	id := e.newStamped(t, 1)
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := e.bp.FetchPage(ctx, id); !errors.Is(err, context.Canceled) {
		t.Errorf("FetchPage err = %v", err)
	}
	if _, err := e.bp.NewPage(ctx, PageTypeHeap); !errors.Is(err, context.Canceled) {
		t.Errorf("NewPage err = %v", err)
	}
	if err := e.bp.DeletePage(ctx, id); !errors.Is(err, context.Canceled) {
		t.Errorf("DeletePage err = %v", err)
	}
	if err := e.bp.FlushPage(ctx, id); !errors.Is(err, context.Canceled) {
		t.Errorf("FlushPage err = %v", err)
	}
	e.bp.checkInvariants(t, 0)
}

func TestWALRuleHook(t *testing.T) {
	var flushed atomic.Uint64
	e := newEnv(t, 1, flushed.Load)
	r, err := e.bp.NewPage(bg, PageTypeHeap)
	if err != nil {
		t.Fatal(err)
	}
	id := r.ID()
	r.Lock()
	binary.LittleEndian.PutUint64(r.Data()[offLSN:], 20)
	fillPayload(r, 8)
	r.Unlock()
	_ = r.Unpin(true)

	// LSN 20 > flushed 10: neither eviction nor an explicit flush may write.
	flushed.Store(10)
	if _, err := e.bp.NewPage(bg, PageTypeHeap); !errors.Is(err, ErrNoFreeFrames) {
		t.Fatalf("eviction err = %v, want ErrNoFreeFrames", err)
	}
	if err := e.bp.FlushPage(bg, id); !errors.Is(err, ErrWALRule) {
		t.Fatalf("flush err = %v, want ErrWALRule", err)
	}
	if err := e.bp.FlushAll(bg); !errors.Is(err, ErrWALRule) {
		t.Fatalf("FlushAll err = %v", err)
	}
	if w := e.spy.writeCount(); w != 0 {
		t.Fatalf("page written against the WAL rule (%d writes)", w)
	}
	// Exactly equal is allowed.
	flushed.Store(20)
	if err := e.bp.FlushPage(bg, id); err != nil {
		t.Fatalf("flush at equal lsn: %v", err)
	}
	buf := make([]byte, PageSize)
	if err := e.dm.ReadPage(bg, id, buf); err != nil || pageLSN(buf) != 20 {
		t.Fatalf("store page: lsn %d, %v", pageLSN(buf), err)
	}
	// A clean page is evictable whatever its LSN.
	flushed.Store(0)
	if _, err := e.bp.NewPage(bg, PageTypeHeap); err != nil {
		t.Fatalf("clean page with high lsn not evictable: %v", err)
	}
}

func TestClose(t *testing.T) {
	e := newEnv(t, 3, nil)
	id := e.newStamped(t, 5)
	pin := e.fetch(t, id)
	if err := e.bp.Close(bg); !errors.Is(err, ErrPagePinned) {
		t.Fatalf("Close with pin err = %v", err)
	}
	again, err := e.bp.FetchPage(bg, id)
	if err != nil {
		t.Fatalf("pool unusable after refused Close: %v", err)
	}
	_ = again.Unpin(false)
	_ = pin.Unpin(false)

	e.spy.set(&e.spy.failWrite, errBoom)
	if err := e.bp.Close(bg); !errors.Is(err, errBoom) {
		t.Fatalf("Close err = %v", err)
	}
	e.spy.set(&e.spy.failWrite, nil)
	if err := e.bp.Close(bg); err != nil { // retry works, data still there
		t.Fatalf("retry Close: %v", err)
	}
	buf := make([]byte, PageSize)
	if err := e.dm.ReadPage(bg, id, buf); err != nil || payloadStamp(t, buf) != 5 {
		t.Fatalf("data after Close: %v", err)
	}
	if _, err := e.bp.FetchPage(bg, id); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("FetchPage after Close err = %v", err)
	}
	if _, err := e.bp.NewPage(bg, PageTypeHeap); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("NewPage after Close err = %v", err)
	}
	if err := e.bp.FlushAll(bg); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("FlushAll after Close err = %v", err)
	}
	if err := e.bp.FlushPage(bg, id); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("FlushPage after Close err = %v", err)
	}
	if err := e.bp.DeletePage(bg, id); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("DeletePage after Close err = %v", err)
	}
	if err := e.bp.Close(bg); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("second Close err = %v", err)
	}
}

// --- model-based test -------------------------------------------------------

type heldPin struct {
	ref   *PageRef
	stamp byte
}

func runModel(t *testing.T, seed uint64, steps int) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, seed+11))
	frames := 3 + rng.IntN(6)
	e := newEnv(t, frames, nil)
	model := map[uint64]byte{} // page id -> payload stamp
	held := map[uint64]*heldPin{}
	ids := func() []uint64 {
		var s []uint64
		for id := range model {
			s = append(s, id)
		}
		slices.Sort(s)
		return s
	}
	check := func(r *PageRef, id uint64, want byte) {
		if h, err := DecodeHeader(r.Data()); err != nil || h.ID != id {
			t.Fatalf("seed %d: page %d header %+v, %v", seed, id, h, err)
		}
		if got := payloadStamp(t, r.Data()); got != want {
			t.Fatalf("seed %d: page %d stamp %d, model %d", seed, id, got, want)
		}
	}
	pickFree := func() (uint64, bool) { // a live page that is not held
		var cand []uint64
		for _, id := range ids() {
			if held[id] == nil {
				cand = append(cand, id)
			}
		}
		if len(cand) == 0 {
			return 0, false
		}
		return cand[rng.IntN(len(cand))], true
	}

	for step := range steps {
		switch op := rng.IntN(100); {
		case op < 15 && len(model) < 60 && len(held) < frames-1: // new page
			r, err := e.bp.NewPage(bg, PageTypeHeap)
			if err != nil {
				t.Fatalf("seed %d step %d: NewPage: %v", seed, step, err)
			}
			if _, dup := model[r.ID()]; dup {
				t.Fatalf("seed %d: NewPage returned live page %d", seed, r.ID())
			}
			stamp := byte(rng.UintN(256))
			fillPayload(r, stamp)
			model[r.ID()] = stamp
			if rng.IntN(3) == 0 && len(held) < frames-1 {
				held[r.ID()] = &heldPin{r, stamp}
			} else {
				_ = r.Unpin(true)
			}
		case op < 35: // fetch, verify, unpin clean
			if id, ok := pickFree(); ok && len(held) < frames-1 {
				r := e.fetch(t, id)
				r.RLock()
				check(r, id, model[id])
				r.RUnlock()
				_ = r.Unpin(false)
			}
		case op < 55: // fetch, modify, unpin dirty
			if id, ok := pickFree(); ok && len(held) < frames-1 {
				r := e.fetch(t, id)
				stamp := byte(rng.UintN(256))
				r.Lock()
				check(r, id, model[id])
				fillPayload(r, stamp)
				r.Unlock()
				model[id] = stamp
				_ = r.Unpin(true)
			}
		case op < 65: // hold a pin
			if id, ok := pickFree(); ok && len(held) < frames-1 {
				r := e.fetch(t, id)
				check(r, id, model[id])
				held[id] = &heldPin{r, model[id]}
			}
		case op < 75: // release a held pin, sometimes modifying
			for id, h := range held {
				dirty := false
				if rng.IntN(2) == 0 {
					h.ref.Lock()
					check(h.ref, id, model[id])
					stamp := byte(rng.UintN(256))
					fillPayload(h.ref, stamp)
					h.ref.Unlock()
					model[id] = stamp
					dirty = true
				}
				_ = h.ref.Unpin(dirty)
				delete(held, id)
				break
			}
		case op < 83: // delete
			if id, ok := pickFree(); ok {
				if err := e.bp.DeletePage(bg, id); err != nil {
					t.Fatalf("seed %d step %d: DeletePage(%d): %v", seed, step, id, err)
				}
				delete(model, id)
			}
			for id := range held { // deleting a pinned page must fail
				if err := e.bp.DeletePage(bg, id); !errors.Is(err, ErrPagePinned) {
					t.Fatalf("seed %d: delete of held page err = %v", seed, err)
				}
				break
			}
		case op < 92: // flush one
			if all := ids(); len(all) > 0 {
				if err := e.bp.FlushPage(bg, all[rng.IntN(len(all))]); err != nil {
					t.Fatalf("seed %d: FlushPage: %v", seed, err)
				}
			}
		default: // flush all
			if err := e.bp.FlushAll(bg); err != nil {
				t.Fatalf("seed %d: FlushAll: %v", seed, err)
			}
		}
		e.bp.checkInvariants(t, len(held))
	}

	for id, h := range held {
		check(h.ref, id, model[id])
		_ = h.ref.Unpin(false)
	}
	if err := e.bp.Close(bg); err != nil {
		t.Fatalf("seed %d: Close: %v", seed, err)
	}
	for id, stamp := range model { // everything must have reached the store
		buf := make([]byte, PageSize)
		if err := e.dm.ReadPage(bg, id, buf); err != nil || payloadStamp(t, buf) != stamp {
			t.Fatalf("seed %d: store page %d: stamp mismatch or %v", seed, id, err)
		}
	}
}

func TestModelBased(t *testing.T) {
	base := testSeed(t)
	runs, steps := 60, 600
	if testing.Short() {
		runs = 10
	}
	for i := range runs {
		runModel(t, base+uint64(i), steps)
	}
}

// --- concurrency ---------------------------------------------------------------

func retryFetch(bp *BufferPool, id uint64) (*PageRef, error) {
	for {
		r, err := bp.FetchPage(bg, id)
		if !errors.Is(err, ErrNoFreeFrames) {
			return r, err
		}
		runtime.Gosched() // pool momentarily all pinned: let others unpin
	}
}

func runConcurrent(t *testing.T, frames, workers, pages, iters int) {
	t.Helper()
	e := newEnv(t, frames, nil)
	ids := make([]uint64, pages)
	counts := make(map[uint64]*atomic.Int64, pages)
	for i := range ids {
		ids[i] = e.newStamped(t, 0)
		counts[ids[i]] = new(atomic.Int64)
	}

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(w), 5))
			for range iters {
				id := ids[rng.IntN(pages)]
				r, err := retryFetch(e.bp, id)
				if err != nil {
					t.Errorf("fetch %d: %v", id, err)
					return
				}
				if rng.IntN(3) == 0 {
					r.Lock()
					n := counts[id].Add(1)
					fillPayload(r, byte(n))
					r.Unlock()
					_ = r.Unpin(true)
				} else {
					r.RLock()
					_ = payloadStamp(t, r.Data()) // uniform => never a torn mix of two writes
					r.RUnlock()
					_ = r.Unpin(false)
				}
			}
		}()
	}
	wg.Add(1)
	go func() { // concurrent checkpoints
		defer wg.Done()
		for range 15 {
			if err := e.bp.FlushAll(bg); err != nil {
				t.Errorf("FlushAll: %v", err)
				return
			}
			runtime.Gosched()
		}
	}()
	wg.Wait()

	e.bp.checkInvariants(t, 0)
	if err := e.bp.Close(bg); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		buf := make([]byte, PageSize)
		if err := e.dm.ReadPage(bg, id, buf); err != nil {
			t.Fatal(err)
		}
		if got, want := payloadStamp(t, buf), byte(counts[id].Load()); got != want {
			t.Fatalf("page %d: store has stamp %d, last write was %d (lost update)", id, got, want)
		}
	}
}

func TestConcurrentPinUnpin(t *testing.T) { runConcurrent(t, 8, 6, 40, 300) }

func TestConcurrentMoreWorkersThanFrames(t *testing.T) { runConcurrent(t, 4, 12, 30, 200) }

func TestConcurrentSingleHotPage(t *testing.T) { runConcurrent(t, 2, 8, 1, 300) }

// --- integration with the real disk manager and crashes ---------------------------

func TestCrashLosesOnlyUnsyncedPages(t *testing.T) {
	e := newEnv(t, 8, nil)
	synced := e.newStamped(t, 1)
	flushedOnly := e.newStamped(t, 2)
	memoryOnly := e.newStamped(t, 3)
	if err := e.bp.FlushPage(bg, synced); err != nil {
		t.Fatal(err)
	}
	if err := e.dm.Sync(bg); err != nil {
		t.Fatal(err)
	}
	if err := e.bp.FlushPage(bg, flushedOnly); err != nil { // written, never synced
		t.Fatal(err)
	}
	e.fs.Crash(vfs.CrashOptions{})

	dm := mustOpenDM(t, e.fs, dbName)
	bp, err := NewBufferPool(dm, Options{Frames: 4})
	if err != nil {
		t.Fatal(err)
	}
	r, err := bp.FetchPage(bg, synced)
	if err != nil || payloadStamp(t, r.Data()) != 1 {
		t.Fatalf("synced page: %v", err)
	}
	_ = r.Unpin(false)
	for _, id := range []uint64{flushedOnly, memoryOnly} {
		if _, err := bp.FetchPage(bg, id); !errors.Is(err, ErrZeroPage) {
			t.Fatalf("unsynced page %d err = %v, want ErrZeroPage", id, err)
		}
	}
}

func TestPayloadStampHelperRejectsMixedPage(t *testing.T) {
	// Guards the concurrency test's own oracle: a page that is half one
	// stamp and half another must be noticed.
	d := bytes.Repeat([]byte{1}, PageSize)
	d[PageSize-1] = 2
	ft := &fakeT{}
	func() {
		defer func() { _ = recover() }()
		payloadStamp(ft, d)
	}()
	if !ft.failed {
		t.Fatal("mixed page not detected")
	}
}

// fakeT records Fatalf (and stops the caller) instead of failing the test.
type fakeT struct {
	testing.TB
	failed bool
}

func (f *fakeT) Helper() {}
func (f *fakeT) Fatalf(string, ...any) {
	f.failed = true
	panic("fatal")
}

func TestPageRefDirty(t *testing.T) {
	e := newEnv(t, 4, nil)
	r, err := e.bp.NewPage(bg, PageTypeHeap)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Dirty() {
		t.Fatal("a new page is not dirty")
	}
	id := r.ID()
	if err := r.Unpin(false); err != nil {
		t.Fatal(err)
	}
	if err := e.bp.FlushPage(bg, id); err != nil {
		t.Fatal(err)
	}
	r, err = e.bp.FetchPage(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Dirty() {
		t.Fatal("a flushed page is dirty")
	}
	r.MarkDirty()
	if !r.Dirty() {
		t.Fatal("MarkDirty did not mark the page dirty")
	}
	if err := r.Unpin(false); err != nil {
		t.Fatal(err)
	}
}
