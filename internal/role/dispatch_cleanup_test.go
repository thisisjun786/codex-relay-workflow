package role

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// dispatchCleanupStop is the stopped report that submits the cleanup evidence of the recorded child.
func dispatchCleanupStop(attempt string) map[string]any {
	return map[string]any{"action": "report", "sessionId": "session-test", "dispatchId": "task-test", "attemptId": attempt, "outcome": "stopped", "agentId": "child-a", "executionState": "stopped", "reconciliation": "child shut down after the denial; partial edits inspected and reverted"}
}

// A policy stop (permission_denied, a cancellation) of an attempt whose child is recorded keeps the dispatch stopped with the
// original code and marks the child's cleanup as outstanding. The stopped report then records the cleanup as evidence only:
// an active child or a turn in progress refuses and writes nothing; a child whose end cannot be seen gets its evidence
// recorded but keeps its id held; a child seen to have ended is cleaned up and its id is free. The dispatch stays stopped
// throughout and no candidate, attempt, spawn or main-direct is ever returned, though a fallback is configured.
func TestDispatchCleanupAfterAPolicyStop(t *testing.T) {
	for _, tc := range []struct{ outcome, code, state string }{
		{"failed", "permission_denied", "running"},
		{"failed", "request_cancelled", "unknown"},
		{"task_failed", "permission_denied", "unknown"},
	} {
		t.Run(tc.outcome+"/"+tc.code+"/"+tc.state, func(t *testing.T) {
			ws, env, attempt, file := dispatchHandoffStart(t, true)
			stop := map[string]any{"action": "report", "sessionId": "session-test", "dispatchId": "task-test", "attemptId": attempt, "outcome": tc.outcome, "error": tc.code, "executionState": tc.state}
			out, err := CheckedDispatch(context.Background(), ws, stop, env, &dispatchHandoffHost{})
			check(t, err)
			stored := must(dispatchRead(file, "session-test", "task-test"))
			a := stored.Attempts[0]
			if out.Action != "stop" || !dispatchIs(stored.Status, "stopped") || !dispatchIs(a.Status, "running") || a.Cleanup == nil || a.Cleanup.Status != "pending" || *a.Code != tc.code {
				t.Fatalf("policy stop = %q, stored %v %v %+v", out.Action, stored.Status, a.Status, a.Cleanup)
			}
			before := must(os.ReadFile(file))
			for _, h := range []*dispatchHandoffHost{{status: "active"}, {newest: "inProgress"}} {
				if _, err := CheckedDispatch(context.Background(), ws, dispatchCleanupStop(attempt), env, h); err == nil || !strings.Contains(err.Error(), "stop it before") {
					t.Fatalf("cleanup of a live child = %v", err)
				}
			}
			if string(before) != string(must(os.ReadFile(file))) {
				t.Fatal("a refused cleanup wrote state")
			}
			out, err = CheckedDispatch(context.Background(), ws, dispatchCleanupStop(attempt), env, &dispatchHandoffHost{listErr: errors.New("turn list unavailable")})
			check(t, err)
			stored = must(dispatchRead(file, "session-test", "task-test"))
			a = stored.Attempts[0]
			if out.Action != "stop" || !strings.Contains(out.Reason, "its id stays held") || !dispatchIs(stored.Status, "stopped") || !dispatchIs(a.Status, "running") || a.Cleanup == nil || a.Cleanup.Status != "unconfirmed" || a.Cleanup.Evidence == "" {
				t.Fatalf("unconfirmed cleanup = %q %q, stored %v %v %+v", out.Action, out.Reason, stored.Status, a.Status, a.Cleanup)
			}
			if createdCheckClosed(&stored, 0) {
				t.Fatal("an unconfirmed cleanup freed the child's id")
			}
			h := &dispatchHandoffHost{newest: "completed"}
			out, err = CheckedDispatch(context.Background(), ws, dispatchCleanupStop(attempt), env, h)
			check(t, err)
			stored = must(dispatchRead(file, "session-test", "task-test"))
			a = stored.Attempts[0]
			if out.Action != "stop" || len(out.Attempts) != 1 || out.Candidate != nil || out.Marker != "" || !dispatchIs(stored.Status, "stopped") || !dispatchIs(a.Status, "failed") ||
				a.Cleanup == nil || a.Cleanup.Status != "confirmed" || a.Cleanup.Newest != "completed" || a.Reconciliation == nil || a.Code == nil || *a.Code != tc.code {
				t.Fatalf("confirmed cleanup = %q, stored %v %v %+v", out.Action, stored.Status, a.Status, a.Cleanup)
			}
			if !createdCheckClosed(&stored, 0) {
				t.Fatal("the confirmed cleanup kept the child's id held")
			}
			after := must(os.ReadFile(file))
			again := &dispatchHandoffHost{}
			for _, input := range []map[string]any{dispatchCleanupStop(attempt), {"action": "claim", "sessionId": "session-test", "dispatchId": "task-test", "attemptId": attempt}, {"action": "status", "sessionId": "session-test", "dispatchId": "task-test"}} {
				out, err := CheckedDispatch(context.Background(), ws, input, env, again)
				if check(t, err); out.Action != "stop" || len(out.Attempts) != 1 {
					t.Fatalf("%v after the cleanup = %q", input["action"], out.Action)
				}
			}
			if len(again.methods) != 0 || string(after) != string(must(os.ReadFile(file))) {
				t.Fatalf("a closed cleanup asked %v or changed the record", again.methods)
			}
		})
	}
}

// A policy stop of an attempt with no recorded child has nothing to clean up, and the stopped report keeps its refusal.
func TestDispatchCleanupWithoutAChild(t *testing.T) {
	ws, env, start, file := dispatchTestFixture(t)
	dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	out, err := CheckedDispatch(context.Background(), ws, map[string]any{"action": "report", "sessionId": "session-test", "dispatchId": "task-test", "attemptId": start.AttemptID, "outcome": "failed", "error": "permission_denied", "executionState": "not_created"}, env, nil)
	check(t, err)
	if stored := must(dispatchRead(file, "session-test", "task-test")); out.Action != "stop" || stored.Attempts[0].Cleanup != nil {
		t.Fatalf("policy stop without a child = %q %+v", out.Action, stored.Attempts[0].Cleanup)
	}
	before := must(os.ReadFile(file))
	out, err = CheckedDispatch(context.Background(), ws, dispatchCleanupStop(start.AttemptID), env, nil)
	if check(t, err); out.Action != "stop" || string(before) != string(must(os.ReadFile(file))) {
		t.Fatalf("stopped report without a child = %q", out.Action)
	}
}

// While a policy-stopped dispatch's child is unaccounted for, another dispatch of the session cannot report it, and the
// refusal names the cleanup report as the way out; the confirmed cleanup frees it.
func TestDispatchCleanupHoldsTheIdUntilConfirmed(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	h := &createdLockHost{status: "idle"}
	one, two := createdArchivedSession(t, ws, env, "task-one"), createdArchivedSession(t, ws, env, "task-two")
	_, err := CheckedDispatch(context.Background(), ws, createdArchivedReport("task-one", one, "child-a"), env, h)
	check(t, err)
	_, err = CheckedDispatch(context.Background(), ws, map[string]any{"action": "report", "sessionId": "session-test", "dispatchId": "task-one", "attemptId": one, "outcome": "failed", "error": "permission_denied", "executionState": "running"}, env, h)
	check(t, err)
	if _, err = CheckedDispatch(context.Background(), ws, createdArchivedReport("task-two", two, "child-a"), env, h); err == nil || !strings.Contains(err.Error(), "records its cleanup and frees the agentId") {
		t.Fatalf("report of an unaccounted child = %v", err)
	}
	cleanup := dispatchCleanupStop(one)
	cleanup["dispatchId"] = "task-one"
	_, err = CheckedDispatch(context.Background(), ws, cleanup, env, &dispatchHandoffHost{newest: "interrupted"})
	check(t, err)
	_, err = CheckedDispatch(context.Background(), ws, createdArchivedReport("task-two", two, "child-a"), env, h)
	check(t, err)
}
