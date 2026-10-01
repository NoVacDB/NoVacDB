package vfs

import (
	"fmt"
	"io"
	"math/rand/v2"
	"path"
	"sort"
	"sync"
)

// MaxMemFileSize caps a MemFS file so a stray huge offset in a test cannot
// exhaust memory.
const MaxMemFileSize = 1 << 30

// Op names a MemFS operation that a Fault can target.
type Op int

// Operations that can have faults injected.
const (
	OpOpenFile Op = iota + 1
	OpRemove
	OpRename
	OpList
	OpMkdirAll
	OpSyncDir
	OpReadAt
	OpWriteAt
	OpSync
	OpTruncate
)

var opNames = map[Op]string{
	OpOpenFile: "OpenFile", OpRemove: "Remove", OpRename: "Rename",
	OpList: "List", OpMkdirAll: "MkdirAll", OpSyncDir: "SyncDir",
	OpReadAt: "ReadAt", OpWriteAt: "WriteAt", OpSync: "Sync", OpTruncate: "Truncate",
}

// String returns the operation name.
func (o Op) String() string {
	if s, ok := opNames[o]; ok {
		return s
	}
	return fmt.Sprintf("Op(%d)", int(o))
}

// Fault describes one injected I/O failure.
type Fault struct {
	// Op is the operation to fail.
	Op Op
	// Name restricts the fault to one path (cleaned). Empty matches any path.
	// For Rename, the old name is matched.
	Name string
	// After is how many matching calls succeed before the fault fires.
	After int
}

// CrashOptions control MemFS.Crash.
type CrashOptions struct {
	// TearLast keeps only a random prefix (possibly empty) of the last
	// unsynced write of each file, as a power cut during a write might.
	TearLast bool
}

// pendingOp is a write or truncate not yet made durable by Sync.
type pendingOp struct {
	truncate bool
	size     int64  // for truncate
	off      int64  // for write
	data     []byte // for write
}

// memNode is one file's contents. Several names/handles may reference it.
type memNode struct {
	volatile []byte // what reads see
	durable  []byte // what survives a crash
	pending  []pendingOp
}

// MemFS is an in-memory FS that models durability. See the package comment
// and docs/design/01-vfs.md.
//
// Two views exist for both file contents and the directory tree:
// volatile (current) and durable (as of the last Sync/SyncDir). Crash throws
// away the volatile view.
type MemFS struct {
	mu       sync.Mutex
	rng      *rand.Rand
	epoch    int // bumped by Crash; older handles become ErrClosed
	files    map[string]*memNode
	durFiles map[string]*memNode
	dirs     map[string]bool
	durDirs  map[string]bool
	faults   []*faultState
}

type faultState struct {
	Fault
	seen int
}

var _ FS = (*MemFS)(nil)

// NewMemFS returns an empty MemFS. seed makes torn-write lengths reproducible.
// The root directories "/" and "." always exist and are durable.
func NewMemFS(seed uint64) *MemFS {
	return &MemFS{
		rng:      rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		files:    map[string]*memNode{},
		durFiles: map[string]*memNode{},
		dirs:     map[string]bool{"/": true, ".": true},
		durDirs:  map[string]bool{"/": true, ".": true},
	}
}

// clean normalises a name; MemFS treats names as opaque slash paths.
func clean(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty name: %w", ErrInvalid)
	}
	return path.Clean(name), nil
}

// InjectError arranges for a matching operation to fail once with
// ErrInjected, after f.After matching calls have succeeded.
func (m *MemFS) InjectError(f Fault) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f.Name != "" {
		f.Name = path.Clean(f.Name)
	}
	m.faults = append(m.faults, &faultState{Fault: f})
}

// ClearFaults removes all pending faults.
func (m *MemFS) ClearFaults() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.faults = nil
}

// checkFault must be called with m.mu held.
func (m *MemFS) checkFault(op Op, name string) error {
	for i, f := range m.faults {
		if f.Op != op || (f.Name != "" && f.Name != name) {
			continue
		}
		if f.seen < f.After {
			f.seen++
			continue
		}
		m.faults = append(m.faults[:i], m.faults[i+1:]...)
		return fmt.Errorf("%s %s: %w", op, name, ErrInjected)
	}
	return nil
}

// Crash simulates a power cut: every unsynced write, truncate, create,
// rename and remove is lost, and all open handles become invalid.
func (m *MemFS) Crash(opts CrashOptions) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.epoch++

	// Directory tree reverts first so orphaned entries can be dropped.
	m.dirs = copySet(m.durDirs)
	m.files = map[string]*memNode{}
	for name, n := range m.durFiles {
		if !m.dirs[path.Dir(name)] {
			delete(m.durFiles, name) // parent directory never became durable
			continue
		}
		m.files[name] = n
	}
	seen := map[*memNode]bool{}
	for _, n := range m.files {
		if seen[n] {
			continue
		}
		seen[n] = true
		if opts.TearLast && len(n.pending) > 0 {
			if last := n.pending[len(n.pending)-1]; !last.truncate && len(last.data) > 0 {
				keep := m.rng.IntN(len(last.data)) // 0 .. len-1: never the full write
				n.durable = applyWrite(n.durable, last.off, last.data[:keep])
			}
		}
		n.pending = nil
		n.volatile = append([]byte(nil), n.durable...)
	}
}

func copySet(s map[string]bool) map[string]bool {
	c := make(map[string]bool, len(s))
	for k, v := range s {
		c[k] = v
	}
	return c
}

// applyWrite writes data at off, zero-filling any gap. An empty write is a
// no-op and does not extend the file.
func applyWrite(buf []byte, off int64, data []byte) []byte {
	if len(data) == 0 {
		return buf
	}
	end := off + int64(len(data))
	if end > int64(len(buf)) {
		buf = append(buf, make([]byte, end-int64(len(buf)))...)
	}
	copy(buf[off:], data)
	return buf
}

func resize(buf []byte, size int64) []byte {
	if size <= int64(len(buf)) {
		return buf[:size]
	}
	return append(buf, make([]byte, size-int64(len(buf)))...)
}

// OpenFile implements FS.
func (m *MemFS) OpenFile(name string, flag Flag) (File, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name, err := clean(name)
	if err != nil {
		return nil, fmt.Errorf("opening file: %w", err)
	}
	if err := flag.validate(); err != nil {
		return nil, fmt.Errorf("opening %s: %w", name, err)
	}
	if err := m.checkFault(OpOpenFile, name); err != nil {
		return nil, err
	}
	if m.dirs[name] {
		return nil, fmt.Errorf("opening %s: %w", name, ErrIsDir)
	}
	n, exists := m.files[name]
	switch {
	case exists && flag&OCreate != 0 && flag&OExcl != 0:
		return nil, fmt.Errorf("opening %s: %w", name, ErrExist)
	case !exists && flag&OCreate == 0:
		return nil, fmt.Errorf("opening %s: %w", name, ErrNotExist)
	case !exists:
		if !m.dirs[path.Dir(name)] {
			return nil, fmt.Errorf("opening %s: parent directory: %w", name, ErrNotExist)
		}
		n = &memNode{}
		m.files[name] = n
	}
	if flag&OTrunc != 0 {
		n.volatile = n.volatile[:0]
		n.pending = append(n.pending, pendingOp{truncate: true})
	}
	return &memFile{fs: m, name: name, node: n, flag: flag, epoch: m.epoch}, nil
}

// Remove implements FS.
func (m *MemFS) Remove(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	name, err := clean(name)
	if err != nil {
		return fmt.Errorf("removing file: %w", err)
	}
	if err := m.checkFault(OpRemove, name); err != nil {
		return err
	}
	if m.dirs[name] {
		return fmt.Errorf("removing %s: %w", name, ErrIsDir)
	}
	if _, ok := m.files[name]; !ok {
		return fmt.Errorf("removing %s: %w", name, ErrNotExist)
	}
	delete(m.files, name)
	return nil
}

// Rename implements FS.
func (m *MemFS) Rename(oldName, newName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	oldName, err := clean(oldName)
	if err != nil {
		return fmt.Errorf("renaming: %w", err)
	}
	newName, err = clean(newName)
	if err != nil {
		return fmt.Errorf("renaming: %w", err)
	}
	if err := m.checkFault(OpRename, oldName); err != nil {
		return err
	}
	n, ok := m.files[oldName]
	if !ok {
		return fmt.Errorf("renaming %s: %w", oldName, ErrNotExist)
	}
	if m.dirs[newName] {
		return fmt.Errorf("renaming to %s: %w", newName, ErrIsDir)
	}
	if !m.dirs[path.Dir(newName)] {
		return fmt.Errorf("renaming to %s: parent directory: %w", newName, ErrNotExist)
	}
	if oldName != newName {
		m.files[newName] = n
		delete(m.files, oldName)
	}
	return nil
}

// List implements FS.
func (m *MemFS) List(dir string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dir, err := clean(dir)
	if err != nil {
		return nil, fmt.Errorf("listing: %w", err)
	}
	if err := m.checkFault(OpList, dir); err != nil {
		return nil, err
	}
	if !m.dirs[dir] {
		return nil, fmt.Errorf("listing %s: %w", dir, ErrNotExist)
	}
	names := []string{}
	for name := range m.files {
		if path.Dir(name) == dir {
			names = append(names, path.Base(name))
		}
	}
	for d := range m.dirs {
		if d != dir && path.Dir(d) == dir {
			names = append(names, path.Base(d))
		}
	}
	sort.Strings(names)
	return names, nil
}

// MkdirAll implements FS.
func (m *MemFS) MkdirAll(dir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	dir, err := clean(dir)
	if err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}
	if err := m.checkFault(OpMkdirAll, dir); err != nil {
		return err
	}
	for d := dir; d != "/" && d != "."; d = path.Dir(d) {
		if _, isFile := m.files[d]; isFile {
			return fmt.Errorf("creating directory %s: %w", d, ErrExist)
		}
		m.dirs[d] = true
	}
	return nil
}

// SyncDir implements FS: it makes the current entries of dir (files and
// immediate subdirectories, including removals and renames) durable.
func (m *MemFS) SyncDir(dir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	dir, err := clean(dir)
	if err != nil {
		return fmt.Errorf("syncing directory: %w", err)
	}
	if err := m.checkFault(OpSyncDir, dir); err != nil {
		return err
	}
	if !m.dirs[dir] {
		return fmt.Errorf("syncing directory %s: %w", dir, ErrNotExist)
	}
	for name := range m.durFiles {
		if _, ok := m.files[name]; !ok && path.Dir(name) == dir {
			delete(m.durFiles, name)
		}
	}
	for name, n := range m.files {
		if path.Dir(name) == dir {
			m.durFiles[name] = n
		}
	}
	for d := range m.durDirs {
		if d != "/" && d != "." && path.Dir(d) == dir && !m.dirs[d] {
			delete(m.durDirs, d)
		}
	}
	for d := range m.dirs {
		if d != "/" && d != "." && path.Dir(d) == dir {
			m.durDirs[d] = true
		}
	}
	return nil
}

// memFile is a handle on a memNode.
type memFile struct {
	fs     *MemFS
	name   string
	node   *memNode
	flag   Flag
	epoch  int
	closed bool
}

// begin locks the file system and checks the handle is usable. The caller
// must unlock fs.mu.
func (f *memFile) begin(op Op, name string) error {
	f.fs.mu.Lock()
	if f.closed || f.epoch != f.fs.epoch {
		f.fs.mu.Unlock()
		return ErrClosed
	}
	if err := f.fs.checkFault(op, name); err != nil {
		f.fs.mu.Unlock()
		return err
	}
	return nil
}

// ReadAt implements File.
func (f *memFile) ReadAt(p []byte, off int64) (int, error) {
	if f.flag&ORead == 0 {
		return 0, ErrPermission
	}
	if off < 0 {
		return 0, fmt.Errorf("read at %d: %w", off, ErrInvalid)
	}
	if err := f.begin(OpReadAt, f.name); err != nil {
		return 0, err
	}
	defer f.fs.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if off >= int64(len(f.node.volatile)) {
		return 0, io.EOF
	}
	n := copy(p, f.node.volatile[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// WriteAt implements File.
func (f *memFile) WriteAt(p []byte, off int64) (int, error) {
	if f.flag&OWrite == 0 {
		return 0, ErrPermission
	}
	if off < 0 {
		return 0, fmt.Errorf("write at %d: %w", off, ErrInvalid)
	}
	if err := f.begin(OpWriteAt, f.name); err != nil {
		return 0, err
	}
	defer f.fs.mu.Unlock()
	if off+int64(len(p)) > MaxMemFileSize {
		return 0, fmt.Errorf("write at %d: %w", off, ErrFileTooLarge)
	}
	if len(p) == 0 {
		return 0, nil
	}
	f.node.volatile = applyWrite(f.node.volatile, off, p)
	f.node.pending = append(f.node.pending, pendingOp{off: off, data: append([]byte(nil), p...)})
	return len(p), nil
}

// Sync implements File.
func (f *memFile) Sync() error {
	if err := f.begin(OpSync, f.name); err != nil {
		return err
	}
	defer f.fs.mu.Unlock()
	f.node.durable = append(f.node.durable[:0], f.node.volatile...)
	f.node.pending = nil
	return nil
}

// Truncate implements File.
func (f *memFile) Truncate(size int64) error {
	if f.flag&OWrite == 0 {
		return ErrPermission
	}
	if size < 0 {
		return fmt.Errorf("truncate to %d: %w", size, ErrInvalid)
	}
	if err := f.begin(OpTruncate, f.name); err != nil {
		return err
	}
	defer f.fs.mu.Unlock()
	if size > MaxMemFileSize {
		return fmt.Errorf("truncate to %d: %w", size, ErrFileTooLarge)
	}
	f.node.volatile = resize(f.node.volatile, size)
	f.node.pending = append(f.node.pending, pendingOp{truncate: true, size: size})
	return nil
}

// Size implements File.
func (f *memFile) Size() (int64, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.closed || f.epoch != f.fs.epoch {
		return 0, ErrClosed
	}
	return int64(len(f.node.volatile)), nil
}

// Close implements File.
func (f *memFile) Close() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.closed || f.epoch != f.fs.epoch {
		return ErrClosed
	}
	f.closed = true
	return nil
}
