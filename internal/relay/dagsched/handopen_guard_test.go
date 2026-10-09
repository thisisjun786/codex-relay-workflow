package dagsched

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// CRW-1036. A plan revision that changed only the criteria of an accepted node makes its route a re-validation. A generation opened by hand for it cannot be
// recorded by dag-correct or accepted by dag-accept, so generation-open and generation-bind refuse it before the child works on it, the reading says where the
// node goes instead of the command that would be refused, and the registration of the new criteria is named. A non_pr node's merged mark is its integration.

// openUnguarded opens a generation by hand the way a store written before the guard holds one: through the registry's inner write, which the guard does not
// stand in front of.
func openUnguarded(t *testing.T, k *releaseKit, rid, request, reason string) int64 {
	t.Helper()
	var number int64
	reg := &registry.Registry{Store: k.s}
	if err := k.s.Transaction(context.Background(), func(ctx context.Context, _ *sql.Conn) error {
		var err error
		number, err = reg.OpenGenerationIn(ctx, rid, request, reason, sql.NullString{})
		return err
	}); err != nil {
		t.Fatalf("open generation: %v", err)
	}
	return number
}

// openAndBindUnguarded is openUnguarded and the bind of a dispatch turn, written as the store of a coordinator holds them from before generation-open and generation-bind
// asked the route: dag-correct's own refusal of such a generation stays pinned by the tests that use it.
func openAndBindUnguarded(t *testing.T, k *releaseKit, rid, request, reason string) {
	t.Helper()
	number := openUnguarded(t, k, rid, request, reason)
	k.exec("UPDATE generations SET anchor_state = 'bound', dispatch_turn_id = ?, bound_at = ? WHERE relationship_id = ? AND execution_generation = ?", "turn-dispatch-"+rid, k.clock(), rid, number)
}

func withCriteria(digest string) func(n doc) {
	return func(n doc) { n["criteria_set_digest"] = digest }
}

// The accepted node whose criteria alone changed: generation-open is refused whichever correction reason it is given, and nothing is written.
func TestGenerationOpenIsRefusedWhereTheRouteIsARevalidation(t *testing.T) {
	t.Parallel()
	k, accepted := rvSettledSharedRoot(t)
	rid := accepted["A"].RelationshipID
	k.invRevise("sr", "A", "sr-r2", withCriteria(dig("the second edition of A's criteria")))
	if got := rvAction(t, k.read("sr"), "A"); got != rvRevalidate {
		t.Fatalf("the route of A = %q, want %q", got, rvRevalidate)
	}
	generations := k.count("SELECT COUNT(*) FROM generations")
	reg := &registry.Registry{Store: k.s}
	for _, reason := range []string{"accepted_result_correction", "needs_changes_revision"} {
		_, err := reg.OpenGeneration(context.Background(), rid, "by-hand-"+reason, reason, sql.NullString{})
		if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "not a correction by hand") || !strings.Contains(err.Error(), "dag-accept") {
			t.Fatalf("generation-open --reason %s = %v, want the revalidation route to refuse it", reason, err)
		}
	}
	if got := k.count("SELECT COUNT(*) FROM generations"); got != generations {
		t.Fatalf("%d generations after the refused opens, want %d", got, generations)
	}
	if got := k.count("SELECT execution_generation FROM relationships WHERE relationship_id = ?", rid); got != 1 {
		t.Fatalf("the relationship moved to generation %d", got)
	}
}

// The same for an implementation node accepted on its pull request, the case the issue reports.
func TestGenerationOpenIsRefusedForAnAcceptedImplementationNodeWhoseCriteriaChanged(t *testing.T) {
	a := newAcceptRefreshKit(t)
	if _, err := a.acceptWith(""); err != nil {
		t.Fatalf("accept: %v", err)
	}
	a.invRevise("g", "I", "g-r2", withCriteria(dig("the criteria of I with C11")))
	if got := rvAction(t, a.read("g"), "I"); got != rvRevalidate {
		t.Fatalf("the route of I = %q, want %q", got, rvRevalidate)
	}
	reg := &registry.Registry{Store: a.s}
	if _, err := reg.OpenGeneration(context.Background(), a.rid, "by-hand-966", "accepted_result_correction", sql.NullString{}); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("generation-open = %v, want disposition_conflict", err)
	}
}

// generation-bind refuses the generation opened before the guard existed, so the dead end is not completed by binding it; it stays unbound and can be withdrawn.
func TestGenerationBindIsRefusedWhereTheRouteIsARevalidation(t *testing.T) {
	t.Parallel()
	k, accepted := rvSettledSharedRoot(t)
	rid := accepted["A"].RelationshipID
	k.invRevise("sr", "A", "sr-r2", withCriteria(dig("the second edition of A's criteria")))
	number := openUnguarded(t, k, rid, "by-hand-old", "accepted_result_correction")
	reg := &registry.Registry{Store: k.s}
	if _, err := reg.BindAnchor(context.Background(), rid, number, "turn-dispatch", "dispatch_receipt"); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "dag-generation-withdraw") {
		t.Fatalf("generation-bind = %v, want disposition_conflict naming the withdrawal", err)
	}
	if k.count("SELECT COUNT(*) FROM generations WHERE relationship_id = ? AND execution_generation = ? AND anchor_state = 'bound'", rid, number) != 0 {
		t.Fatal("the generation was bound")
	}
}

// What the reading says of a node with such a generation open does not send the parent back to the commands that refuse it.
func TestAnOpenHandGenerationOnARevalidationIsNotRoutedToTheRefusedCommands(t *testing.T) {
	t.Parallel()
	k, accepted := rvSettledSharedRoot(t)
	rid := accepted["A"].RelationshipID
	k.invRevise("sr", "A", "sr-r2", withCriteria(dig("the second edition of A's criteria")))
	openUnguarded(t, k, rid, "by-hand-old", "accepted_result_correction")
	reading := k.read("sr")
	if got := rvAction(t, reading, "A"); got != rvHold {
		t.Fatalf("the route of A = %q, want %q", got, rvHold)
	}
	detail := rvDetail(t, reading, "A")
	if !strings.Contains(detail, "dag-generation-withdraw") || !strings.Contains(detail, "re-validation") || strings.Contains(detail, "accept it with dag-accept --supersedes") {
		t.Fatalf("the reading sends A back to a refused command: %q", detail)
	}
}

// A generation a needs_changes ruling opened is a correction wherever the criteria stand, and a hand-opened correction of an accepted node that is not stale is
// still opened and bound (the controls of CRW-906): the same hand-opened generation passes while the route is a correction.
func TestGenerationOpenIsKeptForTheOtherRoutes(t *testing.T) {
	t.Parallel()
	k, accepted, _, prepared := rvHandKit(t)
	reg := &registry.Registry{Store: k.s}
	rid := accepted["C"].RelationshipID
	if g, err := reg.OpenGeneration(context.Background(), rid, prepared.DispatchRequestID, "needs_changes_revision", sql.NullString{}); err != nil || g.Number != 2 {
		t.Fatalf("generation-open for a node whose route is a correction = %v %+v", err, g)
	}
	if _, err := reg.BindAnchor(context.Background(), rid, 2, "turn-dispatch-"+rid, "dispatch_receipt"); err != nil {
		t.Fatalf("generation-bind = %v", err)
	}
}

// The reading of a re-validation names the registration that the plan revision left undone, and stops naming it once it is done.
func TestARevalidationNamesTheCriteriaRegistrationItNeeds(t *testing.T) {
	t.Parallel()
	k, accepted := rvSettledSharedRoot(t)
	rid := accepted["A"].RelationshipID
	other := dig("the second edition of A's criteria")
	k.invRevise("sr", "A", "sr-r2", withCriteria(other))
	if detail := rvDetail(t, k.read("sr"), "A"); !strings.Contains(detail, "criteria-register") {
		t.Fatalf("the revalidation of A does not name the registration its relationship needs: %q", detail)
	}
	k.exec("UPDATE canonical_criteria SET set_digest = ? WHERE relationship_id = ?", other, rid)
	if detail := rvDetail(t, k.read("sr"), "A"); strings.Contains(detail, "criteria-register") {
		t.Fatalf("the revalidation of A names a registration that is done: %q", detail)
	}
}

// A non_pr node has no head to observe, so the merged mark on the revision its acceptance stands on is its integration: the cleanup of its child is not
// held for an observation that no command can make. Another revision, event or generation is still not integrated.
func TestANonPRNodeIsIntegratedOnItsMergedMark(t *testing.T) {
	t.Parallel()
	k, accepted := rvSettledSharedRoot(t)
	a := accepted["B"]
	var event, revision string
	if err := k.s.DB.QueryRow("SELECT event_id, revision_hash FROM dag_acceptances WHERE acceptance_id = ?", a.AcceptanceID).Scan(&event, &revision); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	applicable, integrated, err := k.sched.ExecutionIntegrated(ctx, a.RelationshipID, event, 1, revision)
	if err != nil || !applicable || !integrated {
		t.Fatalf("ExecutionIntegrated = %v %v %v, want the accepted non_pr node integrated on its mark", applicable, integrated, err)
	}
	for name, wrong := range map[string]struct {
		event, revision string
		generation      int64
	}{"another revision": {event, dig("another revision"), 1}, "another event": {"evt-other", revision, 1}, "another generation": {event, revision, 2}} {
		if _, integrated, err := k.sched.ExecutionIntegrated(ctx, a.RelationshipID, wrong.event, wrong.generation, wrong.revision); err != nil || integrated {
			t.Fatalf("%s: integrated = %v (%v), want not integrated", name, integrated, err)
		}
	}
}

// The guard does not stand on the reason a generation is opened under: generation-open --reason initial_assignment for the same accepted node is a generation that
// dag-correct refuses to record (its reason states no correction) and dag-accept refuses to accept, so it is refused as well, and so is the bind of one opened
// before the guard asked it (verification round 2 of CRW-1036).
func TestGenerationOpenAndBindAreRefusedUnderInitialAssignmentWhereTheRouteIsARevalidation(t *testing.T) {
	t.Parallel()
	k, accepted := rvSettledSharedRoot(t)
	rid := accepted["A"].RelationshipID
	k.invRevise("sr", "A", "sr-r2", withCriteria(dig("the second edition of A's criteria")))
	generations := k.count("SELECT COUNT(*) FROM generations")
	reg := &registry.Registry{Store: k.s}
	if _, err := reg.OpenGeneration(context.Background(), rid, "by-hand-initial", "initial_assignment", sql.NullString{}); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "dag-generation-withdraw") {
		t.Fatalf("generation-open --reason initial_assignment = %v, want the revalidation route to refuse it", err)
	}
	if got := k.count("SELECT COUNT(*) FROM generations"); got != generations {
		t.Fatalf("%d generations after the refused open, want %d", got, generations)
	}
	number := openUnguarded(t, k, rid, "by-hand-initial-old", "initial_assignment")
	if _, err := reg.BindAnchor(context.Background(), rid, number, "turn-dispatch", "dispatch_receipt"); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("generation-bind of an initial_assignment generation = %v, want disposition_conflict", err)
	}
	if k.count("SELECT COUNT(*) FROM generations WHERE relationship_id = ? AND execution_generation = ? AND anchor_state = 'bound'", rid, number) != 0 {
		t.Fatal("the generation was bound")
	}
}

// A hand-opened correction that dag-correct already recorded is an execution of the node: a later revision of the criteria alone leaves it acceptable (the
// criteria are registered again, the generation's result is ruled under them and accepted with --supersedes), so the reading does not call it a dead end
// (verification round 2 of CRW-1036).
func TestARecordedHandCorrectionIsNotADeadEndAfterACriteriaRevision(t *testing.T) {
	t.Parallel()
	k, accepted := rvSettledSharedRoot(t)
	rid := accepted["B"].RelationshipID
	prepared := k.rvPrepare("sr", "B")
	acOpenByHand(t, k, rid, prepared.DispatchRequestID, AcceptedResultCorrection, 2, true)
	if _, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", prepared.ManifestDigest); err != nil {
		t.Fatalf("dag-correct: %v", err)
	}
	criteria := dig("the criteria of B after the recorded correction")
	k.invRevise("sr", "B", "sr-r2", withCriteria(criteria))
	detail := rvDetail(t, k.read("sr"), "B")
	if strings.Contains(detail, "dag-correct refuses to record that generation") || strings.Contains(detail, "record is left as it is") || !strings.Contains(detail, "dag-accept --supersedes") || !strings.Contains(detail, "criteria-register") {
		t.Fatalf("the reading of a recorded correction reports a dead end: %q", detail)
	}
	k.exec("UPDATE canonical_criteria SET set_digest = ? WHERE relationship_id = ?", criteria, rid)
	k.rvReportGeneration(rid, "B", 2, criteria)
	if res, err := k.accept("sr", "B", AcceptInput{Supersedes: accepted["B"].AcceptanceID}); err != nil || res.SupersededID != accepted["B"].AcceptanceID {
		t.Fatalf("accept the recorded correction = %+v %v", res, err)
	}
}
