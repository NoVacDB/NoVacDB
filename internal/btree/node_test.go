package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
)

// newNodeBuf returns an empty node page with ID id.
func newNodeBuf(t testing.TB, leaf bool, level int) node {
	t.Helper()
	buf := make([]byte, storage.PageSize)
	typ := storage.PageTypeBTreeInternal
	if leaf {
		typ = storage.PageTypeBTreeLeaf
	}
	if err := storage.InitPage(buf, storage.Header{ID: 9, LSN: 77, Type: typ}); err != nil {
		t.Fatal(err)
	}
	if err := initNode(buf, leaf, level); err != nil {
		t.Fatal(err)
	}
	if !leaf {
		node{buf}.setChild0(storage.FirstDataPage)
	}
	return node{buf}
}

type kv struct{ k, v []byte }

func randBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.IntN(256))
	}
	return b
}

// checkLeafModel checks a leaf against a sorted model.
func checkLeafModel(t *testing.T, n node, model []kv) {
	t.Helper()
	if err := n.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if n.numCells() != len(model) {
		t.Fatalf("%d cells, model has %d", n.numCells(), len(model))
	}
	used := slotSize * len(model)
	for i, e := range model {
		k, err := n.key(i)
		if err != nil {
			t.Fatal(err)
		}
		v, err := n.value(i)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(k, e.k) || !bytes.Equal(v, e.v) {
			t.Fatalf("cell %d = %x/%x, model %x/%x", i, k, v, e.k, e.v)
		}
		used += leafCellSize(e.k, e.v)
	}
	got, err := n.used()
	if err != nil || got != used {
		t.Fatalf("used = %d, %v; want %d", got, err, used)
	}
}

func TestNodeModel(t *testing.T) {
	rng := rand.New(rand.NewPCG(testSeed(t), 3))
	n := newNodeBuf(t, true, 0)
	var model []kv
	var inserts, deletes, compactions, fulls int
	for op := range 20000 {
		if len(model) > 0 && rng.IntN(100) < 45 {
			i := rng.IntN(len(model))
			if err := n.remove(i); err != nil {
				t.Fatalf("op %d: remove %d: %v", op, i, err)
			}
			model = slices.Delete(model, i, i+1)
			deletes++
		} else {
			// Mostly small cells, sometimes maximum-size ones.
			kl, vl := rng.IntN(40), rng.IntN(40)
			if rng.IntN(10) == 0 {
				kl, vl = MaxKeySize, MaxValueSize
			}
			k, v := randBytes(rng, kl), randBytes(rng, vl)
			i, found, err := n.search(k)
			if err != nil {
				t.Fatal(err)
			}
			if found {
				continue
			}
			gapBefore := n.gap()
			err = n.insertLeaf(i, k, v)
			if errors.Is(err, errNodeFull) {
				used, _ := n.used()
				if nodeSpace-used >= leafCellSize(k, v)+slotSize {
					t.Fatalf("op %d: full with %d bytes free for a %d-byte cell", op, nodeSpace-used, leafCellSize(k, v))
				}
				fulls++
				continue
			}
			if err != nil {
				t.Fatalf("op %d: insert: %v", op, err)
			}
			if gapBefore < leafCellSize(k, v)+slotSize {
				compactions++
			}
			model = slices.Insert(model, i, kv{k, v})
			inserts++
		}
		checkLeafModel(t, n, model)
	}
	t.Logf("%d inserts, %d deletes, %d compactions, %d full", inserts, deletes, compactions, fulls)
	if compactions == 0 || fulls == 0 || deletes == 0 {
		t.Fatal("test did not exercise compaction, full nodes and deletes")
	}
}

func TestNodeInternalCells(t *testing.T) {
	n := newNodeBuf(t, false, 3)
	keys := [][]byte{[]byte("b"), []byte("d"), []byte("f")}
	for i, k := range keys {
		if err := n.insertInner(i, k, uint64(100+i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		key   string
		pos   int
		child uint64
	}{
		{"", 0, storage.FirstDataPage}, {"a", 0, storage.FirstDataPage}, {"b", 1, 100}, {"c", 1, 100},
		{"d", 2, 101}, {"e", 2, 101}, {"f", 3, 102}, {"z", 3, 102},
	}
	for _, c := range cases {
		pos, child, err := n.childFor([]byte(c.key))
		if err != nil || pos != c.pos || child != c.child {
			t.Errorf("childFor(%q) = %d, %d, %v; want %d, %d", c.key, pos, child, err, c.pos, c.child)
		}
	}
	n.setCellChild(1, 555)
	if c, _ := n.childAt(2); c != 555 {
		t.Errorf("setCellChild: child %d", c)
	}
	if err := n.remove(0); err != nil {
		t.Fatal(err)
	}
	if k, _ := n.key(0); string(k) != "d" {
		t.Errorf("after remove, first key %q", k)
	}
	if err := n.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNodeCapacity(t *testing.T) {
	maxKey := func(i int) []byte {
		k := bytes.Repeat([]byte{byte(i)}, MaxKeySize)
		return k
	}
	leaf := newNodeBuf(t, true, 0)
	val := make([]byte, MaxValueSize)
	for i := range 5 {
		if err := leaf.insertLeaf(i, maxKey(i), val); err != nil {
			t.Fatalf("max leaf cell %d: %v", i, err)
		}
	}
	if err := leaf.insertLeaf(5, maxKey(5), val); !errors.Is(err, errNodeFull) {
		t.Fatalf("sixth max leaf cell: %v", err)
	}
	inner := newNodeBuf(t, false, 1)
	for i := range 7 {
		if err := inner.insertInner(i, maxKey(i), 10); err != nil {
			t.Fatalf("max internal cell %d: %v", i, err)
		}
	}
	if err := inner.insertInner(7, maxKey(7), 10); !errors.Is(err, errNodeFull) {
		t.Fatalf("eighth max internal cell: %v", err)
	}
	// The design doc's numbers.
	if maxLeafCell+slotSize != 1543 || nodeSpace != 8144 || underfullSize != 2036 {
		t.Fatalf("limits changed: %d %d %d", maxLeafCell+slotSize, nodeSpace, underfullSize)
	}
	// Exactly full: a cell that fits to the byte.
	n := newNodeBuf(t, true, 0)
	for i := 0; ; i++ {
		k := binary.BigEndian.AppendUint32(nil, uint32(i))
		free := n.gap()
		if free < leafCellSize(k, nil)+slotSize {
			// Fill the rest exactly with one value-sized cell.
			rest := free - slotSize - leafCellHdr - len(k)
			if rest < 0 {
				break
			}
			if err := n.insertLeaf(i, k, make([]byte, rest)); err != nil {
				t.Fatalf("exact fill: %v", err)
			}
			if n.gap() != 0 {
				t.Fatalf("gap %d after exact fill", n.gap())
			}
			break
		}
		if err := n.insertLeaf(i, k, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNodeValidateRejectsCorruption(t *testing.T) {
	build := func() node {
		n := newNodeBuf(t, true, 0)
		for i, k := range []string{"a", "b", "c"} {
			if err := n.insertLeaf(i, []byte(k), []byte("vv")); err != nil {
				t.Fatal(err)
			}
		}
		if err := n.validate(); err != nil {
			t.Fatal(err)
		}
		return n
	}
	corruptions := map[string]func(n node){
		"page type":         func(n node) { binary.LittleEndian.PutUint16(n.b[20:], uint16(storage.PageTypeHeap)) },
		"too many cells":    func(n node) { n.setNumCells(5000) },
		"upper past end":    func(n node) { n.setUpper(storage.PageSize + 1) },
		"upper below slots": func(n node) { n.setUpper(slotsStart + 2) },
		"leaf level":        func(n node) { n.put16(offLevel, 1) },
		"leaf child":        func(n node) { n.setChild0(5) },
		"reserved":          func(n node) { n.put16(offReserved, 1) },
		"reserved2":         func(n node) { n.b[offReserve2+7] = 1 },
		"slot below upper":  func(n node) { n.put16(slotsStart, n.upper()-1) },
		"slot past end":     func(n node) { n.put16(slotsStart, storage.PageSize-2) },
		"key too long":      func(n node) { n.put16(n.slot(0), MaxKeySize+1) },
		"value too long":    func(n node) { n.put16(n.slot(0)+2, MaxValueSize+1) },
		"cell overruns":     func(n node) { n.put16(n.slot(2), 2000) },
		"keys out of order": func(n node) { s0, s1 := n.slot(0), n.slot(1); n.put16(slotsStart, s1); n.put16(slotsStart+2, s0) },
		"duplicate key":     func(n node) { n.put16(slotsStart+2, n.slot(0)) },
		"overlap":           func(n node) { n.put16(slotsStart+2, n.slot(0)+1) },
		"dirty free byte":   func(n node) { n.b[slotsStart+10] = 1 },
		"dirty below upper": func(n node) { n.b[n.upper()-1] = 1 },
	}
	for name, f := range corruptions {
		n := build()
		f(n)
		if err := n.validate(); !errors.Is(err, ErrCorruptNode) {
			t.Errorf("%s: validate = %v", name, err)
		}
	}
	internal := map[string]func(n node){
		"level zero":   func(n node) { n.put16(offLevel, 0) },
		"level huge":   func(n node) { n.put16(offLevel, maxLevel+1) },
		"child0 zero":  func(n node) { n.setChild0(0) },
		"cell child 1": func(n node) { n.setCellChild(0, 1) },
	}
	for name, f := range internal {
		n := newNodeBuf(t, false, 2)
		if err := n.insertInner(0, []byte("k"), 40); err != nil {
			t.Fatal(err)
		}
		if err := n.validate(); err != nil {
			t.Fatal(err)
		}
		f(n)
		if err := n.validate(); !errors.Is(err, ErrCorruptNode) {
			t.Errorf("internal %s: validate = %v", name, err)
		}
	}
}

func TestNodeCompactionRejectsOverlap(t *testing.T) {
	// Two slots pointing at one big cell: compaction would need more room
	// than the node has, and must fail rather than overrun.
	n := newNodeBuf(t, true, 0)
	big := make([]byte, MaxValueSize)
	for i := range 5 {
		if err := n.insertLeaf(i, []byte{byte(i)}, big); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i < 5; i++ {
		n.put16(slotsStart+slotSize*i, n.slot(0))
	}
	// Claim more cells than fit, all on the same bytes.
	for i := 5; i < 20; i++ {
		n.put16(slotsStart+slotSize*i, n.slot(0))
	}
	n.setNumCells(20)
	if err := n.compact(); !errors.Is(err, ErrCorruptNode) {
		t.Fatalf("compact = %v", err)
	}
}

// FuzzNode feeds arbitrary bytes to every node operation: none may panic,
// and a node that validates must still validate after any operation that
// succeeds.
func FuzzNode(f *testing.F) {
	n := newNodeBuf(f, true, 0)
	for i, k := range []string{"a", "bb", "ccc"} {
		_ = n.insertLeaf(i, []byte(k), []byte("value"))
	}
	f.Add(bytes.Clone(n.b), []byte("b"), uint16(1), uint8(0))
	in := newNodeBuf(f, false, 1)
	_ = in.insertInner(0, []byte("m"), 12)
	f.Add(bytes.Clone(in.b), []byte("z"), uint16(0), uint8(1))
	f.Fuzz(func(t *testing.T, page, key []byte, idx uint16, op uint8) {
		if len(page) < storage.PageSize {
			page = append(page, make([]byte, storage.PageSize-len(page))...)
		}
		n := node{page[:storage.PageSize]}
		if n.checkHeader() != nil {
			return
		}
		valid := n.validate() == nil
		if len(key) > MaxKeySize {
			key = key[:MaxKeySize]
		}
		i := int(idx)
		var err error
		inOrder := true
		switch op % 7 {
		case 0:
			_, _, err = n.search(key)
		case 1, 6:
			if op%7 == 1 {
				// At the key's place, which keeps the node valid.
				var found bool
				i, found, err = n.search(key)
				if err != nil || found {
					return
				}
			} else {
				inOrder = false // anywhere: must not panic
			}
			if n.isLeaf() {
				err = n.insertLeaf(i, key, key[:len(key)/2])
			} else {
				err = n.insertInner(i, key, 99)
			}
		case 2:
			err = n.remove(i)
		case 3:
			err = n.compact()
		case 4:
			if !n.isLeaf() {
				_, _, err = n.childFor(key)
			} else {
				_, err = n.value(i)
			}
		case 5:
			_, err = n.used()
		}
		if valid && inOrder && err == nil && n.validate() != nil {
			t.Fatalf("op %d made a valid node invalid: %v", op%7, n.validate())
		}
	})
}

func TestNodeRemoveReclaimsTheLowestCell(t *testing.T) {
	n := newNodeBuf(t, true, 0)
	if err := n.insertLeaf(0, []byte("a"), []byte("1")); err != nil {
		t.Fatal(err)
	}
	gap := n.gap()
	if err := n.insertLeaf(1, []byte("b"), []byte("22")); err != nil {
		t.Fatal(err)
	}
	if err := n.remove(1); err != nil { // the lowest cell: Upper moves back up
		t.Fatal(err)
	}
	if n.gap() != gap || n.upper() != storage.PageSize-leafCellSize([]byte("a"), []byte("1")) {
		t.Fatalf("gap %d, upper %d after removing the lowest cell; want gap %d", n.gap(), n.upper(), gap)
	}
}

func TestNodeValidateRejectsEqualKeysAndOverruns(t *testing.T) {
	n := newNodeBuf(t, true, 0)
	for i, k := range []string{"a", "b"} {
		if err := n.insertLeaf(i, []byte(k), nil); err != nil {
			t.Fatal(err)
		}
	}
	// Two distinct cells with the same key.
	k, _ := n.key(1)
	k[0] = 'a'
	if err := n.validate(); !errors.Is(err, ErrCorruptNode) {
		t.Fatalf("equal keys: validate = %v", err)
	}
	// A cell whose lengths are in range but which runs off the page: the
	// accessors must refuse it rather than slice past the end.
	n = newNodeBuf(t, true, 0)
	if err := n.insertLeaf(0, []byte("abc"), []byte("defgh")); err != nil {
		t.Fatal(err)
	}
	// Lengths 3 and 5 at five bytes from the end: in range, but the cell
	// would need twelve bytes.
	at := storage.PageSize - 5
	n.put16(slotsStart, at)
	n.put16(at, 3)
	n.put16(at+2, 5)
	n.setUpper(storage.PageSize - 12)
	if _, err := n.key(0); !errors.Is(err, ErrCorruptNode) {
		t.Fatalf("overrunning cell: key = %v", err)
	}
	if err := n.validate(); !errors.Is(err, ErrCorruptNode) {
		t.Fatalf("overrunning cell: validate = %v", err)
	}
}

func TestNodeValidateRejectsNestedCells(t *testing.T) {
	// Cell B lies inside cell A's value, with a valid header of its own and
	// a key that sorts after A's: only the overlap check can catch it.
	n := newNodeBuf(t, true, 0)
	inner := []byte{1, 0, 0, 0, 0, 'z'} // key "z", empty value, no flags
	if err := n.insertLeaf(0, nil, inner); err != nil {
		t.Fatal(err)
	}
	if err := n.validate(); err != nil {
		t.Fatal(err)
	}
	b := n.slot(0) + leafCellHdr // where A's value starts
	n.put16(slotsStart+slotSize, b)
	n.setNumCells(2)
	if k, err := n.key(1); err != nil || string(k) != "z" {
		t.Fatalf("crafted cell: %q, %v", k, err)
	}
	if err := n.validate(); !errors.Is(err, ErrCorruptNode) {
		t.Fatalf("validate = %v", err)
	}
}

func TestLeafFlagsAreZeroAndChecked(t *testing.T) {
	n := newNodeBuf(t, true, 0)
	if err := n.insertLeaf(0, []byte("k"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	if f := n.b[n.slot(0)+leafOffFlags]; f != 0 {
		t.Fatalf("flags %#x", f)
	}
	for _, f := range []byte{1, 0x80, 0xff} {
		n.b[n.slot(0)+leafOffFlags] = f
		if _, err := n.key(0); !errors.Is(err, ErrCorruptNode) {
			t.Errorf("flags %#x: key = %v", f, err)
		}
		if _, err := n.value(0); !errors.Is(err, ErrCorruptNode) {
			t.Errorf("flags %#x: value = %v", f, err)
		}
		if err := n.validate(); !errors.Is(err, ErrCorruptNode) {
			t.Errorf("flags %#x: validate = %v", f, err)
		}
	}
}
