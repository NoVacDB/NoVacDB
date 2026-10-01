package btree

import (
	"context"
	"os"
	"strconv"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

var bg = context.Background()

func testSeed(t testing.TB) uint64 {
	t.Helper()
	seed := uint64(1)
	if s := os.Getenv("NOVACDB_SEED"); s != "" {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("bad NOVACDB_SEED %q: %v", s, err)
		}
		seed = v
	}
	if tt, ok := t.(*testing.T); ok {
		tt.Logf("seed = %d (override with NOVACDB_SEED)", seed)
	}
	return seed
}

// newPool returns a buffer pool of frames frames over a fresh data file.
func newPool(t testing.TB, frames int) *storage.BufferPool {
	t.Helper()
	bp, _ := newPoolDM(t, frames)
	return bp
}

// newPoolDM is newPool that also returns the data file.
func newPoolDM(t testing.TB, frames int) (*storage.BufferPool, *storage.DiskManager) {
	t.Helper()
	m := vfs.NewMemFS(1)
	dm, err := storage.Create(m, "/data")
	if err != nil {
		t.Fatal(err)
	}
	bp, err := storage.NewBufferPool(dm, storage.Options{Frames: frames})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dm.Close() })
	return bp, dm
}
