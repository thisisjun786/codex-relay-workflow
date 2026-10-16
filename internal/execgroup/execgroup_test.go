package execgroup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// waitPid answers the pid a command wrote to file, polling until the line is complete.
func waitPid(t *testing.T, file string) int {
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

// gone polls until pid no longer exists (a killed process is reaped by its new parent shortly after).
func gone(pid int, within time.Duration) bool {
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

func TestBindKillsTheGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", `sleep 60 & echo $! > grandchild.pid; wait`)
	cmd.Dir = dir
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	Bind(cmd, KillGrace)
	if !cmd.SysProcAttr.Setpgid || cmd.WaitDelay != KillGrace || cmd.Cancel == nil {
		t.Fatalf("Bind left the command without its group: Setpgid=%v WaitDelay=%v Cancel set=%v", cmd.SysProcAttr.Setpgid, cmd.WaitDelay, cmd.Cancel != nil)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	leader := cmd.Process.Pid
	grandchild := waitPid(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })
	if pgid, err := syscall.Getpgid(grandchild); err != nil || pgid != leader {
		t.Fatalf("the grandchild's group is %d (%v), want the leader's own group %d", pgid, err, leader)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	cancel()
	start := time.Now()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("Wait after the cancel = %v, want the killed exit", err)
		}
		if elapsed := time.Since(start); elapsed > KillGrace+2*time.Second {
			t.Fatalf("Wait returned %v after the cancel, want within %v", elapsed, KillGrace+2*time.Second)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("Wait still blocked 15 s after the cancel")
	}
	if !gone(grandchild, 3*time.Second) {
		t.Fatalf("the grandchild %d outlived the cancel", grandchild)
	}
}

func TestBindKeepsTheCallersProcAttr(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "true")
	attr := &syscall.SysProcAttr{}
	cmd.SysProcAttr = attr
	Bind(cmd, time.Second)
	if cmd.SysProcAttr != attr || !attr.Setpgid {
		t.Fatalf("Bind replaced the caller's attributes instead of adding the group: %+v", cmd.SysProcAttr)
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
}
