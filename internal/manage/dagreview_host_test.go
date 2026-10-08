package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// dagHostLine is one rollout line: the JSON the harness writes, built from a value so a test
// spells a call id or an output without quoting it by hand.
func dagHostLine(t *testing.T, value map[string]any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// dagHostResponseItem is a response_item rollout line.
func dagHostResponseItem(t *testing.T, payload map[string]any) string {
	t.Helper()
	return dagHostLine(t, map[string]any{"type": "response_item", "payload": payload})
}

// dagHostWriteRollout writes a rollout file below dir and returns its path.
func dagHostWriteRollout(t *testing.T, dir, name string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// dagHostAppendRollout appends lines to a rollout.
func dagHostAppendRollout(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// dagHostToolCall is a function_call line for a dag- relay command.
func dagHostToolCall(t *testing.T, callID, arguments string) string {
	t.Helper()
	return dagHostResponseItem(t, map[string]any{"type": "function_call", "call_id": callID, "name": "exec", "arguments": arguments})
}

// dagHostToolOutput is a function_call_output line whose output is a JSON string.
func dagHostToolOutput(t *testing.T, callID, output string) string {
	t.Helper()
	return dagHostResponseItem(t, map[string]any{"type": "function_call_output", "call_id": callID, "output": output})
}

// dagHostConfig points a review at the fixture store, the fake host's socket, the parent map and
// the state directory the offsets live under.
func dagHostConfig(t *testing.T, f *dagReviewFixture, socket string, parents map[string]string, stateDir string, receiptMinutes int) *Config {
	t.Helper()
	cfg := dagReviewConfig(t, f.dir, nil, 0)
	cfg.Relay.Socket = socket
	cfg.Parents = parents
	cfg.StateDir = stateDir
	if receiptMinutes > 0 {
		encoded, err := json.Marshal(map[string]any{"receipt_minutes": receiptMinutes})
		if err != nil {
			t.Fatal(err)
		}
		cfg.raw["dag_review"] = encoded
	}
	return cfg
}

// dagHostRun runs the review over the fixture and fails the test on a read error.
func dagHostRun(t *testing.T, ctx context.Context, f *dagReviewFixture, cfg *Config) Review {
	t.Helper()
	review, err := DagReview(ctx, dagReviewEnv(t), cfg)
	if err != nil {
		t.Fatalf("DagReview: %v", err)
	}
	return review
}

// dagHostCheckFind returns the checks of one name.
func dagHostCheckFind(review Review, name string) []Check {
	var found []Check
	for _, check := range review.Checks {
		if check.Name == name {
			found = append(found, check)
		}
	}
	return found
}

// dagHostUnmeasured reports whether a check of that name is unmeasured.
func dagHostUnmeasured(review Review, name string) bool {
	for _, check := range dagHostCheckFind(review, name) {
		if check.State == dagReviewUnmeasured {
			return true
		}
	}
	return false
}

// C1 red: a parent thread in systemError is reported, and the same store with an idle parent
// raises nothing.
func TestDagHostParentSystemError(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     string
		wantRaised bool
	}{
		{"systemError", "systemError", true},
		{"idle", "idle", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := dagReviewNewFixture(t)
			rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl", dagHostResponseItem(t, map[string]any{"type": "message", "role": "user"}))
			f.close()
			host := fakehost.Start(t)
			host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
				"status": map[string]any{"type": test.status}, "path": rollout,
			}}})
			cfg := dagHostConfig(t, f, host.SocketPath, map[string]string{"parent-1": "P-ONE"}, "", 0)

			found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentSystemError)
			if test.wantRaised {
				if len(found) != 1 || found[0].Issue != "P-ONE" {
					t.Fatalf("parent_system_error = %+v, want one for P-ONE", found)
				}
			} else if len(found) != 0 {
				t.Fatalf("an idle parent was reported: %+v", found)
			}
		})
	}
}

// C1 red: two tool results for one call id in a parent's rollout are reported with the count, and
// one result per call id raises nothing.
func TestDagHostDuplicateToolOutputs(t *testing.T) {
	for _, test := range []struct {
		name       string
		lines      []string
		wantRaised bool
	}{
		{"two results for one call", []string{
			dagHostToolCall(t, "call-1", "crw relay status"),
			dagHostToolOutput(t, "call-1", "first"),
			dagHostToolOutput(t, "call-1", "second"),
		}, true},
		{"one result per call", []string{
			dagHostToolCall(t, "call-1", "crw relay status"),
			dagHostToolOutput(t, "call-1", "only"),
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := dagReviewNewFixture(t)
			rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl", test.lines...)
			f.close()
			host := fakehost.Start(t)
			host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
				"status": map[string]any{"type": "idle"}, "path": rollout,
			}}})
			cfg := dagHostConfig(t, f, host.SocketPath, map[string]string{"parent-1": "P-ONE"}, "", 0)

			found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindDuplicateToolOutputs)
			if test.wantRaised {
				if len(found) != 1 || !strings.Contains(found[0].Detail, "call-1") || !strings.Contains(found[0].Detail, "2") {
					t.Fatalf("duplicate_tool_outputs = %+v, want one for call-1 with count 2", found)
				}
			} else if len(found) != 0 {
				t.Fatalf("a single tool result per call id was reported: %+v", found)
			}
		})
	}
}

// dagHostRelationship inserts an active relationship whose child thread is thread.
func dagHostRelationship(f *dagReviewFixture, relationshipID, issue, thread string, generation int) {
	f.exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES (?,?,'active','parent','host',?,'host',?,'[]','[]',?,?)",
		relationshipID, issue, thread, generation, dagReviewAt(0), dagReviewAt(0))
	// The generation row is written with the relationship because the registry refuses a
	// relationship whose generation the store does not carry, and the scheduler now reads a
	// node's relationship through the registry when the review asks it for the plan's progress.
	f.exec("INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at) VALUES (?,?,?,'bound',?,NULL,?,?)",
		relationshipID, generation, "dispatch-"+relationshipID, "turn-"+thread, dagReviewAt(0), dagReviewAt(0))
}

// dagHostReceipt inserts one child receipt event.
func dagHostReceipt(f *dagReviewFixture, eventID, relationshipID string, generation int, stage string, suppressed any) {
	f.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, suppressed_reason, first_seen_at, last_seen_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		eventID, relationshipID, generation, "rev", "ready_for_review", "child", "child-thread-1", "turn-1", "completed", "{}", stage, suppressed, dagReviewAt(0), dagReviewAt(0))
}

// dagHostTurnPage answers thread/turns/list with one terminal turn that ended at ended.
func dagHostTurnPage(host *fakehost.Server, ended time.Time) {
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{
		"id": "turn-1", "status": "completed", "startedAt": float64(ended.Unix() - 60), "completedAt": float64(ended.Unix()),
	}}, "nextCursor": nil}})
}

// C1 red: an active child whose newest turn ended past the threshold with no accepted receipt is
// reported; a receipt in that generation, a recent turn, a staged receipt and a suppressed receipt
// each behave as the design fixes.
func TestDagHostChildTurnWithoutReceipt(t *testing.T) {
	now := dagReviewNow()
	cases := []struct {
		name       string
		ended      time.Time
		receipt    func(f *dagReviewFixture)
		wantRaised bool
	}{
		{"no receipt past the threshold", now.Add(-30 * time.Minute), nil, true},
		{"a final receipt", now.Add(-30 * time.Minute), func(f *dagReviewFixture) {
			dagHostReceipt(f, "event-1", "rel-1", 1, "final", nil)
		}, false},
		{"a turn inside the threshold", now.Add(-5 * time.Minute), nil, false},
		{"a staged receipt only", now.Add(-30 * time.Minute), func(f *dagReviewFixture) {
			dagHostReceipt(f, "event-1", "rel-1", 1, "staged", nil)
		}, true},
		{"a suppressed receipt only", now.Add(-30 * time.Minute), func(f *dagReviewFixture) {
			dagHostReceipt(f, "event-1", "rel-1", 1, "final", "the turn ended failed")
		}, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := dagReviewNewFixture(t)
			dagHostRelationship(f, "rel-1", "CRW-CHILD", "child-thread-1", 1)
			if test.receipt != nil {
				test.receipt(f)
			}
			f.close()
			host := fakehost.Start(t)
			dagHostTurnPage(host, test.ended)
			cfg := dagHostConfig(t, f, host.SocketPath, nil, "", 0)

			found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindChildTurnWithoutReceipt)
			if test.wantRaised {
				if len(found) != 1 || found[0].Issue != "CRW-CHILD" {
					t.Fatalf("child_turn_without_receipt = %+v, want one for CRW-CHILD", found)
				}
			} else if len(found) != 0 {
				t.Fatalf("a child with a receipt or a recent turn was reported: %+v", found)
			}
		})
	}
}

// A newest turn still in progress has no end, so it raises nothing.
func TestDagHostInProgressTurnRaisesNothing(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagHostRelationship(f, "rel-1", "CRW-CHILD", "child-thread-1", 1)
	f.close()
	host := fakehost.Start(t)
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{
		"id": "turn-1", "status": "inProgress", "startedAt": float64(dagReviewNow().Add(-2 * time.Hour).Unix()),
	}}, "nextCursor": nil}})
	cfg := dagHostConfig(t, f, host.SocketPath, nil, "", 0)

	if found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindChildTurnWithoutReceipt); len(found) != 0 {
		t.Fatalf("an in-progress turn was reported: %+v", found)
	}
}

// dagHostRefusalRollout is a rollout holding one refused dag- command.
func dagHostRefusalRollout(t *testing.T) []string {
	t.Helper()
	return []string{
		dagHostToolCall(t, "call-1", "crw relay dag-release --plan p1"),
		dagHostToolOutput(t, "call-1", `{"ok": false, "reason": "stale_coordinator_epoch"}`),
	}
}

// C1 red and C3: a refusal that appeared since the last check is reported once, and the offset
// keeps it from being reported again.
func TestDagHostParentDagRefusalsAndTheOffset(t *testing.T) {
	f := dagReviewNewFixture(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl", dagHostResponseItem(t, map[string]any{"type": "message", "role": "user"}))
	f.close()
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"status": map[string]any{"type": "idle"}, "path": rollout,
	}}})
	cfg := dagHostConfig(t, f, host.SocketPath, map[string]string{"parent-1": "P-ONE"}, stateDir, 0)

	// The first check records the offset at the rollout's end, so its earlier history is not
	// re-reported.
	if found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals); len(found) != 0 {
		t.Fatalf("a first-seen rollout reported its history: %+v", found)
	}
	dagHostAppendRollout(t, rollout, dagHostRefusalRollout(t)...)

	found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals)
	if len(found) != 1 || !strings.Contains(found[0].Detail, "stale_coordinator_epoch") {
		t.Fatalf("parent_dag_refusals = %+v, want one naming the refusal reason", found)
	}

	if again := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals); len(again) != 0 {
		t.Fatalf("the same refusal was reported twice: %+v", again)
	}
}

// --no-state reports the refusal again (no offset is read) and writes no state file.
func TestDagHostNoStateWritesNoFile(t *testing.T) {
	f := dagReviewNewFixture(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl", dagHostRefusalRollout(t)...)
	f.close()
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"status": map[string]any{"type": "idle"}, "path": rollout,
	}}})
	cfg := dagHostConfig(t, f, host.SocketPath, map[string]string{"parent-1": "P-ONE"}, stateDir, 0)
	ctx := dagHostNoState(context.Background(), true)

	for run := 0; run < 2; run++ {
		if found := dagReviewFind(dagHostRun(t, ctx, f, cfg), dagHostKindParentDagRefusals); len(found) != 1 {
			t.Fatalf("run %d: parent_dag_refusals = %+v, want one every run without state", run, found)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, dagHostStateFile)); !os.IsNotExist(err) {
		t.Fatalf("--no-state wrote the offset file: %v", err)
	}
}

// C2 red: a failed App Server read is an unmeasured check, never an anomaly.
func TestDagHostReadFailureIsUnmeasured(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagHostRelationship(f, "rel-1", "CRW-CHILD", "child-thread-1", 1)
	f.close()
	// An empty socket cannot be reached, so every host read fails.
	cfg := dagHostConfig(t, f, "", map[string]string{"parent-1": "P-ONE"}, "", 0)

	review := dagHostRun(t, context.Background(), f, cfg)
	if len(review.Anomalies) != 0 {
		t.Fatalf("a failed read produced anomalies: %+v", review.Anomalies)
	}
	if !dagHostUnmeasured(review, "parent_thread:parent-1") {
		t.Errorf("the parent thread read is not an unmeasured check: %+v", review.Checks)
	}
	if !dagHostUnmeasured(review, "child_turn:child-thread-1") {
		t.Errorf("the child turn read is not an unmeasured check: %+v", review.Checks)
	}
}

// A thread that names no rollout path is an unmeasured check rather than a silent pass.
func TestDagHostThreadWithoutAPathIsUnmeasured(t *testing.T) {
	f := dagReviewNewFixture(t)
	f.close()
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"status": map[string]any{"type": "idle"},
	}}})
	cfg := dagHostConfig(t, f, host.SocketPath, map[string]string{"parent-1": "P-ONE"}, "", 0)

	if !dagHostUnmeasured(dagHostRun(t, context.Background(), f, cfg), "parent_rollout:parent-1") {
		t.Fatal("a thread without a rollout path was not reported as unmeasured")
	}
}

// The receipt threshold comes from the section, so a turn inside it raises nothing and one past it
// is reported.
func TestDagHostReceiptThresholdComesFromTheSection(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagHostRelationship(f, "rel-1", "CRW-CHILD", "child-thread-1", 1)
	f.close()
	host := fakehost.Start(t)
	dagHostTurnPage(host, dagReviewNow().Add(-30*time.Minute))

	if found := dagReviewFind(dagHostRun(t, context.Background(), f, dagHostConfig(t, f, host.SocketPath, nil, "", 60)), dagHostKindChildTurnWithoutReceipt); len(found) != 0 {
		t.Fatalf("a turn inside the configured threshold was reported: %+v", found)
	}
	if found := dagReviewFind(dagHostRun(t, context.Background(), f, dagHostConfig(t, f, host.SocketPath, nil, "", 10)), dagHostKindChildTurnWithoutReceipt); len(found) != 1 {
		t.Fatalf("a turn past the configured threshold was not reported: %+v", found)
	}
}

// The normal state raises nothing: a parent in a healthy state with one tool result per call and
// an active child holding a receipt.
func TestDagHostNormalStateRaisesNothing(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	dagHostRelationship(f, "rel-1", "CRW-CHILD", "child-thread-1", 1)
	dagHostReceipt(f, "event-1", "rel-1", 1, "final", nil)
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl",
		dagHostToolCall(t, "call-1", "crw relay status"),
		dagHostToolOutput(t, "call-1", `{"ok": true, "reason": null}`))
	f.close()
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"status": map[string]any{"type": "idle"}, "path": rollout,
	}}})
	dagHostTurnPage(host, dagReviewNow().Add(-5*time.Minute))
	cfg := dagHostConfig(t, f, host.SocketPath, map[string]string{"parent-1": "P-ONE"}, "", 0)

	review := dagHostRun(t, context.Background(), f, cfg)
	for _, kind := range []string{dagHostKindParentSystemError, dagHostKindDuplicateToolOutputs, dagHostKindChildTurnWithoutReceipt, dagHostKindParentDagRefusals} {
		if found := dagReviewFind(review, kind); len(found) != 0 {
			t.Fatalf("the normal state raised %s: %+v", kind, found)
		}
	}
}

// dagHostSeedOffsets writes an offset file that already knows path at byte 0, so a test's rollout
// is scanned from its start as a known rollout rather than treated as first-seen.
func dagHostSeedOffsets(t *testing.T, stateDir, path string) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	document := map[string]any{"offsets": map[string]int64{path: 0}}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, dagHostStateFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// dagHostRelayRefusal is a rollout holding one relay dag- call and the refusal envelope the relay
// itself writes (internal/relay/dispatch/answer.go emit: {"error":"refused","reason":...}).
func dagHostRelayRefusal(t *testing.T, callID string) []string {
	t.Helper()
	return []string{
		dagHostToolCall(t, callID, "crw relay dag-release --plan p1"),
		dagHostToolOutput(t, callID, `{"error":"refused","reason":"stale_coordinator_epoch","detail":"epoch 0"}`),
	}
}

// The relay's own refusal envelope is reported, and a dag- word that is not a relay invocation is
// not.
func TestDagHostRefusalEnvelopeAndCommandMatching(t *testing.T) {
	cases := []struct {
		name       string
		lines      []string
		wantRaised bool
	}{
		{"the relay refusal envelope", dagHostRelayRefusal(t, "call-1"), true},
		{"a dag- word that is not a relay call", []string{
			dagHostToolCall(t, "call-1", "cat notes/dag-plan.md"),
			dagHostToolOutput(t, "call-1", `{"error":"refused","reason":"not_found"}`),
		}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := dagReviewNewFixture(t)
			rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl", test.lines...)
			f.close()
			stateDir := t.TempDir()
			dagHostSeedOffsets(t, stateDir, rollout)
			host := fakehost.Start(t)
			host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
				"status": map[string]any{"type": "idle"}, "path": rollout,
			}}})
			cfg := dagHostConfig(t, f, host.SocketPath, map[string]string{"parent-1": "P-ONE"}, stateDir, 0)

			found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals)
			if test.wantRaised {
				if len(found) != 1 || !strings.Contains(found[0].Detail, "stale_coordinator_epoch") {
					t.Fatalf("parent_dag_refusals = %+v, want one for the relay envelope", found)
				}
			} else if len(found) != 0 {
				t.Fatalf("a non-relay dag- word was reported: %+v", found)
			}
		})
	}
}

// An unreadable offset file leaves the refusal reading unmeasured rather than replaying every
// historical refusal as a new anomaly.
func TestDagHostUnreadableOffsetsDoNotReplay(t *testing.T) {
	f := dagReviewNewFixture(t)
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, dagHostStateFile), []byte("{\"offsets\":{"), 0o600); err != nil {
		t.Fatal(err)
	}
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl", dagHostRelayRefusal(t, "call-1")...)
	f.close()
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"status": map[string]any{"type": "idle"}, "path": rollout,
	}}})
	cfg := dagHostConfig(t, f, host.SocketPath, map[string]string{"parent-1": "P-ONE"}, stateDir, 0)

	review := dagHostRun(t, context.Background(), f, cfg)
	if found := dagReviewFind(review, dagHostKindParentDagRefusals); len(found) != 0 {
		t.Fatalf("an unreadable offset file replayed historical refusals: %+v", found)
	}
	if !dagHostUnmeasured(review, "parent_refusals:parent-1") {
		t.Fatalf("the refusal reading was not unmeasured: %+v", review.Checks)
	}
}

// A relay call whose output has not been written yet stays pending, so the next check still pairs
// the call with its refusal.
func TestDagHostPendingCallIsNotSkipped(t *testing.T) {
	f := dagReviewNewFixture(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl", dagHostResponseItem(t, map[string]any{"type": "message", "role": "user"}))
	f.close()
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"status": map[string]any{"type": "idle"}, "path": rollout,
	}}})
	cfg := dagHostConfig(t, f, host.SocketPath, map[string]string{"parent-1": "P-ONE"}, stateDir, 0)

	// The first check records the offset at the rollout's end.
	if found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals); len(found) != 0 {
		t.Fatalf("the first check raised an anomaly: %+v", found)
	}
	// A relay call is written; its output is not there yet.
	dagHostAppendRollout(t, rollout, dagHostToolCall(t, "call-1", "crw relay dag-release --plan p1"))
	if found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals); len(found) != 0 {
		t.Fatalf("a call with no output raised an anomaly: %+v", found)
	}
	// An unrelated line follows the call before its output, so a resume that only remembered the
	// last line would start after the call and lose it.
	dagHostAppendRollout(t, rollout, dagHostResponseItem(t, map[string]any{"type": "message", "role": "user"}))
	// A check runs with the call still pending and the unrelated line after it: the offset must not
	// advance past the call.
	if found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals); len(found) != 0 {
		t.Fatalf("a pending call followed by another line raised an anomaly: %+v", found)
	}
	// The output arrives later; the pending call is still paired with it.
	dagHostAppendRollout(t, rollout, dagHostToolOutput(t, "call-1", `{"error":"refused","reason":"stale_coordinator_epoch"}`))

	if found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals); len(found) != 1 {
		t.Fatalf("a pending call was not paired with its later output: %+v", found)
	}
}

// A rollout shorter than its saved offset was replaced: it is read from its start rather than
// skipped.
func TestDagHostShrunkenRolloutIsReadFromTheStart(t *testing.T) {
	f := dagReviewNewFixture(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl",
		dagHostResponseItem(t, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": strings.Repeat("x", 4000)}}}))
	f.close()
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"status": map[string]any{"type": "idle"}, "path": rollout,
	}}})
	cfg := dagHostConfig(t, f, host.SocketPath, map[string]string{"parent-1": "P-ONE"}, stateDir, 0)

	// The first check records an offset at the large rollout's end.
	if found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals); len(found) != 0 {
		t.Fatalf("the first check raised an anomaly: %+v", found)
	}
	// The rollout is replaced by a much shorter one that already holds a refusal.
	dagHostWriteRollout(t, f.dir, "parent.jsonl", dagHostRelayRefusal(t, "call-1")...)

	if found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindParentDagRefusals); len(found) != 1 {
		t.Fatalf("a replaced rollout was skipped instead of read: %+v", found)
	}
}

// A rollout line over the limit is an unmeasured reading rather than an unbounded allocation.
func TestDagHostOversizeLineIsUnmeasured(t *testing.T) {
	f := dagReviewNewFixture(t)
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl",
		dagHostToolOutput(t, "call-1", strings.Repeat("z", dagHostLineLimit+1024)))
	f.close()
	stateDir := t.TempDir()
	dagHostSeedOffsets(t, stateDir, rollout)
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"status": map[string]any{"type": "idle"}, "path": rollout,
	}}})
	cfg := dagHostConfig(t, f, host.SocketPath, map[string]string{"parent-1": "P-ONE"}, stateDir, 0)

	review := dagHostRun(t, context.Background(), f, cfg)
	if found := dagReviewFind(review, dagHostKindParentDagRefusals); len(found) != 0 {
		t.Fatalf("an oversize line raised an anomaly: %+v", found)
	}
	if !dagHostUnmeasured(review, "parent_refusals:parent-1") {
		t.Fatalf("an oversize line was not unmeasured: %+v", review.Checks)
	}
}

// A review narrowed to one plan reads only that plan's children.
func TestDagHostPlanScopedChildren(t *testing.T) {
	f := dagReviewNewFixture(t)
	dagReviewOnePlan(t, f, "A")
	dagHostRelationship(f, "relationship-in", "CRW-IN", "child-in", 1)
	dagHostRelationship(f, "relationship-out", "CRW-OUT", "child-out", 1)
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind) VALUES ('plan-1','A','relationship-in',1,'manifest','initial')")
	f.close()
	host := fakehost.Start(t)
	dagHostTurnPage(host, dagReviewNow().Add(-30*time.Minute))
	cfg := dagHostConfig(t, f, host.SocketPath, nil, "", 0)
	cfg.raw["dag_review"] = json.RawMessage(`{"plans":["plan-1"]}`)

	found := dagReviewFind(dagHostRun(t, context.Background(), f, cfg), dagHostKindChildTurnWithoutReceipt)
	if len(found) != 1 || found[0].Issue != "CRW-IN" {
		t.Fatalf("a plan-scoped review reported another plan's child: %+v", found)
	}
}

// A context cancelled while the review reads the host writes no offset: the offset file is a
// durable effect, and a cancelled review produces none.
func TestDagHostCancelledContextWritesNoOffset(t *testing.T) {
	f := dagReviewNewFixture(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl", dagHostRefusalRollout(t)...)
	f.close()
	ctx, cancel := context.WithCancel(context.Background())
	host := fakehost.Start(t)
	// The fake cancels the context while it answers the parent read, so the review is cancelled
	// by the time it would commit the offsets.
	host.Handle("thread/read", func(json.RawMessage) fakehost.Reply {
		cancel()
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{
			"status": map[string]any{"type": "idle"}, "path": rollout,
		}}}
	})
	cfg := dagHostConfig(t, f, host.SocketPath, map[string]string{"parent-1": "P-ONE"}, stateDir, 0)

	// The cancelled review may report the cancellation as an error; either way it must not have
	// committed the offset file.
	_, _ = DagReview(ctx, dagReviewEnv(t), cfg)
	if _, err := os.Stat(filepath.Join(stateDir, dagHostStateFile)); !os.IsNotExist(err) {
		t.Fatalf("a cancelled review wrote the offset file: %v", err)
	}
}

// The offset file keeps every top-level key it already holds, so a key a later build adds is not
// lost when this review rewrites the offsets.
func TestDagHostOffsetFileKeepsUnknownKeys(t *testing.T) {
	f := dagReviewNewFixture(t)
	stateDir := t.TempDir()
	path := filepath.Join(stateDir, dagHostStateFile)
	if err := os.WriteFile(path, []byte(`{"offsets":{},"note":"keep me"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl", dagHostRefusalRollout(t)...)
	f.close()
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"status": map[string]any{"type": "idle"}, "path": rollout,
	}}})
	cfg := dagHostConfig(t, f, host.SocketPath, map[string]string{"parent-1": "P-ONE"}, stateDir, 0)

	dagHostRun(t, context.Background(), f, cfg)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("the offset file is not a JSON object: %v\n%s", err, data)
	}
	if string(document["note"]) != `"keep me"` {
		t.Fatalf("the unknown key was dropped: %s", data)
	}
	var offsets map[string]int64
	if err := json.Unmarshal(document["offsets"], &offsets); err != nil {
		t.Fatal(err)
	}
	if len(offsets) != 1 {
		t.Fatalf("the offsets were not written beside the unknown key: %s", data)
	}
}

// A rollout line that is not JSON is skipped rather than failing the reading: the scan reports the
// duplicate it can see and names no unmeasured check.
func TestDagHostMalformedRolloutLineIsSkipped(t *testing.T) {
	f := dagReviewNewFixture(t)
	rollout := dagHostWriteRollout(t, f.dir, "parent.jsonl",
		"{not json",
		dagHostToolCall(t, "call-1", "crw relay status"),
		dagHostToolOutput(t, "call-1", "first"),
		dagHostToolOutput(t, "call-1", "second"),
	)
	f.close()
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"status": map[string]any{"type": "idle"}, "path": rollout,
	}}})
	cfg := dagHostConfig(t, f, host.SocketPath, map[string]string{"parent-1": "P-ONE"}, "", 0)

	review := dagHostRun(t, context.Background(), f, cfg)
	if found := dagReviewFind(review, dagHostKindDuplicateToolOutputs); len(found) != 1 {
		t.Fatalf("the malformed line hid the duplicate: %+v", found)
	}
	if dagHostUnmeasured(review, "parent_rollout:parent-1") {
		t.Fatalf("a malformed line was reported as unmeasured: %+v", review.Checks)
	}
}
