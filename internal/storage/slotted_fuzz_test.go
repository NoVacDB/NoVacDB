package storage

import (
	"encoding/binary"
	"testing"
)

// opsSeed encodes a few interesting operation sequences as fuzz seeds.
// Each operation is 4 bytes: op, slot, size low, size high.
func opsSeed(ops ...[4]byte) []byte {
	var b []byte
	for _, o := range ops {
		b = append(b, o[:]...)
	}
	return b
}

// FuzzSlottedOps decodes an operation sequence from the input and runs it
// against a page and the reference model, which checks the results of every
// call and every invariant after every step.
func FuzzSlottedOps(f *testing.F) {
	f.Add(opsSeed([4]byte{opInsert, 0, 100, 0}, [4]byte{opInsert, 0, 50, 0}, [4]byte{opDelete, 0, 0, 0}, [4]byte{opInsert, 0, 10, 0}))
	f.Add(opsSeed([4]byte{opInsert, 0, 0xD4, 0x1F}, [4]byte{opInsert, 0, 1, 0}, [4]byte{opUpdate, 0, 5, 0}))
	f.Add(opsSeed([4]byte{opInsert, 0, 0, 8}, [4]byte{opInsert, 0, 0, 8}, [4]byte{opInsert, 0, 0, 8}, [4]byte{opDelete, 1, 0, 0}, [4]byte{opUpdate, 0, 0, 0x14}, [4]byte{opCompact, 0, 0, 0}))
	f.Add(opsSeed([4]byte{opInsert, 0, 0, 0}, [4]byte{opUpdate, 200, 4, 0}, [4]byte{opDelete, 255, 0, 0}))
	f.Fuzz(func(t *testing.T, data []byte) {
		buf, p := newSlotted(t)
		m := newSlotModel()
		fill := byte(1)
		for len(data) >= 4 && fill != 0 {
			op := int(data[0]) % numOps
			slot := int(data[1])
			size := int(binary.LittleEndian.Uint16(data[2:4])) % (MaxTupleSize + 30)
			data = data[4:]
			m.step(t, buf, p, op, slot, size, fill)
			fill += 7
		}
	})
}

// FuzzSlottedValidate feeds arbitrary bytes as a heap page. Nothing may
// panic, and a page that passes Validate must stay valid under any operation.
func FuzzSlottedValidate(f *testing.F) {
	_, p := newSlotted(f)
	for i := range 12 {
		if _, err := p.Insert(tuple(30+i*40, byte(i))); err != nil {
			f.Fatal(err)
		}
	}
	_ = p.Delete(3)
	_ = p.Delete(7)
	valid := append([]byte(nil), p.buf...)
	f.Add(valid, []byte{opInsert, 0, 77, 0, opUpdate, 5, 0xC8, 0})
	f.Add(make([]byte, PageSize), []byte{})
	f.Add(valid[:100], []byte{1, 2, 3})
	corrupt := append([]byte(nil), valid...)
	corrupt[slotsOffset+1] = 0xFF
	f.Add(corrupt, []byte{opCompact, 0, 0, 0, opInsert, 0, 9, 0})

	f.Fuzz(func(t *testing.T, pageBytes, ops []byte) {
		buf := make([]byte, PageSize)
		copy(buf, pageBytes)
		binary.LittleEndian.PutUint16(buf[20:], uint16(PageTypeHeap)) // reach the heap-specific checks
		sp, err := NewSlottedPage(buf)
		if err != nil {
			return
		}
		valid := sp.Validate() == nil

		poke := func() {
			for s := -1; s < sp.NumSlots()+2; s++ {
				if got, err := sp.Get(s); err == nil && len(got) == 0 {
					t.Fatal("Get returned an empty tuple")
				}
			}
			_ = sp.FreeSpace()
			_ = sp.LiveCount()
			_ = sp.NextLive(0)
		}
		poke()
		for len(ops) >= 4 {
			op := int(ops[0]) % numOps
			slot := int(ops[1])
			size := int(binary.LittleEndian.Uint16(ops[2:4])) % (MaxTupleSize + 30)
			ops = ops[4:]
			data := tuple(size, 5)
			switch op {
			case opInsert:
				_, _ = sp.Insert(data)
			case opUpdate:
				_ = sp.Update(slot, data)
			case opDelete:
				_ = sp.Delete(slot)
			case opCompact:
				_ = sp.Compact()
			}
			if valid {
				if err := sp.Validate(); err != nil {
					t.Fatalf("operation %d broke a valid page: %v", op, err)
				}
			}
			poke()
		}
	})
}
