package dag

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The reach of the fence into the early form of dag-plan-put (Preflight). Preflight refuses what it can refuse before the store is opened for writing; a session that was replaced hears
// stale_coordinator_epoch from it, as it does from Put, and not the plan's own refusal of a request it had no right to send.

func hasReason(err error, reason string) bool {
	var refused *store.RefusedError
	return errors.As(err, &refused) && refused.Reason == reason
}

// claimedTwice is a plan whose epoch 1 was raised past by a newer session of the same task, which has committed the plan's second revision.
func claimedTwice(t *testing.T) (r *Repo, path string) {
	t.Helper()
	r, s, path := newRepo(t)
	parentBinding(t, s, "b1", "t1", "P-TEST", "active")
	claimRow(t, s, "plan", 1, "b1", "t1", "n1")
	if _, err := r.Put(context.Background(), decode(t, epochDoc("plan", "r1", 0, "t1", 1, "P-TEST", addNode("a", NodeNonPR)))); err != nil {
		t.Fatal(err)
	}
	claimRow(t, s, "plan", 2, "b1", "t1", "n2")
	if _, err := r.Put(context.Background(), decode(t, epochDoc("plan", "r2", 1, "t1", 2, "P-TEST", addNode("b", NodeNonPR)))); err != nil {
		t.Fatal(err)
	}
	return r, path
}

func TestPreflightFencesBeforeItJudgesThePlan(t *testing.T) {
	ctx := context.Background()
	_, path := claimedTwice(t)
	for _, c := range []struct {
		name string
		rev  doc
	}{
		{"a parent revision the plan has moved past", epochDoc("plan", "r3", 1, "t1", 1, "P-TEST", addNode("c", NodeNonPR))},
		{"a revision the plan's rules reject", epochDoc("plan", "r4", 2, "t1", 1, "P-TEST", addEdge("e1", "a", "ghost", EdgeArtifactVerified))},
		{"a request that names no epoch", epochDoc("plan", "r5", 2, "t1", 0, "P-TEST", addNode("c", NodeNonPR))},
		{"a request from another task", epochDoc("plan", "r6", 2, "t2", 2, "P-TEST", addNode("c", NodeNonPR))},
	} {
		if err := Preflight(ctx, path, decode(t, c.rev)); !isStale(err) {
			t.Errorf("%s: Preflight = %v, want stale_coordinator_epoch", c.name, err)
		}
	}
	// the holder of the epoch is judged by the plan, as before
	if err := Preflight(ctx, path, decode(t, epochDoc("plan", "r3", 1, "t1", 2, "P-TEST", addNode("c", NodeNonPR)))); !hasReason(err, "plan_revision_conflict") {
		t.Errorf("the holder with an outdated parent = %v, want plan_revision_conflict", err)
	}
	rejected(t, Preflight(ctx, path, decode(t, epochDoc("plan", "r4", 2, "t1", 2, "P-TEST", addEdge("e1", "a", "ghost", EdgeArtifactVerified)))))
	if err := Preflight(ctx, path, decode(t, epochDoc("plan", "r7", 2, "t1", 2, "P-TEST", addNode("c", NodeNonPR)))); err != nil {
		t.Errorf("the holder with a valid revision = %v", err)
	}
}

// A plan that has a claim and no revision yet: the first revision of a session that was replaced is refused by the fence in the early form too.
func TestPreflightFencesAFirstRevision(t *testing.T) {
	ctx := context.Background()
	_, s, path := newRepo(t)
	parentBinding(t, s, "b1", "t1", "P-TEST", "active")
	claimRow(t, s, "plan", 1, "b1", "t1", "n1")
	claimRow(t, s, "plan", 2, "b1", "t1", "n2")
	if err := Preflight(ctx, path, decode(t, epochDoc("plan", "r1", 0, "t1", 1, "P-TEST", addEdge("e1", "a", "ghost", EdgeArtifactVerified)))); !isStale(err) {
		t.Errorf("an invalid first revision of a replaced session = %v, want stale_coordinator_epoch", err)
	}
	if err := Preflight(ctx, path, decode(t, epochDoc("plan", "r1", 0, "t1", 1, "P-TEST", addNode("a", NodeNonPR)))); !isStale(err) {
		t.Errorf("a valid first revision of a replaced session = %v, want stale_coordinator_epoch", err)
	}
	rejected(t, Preflight(ctx, path, decode(t, epochDoc("plan", "r1", 0, "t1", 2, "P-TEST", addEdge("e1", "a", "ghost", EdgeArtifactVerified)))))
	if err := Preflight(ctx, path, decode(t, epochDoc("plan", "r1", 0, "t1", 2, "P-TEST", addNode("a", NodeNonPR)))); err != nil {
		t.Errorf("the holder's first revision = %v", err)
	}
}

// With no store there is no claim: a revision that names an epoch is refused as stale, and creates nothing; one that names none is judged by the plan rules as ever.
func TestPreflightWithoutAStoreRefusesAnEpochNobodyClaimed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	if err := Preflight(ctx, path, decode(t, epochDoc("plan", "r1", 0, "t1", 1, "P-TEST", addNode("a", NodeNonPR)))); !isStale(err) {
		t.Errorf("a revision that names an epoch, with no store = %v, want stale_coordinator_epoch", err)
	}
	if err := Preflight(ctx, path, decode(t, epochDoc("plan", "r1", 0, "t1", 0, "P-TEST", addNode("a", NodeNonPR)))); err != nil {
		t.Errorf("a revision that names no epoch, with no store = %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("Preflight created the store (%v)", err)
	}
}

// A plan that has a claim and no revision belongs to the project the claim was made under: a first revision under another project is refused whatever other parent bindings the claiming
// task holds, because the claim's own binding is the one the fence reads.
func TestAClaimedPlanWithoutARevisionDoesNotChangeProjects(t *testing.T) {
	ctx := context.Background()
	r, s, path := newRepo(t)
	parentBinding(t, s, "b1", "t1", "P-TEST", "active")
	parentBinding(t, s, "b2", "t1", "P-OTHER", "active")
	claimRow(t, s, "plan", 1, "b1", "t1", "n1")
	other := epochDoc("plan", "r1", 0, "t1", 1, "P-OTHER", addNode("a", NodeNonPR))
	before := zoneRows(t, s.DB)
	if _, err := r.Put(ctx, decode(t, other)); !isStale(err) {
		t.Fatalf("a first revision under another project of a parent of both = %v, want stale_coordinator_epoch", err)
	}
	if err := Preflight(ctx, path, decode(t, other)); !isStale(err) {
		t.Fatalf("Preflight of the same = %v, want stale_coordinator_epoch", err)
	}
	if !reflect.DeepEqual(before, zoneRows(t, s.DB)) {
		t.Fatal("the refusal changed the zone")
	}
	if _, err := r.Put(ctx, decode(t, epochDoc("plan", "r1", 0, "t1", 1, "P-TEST", addNode("a", NodeNonPR)))); err != nil {
		t.Fatalf("the first revision under the claim's project = %v", err)
	}
}
