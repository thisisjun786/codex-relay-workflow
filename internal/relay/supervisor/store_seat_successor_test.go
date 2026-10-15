package supervisor

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// CRW-1333: a manager holding the store seat that registers its management seat lays the store seat
// down (archived, superseded by the management binding: what linkage-bind --scope-kind management
// writes). The fallbacks that read the store seat keep reaching that manager.

// takeOverStoreSeat moves the store seat bindingID to a management seat of the same task, the way
// the registry's retireStoreSeat leaves it.
func (w *storeRecipientWorld) takeOverStoreSeat(bindingID, task string) {
	w.t.Helper()
	if err := storeseed.ArchiveScopeBinding(w.ctx, w.s, bindingID, "archived", "bnd-m-"+task, "t2"); err != nil {
		w.t.Fatal(err)
	}
	if err := storeseed.InsertScopeBinding(w.ctx, w.s, store.ScopeBindingsRow{BindingID: "bnd-m-" + task,
		Role: "supervisor", ScopeKind: "management", ScopeKey: task, TaskID: task,
		HostID: "host", Status: "active", Revision: 1, CreatedAt: "t2", UpdatedAt: "t2"}); err != nil {
		w.t.Fatal(err)
	}
}

// TestStoreRecipient_FollowsTheManagementSeatThatTookTheSeatOver: a project no initiative
// supervises still reports a decision the user owes to the manager after it registered its
// management seat (CRW-1333), and nothing else (CRW-1486/S4): a notification reads unregistered_scope,
// as it does with no store seat at all.
func TestStoreRecipient_FollowsTheManagementSeatThatTookTheSeatOver(t *testing.T) {
	t.Parallel()
	w := newStoreRecipientWorld(t)
	w.seat("bnd-store", storeRecipientSeatTask)
	w.takeOverStoreSeat("bnd-store", storeRecipientSeatTask)
	got, err := w.resolve()
	if reason, detail := w.refusal(err); reason != "unregistered_scope" || !strings.Contains(detail, "only a decision the user owes") {
		t.Fatalf("a notification resolved %+v, refused %q (%s), want unregistered_scope naming the laid-down seat", got, reason, detail)
	}
	c := w.channel()
	got, err = c.ResolveFor(w.ctx, w.relationship, GradeUser)
	if err != nil {
		t.Fatalf("resolve a user decision: %v", err)
	}
	if got.Recipient != storeRecipientSeatTask || got.RecipientScopeKind != "store" || got.InitiativeKey != "" {
		t.Fatalf("resolution %+v, want the store seat held through the management seat", got)
	}
	reading, err := c.hierarchyReading(w.ctx, w.relationship, GradeUser)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if levels := scopeLevels(reading); len(levels) == 0 || levels[len(levels)-1] != "store" {
		t.Fatalf("the fallback is recorded as the store level: %v", levels)
	}
}

// TestDirectResolveInitiativeFallsBackToTheManagerAfterItRegisters: an initiative a lead registered
// before any management edge existed is heard by the manager that took the store seat over for a
// decision the user owes (jun_decision_request); a lead's report reads unregistered_scope.
func TestDirectResolveInitiativeFallsBackToTheManagerAfterItRegisters(t *testing.T) {
	w := newStoreRecipientWorld(t)
	w.superviseInitiative()
	w.seat("bnd-store", storeRecipientSeatTask)
	w.takeOverStoreSeat("bnd-store", storeRecipientSeatTask)
	c := w.channel()
	got, err := c.Resolve(w.ctx, "initiative:"+storeRecipientInitiative)
	if reason, _ := w.refusal(err); reason != "unregistered_scope" {
		t.Fatalf("a lead's notification resolved %+v, refused %q, want unregistered_scope", got, reason)
	}
	got, err = c.ResolveFor(w.ctx, "initiative:"+storeRecipientInitiative, directGrade(directKinds[KindJunDecisionRequest]))
	if err != nil {
		t.Fatal(err)
	}
	if got.Sender != storeRecipientSupervisor || got.Recipient != storeRecipientSeatTask || got.RecipientScopeKind != "store" {
		t.Fatalf("resolved %+v, want the manager through the store seat it took over", got)
	}
}
