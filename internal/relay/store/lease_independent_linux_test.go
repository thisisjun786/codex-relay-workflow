//go:build linux

package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

func leaseBreakBound(t *testing.T) time.Duration {
	t.Helper()
	raw, err := os.ReadFile("/proc/sys/fs/lease-break-time")
	if err != nil {
		t.Fatal(err)
	}
	seconds, err := strconv.Atoi(string(bytes.TrimSpace(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return time.Duration(seconds+5) * time.Second
}

func Test28IndependentReadLeases(t *testing.T) {
	// Serial: sends SIGIO to the test process, which every other running test would receive.
	root := t.TempDir()
	paths := []string{filepath.Join(root, "a"), filepath.Join(root, "b")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte(path), 0600); err != nil {
			t.Fatal(err)
		}
	}
	fds := make([]int, 2)
	for i, path := range paths {
		fd, err := pinnedOpen(path)
		if err != nil {
			t.Fatal(err)
		}
		fds[i] = fd
		defer syscall.Close(fd)
		held, detail := acquireReadLease(fd)
		if !held {
			t.Fatalf("lease %d: %s", i, detail)
		}
		defer releaseLease(fd)
	}
	if !leaseStillHeld(fds[0]) || !leaseStillHeld(fds[1]) {
		t.Fatal("nested independent leases not held")
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGIO); err != nil {
		t.Fatal(err)
	}
	if !leaseStillHeld(fds[0]) || !leaseStillHeld(fds[1]) {
		t.Fatal("unrelated SIGIO broke a lease")
	}

	ctx, cancel := context.WithTimeout(context.Background(), leaseBreakBound(t))
	defer cancel()
	writer := exec.CommandContext(ctx, "/bin/sh", "-c", ": >> \"$1\"", "sh", paths[0])
	started := make(chan error, 1)
	go func() { started <- writer.Run() }()
	for leaseStillHeld(fds[0]) {
		if ctx.Err() != nil {
			t.Fatalf("lease A did not break within %v", leaseBreakBound(t))
		}
		runtime.Gosched()
	}
	if !leaseStillHeld(fds[1]) {
		t.Fatal("breaking A also broke B")
	}
	releaseLease(fds[0])
	if err := <-started; err != nil {
		t.Fatalf("writer completion: %v", err)
	}
}

func Test28ConcurrentReadLeases(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	paths := []string{filepath.Join(root, "a"), filepath.Join(root, "b")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte(path), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan error, 2)
	var release sync.WaitGroup
	release.Add(1)
	for _, path := range paths {
		go func() {
			fd, err := pinnedOpen(path)
			if err != nil {
				results <- err
				return
			}
			defer syscall.Close(fd)
			held, detail := acquireReadLease(fd)
			if !held {
				results <- errors.New(detail)
				return
			}
			results <- nil
			release.Wait()
			releaseLease(fd)
		}()
	}
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("concurrent leases blocked")
		}
	}
	release.Done()
}
