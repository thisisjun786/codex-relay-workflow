package mergeturn

import (
	"fmt"
	"strings"
	"testing"
)

// CRW-608: a service with no pull request head reader refuses a recorded forge pull request instead of deciding it by the
// turn's record. The fixture `newFx` builds such a service (no `Pulls`); a pull request on owner/repo is the recorded forge
// pull request, and the fake forge below is seeded by each test, never read off the turn it is asked about.

const (
	readerlessHead = "head-500"
	// readerlessWhy is the text a refused call carries for pull request 500 of owner/repo.
	readerlessWhy = "this service has no pull request head reader for recorded pull request 500 of owner/repo; supply a pull request head reader and call again"
)

// readerlessTurn is a holding turn bound to forge pull request 500 of owner/repo at head-500, its grant answered.
func readerlessTurn(w *fx, relationship string, ready bool) string {
	w.t.Helper()
	return headCompareHeld(w, fxRepo, readerlessHead, 500, relationship, ready)
}

// readerlessLane is every turn and every ledger row, as stored: what a refused call must leave as it was (the turn, its
// candidate head, its readiness, its grants and the merge ledger).
func readerlessLane(w *fx) string {
	w.t.Helper()
	var lane strings.Builder
	for _, table := range []string{"merge_turns", "merge_turn_ledger"} {
		rows, err := w.s.All(w.ctx, "SELECT * FROM "+table+" ORDER BY rowid")
		if err != nil {
			w.t.Fatal(err)
		}
		fmt.Fprintf(&lane, "%s %v\n", table, rows)
	}
	return lane.String()
}

// readerlessContests is the detail of every merge_target_unreadable contest the lane holds.
func readerlessContests(w *fx) []string {
	w.t.Helper()
	rows, err := w.s.All(w.ctx, "SELECT detail FROM coordination_conflicts WHERE reason = 'merge_target_unreadable' ORDER BY id")
	if err != nil {
		w.t.Fatal(err)
	}
	details := make([]string, 0, len(rows))
	for _, row := range rows {
		details = append(details, fmt.Sprint(row.Get("detail")))
	}
	return details
}

// readerlessRefused fails unless the call was refused merge_target_unreadable with the readerless text and left the lane as
// it was, with the contest recorded once.
func readerlessRefused(t *testing.T, w *fx, before string, answer map[string]any, err error, verb, turn string) {
	t.Helper()
	if answer != nil {
		t.Fatalf("a forge pull request that nothing could read was passed: %v", answer)
	}
	headCompareRefused(t, err, "merge_target_unreadable", turn, verb, readerlessWhy)
	if after := readerlessLane(w); after != before {
		t.Errorf("the refused call changed the turn, its candidate, readiness, grant or ledger:\nbefore %s\nafter  %s", before, after)
	}
	if contests := readerlessContests(w); len(contests) != 1 || !strings.Contains(contests[0], readerlessWhy) {
		t.Errorf("the contest was not recorded once with the readerless text: %q", contests)
	}
}

// Ready with no pull request reader refuses a recorded forge pull request whether the head moved or not, and whether it is
// readiness or only a restated head that is declared.
func TestReaderlessReadyRefusesARecordedForgePullRequest(t *testing.T) {
	for _, c := range []struct {
		name  string
		ready bool
		head  string
	}{
		{"unchanged head declared ready", true, ""},
		{"unchanged head named and declared ready", true, readerlessHead},
		{"changed head restated", false, "head-501"},
		{"changed head restated and declared ready", true, "head-501"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newFx(t)
			if w.m.Pulls != nil {
				t.Fatal("the fixture was meant to have no pull request reader")
			}
			turn := readerlessTurn(w, "", false)
			before := readerlessLane(w)
			answer, err := w.m.Ready(w.ctx, turn, alpha.TaskID, c.ready, c.head, "")
			readerlessRefused(t, w, before, answer, err, "declares", turn)
		})
	}
}

// Check refuses it too, on a claim that was ready from the start: a service that has no reader, and a service that has one
// but is given a check reader of the base only, since the check reads the pull request through the reader it is given.
func TestReaderlessCheckRefusesARecordedForgePullRequest(t *testing.T) {
	for _, c := range []struct {
		name         string
		serviceReads bool
	}{
		{"a service with no pull request reader", false},
		{"a service with a pull request reader and a check reader of the base only", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newFx(t)
			pulls := newLivePullHeads()
			pulls.set(fxRepo, 500, readerlessHead)
			if c.serviceReads {
				w.m.Pulls = pulls
			}
			turn := readerlessTurn(w, "", true)
			before := readerlessLane(w)
			checksBefore := headCompareCount(w, "SELECT COUNT(*) FROM merge_turn_checks")
			answer, err := headCompareCheck(w, turn, readerlessHead, w.target)
			readerlessRefused(t, w, before, answer, err, "restates", turn)
			if got := headCompareCount(w, "SELECT COUNT(*) FROM merge_turn_checks WHERE result = 'refused' AND refusal_reason = 'merge_target_unreadable'"); got != 1 || headCompareCount(w, "SELECT COUNT(*) FROM merge_turn_checks") != checksBefore+1 {
				t.Errorf("the refused check was not recorded as one refused check row: %d", got)
			}
			if live := w.must(w.m.Turn(w.ctx, turn)); live["state"] != Holding {
				t.Errorf("a refused check moved the turn to %v", live["state"])
			}
			if pulls.readCount() != 0 {
				t.Errorf("the service's reader was used by the check: %d reads", pulls.readCount())
			}
		})
	}
}

// A forge pull request keeps the forge as its source of truth: a work report of its relationship does not stand in for the
// reader that is missing, at Ready or at Check, whether the report names the head or another one (the refusal is the
// missing reader's, not merge_candidate_moved).
func TestReaderlessForgePullRequestWithAWorkReportDoesNotDowngrade(t *testing.T) {
	for _, report := range []struct{ name, head string }{{"a report that names the head", readerlessHead}, {"a report that names another head", "head-elsewhere"}} {
		seed := func(w *fx) {
			headCompareRelate(w, headCompareRel)
			headCompareReportOf(w, headCompareRel, "event-a", 1, 3, report.head, fxRepo, 500)
		}
		t.Run(report.name+" at ready", func(t *testing.T) {
			w := newFx(t)
			seed(w)
			turn := readerlessTurn(w, headCompareRel, false)
			before := readerlessLane(w)
			answer, err := w.m.Ready(w.ctx, turn, alpha.TaskID, true, "", "")
			readerlessRefused(t, w, before, answer, err, "declares", turn)
		})
		t.Run(report.name+" at check", func(t *testing.T) {
			w := newFx(t)
			seed(w)
			turn := readerlessTurn(w, headCompareRel, true)
			before := readerlessLane(w)
			answer, err := headCompareCheck(w, turn, readerlessHead, w.target)
			readerlessRefused(t, w, before, answer, err, "restates", turn)
		})
	}
}

// A turn that waits behind another holder is refused the same way, so a waiter is never recorded ready on a pull request
// nobody read.
func TestReaderlessReadyRefusesAWaitingTurn(t *testing.T) {
	w := newFx(t)
	w.claim(alpha, fxA, "head-a")
	waiting := w.must(w.m.Request(w.ctx, fxRepo, fxBase, fxB, beta.TaskID, beta.HostID, readerlessHead, false, ClaimOptions{PR: nullInt(500)}))
	turn := waiting["turnId"].(string)
	if waiting["state"] != Waiting {
		t.Fatalf("the second claim did not wait: %v", waiting["state"])
	}
	before := readerlessLane(w)
	answer, err := w.m.Ready(w.ctx, turn, beta.TaskID, true, "", "")
	readerlessRefused(t, w, before, answer, err, "declares", turn)
}

// The remedy the refusal names is the one that works: the same calls pass once a pull request reader is supplied, decided by
// the forge, on the turn the refusal left as it was.
func TestReaderlessRefusalIsLiftedBySupplyingAReader(t *testing.T) {
	w := newFx(t)
	turn := readerlessTurn(w, "", false)
	before := readerlessLane(w)
	answer, err := w.m.Ready(w.ctx, turn, alpha.TaskID, true, "", "")
	readerlessRefused(t, w, before, answer, err, "declares", turn)

	pulls := newLivePullHeads()
	pulls.set(fxRepo, 500, readerlessHead)
	w.m.Pulls = pulls
	declared := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, true, "", ""))
	headCompareDecision(t, declared, "forge", readerlessHead, 500, "")
	checked, err := headCompareCheck(w, turn, readerlessHead, livePullReader{target: w.target, pulls: pulls})
	if err != nil || checked["state"] != Merging {
		t.Fatalf("%v %v", checked, err)
	}
	headCompareDecision(t, checked, "forge", readerlessHead, 500, "")
}

// What the refusal leaves alone: a withdrawal of readiness on an unchanged head, a claim that records nothing to compare, a
// local relationship decided by its work report, and a reader that can read pull requests.
func TestReaderlessRefusalLeavesTheOtherComparisonsAlone(t *testing.T) {
	t.Run("an unchanged not-ready compares nothing", func(t *testing.T) {
		w := newFx(t)
		turn := readerlessTurn(w, "", false)
		for _, head := range []string{"", readerlessHead} {
			w.exec("UPDATE merge_turns SET declared_ready = 1 WHERE turn_id = ?", turn)
			withdrawn := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, false, head, ""))
			if withdrawn["declaredReady"] != false || withdrawn["pullRequestHead"] != nil {
				t.Errorf("head %q: withdrawing readiness compared a head or kept it declared: %v", head, withdrawn)
			}
		}
	})
	t.Run("a bare claim records nothing to compare", func(t *testing.T) {
		w := newFx(t)
		turn := headCompareHeld(w, fxRepo, "head-a", 0, "", false)
		declared := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, true, "", ""))
		checked, err := headCompareCheck(w, turn, "head-a", w.target)
		if err != nil || declared["declaredReady"] != true || checked["state"] != Merging || declared["pullRequestHead"] != nil || checked["pullRequestHead"] != nil {
			t.Fatalf("a bare claim was compared: %v %v %v", declared, checked, err)
		}
	})
	t.Run("a local relationship is decided by its work report", func(t *testing.T) {
		w := newFx(t)
		w.target.set(headCompareLocal, fxBase, "base-0")
		headCompareRelate(w, headCompareRel)
		headCompareReport(w, headCompareRel, "event-a", 1, 3, readerlessHead)
		turn := headCompareHeld(w, headCompareLocal, readerlessHead, 500, headCompareRel, false)
		declared := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, true, "", ""))
		headCompareDecision(t, declared, "work_report", readerlessHead, 500, headCompareRel)
		checked, err := headCompareCheck(w, turn, readerlessHead, w.target)
		if err != nil || checked["state"] != Merging {
			t.Fatalf("%v %v", checked, err)
		}
		headCompareDecision(t, checked, "work_report", readerlessHead, 500, headCompareRel)
	})
	t.Run("a reader that can read pull requests decides by the forge", func(t *testing.T) {
		w := newFx(t)
		pulls := newLivePullHeads()
		pulls.set(fxRepo, 500, readerlessHead)
		w.m.Pulls = pulls
		turn := readerlessTurn(w, "", false)
		declared := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, true, "", ""))
		headCompareDecision(t, declared, "forge", readerlessHead, 500, "")
		checked, err := headCompareCheck(w, turn, readerlessHead, livePullReader{target: w.target, pulls: pulls})
		if err != nil || checked["state"] != Merging {
			t.Fatalf("%v %v", checked, err)
		}
		headCompareDecision(t, checked, "forge", readerlessHead, 500, "")
		if pulls.readCount() != 2 {
			t.Errorf("%d forge reads for Ready and Check", pulls.readCount())
		}
	})
	t.Run("a reader whose forge names another head refuses by the forge", func(t *testing.T) {
		w := newFx(t)
		pulls := newLivePullHeads()
		pulls.set(fxRepo, 500, "head-500-moved")
		w.m.Pulls = pulls
		turn := readerlessTurn(w, "", false)
		answer, err := w.m.Ready(w.ctx, turn, alpha.TaskID, true, "", "")
		if answer != nil {
			t.Fatalf("a head that is not the pull request's was declared ready: %v", answer)
		}
		headCompareRefused(t, err, "merge_candidate_moved", turn, "head-500-moved")
	})
}
