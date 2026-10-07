package testsupport_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// asserted: the exercise runs its copies while four goroutines fork, so its wall clock is a
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
	busy, written, attempted, elapsed := copyRace(t, testsupport.CopyBinary)
	if busy != 0 {
		t.Fatalf("%d of the %d copies could not be run: ETXTBSY", busy, attempted)
	}
	t.Logf("locked: %d copies in %s (the issue's design budget is %s), %d bytes, no ETXTBSY", attempted, elapsed, copyBudget, written)
}

// The same exercise through the copy the package did before this issue - the same open, copy and
// close, with no lock against a concurrent fork. Whether it loses the race is a property of this
// host and this moment, so the count is reported, never asserted.
func TestACopyWithoutTheLockRacesConcurrentForks(t *testing.T) {
	t.Parallel()
	busy, written, attempted, elapsed := copyRace(t, plainCopy)
	t.Logf("unlocked: %d of %d copies hit ETXTBSY in %s (%d bytes written)", busy, attempted, elapsed, written)
}

// copyRace copies and runs fresh copies of source through copy while copyForkers goroutines fork
// and run a program of their own, and answers how many of those executions failed with ETXTBSY,
// how many bytes of copies were written, how many copies were attempted and how long the exercise
// took. It fails when the exercise does not finish within copyRunaway, so a hang is this test's
// failure and not the test binary's timeout.
//
// A fork copies every descriptor that is not close-on-exec into the child, and the child holds
// them until it execs. A path a fork inherited that way is open for writing in another process,
// and Linux refuses to execute a file open for writing with ETXTBSY, "text file busy"
// (golang/go#22315). The forkers here are the concurrent forks; each copier writes a fresh path
// and then runs it.
func copyRace(t *testing.T, copy func(source, path string) error) (busy, written, attempted int64, elapsed time.Duration) {
	t.Helper()
	source := forkProgramPath(t)
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	// The program is whatever true(1) this host has, and on a BusyBox system that is the multi-call
	// binary rather than a tiny program. The copy count comes down from the source's own size so the
	// exercise stays inside the byte ceiling below on any host, rather than failing a correct host
	// whose true(1) is large.
	iterations := int64(copyIterations)
	if size := info.Size(); size > 0 {
		if affordable := copyByteBudget / (size * copyWriters); affordable < iterations {
			iterations = affordable
		}
	}
	if iterations < 1 {
		iterations = 1
	}
	attempted = copyWriters * iterations
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
	// The writers never call testing.T: the runaway ceiling below can end the test while one of them
	// is still stuck, and a call on t from a goroutine that outlives its test panics.
	var failuresMu sync.Mutex
	var failures []string
	fail := func(format string, args ...any) {
		failuresMu.Lock()
		defer failuresMu.Unlock()
		failures = append(failures, fmt.Sprintf(format, args...))
	}
	start := time.Now()
	var writers sync.WaitGroup
	for i := 0; i < copyWriters; i++ {
		writers.Add(1)
		go func(writer int) {
			defer writers.Done()
			for n := int64(0); n < iterations; n++ {
				target := filepath.Join(root, fmt.Sprintf("copy-%d-%d", writer, n))
				if err := copy(source, target); err != nil {
					fail("copying to %s: %v", target, err)
					return
				}
				atomic.AddInt64(&written, info.Size())
				if err := exec.Command(target).Run(); err != nil {
					if errors.Is(err, syscall.ETXTBSY) {
						atomic.AddInt64(&busy, 1)
						continue
					}
					fail("running %s: %v", target, err)
					return
				}
			}
		}(i)
	}
	done := make(chan struct{})
	go func() { writers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(copyRunaway):
		// A stuck copy holds syscall.ForkLock, so the forkers are not waited for here: they could not
		// fork again while it is held. The failure is the answer either way.
		close(stop)
		t.Fatalf("the exercise did not finish within the %s runaway ceiling: a copy is stuck", copyRunaway)
	}
	elapsed = time.Since(start)
	close(stop)
	forkers.Wait()
	failuresMu.Lock()
	defer failuresMu.Unlock()
	if len(failures) > 0 {
		t.Fatalf("%s", strings.Join(failures, "; "))
	}
	return busy, written, attempted, elapsed
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

// The helper's callers rely on three properties beyond the lock, and the lock must not change any
// of them: the copy is the source's bytes, it carries the mode the open asks for, and a path that
// already exists is refused rather than overwritten. The install package's writeExecutable pins
// the same three for its callers (execwrite_test.go TestWriteExecutableKeepsItsCallersContract);
// CopyBinary had none, and this issue is the first change to it.
func TestCopyBinaryKeepsItsCallersContract(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	body := []byte("#!/bin/sh\ntrue\n")
	if err := os.WriteFile(source, body, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "absent", "deeper", "copy")
	if err := testsupport.CopyBinary(source, target); err != nil {
		t.Fatalf("copying into a directory that does not exist yet: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("the copy holds %q, want the source's %q", got, body)
	}
	// The mode is the one the open asks for, less whatever this process's umask clears: a fixed
	// expectation would be wrong under a umask that clears the owner's execute bit, so the reference
	// is the same open with the same flags in this process.
	reference := filepath.Join(root, "reference")
	ref, err := os.OpenFile(reference, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if err := ref.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	refInfo, err := os.Stat(reference)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != refInfo.Mode().Perm() {
		t.Fatalf("the copy came out mode %v, want the %v the same open gives in this process", info.Mode().Perm(), refInfo.Mode().Perm())
	}
	if err := testsupport.CopyBinary(source, target); err == nil {
		t.Fatal("a second copy over an existing file was allowed")
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, body) {
		t.Fatalf("the refused second copy changed the file to %q", raw)
	}
}
