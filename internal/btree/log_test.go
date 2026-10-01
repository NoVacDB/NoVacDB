package btree

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

// fakeLog is an in-memory log with the real WAL's LSN and durability
// semantics: LSNs grow by record size, FlushedLSN is "durable end - 1", and
// the redo point moves when a checkpoint begins.
type fakeLog struct {
	mu       sync.Mutex
	next     uint64
	durable  uint64
	redo     uint64
	recs     []fakeRec
	fail     error
	images   int
	ops      int
	deferred []unlinked
	after    func(blocks []Block) // runs after each append, pages still latched
}

type fakeRec struct {
	lsn     uint64
	payload []byte
}

const fakeFirstLSN = 32

func newFakeLog() *fakeLog {
	return &fakeLog{next: fakeFirstLSN, durable: fakeFirstLSN, redo: fakeFirstLSN}
}

func (l *fakeLog) LogBTree(_ context.Context, build func(uint64) []byte) (uint64, error) {
	l.mu.Lock()
	if l.fail != nil {
		err := l.fail
		l.mu.Unlock()
		return 0, err
	}
	p := build(l.redo)
	blocks, err := DecodeRecord(p)
	if err != nil {
		l.mu.Unlock()
		panic(fmt.Sprintf("tree produced an undecodable record: %v", err)) // test invariant
	}
	for _, b := range blocks {
		if b.Kind == BlockImage {
			l.images++
		} else {
			l.ops++
		}
	}
	lsn := l.next
	l.recs = append(l.recs, fakeRec{lsn: lsn, payload: bytes.Clone(p)})
	l.next += 20 + uint64(len(p))
	after := l.after
	l.mu.Unlock()
	if after != nil {
		after(blocks)
	}
	return lsn, nil
}

func (l *fakeLog) DeferFree(page, lsn uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.deferred = append(l.deferred, unlinked{page, lsn})
}

func (l *fakeLog) flushedLSN() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.durable - 1
}

func (l *fakeLog) flushWAL(context.Context, uint64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.durable = l.next
	return nil
}

func (l *fakeLog) beginCheckpoint() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.redo = l.next
	return l.redo
}

func (l *fakeLog) records() []fakeRec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.recs)
}

func (l *fakeLog) setFail(err error) {
	l.mu.Lock()
	l.fail = err
	l.mu.Unlock()
}

// ruleStore counts page writes that break the WAL rule.
type ruleStore struct {
	storage.PageStore
	flushed    func() uint64
	writes     atomic.Int64
	violations atomic.Int64
}

func (s *ruleStore) WritePage(ctx context.Context, id uint64, buf []byte) error {
	s.writes.Add(1)
	if storage.PageLSN(buf) > s.flushed() {
		s.violations.Add(1)
	}
	return s.PageStore.WritePage(ctx, id, buf)
}

type loggedEnv struct {
	m     *vfs.MemFS
	dm    *storage.DiskManager
	store *ruleStore
	bp    *storage.BufferPool
	lg    *fakeLog
}

func newLoggedEnv(t testing.TB, frames int) *loggedEnv {
	t.Helper()
	m := vfs.NewMemFS(1)
	dm, err := storage.Create(m, "/data")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dm.Close() })
	lg := newFakeLog()
	st := &ruleStore{PageStore: dm, flushed: lg.flushedLSN}
	bp, err := storage.NewBufferPool(st, storage.Options{Frames: frames, FlushedLSN: lg.flushedLSN, FlushWAL: lg.flushWAL})
	if err != nil {
		t.Fatal(err)
	}
	return &loggedEnv{m: m, dm: dm, store: st, bp: bp, lg: lg}
}

// pageImages flushes the pool and returns every allocated page as stored.
func pageImages(t testing.TB, bp *storage.BufferPool, dm *storage.DiskManager) map[uint64][]byte {
	t.Helper()
	if err := bp.FlushAll(bg); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}
	out := map[uint64][]byte{}
	for id := uint64(storage.FirstDataPage); id < dm.PageCount(); id++ {
		buf := make([]byte, storage.PageSize)
		if err := dm.ReadPage(bg, id, buf); err != nil {
			if errors.Is(err, storage.ErrZeroPage) {
				continue // allocated, never written
			}
			t.Fatalf("reading page %d: %v", id, err)
		}
		out[id] = buf
	}
	return out
}

// replayOnto replays recs from lsn from onto the data file in m with a fresh
// pool and returns the pages.
func replayOnto(t testing.TB, m *vfs.MemFS, recs []fakeRec, from uint64, frames int) map[uint64][]byte {
	t.Helper()
	dm, err := storage.Open(m, "/data")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dm.Close() }()
	bp, err := storage.NewBufferPool(dm, storage.Options{Frames: frames})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.lsn < from {
			continue
		}
		if err := Redo(bg, bp, r.lsn, r.payload); err != nil {
			t.Fatalf("redo of record at %d: %v", r.lsn, err)
		}
	}
	return pageImages(t, bp, dm)
}

func samePages(t testing.TB, got, want map[uint64][]byte, what string) {
	t.Helper()
	for id, w := range want {
		g, ok := got[id]
		if !ok || !bytes.Equal(g, w) {
			t.Fatalf("%s: page %d differs (lsn %d vs %d)", what, id, storage.PageLSN(g), storage.PageLSN(w))
		}
	}
	for id := range got {
		if _, ok := want[id]; !ok {
			t.Fatalf("%s: extra page %d", what, id)
		}
	}
}

// loggedWorkload runs random inserts and deletes (with checkpoints that
// move the redo point and flush) against a logged tree and returns the model.
func loggedWorkload(t *testing.T, e *loggedEnv, tr *Tree, rng *rand.Rand, steps int) model {
	t.Helper()
	g := keyGen{rng: rng, maxLen: 600, large: 3}
	m := model{}
	var live [][]byte
	for step := range steps {
		// Alternate growing and shrinking, so merges happen at every height.
		insertPct := 75
		if (step/500)%2 == 1 {
			insertPct = 30
		}
		switch r := rng.IntN(100); {
		case r < 2:
			e.lg.beginCheckpoint()
			if err := e.bp.FlushAll(bg); err != nil {
				t.Fatal(err)
			}
		case r < 2+insertPct || len(live) == 0:
			k, v := g.key(), g.value()
			err := tr.Insert(bg, k, v)
			if errors.Is(err, ErrKeyExists) {
				continue
			}
			if err != nil {
				t.Fatalf("step %d: insert: %v", step, err)
			}
			m[string(k)] = v
			live = append(live, k)
		default:
			j := rng.IntN(len(live))
			k := live[j]
			live[j] = live[len(live)-1]
			live = live[:len(live)-1]
			if found, err := tr.Delete(bg, k); err != nil || !found {
				t.Fatalf("step %d: delete: %v %v", step, found, err)
			}
			delete(m, string(k))
		}
	}
	return m
}

func TestRecordRoundTrip(t *testing.T) {
	img := make([]byte, storage.PageSize)
	img[0] = 7
	blocks := []Block{
		{Page: 5, Kind: BlockImage, Image: img},
		{Page: 6, Kind: BlockLeafInsert, Index: 3, Key: []byte("key"), Value: []byte("value")},
		{Page: 7, Kind: BlockLeafDelete, Index: 9, Key: []byte("k")},
		{Page: 8, Kind: BlockLeafInsert, Index: 0, Key: nil, Value: nil},
	}
	got, err := DecodeRecord(encodeRecord(blocks))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(blocks) {
		t.Fatalf("%d blocks", len(got))
	}
	for i, b := range blocks {
		g := got[i]
		if g.Page != b.Page || g.Kind != b.Kind || g.Index != b.Index || !bytes.Equal(g.Key, b.Key) ||
			!bytes.Equal(g.Value, b.Value) || !bytes.Equal(g.Image, b.Image) {
			t.Fatalf("block %d: %+v, want %+v", i, g, b)
		}
	}
}

func TestRecordGoldenBytes(t *testing.T) {
	got := encodeRecord([]Block{
		{Page: 2, Kind: BlockLeafInsert, Index: 1, Key: []byte("k"), Value: []byte("vv")},
		{Page: 3, Kind: BlockLeafDelete, Index: 258, Key: []byte("k")},
	})
	want := []byte{
		2,
		2, 0, 0, 0, 0, 0, 0, 0, 2, 1, 0, 1, 0, 2, 0, 'k', 'v', 'v',
		3, 0, 0, 0, 0, 0, 0, 0, 3, 2, 1, 1, 0, 'k',
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got % x\nwant % x", got, want)
	}
}

func TestDecodeRecordRejects(t *testing.T) {
	good := encodeRecord([]Block{{Page: 2, Kind: BlockLeafDelete, Index: 1, Key: []byte("k")}})
	twice := encodeRecord([]Block{
		{Page: 2, Kind: BlockLeafDelete, Key: []byte("a")},
		{Page: 2, Kind: BlockLeafDelete, Key: []byte("b")},
	})
	nine := []byte{9}
	for range 9 {
		nine = append(nine, encodeRecord([]Block{{Page: 4, Kind: BlockLeafDelete}})[1:]...)
	}
	longKey := encodeRecord([]Block{{Page: 2, Kind: BlockLeafDelete, Key: make([]byte, MaxKeySize+1)}})
	longVal := encodeRecord([]Block{{Page: 2, Kind: BlockLeafInsert, Value: make([]byte, MaxValueSize+1)}})
	cases := map[string][]byte{
		"empty":          {},
		"zero blocks":    {0},
		"nine blocks":    nine,
		"truncated":      good[:len(good)-1],
		"trailing":       append(bytes.Clone(good), 0),
		"reserved page":  {1, 1, 0, 0, 0, 0, 0, 0, 0, 3, 0, 0, 0, 0},
		"unknown kind":   {1, 2, 0, 0, 0, 0, 0, 0, 0, 9},
		"short image":    append([]byte{1, 2, 0, 0, 0, 0, 0, 0, 0, 1}, make([]byte, 100)...),
		"page twice":     twice,
		"key too long":   longKey,
		"value too long": longVal,
	}
	for name, p := range cases {
		if _, err := DecodeRecord(p); !errors.Is(err, ErrBadRecord) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func FuzzDecodeRecord(f *testing.F) {
	f.Add(encodeRecord([]Block{{Page: 2, Kind: BlockLeafInsert, Index: 1, Key: []byte("k"), Value: []byte("v")}}))
	f.Add(encodeRecord([]Block{{Page: 9, Kind: BlockLeafDelete, Index: 4, Key: []byte("abc")}}))
	f.Add([]byte{1, 2, 0, 0, 0, 0, 0, 0, 0, 1})
	f.Fuzz(func(t *testing.T, p []byte) {
		blocks, err := DecodeRecord(p)
		if err != nil {
			if !errors.Is(err, ErrBadRecord) {
				t.Fatalf("unexpected error: %v", err)
			}
			return
		}
		// What decodes re-encodes to the same bytes.
		if again := encodeRecord(blocks); !bytes.Equal(again, p) {
			t.Fatalf("re-encoding differs")
		}
	})
}

func TestReplayReproducesPagesExactly(t *testing.T) {
	base := testSeed(t)
	var total opCounts
	defer func() { total.requireMost(t) }()
	for i := range 8 {
		seed := base + uint64(i)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 77))
			e := newLoggedEnv(t, 8+rng.IntN(8))
			tr, err := Create(bg, e.bp, WithLogger(e.lg))
			if err != nil {
				t.Fatal(err)
			}
			m := loggedWorkload(t, e, tr, rng, 6000)
			// Then shrink to nothing: merges and collapses at every height.
			for _, k := range m.randomKeys(rng) {
				if _, err := tr.Delete(bg, k); err != nil {
					t.Fatal(err)
				}
				delete(m, string(k))
			}
			m.verify(t, tr)
			if e.store.violations.Load() != 0 {
				t.Fatalf("%d WAL-rule violations", e.store.violations.Load())
			}
			if e.lg.images == 0 || e.lg.ops == 0 || e.store.writes.Load() == 0 {
				t.Fatalf("workload too tame: %d images, %d operations, %d writes", e.lg.images, e.lg.ops, e.store.writes.Load())
			}
			total.add(tr.opCounts())
			t.Logf("%d records: %d image blocks, %d operation blocks", len(e.lg.records()), e.lg.images, e.lg.ops)
			want := pageImages(t, e.bp, e.dm)

			// A fresh data file with the same pages allocated: replaying
			// the whole log rebuilds every written page byte for byte.
			m2 := vfs.NewMemFS(seed)
			dm2, err := storage.Create(m2, "/data")
			if err != nil {
				t.Fatal(err)
			}
			for dm2.PageCount() < e.dm.PageCount() {
				if _, err := dm2.Allocate(bg); err != nil {
					t.Fatal(err)
				}
			}
			_ = dm2.Close()
			recs := e.lg.records()
			samePages(t, replayOnto(t, m2, recs, 0, 4+rng.IntN(8)), want, "full replay")
			// Replaying again on top (pages already newer) changes nothing.
			samePages(t, replayOnto(t, m2, recs, 0, 6), want, "second replay")
		})
	}
}

// What recovery faces: pages changed after the redo point may be torn,
// newer or older on disk. Replaying from the redo point repairs them all.
func TestReplayFromRedoPointRepairsTornPages(t *testing.T) {
	base := testSeed(t)
	for i := range 8 {
		seed := base + 1000 + uint64(i)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 78))
			e := newLoggedEnv(t, 8+rng.IntN(8))
			tr, err := Create(bg, e.bp, WithLogger(e.lg))
			if err != nil {
				t.Fatal(err)
			}
			_ = loggedWorkload(t, e, tr, rng, 2000)
			redo := e.lg.beginCheckpoint()
			if err := e.bp.FlushAll(bg); err != nil {
				t.Fatal(err)
			}
			// Changes after the checkpoint, some reaching disk by eviction.
			_ = loggedWorkload(t, e, tr, rng, 600)
			if err := e.dm.Sync(bg); err != nil {
				t.Fatal(err)
			}
			raw := readAll(t, e.m, "/data")
			if need := int(e.dm.PageCount()) * storage.PageSize; len(raw) < need {
				raw = append(raw, make([]byte, need-len(raw))...)
			}
			want := pageImages(t, e.bp, e.dm)
			var eligible []uint64
			for id, img := range want {
				if storage.PageLSN(img) >= redo {
					eligible = append(eligible, id)
				}
			}
			slices.Sort(eligible)
			if len(eligible) == 0 {
				t.Fatal("no page changed after the checkpoint")
			}
			torn := 0
			for j, id := range eligible {
				if j == 0 || rng.IntN(2) == 0 {
					off := int(id) * storage.PageSize
					for k := range storage.PageSize / 2 {
						raw[off+storage.PageSize/4+k] ^= 0xA5
					}
					torn++
				}
			}
			crashed := vfs.NewMemFS(seed)
			writeAll(t, crashed, "/data", raw)
			got := replayOnto(t, crashed, e.lg.records(), redo, 4+rng.IntN(8))
			samePages(t, got, want, fmt.Sprintf("replay from %d with %d torn pages", redo, torn))
		})
	}
}

func readAll(t testing.TB, fsys vfs.FS, name string) []byte {
	t.Helper()
	f, err := fsys.OpenFile(name, vfs.ORead)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	size, err := f.Size()
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, size)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	return buf
}

func writeAll(t testing.TB, fsys vfs.FS, name string, data []byte) {
	t.Helper()
	f, err := fsys.OpenFile(name, vfs.ORead|vfs.OWrite|vfs.OCreate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestImageRule(t *testing.T) {
	// The first change to a leaf after the redo point logs an image; later
	// changes log operations, until the next checkpoint.
	e := newLoggedEnv(t, 8)
	tr, err := Create(bg, e.bp, WithLogger(e.lg))
	if err != nil {
		t.Fatal(err)
	}
	kinds := func() []BlockKind {
		recs := e.lg.records()
		blocks, err := DecodeRecord(recs[len(recs)-1].payload)
		if err != nil {
			t.Fatal(err)
		}
		var out []BlockKind
		for _, b := range blocks {
			out = append(out, b.Kind)
		}
		return out
	}
	expect := func(what string, want ...BlockKind) {
		t.Helper()
		if got := kinds(); !slices.Equal(got, want) {
			t.Fatalf("%s logged %v, want %v", what, got, want)
		}
	}
	expect("create", BlockImage)
	ins := func(k string) {
		t.Helper()
		if err := tr.Insert(bg, []byte(k), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	ins("a")
	expect("insert after create", BlockLeafInsert)
	e.lg.beginCheckpoint()
	ins("b")
	expect("first insert after the redo point", BlockImage)
	ins("c")
	expect("second insert", BlockLeafInsert)
	if _, err := tr.Delete(bg, []byte("a")); err != nil {
		t.Fatal(err)
	}
	expect("delete", BlockLeafDelete)
	e.lg.beginCheckpoint()
	if _, err := tr.Delete(bg, []byte("b")); err != nil {
		t.Fatal(err)
	}
	expect("first delete after the redo point", BlockImage)
	// A split logs every page it changes as an image, in one record.
	val := make([]byte, MaxValueSize)
	for i := range 6 { // the sixth maximum-size cell splits the root
		if err := tr.Insert(bg, maxKey(fmt.Sprint(i)), val); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range e.lg.records() {
		blocks, _ := DecodeRecord(r.payload)
		if len(blocks) == 3 {
			for _, b := range blocks {
				if b.Kind != BlockImage {
					t.Fatalf("root split logged a %d block", b.Kind)
				}
			}
			return
		}
	}
	t.Fatal("no three-page record for the root split")
}

func TestFailedLogLeavesPagesUntouched(t *testing.T) {
	// Logging fails at random moments: the failing operation reports the
	// error and every page is exactly as before; the tree still matches the
	// model once logging works again.
	rng := rand.New(rand.NewPCG(testSeed(t), 79))
	e := newLoggedEnv(t, 64) // everything stays in memory: pages are compared in the pool
	tr, err := Create(bg, e.bp, WithLogger(e.lg))
	if err != nil {
		t.Fatal(err)
	}
	g := keyGen{rng: rng, maxLen: 600, large: 5}
	m := model{}
	var live [][]byte
	boom := errors.New("log device failed")
	failures, structural := 0, 0
	for step := range 8000 {
		insertPct := 70 // grow, then shrink, so merges fail too
		if (step/1000)%2 == 1 {
			insertPct = 25
		}
		insert := rng.IntN(100) < insertPct || len(live) == 0
		fail := rng.IntN(10) == 0
		var snap map[uint64][]byte
		if fail {
			snap = poolPages(t, e)
			e.lg.setFail(boom)
		}
		var err error
		var k []byte
		if insert {
			k = g.key()
			v := g.value()
			err = tr.Insert(bg, k, v)
			if err == nil {
				m[string(k)] = v
				live = append(live, k)
			}
		} else {
			j := rng.IntN(len(live))
			k = live[j]
			var found bool
			found, err = tr.Delete(bg, k)
			if err == nil {
				if !found {
					t.Fatalf("step %d: key not found", step)
				}
				delete(m, string(k))
				live[j] = live[len(live)-1]
				live = live[:len(live)-1]
			}
		}
		if fail {
			e.lg.setFail(nil)
			if errors.Is(err, ErrKeyExists) {
				continue
			}
			if !errors.Is(err, boom) {
				t.Fatalf("step %d: operation with a failing log: %v", step, err)
			}
			failures++
			after := poolPages(t, e)
			for id, img := range snap {
				if !bytes.Equal(after[id], img) {
					t.Fatalf("step %d: page %d changed by a failed operation", step, id)
				}
			}
			// Retry with the log working, to learn what the failed attempt
			// had to do.
			c0 := tr.opCounts()
			if insert {
				v := g.value()
				if err := tr.Insert(bg, k, v); err != nil {
					t.Fatalf("step %d: retried insert: %v", step, err)
				}
				m[string(k)] = v
				live = append(live, k)
			} else {
				if found, err := tr.Delete(bg, k); err != nil || !found {
					t.Fatalf("step %d: retried delete: %v %v", step, found, err)
				}
				delete(m, string(k))
				live = slices.DeleteFunc(live, func(x []byte) bool { return bytes.Equal(x, k) })
			}
			if c1 := tr.opCounts(); c1 != c0 {
				structural++
			}
			continue
		}
		if err != nil && !errors.Is(err, ErrKeyExists) {
			t.Fatalf("step %d: %v", step, err)
		}
	}
	m.verify(t, tr)
	t.Logf("%d failed operations, %d of them structural; %+v", failures, structural, tr.opCounts())
	if failures < 300 || structural < 20 {
		t.Fatalf("only %d failures, %d structural", failures, structural)
	}
}

// poolPages copies every page of the data file through the pool. The
// checksum field is cleared: it is only meaningful on disk, and a page
// evicted and read back between two snapshots gets a fresh one.
func poolPages(t *testing.T, e *loggedEnv) map[uint64][]byte {
	t.Helper()
	out := map[uint64][]byte{}
	for id := uint64(storage.FirstDataPage); id < e.dm.PageCount(); id++ {
		ref, err := e.bp.FetchPage(bg, id)
		if err != nil {
			if errors.Is(err, storage.ErrZeroPage) {
				continue
			}
			t.Fatal(err)
		}
		ref.RLock()
		img := bytes.Clone(ref.Data())
		ref.RUnlock()
		clear(img[16:20]) // the CRC-32C field of the page header
		out[id] = img
		_ = ref.Unpin(false)
	}
	return out
}

func TestPagesAreDirtyBeforeTheirRecordIsAppended(t *testing.T) {
	// A checkpoint beginning right after a record is appended must find
	// every page in it dirty, or it would skip them while recovery starts
	// after the record.
	rng := rand.New(rand.NewPCG(testSeed(t), 80))
	e := newLoggedEnv(t, 64)
	checks := 0
	e.lg.after = func(blocks []Block) {
		for _, b := range blocks {
			ref, err := e.bp.FetchPage(bg, b.Page) // pins without latching
			if err != nil {
				t.Errorf("page %d: %v", b.Page, err)
				continue
			}
			if !ref.Dirty() {
				t.Errorf("page %d is not dirty when its record (kind %d) is appended", b.Page, b.Kind)
			}
			_ = ref.Unpin(false)
			checks++
		}
	}
	tr, err := Create(bg, e.bp, WithLogger(e.lg))
	if err != nil {
		t.Fatal(err)
	}
	g := keyGen{rng: rng, maxLen: 300, large: 5}
	var live [][]byte
	for step := range 2000 {
		// Start from clean pages, as right after a checkpoint flush.
		if step%10 == 0 {
			if err := e.bp.FlushAll(bg); err != nil {
				t.Fatal(err)
			}
		}
		if rng.IntN(100) < 60 || len(live) == 0 {
			k := g.key()
			if tr.Insert(bg, k, g.value()) == nil {
				live = append(live, k)
			}
		} else {
			j := rng.IntN(len(live))
			if _, err := tr.Delete(bg, live[j]); err != nil {
				t.Fatal(err)
			}
			live[j] = live[len(live)-1]
			live = live[:len(live)-1]
		}
	}
	c := tr.opCounts()
	if checks == 0 || c.merges[0] == 0 || c.splits[0] == 0 || c.collapses+c.rootSplits[0] == 0 {
		t.Fatalf("too little checked: %d pages, %+v", checks, c)
	}
}

func TestDeferredFreesAreUnreachableAndAfterTheirRecord(t *testing.T) {
	e := newLoggedEnv(t, 32)
	tr, err := Create(bg, e.bp, WithLogger(e.lg))
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(testSeed(t), 81))
	m := model{}
	for len(m) < 3000 {
		k := append(AppendInt64(nil, int64(rng.IntN(1_000_000))), make([]byte, 200)...)
		if tr.Insert(bg, k, nil) == nil {
			m[string(k)] = []byte{}
		}
	}
	for _, k := range m.randomKeys(rng) {
		if _, err := tr.Delete(bg, k); err != nil {
			t.Fatal(err)
		}
	}
	// Every page but the root was unlinked, by a record that names it as
	// neither changed nor alive: the deferred list is exactly those pages.
	inUse := int(e.dm.PageCount() - storage.FirstDataPage)
	if got := len(e.lg.deferred); got != inUse-1 {
		t.Fatalf("%d pages deferred, %d allocated besides the root", got, inUse-1)
	}
	lsns := map[uint64]bool{}
	for _, r := range e.lg.records() {
		lsns[r.lsn] = true
	}
	seen := map[uint64]bool{}
	for _, d := range e.lg.deferred {
		if d.page == tr.Root() || seen[d.page] {
			t.Fatalf("page %d deferred twice or is the root", d.page)
		}
		seen[d.page] = true
		if !lsns[d.lsn] {
			t.Fatalf("page %d deferred with lsn %d, which is no record", d.page, d.lsn)
		}
		// The unlinking record does not change the page itself.
		for _, r := range e.lg.records() {
			if r.lsn != d.lsn {
				continue
			}
			blocks, _ := DecodeRecord(r.payload)
			for _, b := range blocks {
				if b.Page == d.page {
					t.Fatalf("record %d both changes and unlinks page %d", d.lsn, d.page)
				}
			}
		}
	}
}

func TestRedoRejectsBlocksThatDoNotFit(t *testing.T) {
	bp := newPool(t, 8)
	tr, err := Create(bg, bp)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"b", "d"} {
		if err := tr.Insert(bg, []byte(k), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	leaf := tr.Root()
	img := pageImage(t, bp, leaf)
	otherID := bytes.Clone(img)
	binary.LittleEndian.PutUint64(otherID, leaf+1) // claims to be another page
	broken := bytes.Clone(img)
	node{broken}.setNumCells(5000)
	inner, _, iref := newNodePage(t, bp, false, 1)
	node{iref.Data()}.setChild0(leaf)
	_ = iref.Unpin(true)
	cases := map[string]Block{
		"image of another page":   {Page: leaf, Kind: BlockImage, Image: otherID},
		"invalid image":           {Page: leaf, Kind: BlockImage, Image: broken},
		"insert out of place":     {Page: leaf, Kind: BlockLeafInsert, Index: 0, Key: []byte("c")},
		"insert of a present key": {Page: leaf, Kind: BlockLeafInsert, Index: 0, Key: []byte("b")},
		"delete of another key":   {Page: leaf, Kind: BlockLeafDelete, Index: 0, Key: []byte("d")},
		"delete past the end":     {Page: leaf, Kind: BlockLeafDelete, Index: 7, Key: []byte("b")},
		"insert into a non-leaf":  {Page: inner, Kind: BlockLeafInsert, Index: 0, Key: []byte("a")},
	}
	for name, b := range cases {
		before := pageImage(t, bp, b.Page)
		err := Redo(bg, bp, 1<<40, encodeRecord([]Block{b}))
		if !errors.Is(err, ErrBadRecord) {
			t.Errorf("%s: %v", name, err)
		}
		if after := pageImage(t, bp, b.Page); !bytes.Equal(before, after) {
			t.Errorf("%s: the page changed", name)
		}
	}
	// The same operations in place apply, once.
	ok := encodeRecord([]Block{{Page: leaf, Kind: BlockLeafInsert, Index: 1, Key: []byte("c"), Value: []byte("v")}})
	if err := Redo(bg, bp, 1<<40, ok); err != nil {
		t.Fatal(err)
	}
	if err := Redo(bg, bp, 1<<40, ok); err != nil {
		t.Fatalf("replaying a record the page already has: %v", err)
	}
	model{"b": []byte("v"), "c": []byte("v"), "d": []byte("v")}.verify(t, tr)
}
