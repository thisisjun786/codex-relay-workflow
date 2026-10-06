package dagsched

import (
	"context"
	"strings"
	"testing"
)

// CRW-826: a plan node whose generation holds two roots and no acceptance cannot be ruled — the
// relay's verdict writer refuses a ruling on an ambiguous head (revision_ambiguous) and opens no
// generation — so the generation the coordinator opens by hand is the only way back to the same
// child. recordHandOpened binds it as the existing kind correction, under the existing manifest,
// dispatch-turn and request-id checks, while a node whose previous generation reads one head keeps
// the answer it had. No kind, state or refusal name is added.

// crw826UnacceptedAmbiguous is a released and reported node that is not accepted whose generation 1
// holds a second root: the shape a child leaves behind when it emits two receipts without naming a
// predecessor. The second root is written the way the intake would have written it.
func crw826UnacceptedAmbiguous(k *releaseKit, plan, node string) accepted {
	k.t.Helper()
	r := k.reportNode(plan, node, acceptOpts{})
	k.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
		" VALUES (?, ?, 1, ?, 'ready_for_review', 'child', ?, 'turn-2', 'completed', '{}', 'final', 'z', 'z')",
		"evt-crw826-"+node, r.Acceptance.RelationshipID, dig("a second root of "+node), "child-"+node)
	return r
}

// Criterion c2: the node reads blocked:ambiguous_head, prepare, generation-open with the request id
// the prepare printed, generation-bind and dag-correct --manifest-digest bind generation 2 as a
// correction (refused today), and one receipt in that generation reads the sole head and is
// accepted.
func TestAnUnacceptedNodeWhosePreviousGenerationIsAmbiguousIsRecoveredByAHandOpenedGeneration(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	r := crw826UnacceptedAmbiguous(k, "rp", "A")

	reading := k.read("rp")
	if n := reading.node("A"); n.Reason != BlockedAmbiguousHead {
		t.Fatalf("A = %+v, want %s: %s", n, BlockedAmbiguousHead, reading.brief())
	}

	prepared := k.rvPrepare("rp", "A")
	if prepared.DispatchRequestID != CorrectionRequestID("rp", "A", prepared.ManifestDigest, 2) {
		t.Fatalf("prepare = %+v", prepared)
	}
	rvOpenByHand(t, k, r.Acceptance.RelationshipID, prepared.DispatchRequestID, true)
	res, err := k.sched.RecordCorrection(context.Background(), "rp", "A", "parent", prepared.ManifestDigest)
	if err != nil || res.Replayed || res.Generation != 2 || res.OpenedBy != OpenedByGenerationOpen || res.ManifestDigest != prepared.ManifestDigest {
		t.Fatalf("dag-correct for the hand-opened generation = %v %+v", err, res)
	}
	var kind, digest string
	if err := k.s.DB.QueryRow("SELECT kind, manifest_digest FROM dag_node_executions WHERE plan_id = 'rp' AND node_id = 'A' AND execution_generation = 2").Scan(&kind, &digest); err != nil ||
		kind != "correction" || digest != prepared.ManifestDigest {
		t.Fatalf("the execution row = %q %q %v", kind, digest, err)
	}

	// the child reports in the bound generation: one receipt reads the sole head, and the parent
	// accepts it
	k.rvReportGeneration(r.Acceptance.RelationshipID, "A", 2, releaseCriteriaDigest())
	second, err := k.accept("rp", "A", AcceptInput{})
	if err != nil || second.AcceptanceID == "" || second.Generation != 2 {
		t.Fatalf("accept of the recovered result = %v %+v", err, second)
	}
	if n := k.read("rp").node("A"); n.Disposition != DispDone {
		t.Fatalf("A after the recovery = %+v, want it accepted", n)
	}
}

// The same steps on a node whose previous generation reads one head are refused: a result that is
// not accepted yet is corrected by a ruling, and the generation opened by hand is not a second
// route for it.
func TestAnUnacceptedNodeWithASingleHeadIsNotRecoveredByAHandOpenedGeneration(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	r := k.reportNode("rp", "A", acceptOpts{})
	if n := k.read("rp").node("A"); n.Reason == BlockedAmbiguousHead {
		t.Fatalf("A reads %s on a single head: %s", BlockedAmbiguousHead, n.Detail)
	}
	prepared := k.rvPrepare("rp", "A")
	rvOpenByHand(t, k, r.Acceptance.RelationshipID, prepared.DispatchRequestID, true)
	if _, err := k.sched.RecordCorrection(context.Background(), "rp", "A", "parent", prepared.ManifestDigest); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("a single-head unaccepted node = %v, want disposition_conflict", err)
	} else if !strings.Contains(err.Error(), "corrected by a ruling") {
		t.Fatalf("the refusal does not name the ruling route: %v", err)
	}
	if n := k.count("SELECT COUNT(*) FROM dag_node_executions WHERE plan_id = 'rp' AND node_id = 'A' AND execution_generation = 2"); n != 0 {
		t.Fatalf("the refused correction bound %d executions", n)
	}
}

// A generation opened by hand is bound to the manifest it was opened for: without --manifest-digest
// nothing is bound, whether the previous generation is ambiguous or not.
func TestAHandOpenedGenerationForAnAmbiguousNodeStillNeedsItsManifest(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	r := crw826UnacceptedAmbiguous(k, "rp", "A")
	prepared := k.rvPrepare("rp", "A")
	rvOpenByHand(t, k, r.Acceptance.RelationshipID, prepared.DispatchRequestID, true)
	if _, err := k.sched.RecordCorrection(context.Background(), "rp", "A", "parent", ""); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("no manifest named = %v, want disposition_conflict", err)
	} else if !strings.Contains(err.Error(), "--manifest-digest") {
		t.Fatalf("the refusal does not ask for the manifest: %v", err)
	}
	if n := k.count("SELECT COUNT(*) FROM dag_node_executions WHERE plan_id = 'rp' AND node_id = 'A' AND execution_generation = 2"); n != 0 {
		t.Fatalf("the refused correction bound %d executions", n)
	}
}
