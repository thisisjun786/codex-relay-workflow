package supervisor

import (
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// The management level directly above a project (CRW-1314 lane D1c, design §2): on the 1.0 path a
// management session binds the project owner and the execution edge management -> project stands,
// so the project's upward messages - a relationship's report, a 'project:KEY' notice and the direct
// project kinds - go to the owner of that management seat. The order is the linked management seat,
// then a legacy initiative supervisor, then the single store seat; an ambiguous chain is refused.

// manageProject puts the world's project directly under a management seat: the seat's binding and
// the execution edge management -> project.
func (w *storeRecipientWorld) manageProject(seat, task string) {
	w.t.Helper()
	if err := storeseed.InsertScopeBinding(w.ctx, w.s, store.ScopeBindingsRow{BindingID: "bnd-m-" + seat,
		Role: "supervisor", ScopeKind: "management", ScopeKey: seat, TaskID: task,
		HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		w.t.Fatal(err)
	}
	w.projectEdge(seat, task)
}

// projectEdge writes only the execution edge from a management seat to the world's project.
func (w *storeRecipientWorld) projectEdge(seat, task string) {
	w.t.Helper()
	if err := storeseed.InsertScopeLink(w.ctx, w.s, store.ScopeLinksRow{LinkID: "lnk-m-" + seat + "-project", LinkKind: "execution",
		UpperKind: "management", UpperKey: seat, UpperTaskID: task,
		LowerKind: "project", LowerKey: storeRecipientProject, LowerTaskID: storeRecipientParent,
		Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		w.t.Fatal(err)
	}
}

// TestProjectResolutionReachesTheManagementSeat: a project under a management seat reports to the
// seat's owner, through a relationship and through 'project:KEY', and the store seat does not take
// the report from it.
func TestProjectResolutionReachesTheManagementSeat(t *testing.T) {
	w := newStoreRecipientWorld(t)
	w.seat("bnd-seat", storeRecipientSeatTask)
	w.manageProject(directManagement, directManagement)
	c := w.channel()
	for _, relationship := range []string{w.relationship, "project:" + storeRecipientProject} {
		got, err := c.Resolve(w.ctx, relationship)
		if err != nil {
			t.Fatalf("%s: %v", relationship, err)
		}
		if got.Sender != storeRecipientParent || got.Recipient != directManagement || got.RecipientScopeKind != "management" ||
			got.ProjectKey != storeRecipientProject || got.InitiativeKey != "" {
			t.Fatalf("%s resolved %+v, want %s -> management seat %s", relationship, got, storeRecipientParent, directManagement)
		}
	}
	// The seat's handover moves the recipient with the edge's upper end.
	if _, err := w.s.DB.ExecContext(w.ctx, "UPDATE scope_bindings SET task_id = ? WHERE binding_id = ?", directOtherManagement, "bnd-m-"+directManagement); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.DB.ExecContext(w.ctx, "UPDATE scope_links SET upper_task_id = ? WHERE link_id = ?", directOtherManagement, "lnk-m-"+directManagement+"-project"); err != nil {
		t.Fatal(err)
	}
	got, err := c.Resolve(w.ctx, "project:"+storeRecipientProject)
	if err != nil || got.Recipient != directOtherManagement || got.RecipientScopeKind != "management" {
		t.Fatalf("after the handover the project resolved %+v (%v), want %s", got, err, directOtherManagement)
	}
}

// TestProjectResolutionManagementSeatWithoutOwnerWaits: a management edge whose seat has no live
// owner is refused unregistered_scope instead of falling back to the store seat, because the seat
// that should hear it exists and waits for its handover.
func TestProjectResolutionManagementSeatWithoutOwnerWaits(t *testing.T) {
	w := newStoreRecipientWorld(t)
	w.seat("bnd-seat", storeRecipientSeatTask)
	w.projectEdge(directManagement, directManagement)
	c := w.channel()
	for _, relationship := range []string{w.relationship, "project:" + storeRecipientProject} {
		got, err := c.Resolve(w.ctx, relationship)
		if err == nil {
			t.Fatalf("%s resolved %+v with the seat above it unowned", relationship, got)
		}
		if reason, _ := w.refusal(err); reason != "unregistered_scope" {
			t.Fatalf("%s refused %q (%v), want unregistered_scope", relationship, reason, err)
		}
	}
}

// TestProjectResolutionTwoParentsIsRefused: a project with both a management edge and a legacy
// initiative edge above it is ambiguous, and is refused as competing parents are today.
func TestProjectResolutionTwoParentsIsRefused(t *testing.T) {
	w := newStoreRecipientWorld(t)
	w.superviseInitiative()
	w.manageProject(directManagement, directManagement)
	c := w.channel()
	for _, relationship := range []string{w.relationship, "project:" + storeRecipientProject} {
		if got, err := c.Resolve(w.ctx, relationship); err == nil {
			t.Fatalf("%s resolved %+v with two parents above the project", relationship, got)
		} else if reason, _ := w.refusal(err); reason != "duplicate_scope_owner" {
			t.Fatalf("%s refused %q (%v), want duplicate_scope_owner", relationship, reason, err)
		}
	}
}

// TestProjectResolutionKeepsTheLegacyOrder: with no management edge the legacy initiative
// supervisor hears the project, and with neither the single store seat does.
func TestProjectResolutionKeepsTheLegacyOrder(t *testing.T) {
	lead := newStoreRecipientWorld(t)
	lead.seat("bnd-seat", storeRecipientSeatTask)
	lead.superviseInitiative()
	got, err := lead.channel().Resolve(lead.ctx, "project:"+storeRecipientProject)
	if err != nil || got.Recipient != storeRecipientSupervisor || got.RecipientScopeKind != "initiative" {
		t.Fatalf("legacy initiative: %+v (%v)", got, err)
	}
	seat := newStoreRecipientWorld(t)
	seat.seat("bnd-seat", storeRecipientSeatTask)
	got, err = seat.channel().Resolve(seat.ctx, seat.relationship)
	if err != nil || got.Recipient != storeRecipientSeatTask || got.RecipientScopeKind != "store" {
		t.Fatalf("store seat: %+v (%v)", got, err)
	}
}

// TestDirectStageProjectReachesTheManagementSeat: a project owner's direct message is staged to the
// management seat above the project, its envelope naming the management layer; the same kind under
// a legacy lead names the lead's layer.
func TestDirectStageProjectReachesTheManagementSeat(t *testing.T) {
	envelopeOf := func(t *testing.T, w *storeRecipientWorld, answer map[string]any) map[string]any {
		t.Helper()
		row, err := w.s.SupervisorMessage(w.ctx, answer["messageId"].(string))
		if err != nil {
			t.Fatal(err)
		}
		var packet map[string]any
		if err := json.Unmarshal([]byte(row.Packet), &packet); err != nil {
			t.Fatal(err)
		}
		env, _ := packet["envelope"].(map[string]any)
		return env
	}
	message := DirectMessage{Kind: KindFlowProjectDecisionRequest, Relationship: "project:" + storeRecipientProject, Subject: "rep_1",
		Summary: "ship before the review?", Text: "the milestone is closed; ship it?", At: "2026-10-14T00:00:00.000000+00:00"}

	w := newStoreRecipientWorld(t)
	w.manageProject(directManagement, directManagement)
	answer, err := w.channel().StageDirect(w.ctx, message, nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer["recipient"] != directManagement || answer["sender"] != storeRecipientParent || answer["recipientScopeKind"] != "management" {
		t.Fatalf("staged %v, want %s -> %s", answer, storeRecipientParent, directManagement)
	}
	env := envelopeOf(t, w, answer)
	recipient, _ := env["recipient"].(map[string]any)
	sender, _ := env["sender"].(map[string]any)
	if env["layer"] != "management" || env["direction"] != "orchestrator_to_management" || recipient["role"] != "management" || recipient["scopeKind"] != "management" || sender["role"] != "orchestrator" {
		t.Fatalf("envelope %v", env)
	}

	legacy := newStoreRecipientWorld(t)
	legacy.superviseInitiative()
	answer, err = legacy.channel().StageDirect(legacy.ctx, message, nil)
	if err != nil {
		t.Fatal(err)
	}
	env = envelopeOf(t, legacy, answer)
	recipient, _ = env["recipient"].(map[string]any)
	if answer["recipient"] != storeRecipientSupervisor || env["layer"] != "lead" || recipient["role"] != "lead" || recipient["scopeKind"] != "initiative" {
		t.Fatalf("legacy staging %v envelope %v", answer, env)
	}
}
