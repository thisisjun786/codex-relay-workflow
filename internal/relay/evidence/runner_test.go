package evidence

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ExecRunner moved here from two copies (package cli and the DAG scheduler); this pins what both did: the exit code and both streams of a command that ran, an error for one that did not
// start, and the context's error, with nothing else, for one the deadline ended.
func TestExecRunner(t *testing.T) {
	run := ExecRunner(context.Background())
	code, out, errText, err := run([]string{"/bin/sh", "-c", "echo out; echo err >&2; exit 3"}, 10*time.Second)
	if code != 3 || out != "out\n" || errText != "err\n" || err != nil {
		t.Fatalf("a failing command = (%d, %q, %q, %v), want (3, \"out\\n\", \"err\\n\", nil)", code, out, errText, err)
	}
	if code, out, _, err = run([]string{"/bin/sh", "-c", "echo fine"}, 10*time.Second); code != 0 || out != "fine\n" || err != nil {
		t.Fatalf("a command that succeeds = (%d, %q, %v)", code, out, err)
	}
	if _, _, _, err = run([]string{"/nonexistent/forge-binary"}, 10*time.Second); err == nil {
		t.Fatal("a command that cannot start answered no error")
	}
	code, out, errText, err = run([]string{"/bin/sh", "-c", "echo late; exec sleep 30"}, 200*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || code != 0 || out != "" || errText != "" {
		t.Fatalf("a command past the deadline = (%d, %q, %q, %v), want only the context's error", code, out, errText, err)
	}
}
