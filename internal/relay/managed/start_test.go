package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func Test27_MST_7_RefusalExcludesBusinessTurnAndReportingArgv(t *testing.T) {
	r := startResult{RequestID: "request", AssignmentID: "assignment", ChildTaskID: "child", StatePath: "/state", MarkerRoot: "/marker", Workspace: "/workspace"}
	var output bytes.Buffer
	if err := contract.Emit(&output, r.result("refused", "business", "recipient_paused")); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(output.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["reason"] != "recipient_paused" {
		t.Fatalf("wrong refusal reason: %v", payload["reason"])
	}
	if _, ok := payload["businessTurnId"]; ok {
		t.Fatal("refused receipt names a business turn")
	}
	if _, ok := payload["reportingArgv"]; ok {
		t.Fatal("refused receipt suggests reporting")
	}
}

func Test27_MST_10_ObservationReadback(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := startResult{RequestID: "request", AssignmentID: "assignment", StatePath: filepath.Dir(s.Path), MarkerRoot: "/markers", Workspace: "/workspace"}
	receipt, err := r.Observe(ctx, s, "2026-09-26T00:00:00+00:00", "refused", "preflight", "worker_policy_unconfigured")
	if err != nil {
		t.Fatal(err)
	}
	observation, err := LastObservation(ctx, s, "request")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := contract.Emit(&output, receipt); err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal(output.Bytes(), &want); err != nil {
		t.Fatal(err)
	}
	if !jsonSame(want, observation) {
		t.Fatalf("readback differs: %v != %v", observation, want)
	}
}

// Test27_MST_1_AdmittedResultAndReplay checks the Python result field contract.
func Test27_MST_1_AdmittedResultAndReplay(t *testing.T) {
	row := startResult{RequestID: "req", AssignmentID: "assignment", ChildTaskID: "child", BusinessTurnID: "business", StatePath: "/state", MarkerRoot: "/marker", Workspace: "/workspace"}
	got := row.admitted()
	want := `{"schema":"managed-start/1","requestId":"req","state":"admitted","stage":"business_accepted","reason":null,"assignmentId":"assignment","relationshipId":null,"executionGeneration":null,"childTaskId":"child","standbyTurnId":null,"creationRequestId":"","businessRequestId":"","requestFingerprint":"","ledger":null,"reservationState":null,"reservationRevision":null,"recovery":"Retry only this same complete request; do not create a replacement.","selectors":{"state":"/state","markerRoot":"/marker","workspace":"/workspace"},"businessTurnId":"business","childClaim":"not_observed","hookFiring":"not_observed","reportingArgv":["--state","/state","reporting-show","--marker-root","/marker","--workspace","/workspace","--assignment","assignment","--session","child","--turn","business"]}`
	var expected, actual any
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := contract.Emit(&output, got)
	b := output.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &actual); err != nil {
		t.Fatal(err)
	}
	if !jsonSame(expected, actual) {
		t.Fatalf("admitted result differs from Python:\nGo: %s\nPython: %s", b, want)
	}
}
