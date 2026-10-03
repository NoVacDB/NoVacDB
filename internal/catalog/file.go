package catalog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"path"

	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

// FileName is the catalog file in a database directory: the first pages of
// the system tables (docs/design/10-executor.md section 2.5).
const FileName = "catalog"

// Catalog file layout, little-endian:
//
//	offset  size  field
//	0       8     magic "NVDBCATL"
//	8       4     version (1)
//	12      24    first page of novac_tables, novac_columns, novac_indexes
//	36      4     CRC-32C of bytes 0-35
const (
	fileMagic   = "NVDBCATL"
	fileVersion = 1
	fileSize    = 40
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// readFile reads the catalog file. A missing file is ok=false; anything
// else that is not a valid catalog file is an error.
func readFile(fsys vfs.FS, dir string) (firsts [numSys]uint64, ok bool, err error) {
	f, err := fsys.OpenFile(path.Join(dir, FileName), vfs.ORead)
	if errors.Is(err, vfs.ErrNotExist) {
		return firsts, false, nil
	}
	if err != nil {
		return firsts, false, fmt.Errorf("opening the catalog file: %w", err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, fileSize+1)
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return firsts, false, fmt.Errorf("reading the catalog file: %w", err)
	}
	if n != fileSize || string(buf[:8]) != fileMagic || binary.LittleEndian.Uint32(buf[8:]) != fileVersion ||
		binary.LittleEndian.Uint32(buf[36:]) != crc32.Checksum(buf[:36], castagnoli) {
		return firsts, false, corruptCatalog("the catalog file is damaged")
	}
	for i := range firsts {
		firsts[i] = binary.LittleEndian.Uint64(buf[12+8*i:])
	}
	return firsts, true, nil
}

// writeFile writes the catalog file atomically: a temporary file, fsynced
// and renamed into place, then the directory fsynced.
func writeFile(fsys vfs.FS, dir string, firsts [numSys]uint64) error {
	buf := []byte(fileMagic)
	buf = binary.LittleEndian.AppendUint32(buf, fileVersion)
	for _, p := range firsts {
		buf = binary.LittleEndian.AppendUint64(buf, p)
	}
	buf = binary.LittleEndian.AppendUint32(buf, crc32.Checksum(buf, castagnoli))
	tmp := path.Join(dir, FileName+".tmp")
	f, err := fsys.OpenFile(tmp, vfs.ORead|vfs.OWrite|vfs.OCreate|vfs.OTrunc)
	if err != nil {
		return fmt.Errorf("writing the catalog file: %w", err)
	}
	_, err = f.WriteAt(buf, 0)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = fsys.Rename(tmp, path.Join(dir, FileName))
	}
	if err == nil {
		err = fsys.SyncDir(dir)
	}
	if err != nil {
		return fmt.Errorf("writing the catalog file: %w", err)
	}
	return nil
}
