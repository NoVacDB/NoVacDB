# 11 — SQL Logic Tests and the SQL Crash Workload

Status: **Implemented (Step 4.6).** Written under the standing autonomous-mode instruction.

## 1. Problem

The executor's unit tests check its parts; nothing yet checks whole SQL scripts the way a user runs them, in a form that is easy to read and extend as more SQL arrives. And the crash harness (07-checkpoints-recovery.md section 2.7) drives heaps and B+Trees directly, so it does not prove the promise SQL users rely on: after any crash, every acknowledged statement is present, and every other statement is entirely present or entirely absent, its rows, index entries and catalog changes included.

## 2. Design

### 2.1 Test files (`tests/sqllogic`)

A dialect of SQLite's sqllogictest format, run by an in-house runner that uses only the standard library. A file is a list of records separated by blank lines; `#` starts a comment line.

```
statement ok [TAG]
SQL...

statement error CODE [message prefix]
SQL...

query TYPES [nosort|rowsort|valuesort]
SQL...
----
result lines

restart
crash
```

- `statement ok` runs the SQL (one or more statements) and requires success; with a tag, the last statement's command tag must equal it (`statement ok INSERT 0 2`).
- `statement error` requires the SQLSTATE `CODE` and, if given, a message beginning with the rest of the line.
- `query` runs one statement. `TYPES` has one letter per output column, which must match: `I` integer or bigint, `R` double precision, `T` text, `B` boolean, `D` timestamptz. Each result line is a row, its values separated by `|`, in PostgreSQL's text form, with `NULL` for NULL and `(empty)` for the empty string. `rowsort` sorts the rows before comparing (for queries without `ORDER BY`); `valuesort` puts one value per line and sorts the values.
- `restart` closes the database cleanly and opens it again; `crash` cuts the power (a MemFS crash with a torn last write) and opens it again, so files check that everything survives both.

Each file runs against a fresh database on a MemFS, with `now()` fixed at 2024-05-06 07:08:09.5 UTC. The runner stops at the first mismatch and reports `file:line` and what differed. `make sqltest` runs every `testdata/*.test`; they also run with `make test`.

The initial files cover every supported statement: `types.test` (literals, input and output forms, casts), `ddl.test`, `writes.test` (INSERT, UPDATE, DELETE, defaults, conversions, constraints), `select.test`, `expressions.test` (operators, three-valued logic, predicates, CASE, functions), `indexes.test` (queries answered through indexes, across special values, updates, restarts and crashes) and `statements.test` (syntax errors and several statements in one string). Expected results are PostgreSQL 16's, with the documented exceptions of 10-executor.md.

### 2.2 SQL crash workload (`tests/crash/sql_test.go`)

Each seeded scenario opens a database on a MemFS with a random pool size, WAL segment size and checkpoint interval, creates a table with a primary key and a secondary index, and runs cycles of random statements against a model: multi-row `INSERT`s, range `UPDATE`s that change indexed columns, `UPDATE`s that swap two primary keys (valid only at the statement's end), multi-row `DELETE`s, and DDL (`CREATE`/`DROP INDEX`, `CREATE`/`DROP TABLE` of a second table with rows).

In most cycles an I/O fault (write, sync, open, rename, remove or truncate) is injected at a random point. A statement it stops may or may not have committed. Half the time a second fault makes the database's self-restart fail too, which leaves it exactly in the middle of the statement, as a crash would. Then comes a process kill, a power cut, or a power cut with a torn last write, and recovery. The recovered database must equal the model after the acknowledged statements, or, if a statement was stopped, after that statement too. It is read through the table, the primary key and every index, and each optional index's and table's existence is checked.

## 3. Formats

The test file format is section 2.1. Nothing is stored on disk.

## 4. Concurrency

None: each file and scenario runs alone. (Concurrent statements are tested in the executor's package.)

## 5. Failure & crash

A failing file reports the file, line, and the difference. A failing crash scenario reports its seed; `NOVACDB_SEED` reproduces it, and `NOVACDB_CRASH_RUNS` sets the number of scenarios (a third of the storage-level count; `make crashtest` runs 1000).

## 6. Alternatives

- **SQLite's sqllogictest corpus.** Most of it needs joins, aggregates, subqueries and views, which arrive in Phase 7, and its results follow SQLite's semantics in places where PostgreSQL's differ. Files written for NoVacDB check PostgreSQL's behaviour.
- **One value per line, as the original format prints.** Rows on one line, `|`-separated, are much easier to read and review; no test value contains `|`.
- **Checking results against a real PostgreSQL in CI.** It would add a dependency and a server to the test loop; the expected results here were written from PostgreSQL's documented behaviour instead.

## 7. Testing plan

The runner has tests of its own: each kind of wrong expectation (a value, a missing or extra row, order, column types, the error code, the message, the tag, a row that survived a crash) must fail, and the same expectations written correctly must pass; malformed files are rejected. The crash workload asserts that it exercised enough of every case (DDL, fault stops, mid-statement crashes, lost in-flight statements, torn crashes, kills).

## 8. Limitations

- Results are compared as text; floating-point results must be written exactly as PostgreSQL prints them.
- There is no `hash-threshold` or result hashing; files list every row.
- The crash workload runs on MemFS only; the SIGKILL test of real processes (07-checkpoints-recovery.md) still drives the storage layer directly.
