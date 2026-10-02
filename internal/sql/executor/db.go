// Package executor runs SQL statements against a NoVacDB database: it
// binds them against the catalog, plans scans, evaluates expressions, and
// applies changes as atomic WAL statement groups. See
// docs/design/10-executor.md, section 2.6.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vikrant-choudhary06/NoVacDB/internal/btree"
	"github.com/vikrant-choudhary06/NoVacDB/internal/catalog"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/ast"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/parser"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/sqlerr"
	"github.com/vikrant-choudhary06/NoVacDB/internal/sql/types"
	"github.com/vikrant-choudhary06/NoVacDB/internal/storage"
	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
	"github.com/vikrant-choudhary06/NoVacDB/internal/wal"
)

// Defaults for Options.
const (
	DefaultFrames          = 4096     // 32 MiB of pages
	DefaultCheckpointBytes = 64 << 20 // of WAL between checkpoints
)

// Options configure a database.
type Options struct {
	// Frames is the buffer pool size in pages; it bounds how many pages
	// one statement can change. Zero means DefaultFrames.
	Frames int
	// CheckpointBytes is how much WAL may grow before a write statement
	// triggers a checkpoint. Zero means DefaultCheckpointBytes.
	CheckpointBytes int64
	// WAL configures the log; zero values take the log's defaults.
	WAL wal.Options
	// Now gives the time for now(); nil means time.Now.
	Now func() time.Time
	// Logger receives operational messages; nil means slog.Default().
	Logger *slog.Logger
}

// Column describes an output column.
type Column struct {
	Name string
	Type types.Type
}

// Result is one statement's outcome.
type Result struct {
	// Tag is PostgreSQL's command tag: "SELECT 2", "INSERT 0 1",
	// "CREATE TABLE", ...
	Tag     string
	Columns []Column        // SELECT only
	Rows    [][]types.Value // SELECT only
	Notices []string
}

// DB is an open database. It is safe for concurrent use: SELECTs run
// concurrently, and every statement that changes something runs alone.
type DB struct {
	fsys vfs.FS
	dir  string
	opts Options
	log  *slog.Logger

	mu       sync.RWMutex // shared for SELECT, exclusive for changes
	e        *wal.Engine
	cat      *catalog.Catalog
	closed   bool
	broken   error   // set when a self-restart failed
	lastCkpt wal.LSN // the WAL position of the last checkpoint

	// noIndexScans makes every scan sequential (tests compare the two).
	noIndexScans bool
	indexScans   atomic.Int64 // scans that used an index
	rowsRead     atomic.Int64 // rows scans have read, before WHERE
	restarts     atomic.Int64 // self-restarts after failed statements
}

// Open opens the database in dir, creating it if it does not exist, and
// recovers it after a crash.
func Open(ctx context.Context, fsys vfs.FS, dir string, opts Options) (*DB, error) {
	if opts.Frames == 0 {
		opts.Frames = DefaultFrames
	}
	if opts.CheckpointBytes == 0 {
		opts.CheckpointBytes = DefaultCheckpointBytes
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	db := &DB{fsys: fsys, dir: dir, opts: opts, log: opts.Logger}
	if db.log == nil {
		db.log = slog.Default()
	}
	if err := db.open(ctx); err != nil {
		return nil, err
	}
	return db, nil
}

func (db *DB) open(ctx context.Context) error {
	e, err := wal.OpenEngine(ctx, db.fsys, db.dir, wal.EngineOptions{Frames: db.opts.Frames, WAL: db.opts.WAL})
	if err != nil {
		return fmt.Errorf("opening the database: %w", err)
	}
	cat, err := catalog.Open(ctx, e, db.fsys, db.dir)
	if err != nil {
		// Creating the catalog may have left a statement open: drop
		// everything as a crash would.
		_ = e.Abandon()
		return fmt.Errorf("opening the catalog: %w", err)
	}
	db.e, db.cat = e, cat
	// Opening ends with a checkpoint at the end of the recovered log.
	db.lastCkpt = e.Recovery().EndLSN
	if rec := e.Recovery(); rec.Replayed > 0 || rec.DiscardedStatements > 0 {
		db.log.Info("recovered the database", "dir", db.dir, "replayed", rec.Replayed, "discarded_statements", rec.DiscardedStatements)
	}
	return nil
}

// Close checkpoints and closes the database. Statements after Close fail.
func (db *DB) Close(ctx context.Context) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return sqlerr.New(sqlerr.ObjectNotInPrerequisiteState, "the database is closed")
	}
	db.closed = true
	if db.broken != nil {
		return nil // nothing is open
	}
	return db.e.Close(ctx)
}

// Exec runs the statements in sql in order and returns one result per
// statement. It stops at the first error, returning the results of the
// statements before it; each statement that succeeded stays committed.
// Errors are *sqlerr.Error.
func (db *DB) Exec(ctx context.Context, sql string) ([]*Result, error) {
	stmts, err := parser.Parse(sql)
	if err != nil {
		return nil, sqlerr.From(err)
	}
	var results []*Result
	for _, s := range stmts {
		r, err := db.exec(ctx, sql, s)
		if err != nil {
			return results, err
		}
		results = append(results, r)
	}
	return results, nil
}

func canceled(err error) *sqlerr.Error {
	return sqlerr.Wrap(err, sqlerr.QueryCanceled, "canceling statement due to user request")
}

// exec runs one statement under the database lock.
func (db *DB) exec(ctx context.Context, sql string, s ast.Stmt) (*Result, error) {
	if _, ok := s.(*ast.Select); ok {
		db.mu.RLock()
		defer db.mu.RUnlock()
	} else {
		db.mu.Lock()
		defer db.mu.Unlock()
	}
	switch {
	case db.closed:
		return nil, sqlerr.New(sqlerr.ObjectNotInPrerequisiteState, "the database is closed")
	case db.broken != nil:
		return nil, sqlerr.Wrap(db.broken, sqlerr.IOError, "the database is unavailable: restarting it after a failure failed: %v", db.broken)
	}
	if err := ctx.Err(); err != nil {
		return nil, canceled(err)
	}
	st := &stmt{db: db, ctx: ctx, sql: sql, ec: &evalCtx{now: types.TimestampFromTime(db.opts.Now())}}
	var r *Result
	var err error
	switch s := s.(type) {
	case *ast.Select:
		r, err = st.selectStmt(s)
	case *ast.Insert:
		r, err = st.insert(s)
	case *ast.Update:
		r, err = st.update(s)
	case *ast.Delete:
		r, err = st.delete(s)
	case *ast.CreateTable:
		r, err = st.createTable(s)
	case *ast.DropTable:
		r, err = st.dropTable(s)
	case *ast.CreateIndex:
		r, err = st.createIndex(s)
	case *ast.DropIndex:
		r, err = st.dropIndex(s)
	default:
		err = sqlerr.New(sqlerr.FeatureNotSupported, "statement not supported: %s", s)
	}
	if err != nil {
		return nil, publicError(err)
	}
	return r, nil
}

// publicError turns any error into a *sqlerr.Error.
func publicError(err error) *sqlerr.Error {
	var se *sqlerr.Error
	switch {
	case errors.As(err, &se):
		return se
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return canceled(err)
	case isCorruption(err):
		return sqlerr.Wrap(err, sqlerr.DataCorrupted, "data corrupted: %v", err)
	}
	return sqlerr.Wrap(err, sqlerr.IOError, "could not access the database: %v", err)
}

func isCorruption(err error) bool {
	for _, c := range []error{storage.ErrCorrupt, storage.ErrCorruptHeap, storage.ErrChecksum, storage.ErrBadPageType,
		storage.ErrZeroPage, storage.ErrInvalidRID, btree.ErrCorruptNode, btree.ErrBadKey, wal.ErrCorrupt} {
		if errors.Is(err, c) {
			return true
		}
	}
	return false
}

// stmt is one statement being run.
type stmt struct {
	db  *DB
	ctx context.Context
	sql string
	ec  *evalCtx
}

// tooMuch is the error for a statement that changes more pages than the
// buffer pool holds.
func (db *DB) tooMuch(err error) *sqlerr.Error {
	return sqlerr.Wrap(err, sqlerr.ProgramLimitExceeded, "statement changes too much data").
		WithHint("A statement can change at most %d pages (the buffer pool's size) until transactions arrive; split it into smaller statements.", db.opts.Frames)
}

// apply runs fn, the change phase of a statement, in a statement group and
// commits it. The caller holds the exclusive
// lock and has already checked everything that can fail for SQL reasons.
//
// If anything fails once the group has begun, the changes cannot be undone
// in memory: the database restarts itself (closing as a crash would, then
// reopening, which discards the group) and returns the error. With ddl
// set, a *sqlerr.Error from fn means fn changed nothing (the catalog's
// rule), so the group commits and the error is returned.
//
// Cancellation is not honoured from here on: a statement that has begun
// changing data runs to its end.
func (st *stmt) apply(ddl bool, fn func(ctx context.Context) error) error {
	db := st.db
	ctx := context.WithoutCancel(st.ctx)
	if err := db.e.BeginStatement(ctx); err != nil {
		return db.restart(ctx, err)
	}
	ferr := fn(ctx)
	var se *sqlerr.Error
	if ferr != nil && (!ddl || !errors.As(ferr, &se)) {
		return db.restart(ctx, ferr)
	}
	lsn, err := db.e.CommitStatement(ctx)
	if err != nil {
		return db.restart(ctx, err)
	}
	if ferr != nil {
		return ferr
	}
	db.maybeCheckpoint(ctx, lsn)
	return nil
}

// restart abandons the engine and reopens the database after a failed
// statement, and returns the statement's error for the client.
func (db *DB) restart(ctx context.Context, cause error) error {
	db.log.Warn("a statement failed while changing data; restarting the database", "dir", db.dir, "err", cause)
	db.restarts.Add(1)
	_ = db.e.Abandon()
	db.e, db.cat = nil, nil
	if err := db.open(ctx); err != nil {
		db.broken = err
		db.log.Error("restarting the database failed", "dir", db.dir, "err", err)
	}
	if errors.Is(cause, storage.ErrNoFreeFrames) {
		return db.tooMuch(cause)
	}
	return publicError(cause)
}

// maybeCheckpoint takes a checkpoint once the WAL has grown enough since
// the last one. The statement has committed, so a failure is only logged;
// the next checkpoint tries again.
func (db *DB) maybeCheckpoint(ctx context.Context, lsn wal.LSN) {
	if int64(lsn-db.lastCkpt) < db.opts.CheckpointBytes {
		return
	}
	c, err := db.e.Checkpoint(ctx)
	if err != nil {
		db.log.Warn("checkpoint failed", "dir", db.dir, "err", err)
		return
	}
	db.lastCkpt = c.CheckpointLSN
}
