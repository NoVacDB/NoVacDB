package vfs

import (
	"errors"
	"fmt"
)

// Sentinel errors returned (wrapped) by every FS and File implementation, so
// callers and tests can match behaviour with errors.Is regardless of which
// implementation is in use.
var (
	// ErrNotExist means a file or directory does not exist.
	ErrNotExist = errors.New("vfs: file does not exist")
	// ErrExist means a file already exists (OExcl) or a path is in the way.
	ErrExist = errors.New("vfs: file already exists")
	// ErrClosed means the handle was closed, or invalidated by MemFS.Crash.
	ErrClosed = errors.New("vfs: file closed")
	// ErrIsDir means a file operation was attempted on a directory.
	ErrIsDir = errors.New("vfs: is a directory")
	// ErrInvalid means a bad argument: empty name, negative offset or size,
	// or an inconsistent flag combination.
	ErrInvalid = errors.New("vfs: invalid argument")
	// ErrPermission means the handle was not opened for this operation.
	ErrPermission = errors.New("vfs: operation not permitted by open flags")
	// ErrFileTooLarge means a MemFS file would exceed MaxMemFileSize.
	ErrFileTooLarge = errors.New("vfs: file too large")
	// ErrInjected is returned by MemFS for faults set with InjectError.
	ErrInjected = errors.New("vfs: injected I/O error")
)

// Flag selects how OpenFile opens a file. Flags are combined with |.
type Flag int

const (
	// ORead allows ReadAt.
	ORead Flag = 1 << iota
	// OWrite allows WriteAt and Truncate.
	OWrite
	// OCreate creates the file if it does not exist. Requires OWrite.
	OCreate
	// OExcl with OCreate fails with ErrExist if the file already exists.
	OExcl
	// OTrunc empties the file on open. Requires OWrite.
	OTrunc
)

// validate rejects flag combinations that have no sensible meaning.
func (f Flag) validate() error {
	switch {
	case f&(ORead|OWrite) == 0:
		return fmt.Errorf("neither ORead nor OWrite set: %w", ErrInvalid)
	case f&(OCreate|OTrunc) != 0 && f&OWrite == 0:
		return fmt.Errorf("OCreate/OTrunc require OWrite: %w", ErrInvalid)
	case f&OExcl != 0 && f&OCreate == 0:
		return fmt.Errorf("OExcl requires OCreate: %w", ErrInvalid)
	}
	return nil
}

// FS is a file system. Names are slash-separated paths.
type FS interface {
	// OpenFile opens (and with OCreate, possibly creates) a file. The parent
	// directory must already exist.
	OpenFile(name string, flag Flag) (File, error)
	// Remove deletes a file. It does not remove directories.
	Remove(name string) error
	// Rename moves a file, replacing any existing file at newName.
	Rename(oldName, newName string) error
	// List returns the sorted base names of the entries in dir.
	List(dir string) ([]string, error)
	// MkdirAll creates dir and any missing parents. It is not an error if
	// dir already exists.
	MkdirAll(dir string) error
	// SyncDir makes creates, renames and removes inside dir durable. Syncing
	// a file does not do this: after creating, renaming or deleting a file,
	// its parent directory must be synced too.
	SyncDir(dir string) error
}

// File is an open file. Access is by offset only, so there is no shared
// cursor and handles are safe for concurrent use.
type File interface {
	// ReadAt has io.ReaderAt semantics: a short read returns io.EOF.
	ReadAt(p []byte, off int64) (int, error)
	// WriteAt has io.WriterAt semantics. Writing past the end extends the
	// file; the gap reads as zero bytes.
	WriteAt(p []byte, off int64) (int, error)
	// Sync makes the file's contents durable. If it fails, the contents
	// must be treated as not durable.
	Sync() error
	// Truncate changes the file size. Like a write, it is not durable until
	// Sync.
	Truncate(size int64) error
	// Size returns the current size in bytes.
	Size() (int64, error)
	// Close releases the handle. It does not sync.
	Close() error
}
