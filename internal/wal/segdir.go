package wal

import (
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

// segInfo describes one segment file of a log directory.
type segInfo struct {
	start LSN
	size  int64
}

// end returns the LSN just past the segment's last byte.
func (s segInfo) end() LSN { return s.start + LSN(s.size) }

// errResidue marks a final segment that is only the debris of a crash during
// its creation.
var errResidue = errors.New("wal: residue")

// scanSegments lists the segment files of dir in log order and validates
// them: every header must be intact and name its own start LSN, and each
// segment must start exactly where the previous one ends. A final segment too
// small to hold a record and lacking a valid header is debris from a crash
// while it was being created; it is returned as residue (not in segs) so the
// caller can ignore or delete it. Nothing is modified.
func scanSegments(fsys vfs.FS, dir string) (segs []segInfo, residue *LSN, err error) {
	names, err := fsys.List(dir)
	if err != nil {
		return nil, nil, err
	}
	var starts []LSN
	for _, n := range names {
		if s, ok := ParseSegmentName(n); ok {
			starts = append(starts, s)
		}
	}
	slices.Sort(starts)
	for i, s := range starts {
		size, err := checkSegment(fsys, dir, s, i == len(starts)-1)
		if errors.Is(err, errResidue) {
			r := s
			residue = &r
			break
		}
		if err != nil {
			return nil, nil, err
		}
		segs = append(segs, segInfo{start: s, size: size})
	}
	for i := 1; i < len(segs); i++ {
		if want := segs[i-1].end(); segs[i].start != want {
			return nil, nil, fmt.Errorf("segment %s starts at %d but the previous one ends at %d: %w",
				SegmentName(segs[i].start), segs[i].start, want, ErrCorrupt)
		}
	}
	return segs, residue, nil
}

// checkSegment validates the header of the segment starting at s and returns
// its file size.
func checkSegment(fsys vfs.FS, dir string, s LSN, last bool) (int64, error) {
	f, err := fsys.OpenFile(segPath(dir, s), vfs.ORead)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	size, err := f.Size()
	if err != nil {
		return 0, err
	}
	hdr := make([]byte, SegmentHeaderSize)
	n, err := f.ReadAt(hdr, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	start, derr := DecodeSegmentHeader(hdr[:n])
	if derr != nil {
		if last && size <= SegmentHeaderSize && errors.Is(derr, ErrCorrupt) {
			return 0, errResidue // cannot contain records, safe to discard
		}
		return 0, fmt.Errorf("segment %s: %w", SegmentName(s), derr)
	}
	if start != s {
		return 0, fmt.Errorf("segment %s: header says it starts at %d: %w", SegmentName(s), start, ErrCorrupt)
	}
	return size, nil
}

// readSegment returns the whole content of a segment file.
func readSegment(fsys vfs.FS, dir string, seg segInfo) ([]byte, error) {
	f, err := fsys.OpenFile(segPath(dir, seg.start), vfs.ORead)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data := make([]byte, seg.size)
	n, err := f.ReadAt(data, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return data[:n], nil
}

// scanRecords walks the records of one segment's content from offset
// SegmentHeaderSize and returns the offset just past the last record that is
// complete, passes its checksum and carries the LSN of its position.
func scanRecords(data []byte, start LSN) int {
	off := SegmentHeaderSize
	for off < len(data) {
		rec, size, err := DecodeRecord(data[off:])
		if err != nil || rec.LSN != start+LSN(off) {
			break
		}
		off += size
	}
	return min(off, len(data))
}
