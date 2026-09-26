package delivery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// Carried from todo 25A: test_cli.py CLI-5, CLI-7, CLI-9, CLI-21, CLI-38 (the emit/deliver/ack/
// verdict half) and test_registration_contention.py RCT-1, RCT-4 (the intent.bind marker-file
// half; the guard-evaluate half is todo 33's). `register` is todo 25's command, so both sides are
// seeded by testdata/cliseed.py through the real Python registry, as TestCLI_every_delivery_...
// does, and every stdout is then compared whole with Python's (stamps and home masked).

// sameCLI runs one command on both sides and requires the same exit code and the same stdout.
func sameCLI(t *testing.T, py, gosd *cliSide, args ...string) (map[string]any, int) {
	t.Helper()
	expand := func(s *cliSide) []string {
		out := make([]string, len(args))
		for i, a := range args {
			out[i] = strings.ReplaceAll(a, "<work>", s.work)
		}
		return out
	}
	pout, pcode := py.run(expand(py)...)
	gout, gcode := gosd.run(expand(gosd)...)
	if pcode != gcode || ageless(py.normal(pout)) != ageless(gosd.normal(gout)) {
		t.Fatalf("%v: exit python %d go %d\npython:\n%s\ngo:\n%s", args, pcode, gcode, py.normal(pout), gosd.normal(gout))
	}
	var parsed map[string]any
	if strings.TrimSpace(pout) != "" {
		mustDo(t, json.Unmarshal([]byte(pout), &parsed))
	}
	return parsed, pcode
}

// ageSeconds are status's staged ages, read against the wall clock at the moment each process ran.
var ageSeconds = regexp.MustCompile(`"((?:oldestStaged)?[aA]geSeconds)": [0-9.e-]+`)

func ageless(text string) string { return ageSeconds.ReplaceAllString(text, `"$1": <age>`) }

func seededSides(t *testing.T) (*cliSide, *cliSide, string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	py, gosd := newSide(t, true, work), newSide(t, false, work)
	rid := strings.Trim(sqliteDump(t, py, "SELECT relationship_id FROM relationships"), "[]\"\n ")
	if !strings.HasPrefix(rid, "rel-") || !strings.Contains(sqliteDump(t, gosd, "SELECT relationship_id FROM relationships"), rid) {
		t.Fatalf("both sides are seeded with the same relationship: %s", rid)
	}
	return py, gosd, rid
}

func emitArgs(rid, turn, status, artifact string, extra ...string) []string {
	return append([]string{"emit", "--relationship", rid, "--generation", "1", "--outcome", "ready_for_review", "--turn-thread", child, "--turn-id", turn, "--turn-status", status, "--artifact", artifact}, extra...)
}

func Test25_CLI05_emit_stages_an_unconfirmed_claim_and_status_lists_nothing(t *testing.T) {
	t.Run("completed turn offline, then status", func(t *testing.T) {
		py, gosd, rid := seededSides(t)
		mustDo(t, os.WriteFile(filepath.Join(py.work, "out.txt"), []byte("the deliverable"), 0o644))
		emitted, code := sameCLI(t, py, gosd, emitArgs(rid, dispatchTurn, "completed", "<work>/out.txt")...)
		if code != 0 || emitted["stage"] != "staged" || emitted["terminalProof"] != "unverified_staged" || emitted["observedTurnStatus"] != "inProgress" || emitted["receipt"].(map[string]any)["outcome"] != "ready_for_review" {
			t.Fatalf("emit %d %v", code, emitted)
		}
		if _, queued := emitted["delivery"]; queued {
			t.Fatal("nothing is queued on an unverified claim")
		}
		status, _ := sameCLI(t, py, gosd, "status")
		if deliveries, _ := status["deliveries"].([]any); len(deliveries) != 0 {
			t.Fatalf("a staged claim is not deliverable: %v", status["deliveries"])
		}
	})
	t.Run("live inProgress turn", func(t *testing.T) {
		py, gosd, rid := seededSides(t)
		mustDo(t, os.WriteFile(filepath.Join(py.work, "out.txt"), []byte("still working"), 0o644))
		emitted, _ := sameCLI(t, py, gosd, emitArgs(rid, dispatchTurn, "inProgress", "<work>/out.txt")...)
		if _, queued := emitted["delivery"]; queued || emitted["stage"] != "staged" {
			t.Fatalf("emit %v", emitted)
		}
	})
}

func Test25_CLI07_a_later_turn_needs_a_continuation(t *testing.T) {
	py, gosd, rid := seededSides(t)
	mustDo(t, os.WriteFile(filepath.Join(py.work, "out.txt"), []byte("finished later"), 0o644))
	refused, code := sameCLI(t, py, gosd, emitArgs(rid, "turn-loop-5", "completed", "<work>/out.txt")...)
	if code != 2 || refused["reason"] != "unassigned_turn" {
		t.Fatalf("refused %d %v", code, refused)
	}
	accepted, code := sameCLI(t, py, gosd, emitArgs(rid, "turn-loop-5", "completed", "<work>/out.txt",
		"--continues-anchor", dispatchTurn, "--continuation-actor", "child-loop", "--continuation-reason", "cycle 5 of this execution")...)
	if code != 0 || accepted["stage"] != "staged" || accepted["receipt"].(map[string]any)["outcome"] != "ready_for_review" {
		t.Fatalf("accepted %d %v", code, accepted)
	}
}

func Test25_CLI09_a_command_needing_the_host_says_so(t *testing.T) {
	py, gosd, _ := seededSides(t)
	result, code := sameCLI(t, py, gosd, "deliver")
	if code != 4 || result["error"] != "usage" || !strings.Contains(result["detail"].(string), "--socket") {
		t.Fatalf("deliver %d %v", code, result)
	}
}

func Test25_CLI38_a_malformed_criteria_entry_with_restoration_is_refused(t *testing.T) {
	py, gosd, _ := seededSides(t)
	for _, criteria := range []string{"[null]", `["c1"]`} {
		result, code := sameCLI(t, py, gosd, "verdict", "--event", strings.Repeat("e", 32), "--verdict", "needs_changes", "--verdict-turn", "v1", "--criteria", criteria, "--restoration", "c1")
		if code != 2 || result["reason"] != "disposition_conflict" {
			t.Fatalf("%s: %d %v", criteria, code, result)
		}
	}
}

// CLI-21: the CLI's verdict carries its sync outbox obligation. The Go `verdict` command wires
// VerdictSync before it rules (there is no lazily built service to forget it on); this proves
// the wiring through the real command: the same sync_outbox row as Python's for one verdict.
func Test25_CLI21_the_verdict_command_is_wired_to_the_outbox(t *testing.T) {
	py, gosd, rid := seededSides(t)
	event := ""
	for _, side := range []*cliSide{py, gosd} {
		// The same acknowledged completion on both sides, through the real Python services, and
		// the coordination-document target the outbox answers to.
		script := `
import sys
from pathlib import Path
from codex_session_relay import identity
from codex_session_relay.clock import FakeClock
from codex_session_relay.ack import AckService
from codex_session_relay.delivery import DeliveryService
from codex_session_relay.fakehost import FakeHostAdapter
from codex_session_relay.receipts import ReceiptIntake, TurnRef
from codex_session_relay.registry import Registry, record_settings
from tests.support import task_settings
from codex_session_relay import manifest
from codex_session_relay.store import Store
state, work, rid = sys.argv[1:4]
store = Store(state + "/relay.sqlite3"); clock = FakeClock(); registry = Registry(store, clock)
intake = ReceiptIntake(store, registry, clock)
delivery = DeliveryService(store, registry, intake, clock)
ack = AckService(store, registry, intake, delivery, clock)
record_settings(store, clock, "01parent-task", task_settings("/parent"), source="creation_result")
record_settings(store, clock, "01child-task", task_settings(work), source="creation_result")
adapter = FakeHostAdapter(clock); adapter.add_thread("01parent-task"); adapter.add_thread("01child-task")
Path(work, "out.txt").write_text("the deliverable")
entries, _ = manifest.build([work + "/out.txt"], [work])
digest = manifest.revision_hash(entries)
event = identity.event_id(rid, 1, digest, "ready_for_review", turn_id="turn-dispatch-1", attempt=1)
payload = {"eventId": event, "relationshipId": rid, "executionGeneration": 1, "attempt": 1, "revisionHash": digest,
  "outcome": "ready_for_review", "producer": "child", "turnRef": {"threadId": "01child-task", "turnId": "turn-dispatch-1", "turnStatus": "completed"},
  "manifest": [{"path": e.path, "sha256": e.sha256, "bytes": e.bytes} for e in entries], "emittedAt": clock.iso()}
intake.accept_child_receipt(payload, observation=TurnRef("01child-task", "turn-dispatch-1", "completed"))
delivery.enqueue(event); delivery.attempt(event, adapter); clock.advance(5)
turn = adapter.start_turn("01parent-task", turn_id="ack-turn", status="inProgress")
ack.acknowledge(event, ack_turn_id="ack-turn", ack_proof=identity.ack_proof(event, "ack-turn"), accepted=True, adapter=adapter)
store.db.execute("INSERT INTO sync_targets (relationship_id, target, target_ref, recorded_at) VALUES (?,?,?,?)", (rid, "coordination_document", "DOC-1", clock.iso()))
store.db.commit() if store.db.in_transaction else None
store.close()
print(event)
`
		out, err := execUV(side, "python", "-c", script, side.state, side.work, rid).CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		event = strings.TrimSpace(string(out))
	}
	record, code := sameCLI(t, py, gosd, "verdict", "--event", event, "--verdict", "verified", "--verdict-turn", "v1")
	if code != 0 || record["verdict"] != "verified" {
		t.Fatalf("verdict %d %v", code, record)
	}
	query := "SELECT sync_id, relationship_id, target, target_ref, subject_kind, event_id, verdict FROM sync_outbox ORDER BY 1"
	pr, gr := sqliteDump(t, py, query), sqliteDump(t, gosd, query)
	if pr != gr || !strings.Contains(pr, event) {
		t.Fatalf("sync_outbox differs or is empty\npython: %s\ngo:     %s", pr, gr)
	}
}

// ---------------------------------------------------------------- test_registration_contention.py

// RCT-1 (marker half): a claim published before the bind, then the late bind and the relationship
// registration, leave the marker facts the fold reads: one claim, one bound identity, the
// registration, and the derived state moving from claimed-unbound to registered. The Stop
// decisions and hold files around it are guard.evaluate (todo 33).
func Test25_RCT01_a_late_bind_lands_on_the_claimed_assignment(t *testing.T) {
	answers := sameOps(t, nil,
		declareOp(),
		markerOp{"op": "claim", "session": session1},
		markerOp{"op": "state"},
		markerOp{"op": "correlated", "session": session1},
		bindOp(), openOp(), registerOp(),
		markerOp{"op": "state"},
		markerOp{"op": "facts"},
	)
	if ok(t, answers[2]) == ok(t, answers[7]) {
		t.Fatalf("the late bind and registration did not move the assignment: %v -> %v", answers[2], answers[7])
	}
	facts := ok(t, answers[8]).(map[string]any)["facts"].(map[string]any)
	if facts["bound"].(map[string]any)["sessionId"] != session1 || len(facts["claims"].([]any)) != 1 {
		t.Fatalf("facts %v", facts)
	}
}

// RCT-4: two concurrent binds on one assignment, from two goroutines released together: one
// bound and one conflict, one recorded conflict naming the loser, the loser told the winner,
// state identity_bound and contested until a resolution naming the winner.
func Test25_RCT04_two_concurrent_binds_leave_one_winner_and_one_recorded_conflict(t *testing.T) {
	// The sequential shape of the same outcome, compared whole with Python.
	sameOps(t, nil,
		declareOp(), bindOp(), markerOp{"op": "bind", "session": "01other-session", "task": "01other-task"},
		markerOp{"op": "state"}, markerOp{"op": "contested"},
		markerOp{"op": "resolution", "facts": []any{"conflicts/0"}, "task": task1, "session": session1},
		markerOp{"op": "contested"},
	)
	// The race itself, in Go.
	root, work := filepath.Join(t.TempDir(), "markers"), t.TempDir()
	t.Setenv(MarkerEnv, "")
	_, err := DeclareIntent(root, IntentDeclaration{Workspace: work, DispatchRequestID: "dispatch-request-1", IssueKey: "REL-1", DeclaredAt: "2026-01-01T00:00:00+00:00"})
	mustDo(t, err)
	assignment := AssignmentID("dispatch-request-1")
	const at = "2026-01-01T00:00:00+00:00"
	outcomes := make([]Obj, 2)
	errs := make([]error, 2)
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	for i, who := range [][2]string{{child, child}, {"01other-session", "01other-task"}} {
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			outcomes[i], errs[i] = BindIdentity(root, work, assignment, who[0], who[1], at)
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	for _, err := range errs {
		mustDo(t, err)
	}
	results := []string{str(outcomes[0], "outcome"), str(outcomes[1], "outcome")}
	if !(results[0] == Bound && results[1] == Conflict) && !(results[0] == Conflict && results[1] == Bound) {
		t.Fatalf("two concurrent binds must settle as one winner and one conflict: %v", outcomes)
	}
	directory, err := AssignmentDir(root, work, assignment)
	mustDo(t, err)
	facts, _ := ReadAssignment(directory)
	bound := sub(facts, "bound")
	conflicts, _ := field(facts, "conflicts").([]any)
	if len(conflicts) != 1 || str(conflicts[0].(Obj), "attemptedSessionId") == str(bound, "sessionId") {
		t.Fatalf("one recorded conflict naming the loser: %v", conflicts)
	}
	loser := outcomes[0]
	if results[1] == Conflict {
		loser = outcomes[1]
	}
	if str(loser, "boundSessionId") != str(bound, "sessionId") {
		t.Fatalf("the loser is told the winner: %v", loser)
	}
	if DeriveAssignmentState(facts, at) != IdentityBound || !IdentityContested(facts) {
		t.Fatalf("state %s contested %v", DeriveAssignmentState(facts, at), IdentityContested(facts))
	}
	fact := conflicts[0].(Obj)
	_, err = PublishResolution(root, work, assignment, str(bound, "taskId"), str(bound, "sessionId"), "the winning link() is the identity", at,
		[]Obj{{{Key: "factId", Value: field(fact, "factId")}, {Key: "digest", Value: FactDigest(fact)}}})
	mustDo(t, err)
	resolved, _ := ReadAssignment(directory)
	if IdentityContested(resolved) {
		t.Fatal("a resolution naming the bound identity did not settle the contest")
	}
}
