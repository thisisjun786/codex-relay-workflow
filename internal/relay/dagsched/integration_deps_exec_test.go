package dagsched

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// verifierGroupEndLimit is execgroup.KillGrace + 2 s: a cancelled verification returns within it.
const verifierGroupEndLimit = 5*time.Second + 2*time.Second

// verifierPid answers the pid a command wrote to file, polling until the line is complete.
func verifierPid(t *testing.T, file string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(file); err == nil && strings.HasSuffix(string(raw), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil || pid <= 1 {
				t.Fatalf("%s holds no pid: %q", file, raw)
			}
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no pid in %s after 10 s", file)
	return 0
}

// verifierGone polls until pid no longer exists (a killed process is reaped by its new parent shortly after).
func verifierGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// CRW-1573: a cancelled integration batch ends with the whole verification tree, not only its first process.
func TestCommandVerifierEndsTheGroupOnCancel(t *testing.T) {
	dir := t.TempDir()
	verify := CommandVerifierEnv([]string{"sh", "-c", `sleep 60 >&2 & echo $! > grandchild.pid; wait`}, os.Environ)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- verify(ctx, dir, nil) }()
	grandchild := verifierPid(t, filepath.Join(dir, "grandchild.pid"))
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })
	cancel()
	start := time.Now()
	select {
	case err := <-done:
		if elapsed := time.Since(start); elapsed > verifierGroupEndLimit {
			t.Fatalf("the verifier returned %v after the cancel, want within %v", elapsed, verifierGroupEndLimit)
		}
		var host *IntegrationVerifierHostError
		if err == nil || errors.As(err, &host) {
			t.Fatalf("a cancelled verification = %v, want a failing run (not a host error)", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the verifier still running 15 s after the cancel: the grandchild %d holds its stderr open", grandchild)
	}
	if !verifierGone(grandchild, 3*time.Second) {
		t.Fatalf("the grandchild %d outlived the cancel", grandchild)
	}
}

func TestCommandVerifierKeepsItsErrors(t *testing.T) {
	ctx := context.Background()
	var host *IntegrationVerifierHostError
	if err := CommandVerifierEnv(nil, os.Environ)(ctx, t.TempDir(), nil); !errors.As(err, &host) {
		t.Fatalf("no program = %v, want an IntegrationVerifierHostError", err)
	}
	missing := filepath.Join(t.TempDir(), "no-such-program")
	if err := CommandVerifierEnv([]string{missing}, os.Environ)(ctx, t.TempDir(), nil); !errors.As(err, &host) {
		t.Fatalf("a missing program = %v, want an IntegrationVerifierHostError", err)
	}
	err := CommandVerifierEnv([]string{"sh", "-c", `echo "boom $CRW_VERIFY_PROBE" >&2; exit 3`}, os.Environ)(ctx, t.TempDir(), []string{"CRW_VERIFY_PROBE=batch"})
	if err == nil || errors.As(err, &host) || !strings.Contains(err.Error(), "boom batch") || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("exit 3 = %v, want a failing run naming exit status 3 and the stderr", err)
	}
	if err := CommandVerifierEnv([]string{"sh", "-c", `exit 0`}, os.Environ)(ctx, t.TempDir(), nil); err != nil {
		t.Fatalf("a passing run = %v, want nil", err)
	}
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		done <- CommandVerifierEnv([]string{"sh", "-c", `sleep 60 >&2 & echo $! > grandchild.pid; exit 0`}, os.Environ)(ctx, dir, nil)
	}()
	grandchild := verifierPid(t, filepath.Join(dir, "grandchild.pid"))
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a passing run that leaves a child holding stderr = %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the verifier still waiting 15 s after its command exited: the child %d holds stderr open", grandchild)
	}
}
