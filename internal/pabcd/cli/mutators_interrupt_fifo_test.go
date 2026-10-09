package cli

import (
	"context"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1074 (post-evaluation d1): the session-state reads that precede the first write must not outlive the
// first SIGINT. A session file that is a FIFO whose writer stays open without sending EOF used to block
// os.ReadFile for good, so the cancelled invocation never ended and, for memory and scan, kept its session
// lock. The reader now refuses a file that is not a regular file without reading it (open with O_NONBLOCK and
// fstat), so the read returns at once: an ended context's precedence answers 130, a live one the unreadable-state
// refusal. The ended-context precedence over that refusal is pinned by the mutatorsFIFOState tests.

// fifoStateWithOpenWriter makes the session state a FIFO and keeps a writer open on it with nothing written.
func fifoStateWithOpenWriter(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
}

// endsWithin runs fn and fails the test when it has not returned after the budget.
func endsWithin(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not end: a state read is blocked on a FIFO whose writer is open", what)
	}
}

func TestMemoryAllowWriteOnAnOpenFIFOWithALiveContextIsRefused(t *testing.T) {
	cwd, _, _ := cliSeed(t)
	path := state.StatePath(cwd, "rec-s1")
	fifoStateWithOpenWriter(t, path)
	var out string
	var code int
	var err error
	endsWithin(t, "memory allow-write", func() {
		out, code, err = RunMemoryCLIContext(context.Background(), MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd})
	})
	if err != nil || code != 1 || !strings.Contains(out, "session state is unreadable; refusing to overwrite it") {
		t.Fatalf("memory allow-write on an open FIFO, live context: %q %d %v, want the unreadable-state refusal", out, code, err)
	}
	mutatorsStillFIFO(t, path)
}

func TestScanRecordOnAnOpenFIFOWithALiveContextIsRefused(t *testing.T) {
	cwd := scanInterruptWorkspace(t)
	path := state.StatePath(cwd, "s1")
	fifoStateWithOpenWriter(t, path)
	var got CliResult
	var err error
	endsWithin(t, "scan record", func() { got, err = RunScanCliContext(context.Background(), scanInterruptArgs(t, cwd)) })
	if err != nil || got.Code != 1 || !strings.Contains(got.Output, "session state is unreadable") {
		t.Fatalf("scan record on an open FIFO, live context: %+v %v, want the unreadable-state refusal", got, err)
	}
}

func TestLoopSteerOnAnOpenFIFOWithALiveContextAnswersUnbound(t *testing.T) {
	cwd, _ := loopMutWorkspace(t, nil)
	fifoStateWithOpenWriter(t, state.StatePath(cwd, loopMutSession))
	args, err := ParseLoopCliArgs([]string{"steer", "--session", loopMutSession, "--cwd", cwd, "--batch-json",
		`{"idempotencyKey":"k1","rationale":"r","evidence":"e","ops":[{"kind":"annotate","note":"n"}]}`}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	var got LoopCliResult
	endsWithin(t, "loop steer", func() { got, err = RunLoopCliContext(context.Background(), args) })
	if err != nil || got.Code != 1 || !strings.Contains(got.Output, "has no bound goalplan") {
		t.Fatalf("loop steer on an open FIFO, live context: %+v %v, want the unbound refusal", got, err)
	}
}
