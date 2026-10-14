package cli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
)

// CRW-1390 (the D2 follow-ups of CRW-1372 and the dev merge of bd2d68b6):
//
//   - `crw flow next` resumes a dead run as `crw flow resume RUN --detach` does, and like it (and `crw flow answer`) asks budgetLeft
//     before it starts the run's process: a run whose spec's budget is spent is refused with the same words, and no process is started
//     (it used to start one that ended at once with budget_exhausted, which next showed as the error of its wait for the lock).
//   - the warning of a binding another live run holds (bindWarning, ownerLineOf) says what makes that run live: its process holds its
//     run lock, or its start holds its start lock (Argon V5 P3 of 8f34843f), as the owner_bound fact says.
//   - a process that holds the run lock removes the process lock files nobody holds: the files of processes that died (D2's named
//     invariant gap). A file another process holds (one that will not get the run lock) is left.

// spentSessionRun writes a session run whose spec gives it budget tokens and whose journal shows one agent call that spent spent.
func spentSessionRun(t *testing.T, w *world, id, owner string, budget, spent int64) Spec {
	t.Helper()
	spec := Spec{Schema: SpecSchema, RunID: id, Workflow: "hello", Script: helloScript(t), Thread: owner, Backend: BackendSession,
		CreatedAt: "2026-10-15T00:00:00Z", TokenBudget: budget, Decision: "ud-d4"}
	if err := WriteSpec(w.state, spec); err != nil {
		t.Fatal(err)
	}
	src, err := spec.Load()
	if err != nil {
		t.Fatal(err)
	}
	j, err := journal.Open(ctx, w.st, id, journal.Identity{PlanID: "plan-1", PlanRevision: 3, ActorTaskID: owner, EpochNonce: "n"}, journal.Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	j.Append(engine.Event{RunID: id, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "hello", "script_digest": src.Program.Digest,
		"args_digest": argsDigest([]byte("null")), "backend": engine.BackendSession, "token_budget": budget}})
	r := &run{w: w, id: id, j: j}
	r.completed("c1", "executor", "A", "thread-c1", spent)
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestNextRefusesTheResumeOfARunWithNoBudgetLeftBeforeItStartsAProcess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fastNextPoll(t)
	w := newWorld(t)
	td := w.deps()
	run, owner := "run-0123456789f1", "01a30000-0000-7000-8000-0000000000f1"
	spentSessionRun(t, w, run, owner, 50, 60)
	spawned := 0
	td.Spawn = func(context.Context, SpawnRequest) (int, error) {
		spawned++
		return os.Getpid(), nil
	}
	td.Ready = AwaitLock
	// the refusal of `crw flow resume RUN --detach`, the words next is to give
	code, _, refusal := td.do("resume", run, "--detach", "--state", w.state)
	refusal = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(refusal), "crw flow resume: error:"))
	if code == 0 || spawned != 0 || !strings.Contains(refusal, engine.ReasonBudgetExhausted) || !strings.Contains(refusal, "60 of the 50 tokens") {
		t.Fatalf("resume --detach: exit %d, %d processes, %q", code, spawned, refusal)
	}
	code, out, errOut := td.do("next", run, "--json", "--state", w.state)
	if code != 0 {
		t.Fatalf("next: %d %s %s", code, out, errOut)
	}
	if spawned != 0 {
		t.Fatalf("next started %d processes for a run with no budget left", spawned)
	}
	ans := decodeNext(t, out)
	if ans.Resumed != 0 {
		t.Fatalf("next answered resumed_pid %d", ans.Resumed)
	}
	found := false
	for _, warning := range ans.Warnings {
		if strings.Contains(warning, refusal) && strings.Contains(warning, "did not start") {
			found = true
		}
	}
	if !found {
		t.Fatalf("next's warnings %q do not carry the resume's refusal %q", ans.Warnings, refusal)
	}
	if ans.State == NextStarting {
		t.Fatalf("next answered %s for a run it did not resume", ans.State)
	}
}

func TestTheWarningOfABindingAStartHoldsSaysTheStartLock(t *testing.T) {
	isolateGit(t)
	w := newWorld(t)
	table := engine.FakePolicy()
	first, second := "run-0123456789f2", "run-0123456789f3"
	start, err := takeStartLock(w.state, first)
	if err != nil {
		t.Fatal(err)
	}
	defer start.Release()
	if res, err := bindOwner(w.state, first, sessOwner, table); err != nil || !res.Bound {
		t.Fatalf("the first run's binding: %+v %v", res, err)
	}
	res, err := bindOwner(w.state, second, sessOwner, table)
	if err != nil || res.Holder != first {
		t.Fatalf("the second run's binding: %+v %v, want it held by %s", res, err, first)
	}
	for name, got := range map[string]string{
		"bindWarning": bindWarning(res, nil, sessOwner, second),
		"ownerLineOf": func() string {
			_, line := ownerLineOf(w.state, Spec{RunID: second, Thread: sessOwner}, table.Digest)
			return line
		}(),
	} {
		if strings.Contains(got, "whose process holds its lock") || !strings.Contains(got, "start lock") || !strings.Contains(got, "run lock") || !strings.Contains(got, first) {
			t.Fatalf("%s says %q: it does not say that run %s lives by its run lock or its start lock", name, got, first)
		}
	}
}

func TestARunProcessRemovesTheProcessLockFilesNobodyHolds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	w := newWorld(t)
	td := w.deps()
	run, owner := "run-0123456789f4", "01a30000-0000-7000-8000-0000000000f4"
	const dead = "00000000000000d4"
	endedSessionProcess(t, w, run, owner, dead, false)
	stale, err := os.OpenFile(processLockPath(w.state, run, dead), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_ = stale.Close()
	// a process that took its process lock and will not get the run lock: its file is held, and stays
	other, err := takeProcessLock(w.state, run)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	g := newGatedProcesses(t, td)
	g.run(ProcessCommand, "--state", w.state, "--resume", run)
	for end := time.Now().Add(10 * time.Second); !LockHeld(w.state, run) && time.Now().Before(end); {
		time.Sleep(5 * time.Millisecond)
	}
	if !LockHeld(w.state, run) {
		t.Fatal("the run process never took the run lock")
	}
	removed := false
	for end := time.Now().Add(5 * time.Second); !removed && time.Now().Before(end); {
		_, err := os.Stat(processLockPath(w.state, run, dead))
		removed = os.IsNotExist(err)
		time.Sleep(5 * time.Millisecond)
	}
	if !removed {
		t.Fatalf("the dead process's lock file %s stays while the run process holds the run", processLockPath(w.state, run, dead))
	}
	if !processHeld(w.state, run, other.Token()) {
		t.Fatalf("the lock file of a process that holds it was removed")
	}
	entries, err := os.ReadDir(RunDir(w.state, run))
	if err != nil {
		t.Fatal(err)
	}
	locks := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "process.") && strings.HasSuffix(e.Name(), ".lock") {
			locks++
		}
	}
	if locks != 2 {
		t.Fatalf("%d process lock files in the run's directory, want the run process's own and the other holder's", locks)
	}
	g.stop()
}
