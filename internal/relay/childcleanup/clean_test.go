package childcleanup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// the threads of one finished child: the child, a sub-thread with a sub-thread of its own, a sub-thread below an intermediate that is not loaded, and a second tree that is not the child's.
func family() []thread {
	return []thread{
		{id: "child", loaded: true, rollout: true},
		{id: "sub-1", parent: "child", loaded: true, rollout: true},
		{id: "sub-2", parent: "sub-1", loaded: true, rollout: true},
		{id: "mid", parent: "child", rollout: true},
		{id: "grand", parent: "mid", loaded: true, rollout: true},
		{id: "other", loaded: true, rollout: true},
		{id: "other-sub", parent: "other", loaded: true, rollout: true},
	}
}

func run(t *testing.T, s *scripted, opts Options) (Report, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return Clean(ctx, s.client(t), "child", opts)
}

// outcomes is "thread:outcome" for each item, in the order handled.
func outcomes(r Report) string {
	var out []string
	for _, item := range r.Items {
		out = append(out, item.ThreadID+":"+item.Outcome)
	}
	return strings.Join(out, " ")
}

func TestCleanReleasesTheChildAndItsSubThreadsDeepestFirst(t *testing.T) {
	for _, pageSize := range []int{0, 2} {
		s := newScripted(t, family()...)
		s.pageSize = pageSize
		report, err := run(t, s, Options{})
		if got := outcomes(report); err != nil || !report.Complete() || got != "grand:archived sub-2:archived sub-1:archived child:archived" {
			t.Fatalf("page %d: err=%v outcomes=%q: deepest first, the child last, nothing of the other tree", pageSize, err, got)
		}
		if report.Items[0].ParentID != "mid" || s.includeTurn != 0 {
			t.Fatalf("items %+v; includeTurns calls %d (it materializes a never-run thread's rollout and is unsupported on ephemeral ones)", report.Items, s.includeTurn)
		}
		if again, err := run(t, s, Options{}); err != nil || !again.Complete() || len(again.Items) != 0 || len(s.archived()) != 4 {
			t.Fatalf("a repeat changes nothing: err=%v report=%+v archives=%v", err, again, s.archived())
		}
	}
}

func TestCleanReleasesSubThreadsOfAChildThatIsAlreadyUnloaded(t *testing.T) {
	threads := family()
	threads[0].loaded = false
	s := newScripted(t, threads...)
	report, err := run(t, s, Options{})
	if got := outcomes(report); err != nil || !report.Complete() || got != "grand:archived sub-2:archived sub-1:archived" {
		t.Fatalf("err=%v outcomes=%q: only loaded threads are touched, so the unloaded child is left alone", err, got)
	}
}

func TestCleanHoldsTheWholeSubtreeWhileAnythingInItIsActive(t *testing.T) {
	threads := family()
	threads[2].status = "active"
	s := newScripted(t, threads...)
	report, err := run(t, s, Options{})
	if got := outcomes(report); err != nil || report.Complete() || len(s.archived()) != 0 || strings.Count(got, OutcomeHeldActive) != 4 || !strings.Contains(report.Items[0].Detail, "sub-2") {
		t.Fatalf("err=%v outcomes=%q archived=%v", err, got, s.archived())
	}
}

func TestCleanArchivesNothingWhenDiscoveryIsNotExact(t *testing.T) {
	unreadable := family()
	unreadable[2].unreadable = true // may be an active descendant: archiving its ancestor would unload it too
	cycle := append(family(), thread{id: "loop-a", parent: "loop-b", loaded: true, rollout: true}, thread{id: "loop-b", parent: "loop-a", loaded: true, rollout: true})
	noStatus, otherThread := family(), family()
	noStatus[2].malformed, otherThread[2].malformed = "status", "id" // an answer with no status, or for another thread, is no evidence the thread is idle
	for name, threads := range map[string][]thread{"an unreadable thread": unreadable, "a parent cycle": cycle, "an answer without a status": noStatus, "an answer for another thread": otherThread} {
		s := newScripted(t, threads...)
		if report, err := run(t, s, Options{}); err != nil || report.Complete() || len(report.Unresolved) == 0 || len(s.archived()) != 0 {
			t.Fatalf("%s: err=%v report=%+v archived=%v", name, err, report, s.archived())
		}
	}
	for name, limit := range map[string]*int{"the page cap": &maxLoadedPages, "the read cap": &maxReads} {
		saved := *limit
		*limit = 2
		s := newScripted(t, family()...)
		s.pageSize = 1
		_, err := run(t, s, Options{})
		*limit = saved
		if err == nil || len(s.archived()) != 0 {
			t.Fatalf("%s: err=%v archived=%v, want an error and no archive", name, err, s.archived())
		}
	}
}

func TestCleanTakesTheNoRolloutPath(t *testing.T) {
	threads := family()
	threads[1].rollout = false // sub-1 never ran a turn: archive refuses it and it stays loaded
	s := newScripted(t, threads...)
	report, err := run(t, s, Options{})
	if got := outcomes(report); err != nil || report.Complete() || got != "grand:archived sub-2:archived sub-1:no_rollout_left_loaded child:archived" || s.srv.Count("thread/delete") != 0 || !strings.Contains(report.Items[2].Detail, "owner") {
		t.Fatalf("err=%v outcomes=%q: report it with what releases it, leave it, never delete", err, got)
	}
	// the host unloaded it meanwhile (an ancestor's archive did): nothing is left to do
	threads = family()
	threads[1].rollout, threads[1].unloadWhenRefused = false, true
	if report, err = run(t, newScripted(t, threads...), Options{}); err != nil || !report.Complete() || !strings.Contains(outcomes(report), "sub-1:"+OutcomeReleasedByAncestor) {
		t.Fatalf("err=%v outcomes=%q", err, outcomes(report))
	}
}

func TestCleanReconcilesANeverRunThreadTakenByAnAncestor(t *testing.T) {
	threads := family()
	threads[1].rollout, threads[1].unloadsWith = false, "child" // refused while loaded, then unloaded by the archive of the child
	report, err := run(t, newScripted(t, threads...), Options{})
	if got := outcomes(report); err != nil || !report.Complete() || got != "grand:archived sub-2:archived sub-1:released_by_ancestor child:archived" {
		t.Fatalf("err=%v outcomes=%q", err, got)
	}
}

func TestCleanStopsOnATransportFailureInsteadOfCallingItARefusal(t *testing.T) {
	threads := family()
	threads[1].dropOnArchive = true
	report, err := run(t, newScripted(t, threads...), Options{})
	if got := outcomes(report); err == nil || report.Complete() || report.Stopped == "" || got != "grand:archived sub-2:archived" {
		t.Fatalf("err=%v outcomes=%q stopped=%q: the unknown request ends the cleanup with the report so far", err, got, report.Stopped)
	}
}

func TestCleanReportsAFailureAndGoesOn(t *testing.T) {
	threads := family()
	threads[1].refuse = "boom"
	report, err := run(t, newScripted(t, threads...), Options{})
	if got := outcomes(report); err != nil || report.Complete() || got != "grand:archived sub-2:archived sub-1:failed child:archived" || !strings.Contains(report.Items[2].Detail, "boom") {
		t.Fatalf("err=%v outcomes=%q", err, got)
	}
}

func TestCleanDryRunAndRecheckMutateNothing(t *testing.T) {
	s := newScripted(t, family()...)
	changed := errors.New("the relationship changed")
	_, err := run(t, s, Options{Recheck: func(context.Context) error { return changed }})
	if report, err2 := run(t, s, Options{DryRun: true}); !errors.Is(err, changed) || err2 != nil || !report.Complete() || strings.Count(outcomes(report), OutcomePlanned) != 4 || len(s.archived()) != 0 {
		t.Fatalf("recheck err=%v, dry run err=%v report=%+v archived=%v", err, err2, report, s.archived())
	}
}

func TestCleanJudgesTheSubtreeAgainRightBeforeTheFirstArchive(t *testing.T) {
	s := newScripted(t, family()...)
	// a thread of the subtree starts running right after the second listing
	started := func() { s.mu.Lock(); s.byID["sub-2"].status = "active"; s.mu.Unlock() }
	report, err := Clean(context.Background(), &afterCall{Host: s.client(t), method: "thread/loaded/list", n: 2, then: started}, "child", Options{})
	if got := outcomes(report); err != nil || report.Complete() || len(s.archived()) != 0 || strings.Count(got, OutcomeHeldActive) != 4 {
		t.Fatalf("err=%v outcomes=%q archived=%v", err, got, s.archived())
	}
}

func TestCleanKeepsWhatItDidWhenTheContextEnds(t *testing.T) {
	s := newScripted(t, family()...)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	report, err := Clean(ctx, &afterCall{Host: s.client(t), method: "thread/archive", then: cancel}, "child", Options{})
	if !errors.Is(err, context.Canceled) || outcomes(report) != "grand:archived" || report.Complete() || report.Stopped == "" {
		t.Fatalf("err=%v report=%+v: the first archive is reported with the error, and the cleanup is not complete", err, report)
	}
	_, err = answerOf("rel", report, err, false)
	var exit *dispatch.PayloadExit
	if !errors.As(err, &exit) || exit.Code != contract.ExitHost || !strings.Contains(fmt.Sprint(exit.Payload), "{ok false}") || !strings.Contains(fmt.Sprint(exit.Payload), "{complete false}") {
		t.Fatalf("the answer is exit %d with ok and complete false and the report: %v", contract.ExitHost, err)
	}
}
