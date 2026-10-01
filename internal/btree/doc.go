// Package btree implements B+Tree indexes over buffer-pool pages: an ordered
// map from unique byte-string keys to small values, with point lookups,
// range scans, inserts and deletes that keep the tree balanced, latch-based
// concurrency, and write-ahead logging of every change.
//
// Keys built with the Append functions in key.go sort as bytes in the same
// order as the typed values they encode.
//
// See docs/design/08-btree.md.
package btree
