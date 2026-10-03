package btree

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
)

// sortedKeys returns the model's keys in order.
func (m model) sortedKeys() [][]byte {
	keys := make([][]byte, 0, len(m))
	for k := range m {
		keys = append(keys, []byte(k))
	}
	sortKeys(keys)
	return keys
}

// inRange reports whether k lies between the bounds.
func inRange(k []byte, start, end Bound) bool {
	switch start.Kind {
	case Inclusive:
		if bytes.Compare(k, start.Key) < 0 {
			return false
		}
	case Exclusive:
		if bytes.Compare(k, start.Key) <= 0 {
			return false
		}
	}
	switch end.Kind {
	case Inclusive:
		return bytes.Compare(k, end.Key) <= 0
	case Exclusive:
		return bytes.Compare(k, end.Key) < 0
	}
	return true
}

// collect runs a scan to the end.
func collect(t *testing.T, tr *Tree, start, end Bound) (keys, vals [][]byte) {
	t.Helper()
	it := tr.Scan(start, end)
	for {
		k, v, ok, err := it.Next(bg)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !ok {
			break
		}
		keys, vals = append(keys, k), append(vals, v)
	}
	// Exhausted iterators stay exhausted.
	if _, _, ok, err := it.Next(bg); ok || err != nil {
		t.Fatalf("Next after the end: %v %v", ok, err)
	}
	return keys, vals
}

// checkScan compares a scan with the model.
func checkScan(t *testing.T, tr *Tree, m model, sorted [][]byte, start, end Bound) int {
	t.Helper()
	keys, vals := collect(t, tr, start, end)
	var want [][]byte
	for _, k := range sorted {
		if inRange(k, start, end) {
			want = append(want, k)
		}
	}
	if len(keys) != len(want) {
		t.Fatalf("scan %v..%v returned %d keys, want %d", start, end, len(keys), len(want))
	}
	for i := range keys {
		if !bytes.Equal(keys[i], want[i]) || !bytes.Equal(vals[i], m[string(want[i])]) {
			t.Fatalf("scan %v..%v: entry %d is %x, want %x", start, end, i, keys[i], want[i])
		}
	}
	return len(keys)
}

// boundAround returns a random bound near the model's keys: equal to one,
// just below or above one, or far outside.
func boundAround(rng *rand.Rand, sorted [][]byte) Bound {
	kind := []BoundKind{Unbounded, Inclusive, Exclusive}[rng.IntN(3)]
	if kind == Unbounded {
		return Bound{}
	}
	var k []byte
	switch r := rng.IntN(10); {
	case r == 0 || len(sorted) == 0:
		k = []byte{} // below everything
	case r == 1:
		k = bytes.Repeat([]byte{0xFF}, 8) // above everything
	default:
		k = bytes.Clone(sorted[rng.IntN(len(sorted))])
		switch rng.IntN(3) {
		case 1:
			k = append(k, 0) // just above
		case 2:
			if len(k) > 0 && k[len(k)-1] > 0 {
				k[len(k)-1]-- // somewhat below
			}
		}
	}
	return Bound{Kind: kind, Key: k}
}

func TestScanAgainstModel(t *testing.T) {
	seed := testSeed(t)
	for _, c := range []struct{ frames, maxLen, n int }{{8, 24, 3000}, {16, 400, 3000}, {8, 24, 0}, {8, 24, 1}} {
		t.Run(fmt.Sprintf("frames=%d,keys<=%d,n=%d", c.frames, c.maxLen, c.n), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, uint64(c.n+c.maxLen)))
			g := keyGen{rng: rng, maxLen: c.maxLen, large: 2}
			bp := newPool(t, c.frames)
			tr, err := Create(bg, bp)
			if err != nil {
				t.Fatal(err)
			}
			m := model{}
			for len(m) < c.n {
				k, v := g.key(), g.value()
				if _, ok := m[string(k)]; ok {
					continue
				}
				if err := tr.Insert(bg, k, v); err != nil {
					t.Fatal(err)
				}
				m[string(k)] = v
			}
			sorted := m.sortedKeys()
			if got := checkScan(t, tr, m, sorted, Bound{}, Bound{}); got != c.n {
				t.Fatalf("full scan: %d keys", got)
			}
			// One descent per leaf, not per row.
			st, err := tr.Check(bg)
			if err != nil {
				t.Fatal(err)
			}
			it := tr.Scan(Bound{}, Bound{})
			for {
				_, _, ok, err := it.Next(bg)
				if err != nil {
					t.Fatal(err)
				}
				if !ok {
					break
				}
			}
			if it.descents != st.Leaves {
				t.Fatalf("full scan of %d leaves descended %d times", st.Leaves, it.descents)
			}
			var empty, partial int
			for range 400 {
				start, end := boundAround(rng, sorted), boundAround(rng, sorted)
				switch n := checkScan(t, tr, m, sorted, start, end); {
				case n == 0:
					empty++
				case n < c.n:
					partial++
				}
			}
			if c.n > 1 && (empty == 0 || partial == 0) {
				t.Fatalf("bounds produced %d empty and %d partial scans", empty, partial)
			}
			if err := bp.Close(bg); err != nil {
				t.Fatalf("pages left pinned: %v", err)
			}
		})
	}
}

func TestScanBoundsExactly(t *testing.T) {
	bp := newPool(t, 8)
	keys := intKeys(100, 2) // 0, 2, ..., 98
	tr := buildTree(t, bp, keys, 4, 3)
	k := func(i int) []byte { return AppendInt64(nil, int64(i)) }
	ints := func(ks [][]byte) []int64 {
		var out []int64
		for _, key := range ks {
			vs, err := DecodeKey(key)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, vs[0].Int64)
		}
		return out
	}
	cases := []struct {
		start, end Bound
		want       []int64
	}{
		{Incl(k(10)), Incl(k(14)), []int64{10, 12, 14}},
		{Excl(k(10)), Incl(k(14)), []int64{12, 14}},
		{Incl(k(10)), Excl(k(14)), []int64{10, 12}},
		{Excl(k(10)), Excl(k(14)), []int64{12}},
		{Incl(k(11)), Incl(k(13)), []int64{12}},
		{Excl(k(11)), Excl(k(13)), []int64{12}},
		{Incl(k(12)), Excl(k(12)), nil},
		{Incl(k(12)), Incl(k(12)), []int64{12}},
		{Excl(k(12)), Incl(k(12)), nil},
		{Incl(k(20)), Incl(k(10)), nil},
		{Bound{}, Excl(k(4)), []int64{0, 2}},
		{Excl(k(94)), Bound{}, []int64{96, 98}},
		{Incl(k(-5)), Incl(k(1)), []int64{0}},
		{Incl(k(97)), Incl(k(500)), []int64{98}},
		{Incl(k(99)), Bound{}, nil},
		{Bound{}, Excl(k(0)), nil},
	}
	for _, c := range cases {
		got, _ := collect(t, tr, c.start, c.end)
		if g := ints(got); fmt.Sprint(g) != fmt.Sprint(c.want) {
			t.Errorf("scan %v..%v = %v, want %v", c.start, c.end, g, c.want)
		}
	}
}

func TestScanOverEmptyLeavesAndNodes(t *testing.T) {
	// Leaves emptied without repair (as after a crash) are skipped.
	bp := newPool(t, 8)
	keys := intKeys(27, 1)
	tr := buildTree(t, bp, keys, 3, 2)
	// Empty the leaves holding 3..8 behind the tree's back.
	for _, i := range []int{3, 6} {
		ref, _, err := tr.descendShared(bg, keys[i])
		if err != nil {
			t.Fatal(err)
		}
		id := ref.ID()
		ref.RUnlock()
		_ = ref.Unpin(false)
		mutatePage(t, bp, id, func(n node) {
			for n.numCells() > 0 {
				_ = n.remove(0)
			}
		})
	}
	m := model{}
	for i, k := range keys {
		if i < 3 || i >= 9 {
			m[string(k)] = valueFor(k)
		}
	}
	sorted := m.sortedKeys()
	checkScan(t, tr, m, sorted, Bound{}, Bound{})
	checkScan(t, tr, m, sorted, Incl(keys[4]), Incl(keys[10]))
	checkScan(t, tr, m, sorted, Excl(keys[2]), Excl(keys[9]))
}

func TestScanInterleavedWithWrites(t *testing.T) {
	// Writes between Next calls: keys stay strictly increasing; keys present
	// throughout are all returned; nothing else that was never in the tree
	// appears.
	rng := rand.New(rand.NewPCG(testSeed(t), 41))
	g := keyGen{rng: rng, maxLen: 60}
	bp := newPool(t, 16)
	tr, err := Create(bg, bp)
	if err != nil {
		t.Fatal(err)
	}
	m := model{}
	for len(m) < 3000 {
		k, v := g.key(), g.value()
		if tr.Insert(bg, k, v) == nil {
			m[string(k)] = v
		}
	}
	stable := map[string]bool{} // never deleted during the scan
	for k := range m {
		if rng.IntN(2) == 0 {
			stable[k] = true
		}
	}
	ever := map[string]bool{}
	for k := range m {
		ever[k] = true
	}
	it := tr.Scan(Bound{}, Bound{})
	var prev []byte
	seen := map[string]bool{}
	for {
		k, _, ok, err := it.Next(bg)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		if prev != nil && bytes.Compare(prev, k) >= 0 {
			t.Fatalf("scan went from %x to %x", prev, k)
		}
		prev = k
		if !ever[string(k)] {
			t.Fatalf("scan returned %x, which was never inserted", k)
		}
		seen[string(k)] = true
		// Churn: insert new keys and delete unstable ones, all over the tree.
		for range 3 {
			nk := g.key()
			if tr.Insert(bg, nk, nil) == nil {
				ever[string(nk)] = true
				m[string(nk)] = []byte{}
			}
			for dk := range m {
				if !stable[dk] {
					if _, err := tr.Delete(bg, []byte(dk)); err != nil {
						t.Fatal(err)
					}
					delete(m, dk)
				}
				break
			}
		}
	}
	for k := range stable {
		if !seen[k] {
			t.Fatalf("stable key %x was not returned", k)
		}
	}
	m.verify(t, tr)
}

func TestScanErrorIsSticky(t *testing.T) {
	bp := newPool(t, 8)
	keys := intKeys(40, 1)
	tr := buildTree(t, bp, keys, 4, 3)
	it := tr.Scan(Bound{}, Bound{})
	if _, _, ok, err := it.Next(bg); !ok || err != nil {
		t.Fatal(err)
	}
	// Damage the root: the next refill fails, and so does every call after.
	var level int
	mutatePage(t, bp, tr.Root(), func(n node) { level = n.level(); n.put16(offLevel, 0) })
	var err error
	for range 10 {
		if _, _, _, err = it.Next(bg); err != nil {
			break
		}
	}
	if !errors.Is(err, ErrCorruptNode) {
		t.Fatalf("Next on a corrupt tree: %v", err)
	}
	// Even once the damage is gone, the iterator stays failed.
	mutatePage(t, bp, tr.Root(), func(n node) { n.put16(offLevel, level) })
	if _, err := tr.Check(bg); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err2 := it.Next(bg); ok || !errors.Is(err2, ErrCorruptNode) {
		t.Fatalf("Next after an error: %v %v", ok, err2)
	}
}

func TestScanStopsAtTheEndBound(t *testing.T) {
	// Leaves hold four keys each (0..3, 4..7, ...): a scan whose end lies in
	// or at the end of the first leaf must not descend again.
	bp := newPool(t, 8)
	keys := intKeys(40, 1)
	tr := buildTree(t, bp, keys, 4, 3)
	for _, end := range []Bound{Incl(keys[2]), Excl(keys[3]), Incl(keys[3]), Excl(keys[4])} {
		it := tr.Scan(Bound{}, end)
		n := 0
		for {
			_, _, ok, err := it.Next(bg)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			n++
		}
		if it.descents != 1 {
			t.Errorf("scan to %v returned %d keys with %d descents, want 1", end, n, it.descents)
		}
	}
}

func TestScanCopiesItsBounds(t *testing.T) {
	bp := newPool(t, 8)
	keys := intKeys(40, 1)
	tr := buildTree(t, bp, keys, 4, 3)
	start, end := bytes.Clone(keys[10]), bytes.Clone(keys[12])
	it := tr.Scan(Incl(start), Incl(end))
	copy(start, keys[0]) // the caller reuses its buffers
	copy(end, keys[39])
	var got int
	for {
		_, _, ok, err := it.Next(bg)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		got++
	}
	if got != 3 {
		t.Fatalf("scan saw %d keys after its bound buffers changed, want 3", got)
	}
}
