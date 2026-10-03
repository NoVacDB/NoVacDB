# NoVacDB — Build Progress

This file is the step-by-step build plan. Each step is sized for roughly one focused session. Work happens **one step at a time, in order**.

**Status legend:** ✅ Done · 🚧 In progress · ⬜ Not started · 👉 **CURRENT**

---

## Current step

👉 **Phase 6 — Transactions and MVCC** (steps to be planned: the phase is outlined under "Later phases" and gets its step list and design doc first)

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
| 2026-10-01 | 2.4 Checkpoints | ✅ Done | Checkpointer (redo point, flush, sync, record, atomic control file, segment trimming), RedoStart. Found and fixed a lost-update race from 2.3 (pages now dirty before their record is appended). 10 deliberate-bug checks. |
| 2026-10-01 | 2.5 Crash recovery and harness | ✅ Done | wal.Engine (open, recover, end-of-recovery checkpoint, logged heaps). tests/crash: MemFS harness checking recovered state is an exact operation prefix ≥ acknowledged, plus real SIGKILL test; `make crashtest`. Phase 2 complete. |
| 2026-10-01 | 3.1 Node format and search | ✅ Done | Design doc `08-btree.md` for all of Phase 3. `internal/btree`: memcomparable key encoding (NULL, bool, int64, float64, bytes) with strict `DecodeKey`, node format with checked accessors, binary search, compaction, `Validate`; `Create`/`Open`/`Get`/`Check`. 2 fuzz targets; 38 deliberate-bug checks (1 equivalent). Fuzz minimisation capped at 5s in `make fuzz`. |
| 2026-10-01 | 3.2 Insert with splits | ✅ Done | Top-down preemptive splits (a split never propagates up), root split in place so the root page never moves, splits built in scratch so a failed split changes nothing. Model tests (3.1M inserts in a long run, heights up to 6), max-size keys, sequential orders. 16 deliberate-bug checks (2 equivalent). |
| 2026-10-01 | 3.3 Delete with merge and redistribution | ✅ Done | Latch crabbing (only the unsafe part of the path stays latched; 1.01 latches at the leaf on average), bottom-up merge or redistribution with a sibling, in-place root collapse, freed pages reused. Found and fixed a latch leak in root collapse. Model tests (3.9M mixed operations in a long run), every repair path counted and required, crash-leftover shapes, corrupt trees. 34 deliberate-bug checks (3 equivalent). |
| 2026-10-01 | 3.4 Range scans | ✅ Done | Iterator with inclusive/exclusive/unbounded bounds that holds nothing between calls: it copies one leaf per descent and finds the next leaf through the upper fence, so leaves need no sibling links (the plan's "leaf chains" replaced, see 08-btree.md). One descent per leaf; strictly increasing keys under concurrent writes. 17 deliberate-bug checks (1 equivalent). |
| 2026-10-01 | 3.5 Concurrency, WAL, and crash safety | ✅ Done | WAL record type 3 (leaf operations physiologically, structural changes as page images), rollback on log failure, deferred frees released by checkpoints, `Engine.CreateBTree`/`OpenBTree` and replay. Concurrent writers, readers and scanners under `-race` (20 repetitions); crash harness extended with a three-level B+Tree (3,000 MemFS scenarios, 30 SIGKILL runs); benchmarks recorded in `08-btree.md`; `make stress`. 33 deliberate-bug checks. Phase 3 complete. |
| 2026-10-02 | 4.1 Lexer | ✅ Done | Design doc `09-sql-frontend.md` (lexer, parser, errors). `internal/sql/sqlerr` (SQLSTATE errors with character positions and hints) and the lexer in `internal/sql/parser`: PostgreSQL identifier folding and quoting, keyword categories, standard/escape/dollar-quoted strings with newline continuation, PostgreSQL 16 number forms with trailing-junk errors, parameters, operator cutting, nested comments. Names over 63 bytes are rejected, not truncated. FuzzLex; 33 deliberate-bug checks (2 equivalent). |
| 2026-10-02 | 4.2 Parser and AST | ✅ Done | Recursive-descent parser for CREATE/DROP TABLE, CREATE/DROP INDEX, INSERT, SELECT (WHERE, ORDER BY, LIMIT/OFFSET, DISTINCT), UPDATE, DELETE; PostgreSQL precedence, keyword categories, typed literals, CASE, casts, parameters; `0A000` with position for recognised but unsupported syntax. `internal/sql/ast` prints every tree back to SQL; FuzzParse checks parse → print → parse. 36 deliberate-bug checks (2 equivalent, removed as redundant code). |
| 2026-10-02 | 4.3 Types and values | ✅ Done | Design doc `10-executor.md` (types, rows, catalog, atomic statements, executor). `internal/sql/types`: integer, bigint, double precision, text, boolean, timestamptz with PostgreSQL's input/output forms, three-valued logic, overflow-checked arithmetic, casts, LIKE; row (tuple) encoding and order-preserving index keys. Arithmetic checked against math/big on 200k random operand pairs; key order equals Compare order. Found and fixed a double-rounding bug in fractional seconds. 2 fuzz targets; 46 deliberate-bug checks (2 equivalent). |
| 2026-10-02 | 4.4 Catalog | ✅ Done | Atomic statements: WAL statement-begin/commit records (types 4, 5), "no steal" through the existing WAL-rule hook, two-pass recovery that discards unfinished groups, `Engine.Abandon`. `internal/catalog`: tables, columns and indexes in three system heaps plus a once-written catalog file; PostgreSQL index naming; DDL whose `*sqlerr.Error` failures change nothing. Randomized DDL-and-crash test against a model (1000 seeded runs) and statement crash test (1500 runs); injected I/O failure at every point of a DDL statement; 87 deliberate-bug checks (1 equivalent piece of code removed). Fixed a Phase 3 B+Tree concurrency test that failed without `-race`. |
| 2026-10-02 | 4.5 Executor | ✅ Done | `internal/sql/executor`: `Open`/`Exec`/`Close`, binding with PostgreSQL's type resolution and error codes, functions, sequential and index scans (planner picks the longest equality prefix plus a range), sort, DISTINCT, LIMIT/OFFSET, two-phase INSERT/UPDATE/DELETE with NOT NULL and end-of-statement uniqueness, DDL, self-restart after failures, checkpoints by WAL growth. Index plans checked against sequential plans on random data (3000 queries × 31 seeds) and by a randomized statement generator (600k statements, 150 seeds); I/O fault at every point of six statement kinds; fuzz target; 73 deliberate-bug checks (4 equivalent); coverage 96%. |
| 2026-10-02 | 4.6 SQL logic tests | ✅ Done | `tests/sqllogic`: dependency-free runner for a sqllogictest dialect (statement ok/error with SQLSTATE and message, typed queries with sort modes, `restart` and `crash` directives), 7 files with 250 records covering every supported statement, `make sqltest`. Writing expectations from PostgreSQL's behaviour found two differences, fixed: `true::text` and the aliased-table error. `tests/crash/sql_test.go`: multi-row statements and DDL all-or-nothing across kills, power cuts and torn writes, including crashes in the middle of a statement (4000 scenarios, ~197k statements, ~9.9k crashes, all matched). Design doc `11-sql-logic-tests.md`. |
| 2026-10-02 | B+Tree revision 2 (review) | ✅ Done | Design doc 08 revised first and approved. Leaf cells gain a Flags byte (zero until Phase 6's delete marks), data file format version 2. Deferred frees are logged (WAL type 6), logged again by checkpoints and replayed by recovery, so they survive crashes; a scavenger is on the roadmap for the remaining leak windows. Every index entry, unique or not, now carries the RID; uniqueness by prefix probe. Deadlock-freedom argument corrected (repairs latch a left sibling after the node); new test aimed at left-sibling repairs under `-race` with a watchdog. A checkpoint failing at every write, sync and rename point never frees a page twice or a live page. Limitations and Phase 4 notes added. |
| 2026-10-02 | 5.1 Startup and authentication | ✅ Done | Design doc `12-wire-protocol.md` (whole of Phase 5, reviewed and approved). `internal/pgwire`: framing with size limits (startup 10,000 bytes, messages 16 MiB, large bodies allocated as they arrive), startup packet, backend messages. `internal/server`: one goroutine per connection, SSL/GSS requests declined, protocol 3.0 with negotiation down from 3.x, PostgreSQL's startup-parameter rules, trust authentication, ParameterStatus, cancel keys, startup timeout, panic containment. `novacdb` now opens the database and serves on localhost:5433, stopping cleanly on SIGINT/SIGTERM. Verified with real `psql` 16 and `pg_isready`. Coverage 93% / 99%; 2 fuzz targets; 35 deliberate-bug checks, all caught. |
| 2026-10-02 | 5.2 Simple query protocol | ✅ Done | Design section 2.8 of `12-wire-protocol.md`. `Query` runs through the executor: `RowDescription` with PostgreSQL's type OIDs and sizes, `DataRow` in text form (NULL as −1), `CommandComplete`, `EmptyQueryResponse`, `ErrorResponse` with SQLSTATE, position, detail and hint, notices with their codes; results sent in 64 KiB writes; extended-protocol messages answered with `0A000` and skipped to `Sync`. All 250 SQL logic records now also run over the wire. Found and fixed: `SELECT FROM t` lost its rows, select lists past 65,535 columns corrupted the stream (now PostgreSQL's 1664 limit, `54011`), and rows over 488 columns crashed the row encoder (a Step 4.3 bug). Verified with real `psql` 16. 29 deliberate-bug checks, all caught. |
| 2026-10-02 | 5.3 Extended query protocol | ✅ Done | Design section 2.10 of `12-wire-protocol.md`. Executor `Prepare`/`ExecPrepared`: parameter types declared or inferred as untyped literals' would be, result columns described without running, parameters bound as constants (indexes used), "cached plan must not change result type" after schema changes. Server: Parse/Bind/Describe/Execute/Close/Flush/Sync, named and unnamed statements and portals, row limits with PortalSuspended, PostgreSQL's error codes with skip-to-Sync, binary formats for the six types (and declared smallint, real, varchar parameters). Found and fixed: an empty string first in a result could be sent as NULL. SQL logic files also run through the extended protocol with binary results; `FuzzPrepare`, `FuzzExtendedMessages`, `FuzzSession` (no contained panic for any input). Verified with `psql` `\bind`, `pgbench` in all three modes, pgx in all five execution modes, and JDBC 42.7. 63 deliberate-bug checks: 58 caught, 1 found redundant code (removed), 4 equivalent (lock mode, allocation bound, flush timing). |
| 2026-10-02 | 5.4 Connections and end-to-end tests | ✅ Done | Design section 2.12. `--max-connections` (53300), `CancelRequest` with constant-time key check (57014, the session goes on), `--idle-timeout` (57P05), graceful shutdown on SIGINT/SIGTERM: idle sessions 57P01, starting ones 57P03, running statements finish up to `--shutdown-timeout`, a second signal stops at once, then the final checkpoint. Found by repetition and fixed: a connection woken by shutdown before its startup deadline was set waited out the whole timeout. `tests/e2e`: the real binary (race-detector build) driven by a protocol client and `psql`: the MVP flow, graceful restarts, and three SIGKILLs under concurrent writes with every acknowledged row recovered and indexes consistent. Cancel verified with pgx against a 300k-row sort. 2000 concurrent sessions measured at about 17 KB each on 9 OS threads (WORKFLOW problem 12 marked done). 25 deliberate-bug checks, all caught. Phase 5 and the MVP complete. |

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

### ✅ Step 2.4 — Checkpoints
**Scope:** Checkpoint record, flushing dirty pages, recording the redo start point, old WAL segment cleanup. Design doc `07-checkpoints-recovery.md`.
**Acceptance:** Recovery after a checkpoint replays only what is needed; old segments removed safely.

### ✅ Step 2.5 — Crash recovery and crash test harness
**Scope:** Redo recovery on startup. Crash test harness in `tests/crash/`: random operations, crash at random points with `MemFS`, recover, verify every acknowledged change is present and nothing partial is visible. Also an OS-level script that kills the real process. `make crashtest` wired up.
**Acceptance:** Thousands of seeded crash runs pass; failing seeds are reproducible.

---

## Phase 3 — B+Tree indexes

### ✅ Step 3.1 — Node format and search
Node layout for internal and leaf pages, key encoding that sorts correctly as bytes, point lookup. Design doc `08-btree.md`.

### ✅ Step 3.2 — Insert with splits
Leaf and internal splits, new root creation.

### ✅ Step 3.3 — Delete with merge and redistribution
Underflow handling, root collapse.

### ✅ Step 3.4 — Range scans
Iterators over leaf chains, forward scans with start/end bounds.

### ✅ Step 3.5 — Concurrency, WAL, and crash safety
Latch crabbing for concurrent access, WAL logging of index changes, crash tests.

**Phase 3 acceptance:** Model-based tests against a sorted map with millions of random operations; concurrent tests under `-race`; crash tests pass; benchmarks recorded.

---

## Phase 4 — SQL

### ✅ Step 4.1 — Lexer
Tokens for keywords, identifiers (PostgreSQL case rules), string and numeric literals, operators, comments. Fuzz target.

### ✅ Step 4.2 — Parser and AST
`CREATE TABLE`, `DROP TABLE`, `CREATE INDEX`, `INSERT`, `SELECT ... WHERE ... ORDER BY ... LIMIT`, `UPDATE`, `DELETE`. Clear syntax errors with position. Fuzz target. Design doc `09-sql-frontend.md`.

### ✅ Step 4.3 — Types and values
`INTEGER`, `BIGINT`, `DOUBLE PRECISION`, `TEXT`, `BOOLEAN`, `TIMESTAMPTZ`, SQL `NULL` semantics (three-valued logic), row encoding into heap tuples.

### ✅ Step 4.4 — Catalog
Tables, columns, and indexes stored in system tables on disk, loaded at startup.

### ✅ Step 4.5 — Executor
Iterator (Volcano) model: sequential scan, index scan, filter, projection, sort, limit, insert, update, delete. Design doc `10-executor.md`.

### ✅ Step 4.6 — SQL logic tests
In-house sqllogictest runner (no dependencies), initial test files covering every supported statement, `make sqltest` wired up.

---

## Phase 5 — PostgreSQL wire protocol (MVP)

### ✅ Step 5.1 — Startup and authentication
TCP listener, `SSLRequest`/`GSSENCRequest` declined, startup message, trust authentication, `ParameterStatus`, `BackendKeyData`, `ReadyForQuery`. Design doc `12-wire-protocol.md`.

### ✅ Step 5.2 — Simple query protocol
`Query` → `RowDescription`, `DataRow`, `CommandComplete`, `ErrorResponse` with correct SQLSTATE codes, `EmptyQueryResponse`. Type OIDs for supported types.

### ✅ Step 5.3 — Extended query protocol
`Parse`, `Bind`, `Describe`, `Execute`, `Sync`, `Close`, `Flush`. Text-format parameters first. Needed for parameterised queries from drivers.

### ✅ Step 5.4 — Connections and end-to-end tests
Goroutine per connection, connection limit, graceful shutdown on signal, cancel requests. End-to-end tests using a minimal protocol client written in the test code, plus manual verification steps with `psql`.

### ✅ 🎯 MVP milestone
`psql -h localhost -p 5433` connects; tables can be created, filled, queried, updated, and deleted; data survives crashes. Reached: `tests/e2e` runs these steps against the real binary, with `psql` and with a protocol client, across graceful restarts and SIGKILLs.

---

## Later phases (detailed when we get there)

- **Phase 6 — Transactions and MVCC:** `BEGIN`/`COMMIT`/`ROLLBACK`, 64-bit transaction IDs, snapshots, in-place updates with an undo log, automatic undo cleanup (no VACUUM), lock manager with deadlock reporting, undo during recovery; index entries delete-marked through the leaf Flags byte and removed by the undo purge (08-btree.md section 2.11)
- **Phase 7 — Planner:** joins, aggregates, `GROUP BY`, statistics, cost-based plan choice, plan hints
- **Phase 8 — Query insights and benchmarks:** slow-query capture, metrics endpoint, `pgbench` comparisons with PostgreSQL
- **Phase 9 — Compatibility:** `pg_catalog` and `information_schema` so Prisma, TypeORM, and Drizzle work
- **Phase 10 — Operations:** online schema changes, online index builds, backups and point-in-time restore; **page scavenger**: find allocated pages reachable from no catalog heap, user heap, index or pending free (leaked by the crash windows of 08-btree.md section 2.7) and free them, with no statement open
- **Phase 11 — Data features:** change streams, audit log, temporal tables, data deletion for privacy laws
