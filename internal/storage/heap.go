package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
)

// Heap errors, in addition to those of slotted pages.
var (
	// ErrCorruptHeap means the page chain is damaged: a cycle, or a page
	// that is not a heap page.
	ErrCorruptHeap = errors.New("storage: corrupt heap")
	// ErrInvalidRID means a record ID does not belong to this heap.
	ErrInvalidRID = errors.New("storage: invalid record id")
)

// RID names one row: the page it lives on and its slot in that page.
type RID struct {
	Page uint64
	Slot uint16
}

// String formats the RID like "(page,slot)".
func (r RID) String() string { return fmt.Sprintf("(%d,%d)", r.Page, r.Slot) }

// heapPage is one entry of the free-space map.
type heapPage struct {
	id   uint64
	free int // largest tuple the page could take, as last observed (a hint)
}

// Heap is a table: a chain of slotted pages reached from a first page. See
// docs/design/05-heap-storage.md. It is safe for concurrent use. Only one
// Heap value may exist per heap file at a time.
type Heap struct {
	bp    *BufferPool
	first uint64
	lg    Logger // nil: changes are not logged (Step 1.4 behaviour)

	// growMu serialises appending pages so there is only ever one tail.
	// Lock order: growMu -> page latch -> mu.
	growMu sync.Mutex

	// mu guards pages, index and cursor. It is a leaf lock: never wait for
	// a page latch or the buffer pool while holding it.
	mu     sync.Mutex
	pages  []heapPage     // chain order; the last entry is the tail
	index  map[uint64]int // page ID -> position in pages
	cursor int            // where the free-space search starts
}

// withPage pins page id, latches it (exclusively if write), and runs fn on it
// as a slotted page. fn reports whether it changed the page. Latch and pin
// are always released, in that order.
//
// If markDirty is true (write must be too), the page is marked dirty as soon
// as it is latched, before fn can log a change to it; see PageRef.MarkDirty.
func withPage(ctx context.Context, bp *BufferPool, id uint64, write, markDirty bool, fn func(*SlottedPage) (bool, error)) error {
	ref, err := bp.FetchPage(ctx, id)
	if err != nil {
		return err
	}
	if write {
		ref.Lock()
		if markDirty {
			ref.MarkDirty()
		}
	} else {
		ref.RLock()
	}
	dirty := false
	sp, err := NewSlottedPage(ref.Data())
	if err == nil {
		dirty, err = fn(sp)
	}
	if write {
		ref.Unlock()
	} else {
		ref.RUnlock()
	}
	if uerr := ref.Unpin(dirty && write); err == nil {
		err = uerr
	}
	return err
}

// HeapOption configures a heap.
type HeapOption func(*Heap)

// WithLogger makes every change to the heap go through the write-ahead log:
// each operation becomes one log record, pages carry the LSN of their last
// change, and operations on two pages are atomic. A logged heap needs a
// buffer pool of at least two frames. See docs/design/07-checkpoints-recovery.md.
func WithLogger(lg Logger) HeapOption { return func(h *Heap) { h.lg = lg } }

// CreateHeap allocates the first page of a new, empty heap.
func CreateHeap(ctx context.Context, bp *BufferPool, opts ...HeapOption) (*Heap, error) {
	h := &Heap{bp: bp, index: map[uint64]int{}}
	for _, o := range opts {
		o(h)
	}
	var id uint64
	var err error
	if h.lg != nil {
		id, err = h.newLoggedHeapPage(ctx)
	} else {
		id, err = newHeapPage(ctx, bp)
	}
	if err != nil {
		return nil, fmt.Errorf("creating heap: %w", err)
	}
	h.first = id
	h.pages = []heapPage{{id: id, free: MaxTupleSize}}
	h.index[id] = 0
	return h, nil
}

// newHeapPage allocates and initialises an empty heap page, leaving it dirty
// in the pool.
func newHeapPage(ctx context.Context, bp *BufferPool) (uint64, error) {
	ref, err := bp.NewPage(ctx, PageTypeHeap)
	if err != nil {
		return 0, err
	}
	id := ref.ID()
	ref.Lock()
	err = InitSlottedPage(ref.Data())
	ref.Unlock()
	if uerr := ref.Unpin(true); err == nil {
		err = uerr
	}
	if err != nil {
		// Give the page back; cleanup must not depend on the caller's ctx.
		_ = bp.DeletePage(context.WithoutCancel(ctx), id)
		return 0, err
	}
	return id, nil
}

// OpenHeap opens the heap whose first page is first. It reads the whole
// chain, fully validating every page, and rebuilds the free-space map.
func OpenHeap(ctx context.Context, bp *BufferPool, first uint64, opts ...HeapOption) (*Heap, error) {
	h := &Heap{bp: bp, first: first, index: map[uint64]int{}}
	for _, o := range opts {
		o(h)
	}
	for id := first; id != 0; {
		if _, seen := h.index[id]; seen {
			return nil, fmt.Errorf("opening heap at page %d: chain loops back to page %d: %w", first, id, ErrCorruptHeap)
		}
		var next uint64
		var free int
		err := withPage(ctx, bp, id, false, false, func(sp *SlottedPage) (bool, error) {
			if err := sp.Validate(); err != nil {
				return false, err
			}
			next, free = sp.NextPage(), sp.FreeSpace()
			return false, nil
		})
		if err != nil {
			return nil, fmt.Errorf("opening heap at page %d: page %d: %w", first, id, err)
		}
		h.index[id] = len(h.pages)
		h.pages = append(h.pages, heapPage{id: id, free: free})
		id = next
	}
	if len(h.pages) == 0 {
		return nil, fmt.Errorf("opening heap: no first page: %w", ErrCorruptHeap)
	}
	return h, nil
}

// FirstPage returns the ID that identifies this heap; OpenHeap takes it.
func (h *Heap) FirstPage() uint64 { return h.first }

// NumPages returns the number of pages in the heap.
func (h *Heap) NumPages() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.pages)
}

func (h *Heap) setFree(id uint64, free int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i, ok := h.index[id]; ok {
		h.pages[i].free = free
	}
}

// pickPage finds a page whose hint says a tuple of size fits.
func (h *Heap) pickPage(size int) (uint64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := len(h.pages)
	for i := range n {
		j := (h.cursor + i) % n
		if h.pages[j].free >= size {
			h.cursor = j
			return h.pages[j].id, true
		}
	}
	return 0, false
}

func (h *Heap) checkRID(rid RID) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.index[rid.Page]; !ok {
		return fmt.Errorf("record %s: page is not part of this heap: %w", rid, ErrInvalidRID)
	}
	return nil
}

// grow appends a page unless someone else has made room for size meanwhile.
func (h *Heap) grow(ctx context.Context, size int) error {
	h.growMu.Lock()
	defer h.growMu.Unlock()
	if _, ok := h.pickPage(size); ok {
		return nil
	}
	if h.lg != nil {
		return h.growLogged(ctx)
	}
	newID, err := newHeapPage(ctx, h.bp)
	if err != nil {
		return fmt.Errorf("growing heap: %w", err)
	}
	h.mu.Lock()
	tail := h.pages[len(h.pages)-1].id
	h.mu.Unlock()
	// Link only after the new page exists, so a chain never points at
	// nothing (crash atomicity proper needs the WAL).
	err = withPage(ctx, h.bp, tail, true, false, func(sp *SlottedPage) (bool, error) {
		sp.SetNextPage(newID)
		return true, nil
	})
	if err != nil {
		_ = h.bp.DeletePage(context.WithoutCancel(ctx), newID)
		return fmt.Errorf("growing heap: linking page %d: %w", newID, err)
	}
	h.mu.Lock()
	h.index[newID] = len(h.pages)
	h.pages = append(h.pages, heapPage{id: newID, free: MaxTupleSize})
	h.mu.Unlock()
	return nil
}

// Insert stores data as a new row and returns its RID.
func (h *Heap) Insert(ctx context.Context, data []byte) (RID, error) {
	if err := checkTupleSize(len(data)); err != nil {
		return RID{}, err
	}
	for {
		id, ok := h.pickPage(len(data))
		if !ok {
			if err := h.grow(ctx, len(data)); err != nil {
				return RID{}, err
			}
			continue
		}
		var slot int
		err := withPage(ctx, h.bp, id, true, h.lg != nil, func(sp *SlottedPage) (bool, error) {
			before := h.snapshot(sp)
			s, err := sp.Insert(data)
			h.setFree(id, sp.FreeSpace()) // refresh the hint whatever happened
			if err != nil {
				return false, err
			}
			if err := h.logChanges(ctx, change{sp: sp, id: id, before: before,
				op: HeapBlock{Page: id, Kind: BlockInsert, Slot: uint16(s), Data: data}}); err != nil {
				h.setFree(id, sp.FreeSpace())
				return false, err
			}
			slot = s
			return true, nil
		})
		switch {
		case err == nil:
			return RID{Page: id, Slot: uint16(slot)}, nil
		case errors.Is(err, ErrNoSpace):
			continue // the hint was stale; it is fixed now
		default:
			return RID{}, fmt.Errorf("inserting row: %w", err)
		}
	}
}

// Get returns a copy of the row at rid.
func (h *Heap) Get(ctx context.Context, rid RID) ([]byte, error) {
	if err := h.checkRID(rid); err != nil {
		return nil, err
	}
	var out []byte
	err := withPage(ctx, h.bp, rid.Page, false, false, func(sp *SlottedPage) (bool, error) {
		d, err := sp.Get(int(rid.Slot))
		if err != nil {
			return false, err
		}
		out = bytes.Clone(d)
		return false, nil
	})
	if err != nil {
		return nil, fmt.Errorf("getting row %s: %w", rid, err)
	}
	return out, nil
}

// Delete removes the row at rid.
func (h *Heap) Delete(ctx context.Context, rid RID) error {
	if err := h.checkRID(rid); err != nil {
		return err
	}
	err := withPage(ctx, h.bp, rid.Page, true, h.lg != nil, func(sp *SlottedPage) (bool, error) {
		before := h.snapshot(sp)
		if err := sp.Delete(int(rid.Slot)); err != nil {
			return false, err
		}
		if err := h.logChanges(ctx, change{sp: sp, id: rid.Page, before: before,
			op: HeapBlock{Page: rid.Page, Kind: BlockDelete, Slot: rid.Slot}}); err != nil {
			return false, err
		}
		h.setFree(rid.Page, sp.FreeSpace())
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("deleting row %s: %w", rid, err)
	}
	return nil
}

// Update replaces the row at rid and returns its RID afterwards. If the new
// version fits on the row's page the RID is unchanged. Otherwise the row
// moves to another page and the new RID is returned; callers that remember
// RIDs (indexes) must switch to it.
func (h *Heap) Update(ctx context.Context, rid RID, data []byte) (RID, error) {
	if err := checkTupleSize(len(data)); err != nil {
		return RID{}, err
	}
	if err := h.checkRID(rid); err != nil {
		return RID{}, err
	}
	err := withPage(ctx, h.bp, rid.Page, true, h.lg != nil, func(sp *SlottedPage) (bool, error) {
		before := h.snapshot(sp)
		err := sp.Update(int(rid.Slot), data)
		if err == nil || errors.Is(err, ErrNoSpace) {
			h.setFree(rid.Page, sp.FreeSpace())
		}
		if err != nil {
			return false, err
		}
		if err := h.logChanges(ctx, change{sp: sp, id: rid.Page, before: before,
			op: HeapBlock{Page: rid.Page, Kind: BlockUpdate, Slot: rid.Slot, Data: data}}); err != nil {
			h.setFree(rid.Page, sp.FreeSpace())
			return false, err
		}
		return true, nil
	})
	if err == nil {
		return rid, nil
	}
	if !errors.Is(err, ErrNoSpace) {
		return RID{}, fmt.Errorf("updating row %s: %w", rid, err)
	}
	if h.lg != nil {
		return h.moveLogged(ctx, rid, data)
	}

	// Does not fit on its page: insert the new version first, then retire the
	// old one, so a failure never loses the row.
	newRID, err := h.Insert(ctx, data)
	if err != nil {
		return RID{}, fmt.Errorf("updating row %s: moving it: %w", rid, err)
	}
	if err := h.Delete(ctx, rid); err != nil {
		// Undo the insert so the row is not duplicated.
		if uerr := h.Delete(context.WithoutCancel(ctx), newRID); uerr != nil {
			err = errors.Join(err, uerr)
		}
		return RID{}, fmt.Errorf("updating row %s: removing old version: %w", rid, err)
	}
	return newRID, nil
}

// Scanner iterates over all rows of a heap in page-chain order. It holds no
// pin or latch between calls. It sees each page as of the moment it visits
// it; there is no snapshot isolation (that is Phase 6).
type Scanner struct {
	h    *Heap
	page uint64 // current page, 0 when finished
	slot int    // next slot to look at on that page
}

// Scan starts a scan at the first page.
func (h *Heap) Scan() *Scanner { return &Scanner{h: h, page: h.first} }

// Next returns the next row (a copy of its bytes). ok is false at the end.
func (s *Scanner) Next(ctx context.Context) (rid RID, data []byte, ok bool, err error) {
	for s.page != 0 {
		var found bool
		var next uint64
		err = withPage(ctx, s.h.bp, s.page, false, false, func(sp *SlottedPage) (bool, error) {
			slot := sp.NextLive(s.slot)
			if slot < 0 {
				next = sp.NextPage()
				return false, nil
			}
			d, gerr := sp.Get(slot)
			if gerr != nil {
				return false, gerr
			}
			rid, data, found = RID{Page: s.page, Slot: uint16(slot)}, bytes.Clone(d), true
			s.slot = slot + 1
			return false, nil
		})
		if err != nil {
			return RID{}, nil, false, fmt.Errorf("scanning page %d: %w", s.page, err)
		}
		if found {
			return rid, data, true, nil
		}
		s.page, s.slot = next, 0
	}
	return RID{}, nil, false, nil
}
