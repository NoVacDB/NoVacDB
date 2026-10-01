package btree

import (
	"bytes"
	"context"
	"fmt"

	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
)

// BoundKind says how a scan bound limits keys.
type BoundKind uint8

// Bound kinds. The zero Bound is Unbounded.
const (
	Unbounded BoundKind = iota
	Inclusive
	Exclusive
)

// Bound is one end of a range scan.
type Bound struct {
	Kind BoundKind
	Key  []byte
}

// Incl returns a bound that includes key.
func Incl(key []byte) Bound { return Bound{Kind: Inclusive, Key: key} }

// Excl returns a bound that excludes key.
func Excl(key []byte) Bound { return Bound{Kind: Exclusive, Key: key} }

// Iterator returns a range of a tree's entries in increasing key order. It
// holds no latch or pin between calls to Next: each time it runs out it
// descends from the root again and copies the in-range entries of one leaf
// (design doc section 2.6). Keys are strictly increasing and never repeat.
// An entry inserted or deleted concurrently may or may not be seen; an entry
// present for the whole scan always is. An Iterator is not safe for
// concurrent use.
type Iterator struct {
	t   *Tree
	end Bound

	seek     []byte // where the next refill starts
	seekIncl bool
	more     bool // there may be entries at seek or beyond

	keys, vals [][]byte
	i          int
	err        error
	descents   int // refills so far, for tests
}

// Scan returns an iterator over the entries with keys between start and end.
func (t *Tree) Scan(start, end Bound) *Iterator {
	it := &Iterator{t: t, end: end, more: true, seekIncl: true}
	switch start.Kind {
	case Inclusive:
		it.seek = bytes.Clone(start.Key)
	case Exclusive:
		it.seek, it.seekIncl = bytes.Clone(start.Key), false
	}
	it.end.Key = bytes.Clone(end.Key)
	return it
}

// Next returns the next entry, or ok=false when the range is exhausted. The
// returned slices belong to the caller. After an error, Next keeps
// returning it.
func (it *Iterator) Next(ctx context.Context) (key, value []byte, ok bool, err error) {
	for it.i >= len(it.keys) {
		if it.err != nil {
			return nil, nil, false, it.err
		}
		if !it.more {
			return nil, nil, false, nil
		}
		if err := it.refill(ctx); err != nil {
			it.err = fmt.Errorf("scan: %w", err)
			return nil, nil, false, it.err
		}
	}
	k, v := it.keys[it.i], it.vals[it.i]
	it.i++
	return k, v, true, nil
}

// beforeEnd reports whether key is within the end bound.
func (it *Iterator) beforeEnd(key []byte) bool {
	switch it.end.Kind {
	case Inclusive:
		return bytes.Compare(key, it.end.Key) <= 0
	case Exclusive:
		return bytes.Compare(key, it.end.Key) < 0
	}
	return true
}

// refill descends to the leaf covering seek and copies its entries from
// seek on. The leaf's upper fence (the separator above it, if any) becomes
// the next seek, inclusive: every key at or past it is in a later leaf.
func (it *Iterator) refill(ctx context.Context) error {
	it.keys, it.vals, it.i = it.keys[:0], it.vals[:0], 0
	it.descents++
	ref, n, fence, err := it.t.descendFence(ctx, it.seek)
	if err != nil {
		return err
	}
	defer release(ref, false)
	i, found, err := n.search(it.seek)
	if err != nil {
		return fmt.Errorf("page %d: %w", ref.ID(), err)
	}
	if found && !it.seekIncl {
		i++
	}
	for ; i < n.numCells(); i++ {
		k, err := n.key(i)
		if err != nil {
			return fmt.Errorf("page %d: %w", ref.ID(), err)
		}
		if !it.beforeEnd(k) {
			it.more = false
			return nil
		}
		v, err := n.value(i)
		if err != nil {
			return fmt.Errorf("page %d: %w", ref.ID(), err)
		}
		it.keys = append(it.keys, bytes.Clone(k))
		it.vals = append(it.vals, bytes.Clone(v))
	}
	if fence == nil || !it.beforeEnd(fence) {
		it.more = false
		return nil
	}
	it.seek, it.seekIncl = fence, true
	return nil
}

// descendFence is descendShared that also returns the leaf's upper fence:
// the smallest separator on the path above the key's child, or nil if the
// leaf is the rightmost.
func (t *Tree) descendFence(ctx context.Context, key []byte) (*storage.PageRef, node, []byte, error) {
	ref, n, err := t.fetch(ctx, t.root, false)
	if err != nil {
		return nil, node{}, nil, err
	}
	var fence []byte
	for !n.isLeaf() {
		pos, c, err := n.childFor(key)
		if err == nil && pos < n.numCells() {
			var k []byte
			if k, err = n.key(pos); err == nil {
				fence = bytes.Clone(k)
			}
		}
		if err != nil {
			release(ref, false)
			return nil, node{}, nil, fmt.Errorf("page %d: %w", ref.ID(), err)
		}
		cref, cn, err := t.childOf(ctx, n, c, false)
		release(ref, false)
		if err != nil {
			return nil, node{}, nil, err
		}
		ref, n = cref, cn
	}
	return ref, n, fence, nil
}
