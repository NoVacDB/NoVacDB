package btree

import (
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"testing"
)

// benchKeys returns n distinct 16-byte keys in random order (an int64
// field and padding, like a two-column index entry).
func benchKeys(n int) [][]byte {
	rng := rand.New(rand.NewPCG(1, 2))
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = AppendInt64(nil, int64(i))
		keys[i] = binary.BigEndian.AppendUint64(keys[i], uint64(i)*2654435761)
	}
	rng.Shuffle(n, func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	return keys
}

const benchSize = 100_000

// benchTree builds a tree of benchSize entries in a pool big enough to keep
// it in memory, so the benchmarks measure the tree, not the disk.
func benchTree(b *testing.B, logged bool) (*Tree, [][]byte) {
	b.Helper()
	var tr *Tree
	var err error
	if logged {
		e := newLoggedEnv(b, 2048)
		tr, err = Create(bg, e.bp, WithLogger(e.lg))
	} else {
		tr, err = Create(bg, newPool(b, 2048))
	}
	if err != nil {
		b.Fatal(err)
	}
	keys := benchKeys(benchSize)
	val := make([]byte, 8) // a RID
	for _, k := range keys {
		if err := tr.Insert(bg, k, val); err != nil {
			b.Fatal(err)
		}
	}
	return tr, keys
}

func benchmarkInsert(b *testing.B, logged, sequential bool) {
	var tr *Tree
	var err error
	if logged {
		e := newLoggedEnv(b, 4096)
		tr, err = Create(bg, e.bp, WithLogger(e.lg))
	} else {
		tr, err = Create(bg, newPool(b, 4096))
	}
	if err != nil {
		b.Fatal(err)
	}
	val := make([]byte, 8)
	rng := rand.New(rand.NewPCG(3, 4))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var k []byte
		if sequential {
			k = AppendInt64(nil, int64(i))
		} else {
			k = AppendInt64(nil, int64(rng.Uint64()))
		}
		if err := tr.Insert(bg, k, val); err != nil && !errors.Is(err, ErrKeyExists) {
			b.Fatal(err)
		}
	}
}

func BenchmarkInsertRandom(b *testing.B)           { benchmarkInsert(b, false, false) }
func BenchmarkInsertSequential(b *testing.B)       { benchmarkInsert(b, false, true) }
func BenchmarkInsertRandomLogged(b *testing.B)     { benchmarkInsert(b, true, false) }
func BenchmarkInsertSequentialLogged(b *testing.B) { benchmarkInsert(b, true, true) }

func BenchmarkGet(b *testing.B) {
	tr, keys := benchTree(b, false)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok, err := tr.Get(bg, keys[i%len(keys)]); !ok || err != nil {
			b.Fatal(ok, err)
		}
	}
}

func BenchmarkGetParallel(b *testing.B) {
	tr, keys := benchTree(b, false)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := rand.IntN(len(keys))
		for pb.Next() {
			if _, ok, err := tr.Get(bg, keys[i%len(keys)]); !ok || err != nil {
				b.Error(ok, err)
				return
			}
			i++
		}
	})
}

// BenchmarkScan reports the cost per entry of a full scan.
func BenchmarkScan(b *testing.B) {
	tr, _ := benchTree(b, false)
	b.ReportAllocs()
	b.ResetTimer()
	n := 0
	for n < b.N {
		it := tr.Scan(Bound{}, Bound{})
		for n < b.N {
			_, _, ok, err := it.Next(bg)
			if err != nil {
				b.Fatal(err)
			}
			if !ok {
				break
			}
			n++
		}
	}
}

func BenchmarkDeleteInsert(b *testing.B) {
	// Steady state: delete a key and put it back, so the tree stays the
	// same size while leaves underflow and refill.
	tr, keys := benchTree(b, true)
	val := make([]byte, 8)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		k := keys[i%len(keys)]
		if _, err := tr.Delete(bg, k); err != nil {
			b.Fatal(err)
		}
		if err := tr.Insert(bg, k, val); err != nil {
			b.Fatal(err)
		}
	}
}
