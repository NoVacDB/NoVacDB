package vfs

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

const rwc = ORead | OWrite | OCreate

// newDurableDir returns a MemFS whose /db directory is durable.
func newDurableDir(t *testing.T) *MemFS {
	t.Helper()
	m := NewMemFS(testSeed(t))
	if err := m.MkdirAll("/db"); err != nil {
		t.Fatal(err)
	}
	if err := m.SyncDir("/"); err != nil {
		t.Fatal(err)
	}
	return m
}

// durableFile creates /db/<name> with data, fully synced (file and directory).
func durableFile(t *testing.T, m *MemFS, name, data string) {
	t.Helper()
	f := mustOpen(t, m, "/db/"+name, rwc)
	mustWrite(t, f, data, 0)
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := m.SyncDir("/db"); err != nil {
		t.Fatal(err)
	}
}

func contentsAfterCrash(t *testing.T, m *MemFS, name string) ([]byte, error) {
	t.Helper()
	f, err := m.OpenFile("/db/"+name, ORead)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readAll(t, f), nil
}

func TestCrashDropsUnsyncedWrites(t *testing.T) {
	m := newDurableDir(t)
	durableFile(t, m, "a", "synced")
	f := mustOpen(t, m, "/db/a", ORead|OWrite)
	mustWrite(t, f, "XXXXXXXXXX", 0) // unsynced overwrite and extension
	m.Crash(CrashOptions{})
	got, err := contentsAfterCrash(t, m, "a")
	if err != nil || string(got) != "synced" {
		t.Fatalf("after crash = %q, %v; want synced", got, err)
	}
}

func TestSyncedWriteSurvivesCrash(t *testing.T) {
	m := newDurableDir(t)
	durableFile(t, m, "a", "one")
	f := mustOpen(t, m, "/db/a", ORead|OWrite)
	mustWrite(t, f, "two", 3)
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, f, "lost", 6)
	m.Crash(CrashOptions{})
	got, _ := contentsAfterCrash(t, m, "a")
	if string(got) != "onetwo" {
		t.Fatalf("after crash = %q", got)
	}
}

func TestUnsyncedTruncateLost(t *testing.T) {
	m := newDurableDir(t)
	durableFile(t, m, "a", "abcdef")
	f := mustOpen(t, m, "/db/a", ORead|OWrite)
	if err := f.Truncate(2); err != nil {
		t.Fatal(err)
	}
	m.Crash(CrashOptions{TearLast: true}) // truncate is last op: nothing to tear
	got, _ := contentsAfterCrash(t, m, "a")
	if string(got) != "abcdef" {
		t.Fatalf("after crash = %q", got)
	}
}

func TestNewFileLostWithoutSyncDir(t *testing.T) {
	m := newDurableDir(t)
	f := mustOpen(t, m, "/db/new", rwc)
	mustWrite(t, f, "data", 0)
	if err := f.Sync(); err != nil { // file synced, directory not
		t.Fatal(err)
	}
	m.Crash(CrashOptions{})
	if _, err := contentsAfterCrash(t, m, "new"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("err = %v, want ErrNotExist", err)
	}
}

func TestRenameAndRemoveDurableOnlyAfterSyncDir(t *testing.T) {
	m := newDurableDir(t)
	durableFile(t, m, "a", "A")
	durableFile(t, m, "gone", "G")
	if err := m.Rename("/db/a", "/db/b"); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove("/db/gone"); err != nil {
		t.Fatal(err)
	}
	m.Crash(CrashOptions{})
	if got, err := contentsAfterCrash(t, m, "a"); err != nil || string(got) != "A" {
		t.Fatalf("a after crash = %q, %v", got, err)
	}
	if _, err := contentsAfterCrash(t, m, "b"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("b err = %v", err)
	}
	if got, err := contentsAfterCrash(t, m, "gone"); err != nil || string(got) != "G" {
		t.Fatalf("gone after crash = %q, %v", got, err)
	}

	if err := m.Rename("/db/a", "/db/b"); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove("/db/gone"); err != nil {
		t.Fatal(err)
	}
	if err := m.SyncDir("/db"); err != nil {
		t.Fatal(err)
	}
	m.Crash(CrashOptions{})
	if got, _ := contentsAfterCrash(t, m, "b"); string(got) != "A" {
		t.Fatalf("b after synced rename = %q", got)
	}
	for _, n := range []string{"a", "gone"} {
		if _, err := contentsAfterCrash(t, m, n); !errors.Is(err, ErrNotExist) {
			t.Fatalf("%s err = %v, want ErrNotExist", n, err)
		}
	}
}

func TestDirectoryNeverSyncedLosesItsFiles(t *testing.T) {
	m := NewMemFS(testSeed(t))
	if err := m.MkdirAll("/d"); err != nil {
		t.Fatal(err)
	}
	f := mustOpen(t, m, "/d/f", rwc)
	f.Close()
	if err := m.SyncDir("/d"); err != nil { // /d itself not synced from "/"
		t.Fatal(err)
	}
	m.Crash(CrashOptions{})
	if _, err := m.List("/d"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("List err = %v, want ErrNotExist", err)
	}
}

func TestTornLastWriteKeepsStrictPrefix(t *testing.T) {
	const payload = "0123456789"
	seen := map[int]bool{}
	for seed := range uint64(200) {
		m := NewMemFS(seed)
		_ = m.MkdirAll("/db")
		_ = m.SyncDir("/")
		durableFile(t, m, "a", "base")
		f := mustOpen(t, m, "/db/a", ORead|OWrite)
		mustWrite(t, f, "dropped", 0) // earlier unsynced write: must vanish
		mustWrite(t, f, payload, 4)   // last write: torn
		m.Crash(CrashOptions{TearLast: true})
		got, err := contentsAfterCrash(t, m, "a")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(got, []byte("base")) {
			t.Fatalf("seed %d: lost durable base: %q", seed, got)
		}
		tail := got[4:]
		if len(tail) >= len(payload) || !bytes.HasPrefix([]byte(payload), tail) {
			t.Fatalf("seed %d: tail %q is not a strict prefix of %q", seed, tail, payload)
		}
		seen[len(tail)] = true
	}
	if !seen[0] || len(seen) < 5 {
		t.Fatalf("tear lengths not varied enough: %v", seen)
	}
}

func TestTornWriteIsReproducibleFromSeed(t *testing.T) {
	run := func() int {
		m := NewMemFS(42)
		_ = m.MkdirAll("/db")
		_ = m.SyncDir("/")
		f := mustOpen(t, m, "/db/a", rwc)
		_ = f.Sync()
		_ = m.SyncDir("/db")
		mustWrite(t, f, "abcdefghijklmnop", 0)
		m.Crash(CrashOptions{TearLast: true})
		got, _ := contentsAfterCrash(t, m, "a")
		return len(got)
	}
	if a, b := run(), run(); a != b {
		t.Fatalf("same seed gave %d and %d", a, b)
	}
}

func TestHandlesInvalidAfterCrash(t *testing.T) {
	m := newDurableDir(t)
	f := mustOpen(t, m, "/db/a", rwc)
	m.Crash(CrashOptions{})
	if _, err := f.WriteAt([]byte{1}, 0); !errors.Is(err, ErrClosed) {
		t.Errorf("WriteAt err = %v", err)
	}
	if _, err := f.Size(); !errors.Is(err, ErrClosed) {
		t.Errorf("Size err = %v", err)
	}
	if err := f.Close(); !errors.Is(err, ErrClosed) {
		t.Errorf("Close err = %v", err)
	}
}

func TestInjectedErrors(t *testing.T) {
	tests := []struct {
		name string
		op   Op
		do   func(m *MemFS, f File) error
	}{
		{"OpenFile", OpOpenFile, func(m *MemFS, _ File) error { _, err := m.OpenFile("/db/z", rwc); return err }},
		{"Remove", OpRemove, func(m *MemFS, _ File) error { return m.Remove("/db/a") }},
		{"Rename", OpRename, func(m *MemFS, _ File) error { return m.Rename("/db/a", "/db/b") }},
		{"List", OpList, func(m *MemFS, _ File) error { _, err := m.List("/db"); return err }},
		{"MkdirAll", OpMkdirAll, func(m *MemFS, _ File) error { return m.MkdirAll("/db/x") }},
		{"SyncDir", OpSyncDir, func(m *MemFS, _ File) error { return m.SyncDir("/db") }},
		{"ReadAt", OpReadAt, func(_ *MemFS, f File) error { _, err := f.ReadAt(make([]byte, 1), 0); return err }},
		{"WriteAt", OpWriteAt, func(_ *MemFS, f File) error { _, err := f.WriteAt([]byte{1}, 0); return err }},
		{"Sync", OpSync, func(_ *MemFS, f File) error { return f.Sync() }},
		{"Truncate", OpTruncate, func(_ *MemFS, f File) error { return f.Truncate(0) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newDurableDir(t)
			durableFile(t, m, "a", "x")
			f := mustOpen(t, m, "/db/a", ORead|OWrite)
			m.InjectError(Fault{Op: tc.op})
			if err := tc.do(m, f); !errors.Is(err, ErrInjected) {
				t.Fatalf("first call err = %v, want ErrInjected", err)
			}
			if tc.op.String() != tc.name {
				t.Errorf("Op.String() = %q", tc.op.String())
			}
			// One-shot: the retry gets past the fault (it may fail for
			// ordinary reasons such as EOF, but not ErrInjected).
			if err := tc.do(m, f); errors.Is(err, ErrInjected) {
				t.Fatalf("fault fired twice")
			}
		})
	}
}

func TestFaultTargeting(t *testing.T) {
	m := newDurableDir(t)
	a := mustOpen(t, m, "/db/a", rwc)
	b := mustOpen(t, m, "/db/b", rwc)
	m.InjectError(Fault{Op: OpWriteAt, Name: "/db/b", After: 1})
	if _, err := a.WriteAt([]byte{1}, 0); err != nil {
		t.Fatalf("write to a: %v", err)
	}
	if _, err := b.WriteAt([]byte{1}, 0); err != nil {
		t.Fatalf("first write to b should pass (After=1): %v", err)
	}
	if _, err := b.WriteAt([]byte{2}, 0); !errors.Is(err, ErrInjected) {
		t.Fatalf("second write to b err = %v", err)
	}
	m.InjectError(Fault{Op: OpSync})
	m.ClearFaults()
	if err := a.Sync(); err != nil {
		t.Fatalf("Sync after ClearFaults: %v", err)
	}
}

func TestFailedWriteChangesNothing(t *testing.T) {
	m := newDurableDir(t)
	f := mustOpen(t, m, "/db/a", rwc)
	mustWrite(t, f, "keep", 0)
	m.InjectError(Fault{Op: OpWriteAt})
	if _, err := f.WriteAt([]byte("zzzz"), 0); !errors.Is(err, ErrInjected) {
		t.Fatal(err)
	}
	if got := string(readAll(t, f)); got != "keep" {
		t.Fatalf("contents = %q", got)
	}
}

func TestFailedSyncLeavesDataVolatile(t *testing.T) {
	m := newDurableDir(t)
	durableFile(t, m, "a", "old")
	f := mustOpen(t, m, "/db/a", ORead|OWrite)
	mustWrite(t, f, "new", 0)
	m.InjectError(Fault{Op: OpSync})
	if err := f.Sync(); !errors.Is(err, ErrInjected) {
		t.Fatal(err)
	}
	m.Crash(CrashOptions{})
	if got, _ := contentsAfterCrash(t, m, "a"); string(got) != "old" {
		t.Fatalf("after crash = %q, want old", got)
	}
}

func TestSizeLimit(t *testing.T) {
	m := newDurableDir(t)
	f := mustOpen(t, m, "/db/a", rwc)
	if _, err := f.WriteAt([]byte{1}, MaxMemFileSize); !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("WriteAt err = %v", err)
	}
	if err := f.Truncate(MaxMemFileSize + 1); !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("Truncate err = %v", err)
	}
}

func TestMkdirAllOverFileFails(t *testing.T) {
	m := newDurableDir(t)
	mustOpen(t, m, "/db/f", rwc).Close()
	if err := m.MkdirAll("/db/f/sub"); !errors.Is(err, ErrExist) {
		t.Fatalf("err = %v, want ErrExist", err)
	}
}

func TestUnlinkedFileStaysReadableThroughHandle(t *testing.T) {
	m := newDurableDir(t)
	f := mustOpen(t, m, "/db/a", rwc)
	mustWrite(t, f, "still here", 0)
	if err := m.Remove("/db/a"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	if _, err := f.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if string(buf) != "still here" {
		t.Fatalf("read %q", buf)
	}
}

func TestRenameToSameNameAndIntoDirectoryErrors(t *testing.T) {
	m := newDurableDir(t)
	mustOpen(t, m, "/db/a", rwc).Close()
	if err := m.Rename("/db/a", "/db/a"); err != nil {
		t.Fatalf("same-name rename: %v", err)
	}
	if err := m.Rename("/db/a", "/db"); !errors.Is(err, ErrIsDir) {
		t.Errorf("rename onto dir err = %v", err)
	}
	if err := m.Rename("/db/a", "/nodir/a"); !errors.Is(err, ErrNotExist) {
		t.Errorf("rename into missing dir err = %v", err)
	}
}

func TestEmptyNamesRejected(t *testing.T) {
	m := NewMemFS(1)
	for name, err := range map[string]error{
		"Remove": m.Remove(""), "Rename": m.Rename("", "x"), "MkdirAll": m.MkdirAll(""),
		"SyncDir": m.SyncDir(""),
	} {
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s(\"\") err = %v", name, err)
		}
	}
	if _, err := m.List(""); !errors.Is(err, ErrInvalid) {
		t.Errorf("List err = %v", err)
	}
}

func TestOpStringUnknown(t *testing.T) {
	if got := Op(99).String(); got != "Op(99)" {
		t.Fatalf("got %q", got)
	}
}
