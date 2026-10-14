package supervisor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// CRW-1390 (the D2 follow-ups of CRW-1380): the rows the settle writes into a flow run's flow_events.
//
//   - A flow message whose send has an unknown outcome (held_uncertain) leaves a visible record: one delivery row with the result
//     unknown, instead of no row and a report that reads queued for ever; a verified readback that settles it adds the accepted row.
//   - A row the daemon writes names the message it is about in its subject (the flow_events column and the payload's _label, as a
//     command's row does) and the node of the run's row the message is about (an escalation's node).
//   - The fail-open branch: a row the settle cannot write (the store holds no run of that name, an insert the store refuses) is journaled
//     as flow_delivery_unrecorded and the settle commits (Argon V5 P2 of 8f34843f).

const flowRecordRun = "run-rec"

// flowRecordFixture is fixture24 with a flow run, its report row rep-1 and its escalation row esc_1 of node N7.
func flowRecordFixture(t *testing.T) *stageFixture {
	t.Helper()
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	for _, stmt := range []string{
		`INSERT INTO flow_runs (run_id, workflow, script_digest, helper_digests_json, args_digest, plan_id, plan_revision, actor_task_id, epoch_nonce, started_at)
			VALUES ('` + flowRecordRun + `', 'project-lane', 'd', '[]', 'a', 'plan-1', 1, 'parent', 'n', '2026-10-15T00:00:00Z')`,
		`INSERT INTO flow_events (run_id, seq, kind, call_id, node_id, subject, payload_json, recorded_at)
			VALUES ('` + flowRecordRun + `', 5, 'escalation', '', 'N7', '', '{"_kind":"flow","escalation_id":"esc_1","node":"N7","reason":"blocked_scope"}', '2026-10-15T00:00:01Z')`,
		`INSERT INTO flow_events (run_id, seq, kind, call_id, node_id, subject, payload_json, recorded_at)
			VALUES ('` + flowRecordRun + `', 1099511627776, 'report', '', '', '', '{"_kind":"flow","report_id":"rep-1","report_kind":"milestone_report"}', '2026-10-15T00:00:02Z')`,
	} {
		if _, err := f.s.DB.ExecContext(f.ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// stageFlow stages one flow message of the run through project PRJ-1 (its recipient is the initiative supervisor above it).
func (f *stageFixture) stageFlow(t *testing.T, kind, run, what string) string {
	t.Helper()
	answer, err := f.c.StageDirect(f.ctx, DirectMessage{Kind: kind, Relationship: "project:PRJ-1", Subject: FlowSubject(run, what),
		Summary: kind + " " + what, Text: "the text of " + what, At: f.at}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := answer["messageId"].(string)
	if id == "" {
		t.Fatalf("staging answered %v", answer)
	}
	return id
}

// flowRecordRow is one delivery row the settle wrote, with its columns.
type flowRecordRow struct {
	kind, node, subject string
	payload             map[string]any
}

func (f *stageFixture) flowRows(t *testing.T, run string) []flowRecordRow {
	t.Helper()
	rows, err := f.s.DB.QueryContext(f.ctx, "SELECT kind, node_id, subject, payload_json FROM flow_events WHERE run_id = ? AND kind IN (?, ?) ORDER BY seq",
		run, FlowReportDeliveryKind, FlowMessageDeliveryKind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []flowRecordRow
	for rows.Next() {
		var r flowRecordRow
		var raw string
		if err := rows.Scan(&r.kind, &r.node, &r.subject, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &r.payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFlowDeliveryOfAnUncertainSendIsRecordedUnknownAndItsReadbackAccepted(t *testing.T) {
	f := flowRecordFixture(t)
	id := f.stageFlow(t, KindFlowMilestoneReport, flowRecordRun, "rep-1")
	h := &sendHost{status: "idle", outcome: "unknown"}
	result, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	if err != nil || result["deliveryState"] != "held_uncertain" {
		t.Fatalf("the send answered %v (%v), want held_uncertain", result, err)
	}
	rows := f.flowRows(t, flowRecordRun)
	if len(rows) != 1 || rows[0].kind != FlowReportDeliveryKind || rows[0].payload["result"] != "unknown" || rows[0].payload["report_id"] != "rep-1" ||
		rows[0].payload["observation"] != "held_uncertain" || rows[0].payload["request_id"] != result["requestId"] {
		t.Fatalf("an uncertain send left the delivery rows %+v, want one unknown report_delivery of rep-1", rows)
	}
	if rows[0].subject != FlowSubject(flowRecordRun, "rep-1") || rows[0].payload["_label"] != rows[0].subject {
		t.Fatalf("the unknown row names the subject %q (label %v), want the message's %q", rows[0].subject, rows[0].payload["_label"], FlowSubject(flowRecordRun, "rep-1"))
	}
	// a second attempt sends nothing (held_uncertain is not claimable) and writes nothing
	_, _ = f.c.Attempt(f.ctx, id, h, 1_700_000_100)
	if len(h.sends) != 1 {
		t.Fatalf("an uncertain message was sent again: %d sends", len(h.sends))
	}
	if got := f.flowRows(t, flowRecordRun); len(got) != 1 {
		t.Fatalf("a second attempt wrote the rows %+v", got)
	}
	// the recipient's readback finds the attempt's token in its own turn: the send arrived, and the delivery says accepted
	h.items = map[string]string{"turn-supervisor-1": h.sends[0]}
	read, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", h, 1_700_000_003)
	if err != nil || read["verified"] != "host_read" {
		t.Fatalf("the readback answered %v (%v)", read, err)
	}
	rows = f.flowRows(t, flowRecordRun)
	if len(rows) != 2 || rows[1].payload["result"] != "accepted" || rows[1].payload["report_id"] != "rep-1" || rows[1].payload["observation"] != "read" ||
		rows[1].payload["reason"] != "readback" || rows[1].subject != FlowSubject(flowRecordRun, "rep-1") {
		t.Fatalf("after the readback the delivery rows are %+v, want unknown then accepted", rows)
	}
	// the same readback again writes no second accepted row
	if _, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", h, 1_700_000_004); err != nil {
		t.Fatal(err)
	}
	if got := f.flowRows(t, flowRecordRun); len(got) != 2 {
		t.Fatalf("a second readback wrote the rows %+v", got)
	}
}

func TestFlowDeliveryRowNamesTheMessageAndTheNodeOfItsEscalation(t *testing.T) {
	f := flowRecordFixture(t)
	id := f.stageFlow(t, KindFlowEscalation, flowRecordRun, "esc-esc_1")
	result, err := f.c.Attempt(f.ctx, id, &sendHost{status: "idle"}, 1_700_000_000)
	if err != nil || result["deliveryState"] != "dispatched" {
		t.Fatalf("the send answered %v (%v)", result, err)
	}
	rows := f.flowRows(t, flowRecordRun)
	want := FlowSubject(flowRecordRun, "esc-esc_1")
	if len(rows) != 1 || rows[0].kind != FlowMessageDeliveryKind || rows[0].payload["result"] != "accepted" || rows[0].payload["message_id"] != id {
		t.Fatalf("the delivery rows are %+v, want one accepted message_delivery", rows)
	}
	if rows[0].subject != want || rows[0].payload["_label"] != want || rows[0].node != "N7" {
		t.Fatalf("the row names subject %q, label %v and node %q; want %q, %q and the escalation's node N7", rows[0].subject, rows[0].payload["_label"], rows[0].node, want, want)
	}
	// a report's row is named by its subject too; the report row has no node, and neither has its delivery
	rep := f.stageFlow(t, KindFlowMilestoneReport, flowRecordRun, "rep-1")
	if _, err := f.c.Attempt(f.ctx, rep, &sendHost{status: "idle"}, 1_700_000_200); err != nil {
		t.Fatal(err)
	}
	rows = f.flowRows(t, flowRecordRun)
	if len(rows) != 2 || rows[1].kind != FlowReportDeliveryKind || rows[1].subject != FlowSubject(flowRecordRun, "rep-1") || rows[1].node != "" {
		t.Fatalf("the report's delivery row is %+v", rows)
	}
}

func TestFlowDeliveryItCannotWriteIsJournaledAndTheSettleCommits(t *testing.T) {
	t.Run("no run", func(t *testing.T) {
		f := flowRecordFixture(t)
		id := f.stageFlow(t, KindFlowMilestoneReport, "run-gone", "rep-9")
		result, err := f.c.Attempt(f.ctx, id, &sendHost{status: "idle"}, 1_700_000_000)
		if err != nil || result["deliveryState"] != "dispatched" || result["messageState"] != "dispatched" {
			t.Fatalf("the send answered %v (%v), want the settle committed as dispatched", result, err)
		}
		if got := f.flowRows(t, "run-gone"); len(got) != 0 {
			t.Fatalf("rows were written for a run the store does not hold: %+v", got)
		}
		journal, err := f.s.Journal(f.ctx, "flow_delivery_unrecorded", id)
		if err != nil || len(journal) != 1 || !strings.Contains(journal[0].Detail, "holds no run run-gone") || !strings.Contains(journal[0].Detail, `"result": "accepted"`) {
			t.Fatalf("the journal holds %+v (%v), want one flow_delivery_unrecorded naming the missing run", journal, err)
		}
	})
	t.Run("an insert the store refuses", func(t *testing.T) {
		f := flowRecordFixture(t)
		if _, err := f.s.DB.ExecContext(f.ctx, `CREATE TRIGGER test_refuse_delivery BEFORE INSERT ON flow_events WHEN NEW.kind = 'message_delivery'
			BEGIN SELECT RAISE(ABORT, 'the test refuses delivery rows'); END`); err != nil {
			t.Fatal(err)
		}
		id := f.stageFlow(t, KindFlowEscalation, flowRecordRun, "esc-esc_1")
		result, err := f.c.Attempt(f.ctx, id, &sendHost{status: "idle"}, 1_700_000_000)
		if err != nil || result["messageState"] != "dispatched" {
			t.Fatalf("the send answered %v (%v), want the settle committed as dispatched", result, err)
		}
		if got := f.flowRows(t, flowRecordRun); len(got) != 0 {
			t.Fatalf("a refused insert left the rows %+v", got)
		}
		journal, err := f.s.Journal(f.ctx, "flow_delivery_unrecorded", id)
		if err != nil || len(journal) != 1 || !strings.Contains(journal[0].Detail, "the test refuses delivery rows") {
			t.Fatalf("the journal holds %+v (%v), want one flow_delivery_unrecorded with the store's refusal", journal, err)
		}
		row, err := f.c.Get(context.Background(), id)
		if err != nil || row.State != "dispatched" {
			t.Fatalf("the message is %+v (%v), want dispatched", row, err)
		}
	})
}
