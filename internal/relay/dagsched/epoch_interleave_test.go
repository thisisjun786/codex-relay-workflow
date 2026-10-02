package dagsched

import (
	"context"
	"reflect"
	"testing"
)

// A claim that lands between the build of a correction's manifest and its store refuses the store: the fence and PutManifest are one transaction, so nothing is stored for the stale session.
func TestAClaimBetweenTheBuildAndTheStoreOfAManifestRefusesTheStore(t *testing.T) {
	ctx := context.Background()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	stale := k.sched
	k.claim(stale, "rp", "parent", "session-1")
	k.mustRelease("rp", "A")
	manifests := k.count("SELECT COUNT(*) FROM dag_input_manifests")
	before := allRows(t, k.s.DB)
	landed := false
	stale.testBeforeManifestStore = func() {
		landed = true
		k.session("rp", "parent", "session-2")
	}
	snapshot := writeFile(t, k.root, "late.md", "the notes of a late correction")
	_, err := stale.PrepareCorrection(ctx, "rp", "A", "parent", ManifestInput{RuleVersion: k.request(false).RuleVersion,
		Volatile: []Volatile{{Source: "linear:comment", SnapshotURI: snapshot, SHA256: shaOf([]byte("the notes of a late correction")), CapturedAt: "2026-10-02T00:00:00Z"}}}, VerifyOptions{ArtifactRoots: []string{k.root}})
	if !landed {
		t.Fatal("the claim did not land between the build and the store")
	}
	if !isStale(err) {
		t.Fatalf("prepare = %v, want stale_coordinator_epoch", err)
	}
	if got := k.count("SELECT COUNT(*) FROM dag_input_manifests"); got != manifests {
		t.Fatalf("a manifest was stored for the stale session (%d before, %d after)", manifests, got)
	}
	after := allRows(t, k.s.DB)
	delete(after, "dag_coordinator_claims")
	delete(before, "dag_coordinator_claims")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("the refused preparation changed the store beyond the claim that landed")
	}
}

// The activation of a project by a claim cannot be got round with a plan nobody claimed, or by the claim of a plan that has no revision yet, and replacing the parent does not undo it.
func TestCapBasisActivationOfAProject(t *testing.T) {
	ctx := context.Background()
	basis := func(plan string) CapBasis {
		return CapBasis{LimitID: "lim-project-runs", Revision: 1, WMinutes: 30, WSource: "measured", SMinutes: 5, SSource: "measured", DecidedBy: "parent", Plan: plan}
	}
	t.Run("an unclaimed second plan of a claimed project", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.putPlan("rp2", 0, "rp2-r1", addNode("X", "non_pr"))
		k.declareLimit("project", "P-TEST", "runs", 10)
		k.claim(k.sched, "rp", "parent", "session-1")
		other := &Scheduler{Store: k.s, Now: k.clock}
		if err := other.RecordCapBasis(ctx, basis("rp2")); !isStale(err) {
			t.Fatalf("a basis for an unclaimed plan of an activated project = %v", err)
		}
		if k.count("SELECT COUNT(*) FROM dag_cap_basis") != 0 {
			t.Fatal("a refused basis was written")
		}
		if err := k.sched.RecordCapBasis(ctx, basis("rp")); err != nil {
			t.Fatalf("the claimed plan under its epoch: %v", err)
		}
	})
	t.Run("the claim of a plan that has no revision yet", func(t *testing.T) {
		k := newReleaseKit(t)
		k.declareLimit("project", "P-TEST", "runs", 10)
		if _, err := k.sched.ClaimEpoch(ctx, "fresh", ClaimInput{Actor: "parent", SessionNonce: "s1", Project: "P-TEST"}); err != nil {
			t.Fatal(err)
		}
		if err := k.sched.RecordCapBasis(ctx, basis("")); !isStale(err) {
			t.Fatalf("a basis without a plan after a headerless plan was claimed = %v", err)
		}
	})
	t.Run("the parent is replaced", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.declareLimit("project", "P-TEST", "runs", 10)
		k.claim(k.sched, "rp", "parent", "session-1")
		k.replaceParent("parent-2")
		if err := (&Scheduler{Store: k.s, Now: k.clock}).RecordCapBasis(ctx, basis("")); !isStale(err) {
			t.Fatalf("a basis without a plan after the claiming binding was replaced = %v", err)
		}
	})
}
