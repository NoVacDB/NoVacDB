package crash

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
	"github.com/vikrant-choudhary06/NoVacDB/internal/wal"
)

const memDir = "/db"

// scenarioStats counts what a scenario exercised, so the test can prove it is
// not passing vacuously.
type scenarioStats struct {
	crashes, faultStops, lostOps, checkpoints, torn, kills int
}

// runScenario runs one seeded crash scenario: several cycles of random work,
// a crash, and recovery checked against the model.
func runScenario(t *testing.T, seed uint64) (st scenarioStats) {
	t.Helper()
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("seed %d (reproduce with NOVACDB_SEED=%d NOVACDB_CRASH_RUNS=1): %s", seed, seed, fmt.Sprintf(format, args...))
	}
	ctl := rand.New(rand.NewPCG(seed, 99)) // crash points and options, separate from the workload stream
	m := vfs.NewMemFS(seed)
	opts := wal.EngineOptions{
		Frames: 2 + ctl.IntN(14),
		WAL:    wal.Options{SegmentSize: []int64{4096, 16384, 1 << 20}[ctl.IntN(3)]},
	}
	w := newWorkload(seed)

	e, err := wal.OpenEngine(bg, m, memDir, opts)
	if err != nil {
		fail("creating the database: %v", err)
	}
	d := &db{e: e}
	firsts, err := d.createHeaps()
	if err != nil {
		fail("creating heaps: %v", err)
	}
	acked := newModel()

	for cycle := range 1 + ctl.IntN(4) {
		if ctl.IntN(10) < 6 {
			faultOps := []vfs.Op{vfs.OpWriteAt, vfs.OpSync, vfs.OpOpenFile, vfs.OpSyncDir, vfs.OpRename, vfs.OpRemove, vfs.OpReadAt, vfs.OpTruncate}
			m.InjectError(vfs.Fault{Op: faultOps[ctl.IntN(len(faultOps))], After: ctl.IntN(120)})
		}
		// states[i] is the model after the first i operations since the
		// last acknowledgement; recovery must land on one of them.
		cur := acked.clone()
		states := []*model{cur.clone()}
		for range 20 + ctl.IntN(250) {
			o := w.next(cur)
			err := d.do(o)
			switch o.kind {
			case opFlush:
				if err == nil {
					acked, states = cur.clone(), []*model{cur.clone()}
				}
			case opCheckpoint:
				if err == nil {
					st.checkpoints++
				}
			default:
				// Even a failed operation may have been logged: its record
				// can be durable although the call reported an error.
				cur.apply(o)
				states = append(states, cur.clone())
			}
			if err != nil {
				st.faultStops++
				break
			}
		}

		m.ClearFaults()
		switch k := ctl.IntN(4); k {
		case 0:
			st.kills++ // process killed: the OS keeps everything it was given
		case 1:
			m.Crash(vfs.CrashOptions{})
		default:
			st.torn++
			m.Crash(vfs.CrashOptions{TearLast: true})
		}
		st.crashes++

		e2, err := wal.OpenEngine(bg, m, memDir, opts)
		if err != nil {
			fail("cycle %d: recovery failed: %v", cycle, err)
		}
		d = &db{e: e2}
		rows, err := d.openHeaps(firsts)
		if err != nil {
			fail("cycle %d: after recovery: %v", cycle, err)
		}
		match := -1
		for i := len(states) - 1; i >= 0; i-- {
			if states[i].equal(rows) {
				match = i
				break
			}
		}
		if match < 0 {
			total := 0
			for _, h := range rows {
				total += len(h)
			}
			fail("cycle %d: recovered %d rows matching none of the %d states since the last acknowledgement (acknowledged state has %d rows)",
				cycle, total, len(states), acked.rowCount())
		}
		if match < len(states)-1 {
			st.lostOps++
		}
		// What recovery kept is durable now (it ended with a checkpoint).
		// The next cycle continues from it with a fresh operation stream.
		acked = states[match].clone()
		w = newWorkload(seed + uint64(cycle+1)*1_000_003)
	}
	if err := d.e.Close(bg); err != nil && !errors.Is(err, wal.ErrClosed) {
		fail("final close: %v", err)
	}
	return st
}

func TestCrashRecoveryMemFS(t *testing.T) {
	base := baseSeed(t)
	runs := envInt(t, "NOVACDB_CRASH_RUNS", 300)
	if testing.Short() {
		runs = min(runs, 40)
	}
	var total scenarioStats
	for i := range runs {
		st := runScenario(t, base+uint64(i))
		total.crashes += st.crashes
		total.faultStops += st.faultStops
		total.lostOps += st.lostOps
		total.checkpoints += st.checkpoints
		total.torn += st.torn
		total.kills += st.kills
	}
	t.Logf("%d runs: %+v", runs, total)
	if runs >= 100 {
		for name, n := range map[string]int{
			"crashes stopped by an injected fault":  total.faultStops,
			"crashes that lost unacknowledged work": total.lostOps,
			"checkpoints":                           total.checkpoints, "torn crashes": total.torn, "process kills": total.kills,
		} {
			if n < runs/10 {
				t.Errorf("only %d %s in %d runs: the harness is not exercising enough", n, name, runs)
			}
		}
	}
}
