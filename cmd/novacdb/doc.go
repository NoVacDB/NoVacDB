// Command novacdb is the NoVacDB server: a relational database that speaks the
// PostgreSQL wire protocol.
//
// This file only holds the package comment. The entry point in main.go stays
// thin on purpose: it parses flags, builds a logger, and hands off to run so
// that the startup logic can be tested without spawning a process.
package main
