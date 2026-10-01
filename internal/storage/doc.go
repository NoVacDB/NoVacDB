// Package storage holds NoVacDB's on-disk page abstractions: the fixed-size
// page format and its checksum (this step), and later the disk manager,
// buffer pool and heap pages.
//
// See docs/design/02-page-format.md for the page format.
package storage
