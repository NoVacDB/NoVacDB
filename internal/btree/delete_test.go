package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
)

// mapKeys returns a model's keys in random order.
func (m model) randomKeys(rng *rand.Rand) [][]byte {
	keys := make([][]byte, 0, len(m))
	for k := range m {
		keys = append(keys, []byte(k))
	}
	// Map order is random but not seeded: sort, then shuffle with rng, so a
	// seed reproduces the run.
	sortKeys(keys)
	rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	return keys
}

func sortKeys(keys [][]byte) {
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && bytes.Compare(keys[j-1], keys[j]) > 0; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
}

// opCounts summarises a tree's structural-change counters.
type opCounts struct {
	splits, rootSplits, merges [2]uint64
	redist                     [2][2]uint64
	collapses                  uint64
}

func (t *Tree) opCounts() opCounts {
	var c opCounts
	for k := range 2 {
		c.splits[k] = t.ops.splits[k].Load()
		c.rootSplits[k] = t.ops.rootSplits[k].Load()
		c.merges[k] = t.ops.merges[k].Load()
		for d := range 2 {
			c.redist[k][d] = t.ops.redistributions[k][d].Load()
		}
	}
	c.collapses = t.ops.collapses.Load()
	return c
}

func (c *opCounts) add(o opCounts) {
	for k := range 2 {
		c.splits[k] += o.splits[k]
		c.rootSplits[k] += o.rootSplits[k]
		c.merges[k] += o.merges[k]
		for d := range 2 {
			c.redist[k][d] += o.redist[k][d]
		}
	}
	c.collapses += o.collapses
}

// requireMost fails unless every kind of structural change happened, except
// redistribution between internal nodes, which needs larger trees
// (TestShrinkTreeWithLongKeys).
func (c opCounts) requireMost(t *testing.T) {
	t.Helper()
	t.Logf("structural changes: %+v", c)
	for k, name := range []string{"leaf", "internal"} {
		if c.splits[k] == 0 || c.rootSplits[k] == 0 || c.merges[k] == 0 {
			t.Errorf("no %s split, root split or merge: %+v", name, c)
		}
	}
	if c.redist[0][0] == 0 || c.redist[0][1] == 0 || c.collapses == 0 {
		t.Errorf("no leaf redistribution in some direction, or no root collapse: %+v", c)
	}
}

func TestInsertDeleteModel(t *testing.T) {
	seed := testSeed(t)
	var total opCounts
	defer func() { total.requireMost(t) }()
	budget := opsBudget(t, 60000)
	for _, c := range []struct {
		frames, large, maxLen, insertPct int
	}{{12, 0, 24, 55}, {12, 0, 400, 60}, {12, 5, 100, 55}, {24, 30, 24, 55}, {64, 2, 200, 60}} {
		t.Run(fmt.Sprintf("frames=%d,large=%d%%,keys<=%d,insert=%d%%", c.frames, c.large, c.maxLen, c.insertPct), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, uint64(c.frames*1000+c.large*10+c.insertPct+c.maxLen*100000)))
			g := keyGen{rng: rng, large: c.large, maxLen: c.maxLen}
			bp := newPool(t, c.frames)
			tr, err := Create(bg, bp)
			if err != nil {
				t.Fatal(err)
			}
			m := model{}
			var live [][]byte // keys in m, for picking deletions
			ops := budget / 4
			if c.large >= 30 {
				ops /= 10
			}
			var ins, del, misses, maxHeight int
			for op := range ops {
				// Phases of growth and shrinkage exercise merges at every
				// height, down to the root collapsing.
				pct := c.insertPct
				if (op/3000)%2 == 1 {
					pct = 100 - pct + 5
				}
				if rng.IntN(100) < pct {
					k, v := g.key(), g.value()
					err := tr.Insert(bg, k, v)
					if _, exists := m[string(k)]; exists {
						if !errors.Is(err, ErrKeyExists) {
							t.Fatalf("op %d: %v", op, err)
						}
						continue
					}
					if err != nil {
						t.Fatalf("op %d: insert: %v", op, err)
					}
					m[string(k)] = v
					live = append(live, k)
					ins++
				} else {
					var k []byte
					if len(live) > 0 && rng.IntN(10) != 0 {
						j := rng.IntN(len(live))
						k = live[j]
						live[j] = live[len(live)-1]
						live = live[:len(live)-1]
					} else {
						k = g.key() // usually absent
					}
					_, want := m[string(k)]
					found, err := tr.Delete(bg, k)
					if err != nil || found != want {
						t.Fatalf("op %d: Delete(%x) = %v, %v; want %v", op, k, found, err, want)
					}
					if want {
						delete(m, string(k))
						del++
					} else {
						misses++
						// It may still be in live if it was generated; drop it.
						for j := range live {
							if bytes.Equal(live[j], k) {
								live[j] = live[len(live)-1]
								live = live[:len(live)-1]
								break
							}
						}
					}
				}
				if op < 300 || op%499 == 0 {
					st, err := tr.Check(bg)
					if err != nil {
						t.Fatalf("op %d: %v", op, err)
					}
					if st.Keys != len(m) {
						t.Fatalf("op %d: %d keys, model %d", op, st.Keys, len(m))
					}
					if st.Underfull > 1+st.Nodes/100 {
						t.Fatalf("op %d: %+v", op, st)
					}
					maxHeight = max(maxHeight, st.Height)
				}
			}
			st := m.verify(t, tr)
			t.Logf("%d inserts, %d deletes, %d misses, max height %d, stats %+v", ins, del, misses, maxHeight, st)
			if del == 0 || misses == 0 || maxHeight < 2 {
				t.Fatal("test did not exercise deletes")
			}
			// Then delete everything: the tree must shrink to one empty leaf.
			for _, k := range m.randomKeys(rng) {
				if found, err := tr.Delete(bg, k); err != nil || !found {
					t.Fatalf("final delete: %v %v", found, err)
				}
			}
			st, err = tr.Check(bg)
			if err != nil || st != (Stats{Height: 1, Nodes: 1, Leaves: 1}) {
				t.Fatalf("after deleting everything: %+v, %v", st, err)
			}
			if err := bp.Close(bg); err != nil {
				t.Fatalf("pages left pinned: %v", err)
			}
			total.add(tr.opCounts())
		})
	}
}

func TestDeleteAllAndReinsertReusesPages(t *testing.T) {
	bp, dm := newPoolDM(t, 16)
	tr, err := Create(bg, bp)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(testSeed(t), 11))
	for cycle := range 5 {
		m := model{}
		for i := range 5000 {
			// Padded keys keep fanout low, so the tree is several levels
			// deep and internal nodes merge too.
			k := append(AppendInt64(nil, int64(i*7919%5000)), make([]byte, 300)...)
			v := randBytes(rng, rng.IntN(200))
			if err := tr.Insert(bg, k, v); err != nil {
				t.Fatal(err)
			}
			m[string(k)] = v
		}
		st := m.verify(t, tr)
		if st.Height < 3 {
			t.Fatalf("height %d", st.Height)
		}
		order := m.randomKeys(rng)
		if cycle%2 == 1 {
			sortKeys(order) // ascending deletes empty leaves from the left
		}
		for _, k := range order {
			if found, err := tr.Delete(bg, k); err != nil || !found {
				t.Fatalf("delete: %v %v", found, err)
			}
		}
		st, err := tr.Check(bg)
		if err != nil || st.Nodes != 1 || st.Keys != 0 {
			t.Fatalf("cycle %d: after deleting everything %+v, %v", cycle, st, err)
		}
		// Every page but the root was freed.
		if inUse := dm.PageCount() - dm.FreePageCount() - storage.FirstDataPage; inUse != 1 {
			t.Fatalf("cycle %d: %d pages in use after deleting everything", cycle, inUse)
		}
	}
}

func TestDeleteSequentialKeepsNodesFull(t *testing.T) {
	for _, order := range []string{"ascending", "descending", "random"} {
		t.Run(order, func(t *testing.T) {
			bp := newPool(t, 16)
			tr, err := Create(bg, bp)
			if err != nil {
				t.Fatal(err)
			}
			const n = 20000
			m := model{}
			for i := range n {
				k := AppendInt64(nil, int64(i))
				if err := tr.Insert(bg, k, valueFor(k)); err != nil {
					t.Fatal(err)
				}
				m[string(k)] = valueFor(k)
			}
			keys := m.randomKeys(rand.New(rand.NewPCG(testSeed(t), 12)))
			switch order {
			case "ascending":
				sortKeys(keys)
			case "descending":
				sortKeys(keys)
				for i, j := 0, len(keys)-1; i < j; i, j = i+1, j-1 {
					keys[i], keys[j] = keys[j], keys[i]
				}
			}
			for i, k := range keys[:n*9/10] {
				if found, err := tr.Delete(bg, k); err != nil || !found {
					t.Fatalf("delete %d: %v %v", i, found, err)
				}
				delete(m, string(k))
			}
			st := m.verify(t, tr)
			held := float64(tr.ops.latchesAtLeaf.Load()) / float64(tr.ops.deletes.Load())
			t.Logf("stats %+v, %.2f latches held at the leaf on average", st, held)
			if st.Underfull != 0 {
				t.Fatalf("%d underfull nodes", st.Underfull)
			}
			// Most leaves are safe, so most deletes hold only the leaf.
			if held > 1.5 {
				t.Fatalf("deletes held %.2f latches at the leaf on average", held)
			}
		})
	}
}

func TestDeleteMissingAndBadKeys(t *testing.T) {
	bp := newPool(t, 8)
	tr, err := Create(bg, bp)
	if err != nil {
		t.Fatal(err)
	}
	if found, err := tr.Delete(bg, []byte("x")); found || err != nil {
		t.Fatalf("delete from empty tree: %v %v", found, err)
	}
	if _, err := tr.Delete(bg, make([]byte, MaxKeySize+1)); !errors.Is(err, ErrKeyTooLarge) {
		t.Fatalf("long key: %v", err)
	}
	if err := tr.Insert(bg, []byte("x"), nil); err != nil {
		t.Fatal(err)
	}
	if found, err := tr.Delete(bg, []byte("x")); !found || err != nil {
		t.Fatalf("delete: %v %v", found, err)
	}
	if found, err := tr.Delete(bg, []byte("x")); found || err != nil {
		t.Fatalf("second delete: %v %v", found, err)
	}
}

func TestShrinkTreeWithLongKeys(t *testing.T) {
	// Long keys give internal nodes few children, so deleting a large tree
	// down to nothing repairs internal nodes in every way.
	seed := testSeed(t)
	var total opCounts
	for round := range 3 {
		rng := rand.New(rand.NewPCG(seed, uint64(31+round)))
		bp := newPool(t, 32)
		tr, err := Create(bg, bp)
		if err != nil {
			t.Fatal(err)
		}
		m := model{}
		for len(m) < 6000 {
			k := make([]byte, 100+rng.IntN(500))
			for i := range k {
				k[i] = byte('a' + rng.IntN(26))
			}
			v := randBytes(rng, rng.IntN(100))
			if err := tr.Insert(bg, k, v); err != nil {
				t.Fatal(err)
			}
			m[string(k)] = v
		}
		st := m.verify(t, tr)
		keys := m.randomKeys(rng)
		if round == 1 {
			sortKeys(keys)
		}
		for i, k := range keys {
			if found, err := tr.Delete(bg, k); err != nil || !found {
				t.Fatalf("delete %d: %v %v", i, found, err)
			}
			delete(m, string(k))
			if i%500 == 0 {
				// Repairs keep nodes full: only a skipped redistribution
				// (the new separator did not fit) leaves one underfull.
				if st := m.verify(t, tr); st.Underfull > 1+st.Nodes/100 {
					t.Fatalf("round %d after %d deletes: %+v", round, i, st)
				}
			}
		}
		end, err := tr.Check(bg)
		if err != nil || end.Nodes != 1 {
			t.Fatalf("after deleting everything: %+v, %v", end, err)
		}
		t.Logf("round %d: started with %+v", round, st)
		total.add(tr.opCounts())
	}
	t.Logf("%+v", total)
	for k := range 2 {
		if total.merges[k] == 0 || total.redist[k][0] == 0 || total.redist[k][1] == 0 {
			t.Fatalf("missing merges or redistributions: %+v", total)
		}
	}
	if total.collapses == 0 {
		t.Fatal("no root collapse")
	}
}

func TestDeleteRepairsLeftoverShapes(t *testing.T) {
	// Shapes a crash can leave between a change and its repair: internal
	// nodes with one child, underfull leaves, a root with one child.
	for _, c := range []struct{ n, perLeaf, fanout int }{{27, 3, 2}, {64, 1, 2}, {40, 2, 3}} {
		t.Run(fmt.Sprintf("n=%d,leaf=%d,fanout=%d", c.n, c.perLeaf, c.fanout), func(t *testing.T) {
			bp := newPool(t, 16)
			keys := intKeys(c.n, 1)
			tr := buildTree(t, bp, keys, c.perLeaf, c.fanout)
			m := model{}
			for _, k := range keys {
				m[string(k)] = valueFor(k)
			}
			st := m.verify(t, tr)
			if st.Underfull == 0 {
				t.Fatalf("hand-built tree is not underfull: %+v", st)
			}
			rng := rand.New(rand.NewPCG(testSeed(t), uint64(c.n)))
			for _, k := range m.randomKeys(rng) {
				if found, err := tr.Delete(bg, k); err != nil || !found {
					t.Fatalf("delete: %v %v", found, err)
				}
				delete(m, string(k))
				m.verify(t, tr)
			}
			if st, _ := tr.Check(bg); st.Nodes != 1 {
				t.Fatalf("after deleting everything: %+v", st)
			}
		})
	}
	t.Run("root with one child", func(t *testing.T) {
		bp := newPool(t, 8)
		leafID, leaf, lref := newNodePage(t, bp, true, 0)
		for i, k := range intKeys(3, 1) {
			if err := leaf.insertLeaf(i, k, valueFor(k)); err != nil {
				t.Fatal(err)
			}
		}
		_ = lref.Unpin(true)
		rootID, root, rref := newNodePage(t, bp, false, 1)
		root.setChild0(leafID)
		_ = rref.Unpin(true)
		tr, err := Open(bg, bp, rootID)
		if err != nil {
			t.Fatal(err)
		}
		if found, err := tr.Delete(bg, intKeys(3, 1)[1]); !found || err != nil {
			t.Fatalf("delete: %v %v", found, err)
		}
		st := model{string(intKeys(3, 1)[0]): valueFor(intKeys(3, 1)[0]), string(intKeys(3, 1)[2]): valueFor(intKeys(3, 1)[2])}.verify(t, tr)
		if st.Height != 1 || tr.Root() != rootID {
			t.Fatalf("root was not collapsed in place: %+v", st)
		}
	})
}

// leafWith returns a leaf page holding keys with values of vlen bytes.
func leafWith(t *testing.T, bp *storage.BufferPool, keys [][]byte, vlen int) uint64 {
	t.Helper()
	id, n, ref := newNodePage(t, bp, true, 0)
	for i, k := range keys {
		if err := n.insertLeaf(i, k, make([]byte, vlen)); err != nil {
			t.Fatal(err)
		}
	}
	if err := ref.Unpin(true); err != nil {
		t.Fatal(err)
	}
	return id
}

// rootOver makes an internal root over children with separators seps.
func rootOver(t *testing.T, bp *storage.BufferPool, level int, children []uint64, seps [][]byte) *Tree {
	t.Helper()
	id, n, ref := newNodePage(t, bp, false, level)
	n.setChild0(children[0])
	for i, s := range seps {
		if err := n.insertInner(i, s, children[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := ref.Unpin(true); err != nil {
		t.Fatal(err)
	}
	tr, err := Open(bg, bp, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Check(bg); err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestMergeWhenExactlyFull(t *testing.T) {
	// Two leaves whose cells fill one node to the byte after a delete must
	// merge (and the root then collapses).
	bp := newPool(t, 8)
	// Left: "a" and "b" (small); right: cells that fill the rest exactly.
	left := [][]byte{[]byte("a"), []byte("b")}
	leftUsed := 2 * (leafCellHdr + 1 + 10 + slotSize) // after deleting nothing
	afterDelete := leftUsed - (leafCellHdr + 1 + 10 + slotSize)
	// Right: 200-byte values, the last one sized to fill the node exactly.
	var right [][]byte
	var vals []int
	rest := nodeSpace - afterDelete
	cell := leafCellHdr + 2 + 200 + slotSize // key "cN", 200-byte value
	for i := 0; rest >= 2*cell; i++ {
		right, vals = append(right, []byte{'c', byte('a' + i)}), append(vals, 200)
		rest -= cell
	}
	right, vals = append(right, []byte{'c', byte('a' + len(right))}), append(vals, rest-leafCellHdr-2-slotSize)
	l := leafWith(t, bp, left, 10)
	rid, rn, rref := newNodePage(t, bp, true, 0)
	for i, k := range right {
		if err := rn.insertLeaf(i, k, make([]byte, vals[i])); err != nil {
			t.Fatal(err)
		}
	}
	if used, _ := rn.used(); used+afterDelete != nodeSpace {
		t.Fatalf("setup: %d + %d bytes", used, afterDelete)
	}
	_ = rref.Unpin(true)
	tr := rootOver(t, bp, 1, []uint64{l, rid}, [][]byte{[]byte("c")})
	if found, err := tr.Delete(bg, []byte("a")); !found || err != nil {
		t.Fatal(found, err)
	}
	st, err := tr.Check(bg)
	if err != nil || st.Height != 1 || st.Keys != 1+len(right) {
		t.Fatalf("an exactly fitting merge did not happen: %+v, %v", st, err)
	}
}

func TestSkippedRedistributionWhenSeparatorDoesNotFit(t *testing.T) {
	// The parent has no room for a maximum-size separator. Refilling the
	// underfull left leaf would move one up, so the repair is skipped: the
	// leaf stays underfull and the tree stays valid.
	bp := newPool(t, 16)
	pad := func(c byte) []byte {
		k := bytes.Repeat([]byte{'x'}, MaxKeySize)
		k[0] = c
		return k
	}
	l := leafWith(t, bp, [][]byte{[]byte("a"), []byte("b")}, MaxValueSize) // too big to merge with r
	r := leafWith(t, bp, [][]byte{pad('m'), pad('n'), pad('o'), pad('p'), pad('q')}, MaxValueSize)
	children := []uint64{l, r}
	seps := [][]byte{[]byte("m")}
	for c := byte('r'); c < 'r'+7; c++ { // seven maximum-size separators over empty leaves
		children = append(children, leafWith(t, bp, nil, 0))
		seps = append(seps, pad(c))
	}
	tr := rootOver(t, bp, 1, children, seps)
	if found, err := tr.Delete(bg, []byte("a")); !found || err != nil {
		t.Fatal(found, err)
	}
	st, err := tr.Check(bg)
	if err != nil {
		t.Fatal(err)
	}
	if st.Underfull == 0 || tr.opCounts().redist[0] != [2]uint64{} || tr.opCounts().merges[0] != 0 {
		t.Fatalf("expected the repair to be skipped: %+v %+v", st, tr.opCounts())
	}
	for _, k := range [][]byte{[]byte("b"), pad('m'), pad('q')} {
		if _, ok, err := tr.Get(bg, k); !ok || err != nil {
			t.Fatalf("Get(%.1s): %v %v", k, ok, err)
		}
	}
}

func TestRepairContinuesAboveAHealthyNode(t *testing.T) {
	// A four-level tree whose level-2 node A is underfull (as a crash can
	// leave it). Deleting b1 merges leaf C into D under B; B loses a small
	// separator but stays healthy; the repair must still go on to A, merging
	// it with its sibling Z, and the root then collapses.
	//
	//	R(3):  A | "h" Z
	//	A(2):  B | "g" B2        Z(2): Z1
	//	B(1):  C | "c" D | e… E | f… F     B2(1): G     Z1(1): H
	bp := newPool(t, 32)
	pad := func(c byte) []byte {
		k := bytes.Repeat([]byte{'x'}, MaxKeySize)
		k[0] = c
		return k
	}
	inner := func(level int, children []uint64, seps [][]byte) uint64 {
		id, n, ref := newNodePage(t, bp, false, level)
		n.setChild0(children[0])
		for i, sp := range seps {
			if err := n.insertInner(i, sp, children[i+1]); err != nil {
				t.Fatal(err)
			}
		}
		_ = ref.Unpin(true)
		return id
	}
	c := leafWith(t, bp, [][]byte{[]byte("b1"), []byte("b2")}, 10)
	d := leafWith(t, bp, [][]byte{[]byte("c1"), []byte("c2")}, 10)
	b := inner(1, []uint64{c, d, leafWith(t, bp, nil, 0), leafWith(t, bp, nil, 0)}, [][]byte{[]byte("c"), pad('e'), pad('f')})
	b2 := inner(1, []uint64{leafWith(t, bp, nil, 0)}, nil)
	a := inner(2, []uint64{b, b2}, [][]byte{[]byte("g")})
	z := inner(2, []uint64{inner(1, []uint64{leafWith(t, bp, nil, 0)}, nil)}, nil)
	tr := rootOver(t, bp, 3, []uint64{a, z}, [][]byte{[]byte("h")})
	before, err := tr.Check(bg)
	if err != nil || before.Height != 4 {
		t.Fatalf("setup: %+v, %v", before, err)
	}
	if found, err := tr.Delete(bg, []byte("b1")); !found || err != nil {
		t.Fatal(found, err)
	}
	after, err := tr.Check(bg)
	if err != nil {
		t.Fatal(err)
	}
	if after.Height != 3 || tr.opCounts().merges != [2]uint64{1, 1} || tr.opCounts().collapses != 1 {
		t.Fatalf("repair stopped at the healthy node: before %+v, after %+v, %+v", before, after, tr.opCounts())
	}
}

func TestOperationsOnCorruptTrees(t *testing.T) {
	// Every operation that meets a damaged node fails with ErrCorruptNode,
	// never panics, and leaves nothing pinned.
	damage := map[string]func(t *testing.T, bp *storage.BufferPool, tr *Tree){
		"leaf slot": func(t *testing.T, bp *storage.BufferPool, tr *Tree) {
			mutatePage(t, bp, leftmostLeaf(t, bp, tr.Root()), func(n node) { n.put16(slotsStart, 3) })
		},
		"leaf key length": func(t *testing.T, bp *storage.BufferPool, tr *Tree) {
			mutatePage(t, bp, leftmostLeaf(t, bp, tr.Root()), func(n node) { n.put16(n.slot(0), MaxKeySize+1) })
		},
		"internal slot": func(t *testing.T, bp *storage.BufferPool, tr *Tree) {
			mutatePage(t, bp, tr.Root(), func(n node) { n.put16(slotsStart, 1) })
		},
		"child level": func(t *testing.T, bp *storage.BufferPool, tr *Tree) {
			mutatePage(t, bp, leftmostLeaf(t, bp, tr.Root()), func(n node) {
				binary.LittleEndian.PutUint16(n.b[20:], uint16(storage.PageTypeBTreeInternal))
				n.put16(offLevel, 5)
				n.setChild0(storage.FirstDataPage)
			})
		},
		"child type": func(t *testing.T, bp *storage.BufferPool, tr *Tree) {
			mutatePage(t, bp, leftmostLeaf(t, bp, tr.Root()), func(n node) {
				binary.LittleEndian.PutUint16(n.b[20:], uint16(storage.PageTypeHeap))
			})
		},
	}
	ops := map[string]func(tr *Tree, k []byte) error{
		"get":    func(tr *Tree, k []byte) error { _, _, err := tr.Get(bg, k); return err },
		"insert": func(tr *Tree, k []byte) error { return tr.Insert(bg, append(bytes.Clone(k), 1), []byte("v")) },
		"delete": func(tr *Tree, k []byte) error { _, err := tr.Delete(bg, k); return err },
	}
	for dname, d := range damage {
		for oname, op := range ops {
			t.Run(dname+"/"+oname, func(t *testing.T) {
				bp := newPool(t, 16)
				keys := intKeys(60, 1)
				tr := buildTree(t, bp, keys, 4, 3)
				d(t, bp, tr)
				if err := op(tr, keys[0]); !errors.Is(err, ErrCorruptNode) {
					t.Fatalf("got %v", err)
				}
				if err := bp.Close(bg); err != nil {
					t.Fatalf("pages left pinned: %v", err)
				}
			})
		}
	}
}
