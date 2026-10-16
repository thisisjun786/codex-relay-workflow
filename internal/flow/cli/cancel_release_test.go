package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The cancel of a run gives up its executions in one cancel (CRW-1573, D3.1): the run process that ends cancelled - whether a
// `crw flow cancel` whose wait expired, a cancel that was killed or a plain SIGTERM ended it - settles the run's open releases itself,
// under its run lock, after its world is closed. A later land of the node is then released, not refused as land_node_held.

// verifyBlock makes the crw.verify of the lab's runs block until its context is done, as the `crw-dev ci local` of the incident ran past
// the cancel; reached is closed when a blocked verify started, and on false lets the verifications run as the lab's verifier does.
type verifyBlock struct {
	on      atomic.Bool
	reached chan struct{}
	once    sync.Once
}

// blockVerify installs the blocking crw.verify over the lab's lane operations (newLaneLab's OpsFor), for this test.
func blockVerify(t *testing.T) *verifyBlock {
	t.Helper()
	b := &verifyBlock{reached: make(chan struct{})}
	b.on.Store(true)
	inner := OpsFor
	OpsFor = func(ctx context.Context, req LaneRequest) (LaneWorld, error) {
		lw, err := inner(ctx, req)
		if err != nil {
			return lw, err
		}
		// the lane's operations are named without the crw. prefix the script calls them by
		name := strings.TrimPrefix(ops.OpVerify, "crw.")
		op, ok := lw.Ops[name]
		if !ok {
			t.Fatalf("the lane has no %s operation", name)
		}
		lw.Ops[name] = blockingOp{Op: op, b: b}
		return lw, nil
	}
	t.Cleanup(func() { OpsFor = inner })
	return b
}

type blockingOp struct {
	engine.Op
	b *verifyBlock
}

func (o blockingOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	if !o.b.on.Load() {
		return o.Op.Run(ctx, req)
	}
	o.b.once.Do(func() { close(o.b.reached) })
	<-ctx.Done()
	return engine.OpResult{}, ctx.Err()
}

// cancelAtVerify sends SIGTERM on sigs once a blocked verify started.
func cancelAtVerify(b *verifyBlock, sigs chan os.Signal) {
	go func() {
		select {
		case <-b.reached:
			sigs <- syscall.SIGTERM
		case <-time.After(150 * time.Second):
		}
	}()
}

// processSigs runs a new run's process as `crw flow _run` does, in this process, with the signals the test sends it on sigs: what
// process() hands execute. It answers the exit status, stdout and stderr.
func (l *laneLab) processSigs(run string, sigs chan os.Signal) (int, string, string) {
	l.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	l.mu.Lock()
	l.cancel, l.killed = cancel, false
	l.processes++
	l.mu.Unlock()
	sel, err := ResolveState(l.w.state, l.host.SocketPath)
	if err != nil {
		l.t.Fatal(err)
	}
	spec, err := ReadSpec(sel.Path, run)
	if err != nil {
		l.t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	l.w.close()
	c := &command{Deps: l.td.Deps, ctx: ctx, stdin: strings.NewReader(""), stdout: &out, stderr: &errOut, name: ProcessCommand}
	code := c.execute(spec, sel.Path, false, sigs)
	l.w.open()
	return code, out.String(), errOut.String()
}

// settleRecorder records the settlements a command asks for and answers them through inner (nil: nothing settled).
type settleRecorder struct {
	mu    sync.Mutex
	inner SettleFunc
	runs  []string
	nodes [][]ops.ReleaseRef
	got   []Settlement
}

func (s *settleRecorder) settle(ctx context.Context, st *store.Store, runID string, nodes []ops.ReleaseRef) (Settlement, error) {
	var out Settlement
	var err error
	if s.inner != nil {
		out, err = s.inner(ctx, st, runID, nodes)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs = append(s.runs, runID)
	s.nodes = append(s.nodes, nodes)
	s.got = append(s.got, out)
	return out, err
}

func (s *settleRecorder) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

// startCancelledLand starts a land-head run of A-1 whose crw.verify blocks, cancels it with SIGTERM during the verify and answers the
// run, the process's exit status, its stdout and its stderr; the verifications run again afterwards.
func startCancelledLand(t *testing.T, l *laneLab, author string) (Spec, int, string, string) {
	t.Helper()
	b := blockVerify(t)
	run := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	sigs := make(chan os.Signal, 2)
	cancelAtVerify(b, sigs)
	code, out, errOut := l.processSigs(run.RunID, sigs)
	b.on.Store(false)
	res, ok := ReadResult(l.w.state, run.RunID)
	if !ok || res.ErrorKind != engine.ErrKindCancelled {
		t.Fatalf("the land was not cancelled during its verify: exit %d %+v\n%s%s", code, res, out, errOut)
	}
	return run, code, out, errOut
}

// TestLandHeadAfterACancelDuringVerifyReleases (CRW-1573, the repro): a land-head run of node A-1 is released the node and reaches
// crw.verify, which blocks; a SIGTERM ends the run cancelled. A second land-head run of A-1, with no second cancel, is released the node
// and lands the head - not refused as land_node_held naming the first run.
func TestLandHeadAfterACancelDuringVerifyReleases(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	head := l.outsideHead("A-1", "outside-a1")
	author := l.authorThread("claude-fake-sonnet", "high")
	run1, code, _, _ := startCancelledLand(t, l, author)
	if code != 1 {
		t.Errorf("the cancelled land exits %d, want 1", code)
	}
	// Cancellation now also releases the project tenure (CRW-1325). Starting
	// new work requires an explicit binding; the returned execution slot stays free.
	if _, err := (&registry.Registry{Store: l.w.st}).BindScope(ctx, "parent", "P", registry.Endpoint{TaskID: l.owner, HostID: "host"}); err != nil {
		t.Fatal(err)
	}
	run2 := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	res := l.runToEnd(run2.RunID)
	v := landValueOf(t, res)
	if v.State != "landed" || v.Head != head {
		t.Fatalf("the land after the cancelled land of %s: %+v\n%s", run1.RunID, v, res.Value)
	}
}

// failingFinish is an upward hook whose RunFinished fails.
type failingFinish struct{}

func (failingFinish) RunStarted(context.Context, string) error     { return nil }
func (failingFinish) BatchCommitted(context.Context, string) error { return nil }
func (failingFinish) RunFinished(context.Context, string, string) error {
	return errors.New("the completion notice could not be sent")
}

// TestCancelledProcessSettlesEvenWhenTheFinishHookFails (CRW-1573, D3.1): the release does not depend on the finish hook. With a hook whose
// RunFinished fails, the process of the cancelled land still gives up the node it released: Settle is called once, for the run, with
// its open release of A-1, and closes that execution; the journal holds the hook's failure row.
func TestCancelledProcessSettlesEvenWhenTheFinishHookFails(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	l.outsideHead("A-1", "outside-a1")
	author := l.authorThread("claude-fake-sonnet", "high")
	inner := l.td.World
	l.td.World = func(ctx context.Context, req WorldRequest) (World, error) {
		w, err := inner(ctx, req)
		w.Upward = failingFinish{}
		return w, err
	}
	rec := &settleRecorder{inner: SchedulerSettle}
	l.td.Settle = rec.settle
	run, _, out, errOut := startCancelledLand(t, l, author)
	rec.mu.Lock()
	runs, nodes, got := rec.runs, rec.nodes, rec.got
	rec.mu.Unlock()
	if len(runs) != 1 || runs[0] != run.RunID {
		t.Fatalf("the process settled %v, want run %s once\n%s%s", runs, run.RunID, out, errOut)
	}
	if len(nodes[0]) != 1 || nodes[0][0].Node != l.nodeOf["A-1"] || nodes[0][0].Plan != l.plan || nodes[0][0].Execution == "" {
		t.Errorf("the open releases settled: %+v", nodes[0])
	}
	if got[0].Closed != 1 {
		t.Errorf("the settlement: %+v, want one execution closed", got[0])
	}
	if !strings.Contains(out, "1 execution(s) of run "+run.RunID+" given up") {
		t.Errorf("the process does not say what it gave up: %q", out)
	}
	failed := 0
	for _, e := range l.events() {
		if e.Kind == engine.KindFlow && e.Event == engine.EvUpwardHookFailed && str(e.Data, "point") == "finished" {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("%d upward_hook_failed rows of the finish, want 1", failed)
	}
}

// TestOwnSettleThenCancelCLIFindsNothing (CRW-1573, D3.1): after the process of the cancelled land settled its own cancel, `crw flow
// cancel` of the run finds it cancelled with nothing left: it exits 1 "nothing to cancel", and the settle it asks for gives up nothing.
func TestOwnSettleThenCancelCLIFindsNothing(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	l.outsideHead("A-1", "outside-a1")
	author := l.authorThread("claude-fake-sonnet", "high")
	rec := &settleRecorder{inner: SchedulerSettle}
	l.td.Settle = rec.settle
	run, _, out, errOut := startCancelledLand(t, l, author)
	if rec.calls() != 1 {
		t.Fatalf("the process settled %d times, want once\n%s%s", rec.calls(), out, errOut)
	}
	code, cout, cerr := l.td.do("cancel", run.RunID, "--state", l.w.state, "--socket", l.host.SocketPath)
	if code != 1 || !strings.Contains(cerr, "nothing to cancel") {
		t.Errorf("cancel after the process settled: exit %d %q %q", code, cout, cerr)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.got) != 2 || rec.got[1] != (Settlement{}) {
		t.Errorf("the settlements: %+v, want the cancel's to give up nothing", rec.got)
	}
}

// TestCompletedRunDoesNotSettle (CRW-1573, D3.1): only a cancelled end settles in the process. A completed run and a paused run end
// without the process asking for a settlement.
func TestCompletedRunDoesNotSettle(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		pw := newProcessWorld(t, twoCalls)
		rec := &settleRecorder{}
		pw.td.Settle = rec.settle
		spec := pw.spec("run-done", nil)
		if code, errOut := pw.execute(spec, false, nil); code != 0 {
			t.Fatalf("exit %d %s", code, errOut)
		}
		if info := pw.info("run-done"); info.State != StateCompleted {
			t.Fatalf("the run: %+v", info)
		}
		if n := rec.calls(); n != 0 {
			t.Errorf("a completed run settled %d times", n)
		}
	})
	t.Run("paused", func(t *testing.T) {
		pw := newProcessWorld(t, twoCalls)
		rec := &settleRecorder{}
		pw.td.Settle = rec.settle
		spec := pw.spec("run-paused", nil)
		sigs := make(chan os.Signal, 2)
		pw.handle = func(r engine.FakeRequest) engine.FakeReply {
			if r.Kind == "turn" && strings.Contains(r.Prompt, "first") {
				sigs <- syscall.SIGUSR1
				time.Sleep(200 * time.Millisecond)
			}
			return engine.FakeReply{}
		}
		if code, errOut := pw.execute(spec, false, sigs); code != 0 {
			t.Fatalf("exit %d %s", code, errOut)
		}
		if info := pw.info("run-paused"); info.State != StatePaused {
			t.Fatalf("the run: %+v", info)
		}
		if n := rec.calls(); n != 0 {
			t.Errorf("a paused run settled %d times", n)
		}
	})
}

// TestRunEndCleansWorktreesAndKeepsEvidence (CRW-1585): a run that ends - landed or cancelled - drops
// only its disposable working copies (land-wt, node-wt, wt and the calls' wt-N worktrees) and keeps
// everything durable: spec, result, journal, call records, the landed head and the cleanup ref.
func TestRunEndCleansWorktreesAndKeepsEvidence(t *testing.T) {
	check := func(t *testing.T, l *laneLab, run Spec, landHead string, hadWorktree *atomic.Bool) {
		t.Helper()
		dir := RunDir(l.w.state, run.RunID)
		if !hadWorktree.Load() {
			t.Fatal("the run never held a land-wt to clean")
		}
		for _, root := range []string{"land-wt", "node-wt", "wt"} {
			if _, err := os.Lstat(filepath.Join(dir, root)); !os.IsNotExist(err) {
				t.Errorf("the worktree root %s survived the run's end", root)
			}
		}
		// the calls' own record stays; only a call worktree directory is removed
		if got, err := os.ReadFile(filepath.Join(dir, "calls", "kept", "record.json")); err != nil || string(got) != `{"evidence":"keep"}`+"\n" {
			t.Errorf("a call record was removed: %v %q", err, got)
		}
		if _, err := os.Lstat(filepath.Join(dir, "calls", "kept", "wt-1")); !os.IsNotExist(err) {
			t.Errorf("the call worktree wt-1 survived the run's end")
		}
		if _, err := ReadSpec(l.w.state, run.RunID); err != nil {
			t.Errorf("the spec is gone: %v", err)
		}
		if _, ok := ReadResult(l.w.state, run.RunID); !ok {
			t.Error("the result is gone")
		}
		events, _, err := journal.Load(context.Background(), l.w.st, run.RunID)
		if err != nil || len(events) == 0 {
			t.Fatalf("the journal is gone: %v", err)
		}
		var runFinished, callOp bool
		for _, ev := range events {
			if ev.Kind == engine.KindRun && ev.Event == "finished" {
				runFinished = true
			}
			if ev.Kind == engine.KindCall {
				callOp = true
			}
		}
		if !runFinished || !callOp {
			t.Errorf("the journal lost its rows: finished=%v call=%v", runFinished, callOp)
		}
		if err := exec.Command("git", "-C", l.repo, "cat-file", "-e", landHead+"^{commit}").Run(); err != nil {
			t.Errorf("the landed head is gone: %v", err)
		}
		out, err := exec.Command("git", "-C", l.repo, "for-each-ref", "refs/crw/flow/"+run.RunID+"/cleanup/").Output()
		if err != nil {
			t.Fatalf("for-each-ref: %v", err)
		}
		if len(out) == 0 {
			t.Errorf("no cleanup ref survived under refs/crw/flow/%s/cleanup/", run.RunID)
		}
	}
	seed := func(t *testing.T, l *laneLab, run string, hadWorktree *atomic.Bool) {
		t.Helper()
		dir := RunDir(l.w.state, run)
		l.afterRow = func(ev engine.Event) {
			if ev.Kind == engine.KindRun && ev.Event == "started" {
				if err := os.MkdirAll(filepath.Join(dir, "calls", "kept", "wt-1"), 0o700); err != nil {
					t.Error(err)
					return
				}
				if err := os.WriteFile(filepath.Join(dir, "calls", "kept", "record.json"), []byte(`{"evidence":"keep"}`+"\n"), 0o600); err != nil {
					t.Error(err)
					return
				}
				if err := os.WriteFile(filepath.Join(dir, "calls", "kept", "wt-1", "cache"), []byte("temporary\n"), 0o600); err != nil {
					t.Error(err)
				}
			}
			if ev.Kind == engine.KindRun && ev.Event == "finished" {
				if _, err := os.Lstat(filepath.Join(dir, "land-wt")); err == nil {
					hadWorktree.Store(true)
				}
			}
		}
	}

	t.Run("completed", func(t *testing.T) {
		l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
		head := l.outsideHead("A-1", "outside-a1")
		author := l.authorThread("claude-fake-sonnet", "high")
		var had atomic.Bool
		run := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
		seed(t, l, run.RunID, &had)
		res := l.runToEnd(run.RunID)
		var landValue struct {
			State string `json:"state"`
		}
		_ = json.Unmarshal([]byte(res.Value), &landValue)
		if res.Status != "completed" || landValue.State != "landed" {
			t.Fatalf("the land did not complete: %+v", res)
		}
		check(t, l, run, head, &had)
	})

	t.Run("cancelled", func(t *testing.T) {
		l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
		head := l.outsideHead("A-1", "outside-a1")
		author := l.authorThread("claude-fake-sonnet", "high")
		var had atomic.Bool
		b := blockVerify(t)
		run := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
		seed(t, l, run.RunID, &had)
		sigs := make(chan os.Signal, 2)
		cancelAtVerify(b, sigs)
		code, _, _ := l.processSigs(run.RunID, sigs)
		b.on.Store(false)
		res, ok := ReadResult(l.w.state, run.RunID)
		if !ok || res.ErrorKind != engine.ErrKindCancelled || code != 1 {
			t.Fatalf("the land was not cancelled during its verify: exit %d %+v", code, res)
		}
		check(t, l, run, head, &had)
	})
}

// TestCleanupWorktreesDefaultsToPreview (CRW-1585): `crw flow cleanup-worktrees RUN` with no flag
// prints what it would remove and removes nothing; only --apply deletes, and only the disposable
// roots - records, refs and anything the links point at are untouched, and the removed work is kept
// at a new cleanup ref.
func TestCleanupWorktreesDefaultsToPreview(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	l.outsideHead("A-1", "outside-a1")
	author := l.authorThread("claude-fake-sonnet", "high")
	run := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	l.runToEnd(run.RunID)
	dir := RunDir(l.w.state, run.RunID)
	manual := filepath.Join(dir, "land-wt", "manual")
	if err := exec.Command("git", "-C", l.repo, "worktree", "add", "--detach", manual, "HEAD").Run(); err != nil {
		t.Fatalf("worktree add: %v", err)
	}
	if err := os.WriteFile(filepath.Join(manual, "kept.txt"), []byte("keep this work until --apply\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(dir, "node-wt")
	if err := os.MkdirAll(linkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(linkDir, "node")); err != nil {
		t.Fatal(err)
	}
	// the complete state before the preview: bytes, alias text, journal, refs, HEAD
	keptBytes := cleanupTestRead(t, filepath.Join(manual, "kept.txt"))
	sentinelBytes := cleanupTestRead(t, filepath.Join(outside, "sentinel"))
	alias, err := os.Readlink(filepath.Join(linkDir, "node"))
	if err != nil {
		t.Fatal(err)
	}
	specBytes := cleanupTestRead(t, filepath.Join(dir, "run.json"))
	resultBytes := cleanupTestRead(t, filepath.Join(dir, "result.json"))
	lockBytes := cleanupTestRead(t, filepath.Join(dir, "run.lock"))
	journalJSON := cleanupTestJournal(t, l.w, run.RunID)
	refsBefore := cleanupTestGit(t, l.repo, "for-each-ref", "--format=%(refname)", "refs/crw/flow/"+run.RunID+"/")
	headOut := cleanupTestGit(t, l.repo, "rev-parse", "HEAD")

	code, out, errOut := l.td.do("cleanup-worktrees", run.RunID, "--state", l.w.state, "--socket", l.host.SocketPath)
	if code != 0 {
		t.Fatalf("preview: exit %d %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "preview") || !strings.Contains(out, "land-wt") || !strings.Contains(out, "bytes") {
		t.Errorf("preview output %q", out)
	}
	// nothing changed: bytes, alias, journal, refs all identical
	for name, path := range map[string]string{"kept": filepath.Join(manual, "kept.txt"), "sentinel": filepath.Join(outside, "sentinel"),
		"spec": filepath.Join(dir, "run.json"), "result": filepath.Join(dir, "result.json"), "lock": filepath.Join(dir, "run.lock")} {
		got, err := os.ReadFile(path)
		want := map[string][]byte{"kept": keptBytes, "sentinel": sentinelBytes, "spec": specBytes, "result": resultBytes, "lock": lockBytes}[name]
		if err != nil || string(got) != string(want) {
			t.Errorf("the preview changed %s: %v", name, err)
		}
	}
	if got, err := os.Readlink(filepath.Join(linkDir, "node")); err != nil || got != alias {
		t.Errorf("the preview changed the link: %q %v", got, err)
	}
	if got := cleanupTestJournal(t, l.w, run.RunID); !bytes.Equal(got, journalJSON) {
		t.Error("the preview changed the journal")
	}
	if got := cleanupTestGit(t, l.repo, "for-each-ref", "--format=%(refname)", "refs/crw/flow/"+run.RunID+"/"); got != refsBefore {
		t.Errorf("the preview moved refs")
	}

	code, out, errOut = l.td.do("cleanup-worktrees", run.RunID, "--apply", "--state", l.w.state, "--socket", l.host.SocketPath)
	if code != 0 {
		t.Fatalf("apply: exit %d %s %s", code, out, errOut)
	}
	for _, root := range []string{"land-wt", "node-wt", "calls/kept/wt-1", "wt"} {
		if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(root))); !os.IsNotExist(err) {
			t.Errorf("--apply left %s", root)
		}
	}
	if got, err := os.ReadFile(filepath.Join(outside, "sentinel")); err != nil || string(got) != string(sentinelBytes) {
		t.Errorf("--apply followed the link: %v %q", err, got)
	}
	for name, want := range map[string][]byte{"run.json": specBytes, "result.json": resultBytes} {
		if got := cleanupTestRead(t, filepath.Join(dir, name)); !bytes.Equal(got, want) {
			t.Errorf("--apply changed %s", name)
		}
	}
	if got := cleanupTestJournal(t, l.w, run.RunID); !bytes.Equal(got, journalJSON) {
		t.Error("--apply changed the journal")
	}
	// the removed work is preserved at a new cleanup ref
	refsAfter, err := exec.Command("git", "-C", l.repo, "for-each-ref", "--format=%(refname)", "refs/crw/flow/"+run.RunID+"/cleanup/").Output()
	if err != nil || len(refsAfter) == 0 {
		t.Fatalf("no cleanup ref after --apply: %v", err)
	}
	head := strings.TrimSpace(headOut)
	before := map[string]bool{}
	for _, ref := range strings.Fields(refsBefore) {
		before[ref] = true
	}
	found := false
	for _, ref := range strings.Fields(string(refsAfter)) {
		if before[ref] {
			continue
		}
		if err := exec.Command("git", "-C", l.repo, "merge-base", "--is-ancestor", head, ref).Run(); err != nil {
			continue
		}
		show, err := exec.Command("git", "-C", l.repo, "show", ref+":kept.txt").Output()
		if err == nil && string(show) == string(keptBytes) {
			found = true
		}
	}
	if !found {
		t.Errorf("no cleanup ref preserves kept.txt on the old head: %q", refsAfter)
	}
	spec, err := ReadSpec(l.w.state, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	events, _, err := journal.Load(context.Background(), l.w.st, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := TakeLock(l.w.state, run.RunID, "test-restore", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if err := restoreCleanedWorkdirs(context.Background(), l.w.state, spec, events); err != nil {
		t.Fatal(err)
	}
	if got := cleanupTestRead(t, filepath.Join(manual, "kept.txt")); !bytes.Equal(got, keptBytes) {
		t.Errorf("restore lost the uncommitted file: %q", got)
	}
	if got := strings.TrimSpace(cleanupTestGit(t, manual, "rev-parse", "HEAD")); got != head {
		t.Errorf("restore moved HEAD: %s, want %s", got, head)
	}
	if got := cleanupTestJournal(t, l.w, run.RunID); !bytes.Equal(got, journalJSON) {
		t.Error("restore changed the journal")
	}
	if err := os.WriteFile(filepath.Join(manual, "kept.txt"), []byte("edited after restore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreCleanedWorkdirs(context.Background(), l.w.state, spec, events); err != nil {
		t.Fatal(err)
	}
	if got := cleanupTestRead(t, filepath.Join(manual, "kept.txt")); string(got) != "edited after restore\n" {
		t.Errorf("repeat restore overwrote existing work: %q", got)
	}
}

func cleanupTestRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func cleanupTestGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	raw, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, raw)
	}
	return string(raw)
}

func cleanupTestJournal(t *testing.T, w *world, run string) []byte {
	t.Helper()
	events, _, err := journal.Load(context.Background(), w.st, run)
	if err != nil || len(events) == 0 {
		t.Fatalf("journal is missing: %v", err)
	}
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCleanupWorktreesKeepsActiveAndResumableRuns(t *testing.T) {
	for _, state := range []string{"live", "paused", "held", "dead", "cancelled-unsettled", "failed"} {
		t.Run(state, func(t *testing.T) {
			w := newWorld(t)
			r := w.newRun("run-keep", "owner")
			switch state {
			case "live":
				r.finish(engine.RunCompleted, "")
			case "paused":
				r.j.Append(engine.Event{RunID: r.id, Kind: engine.KindRun, Event: "paused", Data: map[string]any{"status": engine.RunPaused}})
			case "held":
				r.hold("call-1", "executor", "node-1")
				r.finish(engine.RunCompleted, "")
			case "cancelled-unsettled":
				r.hold("call-1", "executor", "node-1")
				r.finish(engine.RunScriptError, engine.ErrKindCancelled)
			case "failed":
				r.finish(engine.RunScriptError, "")
			}
			r.close()
			if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: r.id}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(RunDir(w.state, r.id), "land-wt", "keep")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("keep\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if state == "live" {
				lock, err := TakeLock(w.state, r.id, "_run", time.Now())
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Release()
			}
			before := cleanupTestJournal(t, w, r.id)
			code, out, errOut := w.deps().do("cleanup-worktrees", r.id, "--apply", "--state", w.state)
			if code == 0 {
				t.Fatalf("unsafe run accepted: %s%s", out, errOut)
			}
			if got := cleanupTestRead(t, path); string(got) != "keep\n" {
				t.Errorf("worktree changed: %q", got)
			}
			if got := cleanupTestJournal(t, w, r.id); !bytes.Equal(got, before) {
				t.Error("cleanup changed the run's journal")
			}
		})
	}
}

func TestCleanupWorktreesRejectsLinkedAncestors(t *testing.T) {
	for _, ancestor := range []string{"run", "calls"} {
		t.Run(ancestor, func(t *testing.T) {
			w := newWorld(t)
			r := w.newRun("run-linked", "owner")
			r.finish(engine.RunCompleted, "")
			r.close()
			if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: r.id}); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			dir := RunDir(w.state, r.id)
			target := filepath.Join(outside, "target")
			if ancestor == "run" {
				if err := os.Rename(dir, target); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				dir = filepath.Join(dir, "calls")
			}
			if err := os.WriteFile(filepath.Join(target, "sentinel"), []byte("outside\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(target, "run.lock"), []byte("outside lock\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, dir); err != nil {
				t.Fatal(err)
			}
			code, out, errOut := w.deps().do("cleanup-worktrees", r.id, "--apply", "--state", w.state)
			if code == 0 {
				t.Fatalf("linked ancestor accepted: %s%s", out, errOut)
			}
			for name, want := range map[string]string{"sentinel": "outside\n", "run.lock": "outside lock\n"} {
				if got := cleanupTestRead(t, filepath.Join(target, name)); string(got) != want {
					t.Errorf("outside %s changed: %q", name, got)
				}
			}
		})
	}
}

func TestCleanupWorktreesUnlinksRootAliasOnly(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint(broken), func(t *testing.T) {
			w := newWorld(t)
			r := w.newRun("run-alias", "owner")
			r.finish(engine.RunCompleted, "")
			r.close()
			if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: r.id}); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("outside\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			target := outside
			if broken {
				target = filepath.Join(outside, "gone")
			}
			alias := filepath.Join(RunDir(w.state, r.id), "node-wt")
			if err := os.Symlink(target, alias); err != nil {
				t.Fatal(err)
			}
			td := w.deps()
			if code, out, errOut := td.do("cleanup-worktrees", r.id, "--state", w.state); code != 0 || !strings.Contains(out, "bytes=0") {
				t.Fatalf("preview alias: %d %s%s", code, out, errOut)
			}
			if got, err := os.Readlink(alias); err != nil || got != target {
				t.Fatalf("preview changed alias: %q %v", got, err)
			}
			if code, out, errOut := td.do("cleanup-worktrees", r.id, "--apply", "--state", w.state); code != 0 {
				t.Fatalf("unlink alias: %d %s%s", code, out, errOut)
			}
			if _, err := os.Lstat(alias); !os.IsNotExist(err) {
				t.Errorf("alias survived: %v", err)
			}
			if got := cleanupTestRead(t, filepath.Join(outside, "sentinel")); string(got) != "outside\n" {
				t.Errorf("target changed: %q", got)
			}
		})
	}
}

func TestCancelDeadCleansWorktrees(t *testing.T) {
	w := newWorld(t)
	r := w.newRun("run-idle-cancel", "owner")
	r.close()
	if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: r.id}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(RunDir(w.state, r.id), "land-wt")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache"), []byte("cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := w.deps().do("cancel", r.id, "--state", w.state); code != 0 {
		t.Fatalf("idle cancel: %d %s%s", code, out, errOut)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("idle cancel kept worktree: %v", err)
	}
	if _, err := ReadSpec(w.state, r.id); err != nil {
		t.Fatal(err)
	}
	rows, _, err := journal.Load(ctx, w.st, r.id)
	if err != nil || Summarize(RunRecord{RunID: r.id}, rows, false, 0).State != StateCancelled {
		t.Fatalf("cancelled journal was lost: %v", err)
	}
}

func TestCleanupWorktreesKeepsRepositoriesItCannotPreserve(t *testing.T) {
	for _, kind := range []string{"standalone", "foreign-linked", "nested", "bare"} {
		t.Run(kind, func(t *testing.T) {
			l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
			l.outsideHead("A-1", "outside-a1")
			author := l.authorThread("claude-fake-sonnet", "high")
			run := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
			l.runToEnd(run.RunID)
			root := filepath.Join(RunDir(l.w.state, run.RunID), "land-wt")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			repo := filepath.Join(root, "result")
			if kind == "foreign-linked" || kind == "bare" {
				repo = t.TempDir()
			} else if kind == "nested" {
				parent := filepath.Join(root, "owned")
				cleanupTestGit(t, l.repo, "worktree", "add", "--detach", parent, "HEAD")
				repo = filepath.Join(parent, "nested")
			}
			cleanupTestGit(t, l.repo, "init", repo)
			if err := os.WriteFile(filepath.Join(repo, "kept.txt"), []byte("independent commit\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cleanupTestGit(t, repo, "add", "kept.txt")
			cleanupTestGit(t, repo, "-c", "user.name=cleanup-test", "-c", "user.email=cleanup@example.invalid",
				"-c", "commit.gpgsign=false", "commit", "-m", "result to retain")
			if kind == "foreign-linked" {
				linked := filepath.Join(root, "foreign")
				cleanupTestGit(t, repo, "worktree", "add", "--detach", linked, "HEAD")
				repo = linked
			} else if kind == "bare" {
				bare := filepath.Join(root, "bare")
				cleanupTestGit(t, l.repo, "clone", "--bare", repo, bare)
				repo = bare
			}
			head := cleanupTestGit(t, repo, "rev-parse", "HEAD")
			ownerHead := cleanupTestGit(t, l.repo, "rev-parse", "HEAD")
			code, out, errOut := l.td.do("cleanup-worktrees", run.RunID, "--apply", "--state", l.w.state, "--socket", l.host.SocketPath)
			if code == 0 {
				t.Fatalf("cleanup accepted an unpreserved repository: %s%s", out, errOut)
			}
			if got := cleanupTestGit(t, repo, "rev-parse", "HEAD"); got != head {
				t.Errorf("repository head changed: %s", got)
			}
			if got := cleanupTestGit(t, repo, "show", "HEAD:kept.txt"); got != "independent commit\n" {
				t.Errorf("independent commit was lost: %q", got)
			}
			if got := cleanupTestGit(t, l.repo, "rev-parse", "HEAD"); got != ownerHead {
				t.Errorf("owner checkout moved: %s", got)
			}
		})
	}
}

func TestCleanupWorktreesAllSkipsActive(t *testing.T) {
	w := newWorld(t)
	for _, id := range []string{"run-done", "run-live"} {
		r := w.newRun(id, "owner")
		if id == "run-done" {
			r.finish(engine.RunCompleted, "")
		}
		r.close()
		if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: id}); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(RunDir(w.state, id), "land-wt")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cache"), []byte(id), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := TakeLock(w.state, "run-live", "_run", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	beforeLive := cleanupTestJournal(t, w, "run-live")
	code, out, errOut := w.deps().do("cleanup-worktrees", "--apply", "--state", w.state)
	if code != 0 || !strings.Contains(out, "removed run=run-done") || !strings.Contains(out, "kept run=run-live") {
		t.Fatalf("cleanup all: %d %s%s", code, out, errOut)
	}
	if _, err := os.Lstat(filepath.Join(RunDir(w.state, "run-done"), "land-wt")); !os.IsNotExist(err) {
		t.Errorf("completed run's worktree survived: %v", err)
	}
	if got := cleanupTestRead(t, filepath.Join(RunDir(w.state, "run-live"), "land-wt", "cache")); string(got) != "run-live" {
		t.Errorf("active run changed: %q", got)
	}
	if got := cleanupTestJournal(t, w, "run-live"); !bytes.Equal(got, beforeLive) {
		t.Error("active run's journal changed")
	}
}

// TestRestoreCleanedWorktreesRejectsUnsafeRecords: a cleanup record naming a path outside the run's
// disposable roots, or a root held by a link, is refused before any Git probe or restore.
func TestRestoreCleanedWorktreesRejectsUnsafeRecords(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(fmt.Sprint(linked), func(t *testing.T) {
			state := t.TempDir()
			spec := Spec{Schema: SpecSchema, RunID: "run-restore"}
			if err := WriteSpec(state, spec); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("outside\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := openCleanupRoot(state, spec.RunID)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			row := cleanupSnapshot{Path: "../outside", Head: strings.Repeat("a", 40), Snapshot: strings.Repeat("b", 40), Commit: strings.Repeat("c", 40)}
			if linked {
				row.Path = "wt/saved"
				if err := root.Symlink(outside, "wt"); err != nil {
					t.Fatal(err)
				}
			}
			key := sha256.Sum256([]byte(row.Path + "\x00" + row.Head + "\x00" + row.Snapshot))
			row.Ref = fmt.Sprintf("refs/crw/flow/%s/cleanup/%x", spec.RunID, key)
			if err := recordCleanup(root, row); err != nil {
				t.Fatal(err)
			}
			// Parent validation must precede any Git probe; this directory is not a repository.
			spec.Repo = outside
			err = restoreCleanedWorkdirs(context.Background(), state, spec, nil)
			want := "invalid cleanup path"
			if linked {
				want = "not a real directory"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("unsafe restore: %v, want %q", err, want)
			}
			if got := cleanupTestRead(t, filepath.Join(outside, "sentinel")); string(got) != "outside\n" {
				t.Fatalf("outside changed: %q", got)
			}
			if _, err := os.Lstat(filepath.Join(outside, "saved")); !os.IsNotExist(err) {
				t.Fatalf("restored through link: %v", err)
			}
		})
	}
}

// TestCleanupWorktreesPreservesQACaptures (CRW-1585 P1): ignored QA evidence under a disposable root
// and completed crw.qaCapture results are archived to immutable cleanup-artifacts blobs before the
// checkout is removed, journaled references stay usable, and a resume restores the files at their
// original paths. A corrupt blob refuses the restore without publishing a partial checkout.
func TestCleanupWorktreesPreservesQACaptures(t *testing.T) {
	w := newWorld(t)
	r := w.newRun("run-qa-artifacts", "owner")
	repo := t.TempDir()
	cleanupTestGit(t, repo, "init", "-q")
	cleanupTestGit(t, repo, "config", "user.name", "cleanup")
	cleanupTestGit(t, repo, "config", "user.email", "cleanup@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".crw/\nqa-output/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupTestGit(t, repo, "add", ".")
	cleanupTestGit(t, repo, "-c", "commit.gpgsign=false", "commit", "-qm", "base")
	spec := Spec{Schema: SpecSchema, RunID: r.id, Repo: repo}
	if err := WriteSpec(w.state, spec); err != nil {
		t.Fatal(err)
	}
	dir := RunDir(w.state, r.id)
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(dir, "wt", "qa")
	cleanupTestGit(t, repo, "worktree", "add", "--detach", wt, "HEAD")
	files := map[string]string{
		".crw/evidence/session/qa/home/invocation.txt": "capture home at 1280x720\n",
		".crw/evidence/session/qa/home/shot.png":       "\x89PNG\r\n\x1a\nreal capture bytes\n",
		".crw/evidence/session/qa/home/verdict.json":   "{\"artifactRefs\":[\"shot.png\"],\"verdict\":\"PASS\"}\n",
		"qa-output/response.txt":                       "HTTP/1.1 200 OK\r\n\r\ncaptured body\n",
	}
	for path, body := range files {
		target := filepath.Join(wt, path)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	view := ops.QACaptureView{OK: true, Root: wt, Captures: []ops.QACaptureEntry{
		{Path: "qa-output/response.txt", Abs: filepath.Join(wt, "qa-output/response.txt"), Kind: "http"},
	}}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	r.op("capture", "crw.qaCapture", map[string]any{"result": string(raw)})
	r.finish(engine.RunCompleted, "")
	r.close()
	before := cleanupTestJournal(t, w, r.id)
	td := w.deps()
	if code, out, errOut := td.do("cleanup-worktrees", r.id, "--apply", "--state", w.state); code != 0 {
		t.Fatalf("cleanup: %d %s%s", code, out, errOut)
	}
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Fatalf("checkout survived: %v", err)
	}
	records := cleanupTestRead(t, filepath.Join(dir, "cleanup-artifacts.jsonl"))
	seen := map[string]bool{}
	for _, line := range bytes.Split(bytes.TrimSpace(records), []byte("\n")) {
		var a struct {
			Path, Blob, SHA256, OriginalPath string
		}
		if err := json.Unmarshal(line, &a); err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel("wt/qa", a.Path)
		if err != nil {
			t.Fatal(err)
		}
		want, ok := files[rel]
		if !ok {
			continue
		}
		content := cleanupTestRead(t, filepath.Join(dir, a.Blob))
		sum := sha256.Sum256(content)
		original := filepath.Join(realDir, a.Path)
		if rel == "qa-output/response.txt" {
			original = view.Captures[0].Abs
		}
		if string(content) != want || a.SHA256 != fmt.Sprintf("%x", sum) || a.OriginalPath != original {
			t.Fatalf("capture reference/content lost: %+v %q", a, content)
		}
		seen[rel] = true
	}
	if len(seen) != len(files) {
		t.Fatalf("archived %v, want %v", seen, files)
	}
	if got := cleanupTestJournal(t, w, r.id); !bytes.Equal(got, before) {
		t.Fatal("cleanup rewrote captured references")
	}
	events, _, err := journal.Load(context.Background(), w.st, r.id)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := TakeLock(w.state, r.id, "test-restore", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	var first struct {
		Blob string
	}
	if err := json.Unmarshal(bytes.Split(bytes.TrimSpace(records), []byte("\n"))[0], &first); err != nil {
		t.Fatal(err)
	}
	blobPath := filepath.Join(dir, first.Blob)
	blobBytes := cleanupTestRead(t, blobPath)
	if err := os.WriteFile(blobPath, []byte("corrupt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreCleanedWorkdirs(context.Background(), w.state, spec, events); err == nil {
		t.Fatal("corrupt QA evidence was accepted")
	}
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Fatalf("failed artifact restoration published a checkout: %v", err)
	}
	if err := os.WriteFile(blobPath, blobBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreCleanedWorkdirs(context.Background(), w.state, spec, events); err != nil {
		t.Fatal(err)
	}
	for path, want := range files {
		if got := cleanupTestRead(t, filepath.Join(wt, path)); string(got) != want {
			t.Fatalf("restored capture %s: %q", path, got)
		}
	}
	if got := cleanupTestJournal(t, w, r.id); !bytes.Equal(got, before) {
		t.Fatal("restore rewrote captured references")
	}
}
