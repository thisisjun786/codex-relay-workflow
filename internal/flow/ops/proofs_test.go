package ops

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// fx is one node's attempt worktree and the journal rows its calls would have written.
type fx struct {
	t      *testing.T
	repo   string
	wt     string
	start  string
	node   string
	events []engine.Event
	prover Prover
	gitLog *gitLog
}

// gitLog records the directories git ran in.
type gitLog struct {
	mu   sync.Mutex
	dirs []string
}

func (l *gitLog) wrap(inner GitFunc) GitFunc {
	return func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
		l.mu.Lock()
		l.dirs = append(l.dirs, dir)
		l.mu.Unlock()
		return inner(ctx, dir, env, args...)
	}
}

func (l *gitLog) touched(dir string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Contains(l.dirs, dir)
}

func newFx(t *testing.T) *fx {
	t.Helper()
	repo, first := newRepo(t)
	f := &fx{t: t, repo: repo, start: first, node: "n1", gitLog: &gitLog{}}
	f.prover = Prover{Git: f.gitLog.wrap(ExecGit("git"))}
	f.wt = addWorktree(t, repo, "attempt1", first)
	f.prepare("t1", "fresh", f.wt, first)
	return f
}

func (f *fx) add(e engine.Event) engine.Event {
	e.RunID = "run-1"
	e.Seq = len(f.events) + 1
	f.events = append(f.events, e)
	return e
}

// prepare writes the prepared row of an executor call: a fresh or restore allocation at path, or a continuation (kind
// "continued") that runs in the directory of its thread.
func (f *fx) prepare(call, kind, path, start string) {
	d := map[string]any{"node": f.node, "family": "executor", "role": "executor", "head": ""}
	if kind == engine.PlaceContinued {
		d["thread_cwd"] = path
		d["allocation"] = map[string]any{"kind": kind}
	} else {
		d["allocation"] = map[string]any{"kind": kind, "path": path, "start_commit": start, "attempt": 1}
	}
	f.add(engine.Event{Kind: engine.KindCall, CallID: call, Op: "agent", Event: engine.EvPrepared, Data: d})
}

func (f *fx) snapshot() string {
	f.t.Helper()
	snap, err := Snapshot(ctx, ExecGit("git"), f.wt)
	if err != nil {
		f.t.Fatal(err)
	}
	return snap
}

// complete ends the call completed with the pending tree P of the worktree as it is now.
func (f *fx) complete(call string) string {
	p := f.snapshot()
	f.add(engine.Event{Kind: engine.KindCall, CallID: call, Op: "agent", Event: engine.EvCompleted, Data: map[string]any{"pending_tree": p}})
	return p
}

// hold ends the call held with the snapshot as P_held.
func (f *fx) hold(call string) string {
	p := f.snapshot()
	f.add(engine.Event{Kind: engine.KindCall, CallID: call, Op: "agent", Event: engine.EvHeld, Data: map[string]any{"p_held": p, "reason": "host_infra_serverOverloaded"}})
	return p
}

// commit commits the snapshot the way crw.commit does and records the row.
func (f *fx) commit(callID string) CommitResult {
	f.t.Helper()
	res, err := Commit(ctx, ExecGit("git"), f.wt, CommitRequest{Tree: f.snapshot(), Parent: gitIn(f.t, f.wt, "rev-parse", "HEAD"), Message: "crw.commit " + callID})
	if err != nil {
		f.t.Fatal(err)
	}
	f.add(engine.Event{Kind: engine.KindCall, CallID: callID, Op: OpCommit, Event: engine.EvCompleted,
		Data: map[string]any{"node": f.node, "commit": res.Commit, "parent": res.Parent, "tree": res.Tree}})
	return res
}

func (f *fx) prove() *NodeProof {
	f.t.Helper()
	np, err := f.prover.Node(ctx, f.events, f.node)
	if err != nil {
		f.t.Fatal(err)
	}
	return np
}

func (f *fx) edit(name, body string) { write(f.t, f.wt, name, body, 0o644) }

func expectProven(t *testing.T, np *NodeProof, want map[string]bool) {
	t.Helper()
	for id, w := range want {
		if got := np.Proven(id); got != w {
			t.Errorf("call %s proven=%v, want %v\n%s\n%+v", id, got, w, np.Describe(), np.Calls)
		}
	}
}

// H1 is committed, the fix t2 completed with P2 and the process died before crw.commit: the dead worktree holds P2 and the
// proof of t2 stands, while t1 is covered by the commit.
func TestPendingTreeProofAfterCrashBeforeCommit(t *testing.T) {
	f := newFx(t)
	f.edit("a.txt", "built\n")
	f.complete("t1")
	h1 := f.commit("k1")
	f.prepare("t2", engine.PlaceContinued, f.wt, "")
	f.edit("src/c.txt", "fixed\n")
	p2 := f.complete("t2")
	// the process dies here: no crw.commit row, the worktree is dirty
	np := f.prove()
	if np.Snapshot != p2 || np.Head != h1.Commit {
		t.Fatalf("snapshot %s head %s, want P2 %s on H1 %s", np.Snapshot, np.Head, p2, h1.Commit)
	}
	expectProven(t, np, map[string]bool{"t1": true, "t2": true})
	if np.Foreign != "" || np.InvalidFrom != "" || np.LastCommit != h1.Commit {
		t.Errorf("%+v", np)
	}
	if !np.Calls[0].Committed || np.Calls[1].Committed {
		t.Errorf("t1 is covered by the commit and t2 by its pending tree: %+v", np.Calls)
	}
}

func TestUnstagedExternalEditBreaksFixProof(t *testing.T) {
	f := newFx(t)
	f.edit("a.txt", "built\n")
	f.complete("t1")
	f.commit("k1")
	f.prepare("t2", engine.PlaceContinued, f.wt, "")
	f.edit("src/c.txt", "fixed\n")
	f.complete("t2")
	expectProven(t, f.prove(), map[string]bool{"t1": true, "t2": true})
	// somebody edits a tracked file by hand, staging nothing
	f.edit("a.txt", "edited from outside\n")
	np := f.prove()
	expectProven(t, np, map[string]bool{"t1": true, "t2": false})
	if np.Foreign != "snapshot" || np.InvalidFrom != "t2" {
		t.Errorf("foreign %q invalid from %q", np.Foreign, np.InvalidFrom)
	}
	if out := gitIn(t, f.wt, "diff", "--cached", "--name-only"); out != "" {
		t.Errorf("the proof staged %q", out)
	}
}

func TestUntrackedFileChangeBreaksBuildProof(t *testing.T) {
	f := newFx(t)
	f.edit("src/generated.txt", "built output\n") // untracked
	f.complete("t1")
	expectProven(t, f.prove(), map[string]bool{"t1": true})
	f.edit("src/generated.txt", "built output, edited\n")
	np := f.prove()
	expectProven(t, np, map[string]bool{"t1": false})
	if np.InvalidFrom != "t1" || np.Foreign != "snapshot" {
		t.Errorf("%+v", np)
	}
	// putting the file back restores the proof: the proof is the state of the files, not a flag
	f.edit("src/generated.txt", "built output\n")
	expectProven(t, f.prove(), map[string]bool{"t1": true})
}

func TestForeignCommitInvalidatesFromBuild(t *testing.T) {
	t.Run("before any recorded commit", func(t *testing.T) {
		f := newFx(t)
		f.edit("a.txt", "built\n")
		f.complete("t1")
		f.prepare("t2", engine.PlaceContinued, f.wt, "")
		f.edit("src/c.txt", "fixed\n")
		f.complete("t2")
		expectProven(t, f.prove(), map[string]bool{"t1": true, "t2": true})
		// a commit nobody recorded
		gitIn(t, f.wt, "add", "-A")
		gitIn(t, f.wt, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "someone else")
		np := f.prove()
		expectProven(t, np, map[string]bool{"t1": false, "t2": false})
		if np.Foreign != "commit" || np.InvalidFrom != "t1" {
			t.Errorf("foreign %q invalid from %q, want commit from the build t1", np.Foreign, np.InvalidFrom)
		}
	})
	t.Run("after a recorded commit", func(t *testing.T) {
		f := newFx(t)
		f.edit("a.txt", "built\n")
		f.complete("t1")
		h1 := f.commit("k1")
		f.prepare("t2", engine.PlaceContinued, f.wt, "")
		f.edit("src/c.txt", "fixed\n")
		f.complete("t2")
		gitIn(t, f.wt, "add", "-A")
		gitIn(t, f.wt, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "someone else")
		np := f.prove()
		// the recorded commit stays proven; the chain broke at the first call after it
		expectProven(t, np, map[string]bool{"t1": true, "t2": false})
		if np.Foreign != "commit" || np.InvalidFrom != "t2" || np.LastCommit != h1.Commit {
			t.Errorf("%+v", np)
		}
	})
	t.Run("the head moved back", func(t *testing.T) {
		f := newFx(t)
		f.edit("a.txt", "built\n")
		f.complete("t1")
		f.commit("k1")
		gitIn(t, f.wt, "reset", "-q", "--hard", f.start)
		np := f.prove()
		if np.Foreign != "commit" || np.Proven("t1") {
			t.Errorf("a HEAD before the recorded commit is not on the chain: %+v", np)
		}
	})
}

func TestExternalBaseAdvanceKeepsBuildProof(t *testing.T) {
	f := newFx(t)
	f.edit("a.txt", "built\n")
	f.complete("t1")
	r := f.commit("k1")
	f.prepare("t2", engine.PlaceContinued, f.wt, "")
	f.edit("src/c.txt", "fixed\n")
	f.complete("t2")
	// the integration target advances in the main repository, as another actor's merge does
	write(t, f.repo, "other.txt", "someone else's change\n", 0o644)
	gitIn(t, f.repo, "add", "-A")
	gitIn(t, f.repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "target moved")
	target := gitIn(t, f.repo, "rev-parse", "HEAD")
	if target == f.start {
		t.Fatal("the target did not move")
	}
	expectProven(t, f.prove(), map[string]bool{"t1": true, "t2": true})
	// an integration that was recorded stays proven when the target moves on, and only while its batch is an ancestor
	f.add(engine.Event{Kind: engine.KindCall, CallID: "i1", Op: OpIntegrate, Event: engine.EvCompleted,
		Data: map[string]any{"node": f.node, "batch_commit": r.Commit, "integration_ref": "main"}})
	gitIn(t, f.repo, "merge", "-q", "--no-edit", r.Commit)
	merged := gitIn(t, f.repo, "rev-parse", "HEAD")
	v, err := f.prover.IntegrateProof(ctx, f.events, f.repo, f.node, merged)
	if err != nil || !v.Proven {
		t.Fatalf("integrated batch: %+v %v", v, err)
	}
	write(t, f.repo, "later.txt", "and again\n", 0o644)
	gitIn(t, f.repo, "add", "-A")
	gitIn(t, f.repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "moved again")
	v, err = f.prover.IntegrateProof(ctx, f.events, f.repo, f.node, gitIn(t, f.repo, "rev-parse", "HEAD"))
	if err != nil || !v.Proven {
		t.Errorf("another actor moved the target on: %+v %v", v, err)
	}
	if v, _ = f.prover.IntegrateProof(ctx, f.events, f.repo, f.node, f.start); v.Proven {
		t.Errorf("a head that does not hold the batch commit proves nothing: %+v", v)
	}
	if v, _ = f.prover.IntegrateProof(ctx, f.events, f.repo, "n2", merged); v.Proven {
		t.Errorf("another node's integration: %+v", v)
	}
}

func TestCriteriaChangeBreaksReviewProof(t *testing.T) {
	f := newFx(t)
	f.add(engine.Event{Kind: engine.KindCall, CallID: "r1", Op: OpReview, Event: engine.EvCompleted,
		Data: map[string]any{"node": "n1", "head": "h1", "criteria_digest": "crit-a"}})
	if v := ReviewProof(f.events, "n1", "h1", "crit-a"); !v.Proven || v.CallID != "r1" {
		t.Errorf("same head and criteria: %+v", v)
	}
	for name, v := range map[string]Verdict{
		"criteria changed": ReviewProof(f.events, "n1", "h1", "crit-b"),
		"head changed":     ReviewProof(f.events, "n1", "h2", "crit-a"),
		"other node":       ReviewProof(f.events, "n2", "h1", "crit-a"),
	} {
		if v.Proven || v.Why == "" {
			t.Errorf("%s: %+v", name, v)
		}
	}
	// verification: the same head and the same command, and it passed
	f.add(engine.Event{Kind: engine.KindCall, CallID: "v1", Op: OpVerify, Event: engine.EvCompleted,
		Data: map[string]any{"node": "n1", "head": "h1", "command": "make test", "ok": true}})
	f.add(engine.Event{Kind: engine.KindCall, CallID: "v2", Op: OpVerify, Event: engine.EvCompleted,
		Data: map[string]any{"node": "n1", "head": "h2", "command": "make test", "ok": false}})
	if !VerifyProof(f.events, "n1", "h1", "make test").Proven {
		t.Error("a passing verification of the same head and command")
	}
	for name, v := range map[string]Verdict{
		"other command": VerifyProof(f.events, "n1", "h1", "make lint"),
		"other head":    VerifyProof(f.events, "n1", "h3", "make test"),
		"failed run":    VerifyProof(f.events, "n1", "h2", "make test"),
	} {
		if v.Proven {
			t.Errorf("%s proves: %+v", name, v)
		}
	}
	// acceptance: active, not stale, the same head
	f.add(engine.Event{Kind: engine.KindCall, CallID: "a1", Op: OpAccept, Event: engine.EvCompleted,
		Data: map[string]any{"node": "n1", "head": "h1", "acceptance_id": "acc-1"}})
	live := func(a Acceptance) func(string) (Acceptance, bool) {
		return func(id string) (Acceptance, bool) { return a, id == "acc-1" }
	}
	if !AcceptProof(f.events, "n1", "h1", live(Acceptance{Active: true, Head: "h1"})).Proven {
		t.Error("an active, fresh acceptance of the same head")
	}
	for name, a := range map[string]Acceptance{
		"inactive": {Active: false, Head: "h1"}, "stale": {Active: true, Stale: true, Head: "h1"}, "other head": {Active: true, Head: "h9"},
	} {
		if v := AcceptProof(f.events, "n1", "h1", live(a)); v.Proven {
			t.Errorf("%s: %+v", name, v)
		}
	}
}

func TestGateAndReviewerFinalProofs(t *testing.T) {
	var events []engine.Event
	add := func(op string, d map[string]any) {
		events = append(events, engine.Event{Seq: len(events) + 1, Kind: engine.KindCall, CallID: op, Op: op, Event: engine.EvCompleted, Data: d})
	}
	add(OpGate, map[string]any{"head": "i9", "command": "make check", "range": "base..i9", "ok": true})
	add(OpReviewerFinal, map[string]any{"range": "base..i9", "criteria_digest": "crit-a"})
	if !GateProof(events, "i9", "make check", "base..i9").Proven {
		t.Error("the current integration head, the same command, the same range")
	}
	for name, v := range map[string]Verdict{
		"integration head moved": GateProof(events, "i10", "make check", "base..i10"),
		"other command":          GateProof(events, "i9", "make lint", "base..i9"),
		"other range":            GateProof(events, "i9", "make check", "other..i9"),
	} {
		if v.Proven {
			t.Errorf("gate, %s: %+v", name, v)
		}
	}
	if !ReviewerFinalProof(events, "base..i9", "crit-a").Proven {
		t.Error("the same integration range and criteria")
	}
	for name, v := range map[string]Verdict{
		"criteria changed": ReviewerFinalProof(events, "base..i9", "crit-b"),
		"range changed":    ReviewerFinalProof(events, "base..i10", "crit-a"),
	} {
		if v.Proven {
			t.Errorf("reviewer-final, %s: %+v", name, v)
		}
	}
	// a gate that failed proves nothing
	add(OpGate, map[string]any{"head": "i11", "command": "make check", "range": "base..i11", "ok": false})
	if GateProof(events, "i11", "make check", "base..i11").Proven {
		t.Error("a failed gate")
	}
}

func TestRecordCallProofsNeverResend(t *testing.T) {
	var events []engine.Event
	add := func(op, call, event string) {
		events = append(events, engine.Event{Seq: len(events) + 1, RunID: "run-1", Kind: engine.KindCall, CallID: call, Op: op, Event: event})
	}
	for _, op := range recordOps {
		call := "rec-" + op
		add(op, call, engine.EvCompleted)
		for i := 0; i < 3; i++ { // however often the run resumes
			d := DecideRecord(events, "run-1", call)
			if !d.Recorded || d.Resend || d.Row == nil || d.Row.CallID != call {
				t.Fatalf("%s: a recorded call is sent again: %+v", op, d)
			}
		}
	}
	// no row, or only the intent: live, under the call id and an idempotent request id that does not change
	add("crw.reportUp", "lost-intent", engine.EvPrepared)
	for _, call := range []string{"never-sent", "lost-intent"} {
		a, b := DecideRecord(events, "run-1", call), DecideRecord(events, "run-1", call)
		if a.Recorded || !a.Resend || a.CallID != call || a.RequestID == "" || a.RequestID != b.RequestID {
			t.Errorf("%s: %+v / %+v", call, a, b)
		}
	}
	if RequestID("run-1", "c_a") == RequestID("run-1", "c_b") || RequestID("run-1", "c_a") == RequestID("run-2", "c_a") {
		t.Error("the request id must tell calls and runs apart")
	}
	// a proof about the worktree is not consulted: a recording call whose row exists is never sent again
	if !IsRecordOp("crw.tierStep") || IsRecordOp(OpCommit) || IsRecordOp("agent") {
		t.Error("the record calls are releaseChild, escalate, reportUp, hold and tierStep")
	}
}

// placed is the fake backend with a placer: calls run in one fixed worktree, and a snapshot is the working snapshot.
type placed struct {
	*engine.Fake
	wt, start string
}

func (p *placed) Place(req engine.PlaceRequest) (engine.Placement, error) {
	if req.Family != "executor" || req.Continues != nil {
		return engine.Placement{Kind: engine.PlaceContinued}, nil
	}
	return engine.Placement{Kind: engine.PlaceFresh, Attempt: 1, Path: p.wt, Start: p.start}, nil
}
func (p *placed) Establish(context.Context, engine.PlaceRequest, engine.Placement) error { return nil }
func (p *placed) Recheck(context.Context, engine.Placement) error                        { return nil }
func (p *placed) Snapshot(ctx context.Context, path string) (string, error) {
	return Snapshot(ctx, ExecGit("git"), path)
}

func runPlaced(t *testing.T, f *fx, handler func(engine.FakeRequest) engine.FakeReply, body string) (*engine.Result, *engine.MemoryJournal) {
	t.Helper()
	fake := engine.NewFake()
	fake.Handler = handler
	j := engine.NewMemoryJournal()
	prog, err := script.Load(script.Source{Name: "t.js", Text: []byte("export const meta = { name: 't' }\n" + body)})
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := engine.Run(cctx, prog, engine.Config{Backend: &placed{Fake: fake, wt: f.wt, start: f.start}, Policy: engine.FakePolicy(), Journal: j, RunID: "run-held"})
	if err != nil {
		t.Fatal(err)
	}
	return res, j
}

// A build edits files and the host drops the response stream: the call is held, the snapshot of the worktree at that moment is
// recorded as P_held, the effect proof of the call stands on it, and a change made to a file while the call is held breaks it.
func TestHeldSnapshotIsOwnPendingState(t *testing.T) {
	f := newFx(t)
	f.events = nil // the rows come from the run
	res, j := runPlaced(t, f, func(r engine.FakeRequest) engine.FakeReply {
		write(t, f.wt, "a.txt", "half built\n", 0o644)
		write(t, f.wt, "src/partial.txt", "partial\n", 0o644)
		return engine.FakeReply{Fail: &engine.TurnFailure{CodexErrorInfo: "responseStreamDisconnected", Message: "stream closed"}}
	}, `const b = await agent.call('build it', { agentType: 'executor', node: 'n1', label: 'build' }); return [b.status, b.reason]`)
	var got []string
	if err := json.Unmarshal(res.Value, &got); err != nil || !slices.Equal(got, []string{"held", "host_infra_responseStreamDisconnected"}) {
		t.Fatalf("%s %v", res.Value, err)
	}
	var held engine.Event
	for _, ev := range j.Events() {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvHeld {
			held = ev
		}
	}
	pHeld, _ := held.Data["p_held"].(string)
	if pHeld == "" {
		t.Fatalf("the held row records no P_held: %v", held.Data)
	}
	if now := f.snapshot(); now != pHeld {
		t.Fatalf("P_held %s is not the snapshot of the files at the hold %s", pHeld, now)
	}
	callID := held.CallID
	np, err := f.prover.Node(ctx, j.Events(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	if !np.Proven(callID) || np.Calls[0].Event != engine.EvHeld || np.Calls[0].Pending != pHeld {
		t.Fatalf("the held call's own state proves it: %+v", np)
	}
	// a file changes while the call is held
	write(t, f.wt, "src/partial.txt", "changed during the hold\n", 0o644)
	np, err = f.prover.Node(ctx, j.Events(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	if np.Proven(callID) || np.InvalidFrom != callID {
		t.Errorf("a change during the hold must break the proof: %+v", np)
	}
}

// A completed executor call records its pending tree P, which is what the build/fix proof compares.
func TestCompletedExecutorCallRecordsPendingTree(t *testing.T) {
	f := newFx(t)
	f.events = nil
	res, j := runPlaced(t, f, func(r engine.FakeRequest) engine.FakeReply {
		write(t, f.wt, "a.txt", "built\n", 0o644)
		return engine.FakeReply{}
	}, `const b = await agent.call('build it', { agentType: 'executor', node: 'n1', label: 'build' }); return b.status`)
	if string(res.Value) != `"completed"` {
		t.Fatalf("%s", res.Value)
	}
	var done engine.Event
	for _, ev := range j.Events() {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted {
			done = ev
		}
	}
	if p, _ := done.Data["pending_tree"].(string); p == "" || p != f.snapshot() {
		t.Errorf("pending_tree %v, snapshot %s", done.Data["pending_tree"], f.snapshot())
	}
	np, err := f.prover.Node(ctx, j.Events(), "n1")
	if err != nil || !np.Proven(done.CallID) {
		t.Errorf("%+v %v", np, err)
	}
}

// The edits a null_model attempt left uncommitted stay in its retired worktree: neither the proof of the current attempt nor
// the foreign judgement reads them, and git is never asked about that directory.
func TestRetiredAttemptWorktreeNotProven(t *testing.T) {
	f := newFx(t)
	old := f.wt
	f.events = nil
	// attempt 1: t1 ends null_model with edits left in its worktree
	f.prepare("t1", engine.PlaceFresh, old, f.start)
	f.edit("a.txt", "half-done edit of the null_model attempt\n")
	f.edit("src/leftover.txt", "leftover\n")
	f.add(engine.Event{Kind: engine.KindCall, CallID: "t1", Op: "agent", Event: engine.EvNullModel, Data: map[string]any{"reason": "schema_invalid_after_repair"}})
	// attempt 2: a new worktree at the same start commit; the node's build t2 completes there
	f.wt = addWorktree(t, f.repo, "attempt2", f.start)
	f.prepare("t2", engine.PlaceFresh, f.wt, f.start)
	f.edit("src/c.txt", "the real build\n")
	f.complete("t2")
	np := f.prove()
	if np.Worktree != f.wt || np.Worktree == old {
		t.Fatalf("proved %s, the current attempt is %s", np.Worktree, f.wt)
	}
	expectProven(t, np, map[string]bool{"t2": true, "t1": false})
	if np.Foreign != "" || np.InvalidFrom != "" || len(np.Calls) != 1 {
		t.Errorf("the retired attempt's edits leaked into the judgement: %+v", np)
	}
	// and the retired directory is untouched by the proof, whatever it holds
	f.edit("src/c.txt", "the real build\n")
	if err := os.WriteFile(filepath.Join(old, "a.txt"), []byte("yet another edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	np = f.prove()
	expectProven(t, np, map[string]bool{"t2": true})
	if f.gitLog.touched(old) {
		t.Error("git was run in the retired attempt's worktree")
	}
	// the file the retired attempt left is not in the current snapshot
	if out := gitIn(t, f.wt, "ls-tree", "-r", "--name-only", np.Snapshot); out == "" || strings.Contains(out, "leftover.txt") {
		t.Errorf("snapshot of the current attempt: %q", out)
	}
}

// The working files are the state an earlier call left: that call stands and the later one, whose edits are gone, does not.
func TestEarlierCallStateLeavesLaterCallsUnproven(t *testing.T) {
	f := newFx(t)
	f.edit("a.txt", "built\n")
	f.complete("t1")
	f.prepare("t2", engine.PlaceContinued, f.wt, "")
	f.edit("src/c.txt", "fixed\n")
	f.complete("t2")
	expectProven(t, f.prove(), map[string]bool{"t1": true, "t2": true})
	f.edit("src/c.txt", "gamma\n") // the fix is undone
	np := f.prove()
	expectProven(t, np, map[string]bool{"t1": true, "t2": false})
	if np.InvalidFrom != "t2" || np.Foreign != "snapshot" {
		t.Errorf("%+v", np)
	}
	// the files back at the commit's tree, with nothing recorded after it: neither call is proven, and the commit stays
	if err := os.Remove(filepath.Join(f.wt, "a.txt")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.wt, "checkout", "-q", "--", "a.txt")
	np = f.prove()
	expectProven(t, np, map[string]bool{"t1": false, "t2": false})
	if np.InvalidFrom != "t1" {
		t.Errorf("the working files are the start commit's tree: %+v", np)
	}
}

// A node with no attempt worktree in the journal has nothing to prove against.
func TestNodeWithoutWorktreeIsAnError(t *testing.T) {
	f := newFx(t)
	if _, err := f.prover.Node(ctx, f.events, "n9"); err == nil || !errors.Is(err, ErrNoWorktree) {
		t.Errorf("%v", err)
	}
}
