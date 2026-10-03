package wal

import (
	"encoding/binary"
	"fmt"

	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
)

// MaxDeferredPerRecord is the most entries one deferred-free record holds,
// which keeps it within MaxPayload.
const MaxDeferredPerRecord = 65535

// Deferred-free record payload (type 6), little-endian: u32 Count, then
// Count entries of u64 PageID and u64 LSN. LSN 0 means the record's own
// LSN. See docs/design/08-btree.md section 3.

// EncodeDeferredFree encodes a deferred-free record: pages, with lsns[i]
// the threshold of pages[i], or all zero (the record's own LSN) if lsns is
// nil.
func EncodeDeferredFree(pages, lsns []uint64) []byte {
	b := binary.LittleEndian.AppendUint32(make([]byte, 0, 4+16*len(pages)), uint32(len(pages)))
	for i, p := range pages {
		var lsn uint64
		if lsns != nil {
			lsn = lsns[i]
		}
		b = binary.LittleEndian.AppendUint64(b, p)
		b = binary.LittleEndian.AppendUint64(b, lsn)
	}
	return b
}

// DecodeDeferredFree decodes a deferred-free record logged at recLSN,
// resolving LSN 0 to recLSN.
func DecodeDeferredFree(payload []byte, recLSN LSN) ([]deferredFree, error) {
	if len(payload) < 4 {
		return nil, fmt.Errorf("deferred-free record of %d bytes: %w", len(payload), ErrCorrupt)
	}
	n := int(binary.LittleEndian.Uint32(payload))
	if n == 0 || n > MaxDeferredPerRecord || len(payload) != 4+16*n {
		return nil, fmt.Errorf("deferred-free record: %d entries in %d bytes: %w", n, len(payload), ErrCorrupt)
	}
	out := make([]deferredFree, n)
	for i := range out {
		e := payload[4+16*i:]
		out[i] = deferredFree{binary.LittleEndian.Uint64(e), binary.LittleEndian.Uint64(e[8:])}
		if out[i].page < storage.FirstDataPage {
			return nil, fmt.Errorf("deferred-free record names page %d: %w", out[i].page, ErrCorrupt)
		}
		if out[i].lsn == 0 {
			out[i].lsn = uint64(recLSN)
		}
	}
	return out, nil
}
