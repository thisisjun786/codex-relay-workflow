//go:build dev

package cxcfuzz

import (
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Shrinking keeps the verdict's detail, not only its kind, so a divergence that is one read difference shrinks
// to that read difference and not into a write difference of the same kind (CRW-978 c1, review d1).
func TestShrinkKeepsTheVerdictDetail(t *testing.T) {
	requireNode(t)
	target := echoTarget()
	target.Generate = func(rng *rand.Rand, size int) any {
		return pyjson.Object{{Key: "a", Value: true}, {Key: "b", Value: true}}
	}
	target.Compare = func(goOut, oracleOut any) Verdict {
		if v, ok := field(goOut, "a"); ok && v == true {
			return Verdict{Kind: Differ, Detail: "read"}
		}
		if v, ok := field(goOut, "b"); ok && v == true {
			return Verdict{Kind: Differ, Detail: "write"}
		}
		return Verdict{Kind: Same}
	}
	out := t.TempDir()
	summary, err := Campaign(Config{Target: target, Cases: 1, Seed: 1, Workers: 1, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Differ != 1 {
		t.Fatalf("summary differ %d, want 1", summary.Differ)
	}
	entries, err := os.ReadDir(filepath.Join(out, DivergenceDir))
	if err != nil || len(entries) != 1 {
		t.Fatalf("the divergences directory holds %d entries (%v), want one", len(entries), err)
	}
	raw, err := os.ReadFile(filepath.Join(out, DivergenceDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var d Divergence
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if d.Verdict.Detail != "read" {
		t.Fatalf("the divergence shrank to the %q difference, want the read difference", d.Verdict.Detail)
	}
}

// The diagnostic paths in an answer's detail and writeError are renamed the same way as its path, and the state
// comparison applies the same rename (CRW-978 c1, review d2).
func TestDiagnosticPathNamesInDetailAndWriteErrorAreNormalised(t *testing.T) {
	goalplanGo := pyjson.Object{{Key: "kind", Value: "unreadable"}, {Key: "path", Value: "<ROOT>/.crw/goalplans/rec-plan/goalplan.json"}, {Key: "detail", Value: "open <ROOT>/.crw/goalplans/rec-plan: permission denied"}, {Key: "plan", Value: nil}}
	goalplanOracle := pyjson.Object{{Key: "kind", Value: "unreadable"}, {Key: "path", Value: "<ROOT>/.codexclaw/goalplans/rec-plan/goalplan.json"}, {Key: "detail", Value: "open <ROOT>/.codexclaw/goalplans/rec-plan: permission denied"}, {Key: "plan", Value: nil}}
	if verdict := goalplanCompare(goalplanGo, goalplanOracle); verdict.Kind != Same {
		t.Fatalf("a name-only detail difference compared %v (%s), want Same", verdict.Kind, verdict.Detail)
	}
	stateGo := pyjson.Object{{Key: "unreadable", Value: false}, {Key: "state", Value: ""}, {Key: "writeError", Value: "open <ROOT>/.crw/sessions/s.json: permission denied"}}
	stateOracle := pyjson.Object{{Key: "unreadable", Value: false}, {Key: "state", Value: ""}, {Key: "writeError", Value: "open <ROOT>/.codexclaw/sessions/s.json: permission denied"}}
	if verdict := stateCompare(stateGo, stateOracle); verdict.Kind != Same {
		t.Fatalf("a name-only writeError difference compared %v (%s), want Same", verdict.Kind, verdict.Detail)
	}
}

// One input is recorded once, whatever its cause, so the summary names one record per input (CRW-978 c7, review d3).
func TestOneInputIsRecordedOnceWhateverItsCause(t *testing.T) {
	cfg := Config{Out: t.TempDir()}
	summary := Summary{}
	written := map[string]bool{}
	input := pyjson.Object{{Key: "x", Value: "same"}}
	if err := recordFailure(cfg, &summary, 1, 1, input, Timeout{}, written); err != nil {
		t.Fatal(err)
	}
	if err := recordFailure(cfg, &summary, 1, 2, input, CaseFailure{Cause: CauseDeadWorker, Err: errors.New("EOF")}, written); err != nil {
		t.Fatal(err)
	}
	if len(summary.Failures) != 1 {
		t.Fatalf("the summary names %v, want one record for the input", summary.Failures)
	}
	if summary.Timeouts != 1 || summary.DeadWorkers != 1 {
		t.Fatalf("the counts are timeouts %d and dead workers %d, want one of each", summary.Timeouts, summary.DeadWorkers)
	}
}

// Closing a pool reports a start-up root it could not remove, and the pin replay does not drop that error (CRW-978 c3, review d4).
func TestPoolCloseReportsAnUnremovableStartRoot(t *testing.T) {
	pool, err := NewPool(Oracle{Command: "sh", Shim: writeRecordEnvShim(t)}, 1, 5*time.Second, 10*time.Second, []string{"PATH=" + os.Getenv("PATH")})
	if err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(pool.root, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "held"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	closeErr := pool.Close()
	_ = os.Chmod(locked, 0o755)
	_ = os.RemoveAll(pool.root)
	if closeErr == nil {
		t.Fatal("Close reported no error for a start-up root it could not remove")
	}
}
