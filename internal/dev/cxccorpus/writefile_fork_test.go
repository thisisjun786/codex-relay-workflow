//go:build dev

package cxccorpus

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// corpusStressBudget bounds the whole exercise: every child runs under one context, and each wait
// below fails the test instead of hanging when a writer or a child does not return inside it.
const corpusStressBudget = 60 * time.Second

// corpusForkerStop bounds the forker shutdown once stop is closed.
const corpusForkerStop = 10 * time.Second

// corpusChildBudget is the parent's own ceiling on the process that runs the exercise, above the
// exercise's 60 s plus the forker shutdown. It is what makes the exercise terminable and not only
// time-bound (CRW-1008): a writeFile blocked in file I/O while it holds syscall.ForkLock cannot be
// cancelled from inside the process that is blocked, and the forkers queue behind it, so the parent
// kills the child's whole process group here and nothing outlives the test.
const corpusChildBudget = 90 * time.Second

// corpusChildEnv marks the process that runs the exercise itself.
const corpusChildEnv = "CRW1008_CORPUS_FORK_CHILD"

// corpusWaitWithin reports whether wg finished before limit; the caller decides what a timeout means.
func corpusWaitWithin(wg *sync.WaitGroup, limit time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// TestWriteFile_executes_under_concurrent_forks runs the exercise below in a child process of this
// test binary, in a process group of its own, and kills the group when the child does not finish
// inside corpusChildBudget. The exercise's own bounds fail it with a message; this one stays in force
// when a write is stuck holding syscall.ForkLock, which no context can cancel.
func TestWriteFile_executes_under_concurrent_forks(t *testing.T) {
	if os.Getenv(corpusChildEnv) != "" {
		t.Skip("this is the child process; the exercise is TestWriteFile_fork_exercise")
	}
	if _, err := exec.LookPath("true"); err != nil {
		t.Skip("no true(1) on this host")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestWriteFile_fork_exercise$", "-test.count=1", "-test.v", "-test.timeout="+corpusChildBudget.String())
	cmd.Env = append(os.Environ(), corpusChildEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := make(chan struct{})
	timer := time.AfterFunc(corpusChildBudget+5*time.Second, func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		close(killed)
	})
	waitErr := cmd.Wait()
	timedOut := !timer.Stop()
	if timedOut {
		<-killed
	}
	// Whatever ended the child, every process it started belongs to this test.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if timedOut {
		t.Fatalf("the exercise did not finish within %s and its process group was killed: a write or a fork is stuck\n%s", corpusChildBudget, output.String())
	}
	if waitErr != nil {
		t.Fatalf("the exercise failed: %v\n%s", waitErr, output.String())
	}
	if !bytes.Contains(output.Bytes(), []byte("--- PASS: TestWriteFile_fork_exercise")) {
		t.Fatalf("the exercise did not run to a pass:\n%s", output.String())
	}
}

// TestWriteFile_fork_exercise writes a program with writeFile, makes it executable the way a
// Given.Modes entry does, and runs it at once, while other goroutines fork. Without the
// syscall.ForkLock read lock around the write, a fork in that window inherits the write descriptor
// and the exec fails with ETXTBSY (golang/go#22315). Each copy must run; no retry hides a failure.
// It runs only in the child process of TestWriteFile_executes_under_concurrent_forks.
func TestWriteFile_fork_exercise(t *testing.T) {
	if os.Getenv(corpusChildEnv) == "" {
		t.Skip("the exercise runs in the child process its parent starts")
	}
	ctx, cancel := context.WithTimeout(context.Background(), corpusStressBudget)
	defer cancel()
	dir := t.TempDir()
	stop := make(chan struct{})
	var forkers sync.WaitGroup
	var forks int64
	// writesOpen counts writeFile calls in progress. forksInWrite counts fork attempts that begin while
	// one is open. The count is taken when an attempt starts, not when a child finishes: a fork begun
	// during a write waits behind syscall.ForkLock until the write closes, so its start is the evidence.
	var writesOpen, forksInWrite int64
	for i := 0; i < 2; i++ {
		forkers.Add(1)
		go func() {
			defer forkers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if atomic.LoadInt64(&writesOpen) > 0 {
					atomic.AddInt64(&forksInWrite, 1)
				}
				if exec.CommandContext(ctx, "true").Run() == nil {
					atomic.AddInt64(&forks, 1)
				}
			}
		}()
	}

	// The forkers must be running before the first write, or the overlap check below proves nothing.
	for i := 0; atomic.LoadInt64(&forks) == 0 && i < 5000; i++ {
		time.Sleep(time.Millisecond)
	}
	if atomic.LoadInt64(&forks) == 0 {
		close(stop)
		if !corpusWaitWithin(&forkers, corpusForkerStop) {
			t.Fatalf("the forkers did not stop within %s", corpusForkerStop)
		}
		t.Fatal("no fork started before the writes")
	}

	const writers, copies, maxWrites, minOverlap = 4, 40, 2000, 5
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		fail []string
	)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for n := 0; n < copies || (atomic.LoadInt64(&forksInWrite) < minOverlap && n < maxWrites); n++ {
				path := filepath.Join(dir, fmt.Sprintf("w%d-%d", w, n))
				atomic.AddInt64(&writesOpen, 1)
				err := writeFile(path, []byte("#!/bin/sh\nexit 0\n"))
				atomic.AddInt64(&writesOpen, -1)
				if err != nil {
					mu.Lock()
					fail = append(fail, err.Error())
					mu.Unlock()
					return
				}
				if err := os.Chmod(path, 0o755); err != nil {
					mu.Lock()
					fail = append(fail, err.Error())
					mu.Unlock()
					return
				}
				if out, err := exec.CommandContext(ctx, path).CombinedOutput(); err != nil {
					mu.Lock()
					fail = append(fail, path+": "+err.Error()+": "+string(out))
					mu.Unlock()
				}
			}
		}(w)
	}
	if !corpusWaitWithin(&wg, corpusStressBudget) {
		close(stop)
		t.Fatalf("the writes did not finish within %s", corpusStressBudget)
	}
	overlap := atomic.LoadInt64(&forksInWrite)
	close(stop)
	if !corpusWaitWithin(&forkers, corpusForkerStop) {
		t.Fatalf("the forkers did not stop within %s after stop was closed", corpusForkerStop)
	}
	if overlap < minOverlap {
		t.Errorf("only %d fork attempts began while a write was open (need %d): the writes never raced a fork", overlap, minOverlap)
	}
	for _, f := range fail {
		t.Error(f)
	}
}
