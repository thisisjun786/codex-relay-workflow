package dagsched

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// CRW-906: the correction route of a result that was accepted and is still current. A bundle review found
// a blocking defect in an accepted member before it landed; the parent opened the next generation by hand
// and the child fixed it, but nothing could record that generation (the accepted result is not stale, so
// the hand-opened route refused it) and dag-accept refused the unrecorded generation, so the corrected
// head could not be accepted or merged. recordHandOpened now admits that generation when the reason it was
// opened under says a correction (needs_changes_revision, the generation-open default, or
// accepted_result_correction, which names the route) and every check it already had still holds: the
// generation right after the one the acceptance stands on, the manifest prepared for the node's current
// slice and criteria, opened under CorrectionRequestID, bound to a dispatch turn, and the inputs as they
// stand now. The acceptance stays active until dag-accept --supersedes takes the new result, the route is
// noted in the generation's own reason column, and no schema, refusal name, CLI flag or specs.json entry
// is added.

// acOpenByHand is the coordinator's act with the relay's own commands, under a named reason:
// generation-open under the request id dag-correct --prepare printed, then generation-bind to the turn that
// carried the instruction line to the child.
func acOpenByHand(t *testing.T, k *releaseKit, relationship, request, reason string, generation int64, bind bool) {
	t.Helper()
	reg := &registry.Registry{Store: k.s}
	if _, err := reg.OpenGeneration(context.Background(), relationship, request, reason, sql.NullString{}); err != nil {
		t.Fatalf("generation-open --reason %s: %v", reason, err)
	}
	if bind {
		if _, err := reg.BindAnchor(context.Background(), relationship, generation, "turn-dispatch-"+relationship, "dispatch_receipt"); err != nil {
			t.Fatalf("generation-bind: %v", err)
		}
	}
}

// acExecution is the execution row of a node at one generation, as text, or "" when there is none: the
// kind, the manifest it is bound to and the request id it was opened under.
func acExecution(k *releaseKit, node string, generation int64) string {
	k.t.Helper()
	return rvRows(k, "SELECT kind, manifest_digest, COALESCE(managed_request_id, '') FROM dag_node_executions WHERE node_id = ? AND execution_generation = ?", node, generation)
}

// acReason is the reason the generation row carries: what the record notes of the route without a schema
// change.
func acReason(k *releaseKit, relationship string, generation int64) string {
	k.t.Helper()
	var reason sql.NullString
	if err := k.s.DB.QueryRow("SELECT reason FROM generations WHERE relationship_id = ? AND execution_generation = ?", relationship, generation).Scan(&reason); err != nil {
		k.t.Fatal(err)
	}
	return reason.String
}

// acPointer is the generation the relationship stands on.
func acPointer(k *releaseKit, relationship string) int64 {
	k.t.Helper()
	var generation int64
	if err := k.s.DB.QueryRow("SELECT execution_generation FROM relationships WHERE relationship_id = ?", relationship).Scan(&generation); err != nil {
		k.t.Fatal(err)
	}
	return generation
}

// Criterion c1 and the first case of c2: an accepted, current and not landed node whose generation was
// opened by hand for a correction is recorded as a correction (refused before this change), and the result
// the child reports in that generation replaces the acceptance through dag-accept --supersedes. The child
// is the same one, the acceptance stays active until then, and the siblings keep their records.
func TestAnAcceptedCurrentResultIsCorrectedByAHandOpenedGeneration(t *testing.T) {
	t.Parallel()
	k, accepted := rvSettledSharedRoot(t)
	rid := accepted["B"].RelationshipID
	siblings := map[string]string{"R": rvRecords(k, "sr", "R"), "A": rvRecords(k, "sr", "A"), "C": rvRecords(k, "sr", "C")}
	fleet := k.rvFleet()

	// the premise: B was accepted and nothing above it changed, so its result is current and the relay's
	// verdict writer has no review to open a second ruling on its head with
	if n := k.read("sr").node("B"); n.Disposition != DispDone || n.Reason != DoneAccepted {
		t.Fatalf("B = %+v, want it accepted and current", n)
	}
	prepared := k.rvPrepare("sr", "B")
	if prepared.DispatchRequestID != CorrectionRequestID("sr", "B", prepared.ManifestDigest, 2) {
		t.Fatalf("prepare = %+v", prepared)
	}
	acOpenByHand(t, k, rid, prepared.DispatchRequestID, "needs_changes_revision", 2, true)

	res, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", prepared.ManifestDigest)
	if err != nil || res.Replayed || res.Generation != 2 || res.RelationshipID != rid || res.OpenedBy != OpenedByGenerationOpen ||
		res.ManifestDigest != prepared.ManifestDigest || res.DispatchRequestID != prepared.DispatchRequestID || res.DispatchTurnID != "turn-dispatch-"+rid {
		t.Fatalf("dag-correct for the accepted current result = %v %+v", err, res)
	}
	if got := acExecution(k, "B", 2); got != "correction|"+prepared.ManifestDigest+"|"+prepared.DispatchRequestID {
		t.Fatalf("the execution row = %q", got)
	}
	if got := acReason(k, rid, 2); got != "needs_changes_revision" {
		t.Fatalf("the generation's reason = %q, want the one it was opened under", got)
	}
	// the acceptance stays active until the corrected result replaces it
	var active string
	if err := k.s.DB.QueryRow("SELECT acceptance_id FROM dag_acceptances WHERE plan_id = 'sr' AND node_id = 'B' AND state = 'active'").Scan(&active); err != nil || active != accepted["B"].AcceptanceID {
		t.Fatalf("the active acceptance = %q (%v), want %q", active, err, accepted["B"].AcceptanceID)
	}
	// recording again is a replay that says how the generation was opened
	if again, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", prepared.ManifestDigest); err != nil || !again.Replayed || again.OpenedBy != OpenedByGenerationOpen {
		t.Fatalf("replay = %v %+v", err, again)
	}

	// the child reports in the bound generation and the parent accepts the corrected result in place of the first one
	k.rvReportGeneration(rid, "B", 2, releaseCriteriaDigest())
	second, err := k.accept("sr", "B", AcceptInput{Supersedes: accepted["B"].AcceptanceID})
	if err != nil || second.SupersededID != accepted["B"].AcceptanceID || second.Generation != 2 || second.AcceptanceID == accepted["B"].AcceptanceID {
		t.Fatalf("dag-accept --supersedes of the corrected result = %v %+v", err, second)
	}
	if n := k.read("sr").node("B"); n.Disposition != DispDone || n.Reason != DoneAccepted {
		t.Fatalf("B after the correction was accepted = %+v", n)
	}
	// the correction went to the child B already had and touched no other node
	if got := k.count("SELECT COUNT(DISTINCT relationship_id) FROM dag_node_executions WHERE plan_id = 'sr' AND node_id = 'B'"); got != 1 {
		t.Fatalf("the correction made %d relationships for B", got)
	}
	if got := k.rvFleet(); !strings.HasPrefix(got, strings.Split(fleet, " generations=")[0]) {
		t.Fatalf("the correction made a child:\nbefore %s\nafter  %s", fleet, got)
	}
	for node, before := range siblings {
		if now := rvRecords(k, "sr", node); now != before {
			t.Fatalf("the records of the unrelated node %s changed:\nbefore %s\nafter  %s", node, before, now)
		}
	}
}

// Criterion c1: the second reason the route admits names it. A generation opened with
// accepted_result_correction is a generation-open reason like any other, dag-correct records it as a
// correction, and the generation's own reason column is what notes the route (no schema change).
func TestTheReasonThatNamesTheRouteIsOpenableAndRecorded(t *testing.T) {
	t.Parallel()
	k, accepted := rvSettledSharedRoot(t)
	rid := accepted["B"].RelationshipID
	prepared := k.rvPrepare("sr", "B")
	acOpenByHand(t, k, rid, prepared.DispatchRequestID, "accepted_result_correction", 2, true)
	res, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", prepared.ManifestDigest)
	if err != nil || res.Replayed || res.OpenedBy != OpenedByGenerationOpen || res.ManifestDigest != prepared.ManifestDigest {
		t.Fatalf("dag-correct under accepted_result_correction = %v %+v", err, res)
	}
	if got := acReason(k, rid, 2); got != "accepted_result_correction" {
		t.Fatalf("the generation's reason = %q, want accepted_result_correction", got)
	}
	// the reason is one literal in two packages: dagsched declares the constant and the registry's closed
	// list is what generation-open validates against, and the registry cannot import dagsched. This pins the
	// two to each other, so a rename in one place cannot leave the other accepting a value nothing records.
	for _, accepted := range []string{AcceptedResultCorrection, "needs_changes_revision", "initial_assignment"} {
		if _, err := (&registry.Registry{Store: k.s}).OpenGeneration(context.Background(), "rel-none", "request-"+accepted, accepted, sql.NullString{}); refusalReason(err) == "unknown_generation" {
			t.Fatalf("the registry refuses the reason %q that generation-open must accept", accepted)
		}
	}

	// a generation opened for this route and never sent is withdrawn like every other generation opened by
	// hand: the withdrawal's own rule is about generation-open reasons, and this is one
	k2, accepted2 := rvSettledSharedRoot(t)
	rid2 := accepted2["C"].RelationshipID
	acOpenByHand(t, k2, rid2, "correction-"+rid2, "accepted_result_correction", 2, false)
	out, err := k2.sched.WithdrawGeneration(context.Background(), "sr", "C", "parent", WithdrawInput{Relationship: rid2, Generation: 2, Reason: "the correction is not needed"})
	if err != nil || out.Replayed || out.RestoredGeneration != 1 || out.Generation != 2 {
		t.Fatalf("dag-generation-withdraw = %v %+v", err, out)
	}
	if got := acPointer(k2, rid2); got != 1 {
		t.Fatalf("the relationship stands on generation %d after the withdrawal, want 1", got)
	}
}

// The second case of c2: every check the hand-opened route already had still refuses, on the same accepted
// current node, and a refusal writes no execution row.
func TestAHandOpenedGenerationOfAnAcceptedCurrentResultIsStillRefused(t *testing.T) {
	t.Parallel()
	bound := func(k *releaseKit, generation int64) string { return acExecution(k, "B", generation) }
	t.Run("an open reason that states no correction", func(t *testing.T) {
		k, accepted := rvSettledSharedRoot(t)
		rid := accepted["B"].RelationshipID
		prepared := k.rvPrepare("sr", "B")
		acOpenByHand(t, k, rid, prepared.DispatchRequestID, "initial_assignment", 2, true)
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", prepared.ManifestDigest); refusalReason(err) != "disposition_conflict" {
			t.Fatalf("correction = %v, want disposition_conflict", err)
		} else if !strings.Contains(err.Error(), `"initial_assignment"`) {
			// the assignment itself is not a correction of anything: the recording refuses the generation by the reason it was opened under
			t.Fatalf("the refusal does not name the reason the generation was opened under: %v", err)
		} else if got := bound(k, 2); got != "" {
			t.Fatalf("the refused correction bound %q", got)
		}
	})
	t.Run("a node that landed", func(t *testing.T) {
		k, a := rvMerged(t)
		acOpenByHand(t, k.releaseKit, a.Acceptance.RelationshipID, "correction-"+a.Acceptance.RelationshipID, "needs_changes_revision", 2, true)
		if _, err := k.sched.RecordCorrection(context.Background(), "g", "I", "parent", dig("a manifest of the landed node")); refusalReason(err) != "disposition_conflict" {
			t.Fatalf("correction = %v, want disposition_conflict", err)
		} else if !strings.Contains(err.Error(), "landed") {
			t.Fatalf("the refusal does not say why: %v", err)
		}
	})
	t.Run("a generation that is not the one after the acceptance's", func(t *testing.T) {
		k, accepted := rvSettledSharedRoot(t)
		rid := accepted["B"].RelationshipID
		first := k.rvPrepare("sr", "B")
		acOpenByHand(t, k, rid, first.DispatchRequestID, "needs_changes_revision", 2, true)
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", first.ManifestDigest); err != nil {
			t.Fatalf("the first correction: %v", err)
		}
		next := k.rvPrepare("sr", "B")
		if !strings.Contains(next.Instruction, "generation 3") {
			t.Fatalf("the second prepare = %+v", next)
		}
		acOpenByHand(t, k, rid, next.DispatchRequestID, "needs_changes_revision", 3, true)
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", next.ManifestDigest); refusalReason(err) != "disposition_conflict" {
			t.Fatalf("a second correction beside the open one = %v, want disposition_conflict", err)
		} else if !strings.Contains(err.Error(), "already open") {
			t.Fatalf("the refusal does not say why: %v", err)
		} else if got := bound(k, 3); got != "" {
			t.Fatalf("the refused correction bound %q", got)
		}
	})
	t.Run("a manifest prepared for another version of the node", func(t *testing.T) {
		k, accepted := rvSettledSharedRoot(t)
		rid := accepted["B"].RelationshipID
		prepared := k.rvPrepare("sr", "B")
		acOpenByHand(t, k, rid, prepared.DispatchRequestID, "needs_changes_revision", 2, true)
		k.invRevise("sr", "B", "sr-r2", invTitle("B, as the revision specifies it"))
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", prepared.ManifestDigest); refusalReason(err) != "disposition_conflict" {
			t.Fatalf("correction = %v, want disposition_conflict", err)
		} else if !strings.Contains(err.Error(), "open no further generation") {
			t.Fatalf("the refusal does not say what to do: %v", err)
		} else if got := bound(k, 2); got != "" {
			t.Fatalf("the refused correction bound %q", got)
		}
	})
	t.Run("a generation nobody bound", func(t *testing.T) {
		k, accepted := rvSettledSharedRoot(t)
		rid := accepted["B"].RelationshipID
		prepared := k.rvPrepare("sr", "B")
		acOpenByHand(t, k, rid, prepared.DispatchRequestID, "needs_changes_revision", 2, false)
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", prepared.ManifestDigest); refusalReason(err) != "disposition_conflict" {
			t.Fatalf("correction = %v, want disposition_conflict", err)
		} else if !strings.Contains(err.Error(), "generation-bind") {
			t.Fatalf("the refusal does not name the way on: %v", err)
		} else if got := bound(k, 2); got != "" {
			t.Fatalf("the refused correction bound %q", got)
		}
	})
	t.Run("no manifest named", func(t *testing.T) {
		k, accepted := rvSettledSharedRoot(t)
		rid := accepted["B"].RelationshipID
		prepared := k.rvPrepare("sr", "B")
		acOpenByHand(t, k, rid, prepared.DispatchRequestID, "needs_changes_revision", 2, true)
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", ""); refusalReason(err) != "disposition_conflict" {
			t.Fatalf("correction = %v, want disposition_conflict", err)
		} else if !strings.Contains(err.Error(), "--manifest-digest") {
			t.Fatalf("the refusal does not ask for the manifest: %v", err)
		} else if got := bound(k, 2); got != "" {
			t.Fatalf("the refused correction bound %q", got)
		}
	})
}

// The reason a generation was opened under is what states the correction, and the stale route keeps its own
// gate: a stale node whose route says correct is recorded under either reason (the new value is a correction
// reason like the default, not a route of its own), and one whose route is anything else is still refused
// whatever reason it carries. This pins that the change admits one case rather than loosening the gate.
func TestTheCorrectionReasonDoesNotLoosenTheStaleRouteGate(t *testing.T) {
	t.Parallel()
	t.Run("a stale node whose route is correct records under the new reason", func(t *testing.T) {
		k, _, ridC, prepared := rvHandKit(t)
		if got := rvAction(t, k.read("sr"), "C"); got != rvCorrect {
			t.Fatalf("the route of C = %q, want %q: the premise of the stale correction", got, rvCorrect)
		}
		acOpenByHand(t, k, ridC, prepared.DispatchRequestID, "accepted_result_correction", 2, true)
		res, err := k.sched.RecordCorrection(context.Background(), "sr", "C", "parent", prepared.ManifestDigest)
		if err != nil || res.Replayed || res.OpenedBy != OpenedByGenerationOpen || res.ManifestDigest != prepared.ManifestDigest {
			t.Fatalf("a stale correction under the new reason = %v %+v", err, res)
		}
		if got := acReason(k, ridC, 2); got != "accepted_result_correction" {
			t.Fatalf("the generation's reason = %q", got)
		}
	})
	t.Run("a stale node whose route is revalidate is refused whatever reason it carries", func(t *testing.T) {
		k, accepted := rvSettledSharedRoot(t)
		ridA := accepted["A"].RelationshipID
		// only A's criteria change: its route is revalidate, which opens no generation
		k.rvReregister("sr", "A", "sr-r2", ridA, dig("A's new criteria"), nil)
		if got := rvAction(t, k.read("sr"), "A"); got != rvRevalidate {
			t.Fatalf("the route of A = %q, want %q: the premise", got, rvRevalidate)
		}
		prepared := k.rvPrepare("sr", "A")
		acOpenByHand(t, k, ridA, prepared.DispatchRequestID, "accepted_result_correction", 2, true)
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "A", "parent", prepared.ManifestDigest); refusalReason(err) != "disposition_conflict" {
			t.Fatalf("a revalidation recorded as a correction = %v, want disposition_conflict", err)
		} else if !strings.Contains(err.Error(), "revalidate") {
			t.Fatalf("the refusal does not name the route: %v", err)
		} else if got := acExecution(k, "A", 2); got != "" {
			t.Fatalf("the refused correction bound %q", got)
		}
	})
}
