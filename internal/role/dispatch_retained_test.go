package role

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// dispatchCorrectionEdit rewrites the stored record of the dispatch as a foreign tool would: edit changes the attempt object.
func dispatchCorrectionEdit(t *testing.T, file string, attempt int, edit func(a map[string]any)) {
	t.Helper()
	var d map[string]any
	check(t, json.Unmarshal(must(os.ReadFile(file)), &d))
	edit(d["attempts"].([]any)[attempt].(map[string]any))
	check(t, os.WriteFile(file, must(json.MarshalIndent(d, "", "  ")), 0o600))
}

// dispatchCorrectionMember reads one member of the stored attempt as generic JSON.
func dispatchCorrectionMember(t *testing.T, file string, attempt int, key string) any {
	t.Helper()
	var d map[string]any
	check(t, json.Unmarshal(must(os.ReadFile(file)), &d))
	return d["attempts"].([]any)[attempt].(map[string]any)[key]
}

// Members of a stored receipt, termination or cleanup that the checked boundary does not own are kept as they were read,
// whatever it writes to the record next: a replayed created report keeps a confirmed receipt whole, and the reports that follow
// rewrite the record around it.
func TestDispatchReceiptKeepsMembersItDoesNotOwn(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	attempt, marker := dispatchReceiptClaim(t, ws, env, Reviewer, "review-test")
	dispatchReceiptIssue(t, ws, marker, "call-1")
	file := dispatchReceiptFile(ws, "review-test")
	dispatchCorrectionEdit(t, file, 0, func(a map[string]any) { a["candidate"].(map[string]any)["externalEvidence"] = "candidate-extension" })
	seen := dispatchReceiptNative(t, env, dispatchReceiptRow{id: "child-a", parent: "session-test", first: marker + "\nREVIEW", model: "xai/grok-4.6", effort: "high"})
	dispatchReceiptParent(t, seen, dispatchReceiptSpawn{"call-1", "completed", []string{"child-a"}})
	_, err := CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, "child-a"), seen, nil)
	check(t, err)
	receipt := dispatchCorrectionMember(t, file, 0, "receipt").(map[string]any)
	if receipt["candidate"].(map[string]any)["externalEvidence"] != "candidate-extension" {
		t.Fatalf("the accepted receipt lost the candidate's extension: %v", receipt)
	}
	dispatchCorrectionEdit(t, file, 0, func(a map[string]any) {
		r := a["receipt"].(map[string]any)
		r["externalEvidence"] = "receipt-extension"
		for _, key := range []string{"issuance", "child", "host"} {
			r[key].(map[string]any)["extra"] = key + "-extension"
		}
	})
	want := dispatchCorrectionMember(t, file, 0, "receipt")
	blind := dispatchReceiptNative(t, env, dispatchReceiptRow{id: "child-a", parent: "session-test"})
	_, err = CheckedDispatch(context.Background(), ws, dispatchReceiptCreated("review-test", attempt, "child-a"), blind, nil)
	check(t, err)
	if got := dispatchCorrectionMember(t, file, 0, "receipt"); !reflect.DeepEqual(got, want) {
		t.Fatalf("a replayed created report changed the receipt:\n got %v\nwant %v", got, want)
	}
	complete := dispatchReceiptCreated("review-test", attempt, "child-a")
	complete["outcome"] = "complete"
	if out, err := CheckedDispatch(context.Background(), ws, complete, blind, nil); err != nil || out.Action != "complete" {
		t.Fatalf("complete = %q %v", out.Action, err)
	}
	if got := dispatchCorrectionMember(t, file, 0, "receipt"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the complete report changed the receipt:\n got %v\nwant %v", got, want)
	}
}

// The same for the termination a handoff recorded and the cleanup of a policy stop: later writes keep their other members, and
// the typed record the library returns carries the termination the stored one has.
func TestDispatchTerminationAndCleanupKeepMembersTheyDoNotOwn(t *testing.T) {
	ws, env, attempt, file := dispatchHandoffStart(t, true)
	_, err := CheckedDispatch(context.Background(), ws, dispatchHandoffReports(attempt)["task_failed"], env, &dispatchHandoffHost{newest: "completed"})
	check(t, err)
	status := dispatchTestCall(t, ws, env, map[string]any{"action": "status"})
	if tm := status.Attempts[0].Termination; tm == nil || tm.Newest != "completed" || tm.Source != "app-server" {
		t.Fatalf("status returned termination %+v", status.Attempts[0].Termination)
	}
	dispatchCorrectionEdit(t, file, 0, func(a map[string]any) {
		a["termination"].(map[string]any)["externalEvidence"] = "termination-extension"
	})
	want := dispatchCorrectionMember(t, file, 0, "termination")
	dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": status.AttemptID})
	if got := dispatchCorrectionMember(t, file, 0, "termination"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the next claim changed the termination:\n got %v\nwant %v", got, want)
	}

	ws, env, attempt, file = dispatchHandoffStart(t, true)
	stop := map[string]any{"action": "report", "sessionId": "session-test", "dispatchId": "task-test", "attemptId": attempt, "outcome": "failed", "error": "permission_denied", "executionState": "running"}
	_, err = CheckedDispatch(context.Background(), ws, stop, env, &dispatchHandoffHost{})
	check(t, err)
	dispatchCorrectionEdit(t, file, 0, func(a map[string]any) { a["cleanup"].(map[string]any)["externalEvidence"] = "cleanup-extension" })
	for _, h := range []*dispatchHandoffHost{{listErr: context.DeadlineExceeded}, {newest: "completed"}} {
		_, err = CheckedDispatch(context.Background(), ws, dispatchCleanupStop(attempt), env, h)
		check(t, err)
		if got := dispatchCorrectionMember(t, file, 0, "cleanup").(map[string]any); got["externalEvidence"] != "cleanup-extension" {
			t.Fatalf("the cleanup report lost the cleanup's extension: %v", got)
		}
	}
}
