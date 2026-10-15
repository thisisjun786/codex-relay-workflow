package dagsched

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// CRW-1449: the refusals a task gets for acting on a relationship or a project it does not hold name the layer it is not, the 자식
// (relay role id parent), and never the bare role id. The held-by form and the short form of the correction read the same up to
// the held-by tail, so the two steps of a correction still give one text.
func TestRelationshipRefusalNamesLayers(t *testing.T) {
	t.Parallel()
	rel := relRow{ID: "rel-1", Status: "active", ParentTaskID: "p1"}
	heldBy := notParentHeldBy("t1", rel).Error()
	short := notParentOf("t1", rel).Error()
	if want := "scope_role_mismatch: task t1 is not the 자식 that holds relationship rel-1, which is held by p1"; heldBy != want {
		t.Errorf("held-by refusal = %q, want %q", heldBy, want)
	}
	if want := "scope_role_mismatch: task t1 is not the 자식 that holds relationship rel-1"; short != want {
		t.Errorf("short refusal = %q, want %q", short, want)
	}
	if !strings.HasPrefix(heldBy, short+", ") {
		t.Errorf("the held-by refusal %q does not extend the short one %q", heldBy, short)
	}

	f := newFixture(t)
	forkJoinPlan(f, "p1")
	f.projectParent()
	ctx := context.Background()
	project := requireProjectParent(ctx, f.s.Q(ctx), "P-TEST", "intruder").Error()
	if want := "scope_role_mismatch: task intruder is not the registered 자식 of project P-TEST"; project != want {
		t.Errorf("project refusal = %q, want %q", project, want)
	}
	for _, text := range []string{heldBy, short, project} {
		if hits := layername.Find(text); len(hits) != 0 {
			t.Errorf("%q names a bare role id: %v", text, hits)
		}
	}
}

// CRW-1449 (PR #890 crosscheck, CRW-1483): the epoch claim refusal is composed at run time, its tail chosen by whether the
// task holds any live 자식 binding, so the per-literal ci scan never reads the sentence whole. Both forms name the layer.
func TestClaimEpochRefusalNamesLayers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	now := k.clock()
	if err := storeseed.InsertScopeBinding(ctx, k.s, store.ScopeBindingsRow{BindingID: "bind-other", Role: "parent", ScopeKind: "project", ScopeKey: "PRJ-OTHER",
		TaskID: "task-other", HostID: "host", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ actor, want string }{
		{"task-other", "task task-other is not the live 자식 of project P-TEST (the plan's project key), so it cannot claim the coordinator epoch of plan rp; the live 자식 bindings it holds are of project PRJ-OTHER"},
		{"task-nobody", "task task-nobody is not the live 자식 of project P-TEST (the plan's project key), so it cannot claim the coordinator epoch of plan rp; it holds no live 자식 binding"},
	} {
		_, err := k.sched.ClaimEpoch(ctx, "rp", ClaimInput{Actor: c.actor, SessionNonce: "session-" + c.actor})
		var refused *store.RefusedError
		if !errors.As(err, &refused) || refused.Reason != "scope_role_mismatch" {
			t.Fatalf("%s claims the plan: %v, want a scope_role_mismatch refusal", c.actor, err)
		}
		if refused.Detail != c.want {
			t.Errorf("refusal = %q, want %q", refused.Detail, c.want)
		}
		if hits := layername.Find(refused.Detail); len(hits) != 0 {
			t.Errorf("%q names a bare role id: %v", refused.Detail, hits)
		}
	}
}
