package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
)

// Size limits for keys and values. A node holds at least five
// maximum-size leaf cells and seven maximum-size internal cells.
const (
	MaxKeySize   = 1024
	MaxValueSize = 512
)

// ErrCorruptNode means a page is not a valid B+Tree node.
var ErrCorruptNode = errors.New("btree: corrupt node")

// errNodeFull means a cell does not fit in a node. The tree splits nodes
// before they can be full, so callers outside this file never see it.
var errNodeFull = errors.New("btree: node full")

// Node layout (after the 24-byte page header; little-endian):
//
//	offset  size  field
//	24      2     NumCells
//	26      2     Upper     (lowest cell byte; PageSize when there are none)
//	28      2     Level     (0 for a leaf)
//	30      2     reserved  (zero)
//	32      8     Child0    (internal: leftmost child; leaf: zero)
//	40      8     reserved  (zero)
//	48      2*N   slot array: cell offsets, in key order
//
// Leaf cell: u16 KeyLen, u16 ValueLen, key, value.
// Internal cell: u16 KeyLen, u64 Child, key.
// Every byte that is not header, slot or cell is zero.
const (
	offNumCells = storage.HeaderSize
	offUpper    = offNumCells + 2
	offLevel    = offUpper + 2
	offReserved = offLevel + 2
	offChild0   = offReserved + 2
	offReserve2 = offChild0 + 8
	slotsStart  = offReserve2 + 8

	nodeSpace     = storage.PageSize - slotsStart
	slotSize      = 2
	leafCellHdr   = 4
	innerCellHdr  = 10
	maxLeafCell   = leafCellHdr + MaxKeySize + MaxValueSize
	maxInnerCell  = innerCellHdr + MaxKeySize
	underfullSize = nodeSpace / 4 // a node using fewer bytes is underfull
	maxLevel      = 64            // far beyond any tree that fits in a data file
)

// node is a view of one page holding a B+Tree node. Accessors check every
// offset they follow, so a corrupt page yields ErrCorruptNode, never a panic.
type node struct{ b []byte }

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrCorruptNode)
}

func (n node) u16(off int) int  { return int(binary.LittleEndian.Uint16(n.b[off:])) }
func (n node) put16(off, v int) { binary.LittleEndian.PutUint16(n.b[off:], uint16(v)) }
func (n node) pageType() storage.PageType {
	return storage.PageType(binary.LittleEndian.Uint16(n.b[20:]))
}
func (n node) isLeaf() bool       { return n.pageType() == storage.PageTypeBTreeLeaf }
func (n node) numCells() int      { return n.u16(offNumCells) }
func (n node) upper() int         { return n.u16(offUpper) }
func (n node) level() int         { return n.u16(offLevel) }
func (n node) child0() uint64     { return binary.LittleEndian.Uint64(n.b[offChild0:]) }
func (n node) setChild0(c uint64) { binary.LittleEndian.PutUint64(n.b[offChild0:], c) }
func (n node) slot(i int) int     { return n.u16(slotsStart + slotSize*i) }
func (n node) setUpper(u int)     { n.put16(offUpper, u) }
func (n node) setNumCells(c int)  { n.put16(offNumCells, c) }

// gap is the free space between the slot array and the cells.
func (n node) gap() int { return n.upper() - (slotsStart + slotSize*n.numCells()) }

// initNode makes buf an empty node, keeping the page's ID and LSN.
func initNode(buf []byte, leaf bool, level int) error {
	h, err := storage.DecodeHeader(buf)
	if err != nil {
		return err
	}
	t := storage.PageTypeBTreeInternal
	if leaf {
		t = storage.PageTypeBTreeLeaf
	}
	if err := storage.InitPage(buf, storage.Header{ID: h.ID, LSN: h.LSN, Type: t}); err != nil {
		return err
	}
	n := node{buf}
	n.setUpper(storage.PageSize)
	n.put16(offLevel, level)
	return nil
}

// checkHeader validates the node header in O(1).
func (n node) checkHeader() error {
	if len(n.b) != storage.PageSize {
		return corrupt("node of %d bytes", len(n.b))
	}
	t := n.pageType()
	if t != storage.PageTypeBTreeLeaf && t != storage.PageTypeBTreeInternal {
		return corrupt("page type %d is not a node", t)
	}
	cnt, up, lvl := n.numCells(), n.upper(), n.level()
	if slotsStart+slotSize*cnt > up || up > storage.PageSize {
		return corrupt("%d cells with upper %d", cnt, up)
	}
	if n.u16(offReserved) != 0 || binary.LittleEndian.Uint64(n.b[offReserve2:]) != 0 {
		return corrupt("reserved header bytes set")
	}
	if t == storage.PageTypeBTreeLeaf {
		if lvl != 0 || n.child0() != 0 {
			return corrupt("leaf with level %d and child %d", lvl, n.child0())
		}
	} else if lvl < 1 || lvl > maxLevel || n.child0() < storage.FirstDataPage {
		return corrupt("internal node with level %d and child %d", lvl, n.child0())
	}
	return nil
}

// cellBounds returns the offset and size of cell i, checked to lie between
// Upper and the end of the page.
func (n node) cellBounds(i int) (off, size int, err error) {
	if i < 0 || i >= n.numCells() {
		return 0, 0, corrupt("cell %d of %d", i, n.numCells())
	}
	off = n.slot(i)
	hdr := innerCellHdr
	if n.isLeaf() {
		hdr = leafCellHdr
	}
	if off < n.upper() || off+hdr > storage.PageSize {
		return 0, 0, corrupt("cell %d at offset %d", i, off)
	}
	kl := n.u16(off)
	size = hdr + kl
	if kl > MaxKeySize {
		return 0, 0, corrupt("cell %d key length %d", i, kl)
	}
	if n.isLeaf() {
		vl := n.u16(off + 2)
		if vl > MaxValueSize {
			return 0, 0, corrupt("cell %d value length %d", i, vl)
		}
		size += vl
	}
	if off+size > storage.PageSize {
		return 0, 0, corrupt("cell %d at %d, %d bytes, overruns the page", i, off, size)
	}
	return off, size, nil
}

// key returns cell i's key, aliasing the page.
func (n node) key(i int) ([]byte, error) {
	off, _, err := n.cellBounds(i)
	if err != nil {
		return nil, err
	}
	hdr := innerCellHdr
	if n.isLeaf() {
		hdr = leafCellHdr
	}
	kl := n.u16(off)
	return n.b[off+hdr : off+hdr+kl : off+hdr+kl], nil
}

// value returns leaf cell i's value, aliasing the page.
func (n node) value(i int) ([]byte, error) {
	off, size, err := n.cellBounds(i)
	if err != nil {
		return nil, err
	}
	start := off + leafCellHdr + n.u16(off)
	return n.b[start : off+size : off+size], nil
}

// cellChild returns internal cell i's child.
func (n node) cellChild(i int) (uint64, error) {
	off, _, err := n.cellBounds(i)
	if err != nil {
		return 0, err
	}
	c := binary.LittleEndian.Uint64(n.b[off+2:])
	if c < storage.FirstDataPage {
		return 0, corrupt("cell %d child %d", i, c)
	}
	return c, nil
}

// childAt returns the internal node's pos'th child: Child0 for pos 0, else
// the child of cell pos-1.
func (n node) childAt(pos int) (uint64, error) {
	if pos == 0 {
		return n.child0(), nil
	}
	return n.cellChild(pos - 1)
}

// setCellChild replaces internal cell i's child. The cell must be valid.
func (n node) setCellChild(i int, c uint64) {
	binary.LittleEndian.PutUint64(n.b[n.slot(i)+2:], c)
}

// search returns the first index whose key is >= key, and whether that key
// equals key.
func (n node) search(key []byte) (int, bool, error) {
	lo, hi := 0, n.numCells()
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		k, err := n.key(mid)
		if err != nil {
			return 0, false, err
		}
		c := bytes.Compare(k, key)
		if c == 0 {
			return mid, true, nil
		}
		if c < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, false, nil
}

// childFor returns the position (for childAt) and ID of the child of an
// internal node that covers key: the child of the last cell whose key is
// <= key, or Child0.
func (n node) childFor(key []byte) (int, uint64, error) {
	i, found, err := n.search(key)
	if err != nil {
		return 0, 0, err
	}
	pos := i
	if found {
		pos = i + 1
	}
	c, err := n.childAt(pos)
	return pos, c, err
}

// cellSize returns the bytes a cell takes, excluding its slot.
func leafCellSize(key, val []byte) int { return leafCellHdr + len(key) + len(val) }
func innerCellSize(key []byte) int     { return innerCellHdr + len(key) }

// used returns the bytes taken by slots and cells.
func (n node) used() (int, error) {
	total := slotSize * n.numCells()
	for i := range n.numCells() {
		_, size, err := n.cellBounds(i)
		if err != nil {
			return 0, err
		}
		total += size
	}
	return total, nil
}

// reserve makes room for a cell of size bytes at index i and returns the
// cell's bytes for the caller to fill. It compacts the node if the gap is too
// small but the free space suffices, and fails with errNodeFull otherwise.
func (n node) reserve(i, size int) ([]byte, error) {
	cnt := n.numCells()
	if i < 0 || i > cnt {
		return nil, corrupt("insert at %d of %d", i, cnt)
	}
	if n.gap() < size+slotSize {
		used, err := n.used()
		if err != nil {
			return nil, err
		}
		if nodeSpace-used < size+slotSize {
			return nil, errNodeFull
		}
		if err := n.compact(); err != nil {
			return nil, err
		}
	}
	off := n.upper() - size
	at := slotsStart + slotSize*i
	end := slotsStart + slotSize*cnt
	copy(n.b[at+slotSize:end+slotSize], n.b[at:end])
	n.put16(at, off)
	n.setNumCells(cnt + 1)
	n.setUpper(off)
	return n.b[off : off+size : off+size], nil
}

// insertLeaf inserts a leaf cell at index i.
func (n node) insertLeaf(i int, key, val []byte) error {
	c, err := n.reserve(i, leafCellSize(key, val))
	if err != nil {
		return err
	}
	binary.LittleEndian.PutUint16(c, uint16(len(key)))
	binary.LittleEndian.PutUint16(c[2:], uint16(len(val)))
	copy(c[leafCellHdr:], key)
	copy(c[leafCellHdr+len(key):], val)
	return nil
}

// insertInner inserts an internal cell at index i.
func (n node) insertInner(i int, key []byte, child uint64) error {
	c, err := n.reserve(i, innerCellSize(key))
	if err != nil {
		return err
	}
	binary.LittleEndian.PutUint16(c, uint16(len(key)))
	binary.LittleEndian.PutUint64(c[2:], child)
	copy(c[innerCellHdr:], key)
	return nil
}

// remove deletes cell i, zeroing its bytes and its slot.
func (n node) remove(i int) error {
	off, size, err := n.cellBounds(i)
	if err != nil {
		return err
	}
	cnt := n.numCells()
	at := slotsStart + slotSize*i
	end := slotsStart + slotSize*cnt
	copy(n.b[at:], n.b[at+slotSize:end])
	clear(n.b[end-slotSize : end])
	clear(n.b[off : off+size])
	n.setNumCells(cnt - 1)
	if off == n.upper() {
		n.setUpper(off + size)
	}
	return nil
}

// compact packs the cells against the end of the page in slot order and
// zeroes everything else. The result depends only on the cells, so redo
// reproduces it exactly.
func (n node) compact() error {
	var scratch [storage.PageSize]byte
	cnt := n.numCells()
	up := storage.PageSize
	offs := make([]int, cnt)
	for i := range cnt {
		off, size, err := n.cellBounds(i)
		if err != nil {
			return err
		}
		up -= size
		if up < slotsStart+slotSize*cnt {
			return corrupt("cells overlap: %d cells do not fit", cnt)
		}
		copy(scratch[up:], n.b[off:off+size])
		offs[i] = up
	}
	clear(n.b[slotsStart+slotSize*cnt:])
	copy(n.b[up:], scratch[up:])
	for i, off := range offs {
		n.put16(slotsStart+slotSize*i, off)
	}
	n.setUpper(up)
	return nil
}

// validate checks the whole node: header, cell bounds, no overlapping
// cells, strictly increasing keys, valid children, and zeroed free space.
func (n node) validate() error {
	if err := n.checkHeader(); err != nil {
		return err
	}
	cnt := n.numCells()
	type span struct{ off, end int }
	spans := make([]span, 0, cnt)
	var prev []byte
	for i := range cnt {
		off, size, err := n.cellBounds(i)
		if err != nil {
			return err
		}
		spans = append(spans, span{off, off + size})
		k, _ := n.key(i)
		if i > 0 && bytes.Compare(prev, k) >= 0 {
			return corrupt("key %d is not above key %d", i, i-1)
		}
		prev = k
		if !n.isLeaf() {
			if _, err := n.cellChild(i); err != nil {
				return err
			}
		}
	}
	// Mark every byte a cell owns; overlaps and stray bytes both show up.
	var owned [storage.PageSize]bool
	for i, s := range spans {
		for j := s.off; j < s.end; j++ {
			if owned[j] {
				return corrupt("cell %d overlaps another cell", i)
			}
			owned[j] = true
		}
	}
	for j := slotsStart + slotSize*cnt; j < storage.PageSize; j++ {
		if !owned[j] && n.b[j] != 0 {
			return corrupt("free byte %d is not zero", j)
		}
	}
	return nil
}
