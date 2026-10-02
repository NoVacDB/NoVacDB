# Design documents

One document per component, named `NN-component-name.md`. A design is written
and reviewed before the matching code is written (see
[WORKFLOW.md](../../WORKFLOW.md#3-development-workflow)).

Each document has these sections: Problem, Design, Formats, Concurrency,
Failure & crash, Alternatives, Testing plan, Limitations.

| Doc | Component | Status |
|---|---|---|
| [01](01-vfs.md) | Virtual file system (`internal/vfs`) | Implemented |
| [02](02-page-format.md) | Page format (`internal/storage`) | Implemented |
| [03](03-disk-manager.md) | Disk manager (`internal/storage`) | Implemented |
| [04](04-buffer-pool.md) | Buffer pool (`internal/storage`) | Implemented |
| [05](05-heap-storage.md) | Slotted pages and heap tables (`internal/storage`) | Implemented |
| [06](06-wal.md) | WAL writer and reader (`internal/wal`) | Implemented |
| [07](07-checkpoints-recovery.md) | Logging heap changes, checkpoints, crash recovery (`internal/storage`, `internal/wal`) | Implemented |
| [08](08-btree.md) | B+Tree indexes (`internal/btree`, `internal/wal`) | Implemented |
| [09](09-sql-frontend.md) | SQL lexer, parser and errors (`internal/sql/parser`, `internal/sql/sqlerr`) | In progress (4.1 implemented) |
