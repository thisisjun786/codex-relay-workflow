package adapter

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// CRW-1314 lane D1c (design §1-2), end to end over the scripted App Server: on the 1.0 path there
// are two session layers, so a management session binds its project owner itself
// (linkage-bind --role parent --creator), the binding writes the execution edge management ->
// project, and the project's upward messages open a turn in that management thread. The seats,
// bindings and edges are written by the relay's own command lines; the transport is the real
// adapter over the fake host's socket and the daemon's own tick (mgmtWorld, CRW-1333).

const ownerA, ownerB, ownerC = "01ownerA", "01ownerB", "01ownerC"

// bindOwner binds a project owner, with --creator when creator is not "".
func (w *mgmtWorld) bindOwner(project, owner, creator string) map[string]any {
	w.t.Helper()
	argv := []string{"linkage-bind", "--role", "parent", "--scope", project, "--task", owner, "--host", "host"}
	if creator != "" {
		argv = append(argv, "--creator", creator)
	}
	return w.relay(argv...)
}

// stageProject stages one project owner's message through the channel's direct staging.
func (w *mgmtWorld) stageProject(kind, project, subject, summary, text string) map[string]any {
	w.t.Helper()
	answer, err := w.c.StageDirect(w.ctx, supervisor.DirectMessage{Kind: kind, Relationship: "project:" + project, Subject: subject, Summary: summary, Text: text}, nil)
	if err != nil {
		w.t.Fatalf("staging %s for %s: %v", kind, project, err)
	}
	return answer
}

// TestManagementProjectE2ETwoSessionsEachHearOnlyTheirOwnProject: two management sessions each bind
// one project owner; each project's message opens a new turn in its own idle management thread and
// in no other, and the transport's record says dispatched.
func TestManagementProjectE2ETwoSessionsEachHearOnlyTheirOwnProject(t *testing.T) {
	w := newMgmtWorld(t)
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--task", mgmtA, "--host", "host")
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtB, "--task", mgmtB, "--host", "host")
	for _, bound := range []map[string]any{w.bindOwner("P-A", ownerA, mgmtA), w.bindOwner("P-B", ownerB, mgmtB)} {
		if bound["managementLink"] == nil {
			t.Fatalf("the project owner binding wrote no management link: %v", bound)
		}
	}
	w.open(mgmtA, mgmtB)
	decision := w.stageProject(supervisor.KindFlowProjectDecisionRequest, "P-A", "rep_a1", "ship P-A before the review?", "owner A asks whether to ship before the review")
	report := w.stageProject(supervisor.KindFlowMilestoneReport, "P-B", "rep_b1", "milestone M1 closed", "owner B closed milestone M1")
	if decision["recipient"] != mgmtA || report["recipient"] != mgmtB || decision["recipientScopeKind"] != "management" {
		t.Fatalf("staged to %v and %v, want %s and %s", decision["recipient"], report["recipient"], mgmtA, mgmtB)
	}
	w.tick(4)
	a, b := w.turnsOf(mgmtA), w.turnsOf(mgmtB)
	if len(a) != 1 || !strings.Contains(a[0], "owner A asks whether to ship") || strings.Contains(a[0], "owner B") ||
		!strings.HasPrefix(a[0], "[codex-session-relay] management message from orchestrator "+ownerA) {
		t.Fatalf("management A's turns %q\n%s", a, w.dump())
	}
	if len(b) != 1 || !strings.Contains(b[0], "owner B closed milestone M1") || strings.Contains(b[0], "owner A") {
		t.Fatalf("management B's turns %q\n%s", b, w.dump())
	}
	if len(w.turnsOf(ownerA))+len(w.turnsOf(ownerB)) != 0 {
		t.Fatal("a project owner was sent its own message")
	}
	for _, id := range []string{decision["messageId"].(string), report["messageId"].(string)} {
		if state := w.messageState(id); state != "dispatched" {
			t.Fatalf("message %s is %s after the ticks, want it sent\n%s", id, state, w.dump())
		}
	}
}

// TestManagementProjectE2EHandoverMovesTheLink: a new management session takes the seat by
// handover; the edge to the project moves with its record (scope_handover, upper_task_id) and the
// project's next message opens a turn in the new owner only.
func TestManagementProjectE2EHandoverMovesTheLink(t *testing.T) {
	w := newMgmtWorld(t)
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--task", mgmtA, "--host", "host")
	bound := w.bindOwner("P-A", ownerA, mgmtA)
	link, _ := bound["managementLink"].(map[string]any)
	w.relay("linkage-handover", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--expect-task", mgmtA,
		"--task", mgmtNew, "--host", "host", "--evidence", "the management session was replaced", "--actor", "test")
	w.open(mgmtA, mgmtNew)
	var handovers int
	if err := w.s.DB.QueryRowContext(w.ctx, "SELECT COUNT(*) FROM journal WHERE kind = 'scope_handover'").Scan(&handovers); err != nil || handovers != 1 {
		t.Fatalf("%d scope_handover rows (%v)", handovers, err)
	}
	var upper string
	if err := w.s.DB.QueryRowContext(w.ctx, "SELECT upper_task_id FROM scope_links WHERE link_id = ?", link["linkId"]).Scan(&upper); err != nil || upper != mgmtNew {
		t.Fatalf("the edge's upper task is %q (%v), want %s", upper, err, mgmtNew)
	}
	staged := w.stageProject(supervisor.KindFlowProjectBlocked, "P-A", "rep_a2", "blocked on the review gate", "owner A is blocked")
	if staged["recipient"] != mgmtNew {
		t.Fatalf("staged to %v, want the new owner %s", staged["recipient"], mgmtNew)
	}
	w.tick(3)
	if got := w.turnsOf(mgmtNew); len(got) != 1 || !strings.Contains(got[0], "owner A is blocked") {
		t.Fatalf("the new owner's turns %q\n%s", got, w.dump())
	}
	if got := w.turnsOf(mgmtA); len(got) != 0 {
		t.Fatalf("the old owner was sent %q", got)
	}
}

// TestManagementProjectE2EStoreSeatFallback: a project whose owner was bound without a management
// creator has no management edge, and its message is heard by the single store seat.
func TestManagementProjectE2EStoreSeatFallback(t *testing.T) {
	w := newMgmtWorld(t)
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "store", "--scope", "store", "--task", storeSeat, "--host", "host")
	if bound := w.bindOwner("P-C", ownerC, ""); bound["managementLink"] != nil {
		t.Fatalf("an owner bound without a management creator got a link: %v", bound)
	}
	w.open(storeSeat)
	staged := w.stageProject(supervisor.KindFlowMilestoneReport, "P-C", "rep_c1", "milestone M1 closed", "owner C reports")
	if staged["recipient"] != storeSeat || staged["recipientScopeKind"] != "store" {
		t.Fatalf("staged %v, want the store seat", staged)
	}
	w.tick(3)
	if got := w.turnsOf(storeSeat); len(got) != 1 || !strings.Contains(got[0], "owner C reports") {
		t.Fatalf("the store seat's turns %q\n%s", got, w.dump())
	}
}

// stageProjectNotice raises one open fault about project, anchored to no relationship, with one
// notification reserved by the daemon's deliverer (a lease well past the ticks), and stages its
// notice through the channel's notice staging; the ledger addresses it to project:<project> (the
// resolveNoticeProject path).
func (w *mgmtWorld) stageProjectNotice(project, fault, notification string) map[string]any {
	w.t.Helper()
	if _, err := w.s.DB.ExecContext(w.ctx, `INSERT INTO fault_ledger (fault_id,product,fault_class,component,severity,signature,scope,scope_key,state,first_seen_at,last_seen_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		fault, "crw", "report_omitted", "component", "broken", `{"turn": "turn-1"}`, `{"projectKey": "`+project+`"}`, project, "open", "t", "t", "t"); err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.s.DB.ExecContext(w.ctx, `INSERT INTO fault_notifications (notification_id,fault_id,product,kind,cycle,state,token,owner,lease_until,attempts,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		notification, fault, "crw", "blocking", 1, "reserved", "token-1314", "relay-daemon", float64(1700003600.5), 1, "t", "t"); err != nil {
		w.t.Fatal(err)
	}
	ledger := &faults.Ledger{Store: w.s, Clock: &delivery.FakeClock{T: 1700000000}}
	notice, err := ledger.NoticeFacts(w.ctx, notification)
	if err != nil || notice == nil {
		w.t.Fatalf("notice facts %q: %v, %v", notification, notice, err)
	}
	if notice["anchor"] != "project:"+project {
		w.t.Fatalf("the notice is about %v, want project:%s", notice["anchor"], project)
	}
	answer, err := supervisor.NoticeChannel{Channel: w.c, Ledger: ledger}.StageNotice(w.ctx, notice)
	if err != nil {
		w.t.Fatalf("staging the notice for %s: %v", project, err)
	}
	return answer
}

// TestManagementProjectE2EProjectNoticeReachesTheManagementSeat: a fault notice addressed to
// project:<key> alone (no relationship) is staged for the management seat directly above the
// project and opens a new turn in that idle management thread only, not in the other management
// session or the project owner.
func TestManagementProjectE2EProjectNoticeReachesTheManagementSeat(t *testing.T) {
	w := newMgmtWorld(t)
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--task", mgmtA, "--host", "host")
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtB, "--task", mgmtB, "--host", "host")
	if bound := w.bindOwner("P-A", ownerA, mgmtA); bound["managementLink"] == nil {
		t.Fatalf("the project owner binding wrote no management link: %v", bound)
	}
	w.bindOwner("P-B", ownerB, mgmtB)
	w.open(mgmtA, mgmtB)
	staged := w.stageProjectNotice("P-A", "abc1314", "n-1314")
	id, _ := staged["messageId"].(string)
	if staged["staged"] != true || staged["recipient"] != mgmtA || id == "" {
		t.Fatalf("the project notice was staged as %v, want it for %s", staged, mgmtA)
	}
	var scopeKind string
	if err := w.s.DB.QueryRowContext(w.ctx, "SELECT json_extract(packet,'$.envelope.recipient.scopeKind') FROM supervisor_messages WHERE message_id = ?", id).Scan(&scopeKind); err != nil || scopeKind != "management" {
		t.Fatalf("the notice's recipient scope kind is %q (%v), want management", scopeKind, err)
	}
	w.tick(3)
	if got := w.turnsOf(mgmtA); len(got) != 1 || !strings.Contains(got[0], "abc1314") {
		t.Fatalf("management A's turns %q\n%s", got, w.dump())
	}
	if len(w.turnsOf(mgmtB))+len(w.turnsOf(ownerA))+len(w.turnsOf(ownerB)) != 0 {
		t.Fatalf("the notice reached another thread\n%s", w.dump())
	}
	if state := w.messageState(id); state != "dispatched" {
		t.Fatalf("the notice %s is %s after the ticks, want it sent\n%s", id, state, w.dump())
	}
}
