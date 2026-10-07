//go:build dev

package cxccorpus

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestWriteFile_executes_under_concurrent_forks writes a program with writeFile, makes it executable
// the way a Given.Modes entry does, and runs it at once, while other goroutines fork. Without the
// syscall.ForkLock read lock around the write, a fork in that window inherits the write descriptor
// and the exec fails with ETXTBSY (golang/go#22315). Each copy must run; no retry hides a failure.
func TestWriteFile_executes_under_concurrent_forks(t *testing.T) {
	if _, err := exec.LookPath("true"); err != nil {
		t.Skip("no true(1) on this host")
	}
	dir := t.TempDir()
	stop := make(chan struct{})
	var forkers sync.WaitGroup
	var forks int64
	// writesOpen counts writeFile calls in progress; forksInWrite counts forks that complete while one is.
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
				if exec.Command("true").Run() == nil {
					atomic.AddInt64(&forks, 1)
					if atomic.LoadInt64(&writesOpen) > 0 {
						atomic.AddInt64(&forksInWrite, 1)
					}
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
		forkers.Wait()
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
				if out, err := exec.Command(path).CombinedOutput(); err != nil {
					mu.Lock()
					fail = append(fail, path+": "+err.Error()+": "+string(out))
					mu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()
	overlap := atomic.LoadInt64(&forksInWrite)
	close(stop)
	forkers.Wait()
	if overlap < minOverlap {
		t.Errorf("only %d forks completed while a write was open (need %d): the writes never raced a fork", overlap, minOverlap)
	}
	for _, f := range fail {
		t.Error(f)
	}
}
