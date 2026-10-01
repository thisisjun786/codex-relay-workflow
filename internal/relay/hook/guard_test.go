package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// compareGuard replays real marker, store and receipt fixtures (testdata/fixtures/guard-<case>,
// which guard_compare.py prepare laid out) through the evaluator, with the options the relay's
// guard-evaluate command handed it (the store the case's state directory holds as the default):
// each verdict, as contract.Emit writes it (the stdout key, which the command printed), and each
// observation record is the case's golden, which began as Python's guard.evaluate's, and the guard
// leaves the store as it was. The fixtures lie at a fixed path (canonicalRootNamed, by rootName),
// since a receipt's revision and event ids digest the artifact paths.
func compareGuard(t *testing.T, rootName, cases string) {
	t.Helper()
	names := strings.Split(cases, ",")
	root := canonicalRootNamed(t, rootName)
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg"))
	for _, name := range names {
		base := filepath.Join(root, name)
		if err := os.Mkdir(base, 0o700); err != nil {
			t.Fatal(err)
		}
		out, _ := layFixture(t, "guard-"+name, base)
		var fixture struct {
			Mode     string
			NoRecord bool
			Record   *string
		}
		if err := json.Unmarshal(out, &fixture); err != nil {
			t.Fatalf("%s: %v %s", name, err, out)
		}
		db := filepath.Join(base, "state", "relay.sqlite3")
		before := storeRows(t, db)
		raw, err := os.ReadFile(filepath.Join(base, "stop.json"))
		if err != nil {
			t.Fatal(err)
		}
		stop, err := decodeObject(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		markers, err := delivery.ResolveMarkerRoot(filepath.Join(base, "markers"))
		if err != nil {
			t.Fatal(err)
		}
		options := GuardOptions{Root: markers.Path, Now: "2026-01-01T00:06:00+00:00", Mode: fixture.Mode, NoRecord: fixture.NoRecord,
			DefaultDBPath: func() (string, error) { return db, nil }}
		verdict, err := Evaluate(context.Background(), stop, options)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var emitted bytes.Buffer
		if err := contract.Emit(&emitted, verdict); err != nil {
			t.Fatal(err)
		}
		// The FULL byte envelope, key order included, as cached adapters consume it.
		golden.Check(t, name+"/stdout", emitted.Bytes(), golden.Substitute(root, "<ROOT>"))
		if fixture.Record != nil {
			raw, err := os.ReadFile(filepath.Join(base, *fixture.Record))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			golden.Check(t, name+"/record", raw, golden.Substitute(root, "<ROOT>"))
		}
		if !slices.Equal(storeRows(t, db), before) {
			t.Fatalf("%s: the guard changed relay evidence", name)
		}
	}
	t.Logf("%d full byte envelopes and marker/DB effects equal: %s", len(names), strings.Join(names, ","))
}

// The cases' receipts were digested under the root of the test that ran them through the relay's
// guard-evaluate command, Test33GuardBinaryPython.
func Test33GuardEvaluatesEachFixture(t *testing.T) {
	compareGuard(t, "Test33GuardBinaryPython", "unmanaged,unregistered,ready_missing,ready_receipted,ready_staged,artifacts_changed,frozen,generation,dispatch,generation_absent,hold_spent,malformed_disposition,in_progress,no_record")
}

// A frozen copy nested deeper than json.loads can descend leaves guard.deliverable_state as the
// RecursionError its except clauses do not name, so the guard faults and releases, recorded. One
// level less is read, and the receipt stands at the head through its frozen copy.
func Test33GuardFrozenCopyAtTheDecoderDepth(t *testing.T) {
	compareGuard(t, t.Name(), "frozen_at_depth,frozen_past_depth")
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
