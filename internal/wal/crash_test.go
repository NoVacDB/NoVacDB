package wal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

// runCrashScenario drives a writer through random appends and flushes with
// random injected I/O errors, power-cuts it (optionally tearing the last
// write), reopens, and checks the two promises of a write-ahead log:
//
//  1. every record acknowledged by a successful flush is still there, and
//  2. what is there is exactly a prefix of what was appended (nothing
//     invented, nothing reordered, nothing half-written).
//
// It repeats for several crash cycles on the same log.
// crashStats counts the interesting things a scenario did, so the test can
// prove it is not passing vacuously.
type crashStats struct {
	faulted, lostRecords, multiSegment, acked, processKills int
}

func runCrashScenario(t *testing.T, seed uint64) (st crashStats) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, seed^0xabcdef))
	m := vfs.NewMemFS(seed)
	if err := m.MkdirAll("/db"); err != nil {
		t.Fatal(err)
	}
	if err := m.SyncDir("/"); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		SegmentSize:     []int64{0, 80, 200, 500, 2000}[rng.IntN(5)],
		MaxPendingBytes: []int{0, 100, 400}[rng.IntN(3)],
	}
	trace := func(format string, args ...any) {
		if os.Getenv("NOVACDB_TRACE") != "" {
			t.Logf(format, args...)
		}
	}
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("seed %d, options %+v: %s", seed, opts, fmt.Sprintf(format, args...))
	}
	w, err := Open(m, testDir, opts)
	if err != nil {
		fail("Open: %v", err)
	}

	var appended []logRec // everything the writer handed out an LSN for, in order
	acked := 0            // appended[:acked] were acknowledged durable

	for cycle := range 1 + rng.IntN(4) {
		if rng.IntN(10) < 7 {
			ops := []vfs.Op{vfs.OpWriteAt, vfs.OpSync, vfs.OpOpenFile, vfs.OpSyncDir, vfs.OpRemove}
			m.InjectError(vfs.Fault{Op: ops[rng.IntN(len(ops))], After: rng.IntN(40)})
		}
	ops:
		for range 5 + rng.IntN(80) {
			switch r := rng.IntN(100); {
			case r < 70: // append
				typ := RecordType(1 + rng.IntN(7))
				p := payload(rng, rng.IntN(150))
				lsn, err := w.Append(bg, typ, p)
				trace("append type %d len %d -> lsn %d err %v", typ, len(p), lsn, err)
				if lsn != 0 {
					appended = append(appended, logRec{lsn, typ, p})
				}
				if err != nil {
					st.faulted++
					break ops
				}
			case r < 85: // flush everything
				err := w.Flush(bg)
				trace("flush -> %v (appended %d)", err, len(appended))
				if err != nil {
					st.faulted++
					break ops
				}
				acked = len(appended)
			case r < 97 && len(appended) > 0: // flush up to one record
				i := rng.IntN(len(appended))
				err := w.FlushTo(bg, appended[i].LSN)
				trace("flushTo record %d (lsn %d) -> %v", i, appended[i].LSN, err)
				if err != nil {
					st.faulted++
					break ops
				}
				acked = max(acked, i+1)
				if w.FlushedLSN() < appended[i].LSN {
					fail("FlushTo(%d) returned but FlushedLSN is %d", appended[i].LSN, w.FlushedLSN())
				}
			default: // clean close and reopen
				err := w.Close(bg)
				trace("close -> %v", err)
				if err != nil {
					break ops
				}
				acked = len(appended)
				nw, err := Open(m, testDir, opts)
				trace("reopen -> %v", err)
				if err != nil {
					break ops
				}
				w = nw
			}
		}
		m.ClearFaults()
		// Three kinds of death: a power cut that tears the last write, a
		// plain power cut, and a killed process (the OS keeps everything it
		// was handed, synced or not, until a later power cut).
		kind := rng.IntN(4)
		trace("cycle %d: death kind %d (0=process kill, 1=power cut, else torn) acked=%d appended=%d", cycle, kind, acked, len(appended))
		switch kind {
		case 0:
			st.processKills++
		case 1:
			m.Crash(vfs.CrashOptions{})
		default:
			m.Crash(vfs.CrashOptions{TearLast: true})
		}

		// Before the writer repairs anything, the reader must already see
		// exactly the log recovery will keep.
		var preRead []logRec
		if r, rerr := NewReader(m, testDir, 0); rerr == nil {
			for {
				rec, err := r.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					fail("cycle %d: reader before recovery: %v", cycle, err)
				}
				preRead = append(preRead, logRec{rec.LSN, rec.Type, bytes.Clone(rec.Payload)})
			}
		} else if !errors.Is(rerr, ErrLSNNotFound) {
			fail("cycle %d: NewReader before recovery: %v", cycle, rerr)
		}

		w, err = Open(m, testDir, opts)
		if err != nil {
			fail("cycle %d: Open after crash: %v", cycle, err)
		}
		got, segs := readLog(t, m, testDir)
		trace("cycle %d: recovered %d records, EndLSN %d", cycle, len(got), w.EndLSN())
		if len(preRead) != len(got) {
			fail("cycle %d: reader saw %d records before recovery, recovery kept %d", cycle, len(preRead), len(got))
		}
		for i := range got {
			if preRead[i].LSN != got[i].LSN || string(preRead[i].Payload) != string(got[i].Payload) {
				fail("cycle %d: reader and recovery disagree at record %d", cycle, i)
			}
		}
		if len(got) < acked || len(got) > len(appended) {
			fail("cycle %d: recovered %d records; %d were acknowledged and %d appended", cycle, len(got), acked, len(appended))
		}
		for i, r := range got {
			want := appended[i]
			if r.LSN != want.LSN || r.Type != want.Type || string(r.Payload) != string(want.Payload) {
				fail("cycle %d: recovered record %d is %v, appended %v", cycle, i, r, want)
			}
		}
		last := segs[len(segs)-1]
		if end := last.Start + LSN(last.Size); w.EndLSN() != end || w.DurableEnd() != end {
			fail("cycle %d: EndLSN %d DurableEnd %d, log ends at %d", cycle, w.EndLSN(), w.DurableEnd(), end)
		}
		// What survived is durable now; what did not is gone for good.
		if len(got) < len(appended) {
			st.lostRecords++
		}
		if len(segs) > 1 {
			st.multiSegment++
		}
		st.acked += acked
		appended = appended[:len(got)]
		acked = len(appended)
	}
	if err := w.Close(bg); err != nil {
		fail("final Close: %v", err)
	}
	return st
}

func TestCrashRecoveryRandom(t *testing.T) {
	base := testSeed(t)
	runs := 3000
	if testing.Short() {
		runs = 300
	}
	var total crashStats
	for i := range runs {
		st := runCrashScenario(t, base+uint64(i))
		total.faulted += st.faulted
		total.lostRecords += st.lostRecords
		total.multiSegment += st.multiSegment
		total.acked += st.acked
		total.processKills += st.processKills
	}
	t.Logf("%d runs: %+v", runs, total)
	// The test only proves something if the interesting cases really happened.
	for name, n := range map[string]int{
		"injected faults hit": total.faulted, "crashes that lost records": total.lostRecords,
		"crashes with several segments": total.multiSegment, "process kills (no power cut)": total.processKills, "acknowledged records checked": total.acked,
	} {
		if n < runs/10 {
			t.Errorf("only %d %s in %d runs: the scenarios are not exercising enough", n, name, runs)
		}
	}
}

// --- concurrency ---------------------------------------------------------------------

func TestConcurrentAppendFlushThenCrash(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"small segments", Options{SegmentSize: 400, MaxPendingBytes: 600}},
		{"default", Options{}},
		{"tiny buffer", Options{SegmentSize: 300, MaxPendingBytes: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runConcurrent(t, tc.opts, 8, 150)
		})
	}
}

type ackedRec struct {
	rec logRec
	seq int
}

func runConcurrent(t *testing.T, opts Options, workers, perWorker int) {
	t.Helper()
	m := newFS(t)
	w := mustOpen(t, m, opts)

	var mu sync.Mutex
	all := map[LSN]logRec{} // everything appended
	var acked []ackedRec    // acknowledged durable by FlushTo/Flush
	perGoroutine := make([][]LSN, workers)

	var watch sync.WaitGroup
	stop := make(chan struct{})
	var monotone atomic.Bool
	monotone.Store(true)
	watch.Add(1)
	go func() { // DurableEnd must never move backwards
		defer watch.Done()
		var prev LSN
		for {
			select {
			case <-stop:
				return
			default:
			}
			cur := w.DurableEnd()
			if cur < prev {
				monotone.Store(false)
			}
			prev = cur
		}
	}()

	var wg sync.WaitGroup
	for g := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(g), 99))
			for seq := range perWorker {
				p := make([]byte, 5+rng.IntN(100))
				p[0] = byte(g)
				p[1], p[2], p[3], p[4] = byte(seq), byte(seq>>8), 0, 0
				typ := RecordType(1 + g)
				lsn, err := w.Append(bg, typ, p)
				if err != nil {
					t.Errorf("worker %d: Append: %v", g, err)
					return
				}
				rec := logRec{lsn, typ, p}
				mu.Lock()
				if _, dup := all[lsn]; dup {
					t.Errorf("LSN %d handed out twice", lsn)
				}
				all[lsn] = rec
				perGoroutine[g] = append(perGoroutine[g], lsn)
				mu.Unlock()
				if rng.IntN(5) == 0 {
					if err := w.FlushTo(bg, lsn); err != nil {
						t.Errorf("worker %d: FlushTo: %v", g, err)
						return
					}
					if w.FlushedLSN() < lsn {
						t.Errorf("FlushTo(%d) returned with FlushedLSN %d", lsn, w.FlushedLSN())
					}
					mu.Lock()
					acked = append(acked, ackedRec{rec, seq})
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	watch.Wait()
	if !monotone.Load() {
		t.Fatal("DurableEnd moved backwards")
	}
	if t.Failed() {
		return
	}

	// Power cut without Close: acknowledged records must survive.
	m.Crash(vfs.CrashOptions{TearLast: true})
	w2 := mustOpen(t, m, opts)
	got, _ := readLog(t, m, testDir)
	if len(acked) == 0 {
		t.Fatal("test never acknowledged anything")
	}
	byLSN := map[LSN]logRec{}
	var prev LSN
	for i, r := range got {
		if i > 0 && r.LSN <= prev {
			t.Fatalf("LSNs not increasing: %d after %d", r.LSN, prev)
		}
		prev = r.LSN
		want, ok := all[r.LSN]
		if !ok || want.Type != r.Type || string(want.Payload) != string(r.Payload) {
			t.Fatalf("recovered record %v was never appended", r)
		}
		byLSN[r.LSN] = r
	}
	for _, a := range acked {
		if _, ok := byLSN[a.rec.LSN]; !ok {
			t.Fatalf("acknowledged record %v (worker seq %d) lost in the crash", a.rec, a.seq)
		}
	}
	// Records are a prefix of the LSN order of everything appended: nothing
	// after a gap may survive.
	var allLSNs []LSN
	for l := range all {
		allLSNs = append(allLSNs, l)
	}
	sortLSNs(allLSNs)
	for i, r := range got {
		if r.LSN != allLSNs[i] {
			t.Fatalf("recovered log is not a prefix of the appended records at index %d", i)
		}
	}
	// Per-goroutine order is preserved by LSN order.
	for g, lsns := range perGoroutine {
		for i := 1; i < len(lsns); i++ {
			if lsns[i] <= lsns[i-1] {
				t.Fatalf("worker %d: LSNs out of order: %d then %d", g, lsns[i-1], lsns[i])
			}
		}
	}
	if err := w2.Close(bg); err != nil {
		t.Fatal(err)
	}
}

func sortLSNs(s []LSN) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Many goroutines flushing at once must all return nil exactly when their own
// record is durable, with no lost wakeups or deadlock.
func TestConcurrentFlushToAllReturn(t *testing.T) {
	m := newFS(t)
	w := mustOpen(t, m, Options{SegmentSize: 500})
	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for g := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 40 {
				lsn, err := w.Append(bg, 1, []byte{byte(g)})
				if err == nil {
					err = w.FlushTo(bg, lsn)
				}
				if err == nil && w.FlushedLSN() < lsn {
					err = fmt.Errorf("FlushTo(%d) returned but FlushedLSN is %d", lsn, w.FlushedLSN())
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	mustClose(t, w)
	recs, _ := readLog(t, m, testDir)
	if len(recs) != workers*40 {
		t.Fatalf("%d records, want %d", len(recs), workers*40)
	}
}

func TestConcurrentCloseDuringAppends(t *testing.T) {
	// Appends racing with Close must each either succeed (and then be in the
	// flushed log) or get ErrClosed; never a lost record or a panic.
	for i := range 30 {
		m := newFS(t)
		w := mustOpen(t, m, Options{SegmentSize: 300})
		var wg sync.WaitGroup
		var mu sync.Mutex
		okRecs := map[LSN]bool{}
		for g := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 50 {
					lsn, err := w.Append(bg, 1, []byte{byte(g)})
					if err == nil {
						mu.Lock()
						okRecs[lsn] = true
						mu.Unlock()
					} else if !errors.Is(err, ErrClosed) {
						t.Errorf("Append err = %v", err)
						return
					}
				}
			}()
		}
		if err := w.Close(bg); err != nil {
			t.Fatalf("iteration %d: Close: %v", i, err)
		}
		wg.Wait()
		mustOpen(t, m, Options{SegmentSize: 300})
		got, _ := readLog(t, m, testDir)
		onDisk := map[LSN]bool{}
		for _, r := range got {
			onDisk[r.LSN] = true
		}
		// Appends that returned before Close took the lock are in the log; an
		// Append that slipped in after Close flushed would be acknowledged
		// but lost, which this check forbids.
		for lsn := range okRecs {
			if !onDisk[lsn] {
				t.Fatalf("iteration %d: Append returned LSN %d but it is not in the flushed log", i, lsn)
			}
		}
	}
}
