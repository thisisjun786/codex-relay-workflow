package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The follow-ups of CRW-1277 (CRW-1285): the lost epoch as an error kind that ends the run, the managed child start with the run's host
// settings, the required commands refused before the run starts, and the integration verifier of the shipped lane.

// TestEpochLostEndsTheRunWithItsReason (CRW-1285 (1)): a fenced write the run makes after another session claimed its plan is the error
// kind epoch_lost (ErrEpochLost, the closed reason in its message, the stale_coordinator_epoch refusal kept), and no other error is. The
// run ends there with the reason: a release answers it as a node not released, an acceptance as the lane's stop, and the run's result is
// stopped with reason epoch_lost; nothing escalates, nothing more is written at the relay.
func TestEpochLostEndsTheRunWithItsReason(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1"})
	r := l.relayFor("run-lost000001", l.handoffBytes(nil), 1)
	if res, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: l.nodeOf["A-1"], RunID: r.RunID, RequestID: "r1"}); err != nil || !res.Released {
		t.Fatalf("release: %+v %v", res, err)
	}
	if _, err := (&dagsched.Scheduler{Store: l.w.st}).ClaimEpoch(ctx, l.plan, dagsched.ClaimInput{Actor: labOwner, SessionNonce: "session-2"}); err != nil {
		t.Fatal(err)
	}
	_, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: l.nodeOf["A-2"], RunID: r.RunID, RequestID: "r2"})
	if !errors.Is(err, ErrEpochLost) || ops.ReasonOf(err.Error()) != "epoch_lost" || !strings.Contains(err.Error(), "stale_coordinator_epoch") {
		t.Fatalf("a fenced write after the epoch was lost: %v (reason %q)", err, ops.ReasonOf(fmtErr(err)))
	}
	if other := epochLost(l.plan, errors.New("a failure")); errors.Is(other, ErrEpochLost) || ops.ReasonOf(other.Error()) != "" {
		t.Errorf("another error is the lost epoch: %v", other)
	}

	// the run process: another session takes the plan right after the run released its node; the acceptance is the next fenced write
	run := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	claimed := false
	run.after = func(r *SchedRelay, effect string) {
		if strings.HasPrefix(effect, "release:") && !claimed {
			claimed = true
			if _, err := (&dagsched.Scheduler{Store: r.Sched.Store}).ClaimEpoch(ctx, run.plan, dagsched.ClaimInput{Actor: labOwner, SessionNonce: "session-other"}); err != nil {
				t.Errorf("the other session's claim: %v", err)
			}
		}
	}
	spec := run.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", run.file)
	res := run.runToEnd(spec.RunID)
	var v struct {
		State, Reason, Detail, Milestone string
		Lanes                            []struct{ Node, State, Reason, Detail string }
	}
	if res.Status != engine.RunCompleted || json.Unmarshal(res.Value, &v) != nil || v.State != "stopped" || v.Reason != "epoch_lost" || v.Milestone != "M1" ||
		!strings.Contains(v.Detail, "stale_coordinator_epoch") || !strings.Contains(v.Detail, "refused:epoch_lost:") {
		t.Fatalf("the run did not end with the lost epoch: %s %s %s\n%s", res.Status, res.ErrorKind, res.Error, res.Value)
	}
	if len(v.Lanes) != 1 || v.Lanes[0].State != "stopped" || v.Lanes[0].Reason != "epoch_lost" || v.Lanes[0].Detail == "" {
		t.Errorf("the lane does not stop at its acceptance with the reason: %s", res.Value)
	}
	for _, e := range run.events() {
		if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation {
			t.Errorf("the run escalated after it lost the epoch: %v", e.Data)
		}
	}
	if n := run.count("SELECT COUNT(*) FROM dag_agent_acceptances") + run.count("SELECT COUNT(*) FROM dag_agent_closures") + run.count("SELECT COUNT(*) FROM dag_integration_batches"); n != 0 {
		t.Errorf("%d relay rows written after the epoch was lost", n)
	}
	t.Logf("the run ended %s with %s after the effects %v", res.Status, res.Value, run.effects)
}

// TestEpochLostAtAReleaseStopsTheRun (CRW-1382 d4): the run-level end of an epoch lost at a release, pinned. Another session claims the
// plan after the run integrated A-1; the release of A-2, which comes after it, is the run's next fenced write: crw.release answers it as
// released false with the reason epoch_lost and the detail, crw:lib/lane ends the lane unreleased with both, and project-lane ends the
// run stopped with the reason and the detail. Nothing escalates and nothing more is written at the relay.
func TestEpochLostAtAReleaseStopsTheRun(t *testing.T) {
	run := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1", After: []string{"A-1"}})
	claimed := false
	run.after = func(r *SchedRelay, effect string) {
		if effect == "integrate:"+run.nodeOf["A-1"] && !claimed {
			claimed = true
			if _, err := (&dagsched.Scheduler{Store: r.Sched.Store}).ClaimEpoch(ctx, run.plan, dagsched.ClaimInput{Actor: labOwner, SessionNonce: "session-other"}); err != nil {
				t.Errorf("the other session's claim: %v", err)
			}
		}
	}
	spec := run.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", run.file)
	res := run.runToEnd(spec.RunID)
	var v struct {
		State, Reason, Detail, Milestone string
		Lanes                            []struct{ Node, State, Reason, Detail string }
	}
	if res.Status != engine.RunCompleted || json.Unmarshal(res.Value, &v) != nil || v.State != "stopped" || v.Reason != "epoch_lost" || v.Milestone != "M1" ||
		!strings.Contains(v.Detail, "refused:epoch_lost:") || !strings.Contains(v.Detail, "stale_coordinator_epoch") {
		t.Fatalf("the run did not stop with the epoch lost at the release: %s %s %s\n%s", res.Status, res.ErrorKind, res.Error, res.Value)
	}
	byNode := map[string]struct{ State, Reason, Detail string }{}
	for _, ln := range v.Lanes {
		byNode[ln.Node] = struct{ State, Reason, Detail string }{ln.State, ln.Reason, ln.Detail}
	}
	if a1 := byNode[run.nodeOf["A-1"]]; a1.State != "landed" {
		t.Errorf("A-1: %+v", a1)
	}
	if a2 := byNode[run.nodeOf["A-2"]]; a2.State != "unreleased" || a2.Reason != "epoch_lost" || !strings.Contains(a2.Detail, "stale_coordinator_epoch") {
		t.Errorf("A-2 does not end unreleased with the reason and the detail: %+v", a2)
	}
	rel := 0
	for _, e := range run.events() {
		if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation {
			t.Errorf("the run escalated after it lost the epoch: %v", e.Data)
		}
		if e.Kind == engine.KindCall && e.Op == ops.OpRelease && e.Event == engine.EvCompleted && str(e.Data, "node") == run.nodeOf["A-2"] && str(e.Data, "reason") == "epoch_lost" {
			rel++
		}
	}
	if rel != 1 {
		t.Errorf("%d crw.release rows of A-2 with the reason epoch_lost", rel)
	}
	if n := run.count("SELECT COUNT(*) FROM dag_agent_executions WHERE node_id = ?", run.nodeOf["A-2"]); n != 0 {
		t.Errorf("A-2 has %d executions after the epoch was lost", n)
	}
}

// TestEpochLostAfterTheLanesStopsTheRun (CRW-1382 d4): an epoch lost at a step after the lanes (the milestone close) ends the run stopped
// with the reason, as one lost in a lane does, not as an error of the script. The close of today reads the relay only (the integration
// tip of the final gate and of the close) and makes no fenced write, so the lab answers the first read after the lane integrated with
// the error kind of a fenced write refused stale_coordinator_epoch (cli.epochLost of the relay's refusal); whatever step after the lanes
// meets it, the run stops on it.
func TestEpochLostAfterTheLanesStopsTheRun(t *testing.T) {
	run := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	run.after = func(r *SchedRelay, effect string) {
		if strings.HasPrefix(effect, "integrate:") {
			run.mu.Lock()
			run.tipErr = func() error {
				return epochLost(run.plan, &store.RefusedError{Reason: string(contract.RefusalStaleCoordinatorEpoch), Detail: "the plan's coordinator epoch is held by another session"})
			}
			run.mu.Unlock()
		}
	}
	spec := run.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", run.file)
	res := run.runToEnd(spec.RunID)
	var v struct {
		State, Reason, Detail, Milestone string
		Lanes                            []struct{ Node, State string }
	}
	if res.Status != engine.RunCompleted || json.Unmarshal(res.Value, &v) != nil || v.State != "stopped" || v.Reason != "epoch_lost" || v.Milestone != "M1" ||
		!strings.Contains(v.Detail, "refused:epoch_lost:") {
		t.Fatalf("the run did not stop with the epoch lost after the lanes: %s %s %s\n%s", res.Status, res.ErrorKind, res.Error, res.Value)
	}
	if len(v.Lanes) != 1 || v.Lanes[0].State != "landed" {
		t.Errorf("the lanes of the stopped run: %s", res.Value)
	}
	for _, e := range run.events() {
		if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation {
			t.Errorf("the run escalated after it lost the epoch: %v", e.Data)
		}
	}
}

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// CRW-1363: nonempty requirements are accepted at start; the configured verifier
// records unknown commands as warnings when it runs.
func TestHandoffWithAnUnknownRequiredCommandStarts(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	withCommands := func(cmds ...string) []byte {
		return l.handoffBytes(func(in *compile.Input) { in.Projects[0].Milestones[0].RequiredCommands = cmds })
	}
	unrunnable, err := DecodeHandoff(withCommands(ops.CILocalCommand, "make e2e"))
	if err != nil {
		t.Fatal(err)
	}
	if why := CheckHandoff(unrunnable, l.plan, "M1"); why != "" {
		t.Errorf("CheckHandoff of a milestone that requires make e2e: %q", why)
	}
	for name, cmds := range map[string][]string{"none": nil, "ci local": {ops.CILocalCommand}} {
		in, err := DecodeHandoff(withCommands(cmds...))
		if err != nil {
			t.Fatal(err)
		}
		if why := CheckHandoff(in, l.plan, "M1"); why != "" {
			t.Errorf("%s: %s", name, why)
		}
	}
	file := filepath.Join(t.TempDir(), "handoff-e2e.json")
	if err := os.WriteFile(file, withCommands("make e2e"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := len(l.td.spawned)
	code, out, errOut := l.td.do("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", file, "--thread", labOwner, "--state", l.w.state,
		"--socket", l.host.SocketPath, "--repo", l.repo)
	if code != 0 || len(l.td.spawned) != before+1 {
		t.Errorf("crw flow start --handoff with make e2e: exit %d, %d processes\n%s%s", code, len(l.td.spawned)-before, out, errOut)
	}
	spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	if spec.RunID == "" {
		t.Error("the compiled handoff does not start")
	}
}
