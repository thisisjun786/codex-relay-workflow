package hook

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func compareGuard(t *testing.T, cases string) {
	t.Helper()
	cmd := exec.Command(python(t), "testdata/guard_compare.py", binary(t), t.TempDir(), cases)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("guard parity %v\n%s", err, out)
	}
	var passed []string
	if err = json.Unmarshal(out, &passed); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	t.Logf("%d full byte envelopes and marker/DB effects equal: %s", len(passed), out)
}
func Test33GuardBinaryPython(t *testing.T) {
	compareGuard(t, "unmanaged,unregistered,ready_missing,ready_receipted,ready_staged,artifacts_changed,frozen,generation,dispatch,generation_absent,hold_spent,malformed_disposition,in_progress,no_record")
}

// A frozen copy nested deeper than json.loads can descend leaves guard.deliverable_state as the
// RecursionError its except clauses do not name, so the guard faults and releases, recorded. One
// level less is read, and the receipt stands at the head through its frozen copy.
func Test33GuardFrozenCopyAtTheDecoderDepth(t *testing.T) {
	compareGuard(t, "frozen_at_depth,frozen_past_depth")
}
func Test33GuardUsagePython(t *testing.T) {
	cmd := exec.Command(python(t), "testdata/guard_usage.py", binary(t), t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("guard usage parity %v\n%s", err, out)
	}
	t.Logf("usage envelopes: %s", out)
}

func Test33HoldReservation(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "a")
	o := GuardOptions{Root: root, Now: "2026-01-01T00:00:00Z", Mode: Hold}
	ctx := context.Background()
	ok, err := reserveHold(ctx, directory, "s", "t", o)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	ok, err = reserveHold(ctx, directory, "s", "t", o)
	if err != nil || ok {
		t.Fatalf("duplicate %v %v", ok, err)
	}
	counts, bad, unreadable := HoldCounters(ctx, directory, "s", "t", o.Now, root)
	if bad != "" || unreadable != "" || get(counts, "holdsThisTurn") != int64(1) || get(counts, "holdsThisGeneration") != int64(1) || get(counts, "holdsThisSessionWindow") != int64(1) {
		t.Fatal(counts, bad, unreadable)
	}
}
func Test33DecisionBoundsAndIdentity(t *testing.T) {
	assignment := delivery.AssignmentID("dispatch")
	marker := Object{{Key: "intent", Value: Object{{Key: "dispatchRequestIdHash", Value: assignment}}}, {Key: "bound", Value: Object{{Key: "sessionId", Value: "s"}}}, {Key: "relationship", Value: Object{{Key: "relationshipId", Value: "r"}}}, {Key: "claims", Value: []any{Object{{Key: "sessionId", Value: "s"}, {Key: "dispatchRequestId", Value: "dispatch"}, {Key: "factId", Value: "claims/s/claim.json"}}}}}
	stop := Object{{Key: "session_id", Value: "s"}, {Key: "turn_id", Value: "t"}, {Key: "stop_hook_active", Value: false}}
	o := Observation{Stop: stop, Marker: marker, Assignment: assignment, Now: "2026-01-01T00:00:00Z"}
	for _, c := range []struct {
		mode            string
		counts          Object
		state, decision string
	}{{Hold, Object{}, "undeclared_turn_end", "block"}, {Observe, Object{}, "undeclared_turn_end", "release"}, {Hold, Object{{Key: "holdsThisTurn", Value: int64(1)}}, "hold_in_flight", "release"}, {Hold, Object{{Key: "holdsThisGeneration", Value: int64(2)}, {Key: "holdsThisTurn", Value: int64(1)}}, "unresolved_handoff", "release"}, {Hold, Object{{Key: "holdsThisSessionWindow", Value: int64(3)}}, "unresolved_handoff", "release"}, {Hold, Object{{Key: "holdsThisTurn", Value: nil}}, "marker_malformed", "release"}} {
		v := Decide(o, c.counts, c.mode)
		if get(v, "state") != c.state || get(v, "decision") != c.decision {
			t.Fatal(v)
		}
	}
	o.Disposition = Object{{Key: "sessionId", Value: "s"}, {Key: "turnId", Value: "other"}, {Key: "outcome", Value: "failed"}}
	if ClassifyDeclaration(o) != "undeclared_turn_end" {
		t.Fatal("foreign turn released")
	}
	o.Disposition = Object{{Key: "sessionId", Value: "s"}, {Key: "turnId", Value: "t"}, {Key: "outcome", Value: "failed"}}
	if !strings.HasPrefix(ClassifyDeclaration(o), "declared_") {
		t.Fatal("own failure did not release")
	}
	o.Receipt = Object{{Key: "sessionId", Value: "s"}, {Key: "turnId", Value: "t"}, {Key: "relationshipId", Value: "foreign"}, {Key: "atCurrentHead", Value: true}}
	if ReceiptMatches(o.Receipt, stop, marker) {
		t.Fatal("foreign assignment receipt matched")
	}
}
