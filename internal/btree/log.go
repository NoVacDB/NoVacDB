package btree

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
)

// Logger is how a tree records its changes in the write-ahead log. The wal
// package's Logger implements it. See docs/design/08-btree.md, section 2.10.
type Logger interface {
	// LogBTree appends a B+Tree record describing a change to pages the
	// caller holds exclusively latched, and returns its LSN. build gets the
	// current redo point, which cannot change before the append. If it
	// fails, the record must be treated as possibly logged.
	LogBTree(ctx context.Context, build func(redoPoint uint64) []byte) (uint64, error)
	// DeferFree asks for pages to be freed once no record that refers to
	// them can be replayed. It is called after the record that unlinked
	// them, and logs the request so that it survives a crash.
	DeferFree(ctx context.Context, pages ...uint64) error
}

// ErrBadRecord means a B+Tree WAL record is malformed, or cannot be replayed
// onto the page it names.
var ErrBadRecord = errors.New("btree: bad log record")

// BlockKind says what a block of a B+Tree record contains.
type BlockKind uint8

// Block kinds.
const (
	// BlockImage holds the whole page as it was after the change.
	BlockImage BlockKind = 1 + iota
	// BlockLeafInsert inserts Key/Value at Index of a leaf.
	BlockLeafInsert
	// BlockLeafDelete deletes Key, which is at Index of a leaf.
	BlockLeafDelete
)

// maxBlocks is the most pages one record may describe.
const maxBlocks = 8

// Block is the part of a B+Tree record about one page.
type Block struct {
	Page  uint64
	Kind  BlockKind
	Index uint16
	Key   []byte
	Value []byte
	Image []byte // PageSize bytes
}

// B+Tree record payload layout (little-endian):
//
//	offset  size  field
//	0       1     BlockCount (1 to 8)
//	1       ...   blocks, back to back
//
// Block: u64 PageID, u8 Kind, then
//
//	image:        8192 bytes
//	leaf insert:  u16 index, u16 key length, u16 value length, key, value
//	leaf delete:  u16 index, u16 key length, key
func encodeRecord(blocks []Block) []byte {
	out := []byte{byte(len(blocks))}
	for _, b := range blocks {
		out = binary.LittleEndian.AppendUint64(out, b.Page)
		out = append(out, byte(b.Kind))
		switch b.Kind {
		case BlockImage:
			out = append(out, b.Image...)
		case BlockLeafInsert:
			out = binary.LittleEndian.AppendUint16(out, b.Index)
			out = binary.LittleEndian.AppendUint16(out, uint16(len(b.Key)))
			out = binary.LittleEndian.AppendUint16(out, uint16(len(b.Value)))
			out = append(out, b.Key...)
			out = append(out, b.Value...)
		case BlockLeafDelete:
			out = binary.LittleEndian.AppendUint16(out, b.Index)
			out = binary.LittleEndian.AppendUint16(out, uint16(len(b.Key)))
			out = append(out, b.Key...)
		}
	}
	return out
}

// DecodeRecord decodes and checks a B+Tree record payload. The blocks alias
// p. It never panics on any input.
func DecodeRecord(p []byte) ([]Block, error) {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrBadRecord)
	}
	if len(p) < 1 {
		return nil, bad("empty payload")
	}
	n := int(p[0])
	if n < 1 || n > maxBlocks {
		return nil, bad("%d blocks", n)
	}
	rest := p[1:]
	blocks := make([]Block, 0, n)
	for i := range n {
		if len(rest) < 9 {
			return nil, bad("block %d: truncated header", i)
		}
		b := Block{Page: binary.LittleEndian.Uint64(rest), Kind: BlockKind(rest[8])}
		rest = rest[9:]
		if b.Page < storage.FirstDataPage {
			return nil, bad("block %d: page %d is reserved", i, b.Page)
		}
		switch b.Kind {
		case BlockImage:
			if len(rest) < storage.PageSize {
				return nil, bad("block %d: truncated image", i)
			}
			b.Image, rest = rest[:storage.PageSize:storage.PageSize], rest[storage.PageSize:]
		case BlockLeafInsert, BlockLeafDelete:
			hdr := 4
			if b.Kind == BlockLeafInsert {
				hdr = 6
			}
			if len(rest) < hdr {
				return nil, bad("block %d: truncated operation", i)
			}
			b.Index = binary.LittleEndian.Uint16(rest)
			kl := int(binary.LittleEndian.Uint16(rest[2:]))
			vl := 0
			if b.Kind == BlockLeafInsert {
				vl = int(binary.LittleEndian.Uint16(rest[4:]))
			}
			rest = rest[hdr:]
			if kl > MaxKeySize || vl > MaxValueSize || kl+vl > len(rest) {
				return nil, bad("block %d: key length %d, value length %d", i, kl, vl)
			}
			b.Key, rest = rest[:kl:kl], rest[kl:]
			if b.Kind == BlockLeafInsert {
				b.Value, rest = rest[:vl:vl], rest[vl:]
			}
		default:
			return nil, bad("block %d: kind %d", i, b.Kind)
		}
		for _, prev := range blocks {
			if prev.Page == b.Page {
				return nil, bad("page %d appears twice", b.Page)
			}
		}
		blocks = append(blocks, b)
	}
	if len(rest) != 0 {
		return nil, bad("%d bytes after the last block", len(rest))
	}
	return blocks, nil
}

// Redo replays the B+Tree record at lsn onto the pages in bp. An image is
// installed whatever the page holds (its copy on disk may be torn or free);
// a leaf operation is applied only if the page's LSN is below lsn, and must
// fit the page exactly. Every changed page is stamped with lsn. Anything
// that does not fit is ErrBadRecord.
func Redo(ctx context.Context, bp *storage.BufferPool, lsn uint64, payload []byte) error {
	blocks, err := DecodeRecord(payload)
	if err != nil {
		return fmt.Errorf("redo at %d: %w", lsn, err)
	}
	for _, b := range blocks {
		if err := redoBlock(ctx, bp, lsn, b); err != nil {
			return fmt.Errorf("redo at %d, page %d: %w", lsn, b.Page, err)
		}
	}
	return nil
}

func redoBlock(ctx context.Context, bp *storage.BufferPool, lsn uint64, b Block) error {
	if b.Kind == BlockImage {
		// Validate before pinning: an overwrite pin must be followed by
		// an overwrite.
		h, err := storage.DecodeHeader(b.Image)
		if err != nil || h.ID != b.Page {
			return fmt.Errorf("image is not of page %d: %w", b.Page, ErrBadRecord)
		}
		if err := (node{b.Image}).validate(); err != nil {
			return fmt.Errorf("image: %w: %w", ErrBadRecord, err)
		}
		ref, err := bp.PinForOverwrite(ctx, b.Page)
		if err != nil {
			return err
		}
		ref.Lock()
		copy(ref.Data(), b.Image)
		storage.SetPageLSN(ref.Data(), lsn)
		ref.Unlock()
		return ref.Unpin(true)
	}
	ref, err := bp.FetchPage(ctx, b.Page)
	if err != nil {
		return err
	}
	ref.Lock()
	dirty, err := applyBlock(ref.Data(), lsn, b)
	ref.Unlock()
	if uerr := ref.Unpin(dirty); err == nil {
		err = uerr
	}
	return err
}

// applyBlock applies a leaf operation to a page, unless the page already
// contains it. It changes the page only if the operation fits exactly.
func applyBlock(page []byte, lsn uint64, b Block) (bool, error) {
	if storage.PageLSN(page) >= lsn {
		return false, nil
	}
	n := node{page}
	if err := n.checkHeader(); err != nil || !n.isLeaf() {
		return false, fmt.Errorf("page is not a leaf: %w", ErrBadRecord)
	}
	i := int(b.Index)
	switch b.Kind {
	case BlockLeafInsert:
		at, found, err := n.search(b.Key)
		if err != nil || found || at != i {
			return false, fmt.Errorf("insert at %d does not fit the page: %w", i, ErrBadRecord)
		}
		if err := n.insertLeaf(i, b.Key, b.Value); err != nil {
			return false, fmt.Errorf("insert: %w: %w", ErrBadRecord, err)
		}
	case BlockLeafDelete:
		k, err := n.key(i)
		if err != nil || !bytes.Equal(k, b.Key) {
			return false, fmt.Errorf("delete at %d does not fit the page: %w", i, ErrBadRecord)
		}
		if err := n.remove(i); err != nil {
			return false, fmt.Errorf("delete: %w: %w", ErrBadRecord, err)
		}
	default:
		return false, fmt.Errorf("kind %d: %w", b.Kind, ErrBadRecord)
	}
	storage.SetPageLSN(page, lsn)
	return true, nil
}
