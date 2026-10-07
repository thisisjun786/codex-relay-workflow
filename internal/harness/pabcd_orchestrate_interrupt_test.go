package harness

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-871 through the row: a non-canonical --session is refused before anything is read, locked or
// written, and the first SIGINT ends an orchestrate mutation with Interrupted (130) and nothing
// written, the way the oracle's process dies at the signal.

// orchestrateInterruptWrite puts a session file at the literal name, bypassing StatePath.
func orchestrateInterruptWrite(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, ".crw", "sessions", name+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestPabcdOrchestrateRowRefusesANonCanonicalSession is the evaluation reproduction through the
// terminal row: the two files are the evaluation's, and neither may change.
func TestPabcdOrchestrateRowRefusesANonCanonicalSession(t *testing.T) {
	root := orchestrateTestHome(t)
	orchestrateInterruptWrite(t, root, "raw id", `{"phase":"B","sessionId":"raw id"}`)
	orchestrateInterruptWrite(t, root, "raw-id", `{"phase":"C","sessionId":"raw-id","checkEpoch":"c-1"}`)
	before := orchestrateTestTree(t, root)
	code, out, errOut := pabcdCLITestRun([]string{"orchestrate", "reset", "--session", "raw id"}, "")
	if code != 1 || errOut != "" || out != "orchestrate reset: session id is not canonical\n" {
		t.Fatalf("reset --session \"raw id\": %d %q %q", code, out, errOut)
	}
	orchestrateTestSameTree(t, before, orchestrateTestTree(t, root))
}

// TestPabcdScanRowRefusesANonCanonicalSession pins the scan row's stream and exit code for the
// parser's new refusal.
func TestPabcdScanRowRefusesANonCanonicalSession(t *testing.T) {
	root := orchestrateTestHome(t)
	orchestrateInterruptWrite(t, root, "raw-id", `{"phase":"C","sessionId":"raw-id"}`)
	before := orchestrateTestTree(t, root)
	code, out, errOut := pabcdCLITestRun([]string{"scan", "record", "--session", "raw id"}, "")
	if code != 1 || out != "" || errOut != "scan: scan record: session id is not canonical\n" {
		t.Fatalf("scan record --session \"raw id\": %d %q %q", code, out, errOut)
	}
	orchestrateTestSameTree(t, before, orchestrateTestTree(t, root))
}

// TestPabcdOrchestrateResetEndsOnTheFirstInterrupt holds the session lock, starts the row and ends
// the invocation while the row waits. The lock is released before the wait budget runs out, so the
// signal is what decides the answer: on the baseline the row ignored the context, took the lock and
// published the reset (exit 0, IDLE); the fix answers Interrupted (130) and writes nothing.
func TestPabcdOrchestrateResetEndsOnTheFirstInterrupt(t *testing.T) {
	root := orchestrateTestHome(t)
	orchestrateTestSeed(t, root, "s", "B")
	before := orchestrateTestTree(t, root)
	lockPath := state.StatePath(root, "s") + ".lock"
	if err := os.WriteFile(lockPath, []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- PabcdContext(ctx, []string{"orchestrate", "reset", "--session", "s"}, strings.NewReader(""), &stdout, &stderr, Verbs())
	}()
	time.Sleep(30 * time.Millisecond) // the row is inside the lock wait (the budget is about 250 ms)
	cancel()
	time.Sleep(30 * time.Millisecond)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != Interrupted || stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("interrupted reset: code %d, stdout %q, stderr %q; want %d with nothing written", code, stdout.String(), stderr.String(), Interrupted)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the orchestrate row still ran 5 s after the SIGINT\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if state.ReadState(root, "s").Phase != state.PhaseB {
		t.Fatal("the interrupted reset moved the phase")
	}
	if _, err := os.Stat(filepath.Join(root, ".crw", "ledger.jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the interrupted reset wrote a ledger row: %v", err)
	}
	after := orchestrateTestTree(t, root)
	if _, ok := after[".crw/sessions/s.json.lock"]; ok {
		t.Fatal("the interrupted reset left its own lock file behind")
	}
	delete(before, ".crw/sessions/s.json.lock")
	orchestrateTestSameTree(t, before, after)
}
