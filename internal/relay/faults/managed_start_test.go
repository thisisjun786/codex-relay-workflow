package faults

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func insertStart(t *testing.T, l *Ledger, ctx context.Context, id, issue, status string) {
	t.Helper()
	var receipt any
	if status != "" {
		receipt = status
	}
	_, err := l.Store.Q(ctx).ExecContext(ctx, "INSERT INTO managed_start_requests (request_id,issue_key,request_fingerprint,fingerprint_version,workspace,marker_root,socket_identity,create_request_id,dispatch_request_id,state,revision,receipt_status,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)", id, issue, "fp", "1", "/work", "/markers", "sock", "create-"+id, "dispatch-"+id, "create_armed", 1, receipt, "now", "now")
	if err != nil {
		t.Fatal(err)
	}
}
func insertAnswer(t *testing.T, l *Ledger, ctx context.Context, id, reason string) {
	t.Helper()
	detail := fmt.Sprintf(`{"stage":"creation","state":"incomplete","reason":%q}`, reason)
	_, err := l.Store.Q(ctx).ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES (?,?,?,?)", "now", "managed_start_observed", id, detail)
	if err != nil {
		t.Fatal(err)
	}
}
func Test22_FLF_1_CreationAnswersBecomeDistinctFaults(t *testing.T) {
	l, c := testLedger(t)
	sw := &Sweeper{Store: l.Store, HostRecordPath: testHostRecordPath(), Installation: Installation{Package: "codex-session-relay", Version: "1", Location: t.TempDir()}}
	insertStart(t, l, c, "managed-1", "REL-MANAGED", "")
	insertAnswer(t, l, c, "managed-1", "creation_failed")
	found, _, err := sw.ManagedStartFaults(c, "crw", nil)
	if err != nil || len(found) != 1 {
		t.Fatalf("collected %d: %v", len(found), err)
	}
	o := found[0]
	if o.Severity != Broken || o.Signature["receiptStatus"] != "failed" || o.OccurrenceKey != "managed:managed-1:failed" {
		t.Fatalf("wrong observation: %+v", o)
	}
	if !strings.Contains(dumps(o.Evidence, false), "journal:") {
		t.Fatal("journal ref missing")
	}
	record(t, l, c, o)
	if state(t, l, c, FaultID("crw", o.FaultClass, o.Signature)) != Open {
		t.Fatal("broken fault not open")
	}
	insertAnswer(t, l, c, "managed-1", "creation_unknown")
	again, _, err := sw.ManagedStartFaults(c, "crw", nil)
	if err != nil || len(again) != 1 || again[0].Signature["receiptStatus"] != "unknown" || FaultID("crw", o.FaultClass, o.Signature) == FaultID("crw", again[0].FaultClass, again[0].Signature) {
		t.Fatalf("unknown creation merged with failed: %v %+v", err, again)
	}
}
func Test22_FLF_2_UnansweredAndReadinessRefusalAreNotFaults(t *testing.T) {
	l, c := testLedger(t)
	sw := &Sweeper{Store: l.Store}
	insertStart(t, l, c, "managed-1", "REL-MANAGED", "")
	found, _, err := sw.ManagedStartFaults(c, "crw", nil)
	if err != nil || len(found) != 0 {
		t.Fatalf("unanswered start: %+v %v", found, err)
	}
	insertAnswer(t, l, c, "managed-1", "worker_policy_unconfigured")
	found, _, err = sw.ManagedStartFaults(c, "crw", nil)
	if err != nil || len(found) != 0 {
		t.Fatalf("preflight refusal: %+v %v", found, err)
	}
}
func Test22_FLF_3_LaterCreationAnswerClearsButPreflightDoesNot(t *testing.T) {
	l, c := testLedger(t)
	sw := &Sweeper{Store: l.Store, HostRecordPath: testHostRecordPath(), Installation: Installation{Package: "codex-session-relay", Version: "1", Location: t.TempDir()}, Now: func() string { return "now" }}
	insertStart(t, l, c, "managed-1", "REL-MANAGED", "")
	insertAnswer(t, l, c, "managed-1", "creation_failed")
	first, err := sw.Sweep(c, "crw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sw.RecordAll(c, l, first); err != nil {
		t.Fatal(err)
	}
	id := FaultID("crw", "managed_start_failed", map[string]any{"issueKey": "REL-MANAGED", "receiptStatus": "failed"})
	if state(t, l, c, id) != Open {
		t.Fatal("failure not recorded")
	}
	insertAnswer(t, l, c, "managed-1", "creation_unknown")
	batch, err := sw.Sweep(c, "crw")
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Clears) != 1 || len(batch.Observations) != 1 {
		t.Fatalf("new answer did not replace old: %+v", batch)
	}
	if _, err = sw.RecordAll(c, l, batch); err != nil {
		t.Fatal(err)
	}
	r, err := l.Store.One(c, "SELECT cleared_at FROM fault_ledger WHERE fault_id = ?", id)
	if err != nil || r.Get("cleared_at") == nil {
		t.Fatalf("failure not cleared: %v %+v", err, r)
	}
	insertAnswer(t, l, c, "managed-1", "worker_policy_unconfigured")
	batch, err = sw.Sweep(c, "crw")
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Observations) != 0 {
		t.Fatal("preflight answer counted as failure")
	}
}
func Test22_FLF_6_CreationAnswerUsesIndex(t *testing.T) {
	l, c := testLedger(t)
	rows, err := l.Store.All(c, "EXPLAIN QUERY PLAN "+managedCreationQuery, "managed-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(rows), "journal_managed_creation") {
		t.Fatalf("index not used: %+v", rows)
	}
}
