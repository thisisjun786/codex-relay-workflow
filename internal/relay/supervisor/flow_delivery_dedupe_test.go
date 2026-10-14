package supervisor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1380, Argon V5 F5 of 4d293b7b: the settle's guard against a second delivery row reads the rows of the message's own run, so
// SQLite searches flow_events by its (run_id, seq) primary key instead of scanning every run's rows inside the settle's write
// transaction; the guard still writes a row once for its key and result.

func flowDeliveryStore(t *testing.T, runs ...string) *store.Store {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, run := range runs {
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO flow_runs (run_id, workflow, script_digest, helper_digests_json, args_digest, plan_id, plan_revision, actor_task_id, epoch_nonce, started_at)
			VALUES (?, 'project-lane', 'd', '[]', 'a', 'plan-1', 1, 'orch-task', 'n', '2026-10-14T00:00:00Z')`, run); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestFlowDeliveryGuardSearchesTheRunsOwnRows(t *testing.T) {
	s := flowDeliveryStore(t, "run-a")
	rows, err := s.DB.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+flowDeliveryHeldQuery,
		flowDeliveryHeldArgs("run-a", FlowReportDeliveryKind, "report_id", "rep_1", FlowDeliveryAccepted)...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "; ")
	if !strings.Contains(joined, "SEARCH flow_events") || !strings.Contains(joined, "run_id=?") || strings.Contains(joined, "SCAN flow_events") {
		t.Fatalf("the guard's plan is %q: it does not search flow_events by the run", joined)
	}
}

func TestFlowDeliveryGuardWritesARowOnceForItsKeyAndResult(t *testing.T) {
	ctx := context.Background()
	s := flowDeliveryStore(t, "run-a", "run-b")
	c := &Channel{Store: s}
	row := store.SupervisorMessagesRow{MessageID: "msg-esc-1", ObligationKind: KindFlowEscalation, Subject: FlowSubject("run-a", "esc_1")}
	accepted := flowDelivery{result: FlowDeliveryAccepted, requestID: "req-1", state: "dispatched", at: "2026-10-14T00:00:01Z"}
	if written, why, err := c.appendFlowDelivery(ctx, row, "run-a", "esc_1", accepted); err != nil || !written || why != "" {
		t.Fatalf("the first settle: written %v why %q err %v", written, why, err)
	}
	// a second settle of the message (a daemon that died after its commit, a replay) writes nothing
	if written, why, err := c.appendFlowDelivery(ctx, row, "run-a", "esc_1", accepted); err != nil || written || why != "" {
		t.Fatalf("the second settle: written %v why %q err %v", written, why, err)
	}
	// a refused row of the same message is another key
	refused := flowDelivery{result: FlowDeliveryRefused, state: "queued", hold: "busy_cap", at: "2026-10-14T00:00:02Z"}
	if written, _, err := c.appendFlowDelivery(ctx, row, "run-a", "esc_1", refused); err != nil || !written {
		t.Fatalf("the refused row: written %v err %v", written, err)
	}
	if written, _, err := c.appendFlowDelivery(ctx, row, "run-a", "esc_1", refused); err != nil || written {
		t.Fatalf("the refused row again: written %v err %v", written, err)
	}
	count := func(run string) int {
		var n int
		if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM flow_events WHERE run_id = ? AND kind = ?", run, FlowMessageDeliveryKind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count("run-a") != 2 || count("run-b") != 0 {
		t.Fatalf("message_delivery rows: run-a %d (want 2), run-b %d (want 0)", count("run-a"), count("run-b"))
	}
}
