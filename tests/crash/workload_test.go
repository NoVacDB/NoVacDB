package crash

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
	"github.com/vikrant-choudhary06/NoVacDB/internal/wal"
)

var bg = context.Background()

// numHeaps is how many heaps every workload uses.
const numHeaps = 2

type opKind int

const (
	opInsert opKind = iota
	opUpdate
	opDelete
	opFlush
	opCheckpoint
)

// op is one step of a workload. Rows are identified by a key stored in their
// first 8 bytes, so the oracle never depends on RIDs.
type op struct {
	kind    opKind
	heap    int
	key     uint64
	version uint64
	size    int
}

// tupleHeaderSize is key + version.
const tupleHeaderSize = 16

// tupleFor builds a row: key, version, then filler derived from both, so
// any mix of two versions is detectable.
func tupleFor(key, version uint64, size int) []byte {
	b := make([]byte, max(size, tupleHeaderSize))
	binary.LittleEndian.PutUint64(b, key)
	binary.LittleEndian.PutUint64(b[8:], version)
	for i := tupleHeaderSize; i < len(b); i++ {
		b[i] = byte(key*31 + version*7 + uint64(i))
	}
	return b
}

// model is the oracle: the rows of every heap, keyed by row key.
type model struct {
	heaps   [numHeaps]map[uint64][]byte
	nextKey uint64
	version uint64
}

func newModel() *model {
	m := &model{nextKey: 1}
	for i := range m.heaps {
		m.heaps[i] = map[uint64][]byte{}
	}
	return m
}

func (m *model) clone() *model {
	c := &model{nextKey: m.nextKey, version: m.version}
	for i, h := range m.heaps {
		c.heaps[i] = maps.Clone(h)
	}
	return c
}

func (m *model) apply(o op) {
	switch o.kind {
	case opInsert:
		m.heaps[o.heap][o.key] = tupleFor(o.key, o.version, o.size)
		m.nextKey = o.key + 1
		m.version = o.version
	case opUpdate:
		m.heaps[o.heap][o.key] = tupleFor(o.key, o.version, o.size)
		m.version = o.version
	case opDelete:
		delete(m.heaps[o.heap], o.key)
	}
}

func (m *model) equal(rows [numHeaps]map[uint64][]byte) bool {
	for i := range m.heaps {
		if len(m.heaps[i]) != len(rows[i]) {
			return false
		}
		for k, v := range m.heaps[i] {
			if !bytes.Equal(rows[i][k], v) {
				return false
			}
		}
	}
	return true
}

func (m *model) rowCount() int {
	n := 0
	for _, h := range m.heaps {
		n += len(h)
	}
	return n
}

// workload generates operations. Each choice depends only on its random
// stream and the model, so the same seed always yields the same sequence of
// operations; a second process can regenerate what a killed one did.
type workload struct {
	rng *rand.Rand
}

func newWorkload(seed uint64) *workload {
	return &workload{rng: rand.New(rand.NewPCG(seed, seed^0x5eed))}
}

func (w *workload) rowSize() int {
	switch r := w.rng.IntN(100); {
	case r < 50:
		return tupleHeaderSize + w.rng.IntN(100)
	case r < 85:
		return tupleHeaderSize + w.rng.IntN(1500)
	case r < 98:
		return tupleHeaderSize + w.rng.IntN(4000)
	default:
		return storage.MaxTupleSize - w.rng.IntN(30)
	}
}

func (w *workload) next(m *model) op {
	h := w.rng.IntN(numHeaps)
	keys := make([]uint64, 0, len(m.heaps[h]))
	for k := range m.heaps[h] {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	switch r := w.rng.IntN(100); {
	case r < 40 || len(keys) == 0:
		return op{kind: opInsert, heap: h, key: m.nextKey, version: m.version + 1, size: w.rowSize()}
	case r < 65:
		return op{kind: opUpdate, heap: h, key: keys[w.rng.IntN(len(keys))], version: m.version + 1, size: w.rowSize()}
	case r < 80:
		return op{kind: opDelete, heap: h, key: keys[w.rng.IntN(len(keys))]}
	case r < 97:
		return op{kind: opFlush}
	default:
		return op{kind: opCheckpoint}
	}
}

// db wraps an engine and its heaps, remembering where each row lives.
type db struct {
	e     *wal.Engine
	heaps [numHeaps]*storage.Heap
	rids  [numHeaps]map[uint64]storage.RID
}

// createHeaps makes the workload's heaps and acknowledges them.
func (d *db) createHeaps() (firsts [numHeaps]uint64, err error) {
	for i := range d.heaps {
		h, err := d.e.CreateHeap(bg)
		if err != nil {
			return firsts, err
		}
		d.heaps[i], d.rids[i], firsts[i] = h, map[uint64]storage.RID{}, h.FirstPage()
	}
	return firsts, d.e.Flush(bg)
}

// openHeaps opens the heaps and reads every row back, so the RID of each key
// is known again.
func (d *db) openHeaps(firsts [numHeaps]uint64) (rows [numHeaps]map[uint64][]byte, err error) {
	for i, first := range firsts {
		h, err := d.e.OpenHeap(bg, first)
		if err != nil {
			return rows, fmt.Errorf("opening heap %d: %w", i, err)
		}
		d.heaps[i], d.rids[i], rows[i] = h, map[uint64]storage.RID{}, map[uint64][]byte{}
		s := h.Scan()
		for {
			rid, data, ok, err := s.Next(bg)
			if err != nil {
				return rows, fmt.Errorf("scanning heap %d: %w", i, err)
			}
			if !ok {
				break
			}
			if len(data) < tupleHeaderSize {
				return rows, fmt.Errorf("heap %d: row %s is %d bytes", i, rid, len(data))
			}
			key := binary.LittleEndian.Uint64(data)
			if _, dup := rows[i][key]; dup {
				return rows, fmt.Errorf("heap %d: key %d appears twice", i, key)
			}
			rows[i][key], d.rids[i][key] = data, rid
		}
	}
	return rows, nil
}

// do performs one operation on the database.
func (d *db) do(o op) error {
	switch o.kind {
	case opInsert:
		rid, err := d.heaps[o.heap].Insert(bg, tupleFor(o.key, o.version, o.size))
		if err == nil {
			d.rids[o.heap][o.key] = rid
		}
		return err
	case opUpdate:
		rid, err := d.heaps[o.heap].Update(bg, d.rids[o.heap][o.key], tupleFor(o.key, o.version, o.size))
		if err == nil {
			d.rids[o.heap][o.key] = rid
		}
		return err
	case opDelete:
		err := d.heaps[o.heap].Delete(bg, d.rids[o.heap][o.key])
		if err == nil {
			delete(d.rids[o.heap], o.key)
		}
		return err
	case opFlush:
		return d.e.Flush(bg)
	case opCheckpoint:
		_, err := d.e.Checkpoint(bg)
		return err
	}
	return fmt.Errorf("unknown op %d", o.kind)
}

// envInt reads a positive integer from the environment, or returns def.
func envInt(t testing.TB, name string, def int) int {
	t.Helper()
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		t.Fatalf("bad %s=%q", name, s)
	}
	return v
}

func baseSeed(t testing.TB) uint64 {
	t.Helper()
	seed := uint64(1)
	if s := os.Getenv("NOVACDB_SEED"); s != "" {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("bad NOVACDB_SEED %q: %v", s, err)
		}
		seed = v
	}
	t.Logf("base seed = %d (override with NOVACDB_SEED)", seed)
	return seed
}
