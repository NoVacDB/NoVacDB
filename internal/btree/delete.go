package btree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"

	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
)

// held is a node on a delete's path, pinned and exclusively latched.
type held struct {
	ref *storage.PageRef
	n   node
	pos int // position in the parent (for childAt); unused for the top
}

// Delete removes key and reports whether it was present. Nodes left
// underfull are merged with or refilled from a sibling, and an internal root
// left with one child is collapsed into it (design doc section 2.5).
func (t *Tree) Delete(ctx context.Context, key []byte) (bool, error) {
	if len(key) > MaxKeySize {
		return false, fmt.Errorf("delete: %d-byte key: %w", len(key), ErrKeyTooLarge)
	}
	found, freed, err := t.delete(ctx, key)
	// Pages are freed once nothing is latched: no one can reach them. In a
	// logged tree only once no record that refers to them can be replayed.
	for _, f := range freed {
		if t.lg != nil {
			t.lg.DeferFree(f.page, f.lsn)
		} else {
			err = errors.Join(err, t.freePage(ctx, f.page))
		}
	}
	if err != nil {
		return found, fmt.Errorf("delete: %w", err)
	}
	return found, nil
}

// unlinked is a page a merge or collapse removed from the tree, by the
// record at lsn.
type unlinked struct{ page, lsn uint64 }

func (t *Tree) delete(ctx context.Context, key []byte) (found bool, freed []unlinked, err error) {
	ref, n, err := t.fetch(ctx, t.root, true)
	if err != nil {
		return false, nil, err
	}
	path := []held{{ref: ref, n: n}}
	defer func() {
		for _, h := range path {
			release(h.ref, true)
		}
	}()
	// Descend, keeping the latches above a node only while it is unsafe.
	for !n.isLeaf() {
		pos, c, err := n.childFor(key)
		if err != nil {
			return false, nil, fmt.Errorf("page %d: %w", path[len(path)-1].ref.ID(), err)
		}
		cref, cn, err := t.childOf(ctx, n, c, true)
		if err != nil {
			return false, nil, err
		}
		safe, err := deleteSafe(cn, key)
		if err != nil {
			release(cref, true)
			return false, nil, fmt.Errorf("page %d: %w", c, err)
		}
		if safe {
			for _, h := range path {
				release(h.ref, true)
			}
			path = path[:0]
		}
		path = append(path, held{ref: cref, n: cn, pos: pos})
		n = cn
	}

	t.ops.deletes.Add(1)
	t.ops.latchesAtLeaf.Add(uint64(len(path)))
	leaf := path[len(path)-1]
	i, found, err := leaf.n.search(key)
	if err != nil {
		return false, nil, fmt.Errorf("page %d: %w", leaf.ref.ID(), err)
	}
	if !found {
		return false, nil, nil
	}
	m := t.mutation()
	m.touch(leaf.ref, &Block{Kind: BlockLeafDelete, Index: uint16(i), Key: key})
	if err := leaf.n.remove(i); err != nil {
		return false, nil, fmt.Errorf("page %d: %w", leaf.ref.ID(), err)
	}
	if _, err := m.commit(ctx); err != nil {
		return false, nil, err
	}

	// Repair bottom-up. Every node checked here is either the leaf or was
	// unsafe, so any of them may be underfull (including from an earlier
	// crash or a skipped redistribution).
	for j := len(path) - 1; j >= 1; j-- {
		under, err := underfull(path[j].n)
		if err != nil {
			return true, freed, fmt.Errorf("page %d: %w", path[j].ref.ID(), err)
		}
		if !under {
			continue
		}
		survivor, gone, err := t.repair(ctx, path[j-1], path[j])
		if err != nil {
			return true, freed, err
		}
		path[j] = survivor
		if gone.page != 0 {
			freed = append(freed, gone)
		}
	}
	if path[0].ref.ID() == t.root && len(path) > 1 {
		lsn, gone, err := t.collapseRoot(ctx, path[0], path[1])
		if err != nil {
			return true, freed, err
		}
		if gone {
			t.ops.collapses.Add(1)
			release(path[1].ref, true)
			freed = append(freed, unlinked{path[1].ref.ID(), lsn})
			path = append(path[:1], path[2:]...) // the rest stay latched
		}
	}
	return true, freed, nil
}

// deleteSafe reports whether deleting key below (or at) node n can leave n
// underfull: a leaf loses the key's cell, if it has it; an internal node can
// lose at most one separator to a repair of its children.
func deleteSafe(n node, key []byte) (bool, error) {
	used, err := n.used()
	if err != nil {
		return false, err
	}
	loss := maxInnerCell + slotSize
	if n.isLeaf() {
		i, found, err := n.search(key)
		if err != nil || !found {
			return true, err
		}
		_, size, err := n.cellBounds(i)
		if err != nil {
			return false, err
		}
		loss = size + slotSize
	}
	return used-loss >= underfullSize, nil
}

func underfull(n node) (bool, error) {
	used, err := n.used()
	return used < underfullSize, err
}

// repair fixes the underfull node x, a child of p, with a sibling: merging
// the two if they fit in one node, else moving cells from the sibling.
// It returns the node that now holds x's keys (still latched) and the page
// unlinked by a merge, if any (no longer latched or pinned). If x has no
// sibling it stays underfull, which is allowed. If it fails nothing has
// changed.
func (t *Tree) repair(ctx context.Context, p, x held) (held, unlinked, error) {
	if p.n.numCells() == 0 {
		return x, unlinked{}, nil
	}
	sibPos := x.pos - 1
	if x.pos == 0 {
		sibPos = 1
	}
	sid, err := p.n.childAt(sibPos)
	if err != nil {
		return x, unlinked{}, fmt.Errorf("page %d: %w", p.ref.ID(), err)
	}
	sref, sn, err := t.childOf(ctx, p.n, sid, true)
	if err != nil {
		return x, unlinked{}, err
	}
	s := held{ref: sref, n: sn, pos: sibPos}
	l, r := s, x
	if x.pos == 0 {
		l, r = x, s
	}
	if l.n.isLeaf() != r.n.isLeaf() {
		release(sref, true)
		return x, unlinked{}, corrupt("siblings %d and %d are of different kinds", l.ref.ID(), r.ref.ID())
	}
	sepIdx := r.pos - 1
	k, err := p.n.key(sepIdx)
	if err != nil {
		release(sref, true)
		return x, unlinked{}, fmt.Errorf("page %d: %w", p.ref.ID(), err)
	}
	sep := bytes.Clone(k)

	merged, err := mergeNodes(l.n, r.n, sep)
	if err != nil {
		release(sref, true)
		return x, unlinked{}, err
	}
	if merged != nil {
		pb := node{bytes.Clone(p.n.b)}
		if err := pb.remove(sepIdx); err != nil {
			release(sref, true)
			return x, unlinked{}, fmt.Errorf("page %d: %w", p.ref.ID(), err)
		}
		m := t.mutation()
		m.touch(p.ref, nil)
		m.touch(l.ref, nil)
		copy(p.n.b, pb.b)
		copy(l.n.b, merged)
		lsn, err := m.commit(ctx)
		if err != nil {
			release(sref, true)
			return x, unlinked{}, err
		}
		t.ops.merges[kindIndex(l.n)].Add(1)
		release(r.ref, true)
		return l, unlinked{r.ref.ID(), lsn}, nil
	}

	lb, rb, newSep, err := redistribute(l.n, r.n, sep, x.pos == 0)
	if err == nil && lb != nil {
		var pb []byte
		pb, err = replaceSeparator(p.n, sepIdx, newSep, r.ref.ID())
		if err == nil && pb != nil {
			m := t.mutation()
			m.touch(p.ref, nil)
			m.touch(l.ref, nil)
			m.touch(r.ref, nil)
			copy(p.n.b, pb)
			copy(l.n.b, lb)
			copy(r.n.b, rb)
			if _, err = m.commit(ctx); err == nil {
				dir := 0
				if x.pos != 0 {
					dir = 1
				}
				t.ops.redistributions[kindIndex(l.n)][dir].Add(1)
			}
		}
	}
	release(sref, true)
	return x, unlinked{}, err
}

// mergeNodes returns l's page with r's content appended (for internal nodes,
// after the separator coming down with r's Child0), or nil if it does not
// fit in one node.
func mergeNodes(l, r node, sep []byte) ([]byte, error) {
	ul, err := l.used()
	if err != nil {
		return nil, err
	}
	ur, err := r.used()
	if err != nil {
		return nil, err
	}
	total := ul + ur
	if !l.isLeaf() {
		total += innerCellSize(sep) + slotSize
	}
	if total > nodeSpace {
		return nil, nil
	}
	out := node{bytes.Clone(l.b)}
	if !l.isLeaf() {
		if err := out.insertInner(out.numCells(), sep, r.child0()); err != nil {
			return nil, err
		}
	}
	rc, _, err := cells(r)
	if err != nil {
		return nil, err
	}
	for _, c := range rc {
		if err := out.appendRaw(c); err != nil {
			return nil, err
		}
	}
	return out.b, nil
}

// redistribute moves cells between siblings l and r into the underfull one
// (l if toLeft), one at a time, until it is no longer underfull or another
// move would leave the other underfull. It returns the new pages and the new
// separator, or nil pages if nothing could move.
func redistribute(l, r node, sep []byte, toLeft bool) (lb, rb, newSep []byte, err error) {
	ln, rn := node{bytes.Clone(l.b)}, node{bytes.Clone(r.b)}
	leaf := l.isLeaf()
	sep = bytes.Clone(sep)
	moved := 0
	for {
		dst, src := ln, rn
		if !toLeft {
			dst, src = rn, ln
		}
		du, err := dst.used()
		if err != nil {
			return nil, nil, nil, err
		}
		if du >= underfullSize || src.numCells() == 0 {
			break
		}
		su, err := src.used()
		if err != nil {
			return nil, nil, nil, err
		}
		from := 0 // the source cell that leaves: R's first or L's last
		if !toLeft {
			from = src.numCells() - 1
		}
		_, size, err := src.cellBounds(from)
		if err != nil {
			return nil, nil, nil, err
		}
		if su-size-slotSize < underfullSize {
			break
		}
		if err := moveOne(ln, rn, &sep, leaf, toLeft); err != nil {
			if errors.Is(err, errNodeFull) {
				break
			}
			return nil, nil, nil, err
		}
		moved++
	}
	if moved == 0 {
		return nil, nil, nil, nil
	}
	if leaf {
		k, err := rn.key(0)
		if err != nil {
			return nil, nil, nil, err
		}
		sep = bytes.Clone(k)
	}
	return ln.b, rn.b, sep, nil
}

// moveOne moves one cell across the boundary between l and r. For leaves the
// cell itself moves; for internal nodes it rotates through the separator.
func moveOne(l, r node, sep *[]byte, leaf, toLeft bool) error {
	switch {
	case leaf && toLeft:
		c, err := rawCell(r, 0)
		if err == nil {
			err = l.appendRaw(c)
		}
		if err == nil {
			err = r.remove(0)
		}
		return err
	case leaf:
		last := l.numCells() - 1
		c, err := rawCell(l, last)
		if err == nil {
			err = r.insertRaw(0, c)
		}
		if err == nil {
			err = l.remove(last)
		}
		return err
	case toLeft:
		// (sep, r.Child0) joins l; r's first key moves up; its child
		// becomes r's Child0.
		k, err := r.key(0)
		if err != nil {
			return err
		}
		c, err := r.cellChild(0)
		if err != nil {
			return err
		}
		k = bytes.Clone(k)
		if err := l.insertInner(l.numCells(), *sep, r.child0()); err != nil {
			return err
		}
		r.setChild0(c)
		*sep = k
		return r.remove(0)
	default:
		// (sep, r.Child0) becomes r's first cell; l's last key moves up;
		// its child becomes r's Child0.
		last := l.numCells() - 1
		k, err := l.key(last)
		if err != nil {
			return err
		}
		c, err := l.cellChild(last)
		if err != nil {
			return err
		}
		k = bytes.Clone(k)
		if err := r.insertInner(0, *sep, r.child0()); err != nil {
			return err
		}
		r.setChild0(c)
		*sep = k
		return l.remove(last)
	}
}

// rawCell returns a copy of cell i's bytes.
func rawCell(n node, i int) ([]byte, error) {
	off, size, err := n.cellBounds(i)
	if err != nil {
		return nil, err
	}
	return bytes.Clone(n.b[off : off+size]), nil
}

// replaceSeparator returns a copy of p with separator cell i replaced by
// (key, child), or nil if the new cell does not fit.
func replaceSeparator(p node, i int, key []byte, child uint64) ([]byte, error) {
	out := node{bytes.Clone(p.b)}
	if err := out.remove(i); err != nil {
		return nil, err
	}
	err := out.insertInner(i, key, child)
	if errors.Is(err, errNodeFull) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return out.b, nil
}

// collapseRoot replaces an internal root that has no keys with the content
// of its only child, c, which the caller then frees. It reports whether it
// did, and the LSN of the record that did it.
func (t *Tree) collapseRoot(ctx context.Context, root, c held) (uint64, bool, error) {
	if root.n.isLeaf() || root.n.numCells() != 0 || root.n.child0() != c.ref.ID() {
		return 0, false, nil
	}
	h, err := storage.DecodeHeader(root.n.b)
	if err != nil {
		return 0, false, err
	}
	buf := make([]byte, storage.PageSize)
	if err := storage.InitPage(buf, storage.Header{ID: h.ID, LSN: h.LSN, Type: c.n.pageType()}); err != nil {
		return 0, false, err
	}
	copy(buf[storage.HeaderSize:], c.n.b[storage.HeaderSize:])
	m := t.mutation()
	m.touch(root.ref, nil)
	copy(root.n.b, buf)
	lsn, err := m.commit(ctx)
	if err != nil {
		return 0, false, err
	}
	return lsn, true, nil
}

// freePage frees a page no node refers to any more. A reader that latched
// the page before it was unlinked may still hold a pin for an instant after
// releasing its latch, so a pinned page is retried.
func (t *Tree) freePage(ctx context.Context, id uint64) error {
	for {
		err := t.bp.DeletePage(ctx, id)
		if !errors.Is(err, storage.ErrPagePinned) {
			return err
		}
		runtime.Gosched()
	}
}
