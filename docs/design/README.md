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
