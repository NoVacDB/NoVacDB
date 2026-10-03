package wal

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"path"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

func TestOpenFreshCreatesDurableSegment(t *testing.T) {
	m := newFS(t)
	w := mustOpen(t, m, Options{})
	if w.EndLSN() != SegmentHeaderSize || w.DurableEnd() != SegmentHeaderSize || w.FlushedLSN() != SegmentHeaderSize-1 {
		t.Fatalf("fresh: end %d durable %d flushed %d", w.EndLSN(), w.DurableEnd(), w.FlushedLSN())
	}
	names, err := m.List(testDir)
	if err != nil || len(names) != 1 || names[0] != SegmentName(0) {
		t.Fatalf("directory = %v, %v", names, err)
	}
	// Nothing was flushed, yet the log must already survive a crash.
	m.Crash(vfs.CrashOptions{})
	w2 := mustOpen(t, m, Options{})
	if w2.EndLSN() != SegmentHeaderSize {
		t.Fatalf("after crash EndLSN = %d", w2.EndLSN())
	}
}

func TestAppendAssignsContiguousLSNs(t *testing.T) {
	m := newFS(t)
	w := mustOpen(t, m, Options{})
	rng := rand.New(rand.NewPCG(testSeed(t), 1))
	want := LSN(SegmentHeaderSize)
	var recs []logRec
	for _, n := range []int{0, 1, 100, 7, 4096, 0, 33} {
		p := payload(rng, n)
		lsn := mustAppend(t, w, RecordType(n%5+1), p)
		if lsn != want {
			t.Fatalf("LSN = %d, want %d", lsn, want)
		}
		want += LSN(RecordSize(n))
		if w.EndLSN() != want {
			t.Fatalf("EndLSN = %d, want %d", w.EndLSN(), want)
		}
		recs = append(recs, logRec{lsn, RecordType(n%5 + 1), p})
	}
	mustFlush(t, w)
	got, segs := readLog(t, m, testDir)
	sameRecs(t, got, recs, "log")
	if len(segs) != 1 || segs[0].Size != int64(want) {
		t.Fatalf("segments = %+v, want one of %d bytes", segs, want)
	}
	if got := readFile(t, m, path.Join(testDir, SegmentName(0))); !bytes.Equal(got[:SegmentHeaderSize], AppendSegmentHeader(nil, 0)) {
		t.Fatal("segment header wrong")
	}
}

func TestDurableEndOnlyAdvancesOnFlush(t *testing.T) {
	m := newFS(t)
	w := mustOpen(t, m, Options{})
	a := mustAppend(t, w, 1, []byte("a"))
	if w.DurableEnd() != SegmentHeaderSize {
		t.Fatalf("DurableEnd moved on Append: %d", w.DurableEnd())
	}
	mustFlush(t, w)
	if w.DurableEnd() != w.EndLSN() {
		t.Fatalf("after Flush DurableEnd %d != EndLSN %d", w.DurableEnd(), w.EndLSN())
	}
	// The record at a is durable (FlushedLSN >= a); the next record start is not.
	if w.FlushedLSN() < a || w.FlushedLSN() >= w.EndLSN() {
		t.Fatalf("FlushedLSN %d, record at %d, end %d", w.FlushedLSN(), a, w.EndLSN())
	}
	b := mustAppend(t, w, 1, []byte("b"))
	if w.FlushedLSN() >= b {
		t.Fatalf("unflushed record %d counted as flushed (FlushedLSN %d)", b, w.FlushedLSN())
	}
	end := w.DurableEnd()
	mustFlush(t, w)
	mustFlush(t, w) // nothing pending: harmless
	if w.DurableEnd() <= end || w.DurableEnd() != w.EndLSN() {
		t.Fatalf("DurableEnd = %d", w.DurableEnd())
	}
}

func TestCrashKeepsFlushedLosesUnflushed(t *testing.T) {
	m := newFS(t)
	w := mustOpen(t, m, Options{})
	a := mustAppend(t, w, 1, []byte("durable"))
	mustFlush(t, w)
	mustAppend(t, w, 2, []byte("volatile"))
	m.Crash(vfs.CrashOptions{})
	w2 := mustOpen(t, m, Options{})
	recs, _ := readLog(t, m, testDir)
	if len(recs) != 1 || recs[0].LSN != a || string(recs[0].Payload) != "durable" {
		t.Fatalf("recovered %v", recs)
	}
	if w2.EndLSN() != a+LSN(RecordSize(len("durable"))) {
		t.Fatalf("EndLSN = %d", w2.EndLSN())
	}
}

func TestFlushToSkipsAlreadyDurableRecords(t *testing.T) {
	m := newFS(t)
	w := mustOpen(t, m, Options{})
	a := mustAppend(t, w, 1, []byte("a"))
	mustFlush(t, w)
	b := mustAppend(t, w, 1, []byte("b"))

	m.InjectError(vfs.Fault{Op: vfs.OpSync})
	if err := w.FlushTo(bg, a); err != nil { // a is already durable: no I/O
		t.Fatalf("FlushTo(durable) did I/O: %v", err)
	}
	if err := w.FlushTo(bg, b); !errors.Is(err, vfs.ErrInjected) { // b is not: fsync runs and fails
		t.Fatalf("FlushTo(b) err = %v", err)
	}
	if w.FlushedLSN() >= b {
		t.Fatal("record counted durable after a failed fsync")
	}
}

func TestFlushToMakesRecordDurable(t *testing.T) {
	m := newFS(t)
	w := mustOpen(t, m, Options{})
	mustAppend(t, w, 1, []byte("a"))
	b := mustAppend(t, w, 1, []byte("b"))
	if err := w.FlushTo(bg, b); err != nil {
		t.Fatal(err)
	}
	if w.FlushedLSN() < b {
		t.Fatalf("FlushedLSN %d < %d after FlushTo", w.FlushedLSN(), b)
	}
	m.Crash(vfs.CrashOptions{})
	mustOpen(t, m, Options{})
	if recs, _ := readLog(t, m, testDir); len(recs) != 2 {
		t.Fatalf("recovered %d records", len(recs))
	}
}

func TestFailedFlushPoisonsWriter(t *testing.T) {
	for _, op := range []vfs.Op{vfs.OpWriteAt, vfs.OpSync} {
		t.Run(op.String(), func(t *testing.T) {
			m := newFS(t)
			w := mustOpen(t, m, Options{})
			mustAppend(t, w, 1, []byte("ok"))
			mustFlush(t, w)
			end := w.DurableEnd()
			mustAppend(t, w, 1, []byte("lost"))
			m.InjectError(vfs.Fault{Op: op})
			if err := w.Flush(bg); !errors.Is(err, vfs.ErrInjected) {
				t.Fatalf("Flush err = %v", err)
			}
			if w.DurableEnd() != end {
				t.Fatalf("DurableEnd moved to %d after a failed flush", w.DurableEnd())
			}
			if _, err := w.Append(bg, 1, []byte("x")); !errors.Is(err, ErrFailed) {
				t.Fatalf("Append err = %v", err)
			}
			if err := w.Flush(bg); !errors.Is(err, ErrFailed) {
				t.Fatalf("Flush err = %v", err)
			}
			if err := w.Close(bg); !errors.Is(err, ErrFailed) {
				t.Fatalf("Close err = %v", err)
			}
			m.Crash(vfs.CrashOptions{})
			mustOpen(t, m, Options{})
			recs, _ := readLog(t, m, testDir)
			if len(recs) != 1 || string(recs[0].Payload) != "ok" {
				t.Fatalf("recovered %v, want only the flushed record", recs)
			}
		})
	}
}

// --- segments ----------------------------------------------------------------

// expectLayout simulates the documented rollover rule independently.
func expectLayout(segSize uint64, sizes []int) (lsns []LSN, segStarts []LSN) {
	curStart, curLen := uint64(0), uint64(SegmentHeaderSize)
	segStarts = []LSN{0}
	for _, n := range sizes {
		rec := uint64(RecordSize(n))
		if curLen+rec > segSize && curLen > SegmentHeaderSize {
			curStart += curLen
			curLen = SegmentHeaderSize
			segStarts = append(segStarts, LSN(curStart))
		}
		lsns = append(lsns, LSN(curStart+curLen))
		curLen += rec
	}
	return lsns, segStarts
}

func TestSegmentRollover(t *testing.T) {
	const segSize = 200
	m := newFS(t)
	w := mustOpen(t, m, Options{SegmentSize: segSize})
	sizes := make([]int, 20)
	rng := rand.New(rand.NewPCG(testSeed(t), 2))
	for i := range sizes {
		sizes[i] = rng.IntN(90)
	}
	wantLSNs, wantStarts := expectLayout(segSize, sizes)
	var recs []logRec
	for i, n := range sizes {
		p := payload(rng, n)
		lsn := mustAppend(t, w, 7, p)
		if lsn != wantLSNs[i] {
			t.Fatalf("record %d: LSN %d, want %d", i, lsn, wantLSNs[i])
		}
		recs = append(recs, logRec{lsn, 7, p})
	}
	mustFlush(t, w)
	got, segs := readLog(t, m, testDir)
	sameRecs(t, got, recs, "log")
	if len(segs) != len(wantStarts) || len(segs) < 4 {
		t.Fatalf("%d segments, want %d (and several)", len(segs), len(wantStarts))
	}
	for i, s := range segs {
		if s.Start != wantStarts[i] {
			t.Fatalf("segment %d starts at %d, want %d", i, s.Start, wantStarts[i])
		}
		if s.Size > segSize && s.Recs != 1 {
			t.Fatalf("segment %d has %d bytes and %d records (limit %d)", i, s.Size, s.Recs, segSize)
		}
	}
	// The log is durable across a crash, including the directory entries.
	m.Crash(vfs.CrashOptions{})
	w2 := mustOpen(t, m, Options{SegmentSize: segSize})
	got2, _ := readLog(t, m, testDir)
	sameRecs(t, got2, recs, "after crash")
	if w2.EndLSN() != w.EndLSN() {
		t.Fatalf("EndLSN %d after reopen, was %d", w2.EndLSN(), w.EndLSN())
	}
}

func TestRolloverBoundaryIsExact(t *testing.T) {
	const payloadLen = 30
	rec := int64(RecordSize(payloadLen))
	for _, tc := range []struct {
		name     string
		segSize  int64
		wantSegs int
	}{
		// Header + two records fit exactly, so only the third rolls over.
		{"two records fit exactly", SegmentHeaderSize + 2*rec, 2},
		// One byte less and the second record no longer fits either.
		{"one byte short of two", SegmentHeaderSize + 2*rec - 1, 3},
		// Header + one record fit exactly.
		{"one record fits exactly", SegmentHeaderSize + rec, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newFS(t)
			w := mustOpen(t, m, Options{SegmentSize: tc.segSize})
			for range 3 {
				mustAppend(t, w, 1, make([]byte, payloadLen))
			}
			mustFlush(t, w)
			_, segs := readLog(t, m, testDir)
			if len(segs) != tc.wantSegs {
				t.Fatalf("%d segments %+v, want %d", len(segs), segs, tc.wantSegs)
			}
		})
	}
}

func TestRecordLargerThanSegmentGetsOwnSegment(t *testing.T) {
	m := newFS(t)
	w := mustOpen(t, m, Options{SegmentSize: 100})
	rng := rand.New(rand.NewPCG(1, 1))
	var recs []logRec
	for _, n := range []int{10, 5000, 10, 3, MaxPayload, 1} {
		p := payload(rng, n)
		recs = append(recs, logRec{mustAppend(t, w, 1, p), 1, p})
	}
	mustFlush(t, w)
	got, segs := readLog(t, m, testDir)
	sameRecs(t, got, recs, "log")
	for _, s := range segs {
		if s.Size > 100 && s.Recs != 1 {
			t.Fatalf("oversize segment holds %d records: %+v", s.Recs, s)
		}
	}
}

func TestManySegments(t *testing.T) {
	m := newFS(t)
	w := mustOpen(t, m, Options{SegmentSize: 1}) // every record gets a segment
	const n = 300
	var recs []logRec
	for i := range n {
		p := []byte(fmt.Sprintf("record-%d", i))
		recs = append(recs, logRec{mustAppend(t, w, 2, p), 2, p})
		if i%7 == 0 {
			mustFlush(t, w)
		}
	}
	mustClose(t, w)
	got, segs := readLog(t, m, testDir)
	sameRecs(t, got, recs, "log")
	if len(segs) != n {
		t.Fatalf("%d segments, want %d", len(segs), n)
	}
	w2 := mustOpen(t, m, Options{SegmentSize: 1})
	if w2.EndLSN() != w.EndLSN() {
		t.Fatalf("EndLSN %d vs %d", w2.EndLSN(), w.EndLSN())
	}
}

func TestAutoFlushBoundsMemory(t *testing.T) {
	m := newFS(t)
	w := mustOpen(t, m, Options{MaxPendingBytes: 1000, SegmentSize: 5000})
	p := bytes.Repeat([]byte{1}, 80)
	for i := range 200 {
		mustAppend(t, w, 1, p)
		w.mu.Lock()
		pending := w.pendingBytes
		w.mu.Unlock()
		if pending >= 1000+RecordSize(len(p))+SegmentHeaderSize {
			t.Fatalf("after %d appends %d bytes are still buffered", i+1, pending)
		}
	}
	if w.DurableEnd() == SegmentHeaderSize {
		t.Fatal("nothing became durable without an explicit Flush")
	}
	mustClose(t, w)
	recs, _ := readLog(t, m, testDir)
	if len(recs) != 200 {
		t.Fatalf("%d records", len(recs))
	}
}

// --- reopen and recovery ------------------------------------------------------

func BenchmarkAppend(b *testing.B) {
	m := vfs.NewMemFS(1)
	_ = m.MkdirAll("/db")
	w, err := Open(m, testDir, Options{})
	if err != nil {
		b.Fatal(err)
	}
	p := bytes.Repeat([]byte{1}, 100)
	b.ReportAllocs()
	b.SetBytes(int64(RecordSize(len(p))))
	for i := 0; i < b.N; i++ {
		if _, err := w.Append(bg, 1, p); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAppendAndFlush(b *testing.B) {
	for _, batch := range []int{1, 16, 256} {
		b.Run(fmt.Sprintf("batch=%d", batch), func(b *testing.B) {
			m := vfs.NewMemFS(1)
			_ = m.MkdirAll("/db")
			w, err := Open(m, testDir, Options{})
			if err != nil {
				b.Fatal(err)
			}
			p := bytes.Repeat([]byte{1}, 100)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for range batch {
					if _, err := w.Append(bg, 1, p); err != nil {
						b.Fatal(err)
					}
				}
				if err := w.Flush(bg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// After a killed process (no power cut) the file system still holds bytes that
// were written but never fsynced. Open reports everything it recovered as
// durable, so it has to make that true by syncing before it returns.
func TestOpenMakesRecoveredRecordsDurable(t *testing.T) {
	m := newFS(t)
	w := mustOpen(t, m, Options{})
	a := mustAppend(t, w, 1, []byte("synced"))
	mustFlush(t, w)
	b := mustAppend(t, w, 1, []byte("written but never synced"))
	m.InjectError(vfs.Fault{Op: vfs.OpSync})
	if err := w.Flush(bg); !errors.Is(err, vfs.ErrInjected) { // bytes reach the file, the fsync fails
		t.Fatalf("Flush err = %v", err)
	}
	// The process dies here. Nothing is lost yet: the bytes are still visible.
	w2 := mustOpen(t, m, Options{})
	if want := b + LSN(RecordSize(len("written but never synced"))); w2.EndLSN() != want {
		t.Fatalf("EndLSN %d, want %d (the unsynced record is still in the file)", w2.EndLSN(), want)
	}
	if w2.DurableEnd() != w2.EndLSN() {
		t.Fatalf("DurableEnd %d != EndLSN %d", w2.DurableEnd(), w2.EndLSN())
	}
	// Now the power goes. Open said the record was durable, so it must be.
	m.Crash(vfs.CrashOptions{})
	mustOpen(t, m, Options{})
	recs, _ := readLog(t, m, testDir)
	if len(recs) != 2 || recs[0].LSN != a || recs[1].LSN != b {
		t.Fatalf("recovered %v: the record Open reported durable was lost", recs)
	}
}

// A flush that dies after creating several segments leaves files whose
// directory entries were never fsynced. A reopen (after a killed process) sees
// them, so it must make them durable before claiming they are.
func TestOpenMakesSegmentEntriesDurable(t *testing.T) {
	m := newFS(t)
	w := mustOpen(t, m, Options{SegmentSize: 1}) // one record per segment
	for i := range 5 {
		mustAppend(t, w, 1, []byte{byte(i)})
	}
	m.InjectError(vfs.Fault{Op: vfs.OpOpenFile, After: 3}) // dies after creating three segments
	if err := w.Flush(bg); !errors.Is(err, vfs.ErrInjected) {
		t.Fatalf("Flush err = %v", err)
	}
	m.ClearFaults()

	w2 := mustOpen(t, m, Options{SegmentSize: 1}) // process killed, nothing lost yet
	first, _ := readLog(t, m, testDir)
	if len(first) < 3 {
		t.Fatalf("setup: only %d records visible after the kill", len(first))
	}
	end := w2.EndLSN()

	m.Crash(vfs.CrashOptions{}) // now the power goes
	w3 := mustOpen(t, m, Options{SegmentSize: 1})
	second, _ := readLog(t, m, testDir)
	sameRecs(t, second, first, "after the power cut")
	if w3.EndLSN() != end {
		t.Fatalf("EndLSN %d after the power cut, was %d", w3.EndLSN(), end)
	}
}
