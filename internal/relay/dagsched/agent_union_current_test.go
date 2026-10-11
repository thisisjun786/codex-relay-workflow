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
