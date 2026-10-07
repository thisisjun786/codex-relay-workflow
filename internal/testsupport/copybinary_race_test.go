package testsupport_test

import (
	"bytes"
	"encoding/json"
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
// copiers to reach the window between a copy's open and its last close, inside the limits the
// issue sets (8 copiers, 40 copies, 64 MiB and one minute). The copy count comes down from the
// source program's size, so a host whose true(1) is the large BusyBox multi-call binary still
// runs the exercise.
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
	// copyRunaway bounds the copies and copyForkerStop the forker shutdown inside the child, so a
	// stalled copy or fork fails there with its own message. The two together stay under the issue's
	// one-minute ceiling.
	copyRunaway    = 30 * time.Second
	copyForkerStop = 10 * time.Second
	// copyChildBudget is the parent's own ceiling on the child process, above the child's internal
	// 40 s. It is what makes the exercise terminable rather than merely time-bound: the parent kills
	// the child's whole process group here, so a copy blocked inside CopyBinary while it holds
	// syscall.ForkLock, and the forks that block behind it, cannot hold this package past the ceiling.
	copyChildBudget = 50 * time.Second
)

// The child the two exercise tests re-execute carries the mode it runs, the program to copy and the
// path it writes its result to.
const (
	copyRaceModeEnv   = "CRW929_COPY_RACE_MODE"
	copyRaceSourceEnv = "CRW929_COPY_RACE_SOURCE"
	copyRaceResultEnv = "CRW929_COPY_RACE_RESULT"
)

// copyRaceResult is what the child hands back: the ETXTBSY count, the bytes copied, the copies
// attempted and the wall clock the child measured.
type copyRaceResult struct {
	Busy      int64 `json:"busy"`
	Written   int64 `json:"written"`
	Attempted int64 `json:"attempted"`
	// Forked is how many forker program starts succeeded. A green run whose forkers never started a
	// process would prove nothing about the race, so the parent requires it to be non-zero.
	Forked int64 `json:"forked"`
	// Overlap is how many forks completed while a copy call was open (from before CopyBinary to its
	// return). A fork cannot complete inside the locked window, so Overlap counts the closest observable
	// interval; a green run shows fork pressure was active at the copy, not only before or after it.
	Overlap int64         `json:"overlap"`
	Elapsed time.Duration `json:"elapsed"`
}

// TestCopyBinarySurvivesConcurrentForks is this issue's regression. CopyBinary opens the copy for
// writing and closes it before returning, and a test that runs the copy right after races every
// other test of its binary that forks: the fork copies the write descriptor into the child, the
// child keeps it until its own exec, and Linux refuses to execute a file open for writing with
// ETXTBSY, "text file busy" (golang/go#22315). CopyBinary holds syscall.ForkLock for reading
// across the copy, so no fork lands inside it and every copy runs.
func TestCopyBinarySurvivesConcurrentForks(t *testing.T) {
	t.Parallel()
	result := copyRaceInChild(t, "locked")
	if result.Forked == 0 {
		t.Fatalf("no forker started a process: the exercise did not establish the concurrent-fork pressure it needs")
	}
	if result.Overlap == 0 {
		t.Fatalf("no fork started while a copy was open: the pressure ended before the copies began")
	}
	if result.Busy != 0 {
		t.Fatalf("%d of the %d copies could not be run: ETXTBSY", result.Busy, result.Attempted)
	}
	t.Logf("locked: %d copies in %s (the issue's design budget is %s), %d bytes, %d forks, no ETXTBSY",
		result.Attempted, result.Elapsed, copyBudget, result.Written, result.Forked)
}

// The same exercise through the copy the package did before this issue - the same open, copy and
// close, with no lock against a concurrent fork. Whether it loses the race is a property of this
// host and this moment, so the count is reported, never asserted.
func TestACopyWithoutTheLockRacesConcurrentForks(t *testing.T) {
	t.Parallel()
	result := copyRaceInChild(t, "plain")
	t.Logf("unlocked: %d of %d copies hit ETXTBSY in %s (%d bytes written, %d forks)",
		result.Busy, result.Attempted, result.Elapsed, result.Written, result.Forked)
}

// TestCopyBinaryRaceExercise is the exercise itself. It runs only in the child process the two
// tests above start, and is a no-op in an ordinary package run.
func TestCopyBinaryRaceExercise(t *testing.T) {
	mode := os.Getenv(copyRaceModeEnv)
	if mode == "" {
		t.Skip("the exercise runs in the child process its parent starts")
	}
	source := os.Getenv(copyRaceSourceEnv)
	copyFile := testsupport.CopyBinary
	if mode == "plain" {
		copyFile = plainCopy
	}
	result, err := copyRace(t, copyFile, source)
	raw, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	// The result is written before the busy count is judged, so the parent can report the counts of a
	// failing run rather than only its exit status.
	if err := os.WriteFile(os.Getenv(copyRaceResultEnv), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatalf("%s: %v", mode, err)
	}
	if mode == "locked" && result.Busy != 0 {
		t.Fatalf("%d of the %d copies could not be run: ETXTBSY", result.Busy, result.Attempted)
	}
	t.Logf("%s: %d copies in %s, %d bytes, %d ETXTBSY", mode, result.Attempted, result.Elapsed, result.Written, result.Busy)
}

// copyRaceInChild runs the exercise in a child process of this test binary, in a process group of
// its own, and kills the whole group when the child does not finish inside copyChildBudget. A copy
// blocked inside CopyBinary cannot be stopped from inside the process that is blocked - it holds
// syscall.ForkLock, so the forkers queue behind it - so the bound has to be enforced from outside.
func copyRaceInChild(t *testing.T, mode string) copyRaceResult {
	t.Helper()
	source := forkProgramPath(t)
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "result.json")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestCopyBinaryRaceExercise$", "-test.count=1", "-test.timeout="+copyChildBudget.String())
	cmd.Env = append(os.Environ(),
		copyRaceModeEnv+"="+mode, copyRaceSourceEnv+"="+source, copyRaceResultEnv+"="+resultPath)
	// The child gets its own process group so the kill below reaches the forks it started too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := make(chan struct{})
	timer := time.AfterFunc(copyChildBudget, func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		close(killed)
	})
	waitErr := cmd.Wait()
	timedOut := !timer.Stop()
	if timedOut {
		<-killed
	}
	// Whatever ended the child, every process it started belongs to this test: the group is killed
	// here so a copy or fork the child left running (its own bounds fire before this one) does not
	// outlive the test and its temporary files.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if timedOut {
		t.Fatalf("the exercise did not finish within %s and its process group was killed: a copy or fork is stuck\n%s",
			copyChildBudget, output.String())
	}
	if waitErr != nil {
		t.Fatalf("the exercise failed: %v\n%s", waitErr, output.String())
	}
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("the exercise left no result: %v\n%s", err, output.String())
	}
	var result copyRaceResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("the exercise's result is not readable: %v\n%s", err, output.String())
	}
	return result
}

// copyRace copies and runs fresh copies of source through copy while copyForkers goroutines fork
// and run a program of their own, and answers how many of those executions failed with ETXTBSY,
// how many bytes of copies were written, how many copies were attempted and how long the exercise
// took.
//
// A fork copies every descriptor that is not close-on-exec into the child, and the child holds
// them until it execs. A path a fork inherited that way is open for writing in another process,
// and Linux refuses to execute a file open for writing with ETXTBSY, "text file busy"
// (golang/go#22315). The forkers here are the concurrent forks; each copier writes a fresh path
// and then runs it.
//
// The bounds here fail the run at copyRunaway and copyForkerStop so a stalled exercise is reported
// as such; the parent's copyChildBudget is what ends it, since nothing inside this process can
// unblock a copy stuck while it holds syscall.ForkLock.
func copyRace(t *testing.T, copyFile func(source, path string) error, source string) (result copyRaceResult, err error) {
	t.Helper()
	info, statErr := os.Stat(source)
	if statErr != nil {
		return result, statErr
	}
	// The program is whatever true(1) this host has, and on a BusyBox system that is the multi-call
	// binary rather than a tiny program. The copy count comes down from the source's own size, so a
	// large true(1) does not fail a correct host; the count keeps at least one round, so a program
	// over an eighth of the ceiling still runs and the ceiling is a budget rather than a refusal.
	iterations := int64(copyIterations)
	if size := info.Size(); size > 0 {
		if affordable := copyByteBudget / (size * copyWriters); affordable < iterations {
			iterations = affordable
		}
	}
	if iterations < 1 {
		iterations = 1
	}
	attempted := copyWriters * iterations
	// The counters the writers and forkers add to are local and read only through atomic.LoadInt64
	// when the result is built, so marshalling the result can never race a writer that the timeout
	// left running.
	var written, busy, forkStarts int64
	// copiesOpen counts copy calls in progress; forksInCopy counts forks that complete while one is.
	var copiesOpen, forksInCopy int64
	snapshot := func() copyRaceResult {
		return copyRaceResult{
			Busy:      atomic.LoadInt64(&busy),
			Written:   atomic.LoadInt64(&written),
			Attempted: attempted,
			Forked:    atomic.LoadInt64(&forkStarts),
		}
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
				if err := exec.Command(source).Run(); err == nil {
					atomic.AddInt64(&forkStarts, 1)
					if atomic.LoadInt64(&copiesOpen) > 0 {
						atomic.AddInt64(&forksInCopy, 1)
					}
				}
			}
		}()
	}
	// The writers never call testing.T: the runaway ceiling below can end the run while one of them
	// is still stuck, and a call on t from a goroutine that outlives it panics.
	var failuresMu sync.Mutex
	var failures []string
	fail := func(format string, args ...any) {
		failuresMu.Lock()
		defer failuresMu.Unlock()
		failures = append(failures, fmt.Sprintf(format, args...))
	}
	// The forkers must be running before the first copy opens, or the overlap check below proves nothing.
	readyBy := time.Now().Add(copyForkerStop)
	for atomic.LoadInt64(&forkStarts) == 0 {
		if time.Now().After(readyBy) {
			close(stop)
			return snapshot(), fmt.Errorf("no forker started a process within %s", copyForkerStop)
		}
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	deadline := start.Add(copyRunaway)
	var writers sync.WaitGroup
	for i := 0; i < copyWriters; i++ {
		writers.Add(1)
		go func(writer int) {
			defer writers.Done()
			for n := int64(0); n < iterations; n++ {
				target := filepath.Join(root, fmt.Sprintf("copy-%d-%d", writer, n))
				atomic.AddInt64(&copiesOpen, 1)
				err := copyFile(source, target)
				atomic.AddInt64(&copiesOpen, -1)
				if err != nil {
					fail("copying to %s: %v", target, err)
					return
				}
				atomic.AddInt64(&written, info.Size())
				// CopyBinary asks for 0755 and this process's umask may clear the owner's execute bit,
				// which would make running the copy fail with EACCES and be read as a copy failure. The
				// exercise is the inherited write descriptor, not the mode (the caller-contract test
				// pins the mode against a same-flags reference open), so the bit is restored here.
				if err := os.Chmod(target, 0o755); err != nil {
					fail("making %s executable: %v", target, err)
					return
				}
				run := exec.Command(target)
				// A multicall program chooses its applet from argv[0]. The copy is named after the
				// copy, not the program, so running it under that name asks a BusyBox true(1) for an
				// applet called copy-0-0 and it fails on a correct CopyBinary. argv[0] is therefore
				// the source's own name; the file executed is still the copy.
				run.Args[0] = filepath.Base(source)
				if err := run.Run(); err != nil {
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
	var overlap int64
	select {
	case <-done:
		overlap = atomic.LoadInt64(&forksInCopy)
	case <-time.After(time.Until(deadline)):
		// A stuck copy holds syscall.ForkLock, so the forkers are not waited for here: they could not
		// fork again while it is held. The parent's kill is what ends this process.
		close(stop)
		return snapshot(), fmt.Errorf("the copies did not finish within %s: a copy is stuck", copyRunaway)
	}
	close(stop)
	// The forker shutdown is bounded too, with its own budget: closing stop ends a forker that is
	// between execs, but one already inside exec.Command(source).Run() must return on its own.
	forked := make(chan struct{})
	go func() { forkers.Wait(); close(forked) }()
	select {
	case <-forked:
	case <-time.After(copyForkerStop):
		return snapshot(), fmt.Errorf("the forkers did not stop within %s after stop was closed: a fork is stuck", copyForkerStop)
	}
	result = snapshot()
	result.Elapsed = time.Since(start)
	result.Overlap = overlap
	failuresMu.Lock()
	defer failuresMu.Unlock()
	if len(failures) > 0 {
		return result, errors.New(strings.Join(failures, "; "))
	}
	return result, nil
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
