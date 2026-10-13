package supervisor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// The supervisor channel's direct messages (CRW-1333, design CRW-1314 §2): 'initiative:KEY' resolves
// a lead (the initiative's owner) to the management seat above it, falling back to the single store
// seat when no management edge stands; 'run:ID' resolves a run to its owner (flow_runs.actor_task_id);
// StageDirect stages the eventless kinds of the design the way StageNotice stages a fault notice.

const (
	directManagement      = "01mgmtA"
	directOtherManagement = "01mgmtB"
	directOtherLead       = "01leadB"
	directOtherInitiative = "INI-2"
)

// manage puts the initiative under a management seat: the seat's binding (keyed by the seat id,
// owned by task) and the execution edge management -> initiative.
func (w *storeRecipientWorld) manage(seat, task, initiative, lead string) {
	w.t.Helper()
	if err := storeseed.InsertScopeBinding(w.ctx, w.s, store.ScopeBindingsRow{BindingID: "bnd-m-" + seat,
		Role: "supervisor", ScopeKind: "management", ScopeKey: seat, TaskID: task,
		HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		w.t.Fatal(err)
	}
	w.edge(seat, task, initiative, lead)
}

// edge writes only the execution edge from a management seat to an initiative.
func (w *storeRecipientWorld) edge(seat, task, initiative, lead string) {
	w.t.Helper()
	if err := storeseed.InsertScopeLink(w.ctx, w.s, store.ScopeLinksRow{LinkID: "lnk-m-" + seat + "-" + initiative, LinkKind: "execution",
		UpperKind: "management", UpperKey: seat, UpperTaskID: task,
		LowerKind: "initiative", LowerKey: initiative, LowerTaskID: lead,
		Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		w.t.Fatal(err)
	}
}

// lead binds one more initiative owner, with no project under it.
func (w *storeRecipientWorld) lead(initiative, task string) {
	w.t.Helper()
	if err := storeseed.InsertScopeBinding(w.ctx, w.s, store.ScopeBindingsRow{BindingID: "bnd-lead-" + initiative,
		Role: "supervisor", ScopeKind: "initiative", ScopeKey: initiative, TaskID: task,
		HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *storeRecipientWorld) channel() *Channel {
	return &Channel{Store: w.s, Linkage: StoreLinkage{w.s}, Program: "codex-session-relay"}
}

// TestDirectResolveInitiativeReachesTheManagementSeat: a lead reports to the management seat above
// its initiative, and two management sessions each receive only their own lead's messages.
func TestDirectResolveInitiativeReachesTheManagementSeat(t *testing.T) {
	w := newStoreRecipientWorld(t)
	w.superviseInitiative()
	w.manage(directManagement, directManagement, storeRecipientInitiative, storeRecipientSupervisor)
	w.lead(directOtherInitiative, directOtherLead)
	w.manage(directOtherManagement, directOtherManagement, directOtherInitiative, directOtherLead)
	c := w.channel()
	for _, want := range []struct{ initiative, sender, recipient string }{
		{storeRecipientInitiative, storeRecipientSupervisor, directManagement},
		{directOtherInitiative, directOtherLead, directOtherManagement},
	} {
		got, err := c.Resolve(w.ctx, "initiative:"+want.initiative)
		if err != nil {
			t.Fatalf("initiative %s: %v", want.initiative, err)
		}
		if got.Sender != want.sender || got.Recipient != want.recipient || got.RecipientScopeKind != "management" || got.InitiativeKey != want.initiative || got.ProjectKey != "" {
			t.Fatalf("initiative %s resolved %+v, want %s -> management seat %s", want.initiative, got, want.sender, want.recipient)
		}
	}
	// The issue walk ignores the management level: an issue's report still goes to the lead.
	got, err := w.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got.Recipient != storeRecipientSupervisor || got.RecipientScopeKind != "initiative" || got.Sender != storeRecipientParent {
		t.Fatalf("the issue's report resolved %+v, want the lead %s", got, storeRecipientSupervisor)
	}
	project, err := c.Resolve(w.ctx, "project:"+storeRecipientProject)
	if err != nil {
		t.Fatal(err)
	}
	if project.Recipient != storeRecipientSupervisor || project.RecipientScopeKind != "initiative" {
		t.Fatalf("the project's report resolved %+v, want the lead", project)
	}
}

// TestDirectResolveInitiativeFallsBackToTheStoreSeat: an initiative with no management edge reaches
// the single store seat, and with neither there is nobody to tell.
func TestDirectResolveInitiativeFallsBackToTheStoreSeat(t *testing.T) {
	w := newStoreRecipientWorld(t)
	w.superviseInitiative()
	c := w.channel()
	if _, err := c.Resolve(w.ctx, "initiative:"+storeRecipientInitiative); err == nil {
		t.Fatal("an initiative with no management edge and no store seat resolved")
	} else if reason, _ := w.refusal(err); reason != "unregistered_scope" {
		t.Fatalf("refused %q, want unregistered_scope", reason)
	}
	w.seat("bnd-seat", storeRecipientSeatTask)
	got, err := c.Resolve(w.ctx, "initiative:"+storeRecipientInitiative)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sender != storeRecipientSupervisor || got.Recipient != storeRecipientSeatTask || got.RecipientScopeKind != "store" || got.InitiativeKey != storeRecipientInitiative {
		t.Fatalf("resolved %+v, want the store seat", got)
	}
	if _, err := c.Resolve(w.ctx, "initiative:INI-NONE"); err == nil {
		t.Fatal("an initiative with no owner resolved")
	} else if reason, _ := w.refusal(err); reason != "unregistered_scope" {
		t.Fatalf("refused %q, want unregistered_scope", reason)
	}
}

// TestDirectResolveRunReachesItsOwner: a run's message goes to the thread that owns the run.
func TestDirectResolveRunReachesItsOwner(t *testing.T) {
	w := newStoreRecipientWorld(t)
	if _, err := w.s.DB.ExecContext(w.ctx, "INSERT INTO flow_runs (run_id, workflow, script_digest, helper_digests_json, args_digest, plan_id, plan_revision, actor_task_id, epoch_nonce, resume_from, started_at)"+
		" VALUES ('run-1', 'wf', 'sha256:a', '{}', 'sha256:b', 'plan-1', 1, '01orchestrator', 'nonce', '', 't')"); err != nil {
		t.Fatal(err)
	}
	c := w.channel()
	got, err := c.Resolve(w.ctx, "run:run-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Sender != "run-1" || got.Recipient != "01orchestrator" || got.RecipientScopeKind != "run" {
		t.Fatalf("resolved %+v, want run-1 -> 01orchestrator", got)
	}
	if _, err := c.Resolve(w.ctx, "run:run-none"); err == nil {
		t.Fatal("an unknown run resolved")
	} else if reason, _ := w.refusal(err); reason != "unregistered_scope" {
		t.Fatalf("refused %q, want unregistered_scope", reason)
	}
}

// TestDirectStageWritesOneMessagePerReport: a lead's decision request is staged as one eventless
// message of the new kind, addressed to the management seat, with a deterministic id; staging it
// again stages nothing more.
func TestDirectStageWritesOneMessagePerReport(t *testing.T) {
	w := newStoreRecipientWorld(t)
	w.superviseInitiative()
	w.manage(directManagement, directManagement, storeRecipientInitiative, storeRecipientSupervisor)
	c := w.channel()
	message := DirectMessage{Kind: KindLeadDecisionRequest, Relationship: "initiative:" + storeRecipientInitiative, Subject: "lrp_1",
		Summary: "release-gate exception for P-CRW-140", Text: "the gate refuses a docs-only change; approve the exception?", Evidence: []string{"linear:CRW-1"}, At: "2026-10-14T00:00:00.000000+00:00"}
	answer, err := c.StageDirect(w.ctx, message, nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer["staged"] != true {
		t.Fatalf("first staging answered %v", answer)
	}
	id, _ := answer["messageId"].(string)
	if id != DirectMessageID(message) || len(id) != 32 {
		t.Fatalf("message id %q, want the deterministic %q", id, DirectMessageID(message))
	}
	row, err := w.s.SupervisorMessage(w.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.ObligationKind != KindLeadDecisionRequest || row.ObligationID != "lrp_1" || row.RelationshipID != "initiative:"+storeRecipientInitiative ||
		row.SenderTaskID != storeRecipientSupervisor || row.RecipientTaskID != directManagement || row.State != "queued" || row.EventID.Valid || row.ProjectKey.Valid {
		t.Fatalf("staged row %+v", row)
	}
	var packet map[string]any
	if err := json.Unmarshal([]byte(row.Packet), &packet); err != nil {
		t.Fatal(err)
	}
	env, _ := packet["envelope"].(map[string]any)
	sender, _ := env["sender"].(map[string]any)
	recipient, _ := env["recipient"].(map[string]any)
	if env["direction"] != "lead_to_management" || env["layer"] != "management" || env["kind"] != "decision" || env["purpose"] != KindLeadDecisionRequest ||
		sender["role"] != "lead" || recipient["role"] != "management" || recipient["scopeKind"] != "management" || packet["body"] != message.Text {
		t.Fatalf("packet %v", packet)
	}
	again, err := c.StageDirect(w.ctx, DirectMessage{Kind: message.Kind, Relationship: message.Relationship, Subject: message.Subject,
		Summary: message.Summary, Text: message.Text, Evidence: message.Evidence, At: "2026-10-14T00:05:00.000000+00:00"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again["staged"] != false || again["messageId"] != id {
		t.Fatalf("second staging answered %v", again)
	}
	var count int
	if err := w.s.DB.QueryRowContext(w.ctx, "SELECT COUNT(*) FROM supervisor_messages").Scan(&count); err != nil || count != 1 {
		t.Fatalf("%d supervisor messages (%v), want one", count, err)
	}

	// The rendered message names the layer and the sender in its header, and carries the text.
	rendered := c.render(Packet(packet), "req-1", "tok-1")
	if !strings.HasPrefix(rendered, "[codex-session-relay] management message from lead "+storeRecipientSupervisor+"\n") ||
		!strings.Contains(rendered, message.Text) || !strings.Contains(rendered, "an answer is owed by the recipient") {
		t.Fatalf("rendered:\n%s", rendered)
	}
}

// TestDirectStageRefusesKindsItDoesNotStage: the event kinds blocked and decision_request are read
// back through the event path (newestRaising), so a direct message never takes them, and a kind is
// staged only on the relationship its sender reports through.
func TestDirectStageRefusesKindsItDoesNotStage(t *testing.T) {
	w := newStoreRecipientWorld(t)
	w.superviseInitiative()
	w.manage(directManagement, directManagement, storeRecipientInitiative, storeRecipientSupervisor)
	c := w.channel()
	for _, m := range []DirectMessage{
		{Kind: "blocked", Relationship: "initiative:" + storeRecipientInitiative, Subject: "s", Text: "x"},
		{Kind: "decision_request", Relationship: "initiative:" + storeRecipientInitiative, Subject: "s", Summary: "d", Text: "x"},
		{Kind: KindLeadReport, Relationship: "project:" + storeRecipientProject, Subject: "s", Text: "x"},
		{Kind: KindLeadReport, Relationship: "initiative:" + storeRecipientInitiative, Subject: "", Text: "x"},
		{Kind: KindLeadDecisionRequest, Relationship: "initiative:" + storeRecipientInitiative, Subject: "s", Text: "x"},
	} {
		if _, err := c.StageDirect(w.ctx, m, nil); err == nil {
			t.Fatalf("%+v was staged", m)
		} else if reason, _ := w.refusal(err); reason != "malformed_receipt" {
			t.Fatalf("%+v refused %q, want malformed_receipt", m, reason)
		}
	}
	for _, kind := range DirectKinds() {
		if kind == "blocked" || kind == "decision_request" {
			t.Fatalf("the direct kinds reuse the event kind %q", kind)
		}
	}
}

// TestDirectStageReaddressesAnUnsentMessage: a message staged to the store seat before the
// management edge existed goes to the management seat when it is staged again, since nothing of it
// was sent.
func TestDirectStageReaddressesAnUnsentMessage(t *testing.T) {
	w := newStoreRecipientWorld(t)
	w.superviseInitiative()
	w.seat("bnd-seat", storeRecipientSeatTask)
	c := w.channel()
	message := DirectMessage{Kind: KindLeadReport, Relationship: "initiative:" + storeRecipientInitiative, Subject: "lrp_2", Summary: "scope done", Text: "the delivery scope is complete"}
	first, err := c.StageDirect(w.ctx, message, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first["recipient"] != storeRecipientSeatTask {
		t.Fatalf("first staging went to %v, want the store seat", first["recipient"])
	}
	// A management session registers the lead: the edge from its seat to the initiative stands.
	w.manage(directManagement, directManagement, storeRecipientInitiative, storeRecipientSupervisor)
	second, err := c.StageDirect(w.ctx, message, nil)
	if err != nil {
		t.Fatal(err)
	}
	row, err := w.s.SupervisorMessage(w.ctx, first["messageId"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if second["readdressed"] != true || row.RecipientTaskID != directManagement {
		t.Fatalf("second staging %v left recipient %s", second, row.RecipientTaskID)
	}
	var packet map[string]any
	if err := json.Unmarshal([]byte(row.Packet), &packet); err != nil {
		t.Fatal(err)
	}
	if recipient, _ := packet["envelope"].(map[string]any)["recipient"].(map[string]any); recipient["scopeKind"] != "management" {
		t.Fatalf("the re-addressed packet still names %v", recipient)
	}
}
