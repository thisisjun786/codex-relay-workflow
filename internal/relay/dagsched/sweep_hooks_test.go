package dagsched

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The sweeps a landing and an accepted receipt owe (CRW-410, criterion c1): run after the command's own transaction, best effort, once per landing and per acceptance, and run again by a repeat of the command
// when the first one failed before it wrote anything.

func TestLandingRunsTheSweepItOwes(t *testing.T) {
	w := newSweepWorld(t)
	k := w.k
	k.sched.Checkout = k.repo.path
	w.accept("D", "E")
	k.mark(k.acceptNode("g", "I", acceptOpts{HeadSHA: w.head["I"], PR: 30, Forge: "owner/repo", Repository: k.repo.path}))

	// the landing is recorded and the sweep dies inside its transaction: the landing stands and says so, nothing of the sweep was written
	k.sched.testInSweepTx = func() error { return errors.New("the process died inside the sweep") }
	res, err := k.observe()
	if err != nil || !res.Integrated || res.Sweep == nil || res.Sweep.State != SweepFailed || !strings.Contains(res.Sweep.Reason, "died") {
		t.Fatalf("landing = %v %+v sweep=%+v", err, res, res.Sweep)
	}
	if got := k.count("SELECT COUNT(*) FROM dag_conflict_sweeps"); got != 0 {
		t.Fatalf("a failed sweep left %d rows", got)
	}
	// the repeat of the same command is a replay of the observation, and runs the sweep it still owes
	k.sched.testInSweepTx = nil
	again, err := k.observe()
	if err != nil || len(again.Observations) != 1 || !again.Observations[0].Replayed || again.Sweep == nil || again.Sweep.State != SweepRecorded {
		t.Fatalf("repeat = %v %+v sweep=%+v", err, again, again.Sweep)
	}
	sweep := again.Sweep
	if sweep.Trigger != TriggerLanding || sweep.TriggerNode != "I" || sweep.TriggerRef != again.Observations[0].ObservationID || sweep.Repository != k.repo.path || len(sweep.Members) != 3 {
		t.Fatalf("sweep = %+v", sweep)
	}
	if m := memberOf(*sweep, MemberPair, "D", "E"); m.Status != MemberObserved || m.Conflicts != 1 {
		t.Errorf("D-E = %+v", m)
	}
	for _, node := range []string{"D", "E"} {
		if m := memberOf(*sweep, MemberTip, node, ""); m.Status != MemberObserved || m.Conflicts != 1 || m.RightHead != w.tip {
			t.Errorf("%s against the tip = %+v", node, m)
		}
	}
	// the landed node is not a live head, and a third call finds the sweep it owes done
	if got := k.count("SELECT COUNT(*) FROM dag_conflict_sweep_members WHERE left_node_id = 'I' OR right_node_id = 'I'"); got != 0 {
		t.Errorf("the landed node was measured: %d", got)
	}
	third, err := k.observe()
	if err != nil || third.Sweep == nil || third.Sweep.State != SweepAlready || third.Sweep.Seq != sweep.Seq || k.count("SELECT COUNT(*) FROM dag_conflict_sweeps") != 1 {
		t.Fatalf("third = %v %+v", err, third.Sweep)
	}
}

// A head that did not land owes no sweep, and a landing with no checkout to measure in skips it, says so, and records the landing all the same.
func TestLandingWithNothingToSweep(t *testing.T) {
	w := newSweepWorld(t)
	k := w.k
	w.accept("D")
	res, err := k.sched.ObserveIntegration(context.Background(), "g", "D", "parent", []Target{{Repository: k.repo.path, BaseRef: "dev"}})
	if err != nil || res.Integrated || res.Sweep != nil {
		t.Fatalf("a head that did not land = %v %+v %+v", err, res, res.Sweep)
	}
	k.mark(k.acceptNode("g", "I", acceptOpts{HeadSHA: w.head["I"], PR: 30, Forge: "owner/repo", Repository: k.repo.path}))
	landed, err := k.observe()
	if err != nil || !landed.Integrated || landed.Sweep == nil || landed.Sweep.State != SweepSkipped || landed.Sweep.Reason != "no_checkout" || k.count("SELECT COUNT(*) FROM dag_conflict_sweeps") != 0 {
		t.Fatalf("landing with no checkout = %v %+v %+v", err, landed, landed.Sweep)
	}
	// the parent's checkout is found from the relationship's parent_cwd
	k.exec("UPDATE relationships SET parent_cwd = ?", k.repo.path)
	if again, err := k.observe(); err != nil || again.Sweep == nil || again.Sweep.State != SweepRecorded || again.Sweep.Repository != k.repo.path {
		t.Fatalf("repeat with parent_cwd = %v %+v", err, again.Sweep)
	}
}

// Taking a receipt in is the other trigger: the sweep rests on the acceptance, measures the pull request's head with the other live heads and against the tip of its base branch, and a repeat of the
// acceptance finds it done.
func TestAcceptRunsTheSweepOfAReceipt(t *testing.T) {
	w := newSweepWorld(t)
	k := w.k
	k.sched.Checkout = k.repo.path
	k.sched.Tips = &tips{sha: w.tip}
	k.sched.PRs = k.forge.read
	w.accept("E")
	k.reportNode("g", "D", acceptOpts{})
	k.forge.by["owner/repo#11"] = openPR("owner/repo", 11, w.head["D"])
	res, err := k.accept("g", "D", AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 11}})
	if err != nil || res.AcceptanceID == "" || res.Sweep == nil || res.Sweep.State != SweepRecorded {
		t.Fatalf("accept = %v %+v sweep=%+v", err, res, res.Sweep)
	}
	sweep := res.Sweep
	if sweep.Trigger != TriggerReceipt || sweep.TriggerNode != "D" || sweep.TriggerRef != res.AcceptanceID || len(sweep.Members) != 3 {
		t.Fatalf("sweep = %+v", sweep)
	}
	if m := memberOf(*sweep, MemberPair, "D", "E"); m.Status != MemberObserved || m.Conflicts != 1 || m.LeftHead != w.head["D"] || m.LeftSource != HeadAcceptance {
		t.Errorf("D-E = %+v", m)
	}
	if m := memberOf(*sweep, MemberTip, "D", ""); m.Status != MemberObserved || m.Conflicts != 1 || m.RightHead != w.tip {
		t.Errorf("D against the tip = %+v", m)
	}
	again, err := k.accept("g", "D", AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 11}})
	if err != nil || !again.Replayed || again.Sweep == nil || again.Sweep.State != SweepAlready || k.count("SELECT COUNT(*) FROM dag_conflict_sweeps WHERE trigger_kind = 'receipt'") != 1 {
		t.Fatalf("repeat = %v %+v sweep=%+v", err, again, again.Sweep)
	}
	// a node with no pull request (non_pr) has no head, and no sweep
	k.reportNode("g", "K", acceptOpts{})
	if res, err := k.accept("g", "K", AcceptInput{}); err != nil || res.Sweep != nil {
		t.Fatalf("a non_pr acceptance = %v %+v", err, res.Sweep)
	}
}

// A head that lands on a second target later owes a sweep for that landing too: the sweep rests on every observation of a landing, so the new one is a new trigger, and a repeat of the same call finds it done.
func TestLandingOnASecondTargetIsSweptToo(t *testing.T) {
	w := newSweepWorld(t)
	k := w.k
	k.sched.Checkout = k.repo.path
	w.accept("D", "E")
	k.mark(k.acceptNode("g", "I", acceptOpts{HeadSHA: w.head["I"], PR: 30, Forge: "owner/repo", Repository: k.repo.path}))
	k.repo.git("branch", "side", w.base) // side does not hold the head yet
	targets := []Target{{Repository: k.repo.path, BaseRef: "dev"}, {Repository: k.repo.path, BaseRef: "side"}}
	first, err := k.observe(targets...)
	if err != nil || first.Sweep == nil || first.Sweep.State != SweepRecorded || len(first.Sweep.Members) != 6 { // I has not landed on every target yet: it is still a live head
		t.Fatalf("first = %v %+v", err, first.Sweep)
	}
	k.repo.git("checkout", "-q", "side")
	k.repo.git("merge", "-q", "--no-ff", "-m", "land the head on side", "b-I")
	k.repo.git("checkout", "-q", "dev")
	second, err := k.observe(targets...)
	if err != nil || second.Sweep == nil || second.Sweep.State != SweepRecorded || second.Sweep.TriggerRef == first.Sweep.TriggerRef || len(second.Sweep.Members) != 5 { // landed everywhere: I is no longer live; the pair D-E and D and E against both tips
		t.Fatalf("second = %v %+v", err, second.Sweep)
	}
	if !second.Observations[0].Replayed || second.Observations[1].Replayed {
		t.Fatalf("observations = %+v", second.Observations)
	}
	third, err := k.observe(targets...)
	if err != nil || third.Sweep == nil || third.Sweep.State != SweepAlready || third.Sweep.Seq != second.Sweep.Seq || k.count("SELECT COUNT(*) FROM dag_conflict_sweeps WHERE trigger_kind = 'landing'") != 2 {
		t.Fatalf("third = %v %+v", err, third.Sweep)
	}
}
