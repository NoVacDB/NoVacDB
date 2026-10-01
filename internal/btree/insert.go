package btree

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
)

// ErrKeyExists means Insert found the key already in the tree.
var ErrKeyExists = errors.New("btree: key already exists")

// Insert adds key with value. It fails with ErrKeyExists if key is present.
// Nodes on the way down that could not take what a split below them would
// push up are split first, so a split never propagates upwards (design doc
// section 2.4).
func (t *Tree) Insert(ctx context.Context, key, value []byte) error {
	if len(key) > MaxKeySize {
		return fmt.Errorf("insert: %d-byte key: %w", len(key), ErrKeyTooLarge)
	}
	if len(value) > MaxValueSize {
		return fmt.Errorf("insert: %d-byte value: %w", len(value), ErrValueTooLarge)
	}
	if err := t.insert(ctx, key, value); err != nil {
		return fmt.Errorf("insert: %w", err)
	}
	return nil
}

func (t *Tree) insert(ctx context.Context, key, value []byte) error {
	cell := leafCellSize(key, value)
	ref, n, err := t.fetch(ctx, t.root, true)
	if err != nil {
		return err
	}
	full, err := needsSplit(n, cell)
	if err == nil && full {
		err = t.splitRoot(ctx, ref, n)
	}
	if err != nil {
		release(ref, true)
		return err
	}
	for !n.isLeaf() {
		pos, c, err := n.childFor(key)
		if err != nil {
			release(ref, true)
			return err
		}
		cref, cn, err := t.childOf(ctx, n, c, true)
		if err != nil {
			release(ref, true)
			return err
		}
		full, err := needsSplit(cn, cell)
		if err == nil && full {
			var rref *storage.PageRef
			var sep []byte
			rref, sep, err = t.splitChild(ctx, ref, n, pos, cref, cn)
			if err == nil {
				// Keep the half that covers key.
				if bytes.Compare(key, sep) >= 0 {
					release(cref, true)
					cref, cn = rref, node{rref.Data()}
				} else {
					release(rref, true)
				}
			}
		}
		release(ref, true)
		if err != nil {
			release(cref, true)
			return err
		}
		ref, n = cref, cn
	}
	defer release(ref, true)
	i, found, err := n.search(key)
	if err != nil {
		return fmt.Errorf("page %d: %w", ref.ID(), err)
	}
	if found {
		return ErrKeyExists
	}
	ref.MarkDirty()
	if err := n.insertLeaf(i, key, value); err != nil {
		return fmt.Errorf("page %d: %w", ref.ID(), err)
	}
	return nil
}

// needsSplit reports whether a node must be split before the insert passes
// through it: a leaf if a cell of cell bytes does not fit, an internal node
// if the largest separator a child split can push up does not fit.
func needsSplit(n node, cell int) (bool, error) {
	need := cell + slotSize
	if !n.isLeaf() {
		need = maxInnerCell + slotSize
	}
	if n.gap() >= need {
		return false, nil
	}
	used, err := n.used()
	if err != nil {
		return false, err
	}
	return nodeSpace-used < need, nil
}

// cells copies a node's cells, checked, with each one's size including its
// slot.
func cells(n node) ([][]byte, []int, error) {
	cs := make([][]byte, n.numCells())
	sizes := make([]int, n.numCells())
	for i := range cs {
		off, size, err := n.cellBounds(i)
		if err != nil {
			return nil, nil, err
		}
		cs[i] = bytes.Clone(n.b[off : off+size])
		sizes[i] = size + slotSize
	}
	return cs, sizes, nil
}

// splitPoint returns the index of the first cell of the right half: the
// first cell at which the cells before it hold at least half of the bytes.
// Both halves of a leaf get a cell; an internal node keeps a cell for each
// half besides the one that moves up.
func splitPoint(sizes []int, leaf bool) int {
	total := 0
	for _, s := range sizes {
		total += s
	}
	s, acc := 0, 0
	for s < len(sizes) && 2*acc < total {
		acc += sizes[s]
		s++
	}
	hi := len(sizes) - 1
	if !leaf {
		hi = len(sizes) - 2
	}
	return min(s, hi) // s >= 1: the loop takes at least one cell
}

// halves is a node's content divided by a split.
type halves struct {
	left, right [][]byte // raw cells
	sep         []byte   // first key of the right half
	rightChild0 uint64   // internal nodes: the separator cell's child
}

func divide(n node) (halves, error) {
	cs, sizes, err := cells(n)
	if err != nil {
		return halves{}, err
	}
	leaf := n.isLeaf()
	if len(cs) < 2 || (!leaf && len(cs) < 3) {
		return halves{}, corrupt("splitting a node with %d cells", len(cs))
	}
	s := splitPoint(sizes, leaf)
	k, err := n.key(s)
	if err != nil {
		return halves{}, err
	}
	h := halves{left: cs[:s], sep: bytes.Clone(k)}
	if leaf {
		h.right = cs[s:]
		return h, nil
	}
	if h.rightChild0, err = n.cellChild(s); err != nil {
		return halves{}, err
	}
	h.right = cs[s+1:]
	return h, nil
}

// fill makes buf a node at level holding cells (and child0 if internal).
func fill(buf []byte, leaf bool, level int, child0 uint64, cs [][]byte) error {
	if err := initNode(buf, leaf, level); err != nil {
		return err
	}
	n := node{buf}
	n.setChild0(child0)
	for i, c := range cs {
		dst, err := n.reserve(i, len(c))
		if err != nil {
			return err
		}
		copy(dst, c)
	}
	return nil
}

// newNode allocates a page for a node of the given kind, pinned and
// exclusively latched. No one else can reach it yet.
func (t *Tree) newNode(ctx context.Context, leaf bool) (*storage.PageRef, error) {
	typ := storage.PageTypeBTreeInternal
	if leaf {
		typ = storage.PageTypeBTreeLeaf
	}
	ref, err := t.bp.NewPage(ctx, typ)
	if err != nil {
		return nil, err
	}
	ref.Lock()
	return ref, nil
}

// discard gives back pages allocated for a change that did not happen.
func (t *Tree) discard(ctx context.Context, refs ...*storage.PageRef) error {
	var errs []error
	for _, r := range refs {
		r.Unlock()
		if err := r.Unpin(false); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := t.bp.DeletePage(ctx, r.ID()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// build returns copies of a node's page refilled as each half of a split.
// Nothing is changed if it fails.
func build(page []byte, leaf bool, level int, child0 uint64, cs [][]byte) ([]byte, error) {
	buf := bytes.Clone(page)
	if err := fill(buf, leaf, level, child0, cs); err != nil {
		return nil, err
	}
	return buf, nil
}

// splitChild splits the child at position pos of the parent, both
// exclusively latched, into the child and a new right sibling, and adds the
// separator to the parent. It returns the new sibling, pinned and
// exclusively latched, and the separator. If it fails nothing has changed.
func (t *Tree) splitChild(ctx context.Context, pref *storage.PageRef, parent node, pos int, cref *storage.PageRef, child node) (*storage.PageRef, []byte, error) {
	h, err := divide(child)
	if err != nil {
		return nil, nil, fmt.Errorf("page %d: %w", cref.ID(), err)
	}
	rref, err := t.newNode(ctx, child.isLeaf())
	if err != nil {
		return nil, nil, err
	}
	leaf, level := child.isLeaf(), child.level()
	left, err := build(child.b, leaf, level, child.child0(), h.left)
	var right []byte
	if err == nil {
		right, err = build(rref.Data(), leaf, level, h.rightChild0, h.right)
	}
	if err == nil {
		pref.MarkDirty()
		err = parent.insertInner(pos, h.sep, rref.ID())
	}
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("splitting page %d: %w", cref.ID(), err), t.discard(ctx, rref))
	}
	cref.MarkDirty()
	rref.MarkDirty()
	copy(child.b, left)
	copy(rref.Data(), right)
	return rref, h.sep, nil
}

// splitRoot moves the root's content into two new nodes and makes the root
// an internal node over them, one level up. The root page stays the root.
// If it fails nothing has changed.
func (t *Tree) splitRoot(ctx context.Context, ref *storage.PageRef, root node) error {
	if root.level() >= maxLevel {
		return corrupt("tree is %d levels deep", root.level()+1)
	}
	h, err := divide(root)
	if err != nil {
		return fmt.Errorf("page %d: %w", ref.ID(), err)
	}
	leaf, level := root.isLeaf(), root.level()
	lref, err := t.newNode(ctx, leaf)
	if err != nil {
		return err
	}
	rref, err := t.newNode(ctx, leaf)
	if err != nil {
		return errors.Join(err, t.discard(ctx, lref))
	}
	left, err := build(lref.Data(), leaf, level, root.child0(), h.left)
	var right, top []byte
	if err == nil {
		right, err = build(rref.Data(), leaf, level, h.rightChild0, h.right)
	}
	if err == nil {
		top, err = build(root.b, false, level+1, lref.ID(), nil)
	}
	if err == nil {
		err = node{top}.insertInner(0, h.sep, rref.ID())
	}
	if err != nil {
		return errors.Join(fmt.Errorf("splitting root %d: %w", ref.ID(), err), t.discard(ctx, lref, rref))
	}
	ref.MarkDirty()
	lref.MarkDirty()
	rref.MarkDirty()
	copy(lref.Data(), left)
	copy(rref.Data(), right)
	copy(root.b, top)
	release(lref, true)
	release(rref, true)
	return nil
}
