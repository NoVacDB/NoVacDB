package storage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Errors returned by the buffer pool, in addition to those of the store.
var (
	// ErrNoFreeFrames means every frame is pinned, or holds a dirty page
	// the WAL rule forbids writing yet, so nothing can be evicted.
	ErrNoFreeFrames = errors.New("storage: no free buffer frames")
	// ErrPagePinned means the operation needs the page to be unpinned.
	ErrPagePinned = errors.New("storage: page is pinned")
	// ErrAlreadyUnpinned means a PageRef was unpinned twice.
	ErrAlreadyUnpinned = errors.New("storage: page already unpinned")
	// ErrWALRule means a page cannot be written because its LSN is beyond
	// what the WAL has made durable.
	ErrWALRule = errors.New("storage: page lsn is ahead of the flushed wal")
	// ErrPoolClosed means the buffer pool was closed.
	ErrPoolClosed = errors.New("storage: buffer pool closed")
	// ErrBadPoolSize means the pool was configured with no frames.
	ErrBadPoolSize = errors.New("storage: buffer pool needs at least one frame")
)

// PageStore is what the buffer pool needs from the layer below. *DiskManager
// satisfies it; tests substitute wrappers that count calls and inject faults.
type PageStore interface {
	ReadPage(ctx context.Context, id uint64, buf []byte) error
	// WritePage seals buf (sets its checksum) and writes it.
	WritePage(ctx context.Context, id uint64, buf []byte) error
	Allocate(ctx context.Context) (uint64, error)
	Free(ctx context.Context, id uint64) error
}

// Options configures a BufferPool.
type Options struct {
	// Frames is the number of 8 KiB page frames, at least 1.
	Frames int
	// FlushedLSN reports how far the WAL is durable. A dirty page whose
	// LSN is greater may not be written to disk. Nil means there is no WAL
	// yet and any page may be written.
	FlushedLSN func() uint64
	// FlushWAL, if set, makes the WAL durable at least up to lsn. The pool
	// calls it instead of failing when the WAL rule is all that stops it from
	// evicting or flushing a page. It must not call back into the pool.
	FlushWAL func(ctx context.Context, lsn uint64) error
}

// Stats are cumulative counters, for tests and later for metrics.
type Stats struct {
	Hits      uint64 // FetchPage found the page in memory
	Misses    uint64 // FetchPage had to read the page from the store
	Evictions uint64 // pages dropped to make room
	Writes    uint64 // pages written back to the store
}

// frame holds one page in memory.
type frame struct {
	data []byte // PageSize bytes, aliasing the pool's arena

	latch   sync.RWMutex // content latch for the bytes of a pinned page
	flushMu sync.Mutex   // serialises writes of this page
	dirty   atomic.Bool  // page differs from the store

	// Guarded by BufferPool.mu.
	pageID   uint64
	valid    bool
	pinCount int
	refBit   bool
}

// BufferPool caches pages of a PageStore in a fixed number of frames. It is
// safe for concurrent use. See docs/design/04-buffer-pool.md.
type BufferPool struct {
	store      PageStore
	flushedLSN func() uint64
	flushWAL   func(ctx context.Context, lsn uint64) error

	mu     sync.Mutex // guards table, free, hand, closed and frame metadata
	frames []*frame
	table  map[uint64]*frame
	free   []*frame // stack of unused frames; popped from the end
	hand   int      // Clock hand
	closed bool

	hits, misses, evictions, writes atomic.Uint64
}

// NewBufferPool creates a pool of opts.Frames frames over store.
func NewBufferPool(store PageStore, opts Options) (*BufferPool, error) {
	if opts.Frames < 1 {
		return nil, fmt.Errorf("creating buffer pool with %d frames: %w", opts.Frames, ErrBadPoolSize)
	}
	if store == nil {
		return nil, errors.New("storage: buffer pool needs a store")
	}
	bp := &BufferPool{
		store:      store,
		flushedLSN: opts.FlushedLSN,
		flushWAL:   opts.FlushWAL,
		frames:     make([]*frame, opts.Frames),
		table:      make(map[uint64]*frame, opts.Frames),
		free:       make([]*frame, 0, opts.Frames),
	}
	arena := make([]byte, opts.Frames*PageSize) // one allocation for all frames
	for i := range bp.frames {
		lo, hi := i*PageSize, (i+1)*PageSize
		bp.frames[i] = &frame{data: arena[lo:hi:hi]}
	}
	// Push in reverse so frame 0 is handed out first (deterministic tests).
	for i := len(bp.frames) - 1; i >= 0; i-- {
		bp.free = append(bp.free, bp.frames[i])
	}
	return bp, nil
}

// PageRef is one pin on one page. Each FetchPage/NewPage returns a new one.
// The bytes from Data are valid until Unpin and must not be used afterwards.
// Hold the exclusive latch (Lock) while modifying the page and the shared
// latch (RLock) while reading it if others may modify it; release latches
// before Unpin.
type PageRef struct {
	pool     *BufferPool
	f        *frame
	id       uint64
	released atomic.Bool
}

// ID returns the page ID.
func (r *PageRef) ID() uint64 { return r.id }

// Data returns the full PageSize bytes of the page, header included.
func (r *PageRef) Data() []byte { return r.f.data }

// Lock takes the exclusive content latch.
func (r *PageRef) Lock() { r.f.latch.Lock() }

// Unlock releases the exclusive content latch.
func (r *PageRef) Unlock() { r.f.latch.Unlock() }

// RLock takes the shared content latch.
func (r *PageRef) RLock() { r.f.latch.RLock() }

// RUnlock releases the shared content latch.
func (r *PageRef) RUnlock() { r.f.latch.RUnlock() }

// MarkDirty marks the page dirty at once, without waiting for Unpin. A logged
// change must do this, under the exclusive latch, before its log record is
// appended: a checkpoint that starts after the append then finds the page
// dirty and writes it. (Marking it only at Unpin leaves a window in which the
// checkpoint misses the page while recovery skips the record.)
func (r *PageRef) MarkDirty() { r.f.dirty.Store(true) }

// Dirty reports whether the page is marked dirty: changed since it was last
// written to the store.
func (r *PageRef) Dirty() bool { return r.f.dirty.Load() }

// Unpin releases the pin. Pass dirty=true if the page was modified. A second
// Unpin of the same PageRef returns ErrAlreadyUnpinned and changes nothing.
func (r *PageRef) Unpin(dirty bool) error {
	if !r.released.CompareAndSwap(false, true) {
		return fmt.Errorf("unpinning page %d: %w", r.id, ErrAlreadyUnpinned)
	}
	// Set dirty before the pin drops, so an evictor that sees pinCount==0
	// also sees the dirty flag.
	if dirty {
		r.f.dirty.Store(true)
	}
	r.pool.unpinFrame(r.f)
	return nil
}

// pageLSN reads the LSN from a page header without validating the page.
func pageLSN(buf []byte) uint64 { return binary.LittleEndian.Uint64(buf[offLSN:]) }

// lsnAllowed implements the WAL rule hook.
func (bp *BufferPool) lsnAllowed(lsn uint64) bool {
	return bp.flushedLSN == nil || lsn <= bp.flushedLSN()
}

func (bp *BufferPool) unpinFrame(f *frame) {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	if f.pinCount > 0 { // always true unless the pool's invariants are broken
		f.pinCount--
	}
}

// getFrame returns an unused frame, evicting a page if necessary. The caller
// holds bp.mu. On error no state has changed.
func (bp *BufferPool) getFrame(ctx context.Context) (*frame, error) {
	if n := len(bp.free); n > 0 {
		f := bp.free[n-1]
		bp.free = bp.free[:n-1]
		return f, nil
	}
	for attempt := 0; ; attempt++ {
		f, walBlocked, err := bp.evict(ctx)
		if f != nil || err != nil {
			return f, err
		}
		// Every candidate was held back only by the WAL rule: force the log
		// (once) and look again.
		if walBlocked == 0 || bp.flushWAL == nil || attempt > 0 {
			return nil, ErrNoFreeFrames
		}
		if err := bp.flushWAL(ctx, walBlocked); err != nil {
			return nil, fmt.Errorf("forcing the wal to evict a page: %w", err)
		}
	}
}

// evict runs the Clock sweep. It returns the freed frame, or the highest LSN
// among dirty pages that could not be written because of the WAL rule.
func (bp *BufferPool) evict(ctx context.Context) (*frame, uint64, error) {
	n := len(bp.frames)
	var walBlocked uint64
	// Two rounds: the first may only clear reference bits.
	for range 2 * n {
		f := bp.frames[bp.hand]
		bp.hand = (bp.hand + 1) % n
		if f.pinCount > 0 {
			continue
		}
		dirty := f.dirty.Load()
		if dirty {
			if lsn := pageLSN(f.data); !bp.lsnAllowed(lsn) {
				walBlocked = max(walBlocked, lsn)
				continue // WAL not durable far enough to write this page yet
			}
		}
		if f.refBit {
			f.refBit = false // second chance
			continue
		}
		if dirty {
			// Unpinned means no one is using the bytes, so write in place.
			if err := bp.store.WritePage(ctx, f.pageID, f.data); err != nil {
				return nil, 0, fmt.Errorf("evicting page %d: %w", f.pageID, err)
			}
			f.dirty.Store(false)
			bp.writes.Add(1)
		}
		delete(bp.table, f.pageID)
		f.valid = false
		bp.evictions.Add(1)
		return f, 0, nil
	}
	return nil, walBlocked, nil
}

// release returns an unused frame to the free list. Caller holds bp.mu.
func (bp *BufferPool) release(f *frame) {
	f.valid = false
	f.pinCount = 0
	f.refBit = false
	f.dirty.Store(false)
	bp.free = append(bp.free, f)
}

// install makes f hold page id, pinned once. Caller holds bp.mu.
func (bp *BufferPool) install(f *frame, id uint64) *PageRef {
	f.pageID, f.valid, f.pinCount, f.refBit = id, true, 1, true
	bp.table[id] = f
	return &PageRef{pool: bp, f: f, id: id}
}

// FetchPage pins page id, reading it from the store if it is not in memory.
// A page that was allocated but never written yields ErrZeroPage.
func (bp *BufferPool) FetchPage(ctx context.Context, id uint64) (*PageRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bp.mu.Lock()
	defer bp.mu.Unlock()
	if bp.closed {
		return nil, ErrPoolClosed
	}
	if f, ok := bp.table[id]; ok {
		f.pinCount++
		f.refBit = true
		bp.hits.Add(1)
		return &PageRef{pool: bp, f: f, id: id}, nil
	}
	f, err := bp.getFrame(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching page %d: %w", id, err)
	}
	if err := bp.store.ReadPage(ctx, id, f.data); err != nil {
		bp.release(f) // never expose a half-loaded frame
		return nil, fmt.Errorf("fetching page %d: %w", id, err)
	}
	bp.misses.Add(1)
	return bp.install(f, id), nil
}

// NewPage allocates a page in the store, initialises its header with type t
// and pins it. The page is dirty: it exists only in memory until flushed.
func (bp *BufferPool) NewPage(ctx context.Context, t PageType) (*PageRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !t.Valid() || t == PageTypeFileHeader || t == PageTypeFree {
		return nil, fmt.Errorf("new page of type %d: %w", t, ErrBadPageType)
	}
	bp.mu.Lock()
	defer bp.mu.Unlock()
	if bp.closed {
		return nil, ErrPoolClosed
	}
	// Get the frame first so a full pool fails before anything is allocated.
	f, err := bp.getFrame(ctx)
	if err != nil {
		return nil, fmt.Errorf("new page: %w", err)
	}
	id, err := bp.store.Allocate(ctx)
	if err != nil {
		bp.release(f)
		return nil, fmt.Errorf("new page: %w", err)
	}
	if err := InitPage(f.data, Header{ID: id, Type: t}); err != nil {
		bp.release(f)
		return nil, err
	}
	f.dirty.Store(true)
	return bp.install(f, id), nil
}

// PinForOverwrite pins page id without reading it from the store: if the
// page is not in memory a zeroed frame is installed for it. It is for callers
// that replace the whole page at once (redo of a full-page image), which is
// how a page whose copy on disk is torn gets rebuilt. The caller must
// overwrite the page under the exclusive latch and unpin it dirty.
func (bp *BufferPool) PinForOverwrite(ctx context.Context, id uint64) (*PageRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bp.mu.Lock()
	defer bp.mu.Unlock()
	if bp.closed {
		return nil, ErrPoolClosed
	}
	if f, ok := bp.table[id]; ok {
		f.pinCount++
		f.refBit = true
		return &PageRef{pool: bp, f: f, id: id}, nil
	}
	f, err := bp.getFrame(ctx)
	if err != nil {
		return nil, fmt.Errorf("pinning page %d for overwrite: %w", id, err)
	}
	clear(f.data)
	return bp.install(f, id), nil
}

// DeletePage frees page id in the store and drops it from the pool without
// writing it. The page must not be pinned.
func (bp *BufferPool) DeletePage(ctx context.Context, id uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bp.mu.Lock()
	defer bp.mu.Unlock()
	if bp.closed {
		return ErrPoolClosed
	}
	f, resident := bp.table[id]
	if resident && f.pinCount > 0 {
		return fmt.Errorf("deleting page %d: %w", id, ErrPagePinned)
	}
	if err := bp.store.Free(ctx, id); err != nil {
		return fmt.Errorf("deleting page %d: %w", id, err)
	}
	if resident {
		delete(bp.table, id)
		bp.release(f)
	}
	return nil
}

// FlushPage writes page id if it is in memory and dirty. It does not fsync.
func (bp *BufferPool) FlushPage(ctx context.Context, id uint64) error {
	bp.mu.Lock()
	if bp.closed {
		bp.mu.Unlock()
		return ErrPoolClosed
	}
	bp.mu.Unlock()
	return bp.flushPage(ctx, id)
}

// flushPage is FlushPage without the closed check, so Close can use it.
func (bp *BufferPool) flushPage(ctx context.Context, id uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bp.mu.Lock()
	f, ok := bp.table[id]
	if !ok || !f.dirty.Load() {
		bp.mu.Unlock()
		return nil
	}
	f.pinCount++ // keeps the frame from being evicted while we write
	bp.mu.Unlock()
	// No pool mutex held from here: never wait for a latch while holding it.
	defer bp.unpinFrame(f)
	return bp.writeFrame(ctx, f, id)
}

// writeFrame writes a copy of a pinned frame. See design doc section 2.
func (bp *BufferPool) writeFrame(ctx context.Context, f *frame, id uint64) error {
	f.flushMu.Lock()
	defer f.flushMu.Unlock()
	if !f.dirty.Load() {
		return nil // another flush got there first
	}
	scratch := make([]byte, PageSize)
	f.latch.RLock()
	copy(scratch, f.data)
	// Clear dirty at copy time: a later change re-dirties the page, so it
	// can cost an extra write but never lose a change.
	f.dirty.Store(false)
	f.latch.RUnlock()
	// The copy is what gets written, so it is the copy's LSN that the log
	// must cover. (Re-reading the live page instead would chase a moving
	// target under concurrent writers.)
	if lsn := pageLSN(scratch); !bp.lsnAllowed(lsn) {
		if bp.flushWAL != nil {
			if err := bp.flushWAL(ctx, lsn); err != nil {
				f.dirty.Store(true)
				return fmt.Errorf("forcing the wal to flush page %d: %w", id, err)
			}
		}
		if !bp.lsnAllowed(lsn) {
			f.dirty.Store(true)
			return fmt.Errorf("flushing page %d with lsn %d: %w", id, lsn, ErrWALRule)
		}
	}
	if err := bp.store.WritePage(ctx, id, scratch); err != nil {
		f.dirty.Store(true)
		return fmt.Errorf("flushing page %d: %w", id, err)
	}
	bp.writes.Add(1)
	return nil
}

// FlushAll writes every page that is dirty when it starts. Errors are joined;
// it keeps going after a failure. It does not fsync.
func (bp *BufferPool) FlushAll(ctx context.Context) error {
	bp.mu.Lock()
	if bp.closed {
		bp.mu.Unlock()
		return ErrPoolClosed
	}
	bp.mu.Unlock()
	return bp.flushAll(ctx)
}

func (bp *BufferPool) flushAll(ctx context.Context) error {
	bp.mu.Lock()
	var ids []uint64
	for id, f := range bp.table {
		if f.dirty.Load() {
			ids = append(ids, id)
		}
	}
	bp.mu.Unlock()
	var errs []error
	for _, id := range ids {
		if err := bp.flushPage(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close flushes all dirty pages and closes the pool. It fails with
// ErrPagePinned, changing nothing, if any page is pinned. It does not fsync
// the store.
func (bp *BufferPool) Close(ctx context.Context) error {
	bp.mu.Lock()
	if bp.closed {
		bp.mu.Unlock()
		return ErrPoolClosed
	}
	for _, f := range bp.frames {
		if f.valid && f.pinCount > 0 {
			bp.mu.Unlock()
			return fmt.Errorf("closing buffer pool: page %d: %w", f.pageID, ErrPagePinned)
		}
	}
	bp.closed = true // stop new pins while flushing
	bp.mu.Unlock()
	if err := bp.flushAll(ctx); err != nil {
		bp.mu.Lock()
		bp.closed = false // dirty pages remain; let the caller retry
		bp.mu.Unlock()
		return fmt.Errorf("closing buffer pool: %w", err)
	}
	return nil
}

// Stats returns a snapshot of the counters.
func (bp *BufferPool) Stats() Stats {
	return Stats{
		Hits:      bp.hits.Load(),
		Misses:    bp.misses.Load(),
		Evictions: bp.evictions.Load(),
		Writes:    bp.writes.Load(),
	}
}
