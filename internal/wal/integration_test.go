package wal

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

// pageLSNOffset is where a page header keeps its LSN (see
// docs/design/02-page-format.md).
const pageLSNOffset = 8

// This pins the contract between the log and the buffer pool: Writer.FlushedLSN
// is the function for Options.FlushedLSN, a page's LSN is the START LSN of the
// last record that changed it, and a page may be written exactly when that
// record is durable. In particular it must not be written when its LSN equals
// the end of the durable log (the next record, which does not exist yet).
func TestFlushedLSNEnforcesTheWALRuleInTheBufferPool(t *testing.T) {
	m := newFS(t)
	w := mustOpen(t, m, Options{SegmentSize: 120})
	dm, err := storage.Create(m, "/db/data")
	if err != nil {
		t.Fatal(err)
	}
	bp, err := storage.NewBufferPool(dm, storage.Options{
		Frames:     8,
		FlushedLSN: func() uint64 { return uint64(w.FlushedLSN()) },
	})
	if err != nil {
		t.Fatal(err)
	}

	// newPage creates a dirty heap page whose LSN is lsn.
	newPage := func(lsn LSN) uint64 {
		ref, err := bp.NewPage(bg, storage.PageTypeHeap)
		if err != nil {
			t.Fatal(err)
		}
		ref.Lock()
		binary.LittleEndian.PutUint64(ref.Data()[pageLSNOffset:], uint64(lsn))
		ref.Unlock()
		id := ref.ID()
		if err := ref.Unpin(true); err != nil {
			t.Fatal(err)
		}
		return id
	}
	mustBlock := func(id uint64, why string) {
		t.Helper()
		if err := bp.FlushPage(bg, id); !errors.Is(err, storage.ErrWALRule) {
			t.Fatalf("%s: FlushPage err = %v, want ErrWALRule", why, err)
		}
	}
	mustAllow := func(id uint64, why string) {
		t.Helper()
		if err := bp.FlushPage(bg, id); err != nil {
			t.Fatalf("%s: FlushPage err = %v", why, err)
		}
	}

	a := mustAppend(t, w, 1, []byte("change to page A"))
	b := mustAppend(t, w, 1, []byte("change to page B")) // crosses into a second segment
	pageA, pageB := newPage(a), newPage(b)

	mustBlock(pageA, "record A not yet durable")
	mustBlock(pageB, "record B not yet durable")

	if err := w.FlushTo(bg, a); err != nil {
		t.Fatal(err)
	}
	// FlushTo flushed everything buffered, so both records are durable now.
	mustAllow(pageA, "record A durable")
	mustAllow(pageB, "record B durable")

	c := mustAppend(t, w, 1, []byte("change to page C"))
	pageC := newPage(c)
	mustBlock(pageC, "record C buffered only")

	// A page stamped with the log's current end (the LSN the next record
	// WILL get) refers to nothing durable yet.
	pageEnd := newPage(w.DurableEnd())
	mustBlock(pageEnd, "LSN equal to the durable end")

	mustFlush(t, w)
	mustAllow(pageC, "record C durable")
	// The old durable end is now below the new one, so that page is covered;
	// but a page stamped with the NEW durable end (the next record, which
	// does not exist yet) must still be blocked.
	mustAllow(pageEnd, "LSN below the new durable end")
	mustBlock(newPage(w.DurableEnd()), "LSN equal to the new durable end")

	// A page never logged (LSN 0) can always be written.
	mustAllow(newPage(0), "LSN 0")

	// And the crash view agrees: what the pool wrote under the rule is
	// covered by the log that survives.
	m.Crash(vfs.CrashOptions{})
	w2 := mustOpen(t, m, Options{SegmentSize: 120})
	if w2.EndLSN() <= c {
		t.Fatalf("log lost record C: EndLSN %d", w2.EndLSN())
	}
}
