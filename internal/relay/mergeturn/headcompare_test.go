package mergeturn

import (
	"strings"
	"testing"
)

// CRW-586: merge-turn-ready and merge-turn-check compare the candidate head with a source of truth whenever the turn
// records a pull request or a relationship, and refuse when there is nothing to compare it with.

// A pull request on a local-path repository has no forge to read from the turn alone, and with no relationship there is
// no work report either: Ready refuses a moved head and refuses to declare readiness, and says how to get out.
func TestHeadCompareLocalPathPullRequestReadyNeedsAWorkReport(t *testing.T) {
	w, _ := headCompareFixture(t)
	turn := headCompareHeld(w, headCompareLocal, "head-500", 500, "", false)
	ledger := headCompareCount(w, "SELECT COUNT(*) FROM merge_turn_ledger")

	for _, ask := range []struct {
		ready bool
		head  string
	}{{false, "head-501"}, {true, "head-500"}, {true, ""}} {
		answer, err := w.m.Ready(w.ctx, turn, alpha.TaskID, ask.ready, ask.head, "")
		if answer != nil {
			t.Fatalf("ready=%v head=%q was accepted with nothing to compare it with: %v", ask.ready, ask.head, answer)
		}
		headCompareRefused(t, err, "merge_target_unreadable", turn, "pull request 500", headCompareWayOneText, headCompareWayTwoText)
	}
	live := w.must(w.m.Turn(w.ctx, turn))
	if live["candidateHead"] != "head-500" || live["declaredReady"] != false {
		t.Errorf("a refused call changed the turn: %v", live)
	}
	if got := headCompareCount(w, "SELECT COUNT(*) FROM merge_turn_ledger"); got != ledger {
		t.Errorf("a refused call wrote ledger rows: %d, was %d", got, ledger)
	}
}

// The check is where a head reaches merging: another pull request's head on a local path no longer gets there, whichever
// reader the relay was given. The tip is readable, so the refusal is the comparison's and not the target's.
func TestHeadCompareLocalPathPullRequestCheckRefusesAnotherPullRequestsHead(t *testing.T) {
	w, pulls := headCompareFixture(t)
	turn := headCompareHeld(w, headCompareLocal, "head-501", 500, "", true)
	for name, reader := range map[string]Reader{"a reader that reads pull requests": livePullReader{target: w.target, pulls: pulls}, "a reader of the base only": w.target} {
		answer, err := headCompareCheck(w, turn, "head-501", reader)
		if answer != nil {
			t.Fatalf("%s: the check began the merge with nothing to compare the head with: %v", name, answer["state"])
		}
		headCompareRefused(t, err, "merge_target_unreadable", "pull request 500", headCompareWayOneText, headCompareWayTwoText)
		if live := w.must(w.m.Turn(w.ctx, turn)); live["state"] != Holding {
			t.Fatalf("%s: a refused check moved the turn to %v", name, live["state"])
		}
	}
	if pulls.readCount() != 0 {
		t.Errorf("a local repository read the forge %d times", pulls.readCount())
	}
}

// With a relationship the work report is the source of truth: it must name the head before Ready takes it, and the same
// report is what the check compares with. This replaces the old answer that the record decided.
func TestHeadCompareLocalPathPullRequestIsDecidedByTheWorkReport(t *testing.T) {
	w, pulls := headCompareFixture(t)
	headCompareRelate(w, headCompareRel)
	headCompareReport(w, headCompareRel, "event-a", 1, 3, "head-500")
	turn := headCompareHeld(w, headCompareLocal, "head-500", 500, headCompareRel, false)

	_, err := w.m.Ready(w.ctx, turn, alpha.TaskID, false, "head-501", "refreshed the base")
	headCompareRefused(t, err, "merge_candidate_moved", headCompareRel, "head-500", "head-501")

	headCompareReport(w, headCompareRel, "event-a", 2, 3, "head-501")
	restated := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, false, "head-501", "refreshed the base"))
	headCompareDecision(t, restated, "work_report", "head-501", 500, headCompareRel)
	reset, _ := restated["readinessReset"].(map[string]any)
	if restated["candidateHead"] != "head-501" || reset == nil {
		t.Fatalf("the head the report names was not taken: %v", restated)
	}
	w.must(w.m.Acknowledge(w.ctx, turn, alpha.TaskID, reset["grantId"].(string), "read the new grant"))

	declared := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, true, "head-501", ""))
	headCompareDecision(t, declared, "work_report", "head-501", 500, headCompareRel)
	if declared["declaredReady"] != true {
		t.Fatalf("readiness was not declared on the head the report names: %v", declared["declaredReady"])
	}
	checked, err := headCompareCheck(w, turn, "head-501", livePullReader{target: w.target, pulls: pulls})
	if err != nil || checked["state"] != Merging {
		t.Fatalf("%v %v", checked, err)
	}
	headCompareDecision(t, checked, "work_report", "head-501", 500, headCompareRel)
	if checked["headVerifiedAgainst"] != "head-501" {
		t.Errorf("headVerifiedAgainst = %v", checked["headVerifiedAgainst"])
	}
	if pulls.readCount() != 0 {
		t.Errorf("a local repository read the forge %d times", pulls.readCount())
	}
}

// A claim that names only a relationship, on a forge repository, used to pass any head when the relationship had no work
// report. It refuses at Ready and at Check, and the head is taken from the report once the report records it.
func TestHeadCompareRelationshipOnlyClaimNeedsAWorkReport(t *testing.T) {
	w, pulls := headCompareFixture(t)
	headCompareRelate(w, headCompareRel)
	pulls.set(fxRepo, 501, "head-501")
	// claimed with another pull request's head and not yet ready, so the candidate-moved check cannot be the reason
	turn := headCompareHeld(w, fxRepo, "head-501", 0, headCompareRel, false)
	reader := livePullReader{target: w.target, pulls: pulls}

	answer, err := w.m.Ready(w.ctx, turn, alpha.TaskID, true, "head-501", "")
	if answer != nil {
		t.Fatalf("readiness was declared on a head nothing vouches for: %v", answer["declaredReady"])
	}
	headCompareRefused(t, err, "merge_target_unreadable", headCompareRel, headCompareWayOneText, headCompareWayTwoText)
	if live := w.must(w.m.Turn(w.ctx, turn)); live["declaredReady"] != false {
		t.Fatalf("a refused call declared readiness: %v", live["declaredReady"])
	}

	w.exec("UPDATE merge_turns SET declared_ready = 1 WHERE turn_id = ?", turn)
	checked, err := headCompareCheck(w, turn, "head-501", reader)
	if checked != nil {
		t.Fatalf("the check began the merge on another pull request's head: %v", checked["state"])
	}
	headCompareRefused(t, err, "merge_target_unreadable", headCompareRel, headCompareWayOneText, headCompareWayTwoText)
	if live := w.must(w.m.Turn(w.ctx, turn)); live["state"] != Holding {
		t.Fatalf("a refused check moved the turn to %v", live["state"])
	}

	headCompareReport(w, headCompareRel, "event-a", 1, 3, "head-501")
	checked, err = headCompareCheck(w, turn, "head-501", reader)
	if err != nil || checked["state"] != Merging {
		t.Fatalf("%v %v", checked, err)
	}
	headCompareDecision(t, checked, "work_report", "head-501", 0, headCompareRel)
}

// Ready read the forge only when the head moved, so a head recorded wrong from the start was declared ready.
func TestHeadCompareReadyComparesAnUnmovedHead(t *testing.T) {
	w, pulls := headCompareFixture(t)
	turn := headCompareHeld(w, fxRepo, "head-501", 500, headCompareRel, false)
	ledger := headCompareCount(w, "SELECT COUNT(*) FROM merge_turn_ledger")

	for _, head := range []string{"", "head-501"} {
		answer, err := w.m.Ready(w.ctx, turn, alpha.TaskID, true, head, "")
		if answer != nil {
			t.Fatalf("head %q: readiness was declared on a head that is not the pull request's: %v", head, answer["declaredReady"])
		}
		headCompareRefused(t, err, "merge_candidate_moved", turn, "pull request 500", "head-500", "head-501")
	}
	if pulls.readCount() != 2 {
		t.Errorf("%d forge reads for two declarations", pulls.readCount())
	}
	live := w.must(w.m.Turn(w.ctx, turn))
	if live["declaredReady"] != false || live["candidateHead"] != "head-501" {
		t.Errorf("a refused call changed the turn: %v", live)
	}
	if got := headCompareCount(w, "SELECT COUNT(*) FROM merge_turn_ledger"); got != ledger {
		t.Errorf("a refused call wrote ledger rows: %d, was %d", got, ledger)
	}

	// withdrawing readiness on an unmoved head asserts nothing about the head, so nothing is read
	withdrawn := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, false, "head-501", ""))
	if withdrawn["pullRequestHead"] != nil || pulls.readCount() != 2 {
		t.Errorf("withdrawing readiness compared a head: %v, %d reads", withdrawn["pullRequestHead"], pulls.readCount())
	}

	// the pull request's own head is taken, and declaring readiness on it compares again
	restated := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, false, "head-500", ""))
	headCompareDecision(t, restated, "forge", "head-500", 500, "")
	reset, _ := restated["readinessReset"].(map[string]any)
	w.must(w.m.Acknowledge(w.ctx, turn, alpha.TaskID, reset["grantId"].(string), "read the new grant"))
	reads := pulls.readCount()
	declared := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, true, "", ""))
	headCompareDecision(t, declared, "forge", "head-500", 500, "")
	if declared["declaredReady"] != true || pulls.readCount() != reads+1 {
		t.Errorf("%v, %d reads (was %d)", declared["declaredReady"], pulls.readCount(), reads)
	}
}

// A candidate that changes between the early forge read and the transaction is not the candidate that was read for,
// whether the caller named the head or left it to default to the candidate.
func TestHeadCompareReadyRefusesAHeadWhoseCandidateChangedAfterTheEarlyRead(t *testing.T) {
	for _, head := range []string{"head-500", ""} {
		t.Run("head "+head, func(t *testing.T) {
			w, _ := headCompareFixture(t)
			turn := headCompareHeld(w, fxRepo, "head-500", 500, "", false)
			ledger := headCompareCount(w, "SELECT COUNT(*) FROM merge_turn_ledger")
			grants := headCompareCount(w, "SELECT COUNT(*) FROM merge_turn_ledger WHERE evidence_kind = 'grant'")
			moved := false
			w.m.Now = func() string {
				if !moved {
					moved = true
					w.exec("UPDATE merge_turns SET candidate_head = ? WHERE turn_id = ?", "head-elsewhere", turn)
				}
				return fxISO
			}
			answer, err := w.m.Ready(w.ctx, turn, alpha.TaskID, true, head, "")
			if answer != nil {
				t.Fatalf("a candidate that changed during the call was declared ready: %v", answer)
			}
			headCompareRefused(t, err, "merge_target_unreadable", "changed during the call")
			live := w.must(w.m.Turn(w.ctx, turn))
			if live["candidateHead"] != "head-elsewhere" || live["declaredReady"] != false {
				t.Errorf("the refused call changed the turn: %v", live)
			}
			if headCompareCount(w, "SELECT COUNT(*) FROM merge_turn_ledger") != ledger || headCompareCount(w, "SELECT COUNT(*) FROM merge_turn_ledger WHERE evidence_kind = 'grant'") != grants {
				t.Errorf("the refused call wrote ledger rows or a grant")
			}
		})
	}
}

// What the work reports of a relationship say about the head: the newest generation that names a head, each event's own
// latest submission, and the same commit written two ways is one head.
func TestHeadCompareWorkReportHeads(t *testing.T) {
	cases := []struct {
		name    string
		reports func(w *fx)
		declare string
		reason  string
		wants   []string
		decided string
	}{
		{"another head", func(w *fx) { headCompareReport(w, headCompareRel, "event-a", 1, 3, "head-a") }, "head-b", "merge_candidate_moved", []string{"head-a", "head-b"}, ""},
		{"two events name two heads", func(w *fx) {
			headCompareReport(w, headCompareRel, "event-a", 1, 3, "head-a")
			headCompareReport(w, headCompareRel, "event-b", 1, 3, "head-b")
		}, "head-a", "revision_ambiguous", []string{"head-a", "head-b"}, ""},
		{"the same commit written two ways", func(w *fx) { headCompareReport(w, headCompareRel, "event-a", 1, 3, " HEAD-A ") }, "head-a", "", nil, "head-a"},
		{"two events name one commit", func(w *fx) {
			headCompareReport(w, headCompareRel, "event-a", 1, 3, "head-a")
			headCompareReport(w, headCompareRel, "event-b", 1, 3, "HEAD-A")
		}, "head-a", "", nil, "head-a"},
		{"the newest generation is read", func(w *fx) {
			headCompareReport(w, headCompareRel, "event-a", 1, 1, "head-old")
			headCompareReport(w, headCompareRel, "event-b", 1, 3, "head-new")
		}, "head-old", "merge_candidate_moved", []string{"head-new"}, ""},
		{"a newer generation that names no head is not read", func(w *fx) {
			headCompareReport(w, headCompareRel, "event-a", 1, 1, "head-a")
			headCompareReport(w, headCompareRel, "event-b", 1, 3, nil)
		}, "head-a", "", nil, "head-a"},
		{"a report that names no head names nothing", func(w *fx) { headCompareReport(w, headCompareRel, "event-a", 1, 3, nil) }, "head-a", "merge_target_unreadable", []string{headCompareWayTwoText}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, _ := headCompareFixture(t)
			headCompareRelate(w, headCompareRel)
			c.reports(w)
			turn := headCompareHeld(w, fxRepo, "head-0", 0, headCompareRel, false)
			answer, err := w.m.Ready(w.ctx, turn, alpha.TaskID, false, c.declare, "")
			if c.reason != "" {
				if answer != nil {
					t.Fatalf("the head was taken: %v", answer["candidateHead"])
				}
				headCompareRefused(t, err, c.reason, c.wants...)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			headCompareDecision(t, answer, "work_report", c.decided, 0, headCompareRel)
		})
	}
}

// A pull request on a forge, with a relationship that has no work report, is the normal flow: the forge decides and the
// check has no report head to disagree with. A report that holds the same commit written another way agrees with the
// forge; one that names another commit still refuses.
func TestHeadCompareForgePullRequestWithARelationshipIsDecidedByTheForge(t *testing.T) {
	w, pulls := headCompareFixture(t)
	headCompareRelate(w, headCompareRel)
	reader := livePullReader{target: w.target, pulls: pulls}
	turn := headCompareHeld(w, fxRepo, "head-500", 500, headCompareRel, false)
	pulls.set(fxRepo, 500, "head-500-refreshed")
	restated := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, false, "head-500-refreshed", ""))
	headCompareDecision(t, restated, "forge", "head-500-refreshed", 500, "")
	reset, _ := restated["readinessReset"].(map[string]any)
	w.must(w.m.Acknowledge(w.ctx, turn, alpha.TaskID, reset["grantId"].(string), "read the new grant"))
	declared := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, true, "head-500-refreshed", ""))
	headCompareDecision(t, declared, "forge", "head-500-refreshed", 500, "")

	headCompareReport(w, headCompareRel, "event-a", 1, 3, "head-elsewhere")
	_, err := headCompareCheck(w, turn, "head-500-refreshed", reader)
	headCompareRefused(t, err, "merge_candidate_moved", "names head 'head-elsewhere'")

	w.exec("DELETE FROM work_reports WHERE relationship_id = ?", headCompareRel)
	headCompareReport(w, headCompareRel, "event-a", 1, 3, " HEAD-500-REFRESHED ")
	checked, err := headCompareCheck(w, turn, "head-500-refreshed", reader)
	if err != nil || checked["state"] != Merging {
		t.Fatalf("%v %v", checked, err)
	}
	headCompareDecision(t, checked, "forge", "head-500-refreshed", 500, "")
	if !strings.EqualFold(strings.TrimSpace(checked["headVerifiedAgainst"].(string)), "head-500-refreshed") {
		t.Errorf("headVerifiedAgainst = %v", checked["headVerifiedAgainst"])
	}

	w2, pulls2 := headCompareFixture(t)
	headCompareRelate(w2, headCompareRel)
	turn2 := headCompareHeld(w2, fxRepo, "head-500", 500, headCompareRel, true)
	checked2, err := headCompareCheck(w2, turn2, "head-500", livePullReader{target: w2.target, pulls: pulls2})
	if err != nil || checked2["state"] != Merging || checked2["headVerifiedAgainst"] != nil {
		t.Fatalf("the normal flow with no work report: %v %v", checked2, err)
	}
	headCompareDecision(t, checked2, "forge", "head-500", 500, "")
}

// A relationship attached to another project is not a source of truth for this project's turn, at Ready as at Check.
func TestHeadCompareWorkReportOfAnotherProjectIsRefused(t *testing.T) {
	w, _ := headCompareFixture(t)
	headCompareRelate(w, headCompareRel)
	headCompareReport(w, headCompareRel, "event-a", 1, 3, "head-a")
	w.exec("INSERT INTO relationship_scope (relationship_id,project_key,recorded_at) VALUES (?,?,?)", headCompareRel, fxB, fxISO)
	turn := headCompareHeld(w, fxRepo, "head-0", 0, headCompareRel, false)
	answer, err := w.m.Ready(w.ctx, turn, alpha.TaskID, false, "head-a", "")
	if answer != nil {
		t.Fatalf("the head of another project's assignment was taken: %v", answer["candidateHead"])
	}
	headCompareRefused(t, err, "foreign_scope", headCompareRel, fxB)
}

// A relationship that was never attached to a project has no scope to contradict; it still needs a report to compare with.
func TestHeadCompareUnattachedRelationshipStillNeedsAReport(t *testing.T) {
	w, _ := headCompareFixture(t)
	turn := headCompareHeld(w, fxRepo, "head-0", 0, "rel-never-attached", false)
	_, err := w.m.Ready(w.ctx, turn, alpha.TaskID, false, "head-a", "")
	headCompareRefused(t, err, "merge_target_unreadable", "rel-never-attached", headCompareWayTwoText)
}

// A claim that records no pull request and no relationship records nothing to compare, as it always was.
func TestHeadCompareBareClaimReadsNothing(t *testing.T) {
	w, pulls := headCompareFixture(t)
	turn := headCompareHeld(w, fxRepo, "head-a", 0, "", false)
	moved := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, true, "head-anything", ""))
	declared := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, true, "head-anything", ""))
	if moved["pullRequestHead"] != nil || declared["pullRequestHead"] != nil || pulls.readCount() != 0 {
		t.Fatalf("a bare claim was compared: %v %v, %d reads", moved["pullRequestHead"], declared["pullRequestHead"], pulls.readCount())
	}
}

// A relay built with no pull request reader decides a forge pull request by its record, as before (production composition
// always supplies one: see the next test).
func TestHeadCompareForgePullRequestWithoutAReaderIsDecidedByTheRecord(t *testing.T) {
	w := newFx(t)
	turn := headCompareHeld(w, fxRepo, "head-501", 500, "", false)
	answer := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, true, "", ""))
	if answer["declaredReady"] != true || answer["pullRequestHead"] != nil {
		t.Fatalf("%v", answer)
	}
	checked, err := headCompareCheck(w, turn, "head-501", w.target)
	if err != nil || checked["state"] != Merging || checked["pullRequestHead"] != nil {
		t.Fatalf("%v %v", checked, err)
	}
}

// The relay commands build the service with a pull request reader and give merge-turn-check one: the comparison with the
// forge is not left to a configuration.
func TestHeadCompareProductionWiringReadsPullRequests(t *testing.T) {
	w := newFx(t)
	if service(w.r).Pulls == nil {
		t.Fatal("the service the relay commands build has no pull request reader")
	}
	var reader Reader = TargetReader{}
	if _, ok := reader.(PullRequestHeadReader); !ok {
		t.Fatal("the reader merge-turn-check is given cannot read pull requests")
	}
}
