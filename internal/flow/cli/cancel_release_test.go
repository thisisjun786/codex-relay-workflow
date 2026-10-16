package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
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
