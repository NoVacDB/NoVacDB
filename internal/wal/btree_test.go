package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/btree"
	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

// btreeKey is a padded key, so trees are several levels deep quickly.
func btreeKey(i int) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(i)), make([]byte, 300)...)
}

func checkTree(t *testing.T, tr *btree.Tree, want map[int][]byte) {
	t.Helper()
	st, err := tr.Check(bg)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if st.Keys != len(want) {
		t.Fatalf("tree has %d keys, want %d", st.Keys, len(want))
	}
	for i, v := range want {
		got, ok, err := tr.Get(bg, btreeKey(i))
		if err != nil || !ok || !bytes.Equal(got, v) {
			t.Fatalf("Get(%d) = %x, %v, %v; want %x", i, got, ok, err, v)
		}
	}
}

func TestEngineBTreeRecoversAfterCrash(t *testing.T) {
	m := newFS(t)
	e := mustEngine(t, m, EngineOptions{Frames: 16})
	tr, err := e.CreateBTree(bg)
	if err != nil {
		t.Fatal(err)
	}
	root := tr.Root()
	rng := rand.New(rand.NewPCG(testSeed(t), 3))
	want := map[int][]byte{}
	for step := range 3000 {
		i := rng.IntN(1500)
		if _, ok := want[i]; ok && rng.IntN(3) == 0 {
			if _, err := tr.Delete(bg, btreeKey(i)); err != nil {
				t.Fatal(err)
			}
			delete(want, i)
		} else if !ok {
			v := payload(rng, rng.IntN(200))
			if err := tr.Insert(bg, btreeKey(i), v); err != nil {
				t.Fatal(err)
			}
			want[i] = v
		}
		if step == 1000 {
			if _, err := e.Checkpoint(bg); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := e.Flush(bg); err != nil {
		t.Fatal(err)
	}
	m.Crash(vfs.CrashOptions{TearLast: true})

	e2 := mustEngine(t, m, EngineOptions{Frames: 16})
	defer func() { _ = e2.Close(bg) }()
	if e2.Recovery().Replayed == 0 {
		t.Fatal("recovery replayed nothing")
	}
	tr2, err := e2.OpenBTree(bg, root)
	if err != nil {
		t.Fatal(err)
	}
	checkTree(t, tr2, want)
}

func TestCheckpointFreesDeferredPagesAfterTheRedoPoint(t *testing.T) {
	m := newFS(t)
	e := mustEngine(t, m, EngineOptions{Frames: 32})
	tr, err := e.CreateBTree(bg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2000 {
		if err := tr.Insert(bg, btreeKey(i), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Checkpoint(bg); err != nil {
		t.Fatal(err)
	}
	freeBefore := e.dm.FreePageCount()
	for i := range 2000 {
		if _, err := tr.Delete(bg, btreeKey(i)); err != nil {
			t.Fatal(err)
		}
	}
	unlinked := e.Logger().PendingFrees()
	if unlinked == 0 {
		t.Fatal("deleting everything unlinked no pages")
	}
	if e.dm.FreePageCount() != freeBefore {
		t.Fatal("pages were freed before a checkpoint")
	}
	// A page unlinked after the next redo point must wait (an unused page,
	// so that freeing it later is harmless).
	waiting, err := e.dm.Allocate(bg)
	if err != nil {
		t.Fatal(err)
	}
	e.Logger().restore(waiting, uint64(e.w.EndLSN())+1)
	if _, err := e.Checkpoint(bg); err != nil {
		t.Fatal(err)
	}
	if got := e.dm.FreePageCount() - freeBefore; got != uint64(unlinked) {
		t.Fatalf("checkpoint freed %d pages, %d were unlinked", got, unlinked)
	}
	if e.Logger().PendingFrees() != 1 {
		t.Fatalf("%d pages pending, want the one unlinked after the redo point", e.Logger().PendingFrees())
	}
	// The tree is intact, the pages are reused, and all of it survives a
	// crash.
	pages := e.dm.PageCount()
	for i := range 2000 {
		if err := tr.Insert(bg, btreeKey(i), []byte{1}); err != nil {
			t.Fatal(err)
		}
	}
	if e.dm.PageCount() != pages {
		t.Fatalf("file grew from %d to %d pages instead of reusing freed ones", pages, e.dm.PageCount())
	}
	if err := e.Flush(bg); err != nil {
		t.Fatal(err)
	}
	m.Crash(vfs.CrashOptions{})
	e2 := mustEngine(t, m, EngineOptions{Frames: 32})
	defer func() { _ = e2.Close(bg) }()
	tr2, err := e2.OpenBTree(bg, tr.Root())
	if err != nil {
		t.Fatal(err)
	}
	want := map[int][]byte{}
	for i := range 2000 {
		want[i] = []byte{1}
	}
	checkTree(t, tr2, want)
}

func TestCheckpointKeepsDeferredPagesItCannotFree(t *testing.T) {
	m := newFS(t)
	e := mustEngine(t, m, EngineOptions{Frames: 8})
	defer func() { _ = e.Close(bg) }()
	tr, err := e.CreateBTree(bg)
	if err != nil {
		t.Fatal(err)
	}
	// A pinned page cannot be freed yet: it stays on the list.
	ref, err := e.bp.FetchPage(bg, tr.Root())
	if err != nil {
		t.Fatal(err)
	}
	e.Logger().restore(tr.Root(), 0)
	if _, err := e.Checkpoint(bg); err != nil {
		t.Fatal(err)
	}
	if e.Logger().PendingFrees() != 1 {
		t.Fatal("a pinned page left the list")
	}
	if err := ref.Unpin(false); err != nil {
		t.Fatal(err)
	}
	e.Logger().takeFreeable(LSN(1 << 62))
	// A page that cannot be freed at all fails the checkpoint, and it and
	// the pages after it stay on the list.
	e.Logger().restore(e.dm.PageCount()+10, 0)
	e.Logger().restore(e.dm.PageCount()+11, 0)
	if _, err := e.Checkpoint(bg); err == nil {
		t.Fatal("freeing a page past the end of the file succeeded")
	}
	if e.Logger().PendingFrees() != 2 {
		t.Fatalf("%d pages pending after a failed free, want 2", e.Logger().PendingFrees())
	}
	e.Logger().takeFreeable(LSN(1 << 62))
}

func TestReplayRejectsBadBTreeRecords(t *testing.T) {
	m := newFS(t)
	e := mustEngine(t, m, EngineOptions{Frames: 8})
	if _, err := e.w.Append(bg, RecordBTree, []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := e.Flush(bg); err != nil {
		t.Fatal(err)
	}
	m.Crash(vfs.CrashOptions{})
	if _, err := OpenEngine(bg, m, dbDir, EngineOptions{Frames: 8}); !errors.Is(err, ErrCorrupt) || !errors.Is(err, btree.ErrBadRecord) {
		t.Fatalf("recovery over a bad b+tree record: %v", err)
	}
}

func TestBTreeCheckpointsDuringConcurrentWorkThenCrash(t *testing.T) {
	// Writers grow and shrink one tree (so pages are split, merged and
	// freed) while checkpoints run back to back; then a torn crash. The
	// recovered tree must hold exactly what the writers left.
	for run := range 4 {
		t.Run(fmt.Sprintf("run=%d", run), func(t *testing.T) {
			m := newFS(t)
			e := mustEngine(t, m, EngineOptions{Frames: 64})
			tr, err := e.CreateBTree(bg)
			if err != nil {
				t.Fatal(err)
			}
			stop := make(chan struct{})
			ckDone := make(chan error, 1)
			go func() {
				n := 0
				for {
					select {
					case <-stop:
						if n == 0 {
							ckDone <- errors.New("no checkpoint completed")
						} else {
							ckDone <- nil
						}
						return
					default:
					}
					if _, err := e.Checkpoint(bg); err != nil {
						ckDone <- err
						return
					}
					n++
				}
			}()
			const writers = 5
			wants := make([]map[int][]byte, writers)
			var wg sync.WaitGroup
			for g := range writers {
				wants[g] = map[int][]byte{}
				wg.Add(1)
				go func() {
					defer wg.Done()
					rng := rand.New(rand.NewPCG(uint64(run), uint64(g)))
					want := wants[g]
					for op := range 1500 {
						i := rng.IntN(400)*writers + g // keys interleave across writers
						grow := (op/250)%2 == 0
						if _, ok := want[i]; ok && (!grow || rng.IntN(4) == 0) {
							if _, err := tr.Delete(bg, btreeKey(i)); err != nil {
								t.Errorf("delete: %v", err)
								return
							}
							delete(want, i)
						} else if !ok && (grow || rng.IntN(4) == 0) {
							v := payload(rng, rng.IntN(300))
							if err := tr.Insert(bg, btreeKey(i), v); err != nil {
								t.Errorf("insert: %v", err)
								return
							}
							want[i] = v
						}
					}
				}()
			}
			wg.Wait()
			close(stop)
			if err := <-ckDone; err != nil {
				t.Fatalf("checkpointer: %v", err)
			}
			if t.Failed() {
				return
			}
			want := map[int][]byte{}
			for _, w := range wants {
				for k, v := range w {
					want[k] = v
				}
			}
			checkTree(t, tr, want)
			if err := e.Flush(bg); err != nil {
				t.Fatal(err)
			}
			m.Crash(vfs.CrashOptions{TearLast: true})
			e2 := mustEngine(t, m, EngineOptions{Frames: 16})
			defer func() { _ = e2.Close(bg) }()
			tr2, err := e2.OpenBTree(bg, tr.Root())
			if err != nil {
				t.Fatal(err)
			}
			checkTree(t, tr2, want)
		})
	}
}
