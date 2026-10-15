package supervisor

import (
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// CRW-1486/S4 (findings 5 and 7). A thread that held the store seat and registered its management
// seat lays the store seat down, and the seat is then read through that management seat (CRW-1333).
// Read that way it answers only for a user-grade message: a decision Jun owes (an event
// decision_request, a jun_decision_request or flow_jun_decision_request, a fault notice of kind
// decision). Every other message of a project with no management edge above it reads
// unregistered_scope, the same as a store with no seat, so a parent that took the seat over for its
// own project stops hearing every other project's completions. A live store seat folds as before.
//
// The world: project PRJ-1 (Q, the fixture's relationship rel-1) has no initiative supervisor and no
// management edge; thread M holds the store seat; M's own project PRJ-OWN gets the execution edge
// management:M -> project when M registers its management seat.

const (
	handoverManager    = "01manager"
	handoverOwnProject = "PRJ-OWN"
	handoverOwnParent  = "01own-parent"
)

type handoverWorld struct {
	*stageFixture
	t    *testing.T
	host *sendHost
	now  float64
}

func newHandoverWorld(t *testing.T) *handoverWorld {
	t.Helper()
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	w := &handoverWorld{stageFixture: f, t: t, host: &sendHost{status: "idle"}, now: 1_700_000_000}
	// No initiative supervises Q: its project owner reports to the store seat when one answers.
	w.exec("DELETE FROM scope_links WHERE link_id='lnk-project'")
	w.exec("DELETE FROM scope_bindings WHERE binding_id='b-supervisor'")
	w.bind(store.ScopeBindingsRow{BindingID: "bnd-store-m", Role: "supervisor", ScopeKind: "store", ScopeKey: "store", TaskID: handoverManager,
		HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"})
	w.bind(store.ScopeBindingsRow{BindingID: "bnd-own-parent", Role: "parent", ScopeKind: "project", ScopeKey: handoverOwnProject, TaskID: handoverOwnParent,
		HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"})
	return w
}

func (w *handoverWorld) exec(query string, args ...any) {
	w.t.Helper()
	if _, err := w.s.DB.ExecContext(w.ctx, query, args...); err != nil {
		w.t.Fatal(err)
	}
}

func (w *handoverWorld) bind(b store.ScopeBindingsRow) {
	w.t.Helper()
	if err := storeseed.InsertScopeBinding(w.ctx, w.s, b); err != nil {
		w.t.Fatal(err)
	}
}

// register is M registering its management seat: the store seat is laid down (archived, superseded
// by the management binding, as retireStoreSeat leaves it) and M's own project gets its edge.
func (w *handoverWorld) register() {
	w.t.Helper()
	if err := storeseed.ArchiveScopeBinding(w.ctx, w.s, "bnd-store-m", "archived", "bnd-m-"+handoverManager, "t2"); err != nil {
		w.t.Fatal(err)
	}
	w.bind(store.ScopeBindingsRow{BindingID: "bnd-m-" + handoverManager, Role: "supervisor", ScopeKind: "management", ScopeKey: handoverManager,
		TaskID: handoverManager, HostID: "host", Status: "active", Revision: 1, CreatedAt: "t2", UpdatedAt: "t2"})
	if err := storeseed.InsertScopeLink(w.ctx, w.s, store.ScopeLinksRow{LinkID: "lnk-m-own", LinkKind: "execution",
		UpperKind: "management", UpperKey: handoverManager, UpperTaskID: handoverManager,
		LowerKind: "project", LowerKey: handoverOwnProject, LowerTaskID: handoverOwnParent,
		Status: "active", Revision: 1, CreatedAt: "t2", UpdatedAt: "t2"}); err != nil {
		w.t.Fatal(err)
	}
}

// bindStoreSeatAgain binds a live store seat to M again, which answers for itself.
func (w *handoverWorld) bindStoreSeatAgain() {
	w.t.Helper()
	w.bind(store.ScopeBindingsRow{BindingID: "bnd-store-m2", Role: "supervisor", ScopeKind: "store", ScopeKey: "store", TaskID: handoverManager,
		HostID: "host", Status: "active", Revision: 2, CreatedAt: "t3", UpdatedAt: "t3"})
}

// direct stages one direct message and returns its id.
func (w *handoverWorld) direct(kind, relationship, subject, at string) string {
	w.t.Helper()
	answer, err := w.c.StageDirect(w.ctx, DirectMessage{Kind: kind, Relationship: relationship, Subject: subject,
		Summary: kind + " " + subject, Text: "the text of " + subject, At: at}, nil)
	if err != nil {
		w.t.Fatalf("stage %s through %s: %v", kind, relationship, err)
	}
	id, _ := answer["messageId"].(string)
	if id == "" {
		w.t.Fatalf("staging %s answered %v", kind, answer)
	}
	return id
}

// pass is one daemon supervisor pass, a minute after the last one.
func (w *handoverWorld) pass() AutoSendResult {
	w.t.Helper()
	w.now += 60
	got, err := w.c.AutoSend(w.ctx, w.host, w.now, 10, 10, "", "", "")
	if err != nil {
		w.t.Fatalf("supervisor pass: %v", err)
	}
	return got
}

func (w *handoverWorld) row(id string) store.SupervisorMessagesRow {
	w.t.Helper()
	row, err := w.s.SupervisorMessage(w.ctx, id)
	if err != nil {
		w.t.Fatalf("message %s: %v", id, err)
	}
	return row
}

func (w *handoverWorld) journalCount(kind, subject string) int {
	w.t.Helper()
	var n int
	if err := w.s.DB.QueryRowContext(w.ctx, "SELECT COUNT(*) FROM journal WHERE kind=? AND subject=?", kind, subject).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func refusalReason(err error) string {
	var refusal Refusal
	if errors.As(err, &refusal) {
		return refusal.Reason
	}
	return ""
}

// TestSuccessorStoreSeatDoesNotHearOtherProjects: after M registers its management seat, a
// completion of Q (no management edge) reads unregistered_scope through the relationship and
// through 'project:KEY', where the live store seat answered M before.
func TestSuccessorStoreSeatDoesNotHearOtherProjects(t *testing.T) {
	w := newHandoverWorld(t)
	for _, relationship := range []string{"rel-1", "project:PRJ-1"} {
		got, err := w.c.Resolve(w.ctx, relationship)
		if err != nil || got.Recipient != handoverManager || got.RecipientScopeKind != "store" {
			t.Fatalf("live store seat: %s resolved %+v (%v), want %s through the store", relationship, got, err, handoverManager)
		}
	}
	w.register()
	for _, relationship := range []string{"rel-1", "project:PRJ-1"} {
		got, err := w.c.Resolve(w.ctx, relationship)
		if reason := refusalReason(err); reason != "unregistered_scope" {
			t.Fatalf("after the handover %s resolved %+v (%v), want unregistered_scope", relationship, got, err)
		}
	}
	// M's own project still reaches M through its management edge.
	got, err := w.c.Resolve(w.ctx, "project:"+handoverOwnProject)
	if err != nil || got.Recipient != handoverManager || got.RecipientScopeKind != "management" {
		t.Fatalf("own project resolved %+v (%v), want %s through its management seat", got, err, handoverManager)
	}
}

// TestStagedMessageToSuccessorIsHeldUnaddressed: messages of Q staged to M while M held the live
// store seat (a queued completion and a deferred_busy flow completion past its next_eligible_at) are
// held hierarchy_unresolved at the next pass after the handover and open no turn; binding a live
// store seat again releases them.
func TestStagedMessageToSuccessorIsHeldUnaddressed(t *testing.T) {
	w := newHandoverWorld(t)
	completion := stagedID(t, w.stageFixture)
	flow := w.direct(KindFlowCompletion, "project:PRJ-1", "q-done-1", "2023-11-14T22:13:21.000000+00:00")
	w.exec("UPDATE supervisor_messages SET state='deferred_busy',attempt_count=1,next_eligible_at=? WHERE message_id=?", w.now+30, flow)
	for _, id := range []string{completion, flow} {
		if row := w.row(id); row.RecipientTaskID != handoverManager {
			t.Fatalf("message %s staged to %q, want %s", id, row.RecipientTaskID, handoverManager)
		}
	}
	w.register()
	for range 3 {
		w.pass()
	}
	if len(w.host.sends) != 0 {
		t.Fatalf("a message of another project opened %d turn(s) in %s after the handover", len(w.host.sends), handoverManager)
	}
	for _, id := range []string{completion, flow} {
		row := w.row(id)
		if row.HoldReason.String != store.SupervisorHoldUnaddressed || !row.Unsent() {
			t.Fatalf("message %s is %s held %q, want unsent and held %s", id, row.State, row.HoldReason.String, store.SupervisorHoldUnaddressed)
		}
		if n := w.journalCount("supervisor_message_unaddressed", id); n != 1 {
			t.Fatalf("message %s journaled supervisor_message_unaddressed %d times, want once", id, n)
		}
	}
	w.bindStoreSeatAgain()
	for _, id := range []string{completion, flow} {
		w.now += 60
		result, err := w.c.Attempt(w.ctx, id, w.host, w.now)
		if err != nil || result["deliveryState"] != "dispatched" {
			t.Fatalf("message %s after the live seat came back: %v (%v), want dispatched", id, result, err)
		}
		if n := w.journalCount("supervisor_message_reopened", id); n != 1 {
			t.Fatalf("message %s journaled supervisor_message_reopened %d times, want once", id, n)
		}
	}
	if len(w.host.sends) != 2 {
		t.Fatalf("the live seat received %d turns, want 2", len(w.host.sends))
	}
}

// TestSuccessorCarriesAUserGradeMessageThroughEveryStep: after the handover a decision Jun owes for
// Q still reaches M through the seat it took over, through staging, the claim and the transport fence,
// for the event path, the direct path and a fault notice; ReportHolds names a waiting completion of Q
// as held and not a waiting decision_request.
func TestSuccessorCarriesAUserGradeMessageThroughEveryStep(t *testing.T) {
	w := newHandoverWorld(t)
	w.exec("UPDATE work_reports SET cxc_status='NEEDS_HUMAN',cxc_reason='needs_jun' WHERE event_id=?", w.event)
	w.register()
	_, staged := w.staged(t)
	event, _ := staged["messageId"].(string)
	jun := w.direct(KindFlowJunDecisionRequest, "project:PRJ-1", "q-jun-1", "2023-11-14T22:13:22.000000+00:00")
	w.exec(`INSERT INTO fault_ledger (fault_id,product,fault_class,component,severity,signature,scope,scope_key,state,first_seen_at,last_seen_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		"abc123", "crw", "report_omitted", "component", "broken", `{"relationship": "rel-1", "turn": "turn-1"}`, `{"projectKey": "PRJ-1"}`, "P", "open", "t", "t", "t")
	w.exec(`INSERT INTO fault_notifications (notification_id,fault_id,product,kind,reason,cycle,state,lease_until,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		"n-dec", "abc123", "crw", "decision", "raised:needs a decision", 1, "reserved", w.now+10_000, "t", "t")
	ledger := &faults.Ledger{Store: w.s, Clock: &delivery.FakeClock{T: 1700000003}}
	notices := NoticeChannel{Channel: w.c, Ledger: ledger, Host: w.host}
	facts, err := ledger.NoticeFacts(w.ctx, "n-dec")
	if err != nil || facts == nil {
		t.Fatalf("notice facts: %v, %v", facts, err)
	}
	answer, err := notices.StageNotice(w.ctx, facts)
	if err != nil {
		t.Fatalf("a decision notice of Q was not staged after the handover: %v", err)
	}
	notice, _ := answer["messageId"].(string)
	for _, id := range []string{event, jun, notice} {
		if row := w.row(id); row.RecipientTaskID != handoverManager {
			t.Fatalf("message %s staged to %q, want %s", id, row.RecipientTaskID, handoverManager)
		}
	}
	for _, id := range []string{event, jun, notice} {
		w.now += 60
		if id == notice {
			// a notice goes out only while the contact of the fault's parent was measured deliverable
			w.exec("INSERT INTO recipient_lifecycle (task_id, deliverable, observed_at) VALUES ('parent','yes',?)", delivery.ISOOf(w.now-1))
		}
		result, err := w.c.Attempt(w.ctx, id, w.host, w.now)
		if err != nil || result["deliveryState"] != "dispatched" {
			t.Fatalf("user-grade message %s: %v (%v), want dispatched to %s", id, result, err, handoverManager)
		}
		if row := w.row(id); row.HoldReason.Valid {
			t.Fatalf("user-grade message %s is held %q", id, row.HoldReason.String)
		}
	}
	if len(w.host.sends) != 3 {
		t.Fatalf("%s received %d turns, want 3", handoverManager, len(w.host.sends))
	}
	standing := map[string]any{"standing": []any{
		map[string]any{"obligationId": "obl-decision", "kind": "decision_request", "relationId": "rel-1", "decision": map[string]any{"report": true}},
		map[string]any{"obligationId": "obl-completion", "kind": "completion", "relationId": "rel-1", "decision": map[string]any{"report": true}},
	}}
	holds, err := w.c.ReportHolds(w.ctx, standing)
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 1 || holds[0]["reason"] != "unregistered_scope" {
		t.Fatalf("report holds %v, want one unregistered_scope hold for the completion", holds)
	}
	if ids, _ := holds[0]["obligationIds"].([]string); len(ids) != 1 || ids[0] != "obl-completion" {
		t.Fatalf("the hold names %v, want only the completion", holds[0]["obligationIds"])
	}
}

// TestOwnEscalationIsTheHeadAfterTheHandover (finding 7): older completions of Q staged to M wait
// in front of an escalation from M's own project; after the handover they are held, and the
// escalation is the next message M receives.
func TestOwnEscalationIsTheHeadAfterTheHandover(t *testing.T) {
	w := newHandoverWorld(t)
	completion := stagedID(t, w.stageFixture)
	flow := w.direct(KindFlowCompletion, "project:PRJ-1", "q-done-1", "2023-11-14T22:13:21.000000+00:00")
	w.register()
	escalation := w.direct(KindFlowEscalation, "project:"+handoverOwnProject, "own-esc-1", "2023-11-14T22:13:30.000000+00:00")
	for range 4 {
		w.pass()
	}
	if row := w.row(escalation); row.Unsent() {
		t.Fatalf("the own escalation is still %s held %q", row.State, row.HoldReason.String)
	}
	if len(w.host.sends) != 1 {
		t.Fatalf("%s received %d turns, want only the escalation", handoverManager, len(w.host.sends))
	}
	for _, id := range []string{completion, flow} {
		if row := w.row(id); !row.Unsent() || row.HoldReason.String != store.SupervisorHoldUnaddressed {
			t.Fatalf("Q message %s is %s held %q, want held %s", id, row.State, row.HoldReason.String, store.SupervisorHoldUnaddressed)
		}
	}
}
