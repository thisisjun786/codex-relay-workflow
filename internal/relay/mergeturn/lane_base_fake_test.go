package mergeturn

import (
	"context"
	"strings"
	"testing"
)

// CRW-403 at the service level, with a reader the test controls: what the check does with each
// answer the branch reading can give, and what it writes (or does not).

// lbMoving is a target reader that also reads how the branch moved, as the test scripts it.
type lbMoving struct {
	*fakeTarget
	read  func(repository, base, from, to string) (Movement, error)
	calls int
}

func (m *lbMoving) Movement(_ context.Context, repository, base, from, to string) (Movement, error) {
	m.calls++
	return m.read(repository, base, from, to)
}

// oneMerge is the movement of a branch that took one merge commit on top of from.
func oneMerge(from string) func(string, string, string, string) (Movement, error) {
	return func(repository, base, f, t string) (Movement, error) {
		return Movement{From: f, To: t, Steps: []Step{{SHA: t, Parents: []string{from, "side-" + t}, Subject: "Merge pull request #7 from team/topic"}}, Source: "fake", Reference: "refs/heads/" + base, Repository: repository}, nil
	}
}

// afterOutsideMerge lands a candidate in the lane (the base reads base-1 afterwards), moves the
// branch to base-2 outside it, and returns the landing and a second parent holding the lane.
func afterOutsideMerge(t *testing.T) (w *fx, landing, second string) {
	t.Helper()
	w = newFx(t)
	landing = w.landed("head-a", fxPost)
	w.target.set(fxRepo, fxBase, "base-2")
	second = w.heldOn(beta, fxB, "head-b", fxBase)
	return w, landing, second
}

func (w *fx) checkWith(reader Reader, turn, actor, head, base string, checks []any) (map[string]any, error) {
	b := defaults()
	b.actor, b.head, b.base, b.checks = actor, head, base, checks
	return w.m.Check(w.ctx, turn, b.actor, b.head, b.base, b.checks, b.review, b.required, reader)
}

func (w *fx) recorded(turn string) string {
	w.t.Helper()
	row, err := w.s.One(w.ctx, "SELECT observed_base_sha FROM merge_turns WHERE turn_id = ?", turn)
	if err != nil {
		w.t.Fatal(err)
	}
	return row.Text("observed_base_sha")
}

func (w *fx) restatementCount(turn string) int {
	w.t.Helper()
	rows, err := w.s.All(w.ctx, "SELECT entry_id FROM merge_turn_ledger WHERE turn_id = ? AND evidence_kind = 'landing_base_restated'", turn)
	if err != nil {
		w.t.Fatal(err)
	}
	return len(rows)
}

func TestCRW403_FakeMovementConfirmsAndRestates(t *testing.T) {
	w, landing, second := afterOutsideMerge(t)
	reader := &lbMoving{fakeTarget: w.target, read: oneMerge(fxPost)}
	got, err := w.checkWith(reader, second, beta.TaskID, "head-b", "base-2", runChecks("head-b", "success", 1, "dev-gate", "run-1"))
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if reader.calls != 1 {
		t.Fatalf("the movement was read %d times, want once", reader.calls)
	}
	if base := w.recorded(landing); base != "base-2" {
		t.Fatalf("recorded base %s", base)
	}
	if got["landingBaseRestated"] == nil {
		t.Fatal("the answer does not report the restatement")
	}
	// The journal keeps the same kind as a manual restatement, marked automatic.
	rows, err := w.s.All(w.ctx, "SELECT detail FROM journal WHERE kind = 'merge_turn_base_restated' AND subject = ?", landing)
	if err != nil || len(rows) != 1 || !strings.Contains(rows[0].Text("detail"), `"automatic": true`) || !strings.Contains(rows[0].Text("detail"), `"from": "`+fxPost+`"`) {
		t.Fatalf("journal rows: %v %v", rows, err)
	}
}

// A check that is refused for another reason after the restatement leaves the restatement in
// place: it is a fact about the branch, not about this candidate.
func TestCRW403_FakeRestatementStandsWhenALaterGateRefuses(t *testing.T) {
	w, landing, second := afterOutsideMerge(t)
	reader := &lbMoving{fakeTarget: w.target, read: oneMerge(fxPost)}
	_, err := w.checkWith(reader, second, beta.TaskID, "head-b", "base-2", nil)
	if reasonOf(err) != "merge_currency_stale" || strings.Contains(err.Error(), "the last landing") {
		t.Fatalf("want the required-check refusal, got %v", err)
	}
	if base := w.recorded(landing); base != "base-2" || w.restatementCount(landing) != 1 {
		t.Fatalf("recorded base %s with %d restatements after a refused check", base, w.restatementCount(landing))
	}
	if _, err := w.checkWith(reader, second, beta.TaskID, "head-b", "base-2", runChecks("head-b", "success", 1, "dev-gate", "run-1")); err != nil {
		t.Fatalf("the repeat with complete evidence was refused: %v", err)
	}
	if w.restatementCount(landing) != 1 || reader.calls != 1 {
		t.Fatalf("%d restatements and %d movement reads after the repeat, want 1 and 1", w.restatementCount(landing), reader.calls)
	}
}

// A reader that cannot read how the branch moved leaves the refusal as it was, plus why and the
// recovery command. Nothing is written.
func TestCRW403_FakeReaderWithoutTheCapabilityRefusesAsBefore(t *testing.T) {
	w, landing, second := afterOutsideMerge(t)
	_, err := w.checkWith(w.target, second, beta.TaskID, "head-b", "base-2", runChecks("head-b", "success", 1, "dev-gate", "run-1"))
	detail := refusalDetail(t, err)
	if !strings.Contains(detail, "the last landing on this target, turn '"+landing+"', recorded base '"+fxPost+"' and this restates 'base-2'; the base moved under the candidate.") {
		t.Fatalf("the refusal lost its sentence: %s", detail)
	}
	if !strings.Contains(detail, "cannot read how the branch moved") {
		t.Fatalf("the refusal does not say why nothing was restated: %s", detail)
	}
	assertRecoveryNamed(t, detail, landing, alpha.TaskID)
	if w.recorded(landing) != fxPost || w.restatementCount(landing) != 0 {
		t.Fatal("something was written without a confirmed cause")
	}
}

func TestCRW403_FakeAFailedMovementReadIsNotConfirmed(t *testing.T) {
	w, landing, second := afterOutsideMerge(t)
	reader := &lbMoving{fakeTarget: w.target, read: func(string, string, string, string) (Movement, error) {
		return Movement{}, &TargetUnreadable{"the forge said HTTP 502"}
	}}
	_, err := w.checkWith(reader, second, beta.TaskID, "head-b", "base-2", runChecks("head-b", "success", 1, "dev-gate", "run-1"))
	detail := refusalDetail(t, err)
	if !strings.Contains(detail, "reading how the branch moved failed: the forge said HTTP 502") {
		t.Fatalf("the refusal does not carry the read failure: %s", detail)
	}
	assertRecoveryNamed(t, detail, landing, alpha.TaskID)
	if w.recorded(landing) != fxPost || w.restatementCount(landing) != 0 {
		t.Fatal("something was written without a confirmed cause")
	}
}

// A reading of some other span is not a reading of this one.
func TestCRW403_FakeAReadingOfAnotherSpanIsNotConfirmed(t *testing.T) {
	w, landing, second := afterOutsideMerge(t)
	reader := &lbMoving{fakeTarget: w.target, read: func(repository, base, from, to string) (Movement, error) {
		return Movement{From: "other-base", To: to, Steps: []Step{{SHA: to, Parents: []string{"other-base", "side"}}}}, nil
	}}
	_, err := w.checkWith(reader, second, beta.TaskID, "head-b", "base-2", runChecks("head-b", "success", 1, "dev-gate", "run-1"))
	if !strings.Contains(refusalDetail(t, err), "does not run from the recorded base to the tip") {
		t.Fatalf("refused for another reason: %v", err)
	}
	if w.recorded(landing) != fxPost || w.restatementCount(landing) != 0 {
		t.Fatal("something was written without a confirmed cause")
	}
}

// The landing is repaired by hand while the branch is being read: the reading is of a record that
// no longer stands, so the check is refused for a repeat and writes nothing.
func TestCRW403_FakeALandingChangedDuringTheReadIsNotConfirmed(t *testing.T) {
	w, landing, second := afterOutsideMerge(t)
	reader := &lbMoving{fakeTarget: w.target}
	reader.read = func(repository, base, from, to string) (Movement, error) {
		w.exec("UPDATE merge_turns SET observed_base_sha = 'base-elsewhere' WHERE turn_id = ?", landing)
		return oneMerge(fxPost)(repository, base, from, to)
	}
	_, err := w.checkWith(reader, second, beta.TaskID, "head-b", "base-2", runChecks("head-b", "success", 1, "dev-gate", "run-1"))
	if !strings.Contains(refusalDetail(t, err), "changed while the branch was being read; call again") {
		t.Fatalf("refused for another reason: %v", err)
	}
	if w.recorded(landing) != "base-elsewhere" || w.restatementCount(landing) != 0 {
		t.Fatal("the check wrote over a record it had not read")
	}
}

// A turn merging or unknown on the target means the landing is not yet known; with the guard
// index absent the check does not rely on it.
func TestCRW403_FakeATurnInFlightIsNotConfirmed(t *testing.T) {
	w, landing, second := afterOutsideMerge(t)
	w.exec("DROP INDEX merge_turns_one_live_holder")
	other := w.must(w.claimOn(alpha, fxA, "head-c", fxBase, true))["turnId"].(string)
	w.exec("UPDATE merge_turns SET state = 'merging' WHERE turn_id = ?", other)
	reader := &lbMoving{fakeTarget: w.target, read: oneMerge(fxPost)}
	_, err := w.checkWith(reader, second, beta.TaskID, "head-b", "base-2", runChecks("head-b", "success", 1, "dev-gate", "run-1"))
	if !strings.Contains(refusalDetail(t, err), "is merging or unknown on this target") {
		t.Fatalf("refused for another reason: %v", err)
	}
	if w.recorded(landing) != fxPost || w.restatementCount(landing) != 0 {
		t.Fatal("something was written while a turn was in flight")
	}
}

// A manual restatement after an automatic one takes the next sequence, and both are listed.
func TestCRW403_FakeManualRestatementFollowsTheAutomaticOne(t *testing.T) {
	w, landing, second := afterOutsideMerge(t)
	reader := &lbMoving{fakeTarget: w.target, read: oneMerge(fxPost)}
	w.must(w.checkWith(reader, second, beta.TaskID, "head-b", "base-2", runChecks("head-b", "success", 1, "dev-gate", "run-1")))
	w.target.set(fxRepo, fxBase, "base-3")
	// A turn that is merging blocks a manual restatement, so return the held one first.
	w.must(w.m.Unknown(w.ctx, second, beta.TaskID, "lost the connection"))
	w.must(w.m.Resolve(w.ctx, second, overseer.TaskID, "base-3", "closed", "closed unmerged", w.target))
	w.must(w.restate(landing, alpha.TaskID, "", "dev moved again by a hand-made merge"))
	list, _ := w.must(w.m.Turn(w.ctx, landing))["baseRestatements"].([]any)
	if len(list) != 2 {
		t.Fatalf("restatements: %v", list)
	}
	first, secondEntry := list[0].(map[string]any), list[1].(map[string]any)
	if first["from"] != fxPost || first["to"] != "base-2" || secondEntry["from"] != "base-2" || secondEntry["to"] != "base-3" {
		t.Fatalf("restatements out of order or wrong: %v", list)
	}
	if !strings.Contains(first["evidence"].(string), "automatic") || strings.Contains(secondEntry["evidence"].(string), "automatic") {
		t.Fatalf("evidence does not tell the two apart: %v", list)
	}
	if w.recorded(landing) != "base-3" {
		t.Fatalf("recorded base %s", w.recorded(landing))
	}
}
