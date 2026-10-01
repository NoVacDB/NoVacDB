package wal

import (
	"context"
	"sync"
)

// Record types written by NoVacDB itself. See
// docs/design/07-checkpoints-recovery.md.
const (
	// RecordHeap is a heap operation (storage.DecodeHeapRecord).
	RecordHeap RecordType = 1
	// RecordCheckpoint marks a completed checkpoint; its payload is the
	// redo LSN.
	RecordCheckpoint RecordType = 2
	// RecordBTree is a B+Tree change (btree.DecodeRecord).
	RecordBTree RecordType = 3
)

// Logger connects heaps and B+Trees to the log (it implements
// storage.Logger and btree.Logger) and holds
// the redo point: the redo LSN of the latest checkpoint that has started. A
// heap logs a full page image for the first change to a page after the redo
// point, so recovery never needs a page's possibly torn copy on disk.
type Logger struct {
	w *Writer

	// mu makes "read the redo point, build the record, append it" atomic
	// with respect to a checkpoint moving the redo point: Log holds it
	// shared, BeginCheckpoint exclusively (and only briefly).
	mu   sync.RWMutex
	redo LSN

	freeMu  sync.Mutex
	pending []deferredFree // pages to free once the redo point passes them
}

// deferredFree is a page unlinked by the record at lsn.
type deferredFree struct{ page, lsn uint64 }

// NewLogger returns a Logger appending to w, with the given redo point (the
// redo LSN recovery started from, or of the last completed checkpoint).
func NewLogger(w *Writer, redo LSN) *Logger { return &Logger{w: w, redo: redo} }

// Log appends a heap record built by build, which receives the current redo
// point. It returns the record's LSN. An error means the record may or may
// not reach the log; the caller undoes its change in memory.
func (l *Logger) Log(ctx context.Context, build func(redoPoint uint64) []byte) (uint64, error) {
	return l.log(ctx, RecordHeap, build)
}

// LogBTree is Log for a B+Tree record.
func (l *Logger) LogBTree(ctx context.Context, build func(redoPoint uint64) []byte) (uint64, error) {
	return l.log(ctx, RecordBTree, build)
}

func (l *Logger) log(ctx context.Context, t RecordType, build func(redoPoint uint64) []byte) (uint64, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	lsn, err := l.w.Append(ctx, t, build(uint64(l.redo)))
	return uint64(lsn), err
}

// DeferFree records that page was unlinked by the record at lsn. The
// checkpointer frees it once a checkpoint's redo point is past lsn, when no
// record that refers to the page can be replayed any more. A crash forgets
// the list; the pages leak.
func (l *Logger) DeferFree(page, lsn uint64) {
	l.freeMu.Lock()
	defer l.freeMu.Unlock()
	l.pending = append(l.pending, deferredFree{page, lsn})
}

// takeFreeable removes and returns the deferred pages whose record lies
// before redo.
func (l *Logger) takeFreeable(redo LSN) []deferredFree {
	l.freeMu.Lock()
	defer l.freeMu.Unlock()
	var out []deferredFree
	keep := l.pending[:0]
	for _, d := range l.pending {
		if LSN(d.lsn) < redo {
			out = append(out, d)
		} else {
			keep = append(keep, d)
		}
	}
	l.pending = keep
	return out
}

// PendingFrees returns how many unlinked pages wait to be freed.
func (l *Logger) PendingFrees() int {
	l.freeMu.Lock()
	defer l.freeMu.Unlock()
	return len(l.pending)
}

// RedoPoint returns the current redo point.
func (l *Logger) RedoPoint() LSN {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.redo
}

// BeginCheckpoint moves the redo point to the current end of the log and
// returns it. Every record already appended has a lower LSN; every record
// appended afterwards was built knowing the new redo point.
func (l *Logger) BeginCheckpoint() LSN {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.redo = l.w.EndLSN()
	return l.redo
}

// RuleHooks returns the two buffer pool hooks (storage.Options.FlushedLSN
// and FlushWAL) that enforce the WAL rule against w.
func RuleHooks(w *Writer) (flushed func() uint64, force func(context.Context, uint64) error) {
	flushed = func() uint64 { return uint64(w.FlushedLSN()) }
	force = func(ctx context.Context, lsn uint64) error { return w.FlushTo(ctx, LSN(lsn)) }
	return flushed, force
}
