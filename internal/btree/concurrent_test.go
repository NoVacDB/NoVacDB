package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
)

// concModel is a model shared by concurrent writers. A writer locks a key's
// stripe around both the tree operation and the model update, so the model
// always agrees with the tree for keys whose stripe is unlocked, while the
// writers still share leaves, splits and merges.
type concModel struct {
	stripes [64]sync.Mutex
	mu      sync.Mutex
	m       model
}

func (c *concModel) stripe(k []byte) *sync.Mutex {
	return &c.stripes[binary.LittleEndian.Uint32(append(bytes.Clone(k), 0, 0, 0, 0))%64]
}

// concKey makes keys from a shared space, with a stable prefix for keys
// that are never touched after setup.
func concKey(stable bool, n int, pad int) []byte {
	k := []byte{'w'}
	if stable {
		k[0] = 's'
	}
	k = binary.BigEndian.AppendUint32(k, uint32(n))
	return append(k, make([]byte, pad)...)
}

func runConcurrent(t *testing.T, tr *Tree, writers, readers, scanners, opsPerWriter, pad int) {
	t.Helper()
	seed := testSeed(t)
	cm := &concModel{m: model{}}
	// Stable keys interleave with the writers' keys: every leaf has some.
	const stableKeys = 500
	stable := map[string]bool{}
	for i := range stableKeys {
		k := concKey(true, i*97, pad)
		if err := tr.Insert(bg, k, []byte("stable")); err != nil {
			t.Fatal(err)
		}
		cm.m[string(k)] = []byte("stable")
		stable[string(k)] = true
	}
	var stop atomic.Bool
	var wg, bg2 sync.WaitGroup
	errc := make(chan error, writers+readers+scanners)
	var reads, scans, scanned atomic.Int64
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seed, uint64(w)))
			for op := range opsPerWriter {
				growing := (op/(opsPerWriter/6))%2 == 0
				k := concKey(false, rng.IntN(stableKeys)*97+1+rng.IntN(6), pad)
				mu := cm.stripe(k)
				mu.Lock()
				cm.mu.Lock()
				_, present := cm.m[string(k)]
				cm.mu.Unlock()
				var err error
				if (growing && rng.IntN(100) < 75) || (!growing && rng.IntN(100) < 8) {
					v := []byte(fmt.Sprintf("w%d-%d", w, op))
					err = tr.Insert(bg, k, v)
					if present && !errors.Is(err, ErrKeyExists) || !present && err != nil {
						err = fmt.Errorf("insert (present=%v): %w", present, err)
					} else {
						err = nil
						if !present {
							cm.mu.Lock()
							cm.m[string(k)] = v
							cm.mu.Unlock()
						}
					}
				} else {
					var found bool
					found, err = tr.Delete(bg, k)
					if err == nil && found != present {
						err = fmt.Errorf("delete found=%v, model %v", found, present)
					}
					if err == nil && found {
						cm.mu.Lock()
						delete(cm.m, string(k))
						cm.mu.Unlock()
					}
				}
				mu.Unlock()
				if err != nil {
					errc <- fmt.Errorf("writer %d op %d: %w", w, op, err)
					return
				}
			}
		}()
	}
	for r := range readers {
		bg2.Add(1)
		go func() {
			defer bg2.Done()
			rng := rand.New(rand.NewPCG(seed, uint64(1000+r)))
			for !stop.Load() {
				k := concKey(true, rng.IntN(stableKeys)*97, pad)
				v, ok, err := tr.Get(bg, k)
				if err != nil || !ok || string(v) != "stable" {
					errc <- fmt.Errorf("reader: Get(stable %x) = %q, %v: %w", k[:5], v, ok, err)
					return
				}
				reads.Add(1)
			}
		}()
	}
	for s := range scanners {
		bg2.Add(1)
		go func() {
			defer bg2.Done()
			rng := rand.New(rand.NewPCG(seed, uint64(2000+s)))
			for !stop.Load() {
				var start, end Bound
				if rng.IntN(2) == 0 {
					start = Incl(concKey(true, rng.IntN(stableKeys/2)*97, pad))
					end = Excl(concKey(true, (stableKeys/2+rng.IntN(stableKeys/2))*97, pad))
				}
				it := tr.Scan(start, end)
				var prev []byte
				seenStable := 0
				for {
					k, _, ok, err := it.Next(bg)
					if err != nil {
						errc <- fmt.Errorf("scanner: %w", err)
						return
					}
					if !ok {
						break
					}
					if prev != nil && bytes.Compare(prev, k) >= 0 {
						errc <- fmt.Errorf("scanner: %x after %x", k[:5], prev[:5])
						return
					}
					prev = k
					if stable[string(k)] {
						seenStable++
					}
					scanned.Add(1)
				}
				want := 0
				for k := range stable {
					if inRange([]byte(k), start, end) {
						want++
					}
				}
				if seenStable != want {
					errc <- fmt.Errorf("scanner: saw %d of %d stable keys", seenStable, want)
					return
				}
				scans.Add(1)
			}
		}()
	}
	wg.Wait()
	stop.Store(true)
	bg2.Wait()
	close(errc)
	for err := range errc {
		t.Fatal(err)
	}
	st := cm.m.verify(t, tr)
	t.Logf("%d keys at the end, %+v; %d reads, %d scans (%d entries); %+v", len(cm.m), st, reads.Load(), scans.Load(), scanned.Load(), tr.opCounts())
	if readers > 0 && reads.Load() == 0 || scanners > 0 && scans.Load() == 0 {
		t.Fatal("readers or scanners never ran")
	}
	c := tr.opCounts()
	if c.splits[0] == 0 || c.merges[0] == 0 {
		t.Fatalf("no concurrent splits or merges: %+v", c)
	}
}

func TestConcurrentWritersReadersScanners(t *testing.T) {
	for _, logged := range []bool{false, true} {
		for _, pad := range []int{60, 700} {
			t.Run(fmt.Sprintf("logged=%v,pad=%d", logged, pad), func(t *testing.T) {
				const writers, readers, scanners = 6, 3, 2
				var opts []Option
				var bpFrames = 16 * (writers + readers + scanners)
				var tr *Tree
				var err error
				if logged {
					e := newLoggedEnv(t, bpFrames)
					opts = append(opts, WithLogger(e.lg))
					tr, err = Create(bg, e.bp, opts...)
				} else {
					tr, err = Create(bg, newPool(t, bpFrames))
				}
				if err != nil {
					t.Fatal(err)
				}
				runConcurrent(t, tr, writers, readers, scanners, opsBudget(t)/20, pad)
			})
		}
	}
}

func TestConcurrentWritersOnly(t *testing.T) {
	// No readers: writers contend with each other alone, on a small pool
	// that keeps evicting.
	tr, err := Create(bg, newPool(t, 8*8))
	if err != nil {
		t.Fatal(err)
	}
	runConcurrent(t, tr, 8, 0, 0, opsBudget(t)/15, 100)
}
