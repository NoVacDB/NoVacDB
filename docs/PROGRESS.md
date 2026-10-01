# NoVacDB — Build Progress

This file is the step-by-step build plan. Each step is sized for roughly one focused session. Work happens **one step at a time, in order**.

**Status legend:** ✅ Done · 🚧 In progress · ⬜ Not started · 👉 **CURRENT**

---

## Current step

👉 **Step 2.4 — Checkpoints** (design in `07-checkpoints-recovery.md`)

---

## Session log

| Date | Step | Result | Notes |
|---|---|---|---|
| 2026-10-01 | 0.1 Project skeleton | ✅ Done | Module, `cmd/novacdb`, Makefile, golangci config, CI workflow, `.gitignore`, design index. Full check loop green. |
| 2026-10-01 | 0.2 Virtual file system | ✅ Done | `internal/vfs`: OSFS, MemFS with durable/volatile model, Crash with torn last write, fault injection. Coverage 97.2%. |
| 2026-10-01 | 1.1 Page format | ✅ Done | `internal/storage`: 8 KiB page, 24-byte header, CRC-32C Seal/Verify, 2 fuzz targets. Coverage 100%. |
| 2026-10-01 | 1.2 Disk manager | ✅ Done | Dual header slots, free list, atomic Create, poisoned-on-error state. 1500 seeded crash runs, FuzzOpen. |
| 2026-10-01 | 1.3 Buffer pool | ✅ Done | Clock replacement, PageRef pins and latches, copy-on-flush, WAL-rule hook. Model-based and concurrent tests. |
| 2026-10-01 | 1.4 Slotted pages and heap | ✅ Done | Slotted page (insert/get/update/delete/compact), heap chain with in-memory FSM, scanner. Model-based, concurrent, crash-at-checkpoint tests; 2 fuzz targets; 16 deliberate-bug checks. |
| 2026-10-01 | 2.1 WAL writer | ✅ Done | `internal/wal`: record + segment formats, Append/Flush/FlushTo, durable-end tracking, open-time tail recovery, buffer-pool hook (`FlushedLSN`). Found and fixed a durability bug via the crash test's process-kill mode. 20 deliberate-bug checks. |
| 2026-10-01 | 2.2 WAL reader | ✅ Done | Sequential reader across segments sharing recovery's validation; torn tail = clean end, earlier damage = ErrCorrupt; boundary-checked start; FuzzReader; crash harness cross-checks reader vs recovery. |
| 2026-10-01 | 2.3 Logging heap changes | ✅ Done | One physiological record per heap operation (two-page moves and grows atomic), full-page image on first change after the redo point, redo function, pool FlushWAL hook + PinForOverwrite, wal.Logger. Found and fixed a flush-path race. 11 deliberate-bug checks. |

---

## Phase 0 — Bootstrap

### ✅ Step 0.1 — Project skeleton
**Goal:** A clean, buildable, testable empty project with all tooling in place.
**Scope:**
- `go.mod` with the repo's module path, no dependencies
- Folder layout from `WORKFLOW.md` Section 6 (only folders needed now, each with `doc.go`)
- `cmd/novacdb/main.go` that parses `--data-dir` and `--port` (default 5433), logs startup with `slog`, and exits cleanly with a "not implemented yet" message
- `Makefile` with all targets listed in `WORKFLOW.md` Section 6, plus `make cover` and `make check` (future ones print a friendly message and exit 0)
- `.gitignore` (`bin/`, `data/`, coverage files, fuzz cache, OS and editor junk, local tool config)
- `.golangci.yml` with a sensible, strict linter set
- `.github/workflows/ci.yml`: gofmt, vet, build, `test -race`, golangci-lint, on push and pull request
- `docs/design/README.md` — index of design docs
**Acceptance:** `make build`, `make test`, `make lint` all pass; `go mod tidy` leaves `go.mod` unchanged; the binary runs and exits cleanly.

### ✅ Step 0.2 — Virtual file system (`internal/vfs`)
**Goal:** Every file operation goes through one interface, so tests can simulate crashes.
**Scope:**
- `FS` and `File` interfaces (open, create, read at, write at, sync, truncate, size, close, remove, rename, list, sync directory)
- `OSFS` implementation backed by the real file system
- `MemFS` implementation for tests that tracks **durable** vs **volatile** data: writes are volatile until `Sync`; `Crash()` discards everything unsynced; an option tears the last unsynced write (keeps only a random prefix); an option injects I/O errors on chosen operations
- Design doc `docs/design/01-vfs.md`
**Acceptance:** Same behaviour test suite runs against both `OSFS` and `MemFS`; crash and torn-write semantics tested; coverage ≥ 80%.

---

## Phase 1 — Storage

### ✅ Step 1.1 — Page format
**Goal:** Fixed 8 KiB pages with a header and checksum.
**Scope:** Page header (page ID, LSN, CRC-32C, page type, flags), encode/decode, checksum compute and verify, page type constants. Design doc `02-page-format.md`.
**Acceptance:** Round-trip tests; corrupted bytes detected; fuzz target for the decoder runs 30s without failures.

### ✅ Step 1.2 — Disk manager
**Goal:** Read, write, and allocate pages in a data file.
**Scope:** File header page (magic number, format version, page size), allocate/free pages with a free list, read/write by page ID via `vfs`, durable `Sync`. Design doc `03-disk-manager.md`.
**Acceptance:** Pages survive reopen; freed pages are reused; corrupt or wrong-version files are rejected with clear errors; crash tests with `MemFS` show no corruption of synced data.

### ✅ Step 1.3 — Buffer pool
**Goal:** Keep hot pages in memory.
**Scope:** Fixed number of frames, page table, pin/unpin with pin counts, dirty tracking, Clock replacement, flush one / flush all, concurrency-safe. A hook (`flushedLSN` function) so the WAL rule can be enforced later. Design doc `04-buffer-pool.md`.
**Acceptance:** Model-based tests; concurrent pin/unpin under `-race` with `-count=20`; error when all frames are pinned; eviction writes dirty pages.

### ✅ Step 1.4 — Slotted pages and heap tables
**Goal:** Store variable-length rows in pages.
**Scope:** Slotted page layout (slot directory + tuple data), insert/get/update/delete, in-page compaction, record ID = (page ID, slot). Heap file spanning many pages with a simple free-space map. Full table scan iterator. Design doc `05-heap-storage.md`.
**Acceptance:** Model-based tests with random inserts/updates/deletes of random sizes; rows survive reopen; fuzz target for slotted-page decoding.

---

## Phase 2 — Write-ahead log and recovery

### ✅ Step 2.1 — WAL writer
**Scope:** WAL record format (LSN, length, type, CRC-32C, payload), segment files, append, flush with `fsync`, current and flushed LSN tracking. Design doc `06-wal.md`.
**Acceptance:** Records round-trip; segment rollover tested; flushed LSN only advances after `Sync`.

### ✅ Step 2.2 — WAL reader
**Scope:** Sequential reader across segments, detection of torn or corrupt tail records (stop cleanly at the last valid record).
**Acceptance:** Torn-write crash tests with `MemFS`; fuzz target for record decoding.

### ✅ Step 2.3 — Logging heap changes
**Scope:** Every heap change writes a WAL record first; pages carry the LSN of their last change; buffer pool enforces the WAL rule before evicting or flushing a page.
**Acceptance:** Tests prove no page reaches disk before its WAL record is durable.

### 👉 Step 2.4 — Checkpoints
**Scope:** Checkpoint record, flushing dirty pages, recording the redo start point, old WAL segment cleanup. Design doc `07-checkpoints-recovery.md`.
**Acceptance:** Recovery after a checkpoint replays only what is needed; old segments removed safely.

### ⬜ Step 2.5 — Crash recovery and crash test harness
**Scope:** Redo recovery on startup. Crash test harness in `tests/crash/`: random operations, crash at random points with `MemFS`, recover, verify every acknowledged change is present and nothing partial is visible. Also an OS-level script that kills the real process. `make crashtest` wired up.
**Acceptance:** Thousands of seeded crash runs pass; failing seeds are reproducible.

---

## Phase 3 — B+Tree indexes

### ⬜ Step 3.1 — Node format and search
Node layout for internal and leaf pages, key encoding that sorts correctly as bytes, point lookup. Design doc `08-btree.md`.

### ⬜ Step 3.2 — Insert with splits
Leaf and internal splits, new root creation.

### ⬜ Step 3.3 — Delete with merge and redistribution
Underflow handling, root collapse.

### ⬜ Step 3.4 — Range scans
Iterators over leaf chains, forward scans with start/end bounds.

### ⬜ Step 3.5 — Concurrency, WAL, and crash safety
Latch crabbing for concurrent access, WAL logging of index changes, crash tests.

**Phase 3 acceptance:** Model-based tests against a sorted map with millions of random operations; concurrent tests under `-race`; crash tests pass; benchmarks recorded.

---

## Phase 4 — SQL

### ⬜ Step 4.1 — Lexer
Tokens for keywords, identifiers (PostgreSQL case rules), string and numeric literals, operators, comments. Fuzz target.

### ⬜ Step 4.2 — Parser and AST
`CREATE TABLE`, `DROP TABLE`, `CREATE INDEX`, `INSERT`, `SELECT ... WHERE ... ORDER BY ... LIMIT`, `UPDATE`, `DELETE`. Clear syntax errors with position. Fuzz target. Design doc `09-sql-frontend.md`.

### ⬜ Step 4.3 — Types and values
`INTEGER`, `BIGINT`, `DOUBLE PRECISION`, `TEXT`, `BOOLEAN`, `TIMESTAMPTZ`, SQL `NULL` semantics (three-valued logic), row encoding into heap tuples.

### ⬜ Step 4.4 — Catalog
Tables, columns, and indexes stored in system tables on disk, loaded at startup.

### ⬜ Step 4.5 — Executor
Iterator (Volcano) model: sequential scan, index scan, filter, projection, sort, limit, insert, update, delete. Design doc `10-executor.md`.

### ⬜ Step 4.6 — SQL logic tests
In-house sqllogictest runner (no dependencies), initial test files covering every supported statement, `make sqltest` wired up.

---

## Phase 5 — PostgreSQL wire protocol (MVP)

### ⬜ Step 5.1 — Startup and authentication
TCP listener, `SSLRequest`/`GSSENCRequest` declined, startup message, trust authentication, `ParameterStatus`, `BackendKeyData`, `ReadyForQuery`. Design doc `11-wire-protocol.md`.

### ⬜ Step 5.2 — Simple query protocol
`Query` → `RowDescription`, `DataRow`, `CommandComplete`, `ErrorResponse` with correct SQLSTATE codes, `EmptyQueryResponse`. Type OIDs for supported types.

### ⬜ Step 5.3 — Extended query protocol
`Parse`, `Bind`, `Describe`, `Execute`, `Sync`, `Close`, `Flush`. Text-format parameters first. Needed for parameterised queries from drivers.

### ⬜ Step 5.4 — Connections and end-to-end tests
Goroutine per connection, connection limit, graceful shutdown on signal, cancel requests. End-to-end tests using a minimal protocol client written in the test code, plus manual verification steps with `psql`.

### 🎯 MVP milestone
`psql -h localhost -p 5433` connects; tables can be created, filled, queried, updated, and deleted; data survives crashes.

---

## Later phases (detailed when we get there)

- **Phase 6 — Transactions and MVCC:** `BEGIN`/`COMMIT`/`ROLLBACK`, 64-bit transaction IDs, snapshots, in-place updates with an undo log, automatic undo cleanup (no VACUUM), lock manager with deadlock reporting, undo during recovery
- **Phase 7 — Planner:** joins, aggregates, `GROUP BY`, statistics, cost-based plan choice, plan hints
- **Phase 8 — Query insights and benchmarks:** slow-query capture, metrics endpoint, `pgbench` comparisons with PostgreSQL
- **Phase 9 — Compatibility:** `pg_catalog` and `information_schema` so Prisma, TypeORM, and Drizzle work
- **Phase 10 — Operations:** online schema changes, online index builds, backups and point-in-time restore
- **Phase 11 — Data features:** change streams, audit log, temporal tables, data deletion for privacy laws
