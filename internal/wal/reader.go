package wal

import (
	"errors"
	"fmt"
	"io"

	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

// ErrLSNNotFound means a reader was asked to start at an LSN that is not the
// start of a record in the log: before its first segment, past its end, or
// in the middle of a record.
var ErrLSNNotFound = errors.New("wal: lsn is not a record boundary in the log")

// Reader reads records sequentially from a log directory, across segments.
//
// It applies the same rules as the writer's recovery: every segment except
// the last must consist of complete, valid records (anything else there is
// corruption, ErrCorrupt), while in the last segment the first record that is
// incomplete, fails its checksum or carries the wrong LSN marks the end of the
// log (a torn tail) and Next returns io.EOF there. A reader does not modify
// the log, and it sees the log as it was when it opened each segment.
type Reader struct {
	fsys vfs.FS
	dir  string
	segs []segInfo
	idx  int    // current segment
	data []byte // its content
	off  int    // next record offset within data
	end  LSN    // where reading stopped, valid once Next returned io.EOF
	err  error  // sticky error or io.EOF
}

// NewReader opens a reader positioned at from, which must be the LSN of a
// record, the start of a segment, or the end of the log (in which case Next
// returns io.EOF at once).
func NewReader(fsys vfs.FS, dir string, from LSN) (*Reader, error) {
	segs, _, err := scanSegments(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("reading wal %s: %w", dir, err)
	}
	if len(segs) == 0 {
		return nil, fmt.Errorf("reading wal %s: no segments: %w", dir, ErrLSNNotFound)
	}
	r := &Reader{fsys: fsys, dir: dir, segs: segs}
	if err := r.seek(from); err != nil {
		return nil, fmt.Errorf("reading wal %s from %d: %w", dir, from, err)
	}
	return r, nil
}

// seek positions the reader at from, verifying it is a record boundary by
// walking the segment's records up to it.
func (r *Reader) seek(from LSN) error {
	if from < r.segs[0].start {
		return fmt.Errorf("log starts at %d: %w", r.segs[0].start, ErrLSNNotFound)
	}
	idx := len(r.segs) - 1
	for i, s := range r.segs {
		if from < s.end() {
			idx = i
			break
		}
	}
	// A segment's end is the next segment's start; prefer the later one.
	if s := r.segs[idx]; from == s.end() && idx+1 < len(r.segs) {
		idx++
	}
	if err := r.load(idx); err != nil {
		return err
	}
	seg := r.segs[idx]
	target := int(from - seg.start)
	if target <= SegmentHeaderSize {
		if target != 0 && target != SegmentHeaderSize {
			return fmt.Errorf("inside the header of segment %s: %w", SegmentName(seg.start), ErrLSNNotFound)
		}
		return nil
	}
	for r.off < target {
		_, n, ok := r.decodeHere()
		if !ok {
			// The log (or this segment) ends before from.
			if idx == len(r.segs)-1 {
				return fmt.Errorf("log ends at %d: %w", seg.start+LSN(r.off), ErrLSNNotFound)
			}
			return fmt.Errorf("bad record at %d in segment %s: %w", seg.start+LSN(r.off), SegmentName(seg.start), ErrCorrupt)
		}
		r.off += n
	}
	if r.off != target {
		return fmt.Errorf("%d is inside the record that ends at %d: %w", from, seg.start+LSN(r.off), ErrLSNNotFound)
	}
	return nil
}

// load reads segment idx and positions at its first record.
func (r *Reader) load(idx int) error {
	data, err := readSegment(r.fsys, r.dir, r.segs[idx])
	if err != nil {
		return err
	}
	if int64(len(data)) != r.segs[idx].size {
		// The file changed under us; only possible for a live last segment.
		r.segs[idx].size = int64(len(data))
	}
	r.idx, r.data, r.off = idx, data, SegmentHeaderSize
	return nil
}

// decodeHere decodes the record at the current offset, requiring it to carry
// the LSN of its position.
func (r *Reader) decodeHere() (Record, int, bool) {
	if r.off >= len(r.data) {
		return Record{}, 0, false
	}
	rec, n, err := DecodeRecord(r.data[r.off:])
	if err != nil || rec.LSN != r.segs[r.idx].start+LSN(r.off) {
		return Record{}, 0, false
	}
	return rec, n, true
}

// Next returns the next record. At the end of the log, including a torn tail
// in the last segment, it returns io.EOF; damage anywhere else is ErrCorrupt.
// After an error every further call returns the same error. The record's
// payload stays valid after later calls.
func (r *Reader) Next() (Record, error) {
	if r.err != nil {
		return Record{}, r.err
	}
	for {
		seg := r.segs[r.idx]
		last := r.idx == len(r.segs)-1
		if r.off < len(r.data) {
			if rec, n, ok := r.decodeHere(); ok {
				r.off += n
				return rec, nil
			}
			if !last {
				r.err = fmt.Errorf("bad record at %d in segment %s, which is not the last: %w",
					seg.start+LSN(r.off), SegmentName(seg.start), ErrCorrupt)
				return Record{}, r.err
			}
			r.end, r.err = seg.start+LSN(r.off), io.EOF // torn tail
			return Record{}, r.err
		}
		if last {
			r.end, r.err = seg.start+LSN(r.off), io.EOF
			return Record{}, r.err
		}
		if int64(r.off) != seg.size {
			r.err = fmt.Errorf("segment %s: records end at %d of %d bytes: %w", SegmentName(seg.start), r.off, seg.size, ErrCorrupt)
			return Record{}, r.err
		}
		if err := r.load(r.idx + 1); err != nil {
			r.err = err
			return Record{}, err
		}
	}
}

// End returns the LSN at which reading stopped: the end of the last valid
// record. It is meaningful once Next has returned io.EOF.
func (r *Reader) End() LSN { return r.end }

// FirstLSN returns the LSN of the first record position in the log in dir
// (the start of its oldest segment plus the segment header), which is where
// a full replay begins.
func FirstLSN(fsys vfs.FS, dir string) (LSN, error) {
	segs, _, err := scanSegments(fsys, dir)
	if err != nil {
		return 0, err
	}
	if len(segs) == 0 {
		return 0, fmt.Errorf("wal %s has no segments: %w", dir, ErrLSNNotFound)
	}
	return segs[0].start + SegmentHeaderSize, nil
}
