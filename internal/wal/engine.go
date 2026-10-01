package wal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sync"

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
	Replayed int // heap records replayed
}

// Engine is a database directory with crash recovery: a data file, its log,
// a buffer pool that obeys the WAL rule, and checkpoints. See
// docs/design/07-checkpoints-recovery.md. It does not know which heaps exist;
// callers keep their first page IDs (until the catalog, Step 4.4).
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
	if e.bp, err = storage.NewBufferPool(e.dm, storage.Options{Frames: opts.Frames, FlushedLSN: flushed, FlushWAL: force}); err != nil {
		return err
	}
	if err := e.replay(ctx, walDir, redo); err != nil {
		return err
	}
	e.lg = NewLogger(e.w, redo)
	e.ck = NewCheckpointer(e.fsys, e.dir, e.w, e.lg, e.bp, e.dm)
	// End-of-recovery checkpoint: the replayed pages reach disk and the
	// next recovery starts here.
	if _, err := e.ck.Checkpoint(ctx); err != nil {
		return fmt.Errorf("end-of-recovery checkpoint: %w", err)
	}
	return nil
}

// replay redoes every record from redo to the end of the log.
func (e *Engine) replay(ctx context.Context, walDir string, redo LSN) error {
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
		switch rec.Type {
		case RecordHeap:
			if err := storage.RedoHeapRecord(ctx, e.bp, uint64(rec.LSN), rec.Payload); err != nil {
				return fmt.Errorf("replay: %w: %w", ErrCorrupt, err)
			}
			e.rec.Replayed++
		case RecordCheckpoint:
			// Nothing to redo; recovery already chose its start point.
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

// Flush makes every change made so far durable; it is the point at which
// changes are acknowledged (later, COMMIT).
func (e *Engine) Flush(ctx context.Context) error {
	if err := e.check(); err != nil {
		return err
	}
	return e.w.Flush(ctx)
}

// Checkpoint takes a checkpoint.
func (e *Engine) Checkpoint(ctx context.Context) (Control, error) {
	if err := e.check(); err != nil {
		return Control{}, err
	}
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
