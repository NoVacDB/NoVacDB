package crash

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/vikrant-choudhary06/NoVacDB/internal/vfs"
	"github.com/vikrant-choudhary06/NoVacDB/internal/wal"
)

// The OS-level crash test re-runs this test binary as a worker process that
// works on a real directory. The parent kills it with SIGKILL at a random
// moment, then recovers the directory itself and checks it.
const (
	workerEnv     = "NOVACDB_CRASH_WORKER"
	workerDirEnv  = "NOVACDB_CRASH_DIR"
	workerSeedEnv = "NOVACDB_CRASH_SEED"
	workerMaxOps  = 20000
)

func TestMain(m *testing.M) {
	if os.Getenv(workerEnv) != "" {
		os.Exit(runWorker())
	}
	os.Exit(m.Run())
}

func workerOptions() wal.EngineOptions {
	return wal.EngineOptions{Frames: 10, WAL: wal.Options{SegmentSize: 16384}}
}

// runWorker is the killed process. It prints "ids a b t" once the heaps (first
// pages a and b) and the tree (root t) are durable, then "ack n" each time the first n operations are durable, and
// "done" if it finishes without being killed.
func runWorker() int {
	seed, err := strconv.ParseUint(os.Getenv(workerSeedEnv), 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker: bad seed:", err)
		return 2
	}
	e, err := wal.OpenEngine(bg, vfs.OSFS{}, os.Getenv(workerDirEnv), workerOptions())
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker: open:", err)
		return 1
	}
	d := &db{e: e}
	id, err := d.create()
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker: heaps and tree:", err)
		return 1
	}
	out := bufio.NewWriter(os.Stdout)
	fmt.Fprintf(out, "ids %d %d %d\n", id.heaps[0], id.heaps[1], id.tree)
	_ = out.Flush()
	w, m := newWorkload(seed), newModel()
	for i := 1; i <= workerMaxOps; i++ {
		o := w.next(m)
		if err := d.do(o); err != nil {
			fmt.Fprintf(os.Stderr, "worker: op %d (%d): %v\n", i, o.kind, err)
			return 1
		}
		m.apply(o)
		if o.kind == opFlush {
			fmt.Fprintf(out, "ack %d\n", i) // operations 1..i are durable
			_ = out.Flush()
		}
	}
	if err := e.Close(bg); err != nil {
		fmt.Fprintln(os.Stderr, "worker: close:", err)
		return 1
	}
	fmt.Fprintln(out, "done")
	_ = out.Flush()
	return 0
}

// killedRun starts a worker, kills it after it has acknowledged acksBeforeKill
// flushes, and returns the heap and tree IDs, the highest acknowledged
// operation count, and whether the worker finished on its own.
func killedRun(t *testing.T, dir string, seed uint64, acksBeforeKill int) (id ids, lastAck int, finished bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), workerEnv+"=1", workerDirEnv+"="+dir, workerSeedEnv+"="+strconv.FormatUint(seed, 10))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(stdout)
	parse := func(line string) {
		f := strings.Fields(line)
		switch {
		case len(f) == numHeaps+2 && f[0] == "ids":
			vals := make([]uint64, numHeaps+1)
			for i := range vals {
				v, err := strconv.ParseUint(f[1+i], 10, 64)
				if err != nil {
					t.Fatalf("bad line %q", line)
				}
				vals[i] = v
			}
			copy(id.heaps[:], vals)
			id.tree = vals[numHeaps]
		case len(f) == 2 && f[0] == "ack":
			n, err := strconv.Atoi(f[1])
			if err != nil {
				t.Fatalf("bad line %q", line)
			}
			lastAck = n
		case len(f) == 1 && f[0] == "done":
			finished = true
		default:
			t.Fatalf("unexpected worker output %q", line)
		}
	}
	acks := 0
	for sc.Scan() {
		parse(sc.Text())
		if strings.HasPrefix(sc.Text(), "ack ") {
			acks++
			if acks == acksBeforeKill {
				break
			}
		}
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatal(err)
	}
	// Lines already written before the kill are still acknowledgements.
	for sc.Scan() {
		parse(sc.Text())
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	if finished {
		if waitErr != nil {
			t.Fatalf("worker finished but exited with %v: %s", waitErr, stderr.String())
		}
	} else if stderr.Len() > 0 {
		t.Fatalf("worker failed before it was killed: %s", stderr.String())
	}
	if id.tree == 0 {
		t.Fatalf("worker never created its heaps and tree (stderr: %s)", stderr.String())
	}
	return id, lastAck, finished
}

// recoverAndMatch recovers dir and returns how many operations of the seed's
// stream the recovered database reflects (at least lastAck), failing if it
// matches no prefix of the stream.
func recoverAndMatch(t *testing.T, dir string, seed uint64, id ids, lastAck int) int {
	t.Helper()
	e, err := wal.OpenEngine(bg, vfs.OSFS{}, dir, workerOptions())
	if err != nil {
		t.Fatalf("seed %d: recovery: %v", seed, err)
	}
	d := &db{e: e}
	rows, err := d.open(id)
	if err != nil {
		t.Fatalf("seed %d: after recovery: %v", seed, err)
	}
	if err := e.Close(bg); err != nil {
		t.Fatalf("seed %d: closing: %v", seed, err)
	}
	// Regenerate the worker's operations and find the prefix that matches.
	w, m := newWorkload(seed), newModel()
	for i := 1; i <= workerMaxOps; i++ {
		if i > lastAck && m.equal(rows) {
			return i - 1
		}
		o := w.next(m)
		m.apply(o)
		if i == lastAck && m.equal(rows) {
			return i
		}
	}
	if m.equal(rows) {
		return workerMaxOps
	}
	t.Fatalf("seed %d: the recovered database matches no prefix of at least %d operations (reproduce with NOVACDB_SEED=%d NOVACDB_OSKILL_RUNS=1)", seed, lastAck, seed)
	return 0
}

func TestOSKill(t *testing.T) {
	base := baseSeed(t)
	runs := envInt(t, "NOVACDB_OSKILL_RUNS", 4)
	if testing.Short() {
		runs = 1
	}
	rng := rand.New(rand.NewPCG(base, 7))
	for i := range runs {
		seed := base + uint64(i)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			dir := t.TempDir() + "/db"
			id, lastAck, finished := killedRun(t, dir, seed, 1+rng.IntN(60))
			got := recoverAndMatch(t, dir, seed, id, lastAck)
			t.Logf("killed after %d acknowledged operations; recovered %d (finished=%v)", lastAck, got, finished)
			// A second, clean reopen sees the same state.
			if again := recoverAndMatch(t, dir, seed, id, got); again != got {
				t.Fatalf("second recovery saw %d operations, first saw %d", again, got)
			}
		})
	}
}
