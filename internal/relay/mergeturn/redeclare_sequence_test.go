package mergeturn

import (
	"strings"
	"testing"
)

// The order a holder follows after it refreshed its pull request branch itself (crw-run's merge
// readiness, "Refresh the base yourself when only the base moved"): the candidate head is
// declared again, the grant that declaration issued is acknowledged, the new head's checks
// finish, readiness is declared on that head, and only then does merge-turn-check pass. A step
// that is skipped is refused or ignored in words that name the next one, with the reason of the
// refusal unchanged.

func mustName(t *testing.T, what, text string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(text, part) {
			t.Fatalf("%s does not name %q: %s", what, part, text)
		}
	}
}

// resetOf is the readinessReset object of an answer, or nil when the answer carries none.
func resetOf(answer map[string]any) map[string]any {
	reset, _ := answer["readinessReset"].(map[string]any)
	return reset
}

func grantOf(t *testing.T, answer map[string]any) (string, map[string]any) {
	t.Helper()
	grant, _ := answer["grant"].(map[string]any)
	if grant == nil {
		t.Fatalf("the answer holds no grant: %v", answer)
	}
	id, _ := grant["grantId"].(string)
	return id, grant
}

// releasedBy is the turn a landing answers about: merge-turn-land wraps it as "released".
func releasedBy(t *testing.T, landing map[string]any) map[string]any {
	t.Helper()
	released, _ := landing["released"].(map[string]any)
	if released == nil {
		t.Fatalf("a landing answers with the turn it released: %v", landing)
	}
	return released
}

// The documented order, after a landing moved the base: re-declare the head, acknowledge the new
// grant, let the new head's checks finish, declare readiness, check, merge and land. (Landing is
// read through releasedBy: merge-turn-land answers with the turn it released.)
func TestRedeclaringTheHeadAfterABaseRefreshInTheDocumentedOrder(t *testing.T) {
	w := newFx(t)
	w.landed("head-x", "base-1")
	turn := w.must(w.claimOn(beta, fxB, "head-b", fxBase, true))["turnId"].(string)
	w.answer(turn, beta.TaskID)

	redeclared := w.must(w.m.Ready(w.ctx, turn, beta.TaskID, false, "head-b2", ""))
	grantID, grant := grantOf(t, redeclared)
	if redeclared["candidateHead"] != "head-b2" || grant["grantedFrom"] != "candidate_restated" || grant["acknowledgedAt"] != nil {
		t.Fatalf("a restated head on a holding turn issues a grant to acknowledge: %v", redeclared)
	}
	reset := resetOf(redeclared)
	if reset == nil || reset["readyRequested"] != false || reset["grantId"] != grantID || reset["previousHead"] != "head-b" || reset["candidateHead"] != "head-b2" {
		t.Fatalf("the answer must name the grant that is owed: %v", redeclared["readinessReset"])
	}
	detail, _ := reset["detail"].(string)
	mustName(t, "the re-declaration", detail, "merge-turn-acknowledge", "--grant "+grantID, "merge-turn-ready", "--head head-b2", "--ready", "checks have finished")
	if strings.Contains(detail, "was not recorded") {
		t.Fatalf("nothing was dropped when --ready was not asked for: %s", detail)
	}

	w.must(w.m.Acknowledge(w.ctx, turn, beta.TaskID, grantID, "read the new grant and re-checked the record"))
	ready := w.must(w.m.Ready(w.ctx, turn, beta.TaskID, true, "head-b2", ""))
	if ready["declaredReady"] != true || resetOf(ready) != nil {
		t.Fatalf("readiness on the head the turn now holds is recorded and needs no explanation: %v", ready)
	}
	if id, _ := grantOf(t, ready); id != grantID {
		t.Fatalf("declaring readiness on an unchanged head must not issue a grant, got %s after %s", id, grantID)
	}

	b := begin{actor: beta.TaskID, head: "head-b2", base: "base-1", checks: runChecks("head-b2", "success", 1, "dev-gate", "run-2"), review: green(), required: []string{"dev-gate"}}
	if merging := w.must(w.begin(turn, b)); merging["state"] != Merging {
		t.Fatalf("the check passes once the head is declared ready and its grant answered: %v", merging["state"])
	}
	w.merged("base-2")
	if released := releasedBy(t, w.must(w.land(turn, "merge-2", "", beta.TaskID))); released["state"] != "landed" || released["landedSha"] != "merge-2" {
		t.Fatalf("the turn lands: %v", released)
	}
}

// A holder that passes --ready with the new head, as the scripts of earlier coordinators did, is
// told that it was not recorded, and each step it then skips is refused with the next one named.
func TestEachStepSkippedAfterRedeclaringTheHeadIsRefusedWithTheNextStep(t *testing.T) {
	w := newFx(t)
	turn := w.held()

	redeclared := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, true, "head-n", ""))
	if redeclared["candidateHead"] != "head-n" || redeclared["declaredReady"] != false {
		t.Fatalf("a restated head must reset readiness even when --ready comes with it: %v", redeclared)
	}
	grantID, _ := grantOf(t, redeclared)
	reset := resetOf(redeclared)
	if reset == nil || reset["readyRequested"] != true || reset["grantId"] != grantID || reset["previousHead"] != "head-a" {
		t.Fatalf("the answer must say that --ready was not recorded and which grant is owed: %v", redeclared["readinessReset"])
	}
	detail, _ := reset["detail"].(string)
	mustName(t, "the ignored --ready", detail, "--ready given with it was not recorded", "merge-turn-acknowledge", "--grant "+grantID, "merge-turn-ready", "--head head-n", "--ready", "checks have finished")

	// Before readiness is declared on the new head the check says how to declare it.
	_, err := w.check(turn, "head-n", "base-0", "")
	if reasonOf(err) != "merge_candidate_moved" {
		t.Fatalf("an undeclared head is refused merge_candidate_moved, got %v", err)
	}
	mustName(t, "the not-ready refusal", err.Error(), "merge-turn-ready", "--head head-n", "--ready")

	w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, true, "head-n", ""))

	// The grant is still unanswered, and the refusal names it and the command that answers it.
	_, err = w.check(turn, "head-n", "base-0", "")
	if reasonOf(err) != "merge_turn_not_held" {
		t.Fatalf("an unacknowledged grant is refused merge_turn_not_held, got %v", err)
	}
	mustName(t, "the unacknowledged-grant refusal", err.Error(), grantID, "merge-turn-acknowledge", "--grant "+grantID)

	w.must(w.m.Acknowledge(w.ctx, turn, alpha.TaskID, grantID, "read the grant and re-checked the record"))
	if merging := w.must(w.check(turn, "head-n", "base-0", "")); merging["state"] != Merging {
		t.Fatalf("the check passes once the head is declared ready and its grant answered: %v", merging["state"])
	}
	w.merged("base-1")
	if released := releasedBy(t, w.must(w.land(turn, "merge-1", "", ""))); released["state"] != "landed" || released["landedSha"] != "merge-1" {
		t.Fatalf("the turn lands: %v", released)
	}
}

func TestACheckNamingAnotherHeadSaysHowToDeclareIt(t *testing.T) {
	w := newFx(t)
	turn := w.held()
	w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, false, "head-n", ""))
	w.answer(turn, alpha.TaskID)
	w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, true, "head-n", ""))
	_, err := w.check(turn, "head-a", "base-0", "")
	if reasonOf(err) != "merge_candidate_moved" {
		t.Fatalf("a restated head other than the declared one is refused merge_candidate_moved, got %v", err)
	}
	mustName(t, "the head mismatch refusal", err.Error(), "head-n", "head-a", "merge-turn-ready", "--head head-a")
}

// A claim behind another holder owes no grant, so its answer asks only for readiness on the new head.
func TestAWaitingClaimIsToldOnlyToDeclareReadinessAgain(t *testing.T) {
	w := newFx(t)
	w.held()
	waiting := w.must(w.claimOn(beta, fxB, "head-b", fxBase, false))["turnId"].(string)
	answer := w.must(w.m.Ready(w.ctx, waiting, beta.TaskID, true, "head-b2", ""))
	if answer["declaredReady"] != false || answer["state"] != Waiting || answer["grant"] != nil || answer["blockedBy"] != nil {
		t.Fatalf("a waiting claim that restates its head holds nothing and is not ready: %v", answer)
	}
	reset := resetOf(answer)
	if reset == nil || reset["grantId"] != nil || reset["readyRequested"] != true {
		t.Fatalf("a waiting claim is told its --ready was dropped and owes no grant: %v", answer["readinessReset"])
	}
	detail, _ := reset["detail"].(string)
	mustName(t, "the ignored --ready", detail, "--ready given with it was not recorded", "merge-turn-ready", "--head head-b2", "--ready")
	if strings.Contains(detail, "merge-turn-acknowledge") {
		t.Fatalf("a waiting claim has no grant to acknowledge: %s", detail)
	}
}

// A waiting claim on a free target that restated its head did not take the target with the --ready
// that came along; the next --ready on that head does, and issues the grant a promotion would.
func TestARestatedHeadOnAFreeTargetTakesItOnlyWhenReadinessIsDeclaredAgain(t *testing.T) {
	w := newFx(t)
	held := fClaim(w, alpha, fxA, "head-a", true)
	waiting := fClaim(w, beta, fxB, "head-b", false)
	w.must(fRelease(w, held, alpha.TaskID, "done", "returned", ""))
	if open := w.must(w.m.Turn(w.ctx, waiting)); open["state"] != Waiting {
		t.Fatalf("an unready claim stays waiting on the target its holder returned: %v", open["state"])
	}

	restated := w.must(w.m.Ready(w.ctx, waiting, beta.TaskID, true, "head-b2", ""))
	reset := resetOf(restated)
	if restated["state"] != Waiting || restated["declaredReady"] != false || restated["blockedBy"] != nil || restated["grant"] != nil || reset == nil || reset["grantId"] != nil {
		t.Fatalf("--ready with a restated head does not take the free target: %v", restated)
	}
	taken := w.must(w.m.Ready(w.ctx, waiting, beta.TaskID, true, "head-b2", ""))
	_, grant := grantOf(t, taken)
	if taken["state"] != Holding || taken["declaredReady"] != true || grant["grantedFrom"] != "late_ready" || resetOf(taken) != nil {
		t.Fatalf("readiness declared again on that head takes the target with a grant to answer: %v", taken)
	}
}

func TestOnlyARestatedHeadIsAnsweredWithAReset(t *testing.T) {
	w := newFx(t)
	turn := w.held()
	same := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, true, "", ""))
	if resetOf(same) != nil || same["declaredReady"] != true {
		t.Fatalf("--ready on the head the turn holds is recorded and needs no explanation: %v", same)
	}
	withdrawn := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, false, "head-n", ""))
	reset := resetOf(withdrawn)
	if reset == nil || reset["readyRequested"] != false || reset["grantId"] == nil {
		t.Fatalf("a restated head is answered with the grant it owes even when --not-ready came with it: %v", withdrawn["readinessReset"])
	}
}
