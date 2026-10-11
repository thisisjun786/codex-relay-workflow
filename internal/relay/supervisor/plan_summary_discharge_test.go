package supervisor

import (
	"context"
	"testing"
)

// CRW-1216 after the evaluation (d1): a verdict on a relationship a plan runs enqueues no sync_outbox row, because the plan's summary outbox carries it. The supervisor's discharge reads the record the
// supervisor reads, so a confirmed summary entry of the relationship's coordination document that was enqueued after the verdict discharges the obligation as a confirmed sync_outbox row does, and
// an entry that was enqueued before the verdict, one that is not confirmed, one of another document and a node the plan no longer holds do not. The tests use temporary stores only.

const (
	verdictAt = "2023-11-14T22:13:21.000000+00:00"
	beforeAt  = "2023-11-14T22:13:20.500000+00:00"
	afterAt   = "2023-11-14T22:13:22.000000+00:00"
)

type planSummary struct {
	state, document, created string
	retired                  bool
}

// planRelationship binds rel-1 to a node of plan-1, rules its event and stores one summary entry.
func planRelationship(t *testing.T, f *stageFixture, sum planSummary) {
	t.Helper()
	retired := "NULL"
	if sum.retired {
		retired = "2"
	}
	for _, stmt := range []string{
		"INSERT INTO sync_targets (relationship_id,target,target_ref,recorded_at) VALUES ('rel-1','coordination_document','doc-1','t')",
		"INSERT INTO dag_plans (plan_id,project_key,created_by_task_id,created_at) VALUES ('plan-1','PRJ-1','parent','t')",
		"INSERT INTO dag_plan_revisions (plan_id,revision_no,parent_revision_no,request_id,request_digest,change_json,state_digest,author_task_id,recorded_at) VALUES ('plan-1',1,0,'r1','d','[]','s','parent','t')",
		"INSERT INTO dag_plan_revisions (plan_id,revision_no,parent_revision_no,request_id,request_digest,change_json,state_digest,author_task_id,recorded_at) VALUES ('plan-1',2,1,'r2','d','[]','s','parent','t')",
		"INSERT INTO dag_nodes (plan_id,node_id,introduced_rev,retired_rev,slice_digest,issue_key,node_kind,criteria_set_digest) VALUES ('plan-1','node-1',1," + retired + ",'slice','REL-1','implementation','criteria')",
		"INSERT INTO dag_node_executions (plan_id,node_id,relationship_id,execution_generation,manifest_digest,kind) VALUES ('plan-1','node-1','rel-1',1,'manifest','initial')",
		"INSERT INTO verdicts (event_id,record,verdict,next_generation,verdict_turn_id,decided_at) VALUES ('event-1','{}','verified',NULL,'verdict-turn','" + verdictAt + "')",
		"INSERT INTO dag_summary_outbox (summary_id,plan_id,project_key,document,plan_revision,seq,subject_digest,state_digest,summary,summary_sha256,state,enqueued_by,created_at,updated_at) VALUES ('sum-1','plan-1','PRJ-1','" +
			sum.document + "',1,1,'" + "0123456789012345678901234567890123456789012345678901234567890123','s','text','sha','pending','parent','" + sum.created + "','" + sum.created + "')",
	} {
		if _, err := f.s.DB.ExecContext(f.ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// the entry reaches its state the way the parent's commands move it: pending, claimed, confirmed
	if sum.state != "pending" {
		for _, stmt := range []string{
			"UPDATE dag_summary_outbox SET state='claimed', claim_token='token', claimed_by='parent', claimed_at='" + sum.created + "', updated_at='" + sum.created + "' WHERE summary_id='sum-1'",
			"UPDATE dag_summary_outbox SET state='confirmed', claim_token=NULL, readback='text', confirmed_at='" + sum.created + "', updated_at='" + sum.created + "' WHERE summary_id='sum-1'",
		} {
			if _, err := f.s.DB.ExecContext(f.ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	}
}

func TestPlanSummaryDischargesTheVerdictObligation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		sum       planSummary
		discharge bool
	}{
		{"confirmed entry after the verdict", planSummary{"confirmed", "doc-1", afterAt, false}, true},
		{"confirmed entry before the verdict", planSummary{"confirmed", "doc-1", beforeAt, false}, false},
		{"entry not confirmed", planSummary{"pending", "doc-1", afterAt, false}, false},
		{"entry of another document", planSummary{"confirmed", "doc-2", afterAt, false}, false},
		{"node the plan retired", planSummary{"confirmed", "doc-1", afterAt, true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := fixture24(t)
			planRelationship(t, f, tc.sum)
			o := f.obligation(t)
			owed, reason, err := f.c.ReportableCurrent(f.ctx, o)
			if err != nil || owed == tc.discharge || (tc.discharge && reason != "already_in_the_record_the_supervisor_reads") {
				t.Fatalf("ReportableCurrent owed=%v reason=%q err=%v, want discharged=%v", owed, reason, err, tc.discharge)
			}
			reportable, reason, err := f.c.Reportable(f.ctx, o)
			if err != nil || reportable == tc.discharge || (tc.discharge && reason != "already_in_the_record_the_supervisor_reads") {
				t.Fatalf("Reportable %v %q %v, want discharged=%v", reportable, reason, err, tc.discharge)
			}
			answer, err := f.c.Selection(context.Background(), o, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if (answer["standing"] == "discharged") != tc.discharge {
				t.Fatalf("Selection standing %v (%v), want discharged=%v", answer["standing"], answer["dischargeReason"], tc.discharge)
			}
			if tc.discharge && answer["reason"] != "already_in_the_record_the_supervisor_reads" {
				t.Fatalf("Selection %v", answer)
			}
		})
	}
}
