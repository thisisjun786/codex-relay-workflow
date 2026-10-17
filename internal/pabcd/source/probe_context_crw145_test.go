package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// CRW-145 (post-evaluation round 3, d2): a probe owned by a context ends with it, with everything it started.

func gone(pid int) bool {
	ended, _ := processGone(pid)
	return ended
}

func processGone(pid int) (bool, string) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		killErr := syscall.Kill(pid, 0)
		return killErr == syscall.ESRCH, fmt.Sprintf("stat: %v; kill(0): %v", err, killErr)
	}
	rest := string(b[strings.LastIndexByte(string(b), ')')+1:])
	state := strings.TrimSpace(rest)
	// A reaped zombie can still be visible as X (dead) before /proc removes it.
	return strings.HasPrefix(state, "Z") || strings.HasPrefix(state, "X"), string(b)
}

// A grandchild that outlives its parent (a clean filter git started) is killed with the group when the deadline passes, and
// the call returns after the leader is reaped. A probe that is not owned (Probe) keeps the old behaviour: its own process only.
func TestRunBoundedCtxKillsTheWholeGroup(t *testing.T) {
	hermetic(t)
	pidFile := filepath.Join(t.TempDir(), "grandchild")
	script := "sleep 300 & echo $! > '" + pidFile + "'; wait"
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runBoundedCtx(ctx, true, "", "sh", []string{"-c", script}, nil, 1<<20, time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
		t.Fatalf("a group cut off by its context: %v after %v", err, time.Since(start))
	}
	b, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Skipf("the probe was cut off before it started its child: %v", rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(3 * time.Second)
	for !gone(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !gone(pid) {
		t.Errorf("the grandchild %d outlived the cancelled group", pid)
	}
}

// The output limit ends the whole group too, and a context that is already over starts nothing.
func TestRunBoundedCtxLimitAndEndedContext(t *testing.T) {
	hermetic(t)
	pidFile := filepath.Join(t.TempDir(), "grandchild")
	script := "sleep 300 & echo $! > '" + pidFile + "'; printf 123456789; wait"
	if _, err := runBoundedCtx(context.Background(), true, "", "sh", []string{"-c", script}, nil, 4, time.Minute); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("over the limit: %v", err)
	}
	if b, err := os.ReadFile(pidFile); err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		deadline := time.Now().Add(3 * time.Second)
		ended, state := processGone(pid)
		for !ended && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			ended, state = processGone(pid)
		}
		// Keep the observation that ended the wait; do not probe a reaping PID again.
		if !ended {
			t.Errorf("the grandchild %d outlived the group killed at the output limit: %s", pid, state)
		}
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	marker := filepath.Join(t.TempDir(), "ran")
	if _, err := runBoundedCtx(ended, true, "", "sh", []string{"-c", "echo x > '" + marker + "'"}, nil, 1<<20, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("an ended context: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("an ended context started the command")
	}
}

// An unowned probe still returns its output and ExitError exactly as before.
func TestRunBoundedCtxKeepsTheProbeResults(t *testing.T) {
	hermetic(t)
	out, err := runBoundedCtx(context.Background(), true, "", "sh", []string{"-c", "printf ok"}, nil, 1<<20, time.Minute)
	if err != nil || string(out) != "ok" {
		t.Errorf("%q %v", out, err)
	}
	var exit *ExitError
	if _, err := runBoundedCtx(context.Background(), true, "", "sh", []string{"-c", "echo oops >&2; exit 3"}, nil, 1<<20, time.Minute); !errors.As(err, &exit) || exit.Code != 3 {
		t.Errorf("exit status: %v", err)
	}
}

// The dirty-file hashing stops once the context has ended, and a capture says why its identity is unavailable.
func TestCaptureContextStopsAtTheDeadlineAndSaysWhy(t *testing.T) {
	base := hermetic(t)
	root := newRepo(t, base)
	writeFile(t, root, "dirty.ts", "changed\n")
	id, err := CaptureContext(context.Background(), root, Options{})
	if err != nil || id.Kind != KindResolved || !id.Dirty || id.TreeHash == "" {
		t.Fatalf("a live context captures as Capture does: %+v %v", id, err)
	}
	if want := Capture(root, Options{}); want.TreeHash != id.TreeHash || want.CommitSha != id.CommitSha {
		t.Errorf("CaptureContext differs from Capture: %+v vs %+v", id, want)
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if id, err := CaptureContext(ended, root, Options{}); id.Kind != KindUnavailable || err == nil {
		t.Errorf("an ended context: %+v %v", id, err)
	}
	// Hashing alone: the status is in hand and the context ends before the files are read.
	if got, err := hashRecords(ended, root, []statusRecord{{xy: []uint16{'?', '?'}, path: []uint16{'a'}}}); err == nil || got != "" {
		t.Errorf("hashing under an ended context: %q %v", got, err)
	}
	// A directory in no repository is git saying so; a git that cannot run is not.
	plain := tempIn(t, base)
	if id, err := CaptureProbed(plain, Options{}); id.Kind != KindUnavailable || !NotARepository(err) {
		t.Errorf("no repository: %+v %v", id, err)
	}
	t.Setenv("PATH", t.TempDir())
	if id, err := CaptureProbed(root, Options{}); id.Kind != KindUnavailable || err == nil || NotARepository(err) {
		t.Errorf("git absent: %+v %v", id, err)
	}
}
