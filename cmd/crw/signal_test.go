package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// An install command asked to stop - SIGTERM from a supervisor, SIGHUP from a closing terminal,
// as well as SIGINT - while it waits for a lock stops waiting and does nothing destructive: a
// remove waiting for the promotion lock answers that it was interrupted, and the directory is
// still there. Without the handling the signal kills it outright, mid-wait or mid-removal.
func TestAnInstallCommandStopsWhenAsked(t *testing.T) {
	crw := filepath.Join(t.TempDir(), "crw")
	if out, err := exec.Command("go", "build", "-o", crw, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT} {
		home := t.TempDir()
		state := filepath.Join(home, "state")
		directory := filepath.Join(home, ".local", "share", "crw-runtime", "bin-0.0.1-000000000000")
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(staging.ClaimPath(directory), record.Encode(staging.NewPayload(staging.Complete, "CRW-158", "1")), 0o644); err != nil {
			t.Fatal(err)
		}
		lock := filepath.Join(state, "codex-relay-workflow", record.Name+record.PromotionLockSuffix)
		if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
			t.Fatal(err)
		}
		held, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Flock(int(held.Fd()), unix.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(crw, "install", "remove", directory)
		cmd.Env = []string{"HOME=" + home, "XDG_STATE_HOME=" + state, "CODEX_HOME=" + filepath.Join(home, ".codex"), "PATH=" + os.Getenv("PATH"), testsupport.RefuseLiveStateEnv + "=1"}
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(700 * time.Millisecond)
		if err := cmd.Process.Signal(signal); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err = <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("%v: the command was still waiting 10 s after it was asked to stop", signal)
		}
		_ = held.Close()
		if cmd.ProcessState.ExitCode() != 1 || !strings.Contains(stdout.String(), "interrupted") {
			t.Fatalf("%v: %v, exit %d\n%s", signal, err, cmd.ProcessState.ExitCode(), stdout.String())
		}
		if _, err := os.Stat(staging.ClaimPath(directory)); err != nil {
			t.Fatalf("%v: the directory was touched: %v", signal, err)
		}
	}
}
