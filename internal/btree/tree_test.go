package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
)

func valueFor(k []byte) []byte { return append([]byte("v:"), k...) }

// newNodePage allocates a page in bp holding an empty node.
func newNodePage(t testing.TB, bp *storage.BufferPool, leaf bool, level int) (uint64, node, *storage.PageRef) {
	t.Helper()
	typ := storage.PageTypeBTreeInternal
	if leaf {
		typ = storage.PageTypeBTreeLeaf
	}
	ref, err := bp.NewPage(bg, typ)
	if err != nil {
		t.Fatal(err)
	}
	if err := initNode(ref.Data(), leaf, level); err != nil {
		t.Fatal(err)
	}
	return ref.ID(), node{ref.Data()}, ref
}

// buildTree bulk-loads sorted keys (with valueFor values) into a tree whose
// leaves hold perLeaf keys and internal nodes fanout children, by hand, so
// lookups can be tested independently of insertion.
func buildTree(t testing.TB, bp *storage.BufferPool, keys [][]byte, perLeaf, fanout int) *Tree {
	t.Helper()
	type entry struct {
		low []byte // smallest key under the page
		id  uint64
	}
	var level []entry
	for i := 0; i < len(keys) || i == 0; i += perLeaf {
		id, n, ref := newNodePage(t, bp, true, 0)
		end := min(i+perLeaf, len(keys))
		for j := i; j < end; j++ {
			if err := n.insertLeaf(j-i, keys[j], valueFor(keys[j])); err != nil {
				t.Fatal(err)
			}
		}
		var low []byte
		if i < len(keys) {
			low = keys[i]
		}
		level = append(level, entry{low, id})
		if err := ref.Unpin(true); err != nil {
			t.Fatal(err)
		}
	}
	for lvl := 1; len(level) > 1; lvl++ {
		var next []entry
		for i := 0; i < len(level); i += fanout {
			id, n, ref := newNodePage(t, bp, false, lvl)
			n.setChild0(level[i].id)
			for j := i + 1; j < min(i+fanout, len(level)); j++ {
				if err := n.insertInner(j-i-1, level[j].low, level[j].id); err != nil {
					t.Fatal(err)
				}
			}
			next = append(next, entry{level[i].low, id})
			if err := ref.Unpin(true); err != nil {
				t.Fatal(err)
			}
		}
		level = next
	}
	tr, err := Open(bg, bp, level[0].id)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func intKeys(to, step int) [][]byte {
	var keys [][]byte
	for i := 0; i < to; i += step {
		keys = append(keys, AppendInt64(nil, int64(i)))
	}
	return keys
}

func TestGetOnHandBuiltTrees(t *testing.T) {
	for _, c := range []struct{ n, perLeaf, fanout, height int }{
		{0, 4, 3, 1}, {1, 4, 3, 1}, {4, 4, 3, 1}, {5, 4, 3, 2}, {12, 4, 3, 2}, {13, 4, 3, 3},
		{200, 3, 2, 8}, {1000, 50, 5, 3},
	} {
		t.Run(fmt.Sprintf("n=%d", c.n), func(t *testing.T) {
			bp := newPool(t, 8)
			keys := intKeys(2*c.n, 2) // even keys; odd ones are absent
			tr := buildTree(t, bp, keys, c.perLeaf, c.fanout)
			st, err := tr.Check(bg)
			if err != nil {
				t.Fatal(err)
			}
			if st.Height != c.height || st.Keys != c.n {
				t.Fatalf("stats %+v, want height %d and %d keys", st, c.height, c.n)
			}
			for i := -1; i <= 2*c.n; i++ {
				k := AppendInt64(nil, int64(i))
				v, ok, err := tr.Get(bg, k)
				if err != nil {
					t.Fatal(err)
				}
				want := i >= 0 && i%2 == 0 && i < 2*c.n
				if ok != want || (ok && !bytes.Equal(v, valueFor(k))) {
					t.Fatalf("Get(%d) = %q, %v; want present=%v", i, v, ok, want)
				}
			}
		})
	}
}

func TestGetReturnsACopy(t *testing.T) {
	bp := newPool(t, 4)
	keys := intKeys(3, 1)
	tr := buildTree(t, bp, keys, 10, 10)
	v, _, err := tr.Get(bg, keys[1])
	if err != nil {
		t.Fatal(err)
	}
	v[0] = 'X'
	again, _, _ := tr.Get(bg, keys[1])
	if !bytes.Equal(again, valueFor(keys[1])) {
		t.Fatal("Get returned bytes aliasing the page")
	}
}

func TestGetRejectsLongKeys(t *testing.T) {
	bp := newPool(t, 4)
	tr, err := Create(bg, bp)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr.Get(bg, make([]byte, MaxKeySize+1)); !errors.Is(err, ErrKeyTooLarge) {
		t.Fatalf("Get with long key: %v", err)
	}
	if _, ok, err := tr.Get(bg, make([]byte, MaxKeySize)); ok || err != nil {
		t.Fatalf("Get with max key: %v %v", ok, err)
	}
}

func TestCreateAndReopenEmptyTree(t *testing.T) {
	bp := newPool(t, 4)
	tr, err := Create(bg, bp)
	if err != nil {
		t.Fatal(err)
	}
	st, err := tr.Check(bg)
	if err != nil || st != (Stats{Height: 1, Nodes: 1, Leaves: 1}) {
		t.Fatalf("Check = %+v, %v", st, err)
	}
	tr2, err := Open(bg, bp, tr.Root())
	if err != nil || tr2.Root() != tr.Root() {
		t.Fatalf("Open: %v", err)
	}
}

func TestOpenRejectsNonNodes(t *testing.T) {
	bp := newPool(t, 4)
	ref, err := bp.NewPage(bg, storage.PageTypeHeap)
	if err != nil {
		t.Fatal(err)
	}
	id := ref.ID()
	if err := ref.Unpin(true); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(bg, bp, id); !errors.Is(err, ErrCorruptNode) {
		t.Fatalf("Open on a heap page: %v", err)
	}
	if _, err := Open(bg, bp, 999); err == nil {
		t.Fatal("Open on a page past the end of the file succeeded")
	}
}

// mutatePage applies f to page id under its exclusive latch.
func mutatePage(t *testing.T, bp *storage.BufferPool, id uint64, f func(n node)) {
	t.Helper()
	ref, err := bp.FetchPage(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	ref.Lock()
	f(node{ref.Data()})
	ref.Unlock()
	if err := ref.Unpin(true); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptTreesAreDetected(t *testing.T) {
	setup := func(t *testing.T) (*storage.BufferPool, *Tree, node) {
		bp := newPool(t, 8)
		tr := buildTree(t, bp, intKeys(40, 1), 4, 3)
		ref, err := bp.FetchPage(bg, tr.Root())
		if err != nil {
			t.Fatal(err)
		}
		root := node{bytes.Clone(ref.Data())}
		if err := ref.Unpin(false); err != nil {
			t.Fatal(err)
		}
		return bp, tr, root
	}
	t.Run("child at wrong level", func(t *testing.T) {
		bp, tr, root := setup(t)
		mutatePage(t, bp, root.child0(), func(n node) { n.put16(offLevel, n.level()+1) })
		if _, err := tr.Check(bg); !errors.Is(err, ErrCorruptNode) {
			t.Fatalf("Check: %v", err)
		}
		if _, _, err := tr.Get(bg, AppendInt64(nil, 0)); !errors.Is(err, ErrCorruptNode) {
			t.Fatalf("Get: %v", err)
		}
	})
	t.Run("child points at itself", func(t *testing.T) {
		bp, tr, _ := setup(t)
		mutatePage(t, bp, tr.Root(), func(n node) { n.setChild0(tr.Root()) })
		if _, _, err := tr.Get(bg, AppendInt64(nil, 0)); !errors.Is(err, ErrCorruptNode) {
			t.Fatalf("Get: %v", err)
		}
	})
	t.Run("key outside its parent's range", func(t *testing.T) {
		bp, tr, _ := setup(t)
		// Raise the first separator above the keys of the second child.
		mutatePage(t, bp, tr.Root(), func(n node) {
			k, _ := n.key(0)
			binary.BigEndian.PutUint64(k[1:], binary.BigEndian.Uint64(k[1:])+1)
		})
		if _, err := tr.Check(bg); !errors.Is(err, ErrCorruptNode) {
			t.Fatalf("Check: %v", err)
		}
	})
	t.Run("key below its lower bound", func(t *testing.T) {
		bp, tr, _ := setup(t)
		mutatePage(t, bp, tr.Root(), func(n node) {
			k, _ := n.key(0)
			binary.BigEndian.PutUint64(k[1:], binary.BigEndian.Uint64(k[1:])-1)
		})
		if _, err := tr.Check(bg); !errors.Is(err, ErrCorruptNode) {
			t.Fatalf("Check: %v", err)
		}
	})
	t.Run("child skips a level", func(t *testing.T) {
		bp, tr, _ := setup(t)
		mutatePage(t, bp, tr.Root(), func(n node) { n.put16(offLevel, n.level()+1) })
		if _, _, err := tr.Get(bg, AppendInt64(nil, 0)); !errors.Is(err, ErrCorruptNode) {
			t.Fatalf("Get: %v", err)
		}
		if _, err := tr.Check(bg); !errors.Is(err, ErrCorruptNode) {
			t.Fatalf("Check: %v", err)
		}
	})
	t.Run("page reachable twice", func(t *testing.T) {
		bp, tr, root := setup(t)
		c1, _ := root.childAt(1)
		mutatePage(t, bp, tr.Root(), func(n node) { n.setChild0(c1) })
		// Separators no longer bound the duplicated child's keys either,
		// but sharing is what this case is about: make the bounds fit.
		mutatePage(t, bp, tr.Root(), func(n node) {
			for n.numCells() > 1 {
				_ = n.remove(1)
			}
			n.setCellChild(0, c1)
			k, _ := n.key(0)
			copy(k, AppendInt64(nil, -100))
		})
		if _, err := tr.Check(bg); !errors.Is(err, ErrCorruptNode) {
			t.Fatalf("Check: %v", err)
		}
	})
	t.Run("leaf damaged", func(t *testing.T) {
		bp, tr, _ := setup(t)
		leafID := leftmostLeaf(t, bp, tr.Root())
		mutatePage(t, bp, leafID, func(n node) { n.put16(slotsStart, 3) })
		if _, err := tr.Check(bg); !errors.Is(err, ErrCorruptNode) {
			t.Fatalf("Check: %v", err)
		}
		if _, _, err := tr.Get(bg, AppendInt64(nil, 0)); !errors.Is(err, ErrCorruptNode) {
			t.Fatalf("Get: %v", err)
		}
	})
}

// leftmostLeaf returns the ID of the leftmost leaf under page id.
func leftmostLeaf(t *testing.T, bp *storage.BufferPool, id uint64) uint64 {
	t.Helper()
	for {
		ref, err := bp.FetchPage(bg, id)
		if err != nil {
			t.Fatal(err)
		}
		n := node{ref.Data()}
		leaf, next := n.isLeaf(), n.child0()
		if err := ref.Unpin(false); err != nil {
			t.Fatal(err)
		}
		if leaf {
			return id
		}
		id = next
	}
}

func TestCheckRejectsSharedPages(t *testing.T) {
	// An empty leaf under two parents' slots keeps every bound satisfied,
	// so only the reachability check can catch it.
	bp := newPool(t, 8)
	leafID, _, lref := newNodePage(t, bp, true, 0)
	if err := lref.Unpin(true); err != nil {
		t.Fatal(err)
	}
	rootID, root, rref := newNodePage(t, bp, false, 1)
	root.setChild0(leafID)
	if err := root.insertInner(0, []byte("k"), leafID); err != nil {
		t.Fatal(err)
	}
	if err := rref.Unpin(true); err != nil {
		t.Fatal(err)
	}
	tr, err := Open(bg, bp, rootID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Check(bg); !errors.Is(err, ErrCorruptNode) {
		t.Fatalf("Check: %v", err)
	}
}

func TestCheckReportsUnderfullAndEmptyNodes(t *testing.T) {
	bp := newPool(t, 8)
	tr := buildTree(t, bp, intKeys(9, 1), 3, 3) // root over three leaves
	st, err := tr.Check(bg)
	if err != nil {
		t.Fatal(err)
	}
	if st.Underfull != 3 || st.Empty != 0 || st.Height != 2 {
		t.Fatalf("stats %+v", st)
	}
	// An internal non-root node with no keys is valid.
	bp = newPool(t, 8)
	// 9 leaves under fanout 2: each internal level ends in a lone child.
	tr = buildTree(t, bp, intKeys(27, 1), 3, 2)
	st, err = tr.Check(bg)
	if err != nil {
		t.Fatal(err)
	}
	if st.Empty != 3 || st.Height != 5 {
		t.Fatalf("stats %+v, want three empty internal nodes", st)
	}
	for i := range 27 {
		if _, ok, err := tr.Get(bg, AppendInt64(nil, int64(i))); !ok || err != nil {
			t.Fatalf("Get(%d) through an empty internal node: %v %v", i, ok, err)
		}
	}
}

func TestPagesListsEveryNodeOnce(t *testing.T) {
	for _, c := range []struct{ n, perLeaf, fanout int }{{0, 4, 3}, {4, 4, 3}, {13, 4, 3}, {200, 3, 2}} {
		bp := newPool(t, 8)
		tr := buildTree(t, bp, intKeys(c.n, 1), c.perLeaf, c.fanout)
		st, err := tr.Check(bg)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := tr.Pages(bg)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[uint64]bool{}
		for _, id := range ids {
			if seen[id] {
				t.Fatalf("n=%d: page %d listed twice", c.n, id)
			}
			seen[id] = true
		}
		// buildTree allocates exactly the tree's pages, from the first data page.
		if len(ids) != st.Nodes || !seen[tr.Root()] {
			t.Fatalf("n=%d: %d pages %v, want %d including root %d", c.n, len(ids), ids, st.Nodes, tr.Root())
		}
		for id := uint64(storage.FirstDataPage); id < storage.FirstDataPage+uint64(st.Nodes); id++ {
			if !seen[id] {
				t.Fatalf("n=%d: page %d missing from %v", c.n, id, ids)
			}
		}
		if err := bp.Close(bg); err != nil {
			t.Fatalf("pages left pinned: %v", err)
		}
	}
}

func TestPagesRejectsDamagedTrees(t *testing.T) {
	damage := map[string]func(bp *storage.BufferPool) *Tree{
		"child reachable twice": func(bp *storage.BufferPool) *Tree {
			leaf, _, ref := newNodePage(t, bp, true, 0)
			_ = ref.Unpin(true)
			root, n, ref := newNodePage(t, bp, false, 1)
			n.setChild0(leaf)
			if err := n.insertInner(0, []byte("m"), leaf); err != nil {
				t.Fatal(err)
			}
			_ = ref.Unpin(true)
			return &Tree{bp: bp, root: root}
		},
		"child points at root": func(bp *storage.BufferPool) *Tree {
			root, n, ref := newNodePage(t, bp, false, 1)
			n.setChild0(root)
			_ = ref.Unpin(true)
			return &Tree{bp: bp, root: root}
		},
		"child skips a level": func(bp *storage.BufferPool) *Tree {
			leaf, _, ref := newNodePage(t, bp, true, 0)
			_ = ref.Unpin(true)
			root, n, ref := newNodePage(t, bp, false, 2)
			n.setChild0(leaf)
			_ = ref.Unpin(true)
			return &Tree{bp: bp, root: root}
		},
		"child level": func(bp *storage.BufferPool) *Tree {
			tr := buildTree(t, bp, intKeys(60, 1), 4, 3)
			mutatePage(t, bp, leftmostLeaf(t, bp, tr.Root()), func(n node) {
				binary.LittleEndian.PutUint16(n.b[20:], uint16(storage.PageTypeBTreeInternal))
				n.put16(offLevel, 5)
				n.setChild0(storage.FirstDataPage)
			})
			return tr
		},
		"child type": func(bp *storage.BufferPool) *Tree {
			tr := buildTree(t, bp, intKeys(60, 1), 4, 3)
			mutatePage(t, bp, leftmostLeaf(t, bp, tr.Root()), func(n node) {
				binary.LittleEndian.PutUint16(n.b[20:], uint16(storage.PageTypeHeap))
			})
			return tr
		},
		"internal slot": func(bp *storage.BufferPool) *Tree {
			tr := buildTree(t, bp, intKeys(60, 1), 4, 3)
			mutatePage(t, bp, tr.Root(), func(n node) { n.put16(slotsStart, 1) })
			return tr
		},
	}
	for name, d := range damage {
		t.Run(name, func(t *testing.T) {
			bp := newPool(t, 16)
			tr := d(bp)
			if ids, err := tr.Pages(bg); !errors.Is(err, ErrCorruptNode) {
				t.Fatalf("got %v, %v", ids, err)
			}
			if err := bp.Close(bg); err != nil {
				t.Fatalf("pages left pinned: %v", err)
			}
		})
	}
}
