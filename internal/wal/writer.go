package wal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"sync"

	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
)

// Defaults for Options.
const (
	DefaultSegmentSize     = 16 << 20
	DefaultMaxPendingBytes = 1 << 20
)

// Errors returned by the Writer.
var (
	// ErrInvalidOptions means Options has a negative field.
	ErrInvalidOptions = errors.New("wal: invalid options")
	// ErrInvalidType means a record type of zero was given.
	ErrInvalidType = errors.New("wal: record type must be non-zero")
	// ErrPayloadTooLarge means a payload exceeds MaxPayload.
	ErrPayloadTooLarge = errors.New("wal: payload too large")
	// ErrClosed means the writer was closed.
	ErrClosed = errors.New("wal: writer closed")
	// ErrFailed means an earlier I/O error left memory and disk possibly out
	// of step. Reopen the log to continue.
	ErrFailed = errors.New("wal: writer failed, reopen required")
)

// Options configures a Writer. Zero values select the defaults.
type Options struct {
	// SegmentSize is the soft limit on a segment file's size. A segment
	// always holds at least one record, so a larger record gets its own.
	SegmentSize int64
	// MaxPendingBytes bounds the memory used by records not yet flushed:
	// Append flushes by itself once this much is buffered.
	MaxPendingBytes int
}

// chunk is a run of bytes destined for one segment. The first chunk of a new
// segment begins with that segment's header.
type chunk struct {
	start LSN
	buf   []byte
}

// Writer appends records to the log. It is safe for concurrent use. See
// docs/design/06-wal.md.
type Writer struct {
	fsys       vfs.FS
	dir        string
	segSize    uint64
	maxPending int

	// mu guards the logical state below. Lock order: flushMu, then mu.
	mu           sync.Mutex
	curStart     uint64 // start LSN of the segment records are being added to
	curLen       uint64 // its logical length, pending bytes included
	pending      []chunk
	pendingBytes int
	durableEnd   uint64 // every record starting below this is durable
	closed       bool
	failed       bool

	// flushMu serialises flushes and owns the open segment file.
	flushMu sync.Mutex
	f       vfs.File
	fStart  uint64 // start LSN of f
	fLen    int64  // bytes written to f (the logical file length)
}

func segPath(dir string, start LSN) string { return path.Join(dir, SegmentName(start)) }

// Open opens the log in dir, creating it if needed. After a crash it finds
// the end of the log: a half-written tail is cut off, and the writer
// continues from the last complete record.
func Open(fsys vfs.FS, dir string, opts Options) (*Writer, error) {
	if opts.SegmentSize < 0 || opts.MaxPendingBytes < 0 {
		return nil, fmt.Errorf("opening wal: %w", ErrInvalidOptions)
	}
	if opts.SegmentSize == 0 {
		opts.SegmentSize = DefaultSegmentSize
	}
	if opts.MaxPendingBytes == 0 {
		opts.MaxPendingBytes = DefaultMaxPendingBytes
	}
	w := &Writer{
		fsys:       fsys,
		dir:        dir,
		segSize:    uint64(opts.SegmentSize),
		maxPending: opts.MaxPendingBytes,
	}
	if err := w.open(); err != nil {
		if w.f != nil {
			_ = w.f.Close()
		}
		return nil, fmt.Errorf("opening wal %s: %w", dir, err)
	}
	return w, nil
}

type segInfo struct {
	start LSN
	size  int64
}

func (w *Writer) open() error {
	if err := w.fsys.MkdirAll(w.dir); err != nil {
		return err
	}
	if err := w.fsys.SyncDir(path.Dir(w.dir)); err != nil {
		return err
	}
	names, err := w.fsys.List(w.dir)
	if err != nil {
		return err
	}
	var starts []LSN
	for _, n := range names {
		if s, ok := ParseSegmentName(n); ok {
			starts = append(starts, s)
		}
	}
	slices.Sort(starts)
	if len(starts) == 0 {
		return w.startFresh(0)
	}

	// Validate every segment's header and the chain between segments.
	infos := make([]segInfo, 0, len(starts))
	for i, s := range starts {
		last := i == len(starts)-1
		size, err := w.checkSegment(s, last)
		if errors.Is(err, errResidue) {
			// A crash while creating the final segment; it holds no records.
			if rerr := w.fsys.Remove(segPath(w.dir, s)); rerr != nil {
				return rerr
			}
			if serr := w.fsys.SyncDir(w.dir); serr != nil {
				return serr
			}
			if len(infos) == 0 {
				if s != 0 {
					return fmt.Errorf("only segment %s is unusable: %w", SegmentName(s), ErrCorrupt)
				}
				return w.startFresh(0)
			}
			break
		}
		if err != nil {
			return err
		}
		infos = append(infos, segInfo{start: s, size: size})
	}
	for i := 1; i < len(infos); i++ {
		prev := infos[i-1]
		if want := uint64(prev.start) + uint64(prev.size); uint64(infos[i].start) != want {
			return fmt.Errorf("segment %s starts at %d but the previous one ends at %d: %w",
				SegmentName(infos[i].start), infos[i].start, want, ErrCorrupt)
		}
	}
	if err := w.recoverLast(infos[len(infos)-1]); err != nil {
		return err
	}
	// After a killed process (as opposed to a power cut) the file system can
	// show files whose directory entries were never fsynced. Open reports
	// everything it found as durable, so make the entries durable too.
	return w.fsys.SyncDir(w.dir)
}

// errResidue marks a final segment that is only the debris of a crash during
// its creation.
var errResidue = errors.New("wal: residue")

// checkSegment validates the header of the segment starting at s and returns
// its file size.
func (w *Writer) checkSegment(s LSN, last bool) (int64, error) {
	f, err := w.fsys.OpenFile(segPath(w.dir, s), vfs.ORead)
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

// startFresh creates the very first segment of an empty log.
func (w *Writer) startFresh(start LSN) error {
	f, err := w.fsys.OpenFile(segPath(w.dir, start), vfs.ORead|vfs.OWrite|vfs.OCreate|vfs.OExcl)
	if err != nil {
		return err
	}
	w.f = f
	hdr := AppendSegmentHeader(nil, start)
	if _, err := f.WriteAt(hdr, 0); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := w.fsys.SyncDir(w.dir); err != nil {
		return err
	}
	w.fStart, w.fLen = uint64(start), SegmentHeaderSize
	w.curStart, w.curLen = uint64(start), SegmentHeaderSize
	w.durableEnd = uint64(start) + SegmentHeaderSize
	return nil
}

// recoverLast scans the final segment for the end of the valid records,
// truncates anything after it, and positions the writer there.
func (w *Writer) recoverLast(seg segInfo) error {
	f, err := w.fsys.OpenFile(segPath(w.dir, seg.start), vfs.ORead|vfs.OWrite)
	if err != nil {
		return err
	}
	w.f = f
	data := make([]byte, seg.size)
	n, err := f.ReadAt(data, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	data = data[:n]

	off := SegmentHeaderSize
	for off < len(data) {
		rec, size, derr := DecodeRecord(data[off:])
		if derr != nil || uint64(rec.LSN) != uint64(seg.start)+uint64(off) {
			break // torn tail, or bytes that do not belong at this position
		}
		off += size
	}
	if off < len(data) {
		if err := f.Truncate(int64(off)); err != nil {
			return err
		}
	}
	// Make what survived durable before building on it.
	if err := f.Sync(); err != nil {
		return err
	}
	w.fStart, w.fLen = uint64(seg.start), int64(off)
	w.curStart, w.curLen = uint64(seg.start), uint64(off)
	w.durableEnd = uint64(seg.start) + uint64(off)
	return nil
}

// Append adds a record to the log and returns its LSN. The record is only in
// memory until a Flush returns. It performs I/O only when the buffer has
// grown past MaxPendingBytes; if that automatic flush fails, Append returns
// both the record's LSN and the error (the record is then buffered and not
// known to be durable).
func (w *Writer) Append(ctx context.Context, t RecordType, payload []byte) (LSN, error) {
	if t == 0 {
		return 0, ErrInvalidType
	}
	if len(payload) > MaxPayload {
		return 0, fmt.Errorf("%d bytes (max %d): %w", len(payload), MaxPayload, ErrPayloadTooLarge)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return 0, ErrClosed
	}
	if w.failed {
		w.mu.Unlock()
		return 0, ErrFailed
	}
	recLen := uint64(RecordSize(len(payload)))
	if w.curLen+recLen > w.segSize && w.curLen > SegmentHeaderSize {
		// Does not fit: begin the next segment right where this one ends.
		w.curStart += w.curLen
		w.curLen = SegmentHeaderSize
		w.pending = append(w.pending, chunk{start: LSN(w.curStart), buf: AppendSegmentHeader(nil, LSN(w.curStart))})
		w.pendingBytes += SegmentHeaderSize
	}
	if len(w.pending) == 0 || uint64(w.pending[len(w.pending)-1].start) != w.curStart {
		w.pending = append(w.pending, chunk{start: LSN(w.curStart)})
	}
	lsn := LSN(w.curStart + w.curLen)
	c := &w.pending[len(w.pending)-1]
	c.buf = AppendRecord(c.buf, lsn, t, payload)
	w.curLen += recLen
	w.pendingBytes += int(recLen)
	over := w.pendingBytes >= w.maxPending
	w.mu.Unlock()

	if over {
		if err := w.flush(ctx, 0); err != nil {
			// The record is buffered (and the writer may now be failed);
			// returning its LSN lets the caller account for it.
			return lsn, err
		}
	}
	return lsn, nil
}

// EndLSN returns the LSN the next record will get.
func (w *Writer) EndLSN() LSN {
	w.mu.Lock()
	defer w.mu.Unlock()
	return LSN(w.curStart + w.curLen)
}

// DurableEnd returns the offset up to which the log is durable: every record
// that starts below it has been fsynced.
func (w *Writer) DurableEnd() LSN {
	w.mu.Lock()
	defer w.mu.Unlock()
	return LSN(w.durableEnd)
}

// FlushedLSN returns DurableEnd()-1, the greatest LSN whose record (if one
// starts there) is durable. A page whose LSN is at most this may be written
// to disk: it is the function for the buffer pool's WAL-rule hook, where a
// page's LSN is the start LSN of the last record that changed it.
func (w *Writer) FlushedLSN() LSN {
	return w.DurableEnd() - 1 // DurableEnd is never 0: a segment header is always durable
}

// Flush writes all buffered records and fsyncs. When it returns nil every
// record appended before the call survives a crash.
func (w *Writer) Flush(ctx context.Context) error {
	if err := w.checkOpen(); err != nil {
		return err
	}
	return w.flush(ctx, 0)
}

// FlushTo makes the record at lsn, and everything before it, durable. It does
// nothing if that is already so; otherwise it flushes everything buffered.
func (w *Writer) FlushTo(ctx context.Context, lsn LSN) error {
	if err := w.checkOpen(); err != nil {
		return err
	}
	return w.flush(ctx, lsn)
}

func (w *Writer) checkOpen() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	return nil
}

// flush makes pending records durable. If upTo is non-zero and that record is
// already durable it returns at once. It does not check w.closed, so Close
// can use it.
func (w *Writer) flush(ctx context.Context, upTo LSN) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.flushMu.Lock()
	defer w.flushMu.Unlock()

	w.mu.Lock()
	if w.failed {
		w.mu.Unlock()
		return ErrFailed
	}
	if (upTo != 0 && uint64(upTo) < w.durableEnd) || len(w.pending) == 0 {
		w.mu.Unlock()
		return nil
	}
	chunks := w.pending
	w.pending, w.pendingBytes = nil, 0
	end := w.curStart + w.curLen
	w.mu.Unlock()

	err := w.writeChunks(chunks)

	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.failed = true // never retry: a failed fsync may have dropped data
		return err
	}
	if end > w.durableEnd {
		w.durableEnd = end
	}
	return nil
}

// writeChunks writes chunks in order, creating segment files as needed, and
// fsyncs. The caller holds flushMu.
func (w *Writer) writeChunks(chunks []chunk) error {
	created := false
	for _, c := range chunks {
		if uint64(c.start) != w.fStart {
			// Leaving a segment: it must be complete and durable before the
			// next one exists, or a crash could leave a hole in the log.
			if err := w.f.Sync(); err != nil {
				return fmt.Errorf("syncing segment %s: %w", SegmentName(LSN(w.fStart)), err)
			}
			if err := w.f.Close(); err != nil {
				return err
			}
			w.f = nil
			f, err := w.fsys.OpenFile(segPath(w.dir, c.start), vfs.ORead|vfs.OWrite|vfs.OCreate|vfs.OExcl)
			if err != nil {
				return fmt.Errorf("creating segment %s: %w", SegmentName(c.start), err)
			}
			w.f, w.fStart, w.fLen = f, uint64(c.start), 0
			created = true
		}
		if _, err := w.f.WriteAt(c.buf, w.fLen); err != nil {
			return fmt.Errorf("writing segment %s: %w", SegmentName(LSN(w.fStart)), err)
		}
		w.fLen += int64(len(c.buf))
	}
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("syncing segment %s: %w", SegmentName(LSN(w.fStart)), err)
	}
	if created {
		if err := w.fsys.SyncDir(w.dir); err != nil {
			return fmt.Errorf("syncing wal directory: %w", err)
		}
	}
	return nil
}

// Close flushes everything buffered and closes the log. A writer that has
// failed is closed without flushing and reports ErrFailed.
func (w *Writer) Close(ctx context.Context) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return ErrClosed
	}
	w.closed = true // from here on Append is refused
	failed := w.failed
	w.mu.Unlock()

	var err error
	if !failed {
		err = w.flush(ctx, 0)
	}
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	if w.f != nil {
		err = errors.Join(err, w.f.Close())
		w.f = nil
	}
	if failed {
		err = errors.Join(ErrFailed, err)
	}
	return err
}
