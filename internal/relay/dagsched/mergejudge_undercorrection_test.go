package dagsched

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// CRW-906 generation 2: the merge gate of an accepted result that is being corrected. The issue's
// decision 2 promises that a node under correction is not merged on its old head. The bundle open and
// the single lane refuse a pull request head that differs from the accepted head, and that guard holds
// only once the child has pushed: between opening (or recording) the correction generation and that
// first push the pull request still shows the accepted head H, so the single-lane judgement must refuse
// by the generation alone. These tests are the single-lane half of that promise, on temporary stores.

// ucRelationship is the relationship of the kit's accepted node I.
func ucRelationship(t *testing.T, k *judgeKit) string {
	t.Helper()
	var rid string
	if err := k.s.DB.QueryRow("SELECT relationship_id FROM dag_acceptances WHERE plan_id = 'g' AND node_id = 'I' AND state = 'active'").Scan(&rid); err != nil {
		t.Fatal(err)
	}
	return rid
}

// ucOpenCorrection opens a correction generation of the relationship by hand, the shape generation-open
// leaves: the generations row and the relationship's live generation moved to it.
func ucOpenCorrection(t *testing.T, k *judgeKit, rid string) {
	t.Helper()
	if _, err := (&registry.Registry{Store: k.s}).OpenGeneration(context.Background(), rid, "correction-"+rid, "needs_changes_revision", sql.NullString{}); err != nil {
		t.Fatalf("generation-open: %v", err)
	}
}

// ucJudge is one judgement of the kit's accepted node, with the refusal it is expected to answer.
func ucJudge(t *testing.T, k *judgeKit) (JudgeResult, error) {
	t.Helper()
	return k.sched.Judge(context.Background(), "g", "I", "parent", JudgeInput{})
}

// ucPointer is the generation the relationship stands on now.
func ucPointer(t *testing.T, k *judgeKit, rid string) int64 {
	t.Helper()
	var generation int64
	if err := k.s.DB.QueryRow("SELECT execution_generation FROM relationships WHERE relationship_id = ?", rid).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	return generation
}

// Criterion c1 (generation 2): while the relationship's live generation is later than the generation its
// active acceptance stands on, the accepted head is not judged at all: the judgement is refused
// disposition_conflict, names the open generation, and writes no row. A generation that is recorded as
// the node's execution (dag-correct --manifest-digest) is the same state to this gate.
func TestTheSingleLaneRefusesAnAcceptedResultUnderCorrection(t *testing.T) {
	t.Parallel()
	for _, recorded := range []bool{false, true} {
		name := "a correction generation that is open and bound"
		if recorded {
			name = "a correction generation recorded as the node's execution"
		}
		t.Run(name, func(t *testing.T) {
			k := newJudgeKit(t)
			rid := ucRelationship(t, k)
			ucOpenCorrection(t, k, rid)
			if recorded {
				// what dag-correct --manifest-digest leaves: the execution row of the new generation
				k.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id)"+
					" VALUES ('g','I',?,2,?,'correction',?)", rid, dig("the corrected manifest"), "request-"+rid)
			}
			res, err := ucJudge(t, k)
			if err == nil {
				t.Fatalf("the judgement of an accepted result under correction was accepted: %+v", res)
			}
			if got := refusalReason(err); got != "disposition_conflict" {
				t.Fatalf("reason = %s, want disposition_conflict (%v)", got, err)
			}
			for _, want := range []string{"under correction", "generation 2", "generation 1"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the refusal does not name %q: %v", want, err)
				}
			}
			if n := k.count("SELECT COUNT(*) FROM dag_merge_checks"); n != 0 {
				t.Fatalf("the refused judgement wrote %d row(s)", n)
			}
			if res.Eligible() {
				t.Fatal("the refused judgement reads eligible")
			}
		})
	}
}

// Criterion c1 (generation 2): the gate is about the correction being open, not about the head. A
// withdrawn generation puts the relationship back on the generation its acceptance stands on, and the
// old head is judged again exactly as before.
func TestTheSingleLaneJudgesAgainAfterTheCorrectionIsWithdrawn(t *testing.T) {
	t.Parallel()
	k := newJudgeKit(t)
	rid := ucRelationship(t, k)
	ucOpenCorrection(t, k, rid)
	if _, err := ucJudge(t, k); err == nil {
		t.Fatal("the judgement of an accepted result under correction was accepted")
	}
	if _, err := k.sched.WithdrawGeneration(context.Background(), "g", "I", "parent",
		WithdrawInput{Relationship: rid, Generation: 2, Reason: "the correction is not needed"}); err != nil {
		t.Fatalf("dag-generation-withdraw: %v", err)
	}
	if got := ucPointer(t, k, rid); got != 1 {
		t.Fatalf("the relationship stands on generation %d after the withdrawal, want 1", got)
	}
	res, err := ucJudge(t, k)
	if err != nil {
		t.Fatalf("the judgement after the withdrawal: %v", err)
	}
	if !res.Eligible() {
		t.Fatalf("the judgement after the withdrawal = %+v, want eligible", res)
	}
}

// Criterion c1 (generation 2): the accepted result is accepted over (the corrected head of the new
// generation replaces the acceptance), and the node is a candidate again. The gate reads the stand
// generation the acceptance moved to, so nothing about the correction is left behind.
func TestTheSingleLaneJudgesAgainAfterTheCorrectionIsAcceptedOver(t *testing.T) {
	t.Parallel()
	k := newJudgeKit(t)
	rid := ucRelationship(t, k)
	ucOpenCorrection(t, k, rid)
	// dag-accept --supersedes of the corrected result: the acceptance moves to generation 2, the same
	// generation the relationship now stands on, so nothing is under correction any more
	k.exec("UPDATE dag_acceptances SET execution_generation = 2 WHERE relationship_id = ? AND state = 'active'", rid)
	res, err := ucJudge(t, k)
	if err != nil {
		t.Fatalf("the judgement of the corrected result: %v", err)
	}
	if !res.Eligible() {
		t.Fatalf("the judgement of the corrected result = %+v, want eligible", res)
	}
}

// Criterion c1 (generation 2): the single lane's other half, dag-merge-request, refuses too. It makes
// its own judgement, so no turn is requested and the lane never carries the head being repaired.
func TestRequestMergeTurnRefusesAnAcceptedResultUnderCorrection(t *testing.T) {
	t.Parallel()
	k := newJudgeKit(t)
	rid := ucRelationship(t, k)
	ucOpenCorrection(t, k, rid)
	_, turn, err := k.sched.RequestMergeTurn(context.Background(), "g", "I", "parent", MergeRequestInput{Host: "host-a"})
	if err == nil {
		t.Fatalf("a merge turn was requested for an accepted result under correction: %v", turn)
	}
	if got := refusalReason(err); got != "disposition_conflict" {
		t.Fatalf("reason = %s, want disposition_conflict (%v)", got, err)
	}
	if !strings.Contains(err.Error(), "under correction") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
	if n := k.count("SELECT COUNT(*) FROM merge_turns"); n != 0 {
		t.Fatalf("the refused request wrote %d merge turn(s)", n)
	}
}
