package exercise

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// heldOutput runs a session with a bridge that leaves a descendant holding its stdout and never
// answers, with a one-second deadline, and fails unless the session ends well before the
// descendant would (it sleeps 60 s). The session runs beside a watchdog, so a session that is
// not bounded fails the test at the watchdog instead of holding it for the descendant's life.
func heldOutput(t *testing.T) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	bridge := filepath.Join(dir, "bridge")
	// The descendant sleeps with stdout inherited; the bridge itself reads stdin and says nothing.
	if err := os.WriteFile(bridge, []byte("#!"+sh+"\nsleep 60 &\necho $! > "+filepath.Join(dir, "child")+"\nexec cat >/dev/null\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if b, err := os.ReadFile(filepath.Join(dir, "child")); err == nil {
			_ = exec.Command("kill", string(trimNL(b))).Run()
		}
	}()
	saved := WaitDelay
	WaitDelay = 200 * time.Millisecond
	defer func() { WaitDelay = saved }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	done := make(chan Bridge, 1)
	go func() { done <- Argv(ctx, []string{bridge}, nil) }()
	select {
	case result := <-done:
		if took := time.Since(start); took > 10*time.Second {
			t.Fatalf("the session outlived its deadline by %v", took)
		}
		if result.Err == nil {
			t.Fatal("a bridge that never answered completed a session")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the session was still reading 9 s after its deadline: a descendant holding the bridge's output keeps it open")
	}
}

// A bridge that leaves a descendant holding its stdout, and never answers, must not keep the
// session past its deadline: the reader is closed when the session's time is up.
func TestASessionEndsAtItsDeadlineWhenADescendantHoldsTheOutput(t *testing.T) {
	heldOutput(t)
}

// The same bound holds when the bridge's stderr goes nowhere. os/exec's WaitDelay closes the
// stdout pipe only while a goroutine copies one of the command's streams (here only stderr's
// capture creates one), so it is the session's own close of its reader at the deadline, and not
// WaitDelay, that ends the read.
func TestTheSessionClosesItsReaderAtTheDeadlineWithoutACopyingGoroutine(t *testing.T) {
	saved := stderrOf
	stderrOf = func(*strings.Builder) io.Writer { return nil }
	defer func() { stderrOf = saved }()
	heldOutput(t)
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
