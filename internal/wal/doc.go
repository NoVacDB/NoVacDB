// Package wal is NoVacDB's write-ahead log.
//
// Every change is described by a log record that must reach disk before the
// changed page may (the WAL rule) and before the transaction is acknowledged
// (the commit rule). This package provides the record format, the segment
// files that hold the log, and the Writer that appends records, flushes them
// with fsync, and tracks what is durable.
//
// An LSN is the byte offset of a record in the log, which is the
// concatenation of the segment files. LSN 0 never names a record.
//
// See docs/design/06-wal.md for the design.
package wal
