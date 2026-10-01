// Package vfs is the only way the rest of NoVacDB touches files.
//
// Every read, write, fsync, rename and directory sync goes through the FS and
// File interfaces. OSFS implements them on the real file system for the
// server. MemFS implements them in memory for tests and, unlike a real disk,
// remembers which bytes are durable (fsynced) and which are only volatile, so
// tests can simulate a power cut, a torn write or a failing disk
// deterministically.
//
// See docs/design/01-vfs.md for the design.
package vfs
