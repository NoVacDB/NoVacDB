package wal

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"sync"
	"sync/atomic"

	"github.com/vikrant-choudhary06/NoVacDB/internal/btree"
	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

// Names inside a database directory.
const (
	DataFileName = "data"
	WALDirName   = "wal"
)

// EngineOptions configures an Engine.
type EngineOptions struct {
	// Frames is the buffer pool size, at least 2 (logged heaps change up
	// to two pages at once). Zero means 256.
	Frames int
	// WAL configures the log.
	WAL Options
}

const defaultFrames = 256

// RecoveryStats describes what OpenEngine's recovery did.
type RecoveryStats struct {
	RedoLSN  LSN // where replay started
	EndLSN   LSN // where the log ended
	Replayed int // heap and B+Tree records replayed
	// Statements that began but never committed, and their records,
	// which recovery left out.
	DiscardedStatements int
	Discarded           int
	// Entries of deferred-free records replayed (pages put back on the
	// list to free).
	DeferredFrees int
}

// ErrStatement means a statement-group call was made out of order.
var ErrStatement = errors.New("wal: statement begin and commit out of order")

// Engine is a database directory with crash recovery: a data file, its log,
// a buffer pool that obeys the WAL rule, and checkpoints. See
// docs/design/07-checkpoints-recovery.md. It does not know which heaps and
// trees exist; callers keep their first and root page IDs (until the
// catalog, Step 4.4).
type Engine struct {
	fsys vfs.FS
	dir  string
	w    *Writer
	dm   *storage.DiskManager
	bp   *storage.BufferPool
	lg   *Logger
	ck   *Checkpointer
	rec  RecoveryStats

	mu     sync.Mutex
	closed bool

	// stmtMu is held from BeginStatement to CommitStatement, so there is
	// one statement group at a time and checkpoints wait for it.
	stmtMu    sync.Mutex
	stmtBegin LSN
	// horizon is the open statement's begin LSN, 0 if none: pages with a
	// higher LSN were changed by it and must not reach disk yet.
	horizon atomic.Uint64
}

// OpenEngine opens the database in dir, creating it if it does not exist,
// and recovers it: the log's torn tail is cut off, every record since the
// last checkpoint's redo point is replayed, and an end-of-recovery checkpoint
// is taken. Every change whose record was durable before a crash is then
// present, and no operation is partly visible.
func OpenEngine(ctx context.Context, fsys vfs.FS, dir string, opts EngineOptions) (*Engine, error) {
	if opts.Frames == 0 {
		opts.Frames = defaultFrames
	}
	if opts.Frames < 2 {
		return nil, fmt.Errorf("opening engine: %d frames, need at least 2: %w", opts.Frames, ErrInvalidOptions)
	}
	e := &Engine{fsys: fsys, dir: dir}
	if err := e.open(ctx, opts); err != nil {
		// Report failures to close what was opened too, after the cause.
		return nil, fmt.Errorf("opening engine %s: %w", dir, errors.Join(err, e.closeAll()))
	}
	return e, nil
}

func (e *Engine) open(ctx context.Context, opts EngineOptions) error {
	if err := e.fsys.MkdirAll(e.dir); err != nil {
		return err
	}
	if err := e.fsys.SyncDir(path.Dir(e.dir)); err != nil {
		return err
	}
	walDir := path.Join(e.dir, WALDirName)
	dataPath := path.Join(e.dir, DataFileName)

	// The data file is created before the log, so a log without a data
	// file means the data file was lost.
	dm, err := storage.Open(e.fsys, dataPath)
	if errors.Is(err, vfs.ErrNotExist) {
		if names, lerr := e.fsys.List(walDir); lerr == nil && len(names) > 0 {
			return fmt.Errorf("log exists but the data file is missing: %w", ErrCorrupt)
		}
		dm, err = storage.Create(e.fsys, dataPath)
	}
	if err != nil {
		return err
	}
	e.dm = dm

	if e.w, err = Open(e.fsys, walDir, opts.WAL); err != nil {
		return err
	}
	redo, err := RedoStart(e.fsys, e.dir, walDir)
	if err != nil {
		return err
	}
	flushed, force := RuleHooks(e.w)
	// No page changed by an unfinished statement may reach disk: while a
	// statement is open, the log counts as durable only up to its begin
	// record (docs/design/10-executor.md section 2.4).
	clamped := func() uint64 {
		f := flushed()
		if h := e.horizon.Load(); h != 0 && h < f {
			return h
		}
		return f
	}
	if e.bp, err = storage.NewBufferPool(e.dm, storage.Options{Frames: opts.Frames, FlushedLSN: clamped, FlushWAL: force}); err != nil {
		return err
	}
	// The logger exists before replay so that replayed deferred-free
	// records can put their pages back on its list.
	e.lg = NewLogger(e.w, redo)
	if err := e.replay(ctx, walDir, redo); err != nil {
		return err
	}
	e.ck = NewCheckpointer(e.fsys, e.dir, e.w, e.lg, e.bp, e.dm)
	// End-of-recovery checkpoint: the replayed pages reach disk and the
	// next recovery starts here.
	if _, err := e.ck.Checkpoint(ctx); err != nil {
		return fmt.Errorf("end-of-recovery checkpoint: %w", err)
	}
	return nil
}

// span is a range of LSNs [from, to).
type span struct{ from, to LSN }

// uncommitted finds the statement groups in the log from redo that began
// and never committed: a begin record followed by another begin, a
// checkpoint record or the end of the log. Their records must not be
// replayed.
func (e *Engine) uncommitted(walDir string, redo LSN) ([]span, error) {
	r, err := NewReader(e.fsys, walDir, redo)
	if err != nil {
		return nil, err
	}
	var out []span
	var open LSN // begin LSN of the open group, 0 if none
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("finding statements: %w", err)
		}
		switch rec.Type {
		case RecordStmtBegin:
			if open != 0 {
				out = append(out, span{open, rec.LSN})
			}
			open = rec.LSN
		case RecordStmtCommit:
			if len(rec.Payload) != 8 || open == 0 || LSN(binary.LittleEndian.Uint64(rec.Payload)) != open {
				return nil, fmt.Errorf("statement commit at %d does not match an open statement: %w", rec.LSN, ErrCorrupt)
			}
			open = 0
		case RecordCheckpoint:
			// A checkpoint never runs inside a statement: one that follows
			// an open group means the group was abandoned.
			if open != 0 {
				out = append(out, span{open, rec.LSN})
				open = 0
			}
		}
	}
	if open != 0 {
		out = append(out, span{open, r.End()})
	}
	return out, nil
}

// replay redoes every record from redo to the end of the log, except those
// of statements that never committed.
func (e *Engine) replay(ctx context.Context, walDir string, redo LSN) error {
	skip, err := e.uncommitted(walDir, redo)
	if err != nil {
		return err
	}
	e.rec.DiscardedStatements = len(skip)
	r, err := NewReader(e.fsys, walDir, redo)
	if err != nil {
		return err
	}
	e.rec.RedoLSN = redo
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("replay: %w", err)
		}
		for len(skip) > 0 && rec.LSN >= skip[0].to {
			skip = skip[1:]
		}
		if len(skip) > 0 && rec.LSN >= skip[0].from {
			e.rec.Discarded++
			continue
		}
		switch rec.Type {
		case RecordHeap:
			if err := storage.RedoHeapRecord(ctx, e.bp, uint64(rec.LSN), rec.Payload); err != nil {
				return fmt.Errorf("replay: %w: %w", ErrCorrupt, err)
			}
			e.rec.Replayed++
		case RecordBTree:
			if err := btree.Redo(ctx, e.bp, uint64(rec.LSN), rec.Payload); err != nil {
				return fmt.Errorf("replay: %w: %w", ErrCorrupt, err)
			}
			e.rec.Replayed++
		case RecordDeferredFree:
			// Pages waiting to be freed: back on the list.
			frees, err := DecodeDeferredFree(rec.Payload, rec.LSN)
			if err != nil {
				return fmt.Errorf("replay: record %d: %w", rec.LSN, err)
			}
			for _, d := range frees {
				// The page was allocated before whatever unlinked it.
				if d.page >= e.dm.PageCount() {
					return fmt.Errorf("replay: record %d defers page %d, past the end of the data file: %w", rec.LSN, d.page, ErrCorrupt)
				}
				e.lg.restore(d.page, d.lsn)
			}
			e.rec.DeferredFrees += len(frees)
		case RecordCheckpoint, RecordStmtBegin, RecordStmtCommit:
			// Nothing to redo.
		default:
			return fmt.Errorf("replay: record %d has unknown type %d: %w", rec.LSN, rec.Type, ErrCorrupt)
		}
	}
	// The writer cut the tail at the same place the reader stopped; any
	// difference means the log changed under us or the two disagree.
	if r.End() != e.w.EndLSN() {
		return fmt.Errorf("replay ended at %d, log ends at %d: %w", r.End(), e.w.EndLSN(), ErrCorrupt)
	}
	e.rec.EndLSN = r.End()
	return nil
}

// BeginStatement starts a statement group: every change until
// CommitStatement is one atomic unit across a crash, and none of the pages
// it changes can reach disk before the commit. It waits for any other
// statement or checkpoint. If anything fails after BeginStatement, the
// caller must Abandon the engine: there is no undo.
func (e *Engine) BeginStatement(ctx context.Context) error {
	if err := e.check(); err != nil {
		return err
	}
	e.stmtMu.Lock()
	lsn, err := e.w.Append(ctx, RecordStmtBegin, nil)
	if err != nil {
		e.stmtMu.Unlock()
		return fmt.Errorf("beginning a statement: %w", err)
	}
	e.stmtBegin = lsn
	e.horizon.Store(uint64(lsn))
	return nil
}

// CommitStatement commits the open statement group: it logs the commit
// record, makes the log durable through it, and returns its LSN. On error
// the statement may or may not be committed; the caller must Abandon.
func (e *Engine) CommitStatement(ctx context.Context) (LSN, error) {
	if e.horizon.Load() == 0 {
		return 0, ErrStatement
	}
	payload := binary.LittleEndian.AppendUint64(nil, uint64(e.stmtBegin))
	lsn, err := e.w.Append(ctx, RecordStmtCommit, payload)
	if err == nil {
		err = e.w.FlushTo(ctx, lsn)
	}
	if err != nil {
		return 0, fmt.Errorf("committing a statement: %w", err)
	}
	e.horizon.Store(0)
	e.stmtMu.Unlock()
	return lsn, nil
}

// Abandon closes the engine as a crash would: the buffer pool is dropped
// without being written, so an unfinished statement's changes are lost and
// recovery at the next OpenEngine discards its records. Use it when a
// statement fails after BeginStatement.
func (e *Engine) Abandon() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	e.closed = true
	e.mu.Unlock()
	return e.closeAll()
}

// Recovery returns what recovery did when the engine was opened.
func (e *Engine) Recovery() RecoveryStats { return e.rec }

// Pool returns the buffer pool.
func (e *Engine) Pool() *storage.BufferPool { return e.bp }

// CreateHeap creates a new, logged heap. Its first page ID identifies it; it
// survives a crash once a Flush has returned after CreateHeap.
func (e *Engine) CreateHeap(ctx context.Context) (*storage.Heap, error) {
	if err := e.check(); err != nil {
		return nil, err
	}
	return storage.CreateHeap(ctx, e.bp, storage.WithLogger(e.lg))
}

// OpenHeap opens the logged heap whose first page is first.
func (e *Engine) OpenHeap(ctx context.Context, first uint64) (*storage.Heap, error) {
	if err := e.check(); err != nil {
		return nil, err
	}
	return storage.OpenHeap(ctx, e.bp, first, storage.WithLogger(e.lg))
}

// CreateBTree creates a new, logged B+Tree. Its root page ID identifies it;
// it survives a crash once a Flush has returned after CreateBTree.
func (e *Engine) CreateBTree(ctx context.Context) (*btree.Tree, error) {
	if err := e.check(); err != nil {
		return nil, err
	}
	return btree.Create(ctx, e.bp, btree.WithLogger(e.lg))
}

// OpenBTree opens the logged B+Tree whose root page is root.
func (e *Engine) OpenBTree(ctx context.Context, root uint64) (*btree.Tree, error) {
	if err := e.check(); err != nil {
		return nil, err
	}
	return btree.Open(ctx, e.bp, root, btree.WithLogger(e.lg))
}

// Logger returns the engine's logger.
func (e *Engine) Logger() *Logger { return e.lg }

// FreePageCount returns how many pages of the data file are free.
func (e *Engine) FreePageCount() uint64 { return e.dm.FreePageCount() }

// Flush makes every change made so far durable; it is the point at which
// changes are acknowledged (later, COMMIT).
func (e *Engine) Flush(ctx context.Context) error {
	if err := e.check(); err != nil {
		return err
	}
	return e.w.Flush(ctx)
}

// Checkpoint takes a checkpoint. It waits for an open statement to commit:
// a checkpoint must not write that statement's pages.
func (e *Engine) Checkpoint(ctx context.Context) (Control, error) {
	if err := e.check(); err != nil {
		return Control{}, err
	}
	e.stmtMu.Lock()
	defer e.stmtMu.Unlock()
	return e.ck.Checkpoint(ctx)
}

func (e *Engine) check() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	return nil
}

// Close takes a final checkpoint and closes everything. No heap may be in
// use. Even if the checkpoint fails, the files are closed; nothing
// acknowledged is lost, because the log has it.
func (e *Engine) Close(ctx context.Context) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	e.closed = true
	e.mu.Unlock()
	if e.horizon.Load() != 0 {
		// A statement is open: closing normally would write its pages.
		return errors.Join(fmt.Errorf("closing with a statement open: %w", ErrStatement), e.closeAll())
	}
	_, err := e.ck.Checkpoint(ctx)
	if err == nil {
		err = e.bp.Close(ctx)
	}
	return errors.Join(err, e.closeAll())
}

// closeAll closes whatever was opened, ignoring what was not.
func (e *Engine) closeAll() error {
	var errs []error
	if e.w != nil {
		if err := e.w.Close(context.Background()); err != nil && !errors.Is(err, ErrClosed) {
			errs = append(errs, err)
		}
	}
	if e.dm != nil {
		if err := e.dm.Close(); err != nil && !errors.Is(err, storage.ErrDiskClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
