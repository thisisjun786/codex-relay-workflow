package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// An interrupted receipt test stops the check command at once and certifies nothing: the first
// SIGINT to the crw process reaches the command through the invocation's context, so the run ends
// with exit 1 and the "terminated by signal" refusal instead of waiting for the command and writing
// a receipt. While the pabcd row dropped that context the run ignored the interrupt, waited out the
// command and published a success receipt (CRW-582).
func TestPabcdReceiptTestStopsOnFirstInterrupt(t *testing.T) {
	crw := testsupport.CRW(t)
	home := t.TempDir()
	root := filepath.Join(home, "work")
	receiptSignalRepo(t, root)
	epoch := "check-epoch"
	s := state.DefaultState("s1", "")
	s.Phase, s.OrchestrationActive, s.CheckEpoch = state.PhaseC, true, &epoch
	if err := state.WriteState(root, s); err != nil {
		t.Fatal(err)
	}
	before := receiptSignalHomeListings(t, home)
	marker := filepath.Join(t.TempDir(), "started")
	cmd := exec.Command(crw, "pabcd", "receipt", "test", "--session", "s1", "--", "/bin/sh", "-c", `: > "$1"; exec sleep 30`, "sh", marker)
	cmd.Dir = root
	cmd.Env = []string{
		"HOME=" + home,
		"CODEX_HOME=" + filepath.Join(home, "codex"),
		"CRW_HOME=" + filepath.Join(home, "crw"),
		"PATH=" + os.Getenv("PATH"),
		testsupport.RefuseLiveStateEnv + "=1",
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the check command did not start within 10 s\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		waited = true
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		waited = true
		t.Fatalf("the command was still running 10 s after the SIGINT\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Fatalf("exit code %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"terminated by signal", "no receipt written"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout does not hold %q\nstdout:\n%s\nstderr:\n%s", want, stdout.String(), stderr.String())
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".crw", "evidence", "s1", "test-receipt.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an interrupted check left a receipt: %v", err)
	}
	if after := receiptSignalHomeListings(t, home); after != before {
		t.Fatalf("the run changed the child's home listings\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// receiptSignalRepo makes root a git repository with one committed file, which the receipt's source
// capture needs; the test's own git calls get the fixture identity and no user configuration.
func receiptSignalRepo(t *testing.T, root string) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") {
			t.Setenv(key, "")
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	for key, value := range map[string]string{
		"GIT_CONFIG_GLOBAL":       os.DevNull,
		"GIT_CONFIG_NOSYSTEM":     "1",
		"GIT_CEILING_DIRECTORIES": filepath.Dir(root),
		"GIT_AUTHOR_NAME":         "fixture",
		"GIT_AUTHOR_EMAIL":        "fixture@example.invalid",
		"GIT_COMMITTER_NAME":      "fixture",
		"GIT_COMMITTER_EMAIL":     "fixture@example.invalid",
	} {
		t.Setenv(key, value)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "initial")
}

// receiptSignalHomeListings lists the home state roots the child could reach, so a run that wrote
// into a live-home path changes this string.
func receiptSignalHomeListings(t *testing.T, home string) string {
	t.Helper()
	var b strings.Builder
	for _, name := range []string{".codex", ".crw", "codex", "crw"} {
		entries, err := os.ReadDir(filepath.Join(home, name))
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(&b, "%s: absent\n", name)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		fmt.Fprintf(&b, "%s: %s\n", name, strings.Join(names, " "))
	}
	return b.String()
}
