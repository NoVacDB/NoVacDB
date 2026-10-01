package btree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
)

// Errors returned by tree operations.
var (
	// ErrKeyTooLarge means a key is longer than MaxKeySize.
	ErrKeyTooLarge = errors.New("btree: key too large")
	// ErrValueTooLarge means a value is longer than MaxValueSize.
	ErrValueTooLarge = errors.New("btree: value too large")
)

// Tree is a B+Tree stored in a buffer pool. It is identified by its root
// page ID, which never changes. It is safe for concurrent use.
type Tree struct {
	bp   *storage.BufferPool
	root uint64
	ops  counters
}

// counters count structural changes, so tests can show which paths they
// exercised (and, later, for metrics). Index 0 is leaves, 1 internal nodes.
type counters struct {
	splits, rootSplits, merges [2]atomic.Uint64
	redistributions            [2][2]atomic.Uint64 // [kind][0: to the left, 1: to the right]
	collapses                  atomic.Uint64
	deletes, latchesAtLeaf     atomic.Uint64 // how much of the path deletes keep latched
}

func kindIndex(n node) int {
	if n.isLeaf() {
		return 0
	}
	return 1
}

// Create creates an empty tree: a root page that is an empty leaf.
func Create(ctx context.Context, bp *storage.BufferPool) (*Tree, error) {
	ref, err := bp.NewPage(ctx, storage.PageTypeBTreeLeaf)
	if err != nil {
		return nil, fmt.Errorf("creating tree: %w", err)
	}
	ref.Lock()
	err = initNode(ref.Data(), true, 0)
	ref.Unlock()
	if uerr := ref.Unpin(true); err == nil {
		err = uerr
	}
	if err != nil {
		return nil, fmt.Errorf("creating tree: %w", err)
	}
	return &Tree{bp: bp, root: ref.ID()}, nil
}

// Open opens the tree whose root page is root.
func Open(ctx context.Context, bp *storage.BufferPool, root uint64) (*Tree, error) {
	t := &Tree{bp: bp, root: root}
	ref, _, err := t.fetch(ctx, root, false)
	if err != nil {
		return nil, fmt.Errorf("opening tree at page %d: %w", root, err)
	}
	ref.RUnlock()
	if err := ref.Unpin(false); err != nil {
		return nil, fmt.Errorf("opening tree at page %d: %w", root, err)
	}
	return t, nil
}

// Root returns the root page ID, which identifies the tree.
func (t *Tree) Root() uint64 { return t.root }

// fetch pins and latches page id (exclusively if excl) and checks that it
// is a node. On error nothing is left pinned or latched.
func (t *Tree) fetch(ctx context.Context, id uint64, excl bool) (*storage.PageRef, node, error) {
	ref, err := t.bp.FetchPage(ctx, id)
	if err != nil {
		return nil, node{}, err
	}
	if excl {
		ref.Lock()
	} else {
		ref.RLock()
	}
	n := node{ref.Data()}
	if err := n.checkHeader(); err != nil {
		release(ref, excl)
		return nil, node{}, fmt.Errorf("page %d: %w", id, err)
	}
	return ref, n, nil
}

// release unlatches and unpins an unchanged page, ignoring the unpin error,
// which can only be a double unpin (a bug the tests catch through the pool's
// pin counts).
func release(ref *storage.PageRef, excl bool) {
	if excl {
		ref.Unlock()
	} else {
		ref.RUnlock()
	}
	_ = ref.Unpin(false)
}

// descendShared follows key down from the root with shared latches, coupling
// them, and returns the leaf, latched shared and pinned.
func (t *Tree) descendShared(ctx context.Context, key []byte) (*storage.PageRef, node, error) {
	ref, n, err := t.fetch(ctx, t.root, false)
	if err != nil {
		return nil, node{}, err
	}
	for !n.isLeaf() {
		_, c, err := n.childFor(key)
		if err == nil {
			var cref *storage.PageRef
			var cn node
			cref, cn, err = t.childOf(ctx, n, c, false)
			if err == nil {
				release(ref, false)
				ref, n = cref, cn
				continue
			}
		}
		release(ref, false)
		return nil, node{}, fmt.Errorf("page %d: %w", ref.ID(), err)
	}
	return ref, n, nil
}

// childOf fetches child c of the internal node parent and checks that it is
// one level below it (which also rules out cycles).
func (t *Tree) childOf(ctx context.Context, parent node, c uint64, excl bool) (*storage.PageRef, node, error) {
	ref, n, err := t.fetch(ctx, c, excl)
	if err != nil {
		return nil, node{}, err
	}
	if n.level() != parent.level()-1 {
		release(ref, excl)
		return nil, node{}, corrupt("child %d at level %d under a node at level %d", c, n.level(), parent.level())
	}
	return ref, n, nil
}

// Get returns a copy of the value stored under key.
func (t *Tree) Get(ctx context.Context, key []byte) ([]byte, bool, error) {
	if len(key) > MaxKeySize {
		return nil, false, fmt.Errorf("get: %d bytes: %w", len(key), ErrKeyTooLarge)
	}
	ref, n, err := t.descendShared(ctx, key)
	if err != nil {
		return nil, false, fmt.Errorf("get: %w", err)
	}
	defer release(ref, false)
	i, found, err := n.search(key)
	if err != nil || !found {
		return nil, false, err
	}
	v, err := n.value(i)
	if err != nil {
		return nil, false, fmt.Errorf("get: page %d: %w", ref.ID(), err)
	}
	return bytes.Clone(v), true, nil
}

// Stats describes a tree, as found by Check.
type Stats struct {
	Height    int // levels, 1 for a lone leaf
	Nodes     int
	Leaves    int
	Keys      int // entries in leaves
	Underfull int // non-root nodes below a quarter full (allowed, reported)
	Empty     int // non-root internal nodes with no keys (allowed, reported)
}

// bound is an optional key bound for Check.
type bound struct {
	key []byte
	set bool
}

// Check validates the whole tree (see design doc section 2.8) and returns its
// statistics. It reads one node at a time and is meant for a tree no one is
// changing.
func (t *Tree) Check(ctx context.Context) (Stats, error) {
	var st Stats
	seen := map[uint64]bool{}
	img, err := t.copyNode(ctx, t.root)
	if err != nil {
		return st, fmt.Errorf("check: %w", err)
	}
	st.Height = img.level() + 1
	if err := t.check(ctx, t.root, img, bound{}, bound{}, true, seen, &st); err != nil {
		return st, fmt.Errorf("check: %w", err)
	}
	return st, nil
}

// copyNode returns a validated copy of node id.
func (t *Tree) copyNode(ctx context.Context, id uint64) (node, error) {
	ref, n, err := t.fetch(ctx, id, false)
	if err != nil {
		return node{}, err
	}
	img := node{bytes.Clone(n.b)}
	release(ref, false)
	if err := img.validate(); err != nil {
		return node{}, fmt.Errorf("page %d: %w", id, err)
	}
	return img, nil
}

func (t *Tree) check(ctx context.Context, id uint64, n node, lo, hi bound, isRoot bool, seen map[uint64]bool, st *Stats) error {
	if seen[id] {
		return corrupt("page %d is reachable twice", id)
	}
	seen[id] = true
	st.Nodes++
	cnt := n.numCells()
	if cnt > 0 {
		first, _ := n.key(0)
		last, _ := n.key(cnt - 1)
		if lo.set && bytes.Compare(first, lo.key) < 0 {
			return corrupt("page %d: key below its lower bound", id)
		}
		if hi.set && bytes.Compare(last, hi.key) >= 0 {
			return corrupt("page %d: key not below its upper bound", id)
		}
	}
	if !isRoot {
		used, _ := n.used()
		if used < underfullSize {
			st.Underfull++
		}
		if !n.isLeaf() && cnt == 0 {
			st.Empty++
		}
	}
	if n.isLeaf() {
		st.Leaves++
		st.Keys += cnt
		return nil
	}
	for pos := 0; pos <= cnt; pos++ {
		c, _ := n.childAt(pos)
		clo, chi := lo, hi
		if pos > 0 {
			k, _ := n.key(pos - 1)
			clo = bound{k, true}
		}
		if pos < cnt {
			k, _ := n.key(pos)
			chi = bound{k, true}
		}
		child, err := t.copyNode(ctx, c)
		if err != nil {
			return err
		}
		if child.level() != n.level()-1 {
			return corrupt("page %d at level %d under page %d at level %d", c, child.level(), id, n.level())
		}
		if err := t.check(ctx, c, child, clo, chi, false, seen, st); err != nil {
			return err
		}
	}
	return nil
}
