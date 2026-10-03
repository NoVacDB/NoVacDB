package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"path"

	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

// ControlFileName is the name of the control file in a database directory.
const ControlFileName = "control"

// ControlSize is the size of the control file.
const ControlSize = 32

var controlMagic = [8]byte{'N', 'O', 'V', 'A', 'C', 'T', 'L', 0}

// Control file layout (32 bytes, little-endian):
//
//	offset  size  field
//	0       4     CRC-32C of bytes 4 .. 32
//	4       8     Magic "NOVACTL\0"
//	12      4     FormatVersion
//	16      8     CheckpointLSN (the last completed checkpoint's record)
//	24      8     RedoLSN (where recovery starts)
const (
	ctlOffMagic   = 4
	ctlOffVersion = 12
	ctlOffCkpt    = 16
	ctlOffRedo    = 24
)

// Control is the content of the control file: where the last completed
// checkpoint is, and where recovery must start replaying.
type Control struct {
	CheckpointLSN LSN
	RedoLSN       LSN
}

// AppendControl appends the encoding of c to dst.
func AppendControl(dst []byte, c Control) []byte {
	b := len(dst)
	dst = append(dst, make([]byte, ControlSize)...)
	h := dst[b:]
	copy(h[ctlOffMagic:], controlMagic[:])
	binary.LittleEndian.PutUint32(h[ctlOffVersion:], FormatVersion)
	binary.LittleEndian.PutUint64(h[ctlOffCkpt:], uint64(c.CheckpointLSN))
	binary.LittleEndian.PutUint64(h[ctlOffRedo:], uint64(c.RedoLSN))
	binary.LittleEndian.PutUint32(h, crc32.Checksum(h[ctlOffMagic:], castagnoli))
	return dst
}

// DecodeControl decodes a control file. Anything but exactly one valid
// control record is ErrCorrupt (or ErrUnsupportedVersion).
func DecodeControl(buf []byte) (Control, error) {
	if len(buf) != ControlSize {
		return Control{}, fmt.Errorf("control file is %d bytes, want %d: %w", len(buf), ControlSize, ErrCorrupt)
	}
	if got, want := binary.LittleEndian.Uint32(buf), crc32.Checksum(buf[ctlOffMagic:], castagnoli); got != want {
		return Control{}, fmt.Errorf("control file checksum: %w", ErrCorrupt)
	}
	if [8]byte(buf[ctlOffMagic:ctlOffMagic+8]) != controlMagic {
		return Control{}, fmt.Errorf("control file magic: %w", ErrCorrupt)
	}
	if v := binary.LittleEndian.Uint32(buf[ctlOffVersion:]); v != FormatVersion {
		return Control{}, fmt.Errorf("control file version %d: %w", v, ErrUnsupportedVersion)
	}
	c := Control{
		CheckpointLSN: LSN(binary.LittleEndian.Uint64(buf[ctlOffCkpt:])),
		RedoLSN:       LSN(binary.LittleEndian.Uint64(buf[ctlOffRedo:])),
	}
	if c.RedoLSN > c.CheckpointLSN {
		// The redo point is set before the checkpoint record is written.
		return Control{}, fmt.Errorf("redo %d after checkpoint %d: %w", c.RedoLSN, c.CheckpointLSN, ErrCorrupt)
	}
	return c, nil
}

// WriteControl atomically replaces the control file in dir: it writes a
// temporary file, fsyncs it, renames it over the control file and fsyncs the
// directory. After a crash at any point the control file holds either the old
// content or the new, never a mix.
func WriteControl(fsys vfs.FS, dir string, c Control) error {
	tmp := path.Join(dir, ControlFileName+".tmp")
	f, err := fsys.OpenFile(tmp, vfs.ORead|vfs.OWrite|vfs.OCreate|vfs.OTrunc)
	if err != nil {
		return fmt.Errorf("writing control file: %w", err)
	}
	_, err = f.WriteAt(AppendControl(nil, c), 0)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = fsys.Rename(tmp, path.Join(dir, ControlFileName))
	}
	if err == nil {
		err = fsys.SyncDir(dir)
	}
	if err != nil {
		return fmt.Errorf("writing control file: %w", err)
	}
	return nil
}

// ReadControl reads the control file in dir. ok is false if there is none.
func ReadControl(fsys vfs.FS, dir string) (c Control, ok bool, err error) {
	f, err := fsys.OpenFile(path.Join(dir, ControlFileName), vfs.ORead)
	if errors.Is(err, vfs.ErrNotExist) {
		return Control{}, false, nil
	}
	if err != nil {
		return Control{}, false, fmt.Errorf("reading control file: %w", err)
	}
	defer func() { _ = f.Close() }()
	size, err := f.Size()
	if err != nil {
		return Control{}, false, fmt.Errorf("reading control file: %w", err)
	}
	if size != ControlSize {
		return Control{}, false, fmt.Errorf("control file is %d bytes: %w", size, ErrCorrupt)
	}
	buf := make([]byte, ControlSize)
	if _, err := f.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
		return Control{}, false, fmt.Errorf("reading control file: %w", err)
	}
	c, err = DecodeControl(buf)
	if err != nil {
		return Control{}, false, err
	}
	return c, true, nil
}
