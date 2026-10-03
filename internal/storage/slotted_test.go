package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
)

// newSlotted returns an empty heap page buffer and its SlottedPage.
func newSlotted(t testing.TB) ([]byte, *SlottedPage) {
	t.Helper()
	buf := make([]byte, PageSize)
	if err := InitPage(buf, Header{ID: 7, Type: PageTypeHeap}); err != nil {
		t.Fatal(err)
	}
	if err := InitSlottedPage(buf); err != nil {
		t.Fatal(err)
	}
	p, err := NewSlottedPage(buf)
	if err != nil {
		t.Fatal(err)
	}
	return buf, p
}

func tuple(n int, fill byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = fill + byte(i)
	}
	return b
}

// slotModel is the trivially correct reference for a slotted page.
type slotModel struct {
	tuples map[int][]byte
}

func newSlotModel() *slotModel { return &slotModel{tuples: map[int][]byte{}} }

// numSlots: the directory always ends at the highest live slot.
func (m *slotModel) numSlots() int {
	n := 0
	for s := range m.tuples {
		n = max(n, s+1)
	}
	return n
}

func (m *slotModel) liveBytes() int {
	n := 0
	for _, d := range m.tuples {
		n += len(d)
	}
	return n
}

func (m *slotModel) lowestDead() int {
	n := m.numSlots()
	for s := range n {
		if _, ok := m.tuples[s]; !ok {
			return s
		}
	}
	return -1
}

func (m *slotModel) totalFree() int {
	return slotAreaSize - slotSize*m.numSlots() - m.liveBytes()
}

func (m *slotModel) insertCapacity() int {
	free := m.totalFree()
	if m.lowestDead() < 0 {
		free -= slotSize
	}
	return max(free, 0)
}

// check compares the page with the model after an operation.
func (m *slotModel) check(t testing.TB, p *SlottedPage, ctx string) {
	t.Helper()
	if err := p.Validate(); err != nil {
		t.Fatalf("%s: Validate: %v", ctx, err)
	}
	if got, want := p.NumSlots(), m.numSlots(); got != want {
		t.Fatalf("%s: NumSlots = %d, model %d", ctx, got, want)
	}
	for s := range p.NumSlots() {
		got, err := p.Get(s)
		want, live := m.tuples[s]
		switch {
		case live && err != nil:
			t.Fatalf("%s: Get(%d): %v", ctx, s, err)
		case live && !bytes.Equal(got, want):
			t.Fatalf("%s: slot %d content differs", ctx, s)
		case !live && !errors.Is(err, ErrSlotNotFound):
			t.Fatalf("%s: dead slot %d: err = %v", ctx, s, err)
		}
	}
	for _, s := range []int{-1, p.NumSlots(), p.NumSlots() + 100, 1 << 20} {
		if _, err := p.Get(s); !errors.Is(err, ErrSlotNotFound) {
			t.Fatalf("%s: Get(%d) err = %v", ctx, s, err)
		}
	}
	if got, want := p.FreeSpace(), m.insertCapacity(); got != want {
		t.Fatalf("%s: FreeSpace = %d, model %d", ctx, got, want)
	}
	if got, want := p.LiveCount(), len(m.tuples); got != want {
		t.Fatalf("%s: LiveCount = %d, model %d", ctx, got, want)
	}
	// Nothing but the headers, the slot directory and live tuples may hold
	// data: freed bytes must be zeroed so deleted rows leave no trace.
	owned := make([]bool, PageSize)
	for i := range slotsOffset + slotSize*p.NumSlots() {
		owned[i] = true
	}
	for s, d := range m.tuples {
		off, _ := p.slot(s)
		for i := off; i < off+len(d); i++ {
			owned[i] = true
		}
	}
	for i, b := range p.buf {
		if !owned[i] && b != 0 {
			t.Fatalf("%s: stale byte %#x at page offset %d", ctx, b, i)
		}
	}
}

const (
	opInsert = iota
	opUpdate
	opDelete
	opCompact
	numOps
)

// step applies one operation to page and model and checks every promise the
// API makes about it, including that failures leave the page untouched.
func (m *slotModel) step(t testing.TB, buf []byte, p *SlottedPage, op, slot, size int, fill byte) {
	t.Helper()
	ctx := fmt.Sprintf("op=%d slot=%d size=%d", op, slot, size)
	before := bytes.Clone(buf)
	data := tuple(size, fill)
	unchanged := func(err error) {
		t.Helper()
		if !bytes.Equal(buf, before) {
			t.Fatalf("%s: page modified although call failed (%v)", ctx, err)
		}
	}
	sizeErr := func() error {
		switch {
		case size == 0:
			return ErrEmptyTuple
		case size > MaxTupleSize:
			return ErrTupleTooLarge
		}
		return nil
	}

	switch op {
	case opInsert:
		got, err := p.Insert(data)
		switch {
		case sizeErr() != nil:
			if !errors.Is(err, sizeErr()) {
				t.Fatalf("%s: err = %v, want %v", ctx, err, sizeErr())
			}
			unchanged(err)
		case size > m.insertCapacity():
			if !errors.Is(err, ErrNoSpace) {
				t.Fatalf("%s: err = %v, want ErrNoSpace (capacity %d)", ctx, err, m.insertCapacity())
			}
			unchanged(err)
		default:
			want := m.lowestDead()
			if want < 0 {
				want = m.numSlots()
			}
			if err != nil || got != want {
				t.Fatalf("%s: Insert = %d, %v; want slot %d (capacity %d)", ctx, got, err, want, m.insertCapacity())
			}
			m.tuples[got] = data
		}
	case opUpdate:
		err := p.Update(slot, data)
		old, live := m.tuples[slot]
		switch {
		case sizeErr() != nil:
			if !errors.Is(err, sizeErr()) {
				t.Fatalf("%s: err = %v, want %v", ctx, err, sizeErr())
			}
			unchanged(err)
		case !live:
			if !errors.Is(err, ErrSlotNotFound) {
				t.Fatalf("%s: err = %v, want ErrSlotNotFound", ctx, err)
			}
			unchanged(err)
		case size > len(old) && size > m.totalFree()+len(old):
			if !errors.Is(err, ErrNoSpace) {
				t.Fatalf("%s: err = %v, want ErrNoSpace", ctx, err)
			}
			unchanged(err)
		default:
			if err != nil {
				t.Fatalf("%s: Update: %v", ctx, err)
			}
			m.tuples[slot] = data
		}
	case opDelete:
		err := p.Delete(slot)
		if _, live := m.tuples[slot]; live {
			if err != nil {
				t.Fatalf("%s: Delete: %v", ctx, err)
			}
			delete(m.tuples, slot)
		} else {
			if !errors.Is(err, ErrSlotNotFound) {
				t.Fatalf("%s: err = %v, want ErrSlotNotFound", ctx, err)
			}
			unchanged(err)
		}
	case opCompact:
		if err := p.Compact(); err != nil {
			t.Fatalf("%s: Compact: %v", ctx, err)
		}
		if gap := p.upper() - p.lower(); gap != m.totalFree() {
			t.Fatalf("%s: after Compact gap = %d, want all free space %d", ctx, gap, m.totalFree())
		}
		// Garbage and the free gap must be zero (no stale data left behind).
		if !bytes.Equal(buf[p.lower():p.upper()], make([]byte, p.upper()-p.lower())) {
			t.Fatalf("%s: free gap not zeroed after Compact", ctx)
		}
	}
	m.check(t, p, ctx)
}

func randomSize(rng *rand.Rand) int {
	switch r := rng.IntN(100); {
	case r < 55:
		return 1 + rng.IntN(64)
	case r < 85:
		return 1 + rng.IntN(1000)
	case r < 97:
		return 1 + rng.IntN(4000)
	case r < 99:
		return MaxTupleSize - rng.IntN(40)
	default:
		return rng.IntN(MaxTupleSize + 20) // includes 0 and oversize
	}
}

func TestSlottedModelBased(t *testing.T) {
	rng := rand.New(rand.NewPCG(testSeed(t), 3))
	pages, steps := 150, 400
	if testing.Short() {
		pages = 20
	}
	for pg := range pages {
		buf, p := newSlotted(t)
		m := newSlotModel()
		for range steps {
			slot := rng.IntN(max(p.NumSlots(), 1) + 2)
			if rng.IntN(20) == 0 {
				slot = rng.IntN(70000) - 100 // wild slot numbers
			}
			m.step(t, buf, p, rng.IntN(numOps), slot, randomSize(rng), byte(rng.UintN(256)))
		}
		_ = pg
	}
}

func TestSlottedEmptyPage(t *testing.T) {
	buf, p := newSlotted(t)
	if p.FreeSpace() != MaxTupleSize || MaxTupleSize != 8148 {
		t.Fatalf("FreeSpace = %d, MaxTupleSize = %d", p.FreeSpace(), MaxTupleSize)
	}
	if p.NumSlots() != 0 || p.NextPage() != 0 || p.NextLive(0) != -1 {
		t.Fatal("fresh page not empty")
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint16(buf[heapOffUpper:]) != PageSize {
		t.Fatal("Upper of empty page is not PageSize")
	}
}

func TestSlottedMaxTupleFillsPageExactly(t *testing.T) {
	_, p := newSlotted(t)
	data := tuple(MaxTupleSize, 1)
	slot, err := p.Insert(data)
	if err != nil || slot != 0 {
		t.Fatalf("Insert max = %d, %v", slot, err)
	}
	if p.FreeSpace() != 0 {
		t.Fatalf("FreeSpace after max tuple = %d", p.FreeSpace())
	}
	if _, err := p.Insert([]byte{1}); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("err = %v, want ErrNoSpace", err)
	}
	got, _ := p.Get(0)
	if !bytes.Equal(got, data) {
		t.Fatal("content differs")
	}
	// One byte too large for an empty page.
	_, p2 := newSlotted(t)
	if _, err := p2.Insert(tuple(MaxTupleSize+1, 1)); !errors.Is(err, ErrTupleTooLarge) {
		t.Fatalf("err = %v", err)
	}
	// Deleting it makes the page empty again, upper reset.
	if err := p.Delete(0); err != nil {
		t.Fatal(err)
	}
	if p.FreeSpace() != MaxTupleSize || p.NumSlots() != 0 {
		t.Fatalf("after delete: FreeSpace %d, slots %d", p.FreeSpace(), p.NumSlots())
	}
	if _, err := p.Insert(data); err != nil {
		t.Fatalf("reinsert max: %v", err)
	}
}

func TestSlottedExactBoundaryWithAndWithoutDeadSlot(t *testing.T) {
	for _, withDead := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadSlot=%v", withDead), func(t *testing.T) {
			_, p := newSlotted(t)
			for range 10 {
				if _, err := p.Insert(tuple(100, 3)); err != nil {
					t.Fatal(err)
				}
			}
			if withDead {
				if err := p.Delete(4); err != nil {
					t.Fatal(err)
				}
				// the 100 bytes freed are garbage in the middle; capacity counts them
			}
			capacity := p.FreeSpace()
			if _, err := p.Insert(tuple(capacity+1, 9)); !errors.Is(err, ErrNoSpace) {
				t.Fatalf("capacity+1 err = %v", err)
			}
			slot, err := p.Insert(tuple(capacity, 9))
			if err != nil {
				t.Fatalf("capacity bytes did not fit: %v", err)
			}
			if withDead && slot != 4 {
				t.Fatalf("dead slot not reused: %d", slot)
			}
			if p.FreeSpace() != 0 {
				t.Fatalf("FreeSpace after exact fill = %d", p.FreeSpace())
			}
			if err := p.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSlottedManyTinyTuplesFillExactly(t *testing.T) {
	_, p := newSlotted(t)
	want := slotAreaSize / (1 + slotSize) // each 1-byte tuple needs 5 bytes
	n := 0
	for {
		if _, err := p.Insert([]byte{byte(n)}); err != nil {
			if !errors.Is(err, ErrNoSpace) {
				t.Fatal(err)
			}
			break
		}
		n++
	}
	if n != want {
		t.Fatalf("fitted %d one-byte tuples, want %d", n, want)
	}
	for s := range n {
		got, err := p.Get(s)
		if err != nil || len(got) != 1 || got[0] != byte(s) {
			t.Fatalf("slot %d = %v, %v", s, got, err)
		}
	}
}

func TestSlottedUpdateCases(t *testing.T) {
	setup := func(t *testing.T) ([]byte, *SlottedPage) {
		buf, p := newSlotted(t)
		for i := range 3 {
			if _, err := p.Insert(tuple(100, byte(i))); err != nil {
				t.Fatal(err)
			}
		}
		return buf, p
	}
	tests := []struct {
		name string
		size int
	}{
		{"same size", 100},
		{"smaller", 10},
		{"one byte", 1},
		{"larger fits in gap", 500},
		{"much larger", 7000},
		{"largest possible", slotAreaSize - 3*slotSize - 200}, // other two tuples and 3 slot entries stay
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, p := setup(t)
			data := tuple(tc.size, 77)
			if err := p.Update(1, data); err != nil {
				t.Fatalf("Update: %v", err)
			}
			if got, _ := p.Get(1); !bytes.Equal(got, data) {
				t.Fatal("updated content wrong")
			}
			for _, s := range []int{0, 2} {
				if got, _ := p.Get(s); !bytes.Equal(got, tuple(100, byte(s))) {
					t.Fatalf("neighbour slot %d damaged", s)
				}
			}
			if err := p.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("too large leaves page untouched", func(t *testing.T) {
		buf, p := setup(t)
		before := bytes.Clone(buf)
		if err := p.Update(1, tuple(slotAreaSize-3*slotSize-200+1, 1)); !errors.Is(err, ErrNoSpace) {
			t.Fatalf("err = %v", err)
		}
		if !bytes.Equal(buf, before) {
			t.Fatal("page changed by failed update")
		}
	})
	t.Run("needs compaction", func(t *testing.T) {
		_, p := newSlotted(t)
		for i := range 20 {
			if _, err := p.Insert(tuple(300, byte(i))); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 20; i += 2 { // leave garbage holes everywhere
			if err := p.Delete(i); err != nil {
				t.Fatal(err)
			}
		}
		gap := p.upper() - p.lower()
		size := gap + 200 // does not fit in the gap, but fits after compaction
		if size > p.FreeSpace()+300 {
			t.Fatalf("test setup wrong: size %d free %d", size, p.FreeSpace())
		}
		if err := p.Update(1, tuple(size, 5)); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if got, _ := p.Get(1); !bytes.Equal(got, tuple(size, 5)) {
			t.Fatal("content wrong")
		}
		for i := 3; i < 20; i += 2 {
			if got, _ := p.Get(i); !bytes.Equal(got, tuple(300, byte(i))) {
				t.Fatalf("slot %d damaged by compaction", i)
			}
		}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("data aliasing the page survives compaction", func(t *testing.T) {
		_, p := newSlotted(t)
		for i := range 3 {
			if _, err := p.Insert(tuple(2500, byte(i+1))); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := p.Insert(tuple(10, 99)); err != nil { // slot 3, lowest address
			t.Fatal(err)
		}
		_ = p.Delete(0)
		_ = p.Delete(1) // 5000 bytes of garbage above the small tuple
		src, err := p.Get(2)
		if err != nil {
			t.Fatal(err)
		}
		want := bytes.Clone(src)
		if gap := p.upper() - p.lower(); len(src) <= gap {
			t.Fatalf("setup wrong: %d bytes fit in gap %d, compaction not forced", len(src), gap)
		}
		// src points into the page and lies in the region compaction moves.
		if err := p.Update(3, src); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if got, _ := p.Get(3); !bytes.Equal(got, want) {
			t.Fatal("aliased input was corrupted by compaction")
		}
		if got, _ := p.Get(2); !bytes.Equal(got, want) {
			t.Fatal("source tuple damaged")
		}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSlottedDeleteBehaviour(t *testing.T) {
	buf, p := newSlotted(t)
	for i := range 5 {
		if _, err := p.Insert(tuple(50, byte(i))); err != nil {
			t.Fatal(err)
		}
	}
	// Middle delete: slot becomes dead, directory keeps its length.
	if err := p.Delete(2); err != nil {
		t.Fatal(err)
	}
	if p.NumSlots() != 5 || p.NextLive(2) != 3 {
		t.Fatalf("NumSlots %d NextLive(2) %d", p.NumSlots(), p.NextLive(2))
	}
	if _, err := p.Get(2); !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("err = %v", err)
	}
	if err := p.Delete(2); !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("double delete err = %v", err)
	}
	// Last-slot delete trims the directory, including dead slots before it.
	if err := p.Delete(4); err != nil {
		t.Fatal(err)
	}
	if p.NumSlots() != 4 {
		t.Fatalf("NumSlots after trim = %d", p.NumSlots())
	}
	if err := p.Delete(3); err != nil {
		t.Fatal(err)
	}
	if p.NumSlots() != 2 { // slot 2 was dead too
		t.Fatalf("NumSlots after cascading trim = %d", p.NumSlots())
	}
	// Lowest dead slot is reused.
	if err := p.Delete(0); err != nil {
		t.Fatal(err)
	}
	if s, err := p.Insert(tuple(5, 9)); err != nil || s != 0 {
		t.Fatalf("Insert = %d, %v, want slot 0", s, err)
	}
	// Deleted bytes are zeroed: nothing of tuple 1 remains after deleting it.
	marker := tuple(50, 1)
	if err := p.Delete(1); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf, marker[:20]) {
		t.Fatal("deleted tuple bytes still in the page")
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSlottedBadArguments(t *testing.T) {
	_, p := newSlotted(t)
	if _, err := p.Insert(nil); !errors.Is(err, ErrEmptyTuple) {
		t.Errorf("Insert(nil) err = %v", err)
	}
	if _, err := p.Insert([]byte{}); !errors.Is(err, ErrEmptyTuple) {
		t.Errorf("Insert(empty) err = %v", err)
	}
	if _, err := p.Insert(make([]byte, MaxTupleSize+1)); !errors.Is(err, ErrTupleTooLarge) {
		t.Errorf("oversize err = %v", err)
	}
	for _, s := range []int{-1, 0, 1, 100} {
		if err := p.Update(s, []byte{1}); !errors.Is(err, ErrSlotNotFound) {
			t.Errorf("Update(%d) err = %v", s, err)
		}
		if err := p.Delete(s); !errors.Is(err, ErrSlotNotFound) {
			t.Errorf("Delete(%d) err = %v", s, err)
		}
	}
	if err := p.Update(0, nil); !errors.Is(err, ErrEmptyTuple) {
		t.Errorf("Update(nil) err = %v", err)
	}
}

func TestSlottedNextPage(t *testing.T) {
	_, p := newSlotted(t)
	p.SetNextPage(1<<63 + 5)
	if p.NextPage() != 1<<63+5 {
		t.Fatal("NextPage round trip failed")
	}
	if _, err := p.Insert(tuple(10, 1)); err != nil {
		t.Fatal(err)
	}
	if p.NextPage() != 1<<63+5 {
		t.Fatal("insert clobbered NextPage")
	}
}

func TestNewSlottedPageRejects(t *testing.T) {
	good, _ := newSlotted(t)
	mutate := func(f func(b []byte)) []byte {
		b := bytes.Clone(good)
		f(b)
		return b
	}
	tests := []struct {
		name string
		buf  []byte
		want error
	}{
		{"nil", nil, ErrBadSize},
		{"short", good[:100], ErrBadSize},
		{"btree page", mutate(func(b []byte) { binary.LittleEndian.PutUint16(b[20:], uint16(PageTypeBTreeLeaf)) }), ErrCorruptPage},
		{"zero page", make([]byte, PageSize), ErrCorruptPage},
		{"reserved bytes set", mutate(func(b []byte) { b[heapOffReserved] = 1 }), ErrCorruptPage},
		{"too many slots", mutate(func(b []byte) { binary.LittleEndian.PutUint16(b[heapOffNumSlots:], 60000) }), ErrCorruptPage},
		{"upper beyond page", mutate(func(b []byte) { binary.LittleEndian.PutUint16(b[heapOffUpper:], PageSize+1) }), ErrCorruptPage},
		{"upper below directory", mutate(func(b []byte) {
			binary.LittleEndian.PutUint16(b[heapOffNumSlots:], 10)
			binary.LittleEndian.PutUint16(b[heapOffUpper:], slotsOffset+8)
		}), ErrCorruptPage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSlottedPage(tc.buf); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if err := InitSlottedPage(mutate(func(b []byte) { binary.LittleEndian.PutUint16(b[20:], uint16(PageTypeBTreeLeaf)) })); !errors.Is(err, ErrCorruptPage) {
		t.Errorf("InitSlottedPage on btree page err = %v", err)
	}
	if err := InitSlottedPage(make([]byte, 5)); !errors.Is(err, ErrBadSize) {
		t.Errorf("InitSlottedPage short err = %v", err)
	}
}

// corruptSlot builds a page with n 100-byte tuples, then lets f edit it.
func corruptPage(t *testing.T, f func(buf []byte, p *SlottedPage)) (*SlottedPage, []byte) {
	t.Helper()
	buf, p := newSlotted(t)
	for i := range 4 {
		if _, err := p.Insert(tuple(100, byte(i))); err != nil {
			t.Fatal(err)
		}
	}
	f(buf, p)
	q, err := NewSlottedPage(buf)
	if err != nil {
		t.Fatalf("setup rejected by NewSlottedPage: %v", err)
	}
	return q, buf
}

func TestValidateAndOpsRejectCorruptSlots(t *testing.T) {
	tests := []struct {
		name string
		edit func(buf []byte, p *SlottedPage)
	}{
		{"half dead slot", func(_ []byte, p *SlottedPage) { p.setSlot(1, 0, 5) }},
		{"zero length live slot", func(_ []byte, p *SlottedPage) { off, _ := p.slot(1); p.setSlot(1, off, 0) }},
		{"offset below upper", func(_ []byte, p *SlottedPage) { p.setSlot(1, p.upper()-1, 100) }},
		{"offset inside directory", func(_ []byte, p *SlottedPage) { p.setSlot(1, slotsOffset, 100) }},
		{"tuple past end of page", func(_ []byte, p *SlottedPage) { p.setSlot(1, PageSize-10, 100) }},
		{"length wraps uint16", func(_ []byte, p *SlottedPage) { off, _ := p.slot(1); p.setSlot(1, off, 65535) }},
		{"overlapping tuples", func(_ []byte, p *SlottedPage) { off, _ := p.slot(0); p.setSlot(1, off+50, 100) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := corruptPage(t, tc.edit)
			if err := p.Validate(); !errors.Is(err, ErrCorruptPage) {
				t.Fatalf("Validate err = %v, want ErrCorruptPage", err)
			}
			// No operation may panic on a corrupt page.
			for s := -1; s < p.NumSlots()+2; s++ {
				_, _ = p.Get(s)
				_ = p.Update(s, []byte{1, 2, 3})
				_ = p.Delete(s)
			}
			_, _ = p.Insert([]byte{1})
			_ = p.Compact()
			_ = p.FreeSpace()
			_ = p.NextLive(0)
			_ = p.LiveCount()
		})
	}
}

func TestSlottedMutationsOnCorruptPageFailCleanly(t *testing.T) {
	p, buf := corruptPage(t, func(_ []byte, p *SlottedPage) { p.setSlot(2, PageSize-5, 100) })
	before := bytes.Clone(buf)
	if _, err := p.Insert([]byte{1}); !errors.Is(err, ErrCorruptPage) {
		t.Errorf("Insert err = %v", err)
	}
	if err := p.Compact(); !errors.Is(err, ErrCorruptPage) {
		t.Errorf("Compact err = %v", err)
	}
	if !bytes.Equal(buf, before) {
		t.Error("failed mutation modified a corrupt page")
	}
	if p.FreeSpace() != 0 {
		t.Error("corrupt page reports free space")
	}
}

func TestSlottedTuplesTooBigForPage(t *testing.T) {
	// Lengths that individually look fine but together exceed the page.
	p, _ := corruptPage(t, func(_ []byte, p *SlottedPage) {
		for s := range 4 {
			off, _ := p.slot(s)
			p.setSlot(s, off, 4000) // 16000 bytes of live data, overlapping
		}
		p.setUpper(PageSize - 4000)
	})
	if err := p.Compact(); err == nil {
		t.Fatal("Compact accepted tuples larger than the page")
	}
	if _, err := p.Insert([]byte{1}); err == nil {
		t.Fatal("Insert accepted overcommitted page")
	}
}

func BenchmarkSlottedInsertDelete(b *testing.B) {
	buf := make([]byte, PageSize)
	if err := InitPage(buf, Header{ID: 1, Type: PageTypeHeap}); err != nil {
		b.Fatal(err)
	}
	if err := InitSlottedPage(buf); err != nil {
		b.Fatal(err)
	}
	p, _ := NewSlottedPage(buf)
	data := tuple(100, 1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s, err := p.Insert(data)
		if err != nil {
			b.Fatal(err)
		}
		_ = p.Delete(s)
	}
}

func BenchmarkSlottedGet(b *testing.B) {
	buf := make([]byte, PageSize)
	_ = InitPage(buf, Header{ID: 1, Type: PageTypeHeap})
	_ = InitSlottedPage(buf)
	p, _ := NewSlottedPage(buf)
	for range 50 {
		_, _ = p.Insert(tuple(100, 1))
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := p.Get(i % 50); err != nil {
			b.Fatal(err)
		}
	}
}
