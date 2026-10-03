package supervisor

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type unclaimedChild struct {
	*noticeWorld
	r         faults.ManagedReadingRequest
	directory string
}

func newUnclaimedChild(t *testing.T) *unclaimedChild {
	t.Helper()
	w := newNoticeWorld(t)
	r := faults.ManagedReadingRequest{Selection: store.StateSelection{Path: filepath.Dir(w.s.Path)}, Root: filepath.Join(w.root, "markers"), Workspace: filepath.Join(w.root, "work"), Assignment: delivery.AssignmentID("dispatch-1"), Session: "child", Turn: "business", Now: nsAt}
	dir, err := delivery.AssignmentDir(r.Root, r.Workspace, r.Assignment)
	if err != nil {
		t.Fatal(err)
	}
	c := &unclaimedChild{w, r, dir}
	w.exec(t, "DELETE FROM work_reports")
	w.exec(t, "DELETE FROM events")
	w.exec(t, "UPDATE relationships SET child_cwd=?", r.Workspace)
	w.exec(t, "INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,opened_at) VALUES('rel-1',1,'dispatch-1','bound','standby',?)", nsAt)
	w.exec(t, "INSERT INTO generation_turns(relationship_id,execution_generation,turn_id,evidence,actor,detail,admitted_at) VALUES('rel-1',1,'business','explicit_admission_bound:standby','child','admitted',?)", nsAt)
	w.exec(t, "INSERT INTO managed_start_requests(request_id,issue_key,request_fingerprint,fingerprint_version,workspace,marker_root,socket_identity,create_request_id,dispatch_request_id,state,revision,child_task_id,standby_turn_id,relationship_id,execution_generation,receipt_status,created_at,updated_at) VALUES('req-1','REL-1','fp','v1',?,?,'sock','create-1','dispatch-1','attached',3,'child','standby','rel-1',1,'accepted',?,?)", r.Workspace, r.Root, nsAt, nsAt)
	w.exec(t, "INSERT INTO assignment_settlements VALUES('rel-1','child','business','completed',?)", nsAt)
	c.marker(t, "intent.json", map[string]any{"dispatchRequestIdHash": r.Assignment, "workspace": r.Workspace, "dbPath": w.s.Path, "issueKey": "REL-1"})
	c.marker(t, "bound.json", map[string]any{"sessionId": "child", "taskId": "child"})
	c.marker(t, "relationship.json", map[string]any{"relationshipId": "rel-1", "executionGeneration": 1})
	return c
}

func (c *unclaimedChild) marker(t *testing.T, name string, value any) {
	t.Helper()
	path := filepath.Join(c.directory, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func (c *unclaimedChild) observe(t *testing.T) map[string]any {
	t.Helper()
	answer, err := (OmissionObserver{}).Observe(c.ctx, c.r)
	if err != nil {
		t.Fatal(err)
	}
	return answer.(map[string]any)
}

func TestCRW398UnclaimedManagedChildReachesFaultNotification(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "no Stop", true: "marker_unclaimed Stop"}[stop], func(t *testing.T) {
			c := newUnclaimedChild(t)
			if stop {
				c.marker(t, "hook/child/business/1.json", map[string]any{"sessionId": "child", "turnId": "business", "observation": "marker_unclaimed", "at": nsAt})
			}
			reading := c.observe(t)
			if reading["reportingState"] != "unreported" || reading["reason"] != delivery.OmittedReason || reading["owed"] != true || reading["stopObservation"] != nil {
				t.Fatalf("admitted settled child with no claim or report: %v", reading)
			}
			sw := &faults.Sweeper{Store: c.s, Selection: c.r.Selection, ManagedObserver: OmissionObserver{}, Now: c.clock.ISO, HostRecordPath: filepath.Join(c.root, "absent-host-record")}
			batch, err := sw.Sweep(c.ctx, "crw")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = sw.RecordAll(c.ctx, c.ledger, batch); err != nil {
				t.Fatal(err)
			}
			row, err := c.s.One(c.ctx, "SELECT n.notification_id,n.kind,n.state,f.severity FROM fault_notifications n JOIN fault_ledger f USING(fault_id) WHERE f.fault_class='report_omitted'")
			if err != nil || row == nil || row.Text("severity") != "broken" || row.Text("kind") != "blocking" || row.Text("state") != "pending" {
				t.Fatalf("omission did not queue a blocking notification: %v, %v", row, err)
			}
			staged, err := c.channel.StageNotice(c.ctx, c.facts(t, row.Text("notification_id")))
			if err != nil || staged["staged"] != true || staged["sender"] != "parent" || staged["recipient"] != "supervisor" {
				t.Fatalf("existing hierarchy did not stage the fault packet: %v, %v", staged, err)
			}
			var claims, reports int
			if err = c.s.DB.QueryRow("SELECT count(*) FROM reporting_sessions").Scan(&claims); err != nil {
				t.Fatal(err)
			}
			if err = c.s.DB.QueryRow("SELECT count(*) FROM events").Scan(&reports); err != nil || claims != 0 || reports != 0 {
				t.Fatalf("observer invented claim/report: %d/%d, %v", claims, reports, err)
			}
		})
	}
}

func TestCRW398ClaimedChildTimingIsUnchanged(t *testing.T) {
	c := newUnclaimedChild(t)
	c.marker(t, "claims/child/claim.json", map[string]any{"sessionId": "child", "dispatchRequestId": "dispatch-1"})
	c.marker(t, "hook/child/business/1.json", map[string]any{"sessionId": "child", "turnId": "business", "observation": "undeclared_turn_end", "decisionState": "unresolved_handoff", "at": nsAt})
	if err := c.s.RecordReportingSession(c.ctx, store.ReportingSessionsRow{AssignmentID: c.r.Assignment, SessionID: "child", DispatchRequestID: "dispatch-1", MarkerRoot: c.r.Root, Workspace: c.r.Workspace, IssueKey: sql.NullString{String: "REL-1", Valid: true}, Capability: "declarations/1", RecordedAt: nsAt}); err != nil {
		t.Fatal(err)
	}
	if reading := c.observe(t); reading["reportingState"] != "unreported" || reading["owed"] != true {
		t.Fatalf("claimed daemon reading changed: %v", reading)
	}
	for _, tc := range []struct {
		seconds float64
		owed    bool
	}{{0.003, false}, {300.14, true}} {
		reading := delivery.DeriveOmission(c.ctx, c.s, filepath.Dir(c.s.Path), "rel-1", "business", delivery.ISOOf(nsNow+tc.seconds), 300)
		if reading.Get("reportingState") != "unreported" || reading.Get("owed") != tc.owed {
			t.Fatalf("claimed child at +%v seconds: %v", tc.seconds, reading)
		}
	}
}
