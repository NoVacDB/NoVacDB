// Command novacdb is the NoVacDB server: a relational database that speaks the
// PostgreSQL wire protocol.
//
// Usage:
//
//	novacdb [--data-dir ./data] [--listen localhost] [--port 5433]
//
// It opens (or creates) the database in the data directory, recovering it
// after a crash, and serves PostgreSQL protocol connections until SIGINT
// or SIGTERM. The entry point in main.go stays thin on purpose: it hands
// off to run, so that the whole life of the server can be tested without
// spawning a process.
package main
