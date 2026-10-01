package vfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	// osFilePerm and osDirPerm are the permissions for created files and
	// directories (before umask).
	osFilePerm fs.FileMode = 0o644
	osDirPerm  fs.FileMode = 0o755
)

// OSFS is the FS backed by the real operating system. It is the only place
// outside tests where package os is used for database files.
type OSFS struct{}

var _ FS = OSFS{}

// mapErr translates os errors to vfs sentinels while keeping the original
// error in the chain.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %w", ErrNotExist, err)
	case errors.Is(err, fs.ErrExist):
		return fmt.Errorf("%w: %w", ErrExist, err)
	case errors.Is(err, os.ErrClosed):
		return fmt.Errorf("%w: %w", ErrClosed, err)
	}
	return err
}

// OpenFile implements FS.
func (OSFS) OpenFile(name string, flag Flag) (File, error) {
	if name == "" {
		return nil, fmt.Errorf("opening file: empty name: %w", ErrInvalid)
	}
	if err := flag.validate(); err != nil {
		return nil, fmt.Errorf("opening %s: %w", name, err)
	}
	osFlag := os.O_RDONLY
	if flag&OWrite != 0 {
		// Always O_RDWR; the wrapper enforces ORead itself so both
		// implementations reject the same operations.
		osFlag = os.O_RDWR
	}
	if flag&OCreate != 0 {
		osFlag |= os.O_CREATE
	}
	if flag&OExcl != 0 {
		osFlag |= os.O_EXCL
	}
	if flag&OTrunc != 0 {
		osFlag |= os.O_TRUNC
	}
	f, err := os.OpenFile(filepath.FromSlash(name), osFlag, osFilePerm)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", name, mapErr(err))
	}
	if info, err := f.Stat(); err == nil && info.IsDir() {
		_ = f.Close()
		return nil, fmt.Errorf("opening %s: %w", name, ErrIsDir)
	}
	return &osFile{f: f, flag: flag}, nil
}

// Remove implements FS.
func (OSFS) Remove(name string) error {
	p := filepath.FromSlash(name)
	info, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("removing %s: %w", name, mapErr(err))
	}
	if info.IsDir() {
		return fmt.Errorf("removing %s: %w", name, ErrIsDir)
	}
	if err := os.Remove(p); err != nil {
		return fmt.Errorf("removing %s: %w", name, mapErr(err))
	}
	return nil
}

// Rename implements FS.
func (OSFS) Rename(oldName, newName string) error {
	if err := os.Rename(filepath.FromSlash(oldName), filepath.FromSlash(newName)); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", oldName, newName, mapErr(err))
	}
	return nil
}

// List implements FS.
func (OSFS) List(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.FromSlash(dir)) // already sorted by name
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", dir, mapErr(err))
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}

// MkdirAll implements FS.
func (OSFS) MkdirAll(dir string) error {
	if err := os.MkdirAll(filepath.FromSlash(dir), osDirPerm); err != nil {
		return fmt.Errorf("creating directory %s: %w", dir, mapErr(err))
	}
	return nil
}

// SyncDir implements FS by fsyncing the directory itself.
func (OSFS) SyncDir(dir string) error {
	d, err := os.Open(filepath.FromSlash(dir))
	if err != nil {
		return fmt.Errorf("syncing directory %s: %w", dir, mapErr(err))
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return fmt.Errorf("syncing directory %s: %w", dir, mapErr(syncErr))
	}
	if closeErr != nil {
		return fmt.Errorf("closing directory %s: %w", dir, mapErr(closeErr))
	}
	return nil
}

// osFile wraps *os.File to enforce open flags and map errors.
type osFile struct {
	f    *os.File
	flag Flag
}

func (o *osFile) ReadAt(p []byte, off int64) (int, error) {
	if o.flag&ORead == 0 {
		return 0, ErrPermission
	}
	if off < 0 {
		return 0, fmt.Errorf("read at %d: %w", off, ErrInvalid)
	}
	n, err := o.f.ReadAt(p, off)
	return n, wrapIO(err)
}

func (o *osFile) WriteAt(p []byte, off int64) (int, error) {
	if o.flag&OWrite == 0 {
		return 0, ErrPermission
	}
	if off < 0 {
		return 0, fmt.Errorf("write at %d: %w", off, ErrInvalid)
	}
	n, err := o.f.WriteAt(p, off)
	return n, wrapIO(err)
}

func (o *osFile) Sync() error { return wrapIO(o.f.Sync()) }

func (o *osFile) Truncate(size int64) error {
	if o.flag&OWrite == 0 {
		return ErrPermission
	}
	if size < 0 {
		return fmt.Errorf("truncate to %d: %w", size, ErrInvalid)
	}
	return wrapIO(o.f.Truncate(size))
}

func (o *osFile) Size() (int64, error) {
	info, err := o.f.Stat()
	if err != nil {
		return 0, wrapIO(err)
	}
	return info.Size(), nil
}

func (o *osFile) Close() error { return wrapIO(o.f.Close()) }

// wrapIO maps errors but leaves io.EOF untouched, since callers compare
// against it directly.
func wrapIO(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	return mapErr(err)
}
