package btree

import (
	"bytes"
	"context"
	"fmt"

	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
)

// change is one page an operation modifies, under its exclusive latch.
type change struct {
	ref    *storage.PageRef
	before []byte // the page before the change: rollback and the image rule
	op     *Block // a leaf operation, or nil to log an image
}

// mutation collects the pages one logged step changes: a leaf insert or
// delete, or one structural change. For an unlogged tree it only marks the
// pages dirty.
type mutation struct {
	t       *Tree
	changes []change
}

// touch must be called for every page before it is changed: it marks the
// page dirty (before anything can be logged about it, see
// storage.PageRef.MarkDirty) and keeps a copy for rollback. op describes a
// leaf operation; nil means the page is logged as an image.
func (m *mutation) touch(ref *storage.PageRef, op *Block) {
	ref.MarkDirty()
	if m.t.lg == nil {
		return
	}
	m.changes = append(m.changes, change{ref: ref, before: bytes.Clone(ref.Data()), op: op})
}

// commit logs the changes as one record and stamps the pages with its LSN.
// A leaf operation on a page whose LSN was below the redo point is logged as
// an image (its copy on disk may be torn when recovery needs it). If logging
// fails every page is restored, so the pool never holds a change the log
// lacks. It returns the record's LSN (0 for an unlogged tree).
func (m *mutation) commit(ctx context.Context) (uint64, error) {
	if m.t.lg == nil || len(m.changes) == 0 {
		return 0, nil
	}
	lsn, err := m.t.lg.LogBTree(ctx, func(redo uint64) []byte {
		blocks := make([]Block, len(m.changes))
		for i, c := range m.changes {
			if c.op == nil || storage.PageLSN(c.before) < redo {
				blocks[i] = Block{Page: c.ref.ID(), Kind: BlockImage, Image: c.ref.Data()}
			} else {
				blocks[i] = *c.op
				blocks[i].Page = c.ref.ID()
			}
		}
		return encodeRecord(blocks)
	})
	if err != nil {
		for _, c := range m.changes {
			copy(c.ref.Data(), c.before)
		}
		return 0, fmt.Errorf("logging b+tree change: %w", err)
	}
	for _, c := range m.changes {
		storage.SetPageLSN(c.ref.Data(), lsn)
	}
	return lsn, nil
}
