package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// `codex --version` whose output a descendant still holds open once it has exited is an unread
// dimension, answered within the wait delay (scope.WaitDelay's bound), never a diagnosis held
// until the descendant exits.
func TestCodexVersionIsUnreadWhenADescendantHoldsItsOutput(t *testing.T) {
	bin := t.TempDir()
	pidFile := filepath.Join(bin, "descendant.pid")
	script := "#!/bin/sh\necho 'codex-cli 0.154.0'\nsleep 5 &\necho $! > " + pidFile + "\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	saved := commandWaitDelay
	commandWaitDelay = 100 * time.Millisecond
	t.Cleanup(func() { commandWaitDelay = saved })
	answer := make(chan *string, 1)
	go func() { answer <- codexVersion(context.Background()) }()
	select {
	case got := <-answer:
		if got != nil {
			t.Fatalf("a version read while a descendant held the output: %q", *got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("codex --version held the diagnosis while a descendant kept its output open")
	}
}
