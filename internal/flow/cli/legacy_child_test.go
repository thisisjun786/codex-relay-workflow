package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/audit"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// TestLegacyIssueChildRunIsNotResumed (CRW-1415): a run whose journal holds a crw.releaseChild row started an issue child under the
// old project lane, which the current lane does not replay. `crw flow resume RUN --detach` refuses it before a process is started, the
// run process refuses it before it writes a row, the refusal names the legacy issue-child path, and `crw flow show` still reads the run.
func TestLegacyIssueChildRunIsNotResumed(t *testing.T) {
	pw := newProcessWorld(t, twoCalls)
	run := "run-legacy-child"
	pw.handle = func(engine.FakeRequest) engine.FakeReply {
		return engine.FakeReply{Tokens: &engine.Tokens{Input: 10, Output: 5, Total: 15}}
	}
	spec := pw.spec(run, nil)
	if code, errOut := pw.execute(spec, false, make(chan os.Signal, 1)); code != 0 {
		t.Fatalf("the first process: exit %d %s", code, errOut)
	}
	// the child start of an old project lane, as an old run's journal holds it
	j, err := openJournal(pw.world, run)
	if err != nil {
		t.Fatal(err)
	}
	j.Append(engine.Event{RunID: run, Kind: engine.KindCall, CallID: "c_child", Path: "root", Position: 9, Op: engine.OpLegacyReleaseChild, Label: engine.OpLegacyReleaseChild,
		Event: engine.EvCompleted, Data: map[string]any{"node": "A", "child": "child-A", "started": true}})
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	rowsOf := func() int {
		events, _, err := loadRun(pw.world, run)
		if err != nil {
			t.Fatal(err)
		}
		return len(events)
	}
	rows := rowsOf()

	before := len(pw.td.spawned)
	code, out, errOut := pw.td.do("resume", run, "--detach", "--state", pw.state)
	if code == 0 || !strings.Contains(errOut, "legacy issue-child run") || !strings.Contains(errOut, engine.OpLegacyReleaseChild) || strings.Contains(out, "resumed") {
		t.Errorf("a detached resume of a legacy issue-child run: exit %d %q %q", code, out, errOut)
	}
	if n := len(pw.td.spawned) - before; n != 0 {
		t.Errorf("a detached resume of a legacy issue-child run started %d processes", n)
	}

	code, errOut = pw.execute(spec, true, make(chan os.Signal, 1))
	if code == 0 || !strings.Contains(errOut, "legacy issue-child run") {
		t.Errorf("the run process resumed a legacy issue-child run: exit %d %q", code, errOut)
	}
	if got := rowsOf(); got != rows {
		t.Errorf("the refused resumes wrote rows: %d -> %d", rows, got)
	}

	if code, out, errOut := pw.td.do("show", run, "--state", pw.state); code != 0 || !strings.Contains(out, run) {
		t.Errorf("crw flow show of a legacy issue-child run: exit %d %q %q", code, out, errOut)
	}
}

// TestLegacyIssueLaneRunIsNotResumed (CRW-1415, PR #889 round 1): a run started as the retired issue-lane is a legacy issue-child run
// whatever its journal holds besides. `crw flow resume RUN --detach` refuses it before a process is started, the run process and
// `crw flow resume RUN /path/to/issue-lane.js` refuse it before a row is written, and `crw flow show` still reads it.
func TestLegacyIssueLaneRunIsNotResumed(t *testing.T) {
	pw := newProcessWorld(t, strings.Replace(twoCalls, "name: 'two'", "name: '"+LegacyIssueLaneWorkflow+"'", 1))
	old := filepath.Join(t.TempDir(), "issue-lane.js")
	if src, err := os.ReadFile(pw.script); err != nil || os.WriteFile(old, src, 0o644) != nil {
		t.Fatalf("the old issue-lane script: %v", err)
	}
	prev := engine.ResumeEnv
	t.Cleanup(func() { engine.ResumeEnv = prev })
	engine.ResumeEnv = func(ctx context.Context, req engine.ResumeEnvRequest) (engine.ResumeSetup, error) {
		w, err := pw.td.World(ctx, WorldRequest{Spec: Spec{RunID: req.RunID}, State: req.State, Resume: true})
		if err != nil {
			return engine.ResumeSetup{}, err
		}
		return engine.ResumeSetup{Journal: w.Journal, Backend: w.Backend, Close: w.Close}, nil
	}
	run := "run-legacy-lane"
	pw.handle = func(engine.FakeRequest) engine.FakeReply {
		return engine.FakeReply{Tokens: &engine.Tokens{Input: 10, Output: 5, Total: 15}}
	}
	spec := pw.spec(run, func(s *Spec) { s.Workflow, s.Script = LegacyIssueLaneWorkflow, old })
	if code, errOut := pw.execute(spec, false, make(chan os.Signal, 1)); code != 0 {
		t.Fatalf("the old issue-lane run: exit %d %s", code, errOut)
	}
	rowsOf := func() int {
		events, _, err := loadRun(pw.world, run)
		if err != nil {
			t.Fatal(err)
		}
		return len(events)
	}
	rows := rowsOf()

	code, out, errOut := pw.td.do("resume", run, "--detach", "--state", pw.state)
	if code == 0 || !strings.Contains(errOut, "legacy issue-child run") || !strings.Contains(errOut, LegacyIssueLaneWorkflow) || len(pw.td.spawned) != 0 {
		t.Errorf("a detached resume of an issue-lane run: exit %d %q %q, %d processes", code, out, errOut, len(pw.td.spawned))
	}
	if code, errOut := pw.execute(spec, true, make(chan os.Signal, 1)); code == 0 || !strings.Contains(errOut, "legacy issue-child run") {
		t.Errorf("the run process resumed an issue-lane run: exit %d %q", code, errOut)
	}
	pw.world.close()
	code, out, errOut = pw.td.do("resume", run, old, "--state", pw.state)
	pw.world.open()
	if code == 0 || !strings.Contains(errOut, "legacy issue-child run") {
		t.Errorf("crw flow resume RUN issue-lane.js: exit %d %q %q", code, out, errOut)
	}
	if got := rowsOf(); got != rows {
		t.Errorf("the refused resumes wrote rows: %d -> %d", rows, got)
	}
	if code, out, errOut := pw.td.do("show", run, "--state", pw.state); code != 0 || !strings.Contains(out, run) {
		t.Errorf("crw flow show of an issue-lane run: exit %d %q %q", code, out, errOut)
	}
}

// legacyAsking is the asking run with the child start of an old project lane in its journal: a legacy issue-child run that waits for
// an answer.
func legacyAsking(t *testing.T, w *world) *asking {
	t.Helper()
	a := newAsking(t, w)
	r, err := w.reopen("run-q")
	if err != nil {
		t.Fatal(err)
	}
	r.op("c_child", engine.OpLegacyReleaseChild, map[string]any{"node": "n2", "child": "child-n2", "started": true})
	r.close()
	if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: "run-q", Workflow: "project-lane", Thread: "thread-owner", CreatedAt: "2026-10-12T08:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	return a
}

// TestAnswerRefusesALegacyRun (CRW-1415, PR #889 round 1): `crw flow answer` writes nothing to a legacy issue-child run. With the
// auto-resume and with --no-resume the answer is refused under the run lock before the claim is judged, with a reason that names the
// legacy issue-child path, no decision_applied row (no row at all) is appended and no process is started.
func TestAnswerRefusesALegacyRun(t *testing.T) {
	w := newWorld(t)
	a := legacyAsking(t, w)
	td := w.deps()
	id := a.answered(t, "ud-legacy", nil)
	before := len(mustLoad(t, w))
	for _, extra := range [][]string{nil, {"--no-resume"}} {
		code, reply, errOut := a.answer(td, id, extra...)
		if code == 0 || reply.Applied || reply.Refusal == nil || reply.Refusal.Code != RefusedLegacyRun || !strings.Contains(reply.Refusal.Detail, "legacy issue-child run") {
			t.Errorf("answer %v to a legacy run: exit %d %+v %q", extra, code, reply, errOut)
		}
		if got := len(mustLoad(t, w)); got != before || len(a.appliedRows(t)) != 0 {
			t.Errorf("answer %v to a legacy run wrote rows: %d -> %d", extra, before, got)
		}
		if len(td.spawned) != 0 {
			t.Errorf("answer %v to a legacy run started %d processes", extra, len(td.spawned))
		}
	}
}

// TestNextRefusesALegacyRun (CRW-1415, PR #889 round 1): `crw flow next` of a legacy issue-child run with no process does not wait out
// --wait and answer running: the refusal of its resume is the command's answer, nonzero, and no process is started.
func TestNextRefusesALegacyRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fastNextPoll(t)
	w := newWorld(t)
	td := w.deps()
	run, owner := "run-0123456789e1", "01a30000-0000-7000-8000-0000000000e1"
	spentSessionRun(t, w, run, owner, 0, 10)
	r, err := w.reopen(run)
	if err != nil {
		t.Fatal(err)
	}
	r.op("c_child", engine.OpLegacyReleaseChild, map[string]any{"node": "A", "child": "child-A", "started": true})
	r.close()
	spawned := 0
	td.Spawn = func(context.Context, SpawnRequest) (int, error) {
		spawned++
		return os.Getpid(), nil
	}
	td.Ready = AwaitLock
	began := time.Now()
	code, out, errOut := td.do("next", run, "--wait", "5", "--json", "--state", w.state)
	if code == 0 || !strings.Contains(errOut, "legacy issue-child run") || spawned != 0 {
		t.Errorf("next of a legacy run: exit %d, %d processes, %q %q", code, spawned, out, errOut)
	}
	if took := time.Since(began); took > 4*time.Second {
		t.Errorf("next of a legacy run waited %s", took)
	}
}

// TestResumeRefusedReadsTheJournalOnce (PR #889 round 1): the refusal of a resume reads the run's journal once, for the legacy check and
// the budget of a spec that has one.
func TestResumeRefusedReadsTheJournalOnce(t *testing.T) {
	w := newWorld(t)
	run, owner := "run-0123456789e2", "01a30000-0000-7000-8000-0000000000e2"
	spec := spentSessionRun(t, w, run, owner, 50, 10)
	loads := 0
	prev := loadLeanJournal
	t.Cleanup(func() { loadLeanJournal = prev })
	loadLeanJournal = func(ctx context.Context, st *store.Store, id string) ([]engine.Event, bool, error) {
		loads++
		return prev(ctx, st, id)
	}
	if err := resumeRefused(ctx, w.st, spec); err != nil {
		t.Fatalf("a run with budget left: %v", err)
	}
	if loads != 1 {
		t.Errorf("resumeRefused read the journal %d times", loads)
	}
}

// TestEveryJournalWriterRefusesALegacyRun (CRW-1415, PR #889 round 2): every command that can write to a run's journal refuses a legacy
// issue-child run with legacy_issue_child_run before it writes, whether a process holds the run or not: `crw flow cancel` (which ends a
// run no process holds with a finished row and gives its executions up), `crw flow pause`, `crw flow recover` (which appends recovery
// rows) and `crw flow dispatched` (whose report the run journals when it answers it). Nothing is appended, no execution is given up, the
// recovery's engine is not reached and no report is put in the run's inbox; `crw flow show` still reads the run.
func TestEveryJournalWriterRefusesALegacyRun(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	run := "run-legacy-writers"
	r := w.newRun(run, "thread-owner")
	r.hold("c_hold", "executor", "n1")
	r.op("c_child", engine.OpLegacyReleaseChild, map[string]any{"node": "n2", "child": "child-n2", "started": true})
	r.close()
	if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: run, Workflow: "project-lane", Thread: "thread-owner", CreatedAt: "2026-10-12T08:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	settled := 0
	td.Settle = func(context.Context, *store.Store, string, []ops.ReleaseRef) (Settlement, error) {
		settled++
		return Settlement{Closed: 1}, nil
	}
	reached := 0
	prev := engine.RecoverEnv
	t.Cleanup(func() { engine.RecoverEnv = prev })
	engine.RecoverEnv = func(context.Context, engine.RecoverEnvRequest) (engine.RecoverSetup, error) {
		reached++
		return engine.RecoverSetup{}, errors.New("the recovery reached the journal")
	}
	rowsOf := func() int {
		events, _, err := loadRun(w, run)
		if err != nil {
			t.Fatal(err)
		}
		return len(events)
	}
	rows := rowsOf()
	commands := [][]string{
		{"cancel", run, "--state", w.state},
		{"pause", run, "--state", w.state},
		{"recover", "--run", run, "--state", w.state, "c_hold", "--absent"},
		{"dispatched", run, "c_hold", "--agent", "agent-1", "--k", "1", "--attempt", "1", "--wait", "0", "--state", w.state},
	}
	check := func(when string) {
		t.Helper()
		for _, argv := range commands {
			code, out, errOut := td.do(argv...)
			if code == 0 || !strings.Contains(errOut, RefusedLegacyRun) || !strings.Contains(errOut, "legacy issue-child run") || !strings.Contains(errOut, engine.OpLegacyReleaseChild) {
				t.Errorf("%s: %s of a legacy run: exit %d %q %q", when, argv[0], code, out, errOut)
			}
		}
		if got := rowsOf(); got != rows {
			t.Errorf("%s: the refused commands wrote rows: %d -> %d", when, rows, got)
		}
		if settled != 0 || reached != 0 {
			t.Errorf("%s: a refused command gave %d executions up and reached the recovery %d times", when, settled, reached)
		}
		if entries, _ := os.ReadDir(engine.NewFileStepBus(RunDir(w.state, run)).InboxDir()); len(entries) != 0 {
			t.Errorf("%s: a refused dispatched report is in the run's inbox: %v", when, entries)
		}
	}
	check("no process")
	// a process holds the run: the signal that would make it write is not sent either
	signalled := 0
	td.Signal = func(int, syscall.Signal) error { signalled++; return nil }
	lock, err := TakeLock(w.state, run, "test", td.Now())
	if err != nil {
		t.Fatal(err)
	}
	commands = commands[:2]
	check("a process holds the run")
	lock.Release()
	if signalled != 0 {
		t.Errorf("a legacy run's process was signalled %d times", signalled)
	}
	if code, out, errOut := td.do("show", run, "--state", w.state); code != 0 || !strings.Contains(out, run) {
		t.Errorf("crw flow show of a legacy run: exit %d %q %q", code, out, errOut)
	}
}

// TestPreFirstRowIssueLaneRunIsRefusedAsLegacy (CRW-1415, PR #889 round 2): an old issue-lane process that ended after the run's spec was
// written and before its started row left a run whose journal holds nothing to name it legacy. A foreground resume, the run process's
// and `crw flow resume RUN issue-lane.js`, refuses it as a legacy issue-child run by the workflow its spec or its script names (as the
// detached resume does), and no row is written.
func TestPreFirstRowIssueLaneRunIsRefusedAsLegacy(t *testing.T) {
	pw := newProcessWorld(t, strings.Replace(twoCalls, "name: 'two'", "name: '"+LegacyIssueLaneWorkflow+"'", 1))
	old := filepath.Join(t.TempDir(), "issue-lane.js")
	if src, err := os.ReadFile(pw.script); err != nil || os.WriteFile(old, src, 0o644) != nil {
		t.Fatalf("the old issue-lane script: %v", err)
	}
	prev := engine.ResumeEnv
	t.Cleanup(func() { engine.ResumeEnv = prev })
	engine.ResumeEnv = func(ctx context.Context, req engine.ResumeEnvRequest) (engine.ResumeSetup, error) {
		w, err := pw.td.World(ctx, WorldRequest{Spec: Spec{RunID: req.RunID}, State: req.State, Resume: true})
		if err != nil {
			return engine.ResumeSetup{}, err
		}
		return engine.ResumeSetup{Journal: w.Journal, Backend: w.Backend, Close: w.Close}, nil
	}
	run := "run-legacy-pre"
	turns := 0
	pw.handle = func(engine.FakeRequest) engine.FakeReply {
		turns++
		return engine.FakeReply{Tokens: &engine.Tokens{Input: 10, Output: 5, Total: 15}}
	}
	spec := pw.spec(run, func(s *Spec) { s.Workflow, s.Script = LegacyIssueLaneWorkflow, old })
	rowsOf := func() int {
		events, _, err := loadRun(pw.world, run)
		if err != nil {
			t.Fatal(err)
		}
		return len(events)
	}
	if code, errOut := pw.execute(spec, true, make(chan os.Signal, 1)); code == 0 || !strings.Contains(errOut, "legacy issue-child run") || !strings.Contains(errOut, LegacyIssueLaneWorkflow) {
		t.Errorf("the run process resumed a pre-first-row issue-lane run: exit %d %q", code, errOut)
	}
	pw.world.close()
	code, out, errOut := pw.td.do("resume", run, old, "--state", pw.state)
	pw.world.open()
	if code == 0 || !strings.Contains(errOut, "legacy issue-child run") || !strings.Contains(errOut, LegacyIssueLaneWorkflow) {
		t.Errorf("crw flow resume RUN issue-lane.js of a pre-first-row run: exit %d %q %q", code, out, errOut)
	}
	if got := rowsOf(); got != 0 || turns != 0 {
		t.Errorf("the refused resumes wrote %d rows and ran %d turns", got, turns)
	}
}

// TestReportUpAndAuditWriteNothingToALegacyRun (CRW-1415, PR #889 round 2): `crw flow report-up` about a legacy issue-child run is
// refused with legacy_issue_child_run before its report row is written, and `crw flow audit` of a legacy run that has finished prints
// its results and records no audit row (not_recorded_because names legacy_issue_child_run).
func TestReportUpAndAuditWriteNothingToALegacyRun(t *testing.T) {
	w := newCLIWorld(t)
	run := "run-legacy-up"
	j, err := journal.Open(ctx, w.st, run, journal.Identity{PlanID: "plan-1", PlanRevision: 1, ActorTaskID: "orch-task", EpochNonce: "n"}, journal.Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	j.Append(engine.Event{RunID: run, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane", "script_digest": "d", "args_digest": "a"}})
	j.Append(engine.Event{RunID: run, Kind: engine.KindCall, CallID: "c_child", Path: "root", Position: 2, Op: engine.OpLegacyReleaseChild, Label: engine.OpLegacyReleaseChild, Event: engine.EvCompleted, Data: map[string]any{"node": "A", "child": "child-A", "started": true}})
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	rowsOf := func() int {
		events, _, err := journal.Load(ctx, w.st, run)
		if err != nil {
			t.Fatal(err)
		}
		return len(events)
	}
	rows := rowsOf()
	evidence := filepath.Join(t.TempDir(), "evidence.md")
	if err := os.WriteFile(evidence, []byte("the legacy run is blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := w.run("report-up", "--kind", "project_blocked", "--run", run, "--evidence", evidence, "--subject", "legacy"); code == ExitOK || !strings.Contains(w.errOut.String(), RefusedLegacyRun) || !strings.Contains(w.errOut.String(), "legacy issue-child run") {
		t.Errorf("report-up about a legacy run: exit %d %q", code, w.errOut.String())
	}
	if got := rowsOf(); got != rows || len(w.sender.supervisor) != 0 {
		t.Errorf("the refused report-up wrote rows %d -> %d and sent %d notices", rows, got, len(w.sender.supervisor))
	}

	fixedClock(t)
	mj := acceptedWithNoReview(true)
	mj.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_child", Op: engine.OpLegacyReleaseChild, Event: engine.EvCompleted, Data: map[string]any{"node": "B", "child": "child-B", "started": true}})
	useEnv(t, mj, true, nil)
	before := len(mj.Events())
	code, out, _ := auditCmd("--run", "run-1", "--json")
	var rep struct {
		Results  []audit.Result `json:"results"`
		Recorded bool           `json:"recorded"`
		Why      string         `json:"not_recorded_because"`
	}
	_ = json.Unmarshal([]byte(out), &rep)
	if code != 1 || len(rep.Results) != 1 || rep.Recorded || !strings.Contains(rep.Why, RefusedLegacyRun) || len(mj.Events()) != before {
		t.Errorf("audit of a legacy run: exit %d %+v, rows %d -> %d", code, rep, before, len(mj.Events()))
	}
}
