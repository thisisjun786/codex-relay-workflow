package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A cancelled run is over (CRW-1495, D2 e): every resume refuses it before anything is appended to its journal and before a run
// process is started for it: `crw flow resume RUN --detach` and the resumes of `crw flow answer` (resumeRefused), the run process
// (execute, under its run lock and before its world opens) and the foreground `crw flow resume RUN SCRIPT` (under its run lock). A
// `run/paused` end and a held finish stay resumable.

// cancelledRow is the finished row a `crw flow cancel` of a run no process holds appends (cancelDead).
func cancelledRow(run string) engine.Event {
	return engine.Event{RunID: run, Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": engine.RunScriptError, "error_kind": engine.ErrKindCancelled, "by": "operator"}}
}

// cancelledRun writes a run of the world with its started row and a cancel's finished row, and its spec; no lock is held.
func cancelledRun(t *testing.T, w *world, id string) {
	t.Helper()
	r := w.newRun(id, "thread-owner")
	r.j.Append(cancelledRow(id))
	r.close()
	if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: id, Workflow: "project-lane", Thread: "thread-owner", CreatedAt: "2026-10-12T08:00:00Z"}); err != nil {
		t.Fatal(err)
	}
}

// pausedThenCancelled runs the first process of a run of pw's script, which the operator pauses in its first call, and cancels the
// paused run with `crw flow cancel` (a finished row by the operator after the paused end): the journal of a real run, its started row
// naming the script's digest, so a resume of it passes notTheRuns.
func pausedThenCancelled(t *testing.T, pw *processWorld, run string) Spec {
	t.Helper()
	spec := pw.spec(run, nil)
	sigs := make(chan os.Signal, 2)
	pause := true
	pw.handle = func(r engine.FakeRequest) engine.FakeReply {
		if pause && r.Kind == "turn" && strings.Contains(r.Prompt, "first") {
			sigs <- syscall.SIGUSR1
			time.Sleep(200 * time.Millisecond)
		}
		return engine.FakeReply{}
	}
	if code, errOut := pw.execute(spec, false, sigs); code != 0 {
		t.Fatalf("the first process: exit %d %s", code, errOut)
	}
	pause = false
	if code, out, errOut := pw.td.do("cancel", run, "--state", pw.state); code != 0 || !strings.Contains(out, "it is cancelled") {
		t.Fatalf("the cancel: exit %d, out %q, err %q", code, out, errOut)
	}
	return spec
}

// signalCancelled runs the first process of a run of pw's script and ends it with SIGTERM in its first call: the process itself
// writes the cancelled finished row (process_test.go, the cancel subtest).
func signalCancelled(t *testing.T, pw *processWorld, run string) Spec {
	t.Helper()
	spec := pw.spec(run, nil)
	sigs := make(chan os.Signal, 2)
	cancel := true
	pw.handle = func(r engine.FakeRequest) engine.FakeReply {
		if cancel && r.Kind == "turn" && strings.Contains(r.Prompt, "first") {
			sigs <- syscall.SIGTERM
			time.Sleep(200 * time.Millisecond)
		}
		return engine.FakeReply{}
	}
	if code, _ := pw.execute(spec, false, sigs); code != 1 {
		t.Fatalf("a cancelled run exits %d", code)
	}
	cancel = false
	return spec
}

// TestEveryResumeRefusesACancelledRun: (a) `resume RUN --detach` refuses a cancelled run and starts no process; (b) the run process
// (execute with resume) refuses it under its lock before its world opens, so nothing is appended, whether a cancel of the paused run or
// SIGTERM in its process ended it; (c) the foreground `resume RUN SCRIPT` refuses it under its lock and appends nothing; (d) an answer to
// an open question of a cancelled run is applied (Judge has no rule on the run's end) and its resume is refused; (e) a paused end and a
// held finish are still resumed.
func TestEveryResumeRefusesACancelledRun(t *testing.T) {
	t.Run("detach", func(t *testing.T) {
		w := newWorld(t)
		cancelledRun(t, w, "run-c")
		td := w.deps()
		code, out, errOut := td.do("resume", "run-c", "--detach", "--state", w.state)
		if code == 0 || !strings.Contains(errOut, "run run-c is cancelled") || strings.Contains(out, "resumed") || len(td.spawned) != 0 {
			t.Errorf("resume --detach of a cancelled run: exit %d, out %q, err %q, %d spawned", code, out, errOut, len(td.spawned))
		}
	})
	for _, by := range []string{"cancel", "sigterm"} {
		t.Run("execute-"+by, func(t *testing.T) {
			pw := newProcessWorld(t, twoCalls)
			run := "run-x-" + by
			var spec Spec
			if by == "cancel" {
				spec = pausedThenCancelled(t, pw, run)
			} else {
				spec = signalCancelled(t, pw, run)
			}
			before, _, err := loadRun(pw.world, run)
			if err != nil {
				t.Fatal(err)
			}
			opened := false
			world := pw.td.World
			pw.td.World = func(ctx context.Context, req WorldRequest) (World, error) {
				opened = true
				return world(ctx, req)
			}
			code, errOut := pw.execute(spec, true, nil)
			if code == 0 || !strings.Contains(errOut, "run "+run+" is cancelled") {
				t.Errorf("the run process resuming a cancelled run: exit %d %s", code, errOut)
			}
			if opened {
				t.Errorf("the run process opened its world for a cancelled run")
			}
			events, _, err := loadRun(pw.world, run)
			if err != nil {
				t.Fatal(err)
			}
			// the end of the run is the one its first process (and a cancel) left: a cancel's finished row after the paused end, or the
			// cancelled process's own finished row
			want := map[string]string{"cancel": "paused|finished", "sigterm": "finished"}[by]
			if got := strings.Join(endRowsAfterLastStart(events), "|"); got != want || startedRows(events, run) != 1 || len(events) != len(before) {
				t.Errorf("the rows after the last start %q (want %q), %d started rows, %d rows appended", got, want, startedRows(events, run), len(events)-len(before))
			}
			if info, _ := readRun(t, pw.td, run); info.State != StateCancelled {
				t.Errorf("the run reads %q after the refused resume", info.State)
			}
		})
	}
	t.Run("script", func(t *testing.T) {
		pw := newProcessWorld(t, twoCalls)
		scriptResumeEnv(t, pw)
		run := "run-script"
		pausedThenCancelled(t, pw, run)
		var sent atomic.Int32
		pw.handle = func(r engine.FakeRequest) engine.FakeReply {
			if r.Kind == "turn" {
				sent.Add(1)
			}
			return engine.FakeReply{}
		}
		before, _, err := loadRun(pw.world, run)
		if err != nil {
			t.Fatal(err)
		}
		code, out, errOut := pw.td.do("resume", run, pw.script, "--state", pw.state)
		if code == 0 || !strings.Contains(errOut, "run "+run+" is cancelled") {
			t.Errorf("resume RUN SCRIPT of a cancelled run: exit %d, out %q, err %q", code, out, errOut)
		}
		after, _, err := loadRun(pw.world, run)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) || sent.Load() != 0 {
			t.Errorf("the refused resume appended %d rows and sent %d turns", len(after)-len(before), sent.Load())
		}
		if info := pw.info(run); info.State != StateCancelled {
			t.Errorf("the run reads %q after the refused resume", info.State)
		}
	})
	t.Run("answer", func(t *testing.T) {
		w := newWorld(t)
		r := w.newRun("run-q", "thread-owner")
		r.phase("build")
		r.call("c_ex", "executor", "n1", 2, []string{engine.EvPrepared, engine.EvRefused}, map[string]map[string]any{engine.EvRefused: {"reason": "pair_refused"}})
		r.flow(engine.EvEscalation, map[string]any{"reason": "pair_refused", "class": "configuration_fault", "node": "n1", "call_id": "c_ex", "escalation_id": "esc_one", "ask": "configuration"})
		r.j.Append(cancelledRow("run-q"))
		r.close()
		a := &asking{run: r, q: "esc_one", rec: RunRecord{RunID: "run-q", Owner: "thread-owner", PlanID: "plan-1", PlanRevision: 3}}
		if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: "run-q", Workflow: "project-lane", Thread: "thread-owner", CreatedAt: "2026-10-12T08:00:00Z"}); err != nil {
			t.Fatal(err)
		}
		td := w.deps()
		id := a.answered(t, "ud-cancelled", nil)
		code, out, errOut := td.do("answer", "--state", w.state, "--decision", id, "--run", "run-q", "--node", "n1", "--question", "esc_one", "--revision", "1", "--thread", "thread-owner", "--json")
		var reply AnswerReply
		if err := json.Unmarshal([]byte(strings.TrimPrefix(out, "<<spawned>>\n")), &reply); err != nil {
			t.Fatalf("the reply %q: %v", out, err)
		}
		if code != 1 || !reply.Applied || reply.Refusal != nil || reply.Resumed != 0 || strings.Contains(out, "resumed_pid") {
			t.Errorf("the answer to a cancelled run: exit %d %+v %q", code, reply, errOut)
		}
		if !strings.Contains(errOut, "crw flow answer: the answer is applied and the run is not resumed: run run-q is cancelled: ") {
			t.Errorf("the refusal of the resume: %q", errOut)
		}
		if len(td.spawned) != 0 || len(a.appliedRows(t)) != 1 {
			t.Errorf("the answer started %d processes and appended %d decision_applied rows", len(td.spawned), len(a.appliedRows(t)))
		}
	})
	t.Run("guard", func(t *testing.T) {
		w := newWorld(t)
		paused := w.newRun("run-paused", "thread-owner")
		paused.endPaused()
		paused.close()
		held := w.newRun("run-held", "thread-owner")
		held.hold("c_a", "executor", "n1")
		held.finish(engine.RunCompleted, "")
		held.close()
		for _, run := range []string{"run-paused", "run-held"} {
			if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: run, Workflow: "project-lane", Thread: "thread-owner", CreatedAt: "2026-10-12T08:00:00Z"}); err != nil {
				t.Fatal(err)
			}
			td := w.deps()
			code, out, errOut := td.do("resume", run, "--detach", "--state", w.state)
			if code != 0 || !strings.Contains(out, "run "+run+" resumed (process 4242)") || len(td.spawned) != 1 {
				t.Errorf("resume --detach of %s: exit %d, out %q, err %q, %d spawned", run, code, out, errOut, len(td.spawned))
			}
		}
	})
}

// startedRows counts the started rows of run in events.
func startedRows(events []engine.Event, run string) int {
	n := 0
	for _, e := range events {
		if e.Kind == engine.KindRun && e.RunID == run && e.Event == "started" {
			n++
		}
	}
	return n
}

// TestAResumeAfterACancelsHoldRefuses: a cancel holds the run lock while it writes its finished row; a `resume RUN --detach` that
// begins meanwhile waits the writer's hold out (D2 c) and then reads the run cancelled: it is refused and starts no process.
func TestAResumeAfterACancelsHoldRefuses(t *testing.T) {
	w := newWorld(t)
	w.newRun("run-c", "thread-owner").close()
	if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: "run-c", Workflow: "project-lane", Thread: "thread-owner", CreatedAt: "2026-10-12T08:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	td := w.deps()
	lock := holdLock(t, w, "run-c", "cancel")
	done := make(chan error, 1)
	go func() {
		defer lock.Release()
		time.Sleep(150 * time.Millisecond)
		st, err := store.Open(ctx, w.db, "")
		if err != nil {
			done <- err
			return
		}
		defer st.Close()
		j, err := journal.Open(ctx, st, "run-c", journal.Identity{}, journal.Options{Immediate: true})
		if err != nil {
			done <- err
			return
		}
		j.Append(cancelledRow("run-c"))
		done <- j.Close()
	}()
	code, out, errOut := td.do("resume", "run-c", "--detach", "--state", w.state)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if code == 0 || !strings.Contains(errOut, "run run-c is cancelled") || strings.Contains(out, "resumed") || len(td.spawned) != 0 {
		t.Errorf("resume --detach after a cancel's hold: exit %d, out %q, err %q, %d spawned", code, out, errOut, len(td.spawned))
	}
}
