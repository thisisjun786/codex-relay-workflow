package testsupport_test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The shape of the ETXTBSY regression CRW-929 records, and its envelope: enough forkers and
// copiers to reach the window between a copy's open and its last close, small enough to stay
// inside the limits the issue sets (8 copiers x 40 copies, 64 MiB and one minute).
const (
	copyWriters    = 8
	copyIterations = 40
	copyForkers    = 4
	// copyBudget is the issue's design budget for the exercise. It is measured and reported, never
	// asserted: the exercise runs 320 copies while four goroutines fork, so its wall clock is a
	// property of the host and its load, not of the lock. A hard budget would fail a slow or busy
	// machine whose locking is correct.
	copyBudget     = 2 * time.Second
	copyByteBudget = 64 << 20
	// copyRunaway is what the wall clock is actually asserted against: a ceiling a hang or a
	// pathological slowdown still trips, wide enough that no correct run reaches it.
	copyRunaway = 30 * time.Second
)

// TestCopyBinarySurvivesConcurrentForks is this issue's regression. CopyBinary opens the copy for
// writing and closes it before returning, and a test that runs the copy right after races every
// other test of its binary that forks: the fork copies the write descriptor into the child, the
// child keeps it until its own exec, and Linux refuses to execute a file open for writing with
// ETXTBSY, "text file busy" (golang/go#22315). CopyBinary holds syscall.ForkLock for reading
// across the copy, so no fork lands inside it and every copy runs.
func TestCopyBinarySurvivesConcurrentForks(t *testing.T) {
	t.Parallel()
	busy, written, elapsed := copyRace(t, testsupport.CopyBinary)
	if busy != 0 {
		t.Fatalf("%d of the %d copies could not be run: ETXTBSY", busy, copyWriters*copyIterations)
	}
	if elapsed > copyRunaway {
		t.Fatalf("the exercise took %s, over the %s runaway ceiling: it did not finish", elapsed, copyRunaway)
	}
	t.Logf("locked: %d copies in %s (the issue's design budget is %s), %d bytes, no ETXTBSY", copyWriters*copyIterations, elapsed, copyBudget, written)
}

// The same exercise through the copy the package did before this issue - the same open, copy and
// close, with no lock against a concurrent fork. Whether it loses the race is a property of this
// host and this moment, so the count is reported, never asserted.
func TestACopyWithoutTheLockRacesConcurrentForks(t *testing.T) {
	t.Parallel()
	busy, written, elapsed := copyRace(t, plainCopy)
	t.Logf("unlocked: %d of %d copies hit ETXTBSY in %s (%d bytes written)", busy, copyWriters*copyIterations, elapsed, written)
}

// copyRace copies and runs copyWriters x copyIterations fresh copies of source through copy while
// copyForkers goroutines fork and run a program of their own, and answers how many of those
// executions failed with ETXTBSY, how many bytes of copies were written and how long the exercise
// took.
//
// A fork copies every descriptor that is not close-on-exec into the child, and the child holds
// them until it execs. A path a fork inherited that way is open for writing in another process,
// and Linux refuses to execute a file open for writing with ETXTBSY, "text file busy"
// (golang/go#22315). The forkers here are the concurrent forks; each copier writes a fresh path
// and then runs it.
func copyRace(t *testing.T, copy func(source, path string) error) (busy, written int64, elapsed time.Duration) {
	t.Helper()
	source := forkProgramPath(t)
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if size := info.Size() * copyWriters * copyIterations; size > copyByteBudget {
		t.Fatalf("the exercise would write %d bytes of copies, over the %d it is bounded to", size, copyByteBudget)
	}
	root := t.TempDir()
	stop := make(chan struct{})
	var forkers sync.WaitGroup
	for i := 0; i < copyForkers; i++ {
		forkers.Add(1)
		go func() {
			defer forkers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = exec.Command(source).Run()
			}
		}()
	}
	start := time.Now()
	var writers sync.WaitGroup
	for i := 0; i < copyWriters; i++ {
		writers.Add(1)
		go func(writer int) {
			defer writers.Done()
			for n := 0; n < copyIterations; n++ {
				target := filepath.Join(root, fmt.Sprintf("copy-%d-%d", writer, n))
				if err := copy(source, target); err != nil {
					t.Errorf("copying to %s: %v", target, err)
					return
				}
				atomic.AddInt64(&written, info.Size())
				if err := exec.Command(target).Run(); err != nil {
					if errors.Is(err, syscall.ETXTBSY) {
						atomic.AddInt64(&busy, 1)
						continue
					}
					t.Errorf("running %s: %v", target, err)
					return
				}
			}
		}(i)
	}
	writers.Wait()
	elapsed = time.Since(start)
	close(stop)
	forkers.Wait()
	return busy, written, elapsed
}

// plainCopy is the copy the package did before this issue: the same open, copy and close, with no
// lock against a concurrent fork.
func plainCopy(source, path string) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	_, err = io.Copy(out, in)
	return err
}

// forkProgramPath is the program the forkers run and the copiers copy: a real program, so the file
// every copy produces is one a process can run.
func forkProgramPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true binary to run")
	}
	return path
}
