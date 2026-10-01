package btree

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
)

// opsBudget scales the random model tests: NOVACDB_BTREE_OPS overrides the
// default number of operations per test.
func opsBudget(t testing.TB, def int) int {
	t.Helper()
	if s := os.Getenv("NOVACDB_BTREE_OPS"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 1 {
			t.Fatalf("bad NOVACDB_BTREE_OPS %q", s)
		}
		return v
	}
	return def
}

// model is the reference for a tree: a plain map.
type model map[string][]byte

// verify checks the tree's structure and that it holds exactly the model.
func (m model) verify(t *testing.T, tr *Tree) Stats {
	t.Helper()
	st, err := tr.Check(bg)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if st.Keys != len(m) {
		t.Fatalf("tree has %d keys, model %d", st.Keys, len(m))
	}
	for k, v := range m {
		got, ok, err := tr.Get(bg, []byte(k))
		if err != nil || !ok || !bytes.Equal(got, v) {
			t.Fatalf("Get(%x) = %x, %v, %v; want %x", k, got, ok, err, v)
		}
	}
	return st
}

// keyGen produces random keys and values with a mix of sizes, including
// the maximum ones, from a small alphabet so prefixes and near-collisions are
// common.
type keyGen struct {
	rng    *rand.Rand
	large  int // percent of keys and values that are maximum size
	maxLen int // longest ordinary key; 0 means 24
}

func (g keyGen) key() []byte {
	if g.rng.IntN(100) < g.large {
		k := make([]byte, MaxKeySize)
		for i := range k {
			k[i] = byte('a' + g.rng.IntN(3))
		}
		return k
	}
	n := g.maxLen
	if n == 0 {
		n = 24
	}
	k := make([]byte, 1+g.rng.IntN(n))
	for i := range k {
		k[i] = byte('a' + g.rng.IntN(4))
	}
	return k
}

func (g keyGen) value() []byte {
	if g.rng.IntN(100) < g.large {
		return randBytes(g.rng, MaxValueSize)
	}
	return randBytes(g.rng, g.rng.IntN(30))
}

func TestInsertModel(t *testing.T) {
	seed := testSeed(t)
	budget := opsBudget(t, 60000)
	for _, c := range []struct {
		frames, large int
	}{{8, 0}, {8, 5}, {16, 30}, {64, 2}} {
		t.Run(fmt.Sprintf("frames=%d,large=%d%%", c.frames, c.large), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, uint64(c.frames*100+c.large)))
			g := keyGen{rng: rng, large: c.large}
			bp := newPool(t, c.frames)
			tr, err := Create(bg, bp)
			if err != nil {
				t.Fatal(err)
			}
			m := model{}
			ops := budget / 4
			if c.large >= 30 {
				ops /= 10 // maximum-size entries: fewer, bigger operations
			}
			var dups, checks int
			for op := range ops {
				k, v := g.key(), g.value()
				err := tr.Insert(bg, k, v)
				if _, exists := m[string(k)]; exists {
					if !errors.Is(err, ErrKeyExists) {
						t.Fatalf("op %d: insert of existing key: %v", op, err)
					}
					dups++
				} else if err != nil {
					t.Fatalf("op %d: insert: %v", op, err)
				} else {
					m[string(k)] = v
				}
				if rng.IntN(4) == 0 {
					probe := g.key()
					got, ok, err := tr.Get(bg, probe)
					want, wok := m[string(probe)]
					if err != nil || ok != wok || !bytes.Equal(got, want) {
						t.Fatalf("op %d: Get(%x) = %x, %v, %v; model %x, %v", op, probe, got, ok, err, want, wok)
					}
				}
				if op < 300 || op%997 == 0 {
					if _, err := tr.Check(bg); err != nil {
						t.Fatalf("op %d: %v", op, err)
					}
					checks++
				}
			}
			st := m.verify(t, tr)
			t.Logf("%d inserts (%d duplicates), %d checks, stats %+v", ops, dups, checks, st)
			if st.Height < 2 || dups == 0 {
				t.Fatal("test did not split the root or try duplicates")
			}
			if err := bp.Close(bg); err != nil {
				t.Fatalf("pages left pinned: %v", err)
			}
		})
	}
}

func TestInsertSequentialOrders(t *testing.T) {
	const n = 20000
	for _, order := range []string{"ascending", "descending", "interleaved"} {
		t.Run(order, func(t *testing.T) {
			bp := newPool(t, 16)
			tr, err := Create(bg, bp)
			if err != nil {
				t.Fatal(err)
			}
			m := model{}
			for i := range n {
				v := i
				switch order {
				case "descending":
					v = n - 1 - i
				case "interleaved":
					if i%2 == 1 {
						v = n - i
					}
				}
				k := AppendInt64(nil, int64(v))
				if err := tr.Insert(bg, k, valueFor(k)); err != nil {
					t.Fatalf("insert %d: %v", v, err)
				}
				m[string(k)] = valueFor(k)
			}
			st := m.verify(t, tr)
			t.Logf("stats %+v", st)
			if st.Underfull != 0 {
				t.Fatalf("splits left %d underfull nodes", st.Underfull)
			}
		})
	}
}

func TestRootSplitKeepsTheRootPage(t *testing.T) {
	bp := newPool(t, 8)
	tr, err := Create(bg, bp)
	if err != nil {
		t.Fatal(err)
	}
	root := tr.Root()
	val := make([]byte, MaxValueSize)
	m := model{}
	for i := 0; ; i++ {
		k := bytes.Repeat([]byte{byte('a' + i)}, MaxKeySize)
		if err := tr.Insert(bg, k, val); err != nil {
			t.Fatal(err)
		}
		m[string(k)] = val
		st := m.verify(t, tr)
		if st.Height == 2 {
			if i != 5 || st.Leaves != 2 { // five maximum-size cells fit in a leaf
				t.Fatalf("root split at insert %d into %d leaves", i, st.Leaves)
			}
			break
		}
	}
	if tr.Root() != root {
		t.Fatal("root page changed")
	}
	reopened, err := Open(bg, bp, root)
	if err != nil {
		t.Fatal(err)
	}
	m.verify(t, reopened)
}

func TestDeepTreeOfMaximumKeys(t *testing.T) {
	// Maximum-size keys make internal nodes split after a few children,
	// growing the tree several levels and splitting internal roots.
	bp := newPool(t, 16)
	tr, err := Create(bg, bp)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(testSeed(t), 7))
	m := model{}
	for i := range 3000 {
		k := make([]byte, MaxKeySize)
		copy(k, fmt.Sprintf("%08d", rng.IntN(1_000_000)))
		if _, ok := m[string(k)]; ok {
			continue
		}
		v := []byte(strconv.Itoa(i))
		if err := tr.Insert(bg, k, v); err != nil {
			t.Fatal(err)
		}
		m[string(k)] = v
	}
	st := m.verify(t, tr)
	t.Logf("stats %+v", st)
	if st.Height < 5 {
		t.Fatalf("height %d, want a deep tree", st.Height)
	}
}

func TestInsertRejectsBadInput(t *testing.T) {
	bp := newPool(t, 8)
	tr, err := Create(bg, bp)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Insert(bg, make([]byte, MaxKeySize+1), nil); !errors.Is(err, ErrKeyTooLarge) {
		t.Fatalf("long key: %v", err)
	}
	if err := tr.Insert(bg, []byte("k"), make([]byte, MaxValueSize+1)); !errors.Is(err, ErrValueTooLarge) {
		t.Fatalf("long value: %v", err)
	}
	if err := tr.Insert(bg, []byte("k"), []byte("1")); err != nil {
		t.Fatal(err)
	}
	if err := tr.Insert(bg, []byte("k"), []byte("2")); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("duplicate: %v", err)
	}
	// Empty keys and values are allowed.
	if err := tr.Insert(bg, nil, nil); err != nil {
		t.Fatal(err)
	}
	model{"k": []byte("1"), "": {}}.verify(t, tr)
}

func TestFailedSplitChangesNothing(t *testing.T) {
	// With two frames a root split (root plus two new pages) cannot get its
	// pages: the insert fails and the tree is exactly as it was.
	bp, dm := newPoolDM(t, 2)
	tr, err := Create(bg, bp)
	if err != nil {
		t.Fatal(err)
	}
	m := model{}
	val := make([]byte, MaxValueSize)
	inUse := func() uint64 { return dm.PageCount() - dm.FreePageCount() }
	for i := range 5 {
		k := bytes.Repeat([]byte{byte('a' + i)}, MaxKeySize)
		if err := tr.Insert(bg, k, val); err != nil {
			t.Fatal(err)
		}
		m[string(k)] = val
	}
	before, pages := pageImage(t, bp, tr.Root()), inUse()
	err = tr.Insert(bg, bytes.Repeat([]byte{'z'}, MaxKeySize), val)
	if !errors.Is(err, storage.ErrNoFreeFrames) {
		t.Fatalf("insert needing a split with two frames: %v", err)
	}
	if !bytes.Equal(before, pageImage(t, bp, tr.Root())) {
		t.Fatal("failed split changed the root")
	}
	if inUse() != pages {
		t.Fatalf("failed split left %d pages allocated", inUse()-pages)
	}
	m.verify(t, tr)
	if err := bp.Close(bg); err != nil {
		t.Fatalf("pages left pinned: %v", err)
	}
}

// pageImage returns a copy of page id.
func pageImage(t *testing.T, bp *storage.BufferPool, id uint64) []byte {
	t.Helper()
	ref, err := bp.FetchPage(bg, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ref.Unpin(false) }()
	return bytes.Clone(ref.Data())
}

func TestSplitPoint(t *testing.T) {
	cases := []struct {
		sizes []int
		leaf  bool
		want  int
	}{
		{[]int{10, 10}, true, 1},
		{[]int{10, 10, 10, 10}, true, 2},
		{[]int{100, 1, 1, 1}, true, 1},
		{[]int{1, 1, 1, 100}, true, 3},
		{[]int{1, 1, 1, 100}, false, 2},
		{[]int{10, 10, 10, 10, 10}, false, 3},
		{[]int{50, 10, 10, 10, 10, 10}, false, 1},
	}
	for _, c := range cases {
		if got := splitPoint(c.sizes, c.leaf); got != c.want {
			t.Errorf("splitPoint(%v, leaf=%v) = %d, want %d", c.sizes, c.leaf, got, c.want)
		}
	}
}

// maxKey returns a maximum-size key starting with prefix, padded with 'x'.
func maxKey(prefix string) []byte {
	k := bytes.Repeat([]byte{'x'}, MaxKeySize)
	copy(k, prefix)
	return k
}

func TestDuplicateOfASplitSeparator(t *testing.T) {
	// Five maximum-size cells fill a leaf. A sixth insert into a full child
	// leaf splits it at its fourth key; inserting that key again must find
	// it in the right half, not create a second copy in the left.
	bp := newPool(t, 8)
	tr, err := Create(bg, bp)
	if err != nil {
		t.Fatal(err)
	}
	val := make([]byte, MaxValueSize)
	m := model{}
	ins := func(p string) error {
		k := maxKey(p)
		err := tr.Insert(bg, k, val)
		if err == nil {
			m[string(k)] = val
		}
		return err
	}
	for _, p := range []string{"a", "b", "c", "d", "e", "f"} { // root split: [a b c] [d e f]
		if err := ins(p); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"g", "h"} { // right leaf now full: d e f g h
		if err := ins(p); err != nil {
			t.Fatal(err)
		}
	}
	if st := m.verify(t, tr); st.Leaves != 2 {
		t.Fatalf("stats %+v", st)
	}
	// The right leaf splits into [d e f] [g h] with separator g, then the
	// duplicate g is looked up.
	if err := ins("g"); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("duplicate of the separator: %v", err)
	}
	if st := m.verify(t, tr); st.Leaves != 3 {
		t.Fatalf("stats %+v, want the split to have happened", st)
	}
}

func TestLargeSeparatorsAboveSmallInserts(t *testing.T) {
	// Internal nodes must keep room for the largest separator a split can
	// push up, whatever the size of the key being inserted: here small
	// keys sit between maximum-size ones, so small inserts split leaves
	// whose right halves start with maximum-size keys.
	bp := newPool(t, 16)
	tr, err := Create(bg, bp)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(testSeed(t), 21))
	m := model{}
	val := make([]byte, 200)
	for i := range 4000 {
		var k []byte
		p := fmt.Sprintf("%06d", rng.IntN(1_000_000))
		if i%2 == 0 {
			k = maxKey(p)
		} else {
			k = []byte(p)
		}
		if _, ok := m[string(k)]; ok {
			continue
		}
		if err := tr.Insert(bg, k, val); err != nil {
			t.Fatalf("insert %d (%d bytes): %v", i, len(k), err)
		}
		m[string(k)] = val
	}
	st := m.verify(t, tr)
	t.Logf("stats %+v", st)
}
