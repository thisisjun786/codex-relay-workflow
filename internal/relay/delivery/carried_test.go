package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Carried from todo 25A: test_cli.py CLI-5, CLI-7, CLI-9, CLI-21, CLI-38 (the emit/deliver/ack/
// verdict half) and test_registration_contention.py RCT-1, RCT-4 (the intent.bind marker-file
// half; the guard-evaluate half is todo 33's). `register` is todo 25's command, so the side is
// seeded with the store the Python registry created (copyCLISeed), as TestCLI_every_delivery_...
// is, and every stdout is then checked whole against the golden (stamps, ages and home masked).

// sameCLI runs one command and checks its exit code and stdout against the golden; it returns
// them, the stdout parsed.
func sameCLI(t *testing.T, side *cliSide, args ...string) (map[string]any, int) {
	t.Helper()
	expanded := make([]string, len(args))
	for i, a := range args {
		expanded[i] = strings.ReplaceAll(a, "<work>", side.work)
	}
	out, code := side.run(expanded...)
	side.expect("run "+strings.Join(args, " "), fmt.Sprintf("%d\n%s", code, ageless(side.normal(out))))
	var parsed map[string]any
	if strings.TrimSpace(out) != "" {
		mustDo(t, json.Unmarshal([]byte(out), &parsed))
	}
	return parsed, code
}

// ageSeconds are status's staged ages, read against the wall clock at the moment each process ran.
var ageSeconds = regexp.MustCompile(`"((?:oldestStaged)?[aA]geSeconds)": [0-9.e-]+`)

func ageless(text string) string { return ageSeconds.ReplaceAllString(text, `"$1": <age>`) }

// seededSide is a seeded side over tree/work: a parityTree where the goldens hold ids hashing the
// artifact paths.
func seededSide(t *testing.T, tree string) (*cliSide, string) {
	t.Helper()
	side := newSide(t, filepath.Join(tree, "work"))
	seeded := sqliteDump(t, side, "SELECT relationship_id FROM relationships")
	side.expect("sqlite SELECT relationship_id FROM relationships", seeded)
	rid := strings.Trim(seeded, "[]\"\n ")
	if !strings.HasPrefix(rid, "rel-") {
		t.Fatalf("the side is seeded with one relationship: %s", seeded)
	}
	return side, rid
}

func emitArgs(rid, turn, status, artifact string, extra ...string) []string {
	return append([]string{"emit", "--relationship", rid, "--generation", "1", "--outcome", "ready_for_review", "--turn-thread", child, "--turn-id", turn, "--turn-status", status, "--artifact", artifact}, extra...)
}

func Test25_CLI05_emit_stages_an_unconfirmed_claim_and_status_lists_nothing(t *testing.T) {
	t.Run("completed turn offline, then status", func(t *testing.T) {
		side, rid := seededSide(t, parityTree(t))
		mustDo(t, os.WriteFile(filepath.Join(side.work, "out.txt"), []byte("the deliverable"), 0o644))
		emitted, code := sameCLI(t, side, emitArgs(rid, dispatchTurn, "completed", "<work>/out.txt")...)
		if code != 0 || emitted["stage"] != "staged" || emitted["terminalProof"] != "unverified_staged" || emitted["observedTurnStatus"] != "inProgress" || emitted["receipt"].(map[string]any)["outcome"] != "ready_for_review" {
			t.Fatalf("emit %d %v", code, emitted)
		}
		if _, queued := emitted["delivery"]; queued {
			t.Fatal("nothing is queued on an unverified claim")
		}
		status, _ := sameCLI(t, side, "status")
		if deliveries, _ := status["deliveries"].([]any); len(deliveries) != 0 {
			t.Fatalf("a staged claim is not deliverable: %v", status["deliveries"])
		}
	})
	t.Run("live inProgress turn", func(t *testing.T) {
		side, rid := seededSide(t, parityTree(t))
		mustDo(t, os.WriteFile(filepath.Join(side.work, "out.txt"), []byte("still working"), 0o644))
		emitted, _ := sameCLI(t, side, emitArgs(rid, dispatchTurn, "inProgress", "<work>/out.txt")...)
		if _, queued := emitted["delivery"]; queued || emitted["stage"] != "staged" {
			t.Fatalf("emit %v", emitted)
		}
	})
}

func Test25_CLI07_a_later_turn_needs_a_continuation(t *testing.T) {
	side, rid := seededSide(t, parityTree(t))
	mustDo(t, os.WriteFile(filepath.Join(side.work, "out.txt"), []byte("finished later"), 0o644))
	refused, code := sameCLI(t, side, emitArgs(rid, "turn-loop-5", "completed", "<work>/out.txt")...)
	if code != 2 || refused["reason"] != "unassigned_turn" {
		t.Fatalf("refused %d %v", code, refused)
	}
	accepted, code := sameCLI(t, side, emitArgs(rid, "turn-loop-5", "completed", "<work>/out.txt",
		"--continues-anchor", dispatchTurn, "--continuation-actor", "child-loop", "--continuation-reason", "cycle 5 of this execution")...)
	if code != 0 || accepted["stage"] != "staged" || accepted["receipt"].(map[string]any)["outcome"] != "ready_for_review" {
		t.Fatalf("accepted %d %v", code, accepted)
	}
}

func Test25_CLI09_a_command_needing_the_host_says_so(t *testing.T) {
	side, _ := seededSide(t, t.TempDir())
	result, code := sameCLI(t, side, "deliver")
	if code != 4 || result["error"] != "usage" || !strings.Contains(result["detail"].(string), "--socket") {
		t.Fatalf("deliver %d %v", code, result)
	}
}

func Test25_CLI38_a_malformed_criteria_entry_with_restoration_is_refused(t *testing.T) {
	side, _ := seededSide(t, t.TempDir())
	for _, criteria := range []string{"[null]", `["c1"]`} {
		result, code := sameCLI(t, side, "verdict", "--event", strings.Repeat("e", 32), "--verdict", "needs_changes", "--verdict-turn", "v1", "--criteria", criteria, "--restoration", "c1")
		if code != 2 || result["reason"] != "disposition_conflict" {
			t.Fatalf("%s: %d %v", criteria, code, result)
		}
	}
}

// CLI-21: the CLI's verdict carries its sync outbox obligation. The Go `verdict` command wires
// VerdictSync before it rules (there is no lazily built service to forget it on); this proves
// the wiring through the real command: the sync_outbox row one verdict leaves, checked against the
// golden. The side stages an acknowledged completion through this package's services first, as
// the Python test staged it through its own.
func Test25_CLI21_the_verdict_command_is_wired_to_the_outbox(t *testing.T) {
	side, rid := seededSide(t, parityTree(t))
	event := stageAcknowledgedCompletion(t, side, rid)
	side.expect("stage acknowledged completion", event)
	record, code := sameCLI(t, side, "verdict", "--event", event, "--verdict", "verified", "--verdict-turn", "v1")
	if code != 0 || record["verdict"] != "verified" {
		t.Fatalf("verdict %d %v", code, record)
	}
	query := "SELECT sync_id, relationship_id, target, target_ref, subject_kind, event_id, verdict FROM sync_outbox ORDER BY 1"
	rows := sqliteDump(t, side, query)
	side.expect("sqlite "+query, rows)
	if !strings.Contains(rows, event) {
		t.Fatalf("sync_outbox holds no row of the verdict: %s", rows)
	}
}

// stageAcknowledgedCompletion is the CLI-21 Python script's fixture on the side's store, through
// this package's services: both tasks' settings, one accepted completion of work/out.txt, its
// delivery attempted and acknowledged, and the coordination-document sync target. It returns the
// event.
func stageAcknowledgedCompletion(t *testing.T, side *cliSide, rid string) string {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(side.state, "relay.sqlite3"), "")
	mustDo(t, err)
	defer func() { mustDo(t, s.Close()) }()
	clock := NewFakeClock()
	f := &fixture{t: t, ctx: ctx, tree: filepath.Dir(side.work), root: side.work, clock: clock, store: s, host: newFakeHost(clock)}
	f.intake = store.ReceiptIntake{Store: s, Now: clock.ISO, Minimum: store.BestEffortDetection}
	f.delivery = NewService(s, clock)
	f.host.addThread(parent)
	f.host.addThread(child)
	now := clock.ISO()
	for _, task := range []struct{ id, cwd string }{{parent, "/parent"}, {child, side.work}} {
		mustDo(t, s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
			if _, err := execSQL(ctx, s, "INSERT INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?) ON CONFLICT(task_id) DO UPDATE SET settings = excluded.settings, source = excluded.source, recorded_at = excluded.recorded_at", task.id, taskSettings(task.cwd), "creation_result", now); err != nil {
				return err
			}
			return journal(ctx, s, "settings_recorded", task.id, Obj{{Key: "source", Value: "creation_result"}}, now)
		}))
	}
	payload := f.readyPayload(rid, 1, []string{f.artifact("out.txt", "the deliverable")}, 1, assigned("completed"))
	_, err = f.accept(payload, store.AcceptOptions{})
	mustDo(t, err)
	event := pyjson.Text(payload.Get("eventId"))
	_, err = f.delivery.Enqueue(ctx, event, "", "")
	mustDo(t, err)
	f.mustAttempt(event, nil)
	clock.Advance(5)
	turn := f.host.startTurn(parent, "ack-turn", "inProgress", "")
	_, err = NewAck(f.delivery).Acknowledge(ctx, event, turn.TurnID, AckProof(event, turn.TurnID), true, nil, f.host)
	mustDo(t, err)
	_, err = execSQL(ctx, s, "INSERT INTO sync_targets (relationship_id, target, target_ref, recorded_at) VALUES (?,?,?,?)", rid, "coordination_document", "DOC-1", clock.ISO())
	mustDo(t, err)
	return event
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
	// The sequential shape of the same outcome, checked whole against the golden.
	sameOps(t, nil,
		declareOp(), bindOp(), markerOp{"op": "bind", "session": "01other-session", "task": "01other-task"},
		markerOp{"op": "state"}, markerOp{"op": "contested"},
		markerOp{"op": "resolution", "facts": []any{"conflicts/0"}, "task": task1, "session": session1},
		markerOp{"op": "contested"},
	)
	// The race itself, in Go.
	root, work := filepath.Join(t.TempDir(), "markers"), t.TempDir()
	t.Setenv(MarkerEnv, "")
	_, err := DeclareIntent(context.Background(), root, IntentDeclaration{Workspace: work, DispatchRequestID: "dispatch-request-1", IssueKey: "REL-1", DeclaredAt: "2026-01-01T00:00:00+00:00"})
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
			outcomes[i], errs[i] = BindIdentity(context.Background(), root, work, assignment, who[0], who[1], at)
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	for _, err := range errs {
		mustDo(t, err)
	}
	results := []string{pyjson.Text(outcomes[0].Get("outcome")), pyjson.Text(outcomes[1].Get("outcome"))}
	if !(results[0] == Bound && results[1] == Conflict) && !(results[0] == Conflict && results[1] == Bound) {
		t.Fatalf("two concurrent binds must settle as one winner and one conflict: %v", outcomes)
	}
	directory, err := AssignmentDir(root, work, assignment)
	mustDo(t, err)
	facts, _ := ReadAssignment(context.Background(), directory)
	bound := sub(facts, "bound")
	conflicts, _ := field(facts, "conflicts").([]any)
	if len(conflicts) != 1 || pyjson.Text(conflicts[0].(Obj).Get("attemptedSessionId")) == pyjson.Text(bound.Get("sessionId")) {
		t.Fatalf("one recorded conflict naming the loser: %v", conflicts)
	}
	loser := outcomes[0]
	if results[1] == Conflict {
		loser = outcomes[1]
	}
	if pyjson.Text(loser.Get("boundSessionId")) != pyjson.Text(bound.Get("sessionId")) {
		t.Fatalf("the loser is told the winner: %v", loser)
	}
	if DeriveAssignmentState(facts, at) != IdentityBound || !IdentityContested(facts) {
		t.Fatalf("state %s contested %v", DeriveAssignmentState(facts, at), IdentityContested(facts))
	}
	fact := conflicts[0].(Obj)
	_, err = PublishResolution(context.Background(), root, work, assignment, pyjson.Text(bound.Get("taskId")), pyjson.Text(bound.Get("sessionId")), "the winning link() is the identity", at,
		[]Obj{{{Key: "factId", Value: field(fact, "factId")}, {Key: "digest", Value: FactDigest(fact)}}})
	mustDo(t, err)
	resolved, _ := ReadAssignment(context.Background(), directory)
	if IdentityContested(resolved) {
		t.Fatal("a resolution naming the bound identity did not settle the contest")
	}
}
