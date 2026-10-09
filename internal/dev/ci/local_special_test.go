//go:build dev && !windows

package ci

import (
	"io"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// CRW-1025 (pre-merge evaluation of 71490802, D2): a tracked engine file replaced by a FIFO, device or other
// special file is a difference from the commit. It is refused without being opened: opening a FIFO with no
// writer blocks, and the verification must not wait on it.
func TestLocal_a_tracked_engine_file_replaced_by_a_fifo_refuses_the_run_without_blocking(t *testing.T) {
	repo := localEngineRepo(t)
	fifo := filepath.Join(repo.root, "internal/dev/ci/engine.go")
	removeForTest(t, fifo)
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("no FIFO here: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "r.json")), "", io.Discard)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "engine_differs_from_commit") || !strings.Contains(err.Error(), "internal/dev/ci/engine.go") {
			t.Fatalf("err = %v; want an engine_differs_from_commit refusal naming the FIFO", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the verification blocked on a FIFO that replaced a tracked engine file")
	}
}

// The same replacement reaches the guard on a --reuse run, ahead of any record decision.
func TestLocal_a_fifo_in_place_of_an_engine_file_refuses_a_reuse_run_without_blocking(t *testing.T) {
	repo := localEngineRepo(t)
	fifo := filepath.Join(repo.root, "internal/dev/ci/engine.go")
	removeForTest(t, fifo)
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("no FIFO here: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "r.json")), filepath.Join(t.TempDir(), "absent.json"), io.Discard)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "engine_differs_from_commit") {
			t.Fatalf("err = %v; want an engine_differs_from_commit refusal", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the --reuse verification blocked on a FIFO that replaced a tracked engine file")
	}
}
