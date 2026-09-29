package exercise

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A bridge that leaves a descendant holding its stdout, and never answers, must not keep the
// session past its deadline: the reader is closed when the session's time is up.
func TestASessionEndsAtItsDeadlineWhenADescendantHoldsTheOutput(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	bridge := filepath.Join(dir, "bridge")
	// The descendant sleeps with stdout inherited; the bridge itself reads stdin and says nothing.
	if err := os.WriteFile(bridge, []byte("#!"+sh+"\nsleep 30 &\necho $! > "+filepath.Join(dir, "child")+"\nexec cat >/dev/null\n"), 0o700); err != nil {
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
	result := Argv(ctx, []string{bridge}, nil)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the session outlived its deadline by %v", took)
	}
	if result.Err == nil {
		t.Fatal("a bridge that never answered completed a session")
	}
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
