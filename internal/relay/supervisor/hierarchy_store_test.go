package supervisor

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// The store seat (CRW-450) is the one supervisor scope no Linear level owns, so a project whose
// initiative has no supervisor still has somewhere to report. These drive the real walk over a
// real store rather than a hand-built reading, because the seat is a fact about the store.
const (
	storeRecipientRelationship = "rel-store-recipient"
	storeRecipientProject      = "PRJ-1"
	storeRecipientIssue        = "CRW-1"
	storeRecipientParent       = "01parent"
	storeRecipientChild        = "01child"
	storeRecipientInitiative   = "INI-1"
	storeRecipientSupervisor   = "01supervisor"
	storeRecipientSeatTask     = "01management"
	storeRecipientOtherSeat    = "01other-management"
)

// storeRecipientWorld is one store holding a scoped relationship, its project owner and its child.
type storeRecipientWorld struct {
	t            *testing.T
	s            *store.Store
	ctx          context.Context
	relationship string
}

func newStoreRecipientWorld(t *testing.T) *storeRecipientWorld {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	w := &storeRecipientWorld{t: t, s: s, ctx: ctx, relationship: storeRecipientRelationship}
	if _, err = s.DB.ExecContext(ctx, `INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES (?,?,'active',?,?,?,?,1,'[]','[]','t','t')`,
		w.relationship, storeRecipientIssue, storeRecipientParent, "host", storeRecipientChild, "host"); err != nil {
		t.Fatal(err)
	}
	if err = storeseed.RecordRelationshipScope(ctx, s, w.relationship, storeRecipientProject, "t"); err != nil {
		t.Fatal(err)
	}
	for _, b := range []store.ScopeBindingsRow{
		{BindingID: "bnd-child", Role: "child", ScopeKind: "issue", ScopeKey: storeRecipientIssue, TaskID: storeRecipientChild, HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"},
		{BindingID: "bnd-parent", Role: "parent", ScopeKind: "project", ScopeKey: storeRecipientProject, TaskID: storeRecipientParent, HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"},
	} {
		if err = storeseed.InsertScopeBinding(ctx, s, b); err != nil {
			t.Fatal(err)
		}
	}
	if err = storeseed.InsertScopeLink(ctx, s, store.ScopeLinksRow{LinkID: "lnk-issue", LinkKind: "execution",
		UpperKind: "project", UpperKey: storeRecipientProject, UpperTaskID: storeRecipientParent,
		LowerKind: "issue", LowerKey: storeRecipientIssue, LowerTaskID: storeRecipientChild,
		Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		t.Fatal(err)
	}
	return w
}

// superviseInitiative gives the project the initiative supervisor the ordinary case has.
func (w *storeRecipientWorld) superviseInitiative() {
	w.t.Helper()
	if err := storeseed.InsertScopeBinding(w.ctx, w.s, store.ScopeBindingsRow{BindingID: "bnd-supervisor",
		Role: "supervisor", ScopeKind: "initiative", ScopeKey: storeRecipientInitiative, TaskID: storeRecipientSupervisor,
		HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		w.t.Fatal(err)
	}
	if err := storeseed.InsertScopeLink(w.ctx, w.s, store.ScopeLinksRow{LinkID: "lnk-project", LinkKind: "execution",
		UpperKind: "initiative", UpperKey: storeRecipientInitiative, UpperTaskID: storeRecipientSupervisor,
		LowerKind: "project", LowerKey: storeRecipientProject, LowerTaskID: storeRecipientParent,
		Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		w.t.Fatal(err)
	}
}

// seat registers one live store-scope supervisor binding, the seat CRW-450 made bindable.
func (w *storeRecipientWorld) seat(bindingID, task string) {
	w.t.Helper()
	if err := storeseed.InsertScopeBinding(w.ctx, w.s, store.ScopeBindingsRow{BindingID: bindingID,
		Role: "supervisor", ScopeKind: "store", ScopeKey: "store", TaskID: task,
		HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		w.t.Fatal(err)
	}
}

// resolve is the channel's own decision, over the store the world built.
func (w *storeRecipientWorld) resolve() (Resolution, error) {
	w.t.Helper()
	c := &Channel{Store: w.s, Linkage: StoreLinkage{w.s}}
	return c.Resolve(w.ctx, w.relationship)
}

// reading is the hierarchy the decision is made from, before it is turned into a resolution.
func (w *storeRecipientWorld) reading() map[string]any {
	w.t.Helper()
	c := &Channel{Store: w.s, Linkage: StoreLinkage{w.s}}
	reading, err := c.hierarchyReading(w.ctx, w.relationship)
	if err != nil {
		w.t.Fatalf("reading: %v", err)
	}
	return reading
}

// scopeLevels is the scope kinds the reading names, in the order it walks them.
func scopeLevels(reading map[string]any) []string {
	levels, _ := reading["levels"].([]any)
	out := make([]string, 0, len(levels))
	for _, raw := range levels {
		level, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := level["scopeKind"].(string)
		out = append(out, kind)
	}
	return out
}

// refusal is the named refusal a resolution answered with, and its detail.
func (w *storeRecipientWorld) refusal(err error) (string, string) {
	w.t.Helper()
	var refused Refusal
	if !errors.As(err, &refused) {
		w.t.Fatalf("expected a refusal, got %v", err)
	}
	return refused.Reason, refused.Detail
}

// TestStoreRecipient_FallsBackToTheStoreSeat: a project no initiative supervises reports to the
// store-scope supervisor instead of having nowhere to send its report.
func TestStoreRecipient_FallsBackToTheStoreSeat(t *testing.T) {
	t.Parallel()
	w := newStoreRecipientWorld(t)
	w.seat("bnd-store", storeRecipientSeatTask)
	got, err := w.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Recipient != storeRecipientSeatTask {
		t.Fatalf("recipient %q, want the store seat %q", got.Recipient, storeRecipientSeatTask)
	}
	if got.Sender != storeRecipientParent || got.ProjectKey != storeRecipientProject || got.Source != "linkage" {
		t.Fatalf("resolution %+v", got)
	}
	if got.InitiativeKey != "" {
		t.Fatalf("a store-scope recipient is not an initiative: initiativeKey %q", got.InitiativeKey)
	}
	// The substitution is left in the record: the level the recipient came from says its scope
	// kind is the store, not an initiative it does not have.
	if levels := scopeLevels(w.reading()); len(levels) == 0 || levels[len(levels)-1] != "store" {
		t.Fatalf("the store seat is not recorded as the recipient's scope: %v", levels)
	}
}

// TestStoreRecipient_TheInitiativeSupervisorKeepsTheReport: the seat is a fallback, never a
// substitute for a live initiative supervisor.
func TestStoreRecipient_TheInitiativeSupervisorKeepsTheReport(t *testing.T) {
	t.Parallel()
	w := newStoreRecipientWorld(t)
	w.superviseInitiative()
	w.seat("bnd-store", storeRecipientSeatTask)
	got, err := w.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Recipient != storeRecipientSupervisor || got.InitiativeKey != storeRecipientInitiative {
		t.Fatalf("resolution %+v, want the initiative supervisor", got)
	}
	if levels := scopeLevels(w.reading()); len(levels) == 0 || levels[len(levels)-1] != "initiative" {
		t.Fatalf("a supervised project must not gain a store level: %v", levels)
	}
}

// TestStoreRecipient_NoSeatStaysUnregistered: with neither seat the report still has nowhere to
// go, and the obligation stays standing rather than being answered as nothing owed.
func TestStoreRecipient_NoSeatStaysUnregistered(t *testing.T) {
	t.Parallel()
	w := newStoreRecipientWorld(t)
	_, err := w.resolve()
	reason, detail := w.refusal(err)
	if reason != "unregistered_scope" || !strings.Contains(detail, "nobody to report to") {
		t.Fatalf("refusal %q: %s", reason, detail)
	}
	if !strings.Contains(detail, "obligation stays standing") {
		t.Fatalf("the obligation has to stay standing: %s", detail)
	}
}

// TestStoreRecipient_TwoSeatsAreRefusedNotPicked: one store scope, one live supervisor. Two rows
// can only come from a write outside the bind command, and the reader refuses rather than choosing.
func TestStoreRecipient_TwoSeatsAreRefusedNotPicked(t *testing.T) {
	t.Parallel()
	w := newStoreRecipientWorld(t)
	// The uniqueness index (scope_bindings_one_live_owner) is what normally makes a second live
	// seat unwritable; this store drops it, which is the only state the reader can see two in.
	if _, err := w.s.DB.ExecContext(w.ctx, "DROP INDEX scope_bindings_one_live_owner"); err != nil {
		t.Fatal(err)
	}
	w.seat("bnd-store-one", storeRecipientSeatTask)
	w.seat("bnd-store-two", storeRecipientOtherSeat)
	_, err := w.resolve()
	reason, _ := w.refusal(err)
	if reason != "duplicate_scope_owner" {
		t.Fatalf("refusal %q, want duplicate_scope_owner", reason)
	}
}
