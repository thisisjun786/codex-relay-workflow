package supervisor

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func ackFlowReport1612(t *testing.T, f *stageFixture, run, id string) {
	t.Helper()
	_, err := f.s.DB.ExecContext(f.ctx, `INSERT INTO flow_events (run_id,seq,kind,call_id,node_id,subject,payload_json,recorded_at)
		SELECT ?,COALESCE(MAX(seq)+1,1099511627776),'report_read','','','',json_object('report_id',?,'turn','parent-already-read'),?
		FROM flow_events WHERE run_id=?`, run, id, f.at, run)
	if err != nil {
		t.Fatal(err)
	}
}

func TestFlowReportAck1612SkipsQueuedAndRacingReports(t *testing.T) {
	for _, kind := range []string{KindFlowMilestoneReport, KindFlowProjectBlocked, KindFlowProjectDecisionRequest, KindFlowJunDecisionRequest, KindFlowNoNodeStarted, KindFlowMergeRequest} {
		for _, late := range []bool{false, true} {
			for _, byID := range []bool{false, true} {
				name := kind
				if late {
					name += "/at-transport"
				} else {
					name += "/queued"
				}
				if byID {
					name += "/by-id"
				} else {
					name += "/daemon"
				}
				t.Run(name, func(t *testing.T) {
					f := flowRecordFixture(t)
					id := f.stageFlow(t, kind, flowRecordRun, "rep-1")
					ack := func() { ackFlowReport1612(t, f, flowRecordRun, "rep-1") }
					if late {
						f.c.beforeTransport = ack
					} else {
						ack()
					}
					h := &sendHost{status: "idle"}
					var err error
					if byID {
						_, err = f.c.Attempt(f.ctx, id, h, 1_700_000_000)
					} else {
						_, err = f.c.attempt(f.ctx, id, h, 1_700_000_000, 0, "relay-daemon", daemonAttempt)
					}
					if err != nil {
						t.Fatal(err)
					}
					if len(h.sends) != 0 {
						t.Fatalf("acked report started a parent turn: %d sends", len(h.sends))
					}
					row, err := f.c.Get(f.ctx, id)
					if err != nil || row.State != "read" || row.LeaseOwner.Valid || row.LeaseUntil.Valid || row.NextEligibleAt.Valid {
						t.Fatalf("acked message did not finish: %+v %v", row, err)
					}
					attempts, err := f.s.SupervisorAttempts(f.ctx, id)
					if err != nil || len(attempts) != 1 || attempts[0].State != "withheld_pre_send" || attempts[0].SendAttempted != "no" || attempts[0].TransportStartedAt.Valid || attempts[0].TurnID.Valid || !strings.Contains(attempts[0].Record, "report_ack") {
						t.Fatalf("skip fabricated a transport: %+v %v", attempts, err)
					}
					rows := f.flowRows(t, flowRecordRun)
					if len(rows) != 1 || rows[0].kind != FlowReportDeliveryKind || rows[0].payload["result"] != "accepted" || rows[0].payload["observation"] != "read" || rows[0].payload["reason"] != "report_ack" || rows[0].payload["report_id"] != "rep-1" || rows[0].payload["turn_id"] != "parent-already-read" {
						t.Fatalf("missing already-read delivery result: %+v", rows)
					}
					if _, err := f.s.SupervisorReadback(f.ctx, id); !errors.Is(err, sql.ErrNoRows) {
						t.Fatalf("skip fabricated a host readback: %v", err)
					}
					f.c.beforeTransport = nil
					if again := f.stageFlow(t, kind, flowRecordRun, "rep-1"); again != id {
						t.Fatalf("restaging changed message id: %s", again)
					}
					if _, err := f.c.Attempt(f.ctx, id, h, 1_700_000_200); err != nil {
						t.Fatal(err)
					}
					if len(h.sends) != 0 || len(f.flowRows(t, flowRecordRun)) != 1 {
						t.Fatal("retry or restage redelivered the acked report")
					}
					after, err := f.s.SupervisorAttempts(f.ctx, id)
					if err != nil || len(after) != 1 {
						t.Fatalf("retry attempted a finished message: %+v %v", after, err)
					}
				})
			}
		}
	}
}

func TestFlowReportAck1612UnackedReportsStillSend(t *testing.T) {
	for _, kind := range []string{KindFlowMilestoneReport, KindFlowProjectBlocked, KindFlowProjectDecisionRequest, KindFlowJunDecisionRequest, KindFlowNoNodeStarted, KindFlowMergeRequest} {
		t.Run(kind, func(t *testing.T) {
			f := flowRecordFixture(t)
			id := f.stageFlow(t, kind, flowRecordRun, "rep-1")
			h := &sendHost{status: "idle"}
			result, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
			if err != nil || result["deliveryState"] != "dispatched" || len(h.sends) != 1 {
				t.Fatalf("unacked report not sent: %+v %v, sends %d", result, err, len(h.sends))
			}
			rows := f.flowRows(t, flowRecordRun)
			if len(rows) != 1 || rows[0].payload["result"] != "accepted" || rows[0].payload["observation"] != "dispatched" || rows[0].payload["reason"] != "" {
				t.Fatalf("unacked delivery changed: %+v", rows)
			}
		})
	}
}

func TestFlowReportAck1612OtherMessagesAndAcksDoNotSuppressDelivery(t *testing.T) {
	for _, scene := range []string{"other-report", "other-run", "missing-report", "escalation", "completion"} {
		t.Run(scene, func(t *testing.T) {
			f := flowRecordFixture(t)
			kind := KindFlowProjectDecisionRequest
			run := flowRecordRun
			switch scene {
			case "other-report":
				ackFlowReport1612(t, f, flowRecordRun, "rep-other")
			case "other-run":
				_, err := f.s.DB.ExecContext(f.ctx, `INSERT INTO flow_runs (run_id,workflow,script_digest,helper_digests_json,args_digest,plan_id,plan_revision,actor_task_id,epoch_nonce,started_at)
					VALUES ('run-other','project-lane','d','[]','a','plan-1',1,'parent','n','2026-10-15T00:00:00Z')`)
				if err != nil {
					t.Fatal(err)
				}
				ackFlowReport1612(t, f, "run-other", "rep-1")
			case "missing-report":
				_, err := f.s.DB.ExecContext(f.ctx, `INSERT INTO flow_runs (run_id,workflow,script_digest,helper_digests_json,args_digest,plan_id,plan_revision,actor_task_id,epoch_nonce,started_at)
					VALUES ('run-missing','project-lane','d','[]','a','plan-1',1,'parent','n','2026-10-15T00:00:00Z')`)
				if err != nil {
					t.Fatal(err)
				}
				ackFlowReport1612(t, f, "run-missing", "rep-1")
				run = "run-missing"
			case "escalation":
				kind = KindFlowEscalation
				ackFlowReport1612(t, f, flowRecordRun, "rep-1")
			case "completion":
				kind = KindFlowCompletion
				ackFlowReport1612(t, f, flowRecordRun, "rep-1")
			}
			id := f.stageFlow(t, kind, run, "rep-1")
			h := &sendHost{status: "idle"}
			result, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
			if err != nil || result["deliveryState"] != "dispatched" || len(h.sends) != 1 {
				t.Fatalf("unrelated ack suppressed message: %+v %v, sends %d", result, err, len(h.sends))
			}
			rows := f.flowRows(t, run)
			if len(rows) != 1 || rows[0].payload["observation"] != "dispatched" || rows[0].payload["reason"] != "" {
				t.Fatalf("unrelated ack changed result: %+v", rows)
			}
		})
	}
}

func TestFlowReportAck1612RecordFailureRollsBackWithoutSending(t *testing.T) {
	f := flowRecordFixture(t)
	id := f.stageFlow(t, KindFlowProjectDecisionRequest, flowRecordRun, "rep-1")
	ackFlowReport1612(t, f, flowRecordRun, "rep-1")
	_, err := f.s.DB.ExecContext(f.ctx, `CREATE TRIGGER test_ack_delivery_abort BEFORE INSERT ON flow_events
		WHEN NEW.kind='report_delivery' AND json_extract(NEW.payload_json,'$.reason')='report_ack'
		BEGIN SELECT RAISE(ABORT,'test refuses ack delivery record'); END`)
	if err != nil {
		t.Fatal(err)
	}
	h := &sendHost{status: "idle"}
	_, err = f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	if err == nil || !strings.Contains(err.Error(), "test refuses ack delivery record") || len(h.sends) != 0 {
		t.Fatalf("failed record sent or swallowed failure: %v, sends %d", err, len(h.sends))
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.State != "sending" {
		t.Fatalf("failed fence partially completed message: %+v %v", row, err)
	}
	attempts, err := f.s.SupervisorAttempts(f.ctx, id)
	if err != nil || len(attempts) != 1 || attempts[0].State != "held_uncertain" || attempts[0].TransportStartedAt.Valid {
		t.Fatalf("failed fence partly settled attempt: %+v %v", attempts, err)
	}
	if len(f.flowRows(t, flowRecordRun)) != 0 {
		t.Fatal("failed fence committed a delivery result")
	}
}

func TestFlowReportAck1612KeepsAHoldSetDuringTheClaim(t *testing.T) {
	for _, hold := range []string{"busy_cap", "superseded_by_report", "hierarchy_unresolved"} {
		t.Run(hold, func(t *testing.T) {
			f := flowRecordFixture(t)
			id := f.stageFlow(t, KindFlowProjectDecisionRequest, flowRecordRun, "rep-1")
			f.c.beforeTransport = func() {
				ackFlowReport1612(t, f, flowRecordRun, "rep-1")
				if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET hold_reason=? WHERE message_id=?", hold, id); err != nil {
					t.Fatal(err)
				}
			}
			h := &sendHost{status: "idle"}
			if _, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000); err != nil {
				t.Fatal(err)
			}
			row, err := f.c.Get(f.ctx, id)
			if err != nil || row.State != "read" || !row.HoldReason.Valid || row.HoldReason.String != hold || len(h.sends) != 0 {
				t.Fatalf("ack settlement lost the existing hold or sent: %+v %v, sends %d", row, err, len(h.sends))
			}
			rows := f.flowRows(t, flowRecordRun)
			if len(rows) != 1 || rows[0].payload["reason"] != "report_ack" || rows[0].payload["observation"] != "read" {
				t.Fatalf("hold changed the acknowledgement evidence: %+v", rows)
			}
		})
	}
}
