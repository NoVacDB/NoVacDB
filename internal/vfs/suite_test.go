package vfs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"reflect"
	"strconv"
	"sync"
	"testing"
)

// testSeed returns the seed for deterministic randomness, logged so failures
// can be reproduced; NOVACDB_SEED overrides it.
func testSeed(t *testing.T) uint64 {
	t.Helper()
	seed := uint64(1)
	if s := os.Getenv("NOVACDB_SEED"); s != "" {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("bad NOVACDB_SEED %q: %v", s, err)
		}
		seed = v
	}
	t.Logf("seed = %d (override with NOVACDB_SEED)", seed)
	return seed
}

// factory returns a fresh FS and an existing directory to work in.
type factory func(t *testing.T) (FS, string)

func osFactory(t *testing.T) (FS, string) { return OSFS{}, t.TempDir() }

func memFactory(t *testing.T) (FS, string) {
	fsys := NewMemFS(testSeed(t))
	if err := fsys.MkdirAll("/db"); err != nil {
		t.Fatal(err)
	}
	return fsys, "/db"
}

func TestBehaviour(t *testing.T) {
	for name, f := range map[string]factory{"OSFS": osFactory, "MemFS": memFactory} {
		t.Run(name, func(t *testing.T) { runSuite(t, f) })
	}
}

func mustOpen(t *testing.T, fsys FS, name string, flag Flag) File {
	t.Helper()
	f, err := fsys.OpenFile(name, flag)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", name, err)
	}
	return f
}

func mustWrite(t *testing.T, f File, data string, off int64) {
	t.Helper()
	if n, err := f.WriteAt([]byte(data), off); err != nil || n != len(data) {
		t.Fatalf("WriteAt = %d, %v", n, err)
	}
}

func readAll(t *testing.T, f File) []byte {
	t.Helper()
	size, err := f.Size()
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, size)
	if size == 0 {
		return buf
	}
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	return buf
}

func runSuite(t *testing.T, newFS factory) {
	const rw = ORead | OWrite
	tests := []struct {
		name string
		run  func(t *testing.T, fsys FS, dir string)
	}{
		{"open missing file", func(t *testing.T, fsys FS, dir string) {
			_, err := fsys.OpenFile(dir+"/nope", ORead)
			if !errors.Is(err, ErrNotExist) {
				t.Fatalf("err = %v, want ErrNotExist", err)
			}
		}},
		{"create in missing directory", func(t *testing.T, fsys FS, dir string) {
			_, err := fsys.OpenFile(dir+"/no/such/f", rw|OCreate)
			if !errors.Is(err, ErrNotExist) {
				t.Fatalf("err = %v, want ErrNotExist", err)
			}
		}},
		{"create then exclusive create fails", func(t *testing.T, fsys FS, dir string) {
			mustOpen(t, fsys, dir+"/a", rw|OCreate|OExcl).Close()
			_, err := fsys.OpenFile(dir+"/a", rw|OCreate|OExcl)
			if !errors.Is(err, ErrExist) {
				t.Fatalf("err = %v, want ErrExist", err)
			}
		}},
		{"invalid arguments", func(t *testing.T, fsys FS, dir string) {
			bad := []struct {
				name string
				flag Flag
			}{
				{dir + "/a", 0}, {dir + "/a", ORead | OCreate}, {dir + "/a", rw | OExcl},
				{dir + "/a", ORead | OTrunc}, {"", rw},
			}
			for _, b := range bad {
				if _, err := fsys.OpenFile(b.name, b.flag); !errors.Is(err, ErrInvalid) {
					t.Errorf("OpenFile(%q, %d) err = %v, want ErrInvalid", b.name, b.flag, err)
				}
			}
		}},
		{"open directory as file", func(t *testing.T, fsys FS, dir string) {
			if _, err := fsys.OpenFile(dir, ORead); !errors.Is(err, ErrIsDir) {
				t.Fatalf("err = %v, want ErrIsDir", err)
			}
		}},
		{"write read round trip at offsets", func(t *testing.T, fsys FS, dir string) {
			f := mustOpen(t, fsys, dir+"/a", rw|OCreate)
			defer f.Close()
			mustWrite(t, f, "hello", 0)
			mustWrite(t, f, "WORLD", 6)
			if got := string(readAll(t, f)); got != "hello\x00WORLD" {
				t.Fatalf("contents = %q", got)
			}
			buf := make([]byte, 5)
			if n, err := f.ReadAt(buf, 6); err != nil || n != 5 || string(buf) != "WORLD" {
				t.Fatalf("ReadAt = %d, %v, %q", n, err, buf)
			}
		}},
		{"read past end", func(t *testing.T, fsys FS, dir string) {
			f := mustOpen(t, fsys, dir+"/a", rw|OCreate)
			defer f.Close()
			mustWrite(t, f, "abc", 0)
			buf := make([]byte, 5)
			n, err := f.ReadAt(buf, 1)
			if n != 2 || !errors.Is(err, io.EOF) || string(buf[:n]) != "bc" {
				t.Fatalf("short read = %d, %v", n, err)
			}
			if n, err := f.ReadAt(buf, 3); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("read at exact EOF = %d, %v", n, err)
			}
			if n, err := f.ReadAt(buf, 100); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("read beyond EOF = %d, %v", n, err)
			}
			if n, err := f.ReadAt(nil, 0); n != 0 || err != nil {
				t.Fatalf("empty read = %d, %v", n, err)
			}
		}},
		{"empty write is a no-op", func(t *testing.T, fsys FS, dir string) {
			f := mustOpen(t, fsys, dir+"/a", rw|OCreate)
			defer f.Close()
			if n, err := f.WriteAt(nil, 50); n != 0 || err != nil {
				t.Fatalf("WriteAt(nil) = %d, %v", n, err)
			}
			if size, _ := f.Size(); size != 0 {
				t.Fatalf("size = %d, want 0", size)
			}
		}},
		{"negative offsets and sizes", func(t *testing.T, fsys FS, dir string) {
			f := mustOpen(t, fsys, dir+"/a", rw|OCreate)
			defer f.Close()
			if _, err := f.ReadAt(make([]byte, 1), -1); !errors.Is(err, ErrInvalid) {
				t.Errorf("ReadAt(-1) err = %v", err)
			}
			if _, err := f.WriteAt([]byte{1}, -1); !errors.Is(err, ErrInvalid) {
				t.Errorf("WriteAt(-1) err = %v", err)
			}
			if err := f.Truncate(-1); !errors.Is(err, ErrInvalid) {
				t.Errorf("Truncate(-1) err = %v", err)
			}
		}},
		{"truncate shrink and grow", func(t *testing.T, fsys FS, dir string) {
			f := mustOpen(t, fsys, dir+"/a", rw|OCreate)
			defer f.Close()
			mustWrite(t, f, "abcdef", 0)
			if err := f.Truncate(3); err != nil {
				t.Fatal(err)
			}
			if got := string(readAll(t, f)); got != "abc" {
				t.Fatalf("after shrink = %q", got)
			}
			if err := f.Truncate(5); err != nil {
				t.Fatal(err)
			}
			if got := readAll(t, f); !bytes.Equal(got, []byte("abc\x00\x00")) {
				t.Fatalf("after grow = %q", got)
			}
			if err := f.Truncate(0); err != nil {
				t.Fatal(err)
			}
			if size, _ := f.Size(); size != 0 {
				t.Fatalf("size = %d", size)
			}
		}},
		{"OTrunc empties existing file", func(t *testing.T, fsys FS, dir string) {
			f := mustOpen(t, fsys, dir+"/a", rw|OCreate)
			mustWrite(t, f, "data", 0)
			f.Close()
			f = mustOpen(t, fsys, dir+"/a", rw|OTrunc)
			defer f.Close()
			if size, _ := f.Size(); size != 0 {
				t.Fatalf("size = %d, want 0", size)
			}
		}},
		{"open flags are enforced", func(t *testing.T, fsys FS, dir string) {
			mustOpen(t, fsys, dir+"/a", rw|OCreate).Close()
			ro := mustOpen(t, fsys, dir+"/a", ORead)
			defer ro.Close()
			if _, err := ro.WriteAt([]byte{1}, 0); !errors.Is(err, ErrPermission) {
				t.Errorf("write on read-only err = %v", err)
			}
			if err := ro.Truncate(0); !errors.Is(err, ErrPermission) {
				t.Errorf("truncate on read-only err = %v", err)
			}
			wo := mustOpen(t, fsys, dir+"/a", OWrite)
			defer wo.Close()
			if _, err := wo.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrPermission) {
				t.Errorf("read on write-only err = %v", err)
			}
		}},
		{"closed handle", func(t *testing.T, fsys FS, dir string) {
			f := mustOpen(t, fsys, dir+"/a", rw|OCreate)
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteAt([]byte{1}, 0); !errors.Is(err, ErrClosed) {
				t.Errorf("WriteAt err = %v", err)
			}
			if _, err := f.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrClosed) {
				t.Errorf("ReadAt err = %v", err)
			}
			if err := f.Sync(); !errors.Is(err, ErrClosed) {
				t.Errorf("Sync err = %v", err)
			}
			if _, err := f.Size(); !errors.Is(err, ErrClosed) {
				t.Errorf("Size err = %v", err)
			}
			if err := f.Close(); !errors.Is(err, ErrClosed) {
				t.Errorf("second Close err = %v", err)
			}
		}},
		{"two handles share contents", func(t *testing.T, fsys FS, dir string) {
			a := mustOpen(t, fsys, dir+"/a", rw|OCreate)
			defer a.Close()
			b := mustOpen(t, fsys, dir+"/a", ORead)
			defer b.Close()
			mustWrite(t, a, "shared", 0)
			if got := string(readAll(t, b)); got != "shared" {
				t.Fatalf("second handle sees %q", got)
			}
		}},
		{"rename replaces target", func(t *testing.T, fsys FS, dir string) {
			f := mustOpen(t, fsys, dir+"/a", rw|OCreate)
			mustWrite(t, f, "new", 0)
			f.Close()
			g := mustOpen(t, fsys, dir+"/b", rw|OCreate)
			mustWrite(t, g, "old-old", 0)
			g.Close()
			if err := fsys.Rename(dir+"/a", dir+"/b"); err != nil {
				t.Fatal(err)
			}
			if _, err := fsys.OpenFile(dir+"/a", ORead); !errors.Is(err, ErrNotExist) {
				t.Errorf("old name err = %v", err)
			}
			h := mustOpen(t, fsys, dir+"/b", ORead)
			defer h.Close()
			if got := string(readAll(t, h)); got != "new" {
				t.Errorf("contents = %q", got)
			}
			if err := fsys.Rename(dir+"/missing", dir+"/c"); !errors.Is(err, ErrNotExist) {
				t.Errorf("rename missing err = %v", err)
			}
		}},
		{"remove", func(t *testing.T, fsys FS, dir string) {
			mustOpen(t, fsys, dir+"/a", rw|OCreate).Close()
			if err := fsys.Remove(dir + "/a"); err != nil {
				t.Fatal(err)
			}
			if err := fsys.Remove(dir + "/a"); !errors.Is(err, ErrNotExist) {
				t.Errorf("second remove err = %v", err)
			}
			if err := fsys.Remove(dir); !errors.Is(err, ErrIsDir) {
				t.Errorf("remove dir err = %v", err)
			}
		}},
		{"list is sorted and includes subdirectories", func(t *testing.T, fsys FS, dir string) {
			if got, err := fsys.List(dir); err != nil || len(got) != 0 {
				t.Fatalf("empty list = %v, %v", got, err)
			}
			for _, n := range []string{"c", "a", "b"} {
				mustOpen(t, fsys, dir+"/"+n, rw|OCreate).Close()
			}
			if err := fsys.MkdirAll(dir + "/sub/deeper"); err != nil {
				t.Fatal(err)
			}
			got, err := fsys.List(dir)
			if err != nil || !reflect.DeepEqual(got, []string{"a", "b", "c", "sub"}) {
				t.Fatalf("List = %v, %v", got, err)
			}
			if _, err := fsys.List(dir + "/missing"); !errors.Is(err, ErrNotExist) {
				t.Errorf("list missing err = %v", err)
			}
		}},
		{"mkdirall is idempotent", func(t *testing.T, fsys FS, dir string) {
			for range 2 {
				if err := fsys.MkdirAll(dir + "/x/y"); err != nil {
					t.Fatal(err)
				}
			}
			mustOpen(t, fsys, dir+"/x/y/f", rw|OCreate).Close()
		}},
		{"sync and syncdir succeed", func(t *testing.T, fsys FS, dir string) {
			f := mustOpen(t, fsys, dir+"/a", rw|OCreate)
			defer f.Close()
			mustWrite(t, f, "x", 0)
			if err := f.Sync(); err != nil {
				t.Fatal(err)
			}
			if err := fsys.SyncDir(dir); err != nil {
				t.Fatal(err)
			}
			if err := fsys.SyncDir(dir + "/missing"); !errors.Is(err, ErrNotExist) {
				t.Errorf("SyncDir missing err = %v", err)
			}
		}},
		{"concurrent disjoint writes and reads", func(t *testing.T, fsys FS, dir string) {
			const workers, chunk = 8, 512
			f := mustOpen(t, fsys, dir+"/a", rw|OCreate)
			defer f.Close()
			var wg sync.WaitGroup
			for w := range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					data := bytes.Repeat([]byte{byte('a' + w)}, chunk)
					for range 50 {
						if _, err := f.WriteAt(data, int64(w*chunk)); err != nil {
							t.Error(err)
							return
						}
						got := make([]byte, chunk)
						if _, err := f.ReadAt(got, int64(w*chunk)); err != nil && !errors.Is(err, io.EOF) {
							t.Error(err)
							return
						}
					}
				}()
			}
			wg.Wait()
			all := readAll(t, f)
			for w := range workers {
				want := bytes.Repeat([]byte{byte('a' + w)}, chunk)
				if !bytes.Equal(all[w*chunk:(w+1)*chunk], want) {
					t.Errorf("region %d corrupted", w)
				}
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fsys, dir := newFS(t)
			tc.run(t, fsys, dir)
		})
	}
}
