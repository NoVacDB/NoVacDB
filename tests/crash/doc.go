// Package crash holds NoVacDB's crash and recovery test harness.
//
// The tests run random heap operations through a wal.Engine, acknowledge
// them with Flush, crash at random points (power cuts with or without a torn
// last write, killed processes, injected I/O errors, and in oskill_test.go a
// real process killed with SIGKILL), recover, and check two promises: every
// acknowledged operation is present, and the recovered contents equal the
// state after some number of operations in order (each operation is either
// entirely there or entirely absent, and none appears without the ones
// before it).
//
// A failing run prints its seed; NOVACDB_SEED reproduces it. See
// docs/design/07-checkpoints-recovery.md.
package crash
