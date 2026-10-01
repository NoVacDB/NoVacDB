package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
)

// change is one page modified by a logged heap operation.
type change struct {
	sp         *SlottedPage
	id         uint64
	before     []byte    // the page before the change: for rollback and the image rule
	op         HeapBlock // logged unless an image is needed
	forceImage bool      // a newly created page: only an image can describe it
}

// snapshot copies the page for rollback, if the heap is logged.
func (h *Heap) snapshot(sp *SlottedPage) []byte {
	if h.lg == nil {
		return nil
	}
	return bytes.Clone(sp.buf)
}

// logChanges logs changes already made to pages the caller holds exclusively
// latched, as one record, and stamps them with its LSN. A page whose LSN was
// below the redo point is logged as a full image (its copy on disk may be torn
// when recovery needs it). If logging fails every page is restored, so the
// buffer pool never holds a change the log does not have. For an unlogged
// heap it does nothing.
func (h *Heap) logChanges(ctx context.Context, changes ...change) error {
	if h.lg == nil {
		return nil
	}
	lsn, err := h.lg.Log(ctx, func(redo uint64) []byte {
		blocks := make([]HeapBlock, len(changes))
		for i, c := range changes {
			if c.forceImage || pageLSN(c.before) < redo {
				blocks[i] = HeapBlock{Page: c.id, Kind: BlockImage, Data: c.sp.buf}
			} else {
				blocks[i] = c.op
			}
		}
		return encodeHeapRecord(blocks)
	})
	if err != nil {
		for _, c := range changes {
			copy(c.sp.buf, c.before)
		}
		return fmt.Errorf("logging heap change: %w", err)
	}
	for _, c := range changes {
		setPageLSN(c.sp.buf, lsn)
	}
	return nil
}

// newLoggedHeapPage allocates a page, makes it an empty heap page and logs
// its image. If logging fails the page is not freed: the record may still
// reach the log, and recovery would then use the page. It stays allocated
// and unreachable (a leak) instead.
func (h *Heap) newLoggedHeapPage(ctx context.Context) (uint64, error) {
	ref, err := h.bp.NewPage(ctx, PageTypeHeap)
	if err != nil {
		return 0, err
	}
	id := ref.ID()
	ref.Lock()
	before := bytes.Clone(ref.Data())
	err = InitSlottedPage(ref.Data())
	var sp *SlottedPage
	if err == nil {
		sp, err = NewSlottedPage(ref.Data())
	}
	if err == nil {
		err = h.logChanges(ctx, change{sp: sp, id: id, before: before, forceImage: true})
	}
	ref.Unlock()
	if uerr := ref.Unpin(err == nil); err == nil {
		err = uerr
	}
	return id, err
}

// withTwoPages pins and exclusively latches two different pages, always in
// ascending page-ID order so two such operations can never deadlock, and runs
// fn on them. fn reports whether it changed them.
func withTwoPages(ctx context.Context, bp *BufferPool, a, b uint64, fn func(spA, spB *SlottedPage) (bool, error)) error {
	refA, err := bp.FetchPage(ctx, a)
	if err != nil {
		return err
	}
	refB, err := bp.FetchPage(ctx, b)
	if err != nil {
		_ = refA.Unpin(false)
		return err
	}
	first, second := refA, refB
	if b < a {
		first, second = refB, refA
	}
	first.Lock()
	second.Lock()
	dirty := false
	spA, err := NewSlottedPage(refA.Data())
	var spB *SlottedPage
	if err == nil {
		spB, err = NewSlottedPage(refB.Data())
	}
	if err == nil {
		dirty, err = fn(spA, spB)
	}
	second.Unlock()
	first.Unlock()
	errA, errB := refA.Unpin(dirty), refB.Unpin(dirty)
	return errors.Join(err, errA, errB)
}

// growLogged appends a page to a logged heap: the new page's image and the
// tail's link to it are one record, so after a crash the chain either has the
// complete new page or does not reach it. The caller holds growMu.
func (h *Heap) growLogged(ctx context.Context) error {
	ref, err := h.bp.NewPage(ctx, PageTypeHeap)
	if err != nil {
		return fmt.Errorf("growing heap: %w", err)
	}
	newID := ref.ID()
	h.mu.Lock()
	tail := h.pages[len(h.pages)-1].id
	h.mu.Unlock()
	tref, err := h.bp.FetchPage(ctx, tail)
	if err != nil {
		// The page stays allocated but unreachable; its record was never made.
		_ = ref.Unpin(false)
		return fmt.Errorf("growing heap: %w", err)
	}
	first, second := ref, tref
	if tail < newID {
		first, second = tref, ref
	}
	first.Lock()
	second.Lock()
	err = func() error {
		newBefore := bytes.Clone(ref.Data())
		tailBefore := bytes.Clone(tref.Data())
		if err := InitSlottedPage(ref.Data()); err != nil {
			return err
		}
		spNew, err := NewSlottedPage(ref.Data())
		if err != nil {
			return err
		}
		spTail, err := NewSlottedPage(tref.Data())
		if err != nil {
			copy(ref.Data(), newBefore)
			return err
		}
		spTail.SetNextPage(newID)
		return h.logChanges(ctx,
			change{sp: spNew, id: newID, before: newBefore, forceImage: true},
			change{sp: spTail, id: tail, before: tailBefore,
				op: HeapBlock{Page: tail, Kind: BlockSetNext, Next: newID}})
	}()
	second.Unlock()
	first.Unlock()
	uerr := errors.Join(ref.Unpin(err == nil), tref.Unpin(err == nil))
	if err != nil {
		return fmt.Errorf("growing heap: %w", err)
	}
	if uerr != nil {
		return uerr
	}
	h.mu.Lock()
	h.index[newID] = len(h.pages)
	h.pages = append(h.pages, heapPage{id: newID, free: MaxTupleSize})
	h.mu.Unlock()
	return nil
}

// pickPageExcept is pickPage that never returns the page skip.
func (h *Heap) pickPageExcept(size int, skip uint64) (uint64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := len(h.pages)
	for i := range n {
		j := (h.cursor + i) % n
		if h.pages[j].id != skip && h.pages[j].free >= size {
			h.cursor = j
			return h.pages[j].id, true
		}
	}
	return 0, false
}

// moveLogged moves a row that no longer fits on its page: the delete on the
// old page and the insert on the new one are a single record, so a crash can
// never leave the row in both places or in neither.
func (h *Heap) moveLogged(ctx context.Context, rid RID, data []byte) (RID, error) {
	for {
		target, ok := h.pickPageExcept(len(data), rid.Page)
		if !ok {
			if err := h.growFor(ctx, len(data), rid.Page); err != nil {
				return RID{}, fmt.Errorf("updating row %s: moving it: %w", rid, err)
			}
			continue
		}
		var newSlot int
		retry := false
		err := withTwoPages(ctx, h.bp, rid.Page, target, func(spOld, spNew *SlottedPage) (bool, error) {
			oldBefore, newBefore := bytes.Clone(spOld.buf), bytes.Clone(spNew.buf)
			if rid.Slot >= uint16(spOld.NumSlots()) || !spOld.isLive(int(rid.Slot)) {
				return false, fmt.Errorf("slot %d: %w", rid.Slot, ErrSlotNotFound)
			}
			s, err := spNew.Insert(data)
			h.setFree(target, spNew.FreeSpace())
			if errors.Is(err, ErrNoSpace) {
				retry = true // the hint was stale; it is fixed now
				return false, nil
			}
			if err != nil {
				return false, err
			}
			if err := spOld.Delete(int(rid.Slot)); err != nil {
				copy(spNew.buf, newBefore)
				h.setFree(target, spNew.FreeSpace())
				return false, err
			}
			if err := h.logChanges(ctx,
				change{sp: spOld, id: rid.Page, before: oldBefore,
					op: HeapBlock{Page: rid.Page, Kind: BlockDelete, Slot: rid.Slot}},
				change{sp: spNew, id: target, before: newBefore,
					op: HeapBlock{Page: target, Kind: BlockInsert, Slot: uint16(s), Data: data}}); err != nil {
				h.setFree(target, spNew.FreeSpace())
				return false, err
			}
			h.setFree(rid.Page, spOld.FreeSpace())
			h.setFree(target, spNew.FreeSpace())
			newSlot = s
			return true, nil
		})
		if err != nil {
			return RID{}, fmt.Errorf("updating row %s: moving it: %w", rid, err)
		}
		if retry {
			continue
		}
		return RID{Page: target, Slot: uint16(newSlot)}, nil
	}
}

// growFor grows the heap unless some page other than skip already has room.
func (h *Heap) growFor(ctx context.Context, size int, skip uint64) error {
	h.growMu.Lock()
	defer h.growMu.Unlock()
	if _, ok := h.pickPageExcept(size, skip); ok {
		return nil
	}
	return h.growLogged(ctx)
}
