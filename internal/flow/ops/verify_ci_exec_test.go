package ops

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

// groupEndLimit is execgroup.KillGrace + 2 s: a cancelled command returns within it.
const groupEndLimit = 5*time.Second + 2*time.Second

// execPid answers the pid a command wrote to file, polling until the line is complete.
func execPid(t *testing.T, file string) int {
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

// execGone polls until pid no longer exists (a killed process is reaped by its new parent shortly after).
func execGone(pid int, within time.Duration) bool {
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

type execResult struct {
	code int
	out  []byte
	err  error
}

// CRW-1573: a cancelled crw.verify ends with the whole `crw-dev ci local` tree, not only its first process, so the run's
// inflight wait returns within seconds of the cancel.
func TestExecCommandEndsTheGroupOnCancel(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan execResult, 1)
	go func() {
		code, out, err := ExecCommand(ctx, dir, nil, "sh", "-c", `sleep 60 & echo $! > grandchild.pid; echo started; wait`)
		done <- execResult{code, out, err}
	}()
	grandchild := execPid(t, filepath.Join(dir, "grandchild.pid"))
	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })
	cancel()
	start := time.Now()
	select {
	case r := <-done:
		if elapsed := time.Since(start); elapsed > groupEndLimit {
			t.Fatalf("ExecCommand returned %v after the cancel, want within %v", elapsed, groupEndLimit)
		}
		if r.err != nil {
			t.Fatalf("a killed command is an exit, not a start error: %v", r.err)
		}
		if r.code == 0 {
			t.Fatalf("a killed command answered exit 0")
		}
		if !strings.Contains(string(r.out), "started") {
			t.Fatalf("the output before the cancel is lost: %q", r.out)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("ExecCommand still running 15 s after the cancel: the grandchild %d holds its output open", grandchild)
	}
	if !execGone(grandchild, 3*time.Second) {
		t.Fatalf("the grandchild %d outlived the cancel", grandchild)
	}
}

func TestExecCommandKeepsItsContract(t *testing.T) {
	t.Run("exit code and combined output", func(t *testing.T) {
		dir := t.TempDir()
		code, out, err := ExecCommand(context.Background(), dir, []string{"CRW_EXEC_PROBE=set"}, "sh", "-c",
			`echo "out $CRW_EXEC_PROBE"; echo err >&2; pwd -P; exit 3`)
		if err != nil || code != 3 {
			t.Fatalf("ExecCommand = %d, %v; want exit 3 and no error", code, err)
		}
		real, _ := filepath.EvalSymlinks(dir)
		for _, want := range []string{"out set", "err", real} {
			if !strings.Contains(string(out), want) {
				t.Fatalf("combined output %q lacks %q", out, want)
			}
		}
	})
	t.Run("a success", func(t *testing.T) {
		code, out, err := ExecCommand(context.Background(), t.TempDir(), nil, "sh", "-c", `echo ok`)
		if err != nil || code != 0 || strings.TrimSpace(string(out)) != "ok" {
			t.Fatalf("ExecCommand = %d, %q, %v; want 0, ok, nil", code, out, err)
		}
	})
	t.Run("a start error", func(t *testing.T) {
		code, _, err := ExecCommand(context.Background(), t.TempDir(), nil, filepath.Join(t.TempDir(), "no-such-program"))
		if err == nil || code != 0 {
			t.Fatalf("ExecCommand of a missing program = %d, %v; want 0 and the start error", code, err)
		}
	})
	t.Run("a success that leaves a child holding the output", func(t *testing.T) {
		dir := t.TempDir()
		done := make(chan execResult, 1)
		go func() {
			code, out, err := ExecCommand(context.Background(), dir, nil, "sh", "-c", `sleep 60 & echo $! > grandchild.pid; echo ok`)
			done <- execResult{code, out, err}
		}()
		grandchild := execPid(t, filepath.Join(dir, "grandchild.pid"))
		t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })
		select {
		case r := <-done:
			if r.err != nil || r.code != 0 || !strings.Contains(string(r.out), "ok") {
				t.Fatalf("ExecCommand = %d, %q, %v; want the exit 0 and its output, no error", r.code, r.out, r.err)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("ExecCommand still waiting 15 s after its command exited: the child %d holds the output open", grandchild)
		}
	})
}
