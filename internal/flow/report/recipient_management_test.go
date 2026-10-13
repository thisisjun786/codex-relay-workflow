package report

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// A report's recipient when a management session bound the project owner (CRW-1314 lane D1c): the
// management seat directly above the project hears every report-up kind of the project, a decision
// only Jun can make included, so a project owner's report reaches its own management session.

const (
	testManagement      = "01mgmtA"
	testOtherManagement = "01mgmtB"
)

// manage binds the management seat (keyed by its own thread) and the execution edge from it to the
// world's project.
func (w *world) manage(seat string) {
	w.t.Helper()
	w.bind("bnd-m-"+seat, "supervisor", "management", seat, seat)
	if err := storeseed.InsertScopeLink(ctx, w.st, store.ScopeLinksRow{LinkID: "lnk-m-" + seat, LinkKind: "execution", UpperKind: "management", UpperKey: seat, UpperTaskID: seat,
		LowerKind: "project", LowerKey: testProject, LowerTaskID: testOrch, Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		w.t.Fatal(err)
	}
}

// TestReportUpReachesTheManagementSeat: a milestone report of a project under a management seat is
// recorded for the seat's owner and delivered to that thread, and the store seat does not take it.
func TestReportUpReachesTheManagementSeat(t *testing.T) {
	w := newWorld(t)
	w.seat("manager-seat")
	w.manage(testManagement)
	res := w.up(KindMilestoneReport, "M1", "milestone M1 is closed")
	if res.Recipient.State != RecipientManagement || res.Recipient.Thread != testManagement || res.Recipient.ProjectKey != testProject || res.Recipient.InitiativeKey != "" {
		t.Fatalf("recipient %+v, want the management seat %s", res.Recipient, testManagement)
	}
	sent := w.sender.supervisorSent()
	if len(sent) != 1 || sent[0].Thread != testManagement || sent[0].Initiative != "" || !strings.Contains(sent[0].Text, "milestone M1 is closed") ||
		strings.Contains(sent[0].Text, "<initiative>") || !strings.Contains(sent[0].Text, "crw flow reports --project "+testProject+" --unread") {
		t.Fatalf("sent %+v", sent)
	}
	if res.Delivery != ResultAccepted || res.Standing {
		t.Fatalf("result %+v", res)
	}
	rows := w.rows(KindReport)
	if len(rows) != 1 || rows[0].Str("recipient_state") != RecipientManagement || rows[0].Str("recipient_thread") != testManagement {
		t.Fatalf("rows %+v", rows)
	}
}

// TestJunDecisionReportReachesTheManagementSeat: a decision only Jun can make goes to the management
// session above the project, the same recipient as the project's other reports; with no management
// edge it stays the store seat's row (TestJunDecisionReportIsARowForTheStoreSeat).
func TestJunDecisionReportReachesTheManagementSeat(t *testing.T) {
	w := newWorld(t)
	w.seat("manager-seat")
	w.manage(testManagement)
	res, err := w.writer().Up(ctx, UpRequest{Kind: KindJunDecisionRequest, Run: testRun, Subject: "ship before the review?", Body: []byte("evidence")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Recipient.State != RecipientManagement || res.Recipient.Thread != testManagement || res.Delivery != ResultAccepted {
		t.Fatalf("result %+v", res)
	}
	if sent := w.sender.supervisorSent(); len(sent) != 1 || sent[0].Thread != testManagement {
		t.Fatalf("sent %+v", sent)
	}
}

// TestReportRecipientsOfTwoManagementSessions: two projects, each under its own management session,
// each report to their own; a handover of the seat moves the next report to the new owner.
func TestReportRecipientsOfTwoManagementSessions(t *testing.T) {
	w := newWorld(t)
	w.manage(testManagement)
	w.bind("bnd-parent-2", "parent", "project", "PRJ-2", "orch-2")
	w.bind("bnd-m-"+testOtherManagement, "supervisor", "management", testOtherManagement, testOtherManagement)
	if err := storeseed.InsertScopeLink(ctx, w.st, store.ScopeLinksRow{LinkID: "lnk-m-2", LinkKind: "execution", UpperKind: "management", UpperKey: testOtherManagement,
		UpperTaskID: testOtherManagement, LowerKind: "project", LowerKey: "PRJ-2", LowerTaskID: "orch-2", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		t.Fatal(err)
	}
	resolver := StoreResolver{Store: w.st}
	first, err := resolver.Recipient(ctx, testPlan, KindProjectBlocked)
	if err != nil || first.Thread != testManagement || first.State != RecipientManagement {
		t.Fatalf("project %s: %+v (%v)", testProject, first, err)
	}
	second, err := resolverFor(w, "plan-2", "PRJ-2").Recipient(ctx, "plan-2", KindJunDecisionRequest)
	if err != nil || second.Thread != testOtherManagement || second.State != RecipientManagement {
		t.Fatalf("project PRJ-2: %+v (%v)", second, err)
	}
	if err := storeseed.ArchiveScopeBinding(ctx, w.st, "bnd-m-"+testManagement, "archived", "bnd-m-new", "t2"); err != nil {
		t.Fatal(err)
	}
	w.bind("bnd-m-new", "supervisor", "management", testManagement, "01mgmtNew")
	if err := storeseed.RepointScopeLink(ctx, w.st, "lnk-m-"+testManagement, "active", testOrch, "01mgmtNew", "t2"); err != nil {
		t.Fatal(err)
	}
	moved, err := resolver.Recipient(ctx, testPlan, KindProjectDecisionRequest)
	if err != nil || moved.Thread != "01mgmtNew" || moved.State != RecipientManagement {
		t.Fatalf("after the handover: %+v (%v)", moved, err)
	}
}

// resolverFor puts a second plan of project into the world's store and answers the store resolver.
func resolverFor(w *world, plan, project string) StoreResolver {
	w.t.Helper()
	second := &world{t: w.t, st: w.st, repo: w.repo}
	second.putPlanOf(plan, project)
	return StoreResolver{Store: w.st}
}

// putPlanOf writes the first revision of plan, a plan of project with one node.
func (w *world) putPlanOf(plan, project string) {
	w.t.Helper()
	raw, err := json.Marshal(map[string]any{"schema": dag.SchemaRevision, "plan_id": plan, "project_key": project, "request_id": "r-" + plan,
		"expected_parent_revision": 0, "author_task_id": "task-test", "changes": []any{map[string]any{"op": dag.OpAddNode, "node": testsupport.NodeDoc("A", dag.NodeNonPR)}}})
	if err != nil {
		w.t.Fatal(err)
	}
	rev, err := dag.DecodeRevision(raw)
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.repo.Put(ctx, rev); err != nil {
		w.t.Fatal(err)
	}
}

// TestReportsOfOneProjectForItsManagementSession: the notice to a management session names the
// listing of its own project's reports, and that listing holds no other project's report, so with
// two management sessions neither is pointed at (or lists) the other's reports.
func TestReportsOfOneProjectForItsManagementSession(t *testing.T) {
	w := newWorld(t)
	w.manage(testManagement)
	res := w.up(KindProjectBlocked, "", "blocked on the schema decision")
	sent := w.sender.supervisorSent()
	if len(sent) != 1 || !strings.Contains(sent[0].Text, "crw flow reports --project "+testProject+" --unread") {
		t.Fatalf("the notice does not name the project's own listing: %+v", sent)
	}
	if _, err := w.writer().Rows.Append(ctx, NewRow{Kind: KindReport, Payload: map[string]any{"report_id": "rep-other", "report_kind": KindProjectBlocked, "run": testRun,
		"recipient_state": RecipientManagement, "recipient_thread": testOtherManagement, "recipient_project": "PRJ-2"}}); err != nil {
		t.Fatal(err)
	}
	own, err := ReportsMatching(ctx, w.st.DB, ReportFilter{Project: testProject, Unread: true})
	if err != nil || len(own) != 1 || own[0].ID != res.ReportID {
		t.Fatalf("the reports of %s: %+v (%v)", testProject, own, err)
	}
	other, err := ReportsMatching(ctx, w.st.DB, ReportFilter{Project: "PRJ-2", Unread: true})
	if err != nil || len(other) != 1 || other[0].ID != "rep-other" {
		t.Fatalf("the reports of PRJ-2: %+v (%v)", other, err)
	}
	if all, err := Reports(ctx, w.st.DB, "", true); err != nil || len(all) != 2 {
		t.Fatalf("every unread report: %+v (%v)", all, err)
	}
}
