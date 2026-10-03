package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// LSN is a log sequence number: the byte offset of a record's first byte in
// the log. No record has LSN 0.
type LSN uint64

// RecordType says what a record means. The log does not interpret it; zero is
// invalid so that zeroed space never decodes as a record.
type RecordType uint16

const (
	// RecordHeaderSize is the size of the fixed header in front of a payload.
	RecordHeaderSize = 20
	// MaxPayload is the largest payload a record may carry.
	MaxPayload = 1 << 20
)

// Record header layout (20 bytes, little-endian):
//
//	offset  size  field
//	0       4     CRC-32C of bytes 4 .. 20+PayloadLen
//	4       4     PayloadLen
//	8       8     LSN (this record's own offset in the log)
//	16      2     Type (0 is invalid)
//	18      2     Flags (must be zero)
//	20      ...   Payload
const (
	recOffCRC   = 0
	recOffLen   = 4
	recOffLSN   = 8
	recOffType  = 16
	recOffFlags = 18
)

// Errors returned by the record codec and the writer.
var (
	// ErrShortRecord means the buffer ends before the record does. At the
	// end of a log this is the normal sign of a record that was never
	// completely written.
	ErrShortRecord = errors.New("wal: incomplete record")
	// ErrBadRecord means a header field is invalid.
	ErrBadRecord = errors.New("wal: invalid record")
	// ErrChecksum means the record's checksum does not match its content.
	ErrChecksum = errors.New("wal: record checksum mismatch")
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Record is a decoded log record. Payload aliases the buffer it was decoded
// from.
type Record struct {
	LSN     LSN
	Type    RecordType
	Payload []byte
}

// RecordSize returns the encoded size of a record with the given payload
// length.
func RecordSize(payloadLen int) int { return RecordHeaderSize + payloadLen }

// AppendRecord appends the encoding of a record to dst and returns the
// extended slice. The caller must have checked that t is non-zero and the
// payload is at most MaxPayload; DecodeRecord rejects anything else.
func AppendRecord(dst []byte, lsn LSN, t RecordType, payload []byte) []byte {
	start := len(dst)
	dst = append(dst, make([]byte, RecordHeaderSize)...)
	dst = append(dst, payload...)
	h := dst[start:]
	binary.LittleEndian.PutUint32(h[recOffLen:], uint32(len(payload)))
	binary.LittleEndian.PutUint64(h[recOffLSN:], uint64(lsn))
	binary.LittleEndian.PutUint16(h[recOffType:], uint16(t))
	binary.LittleEndian.PutUint16(h[recOffFlags:], 0)
	binary.LittleEndian.PutUint32(h[recOffCRC:], crc32.Checksum(h[recOffLen:], castagnoli))
	return dst
}

// DecodeRecord decodes the record at the start of buf and returns it together
// with its encoded size. Bytes after the record are ignored. It never panics
// and allocates nothing; the payload aliases buf.
//
// Errors: ErrShortRecord if buf ends early, ErrChecksum if the content does
// not match its checksum, ErrBadRecord for an invalid type, nonzero flags or
// an oversize length. It does not know where in the log the record sits, so
// checking the LSN against the position is up to the caller.
func DecodeRecord(buf []byte) (Record, int, error) {
	if len(buf) < RecordHeaderSize {
		return Record{}, 0, fmt.Errorf("%d bytes, header needs %d: %w", len(buf), RecordHeaderSize, ErrShortRecord)
	}
	payloadLen := uint64(binary.LittleEndian.Uint32(buf[recOffLen:]))
	if payloadLen > MaxPayload {
		return Record{}, 0, fmt.Errorf("payload length %d exceeds %d: %w", payloadLen, MaxPayload, ErrBadRecord)
	}
	total := RecordHeaderSize + int(payloadLen)
	if len(buf) < total {
		return Record{}, 0, fmt.Errorf("%d bytes, record needs %d: %w", len(buf), total, ErrShortRecord)
	}
	if got, want := binary.LittleEndian.Uint32(buf[recOffCRC:]), crc32.Checksum(buf[recOffLen:total], castagnoli); got != want {
		return Record{}, 0, fmt.Errorf("stored crc %#08x, computed %#08x: %w", got, want, ErrChecksum)
	}
	t := RecordType(binary.LittleEndian.Uint16(buf[recOffType:]))
	if t == 0 {
		return Record{}, 0, fmt.Errorf("record type 0: %w", ErrBadRecord)
	}
	if f := binary.LittleEndian.Uint16(buf[recOffFlags:]); f != 0 {
		return Record{}, 0, fmt.Errorf("flags %#x: %w", f, ErrBadRecord)
	}
	return Record{
		LSN:     LSN(binary.LittleEndian.Uint64(buf[recOffLSN:])),
		Type:    t,
		Payload: buf[RecordHeaderSize:total:total],
	}, total, nil
}
