package dagsched

import (
	"context"
	"testing"
)

// CRW-1212 (R04), verification fix: the relationship side of executionOf and releaseOf reads the relationship the node stands on. A release is an open intent only until its child is bound; from then on
// the execution rows and the relationship decide, so a correction is read under its own manifest and generation, and a relationship that ended is not a current execution.

// A relationship correction opens the next generation under a new manifest: the current execution is that generation and that manifest, and the release of the new manifest is the correction row. The first
// manifest is the release of an earlier generation, not the current one (the agent side reads the same way).
func TestExecutionOfFollowsTheRelationshipCorrection(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	rid := k.correctionKit()
	ctx := context.Background()
	var first string
	if err := k.s.DB.QueryRow("SELECT manifest_digest FROM dag_node_executions WHERE relationship_id = ?", rid).Scan(&first); err != nil {
		t.Fatal(err)
	}
	prepared := k.prepare()
	k.openCorrection(rid, []map[string]any{{"id": "c1", "verdict": "needs_changes", "note": prepared.Instruction, "restoration": true}})
	correction, err := k.correct("")
	if err != nil {
		t.Fatal(err)
	}
	if correction.ManifestDigest == first || correction.Generation != 2 {
		t.Fatalf("the correction = %+v (first manifest %s)", correction, first)
	}
	cur, found, err := executionOf(ctx, k.s.Q(ctx), "rp", "A")
	if err != nil || !found || cur.Kind != ExecutionRelationship || cur.RelationshipID != rid || cur.ManifestDigest != correction.ManifestDigest || cur.Generation != 2 {
		t.Fatalf("executionOf after the correction = %+v found=%v err=%v, want generation 2 under %s", cur, found, err, correction.ManifestDigest)
	}
	rel, found, err := releaseOf(ctx, k.s.Q(ctx), "rp", "A", correction.ManifestDigest)
	if err != nil || !found || rel.Kind != ExecutionRelationship || rel.ManifestDigest != correction.ManifestDigest || rel.Generation != 2 || rel.RelationshipID != rid {
		t.Fatalf("releaseOf the correction manifest = %+v found=%v err=%v", rel, found, err)
	}
	if old, found, err := releaseOf(ctx, k.s.Q(ctx), "rp", "A", first); err != nil || found {
		t.Fatalf("the first manifest is the release of an earlier generation: %+v found=%v err=%v", old, found, err)
	}
}

// A bound release is history: once the relationship has ended (cancelled, or replaced by the registry) the node has no current execution on the relationship side, the agent surface may be given the node,
// and a relationship that is live (active, paused) is still current. An open intent that has no child yet is current whatever the history under it.
func TestEndedRelationshipIsNotACurrentExecution(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	ctx := context.Background()
	q := k.s.Q(ctx)
	live := func(node string) bool {
		t.Helper()
		_, found, err := executionOf(ctx, q, "rp", node)
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	rel := k.mustRelease("rp", "I")
	if !live("I") {
		t.Fatal("an active relationship is a current execution")
	}
	k.exec("UPDATE relationships SET status = 'paused' WHERE relationship_id = ?", rel.RelationshipID)
	if !live("I") {
		t.Fatal("a paused relationship is a current execution")
	}
	k.exec("UPDATE relationships SET status = 'cancelled' WHERE relationship_id = ?", rel.RelationshipID)
	if live("I") {
		t.Fatal("a cancelled relationship is a current execution")
	}
	if _, found, err := releaseOf(ctx, q, "rp", "I", rel.ManifestDigest); err != nil || found {
		t.Fatalf("the release of a cancelled relationship is current: found=%v err=%v", found, err)
	}
	// the node is free for the other surface as far as this exclusion goes: it no longer refuses live_relationship_execution (the scheduler's own judgement of the node, which still names it owned, is another check)
	if err := refuseLiveRelationshipExecution(ctx, q, "rp", "I"); err != nil {
		t.Fatalf("an ended relationship refuses the agent release: %v", err)
	}
	// a relationship the registry replaced with another is not current either
	other := k.mustRelease("rp", "A")
	if !live("A") {
		t.Fatal("A is current after its release")
	}
	if err := refuseLiveRelationshipExecution(ctx, q, "rp", "A"); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("a live relationship does not refuse the agent release: %v", err)
	}
	k.foreignRelationship("CRW-A")
	k.exec("UPDATE relationships SET superseded_by = 'rel-foreign-CRW-A' WHERE relationship_id = ?", other.RelationshipID)
	if live("A") {
		t.Fatal("a superseded relationship is a current execution")
	}
	if err := refuseLiveRelationshipExecution(ctx, q, "rp", "A"); err != nil {
		t.Fatalf("a superseded relationship refuses the agent release: %v", err)
	}
}

// Fix round 2: an accepted relationship that the registry replaced (superseded_by set and status archived, as registry.go writes a replacement) is not the node's current execution: its result is
// nobody's to accept, so the acceptance no longer holds the node for it. And an acceptance holds only the relationship it was made for: a later relationship of the node that ended unaccepted is not
// made current by another relationship's acceptance.
func TestAcceptedThenSupersededRelationshipIsNotACurrentExecution(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	ctx := context.Background()
	q := k.s.Q(ctx)
	a := k.acceptNode("rp", "A", acceptOpts{})
	rid := a.Acceptance.RelationshipID
	k.exec("UPDATE relationships SET status = 'archived' WHERE relationship_id = ?", rid)
	// accepted and archived: the accepted result still holds the node
	if cur, found, err := executionOf(ctx, q, "rp", "A"); err != nil || !found || cur.RelationshipID != rid {
		t.Fatalf("an accepted archived relationship: %+v found=%v err=%v", cur, found, err)
	}
	if err := refuseLiveRelationshipExecution(ctx, q, "rp", "A"); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("an accepted relationship does not refuse the agent release: %v", err)
	}
	// replaced by the registry after the acceptance
	k.foreignRelationship("CRW-A")
	k.exec("UPDATE relationships SET superseded_by = 'rel-foreign-CRW-A', status = 'archived' WHERE relationship_id = ?", rid)
	if cur, found, err := executionOf(ctx, q, "rp", "A"); err != nil || found {
		t.Fatalf("an accepted relationship that was replaced is current: %+v found=%v err=%v", cur, found, err)
	}
	if rel, found, err := releaseOf(ctx, q, "rp", "A", a.Manifest); err != nil || found {
		t.Fatalf("the release of a replaced relationship is current: %+v found=%v err=%v", rel, found, err)
	}
	if err := refuseLiveRelationshipExecution(ctx, q, "rp", "A"); err != nil {
		t.Fatalf("a replaced relationship refuses the agent release: %v", err)
	}

	// the acceptance of an earlier relationship does not make a later, unaccepted and ended one current
	k2 := newReleaseKit(t)
	releasePlan(k2.fixture, "rp")
	q2 := k2.s.Q(ctx)
	first := k2.acceptNode("rp", "A", acceptOpts{})
	k2.exec("UPDATE relationships SET status = 'archived' WHERE relationship_id = ?", first.Acceptance.RelationshipID)
	later := k2.reportNode("rp", "A", acceptOpts{Suffix: "-2", Status: "archived"})
	if later.Acceptance.RelationshipID == first.Acceptance.RelationshipID {
		t.Fatal("the fixture made one relationship")
	}
	if rel, found, err := currentRelationshipOf(ctx, q2, "rp", "A"); err != nil || !found || rel.ID != later.Acceptance.RelationshipID {
		t.Fatalf("the node stands on %+v (%v %v), want the later relationship", rel, found, err)
	}
	if cur, found, err := executionOf(ctx, q2, "rp", "A"); err != nil || found {
		t.Fatalf("another relationship's acceptance makes an ended relationship current: %+v found=%v err=%v", cur, found, err)
	}
}
