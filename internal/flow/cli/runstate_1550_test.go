package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// TestCRW1550LandReportsWriterHeldEndedRun (CRW-1550 item 1): while a writer (audit) holds the lock of an ended run, the land
// report reads the run's state from its rows, not from the lock: land_node_held names the ended run's real state (failed), and
// the guidance names crw flow cancel, not waiting for a "running" run to complete.
func TestCRW1550LandReportsWriterHeldEndedRun(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	l.outsideHead("A-1", "outside-a1")
	author := l.authorThread("claude-fake-sonnet", "high")
	l.killAt = l.writes + 1
	run1 := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	if _, killed := l.process(run1.RunID, false); !killed {
		t.Fatal("the first land was not killed")
	}
	l.killAt = 0
	l.endRun(run1.RunID)

	run2 := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	// a writer holds run1's lock for a moment while run2's land reads its state
	held, err := TakeLock(l.w.state, run1.RunID, "audit", l.td.Now())
	if err != nil {
		t.Fatal(err)
	}
	v := landValueOf(t, l.runToEnd(run2.RunID))
	held.Release()
	if v.State != "unreleased" || v.Reason != "land_node_held" {
		t.Fatalf("a land of a node an ended run holds: %+v", v)
	}
	rows := l.releaseRowsOf(l.nodeOf["A-1"])
	if len(rows) != 1 || str(rows[0].Data, "held_run") != run1.RunID || str(rows[0].Data, "held_run_state") != StateFailed {
		t.Fatalf("the release row of the held land: %+v", rows)
	}
	res, _ := ReadResult(l.w.state, run2.RunID)
	if !strings.Contains(string(res.Value), "crw flow cancel "+run1.RunID) || strings.Contains(string(res.Value), "crw flow resume "+run1.RunID) {
		t.Errorf("the land names the command that gives the ended run's execution up: %s", res.Value)
	}
}

// TestCRW1550AwaitLockRejectsUnrelatedEvidence (CRW-1550 item 2): a run process's start is known from its own durable start, never
// from a lock any process happens to hold or a fresh result file.
func TestCRW1550AwaitLockRejectsUnrelatedEvidence(t *testing.T) {
	gone := func(t *testing.T) int {
		t.Helper()
		cmd := exec.Command("true")
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		return cmd.Process.Pid
	}
	t.Run("another process's run lock", func(t *testing.T) {
		w := newWorld(t)
		w.newRun("run-a", "thread-owner").close()
		l := holdLock(t, w, "run-a", ProcessCommand)
		defer l.Release()
		if err := AwaitLock(ctx, w.state, "run-a", gone(t), time.Now()); err == nil || !strings.Contains(err.Error(), "ended before") {
			t.Fatalf("a run lock the test pid holds is no start of another process: %v", err)
		}
	})
	t.Run("a fresh result without a start", func(t *testing.T) {
		w := newWorld(t)
		w.newRun("run-b", "thread-owner").close()
		if err := os.MkdirAll(RunDir(w.state, "run-b"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeResult(w.state, "run-b", &engine.Result{RunID: "run-b", Status: engine.RunCompleted, Value: json.RawMessage("1")}, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := AwaitLock(ctx, w.state, "run-b", gone(t), time.Now()); err == nil || !strings.Contains(err.Error(), "ended before") {
			t.Fatalf("a fresh result is no start: %v", err)
		}
	})
	t.Run("a lock held where the run was cancelled", func(t *testing.T) {
		w := newWorld(t)
		r := w.newRun("run-c", "thread-owner")
		r.finish(engine.RunScriptError, engine.ErrKindCancelled)
		r.close()
		l := holdLock(t, w, "run-c", ProcessCommand)
		defer l.Release()
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := AwaitLock(cctx, w.state, "run-c", os.Getpid(), time.Now()); err == nil {
			t.Fatal("the test pid's lock over a cancelled end is no start")
		}
	})
}

// TestCRW1550DetachedResumeDoesNotAnnounceRefusedStart (CRW-1550 item 2): a detached resume reports only the start of the process
// it spawned: one that loses a race with cancel is refused, never announced as `resumed (process N)`.
func TestCRW1550DetachedResumeDoesNotAnnounceRefusedStart(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	run := "run-resume-refused"
	if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: run, Workflow: "hello", Script: helloScript(t), Thread: "thread-owner", CreatedAt: "2026-10-12T08:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	r := w.newRun(run, "thread-owner")
	r.endPaused()
	r.close()
	startedBefore := countStarted(t, w, run)
	if startedBefore != 1 {
		t.Fatalf("the resumable run fixture: %d started rows", startedBefore)
	}
	// the child the resume spawns loses the race: a cancel ends the run before it refuses it under the lock it then takes
	var child *Lock
	td.Spawn = func(ctx context.Context, req SpawnRequest) (int, error) {
		if code := RunWith(ctx, td.Deps, []string{"cancel", req.Spec.RunID, "--state", req.State}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
			t.Errorf("the cancel inside the spawned window: %d", code)
		}
		l, err := TakeLock(req.State, req.Spec.RunID, ProcessCommand, td.Now())
		if err != nil {
			t.Fatalf("the refused child's run lock: %v", err)
		}
		child = l
		return os.Getpid(), nil
	}
	// the refused child's cancellation check: an already cancelled context, so the judgment is immediate and deterministic
	td.Ready = func(ctx context.Context, state, run string, pid int, since time.Time) error {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		return AwaitLock(cctx, state, run, pid, since)
	}
	code, out, errOut := td.do("resume", run, "--detach", "--state", w.state)
	child.Release()
	if code == 0 || strings.Contains(out, "resumed (process") {
		t.Fatalf("a resume whose process refused the run: exit %d out %q err %q", code, out, errOut)
	}
	if n := countStarted(t, w, run); n != startedBefore {
		t.Errorf("the refused resume recorded %d started rows (was %d)", n, startedBefore)
	}
}

func countStarted(t *testing.T, w *world, run string) int {
	t.Helper()
	rows, found, err := loadRun(w, run)
	if err != nil || !found {
		t.Fatalf("load run %s: found %v, error %v", run, found, err)
	}
	n := 0
	for _, ev := range rows {
		if ev.RunID == run && ev.Kind == engine.KindRun && ev.Event == "started" {
			n++
		}
	}
	return n
}

// TestCRW1550ReadyRefusalKeepsResumedZero (CRW-1550 item 2): the answers of answer and next name a resumed pid only when the start
// is acknowledged; when the injected Ready refuses, resumed_pid stays 0.
func TestCRW1550ReadyRefusalKeepsResumedZero(t *testing.T) {
	t.Run("answer", func(t *testing.T) {
		w := newWorld(t)
		a := newLiveAsking(t, w)
		td := w.deps()
		id := a.answered(t, "ud-ready-refused", nil)
		td.Ready = func(ctx context.Context, state, run string, pid int, since time.Time) error {
			return errors.New("the start was not acknowledged")
		}
		code, reply, errOut := a.answer(td, id)
		if code != 1 || !reply.Applied || reply.Resumed != 0 || len(td.spawned) != 1 {
			t.Fatalf("an applied answer whose resume's start was refused: exit %d %+v %s", code, reply, errOut)
		}
	})
	t.Run("next", func(t *testing.T) {
		w := newWorld(t)
		td := w.deps()
		run := "run-0123456789d9"
		sessionSpec(t, w, run)
		w.newRun(run, sessOwner).close()
		td.Ready = func(ctx context.Context, state, run string, pid int, since time.Time) error {
			return errors.New("the start was not acknowledged")
		}
		ans := nextOf(t, td, run)
		if ans.Resumed != 0 || len(td.spawned) != 1 {
			t.Fatalf("next announced resumed_pid %d for a refused start: %+v", ans.Resumed, ans)
		}
	})
}

// TestCRW1550TailSeparatesWriterBarrierFromState (CRW-1550 item 3): Tail's live is the commit barrier (any holder, a writer too)
// while its state is the run-process liveness: a dead or cancelled run reports its own state while a writer holds the lock.
func TestCRW1550TailSeparatesWriterBarrierFromState(t *testing.T) {
	w := newWorld(t)
	w.newRun("run-dead", "thread-owner").close()
	c := w.newRun("run-cancelled", "thread-owner")
	c.finish(engine.RunScriptError, engine.ErrKindCancelled)
	c.close()
	live := w.newRun("run-live", "thread-owner")
	live.close()
	rd, err := OpenReader(ctx, w.state, "")
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	for _, tc := range []struct {
		run, state string
	}{
		{"run-dead", StateDead},
		{"run-cancelled", StateCancelled},
	} {
		for _, cmd := range writerCommands {
			l := holdLock(t, w, tc.run, cmd)
			p, err := rd.Tail(ctx, tc.run, 0, 10, 0)
			l.Release()
			if err != nil || !p.Live || p.State != tc.state {
				t.Fatalf("%s held by %s: live %v state %q err %v, want live, %s", tc.run, cmd, p.Live, p.State, err, tc.state)
			}
			p, err = rd.Tail(ctx, tc.run, 0, 10, 0)
			if err != nil || p.Live || p.State != tc.state {
				t.Fatalf("%s released: live %v state %q err %v, want not live, %s", tc.run, p.Live, p.State, err, tc.state)
			}
		}
	}
	l := holdLock(t, w, "run-live", ProcessCommand)
	p, err := rd.Tail(ctx, "run-live", 0, 10, 0)
	l.Release()
	if err != nil || !p.Live || p.State != StateRunning {
		t.Fatalf("a live run: live %v state %q err %v", p.Live, p.State, err)
	}
}

// TestCRW1550NextResumesAnAlreadyAppliedAnswer (CRW-1550 item 4): when the run ended paused for an answer whose row was applied
// after that end (--no-resume, or a resume that failed), next says crw flow resume RUN, not to replay the answer.
func TestCRW1550NextResumesAnAlreadyAppliedAnswer(t *testing.T) {
	// setup builds the live asking run as a session run with a drained paused end, then runs the answer with extra args
	setup := func(t *testing.T, spawnErr bool, extra ...string) (*testDeps, NextAnswer) {
		w := newWorld(t)
		td := w.deps()
		a := newLiveAsking(t, w)
		// next reads a session run's spec: the asking fixture's is rewritten as one
		_ = os.Remove(specPath(w.state, "run-q"))
		if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: "run-q", Workflow: "project-lane", Thread: "thread-owner", Backend: BackendSession, CreatedAt: "2026-10-12T08:00:00Z"}); err != nil {
			t.Fatal(err)
		}
		id := a.answered(t, "ud-applied-after", nil)
		j, err := openJournal(w, "run-q")
		if err != nil {
			t.Fatal(err)
		}
		appendDrainedEnd(j)
		if err := j.Close(); err != nil {
			t.Fatal(err)
		}
		if spawnErr {
			td.Spawn = func(ctx context.Context, req SpawnRequest) (int, error) { return 0, errors.New("no spawn") }
		}
		code, reply, errOut := a.answer(td, id, extra...)
		if code == 2 || !reply.Applied || reply.Refusal != nil {
			t.Fatalf("the answer: exit %d %+v %s", code, reply, errOut)
		}
		return td, nextOf(t, td, "run-q")
	}
	assertHint := func(t *testing.T, td *testDeps, ans NextAnswer) {
		t.Helper()
		hint := bare(ans.Hint)
		if ans.State != NextPaused || ans.Resumed != 0 || len(td.spawned) != 0 ||
			!strings.Contains(hint, "already applied") || !strings.Contains(hint, "flow resume run-q") || strings.Contains(hint, "flow answer again") {
			t.Fatalf("the hint of an answer already applied after the paused end: %+v\nhint: %s", ans, hint)
		}
	}
	t.Run("applied with --no-resume", func(t *testing.T) {
		td, ans := setup(t, false, "--no-resume")
		assertHint(t, td, ans)
	})
	t.Run("applied but the resume failed", func(t *testing.T) {
		td, ans := setup(t, true)
		assertHint(t, td, ans)
	})
	t.Run("an application before the paused end keeps the retry hint", func(t *testing.T) {
		w := newWorld(t)
		td := w.deps()
		a := newLiveAsking(t, w)
		// next reads a session run's spec: the asking fixture's is rewritten as one
		_ = os.Remove(specPath(w.state, "run-q"))
		if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: "run-q", Workflow: "project-lane", Thread: "thread-owner", Backend: BackendSession, CreatedAt: "2026-10-12T08:00:00Z"}); err != nil {
			t.Fatal(err)
		}
		id := a.answered(t, "ud-applied-before", nil)
		code, reply, errOut := a.answer(td, id, "--no-resume")
		if code != 0 || !reply.Applied {
			t.Fatalf("the answer: exit %d %+v %s", code, reply, errOut)
		}
		j, err := openJournal(w, "run-q")
		if err != nil {
			t.Fatal(err)
		}
		appendDrainedEnd(j)
		if err := j.Close(); err != nil {
			t.Fatal(err)
		}
		ans := nextOf(t, td, "run-q")
		if ans.State != NextPaused || !strings.Contains(bare(ans.Hint), "flow answer again") {
			t.Fatalf("an application before the paused end keeps the answer hint: %+v\nhint: %s", ans, ans.Hint)
		}
	})
}

// TestCRW1550DrainGuidanceHandlesEarlierStop (CRW-1550 item 5): a drain signalled after the run already stops for another reason
// is a no-op by design, and the run_draining guidance says what to do instead of waiting for a paused end that never comes.
func TestCRW1550DrainGuidanceHandlesEarlierStop(t *testing.T) {
	for _, by := range []string{"deadline", "operator"} {
		t.Run(by, func(t *testing.T) {
			w := newWorld(t)
			a := newLiveAsking(t, w)
			td := w.deps()
			id := a.answered(t, "ud-drain-"+by, nil)
			held, err := TakeLock(w.state, "run-q", ProcessCommand, td.Now())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(held.Release)
			j, err := openJournal(w, "run-q")
			if err != nil {
				t.Fatal(err)
			}
			j.Append(engine.Event{RunID: "run-q", Kind: engine.KindFlow, Event: engine.EvRunPaused, Data: map[string]any{"by": by, "reason": "earlier stop"}})
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			td.Signal = func(pid int, sig syscall.Signal) error { return nil }
			code, reply, errOut := a.answer(td, id, "--wait", "0")
			if code != 1 || reply.Refusal == nil || reply.Refusal.Code != RefusedRunDraining || !reply.DrainAsked || reply.Drained ||
				reply.Applied || len(a.appliedRows(t)) != 0 || len(td.spawned) != 0 {
				t.Fatalf("the no-op drain: exit %d %+v %s", code, reply, errOut)
			}
			detail := reply.Refusal.Detail
			for _, want := range []string{"no-op", "crw flow show", "crw flow resume run-q", "already applied"} {
				if !strings.Contains(detail, want) {
					t.Errorf("the guidance does not name %q: %s", want, detail)
				}
			}
			// the run ends as the earlier stop said, not as an answer pause: a deadline ends it completed
			// (held for its open question), an operator pause ends it paused
			wantState, wantNext := StatePaused, NextPaused
			j, err = openJournal(w, "run-q")
			if err != nil {
				t.Fatal(err)
			}
			if by == "deadline" {
				j.Append(engine.Event{RunID: "run-q", Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": engine.RunCompleted}})
				wantState, wantNext = StateHeld, NextEnded // the unanswered question keeps it held
			} else {
				j.Append(engine.Event{RunID: "run-q", Kind: engine.KindRun, Event: "paused", Data: map[string]any{"status": engine.RunPaused, "by": by, "reason": "earlier stop"}})
			}
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			held.Release()
			rd, err := OpenReader(ctx, w.state, "")
			if err != nil {
				t.Fatal(err)
			}
			info, err := rd.Info(ctx, a.rec)
			_ = rd.Close()
			if err != nil || info.State != wantState || nextState(info, false, nil) != wantNext {
				t.Fatalf("the run's end is %q's, not an answer pause: %+v %v", by, info, err)
			}
		})
	}
}

// TestCRW1550DifferentQuestionAnswerDoesNotDrainNewResume (CRW-1550 item 6): an answer applied while this answer waited - to
// another question, of another node - protects the resume that consumed it: B is refused run_live and that resume is never
// signalled. An application already in the first snapshot does not block a deliberate drain.
func TestCRW1550DifferentQuestionAnswerDoesNotDrainNewResume(t *testing.T) {
	secondQuestion := func(t *testing.T, a *asking) {
		r2, err := a.w.reopen("run-q")
		if err != nil {
			t.Fatal(err)
		}
		r2.call("c_ex2", "executor", "n2", 1, []string{engine.EvPrepared, engine.EvRefused}, map[string]map[string]any{engine.EvRefused: {"reason": "pair_refused"}})
		r2.flow(engine.EvEscalation, map[string]any{"reason": "pair_refused", "class": "configuration_fault", "node": "n2", "call_id": "c_ex2", "escalation_id": "esc_two", "ask": "configuration"})
		r2.close()
	}
	t.Run("a different question's answer blocks the new holder's drain", func(t *testing.T) {
		w := newWorld(t)
		a := newLiveAsking(t, w)
		secondQuestion(t, a)
		td := w.deps()
		b := a.answered(t, "ud-b", nil)
		a.answered(t, "ud-a", func(bind *Binding, _ *decisions.Record, ans *decisions.Answer) {
			bind.Node, bind.Question, bind.Attempt = "n2", "esc_two", 1
			ans.Option = "stop"
		})
		p1, err := TakeLock(w.state, "run-q", ProcessCommand, td.Now())
		if err != nil {
			t.Fatal(err)
		}
		const n2pid = 999995
		var n2 *Lock
		fake := newDrainFake(t, w)
		fake.on = func(n, pid int) {
			if n != 0 {
				return
			}
			fake.write(func(j *journal.Journal, _ []engine.Event) { appendDrainedEnd(j) })
			p1.Release()
			hold, err := TakeLock(w.state, "run-q", "answer", td.Now())
			if err != nil {
				t.Fatal(err)
			}
			recA := fake.decisionRecord("ud-a")
			fake.write(func(j *journal.Journal, events []engine.Event) {
				app, refusal := Judge(recA, events, a.rec, Claim{Decision: "ud-a", Run: "run-q", Node: "n2", Question: "esc_two", Revision: 1, By: "thread-owner"})
				if refusal != nil || app.Replayed {
					t.Fatalf("answer A: %+v %+v", app, refusal)
				}
				j.Append(engine.Event{RunID: "run-q", Kind: engine.KindFlow, Event: ops.EvDecisionApplied, Data: app.Data})
			})
			hold.Release()
			n2 = holdByHand(t, w.state, "run-q", n2pid, ProcessCommand, "2026-10-12T09:00:07Z")
		}
		td.Signal = fake.signal
		code, reply, errOut := a.answer(td, b, "--wait", "1")
		if n2 == nil {
			t.Fatalf("B never signalled the first process: exit %d %+v %s", code, reply, errOut)
		}
		stillHeld := LockHeld(w.state, "run-q")
		n2.Release()
		if code != 1 || reply.Refusal == nil || reply.Refusal.Code != "run_live" ||
			!strings.Contains(reply.Refusal.Detail, "another answer was applied") || !reply.DrainAsked || !reply.Drained {
			t.Fatalf("B refused for the resume of another question's answer: exit %d %+v %s", code, reply, errOut)
		}
		if got := fake.calls(); len(got) != 1 || got[0].pid != os.Getpid() || got[0].sig != syscall.SIGUSR2 {
			t.Fatalf("the signals (only the first process, never %d): %+v", n2pid, got)
		}
		if len(td.spawned) != 0 {
			t.Errorf("B started %d processes: %+v", len(td.spawned), td.spawned)
		}
		if rows := a.appliedRows(t); len(rows) != 1 {
			t.Fatalf("B appended %d rows, want only A's", len(rows))
		}
		if !stillHeld {
			t.Error("the new resume did not hold the run when B returned")
		}
	})
	t.Run("an application before the invocation does not block the drain", func(t *testing.T) {
		w := newWorld(t)
		a := newLiveAsking(t, w)
		secondQuestion(t, a)
		td := w.deps()
		b := a.answered(t, "ud-b", nil)
		a.answered(t, "ud-a", func(bind *Binding, _ *decisions.Record, ans *decisions.Answer) {
			bind.Node, bind.Question, bind.Attempt = "n2", "esc_two", 1
			ans.Option = "stop"
		})
		// A's answer is already applied before B's first judgment
		j, err := openJournal(w, "run-q")
		if err != nil {
			t.Fatal(err)
		}
		rec := mustDecisionRecord(t, w, "ud-a")
		app, refusal := Judge(rec, j.Events(), a.rec, Claim{Decision: "ud-a", Run: "run-q", Node: "n2", Question: "esc_two", Revision: 1, By: "thread-owner"})
		if refusal != nil || app.Replayed {
			t.Fatalf("answer A: %+v %+v", app, refusal)
		}
		j.Append(engine.Event{RunID: "run-q", Kind: engine.KindFlow, Event: ops.EvDecisionApplied, Data: app.Data})
		if err := j.Close(); err != nil {
			t.Fatal(err)
		}
		p1, err := TakeLock(w.state, "run-q", ProcessCommand, td.Now())
		if err != nil {
			t.Fatal(err)
		}
		fake := newDrainFake(t, w)
		fake.on = func(n, pid int) {
			if n != 0 {
				return
			}
			fake.write(func(j *journal.Journal, _ []engine.Event) { appendDrainedEnd(j) })
			p1.Release()
		}
		td.Signal = fake.signal
		code, reply, errOut := a.answer(td, b, "--wait", "5")
		if code != 0 || !reply.Applied || reply.Refusal != nil {
			t.Fatalf("B drains past A's earlier application: exit %d %+v %s", code, reply, errOut)
		}
		if rows := a.appliedRows(t); len(rows) != 2 {
			t.Fatalf("the applied rows: %d, want A's and B's", len(rows))
		}
	})
}

func mustDecisionRecord(t *testing.T, w *world, id string) decisions.Record {
	t.Helper()
	rec, err := w.st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// TestCRW1550SpawnAcknowledgesItsCommittedStart (CRW-1550 item 2, positive): the spawned child acknowledges its committed started
// row over the spawn pipe, and the acknowledgement stands even when the run ends before the parent reads it.
func TestCRW1550SpawnAcknowledgesItsCommittedStart(t *testing.T) {
	saved := executable
	executable = os.Executable
	t.Cleanup(func() { executable = saved })
	t.Setenv("CRW_TEST_1550_PROCESS", "1")
	// a spawn is reaped by its waitChild1550, or by the cleanup if a failure leaves it running
	reaped := func(pid int) *bool {
		done := new(bool)
		t.Cleanup(func() {
			if *done {
				return
			}
			_ = syscall.Kill(pid, syscall.SIGKILL)
			for {
				var st syscall.WaitStatus
				if _, err := syscall.Wait4(pid, &st, 0, nil); !errors.Is(err, syscall.EINTR) {
					break
				}
			}
		})
		return done
	}
	w := newWorld(t)
	script := filepath.Join(t.TempDir(), "one.js")
	if err := os.WriteFile(script, []byte("export const meta = { name: 'one' }\nreturn 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Schema: SpecSchema, RunID: "run-pipe", Workflow: "one", Script: script, Thread: "thread-owner", CreatedAt: "2026-10-12T08:00:00Z"}
	if err := WriteSpec(w.state, spec); err != nil {
		t.Fatal(err)
	}
	pid, err := SpawnDetached(ctx, SpawnRequest{Spec: spec, State: w.state, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	done := reaped(pid)
	// the child is fully over - exit and result stored - before the parent reads the acknowledgement it left in the pipe
	if got := waitChild1550(t, pid, done); got != 0 {
		t.Fatalf("the run's process exited %d", got)
	}
	if err := AwaitLock(ctx, w.state, spec.RunID, pid, time.Now()); err != nil {
		t.Fatalf("the committed start of a child that already ended: %v", err)
	}
	if n := countStarted(t, w, spec.RunID); n != 1 {
		t.Fatalf("the durable start of %s: %d started rows", spec.RunID, n)
	}
	if res, found := ReadResult(w.state, spec.RunID); !found || res.Status != engine.RunCompleted {
		t.Errorf("the finished child's result: %+v %v", res, found)
	}
	t.Run("a refused start is not acknowledged", func(t *testing.T) {
		// a resume of a cancelled run refuses it under the lock, before any row: the pipe ends without a byte
		run := "run-pipe-refused"
		r := w.newRun(run, "thread-owner")
		r.finish(engine.RunScriptError, engine.ErrKindCancelled)
		r.close()
		spec2 := Spec{Schema: SpecSchema, RunID: run, Workflow: "one", Script: script, Thread: "thread-owner", CreatedAt: "2026-10-12T08:00:00Z"}
		if err := WriteSpec(w.state, spec2); err != nil {
			t.Fatal(err)
		}
		// a result an earlier process left cannot stand in for the start either
		if err := os.MkdirAll(RunDir(w.state, run), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeResult(w.state, run, &engine.Result{RunID: run, Status: engine.RunCompleted, Value: json.RawMessage("1")}, time.Now()); err != nil {
			t.Fatal(err)
		}
		pid, err := SpawnDetached(ctx, SpawnRequest{Spec: spec2, State: w.state, Resume: true, Dir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		done := reaped(pid)
		awaitErr := AwaitLock(ctx, w.state, run, pid, time.Now())
		if awaitErr == nil {
			t.Error("a resume that refuses a cancelled run was acknowledged")
		}
		if got := waitChild1550(t, pid, done); got == 0 {
			t.Error("the refused child exited 0")
		}
		if n := countStarted(t, w, run); n != 1 {
			t.Errorf("the refused resume recorded %d started rows (was 1)", n)
		}
	})
}

// waitChild1550 reaps the child of pid with Wait4, retrying EINTR and polling; on the deadline it kills, reaps and fails. done is
// set so the cleanup never signals an already reaped pid.
func waitChild1550(t *testing.T, pid int, done *bool) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var st syscall.WaitStatus
		wpid, err := syscall.Wait4(pid, &st, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if wpid == pid {
			*done = true
			return st.ExitStatus()
		}
		if err != nil {
			*done = true
			t.Fatalf("reaping %d: %v", pid, err)
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			for {
				if _, err := syscall.Wait4(pid, &st, 0, nil); !errors.Is(err, syscall.EINTR) {
					break
				}
			}
			*done = true
			t.Fatalf("the child of pid %d did not end in time", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// init, in the helper child marked CRW_TEST_1550_PROCESS, runs the crw flow command line it was given (the hidden _run of
// SpawnDetached) over a fake world on the temporary store, then exits: the run's started row commits in the store and the
// ready descriptor acknowledges it.
func init() {
	marker := os.Getenv("CRW_TEST_1550_PROCESS")
	if marker == "" {
		return
	}
	i := slices.Index(os.Args, "flow")
	if i < 0 {
		os.Exit(2)
	}
	deps := Default()
	deps.World = func(ctx context.Context, req WorldRequest) (World, error) {
		st, err := store.Open(ctx, filepath.Join(req.State, "relay.sqlite3"), "")
		if err != nil {
			return World{}, err
		}
		id := journal.Identity{}
		if !req.Resume {
			id = journal.Identity{ActorTaskID: req.Spec.Thread, PlanID: "plan-1", PlanRevision: 3}
		}
		j, err := journal.Open(ctx, st, req.Spec.RunID, id, journal.Options{Immediate: true})
		if err != nil {
			_ = st.Close()
			return World{}, err
		}
		return World{Journal: j, Backend: engine.NewFake(), Policy: engine.FakePolicy(), Close: func() error {
			return errors.Join(j.Close(), st.Close())
		}}, nil
	}
	os.Exit(RunWith(context.Background(), deps, os.Args[i+1:], os.Stdin, os.Stdout, os.Stderr))
}
