package scenario

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// turnsSince are the turns the host was sent since the journal had seq rows, as call id, thread and turn number.
type turnSent struct{ call, thread, op string }

func turnsSince(l *Lab, seq int) []turnSent {
	var out []turnSent
	thread := map[string]string{}
	for _, ev := range l.Events() {
		if ev.Kind == engine.KindCall && (ev.Event == engine.EvThreadStarted || ev.Event == engine.EvForkStarted) {
			thread[ev.CallID] = str(ev.Data, "thread_id")
		}
		if ev.Seq > seq && ev.Kind == engine.KindCall && ev.Event == engine.EvTurnRequested {
			out = append(out, turnSent{call: ev.CallID, thread: thread[ev.CallID], op: str(ev.Data, "op")})
		}
	}
	return out
}

func labelOf(l *Lab, callID string) string {
	for _, c := range l.Calls() {
		if c.ID == callID {
			return c.Label
		}
	}
	return ""
}

// apply appends a flow row the way `crw flow answer` does: to the run's journal, by the process that holds the run.
func (l *Lab) apply(event string, data map[string]any) {
	l.journal.Append(engine.Event{RunID: l.RunID, Kind: engine.KindFlow, Event: event, Data: data})
	l.journal.flush()
}

// A lane that waits for the orchestrator's decision goes on only when the run journal has the row that says the decision was
// applied (decision_applied): the run resumed without it, with a decision that is only recorded, or with an applied row of another
// question or node, runs nothing; the row of this question and node resumes that wait, and nothing else.
func TestDecideResumesOnlyOnAppliedRow(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Sequence("^build:A:executor$", func(*Call) fakehostPlan {
		return fakehostPlan{StartError: map[string]any{"code": -32600, "message": "bad request", "data": map[string]any{"codexErrorInfo": "badRequest"}}}
	})
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "waiting" || reason != "host_config_badRequest" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	esc := l.FlowRows(engine.EvEscalation)
	if len(esc) != 1 || str(esc[0].Data, "class") != "configuration_fault" {
		t.Fatalf("escalations %+v", esc)
	}
	ask := str(esc[0].Data, "escalation_id")
	quiet := func(what string) {
		t.Helper()
		before := l.LastSeq()
		l.NewProcess("proc-" + what)
		res := l.Resume()
		if res.Status != engine.RunCompleted {
			t.Fatalf("%s: run %s: %s", what, res.Status, res.Error)
		}
		if state, _ := laneOf(t, Value[laneResult](t, res), "A"); state != "waiting" {
			t.Fatalf("%s: lane A is %s: the wait must hold", what, state)
		}
		if live := turnsSince(l, before); len(live) != 0 {
			t.Fatalf("%s: live turns %+v", what, live)
		}
		if rows := rowsAfter(l, before); len(rows) > 0 {
			for _, ev := range rows {
				if ev.Kind == engine.KindCall && (ev.Event == engine.EvPrepared || ev.Event == engine.EvThreadRequested) && ev.Op == "agent" {
					t.Fatalf("%s: a call was prepared: %s", what, ev.Label)
				}
			}
		}
	}
	quiet("nothing")
	l.apply("decision_recorded", map[string]any{"ask": ask, "node": "A", "decision": map[string]any{"action": "retry"}})
	quiet("recorded")
	l.apply(ops.EvDecisionApplied, map[string]any{"ask": "esc_other", "node": "A", "decision": map[string]any{"action": "retry"}})
	l.apply(ops.EvDecisionApplied, map[string]any{"ask": ask, "node": "B", "decision": map[string]any{"action": "retry"}})
	quiet("other")

	before := l.LastSeq()
	l.apply(ops.EvDecisionApplied, map[string]any{"ask": ask, "node": "A", "decision": map[string]any{"action": "retry"}})
	l.NewProcess("proc-applied")
	res := l.Resume()
	if got := Value[laneResult](t, res); got.State != "closed" {
		t.Fatalf("after the applied row: %+v", got)
	}
	var live []string
	for _, ts := range turnsSince(l, before) {
		live = append(live, labelOf(l, ts.call))
	}
	if !slices.Equal(live, []string{"rebuild:A:executor:r1", "reflection:M1"}) {
		t.Errorf("live turns %v: want the fresh executor the decision let go on and the close", live)
	}
	rows := l.Rows("crw.decide")
	if len(rows) == 0 || !dataBoolOf(rows[len(rows)-1], "applied") {
		t.Errorf("decide rows %+v", rows)
	}
}

// The host refuses thread/start of the first executor (a configuration fault), the orchestrator retries, and the replacement
// executor builds the head that is unit-checked and accepted: the refused attempt never created a thread and wrote nothing, so the
// acceptance is audited ok - its pair is the replacement's (CRW-1206 correction, d5). The acceptance rests on a unit check
// (CRW-1465), which has no thread: nothing is archived.
func TestAuditOfARecoveredNodeIgnoresTheRefusedExecutorAttempt(t *testing.T) {
	l := NewLab(t, oneNode()...)
	var mu sync.Mutex
	refused := 0
	l.Threads.OnStart(func(params map[string]any) map[string]any {
		mu.Lock()
		defer mu.Unlock()
		if cwd, _ := params["cwd"].(string); strings.Contains(cwd, "/wt/A/") && refused == 0 {
			refused++
			return map[string]any{"code": -32600, "message": "bad request", "data": map[string]any{"codexErrorInfo": "badRequest"}}
		}
		return nil
	})
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "waiting" || reason != "host_config_badRequest" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	esc := l.FlowRows(engine.EvEscalation)
	if len(esc) != 1 {
		t.Fatalf("escalations %+v", esc)
	}
	for _, ev := range l.Events() {
		if ev.Kind == engine.KindCall && ev.Op == "agent" && ev.Label == "build:A:executor" && ev.Event == engine.EvThreadStarted {
			t.Fatalf("the refused executor attempt started a thread: the fixture does not model a refused thread/start")
		}
	}
	l.apply(ops.EvDecisionApplied, map[string]any{"ask": str(esc[0].Data, "escalation_id"), "node": "A", "decision": map[string]any{"action": "retry"}})
	l.NewProcess("proc-applied")
	if got := Value[laneResult](t, l.Resume()); got.State != "closed" {
		t.Fatalf("after the applied row: %+v", got)
	}
	audits := l.Rows("crw.audit")
	if len(audits) == 0 {
		t.Fatalf("no audit row after the close")
	}
	last := audits[len(audits)-1]
	if num(last.Data["ok"]) != 1 || num(last.Data["mismatch"]) != 0 || num(last.Data["unobserved"]) != 0 {
		t.Errorf("the audit of the recovered node: ok %v mismatch %v unobserved %v (%v)", last.Data["ok"], last.Data["mismatch"], last.Data["unobserved"], last.Data["results"])
	}
	if len(l.Archived()) != 0 {
		t.Errorf("archived %v: a unit-check acceptance has no thread to archive", l.Archived())
	}
}

func num(v any) int {
	f, _ := v.(float64)
	return int(f)
}

// A landing the integration target lost is integrated again when the run resumes: the lane that landed it runs only its integrate
// (everything before it replays: the unit check, the commit, the verification and the acceptance still stand) and lands a new batch,
// the target is not the head the final gate judged, and the whole close runs live again over the new head - the gate, the
// verification at the new head, the crosscheck, the close, the audit and the report. The other lane runs nothing, the only agent
// called is the architect's reflection, and the lab's crosschecker answers both passes of M1 at the new head after the resume, its
// receipts file under the lab's run directory, the same one as before the resume.
func TestMilestoneCloseRerunsAfterLaneSuffix(t *testing.T) {
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}},
	}
	l := NewLab(t, nodes...)
	first := mustRun(t, l)
	if first.State != "closed" {
		t.Fatalf("state %q", first.State)
	}
	oldTip := l.Tip()
	closes := l.Rows("crw.milestoneClose")
	if len(closes) != 1 || str(closes[0].Data, "tip") != oldTip {
		t.Fatalf("close rows %+v, tip %s", closes, oldTip)
	}
	if len(l.Relay.Landed) != 2 {
		t.Fatalf("landed %v", l.Relay.Landed)
	}
	// the second lane to land merged into the first; the target loses that merge
	kept, lost := l.Relay.Landed[0], l.Relay.Landed[1]
	var keptBatch, lostPath string
	for _, r := range l.Rows("crw.integrate") {
		if str(r.Data, "node") == kept {
			keptBatch = str(r.Data, "batch_commit")
		}
		// the path of the lost lane's integrate call: its position on root follows the design and explore phases (CRW-1466)
		if str(r.Data, "node") == lost {
			lostPath = r.Path
		}
	}
	l.Git("update-ref", IntegrationRef, keptBatch)

	crosschecksBefore := len(l.Crosschecker.Calls())
	before := l.LastSeq()
	l.NewProcess("proc-2")
	res := l.Resume()
	got := Value[laneResult](t, res)
	if got.State != "closed" {
		t.Fatalf("resume: %+v", got)
	}
	newTip := l.Tip()
	if newTip == oldTip || newTip == keptBatch {
		t.Fatalf("the integration branch is at %s (was %s, kept %s)", newTip, oldTip, keptBatch)
	}
	type live struct{ op, path string }
	var ranLive []live
	for _, ev := range rowsAfter(l, before) {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted && strings.HasPrefix(ev.Op, "crw.") && ev.Op != "crw.classify" {
			ranLive = append(ranLive, live{ev.Op, ev.Path})
		}
	}
	want := []live{{"crw.integrate", lostPath}, {"crw.finalGate", "root"}, {"crw.verify", "root"}, {"crw.finalGate", "root"}, {"crw.crosscheck", "root"},
		{"crw.milestoneClose", "root"}, {"crw.currentDesign", "root"}, {"crw.spawnDecision", "root"}, {"crw.reflection", "root"},
		{"crw.replanSignals", "root"}, {"crw.audit", "root"}, {"crw.archive", "root"}, {"crw.reportUp", "root"}, {"crw.spawnDecision", "root"}}
	if !slices.Equal(ranLive, want) {
		t.Errorf("live operations %v, want %v", ranLive, want)
	}
	if agents := turnsSince(l, before); len(agents) != 1 || labelOf(l, agents[0].call) != "reflection:M1" {
		t.Errorf("live agent turns %+v, want the reflection on the new head only", agents)
	}
	checkCrosscheckAfterResume(t, l, crosschecksBefore, newTip)
	closes = l.Rows("crw.milestoneClose")
	if last := closes[len(closes)-1]; len(closes) != 2 || str(last.Data, "tip") != newTip {
		t.Errorf("close rows: %d, the last judged %v, the branch is at %s", len(closes), last.Data["tip"], newTip)
	}
}

// Three lanes run; B is held by the host's usage limit after A and C landed, and the run pauses. When the cause is gone and the
// run resumes, the only live work is B's held call, which goes on in place - the next turn of the same thread - and what follows
// it in B's lane: A and C run nothing, and B's hold is cleared.
func TestQuotaPauseResumeRunsOnlyHeldCall(t *testing.T) {
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}},
		{ID: "C", Issue: "CRW-C", Title: "issue C", Regions: []string{"c/"}},
	}
	l := NewLab(t, nodes...)
	gate := make(chan struct{})
	var once sync.Once
	landed := 0
	var mu sync.Mutex
	l.OnRow(func(ev engine.Event) {
		if ev.Op == ops.OpIntegrate && ev.Event == engine.EvCompleted {
			mu.Lock()
			landed++
			n := landed
			mu.Unlock()
			if n == 2 {
				once.Do(func() { close(gate) })
			}
		}
	})
	l.On("^build:B:executor$", func(c *Call) *fakehostPlan {
		if c.N > 1 {
			return nil
		}
		c.Edit()
		p := Fail("usageLimitExceeded")
		p.Gate = gate
		return &p
	})
	got := mustRunIncomplete(t, l)
	if got.State != "incomplete" {
		t.Fatalf("state %q: %+v", got.State, got)
	}
	for node, want := range map[string]string{"A": "landed", "B": "held", "C": "landed"} {
		if state, _ := laneOf(t, got, node); state != want {
			t.Fatalf("lane %s is %s, want %s", node, state, want)
		}
	}
	if reason := ""; true {
		_, reason = laneOf(t, got, "B")
		if reason != "host_infra_usageLimitExceeded" {
			t.Fatalf("B is held for %q", reason)
		}
	}
	var quota int
	for _, ev := range l.FlowRows(engine.EvEscalation) {
		if str(ev.Data, "reason") == "quota_or_rate" {
			quota++
		}
	}
	if quota != 1 {
		t.Errorf("%d quota_or_rate escalations", quota)
	}
	if rows := l.Rows("crw.hold"); len(rows) != 1 || str(rows[0].Data, "node") != "B" {
		t.Fatalf("hold rows %+v", rows)
	}
	build := callNamed(t, l, "build:B:executor")
	var firstThread string
	for _, ev := range l.Events() {
		if ev.CallID == build.ID && ev.Event == engine.EvThreadStarted {
			firstThread = str(ev.Data, "thread_id")
		}
	}

	before := l.LastSeq()
	l.NewProcess("proc-2")
	res := l.Resume()
	final := Value[laneResult](t, res)
	if final.State != "closed" {
		t.Fatalf("resume: %+v", final)
	}
	var sent []string
	var heldTurn turnSent
	for _, ts := range turnsSince(l, before) {
		sent = append(sent, labelOf(l, ts.call))
		if ts.call == build.ID {
			heldTurn = ts
		}
	}
	if !slices.Equal(sent, []string{"build:B:executor", "reflection:M1"}) {
		t.Fatalf("live turns %v, want B's held call and the reflection of the close", sent)
	}
	if !strings.HasSuffix(heldTurn.op, "/t2") || heldTurn.thread != firstThread || firstThread == "" {
		t.Errorf("the held call went on with %+v, want turn t2 of its own thread %s", heldTurn, firstThread)
	}
	for _, ev := range rowsAfter(l, before) {
		if ev.Kind == engine.KindCall && ev.Op == "agent" && ev.Event == engine.EvPrepared && ev.Label != "build:B:executor" && ev.Label != "reflection:M1" {
			t.Errorf("a call prepared on the resume: %s", ev.Label)
		}
	}
	cleared := l.FlowRows(engine.EvHoldCleared)
	if len(cleared) != 1 || str(cleared[0].Data, "call_id") != build.ID {
		t.Errorf("hold_cleared rows %+v", cleared)
	}
	if !slices.Equal(l.Relay.Landed, []string{"A", "C", "B"}) && !slices.Equal(l.Relay.Landed, []string{"C", "A", "B"}) {
		t.Errorf("landed %v", l.Relay.Landed)
	}
	_ = json.Marshal
}

// A close that judged a tip is not the close of a target that moved on after it, even when no lane changed: the target gets a commit
// of another actor, and the resume judges the new head again - the final gate, a verification of it, the crosscheck, the close,
// the audit and the report - and runs no lane.
func TestMilestoneCloseRunsAgainWhenTheTargetMovedOn(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	if first := mustRun(t, l); first.State != "closed" {
		t.Fatalf("state %q", first.State)
	}
	oldTip := l.Tip()
	moved := l.Git("commit-tree", l.Git("rev-parse", oldTip+"^{tree}"), "-p", oldTip, "-m", "another actor moved the target")
	l.Git("update-ref", IntegrationRef, moved)

	crosschecksBefore := len(l.Crosschecker.Calls())
	before := l.LastSeq()
	l.NewProcess("proc-2")
	got := Value[laneResult](t, l.Resume())
	if got.State != "closed" || got.Tip != moved {
		t.Fatalf("resume: %+v, the target is at %s", got, moved)
	}
	var live []string
	for _, ev := range rowsAfter(l, before) {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted && strings.HasPrefix(ev.Op, "crw.") && ev.Op != "crw.classify" {
			live = append(live, ev.Op)
		}
	}
	if !slices.Equal(live, []string{"crw.finalGate", "crw.verify", "crw.finalGate", "crw.crosscheck", "crw.milestoneClose", "crw.currentDesign", "crw.spawnDecision", "crw.reflection",
		"crw.replanSignals", "crw.audit", "crw.archive", "crw.reportUp", "crw.spawnDecision"}) {
		t.Errorf("live operations %v", live)
	}
	if turns := turnsSince(l, before); len(turns) != 1 || labelOf(l, turns[0].call) != "reflection:M1" {
		t.Errorf("live agent turns %+v, want the reflection on the new head only", turns)
	}
	checkCrosscheckAfterResume(t, l, crosschecksBefore, moved)
}

// checkCrosscheckAfterResume checks the crosscheck a resume ran live: the lab's crosschecker answered both passes of round 1 of M1 after
// the resume (the passes before it are the first n), at the head the close judged, in checkouts under the lab's run directory; the
// close's receipts file is under the same run directory as before the resume; and nothing was written under a relative crosscheck
// directory of the test's working directory.
func checkCrosscheckAfterResume(t *testing.T, l *Lab, n int, head string) {
	t.Helper()
	calls := l.Crosschecker.Calls()
	if len(calls) < n {
		t.Fatalf("the crosschecker has %d calls, %d before the resume", len(calls), n)
	}
	var keys []string
	for _, c := range calls[n:] {
		keys = append(keys, c.Key)
		if !strings.HasPrefix(c.Request.Worktree, l.LandRoot+string(os.PathSeparator)) || !c.Existed {
			t.Errorf("the pass %s ran in %s (existed %v), not in a checkout under %s", c.Key, c.Request.Worktree, c.Existed, l.LandRoot)
		}
		if !strings.Contains(c.Request.Prompt, "HEAD: "+head) {
			t.Errorf("the pass %s after the resume does not judge the head %s", c.Key, head)
		}
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"M1/r1/e0/A1", "M1/r1/e0/B1"}) {
		t.Errorf("the passes after the resume %v, want both passes of round 1 of M1", keys)
	}
	if _, err := os.Stat(filepath.Join(l.LandRoot, "crosscheck", "M1-r1-receipts.md")); err != nil {
		t.Errorf("the close's receipts file is not under the lab's run directory: %v", err)
	}
	if cwd, err := os.Getwd(); err == nil {
		if _, err := os.Stat(filepath.Join(cwd, "crosscheck")); err == nil {
			t.Errorf("the crosscheck wrote under the test's working directory %s", cwd)
		}
	}
}

// A node the lane gave up (tiers_exhausted, blocked_scope) has its execution closed at the relay, and a closed execution does not
// stand. The release of that node is still proven on a resume: the run itself closed the execution, so the lane is not released
// again, no executor is built again, the relay is not asked to close a second time, and the escalation stays what it was.
func TestResumeAfterGivenUpNodeRunsNothing(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		lab    func(t *testing.T) *Lab
	}{
		{"tiers_exhausted", "tiers_exhausted", exhausted},
		{"blocked_scope", "blocked_scope", func(t *testing.T) *Lab {
			l := NewLab(t, oneNode()...)
			l.Sequence("^build:A:executor$", func(c *Call) fakehostPlan {
				return Say(ReceiptWith("blocked_scope", map[string]any{"gap": "the change needs the other package"}))
			})
			return l
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := tc.lab(t)
			first := mustRunIncomplete(t, l)
			if state, reason := laneOf(t, first, "A"); state != "escalated" || reason != tc.reason {
				t.Fatalf("lane A: %s %s", state, reason)
			}
			if l.Relay.SlotHeld("A") || len(l.Relay.CloseReasons()) != 1 {
				t.Fatalf("closes %v, slot held %v", l.Relay.CloseReasons(), l.Relay.SlotHeld("A"))
			}
			releases := len(l.Relay.Releases)
			before := l.LastSeq()
			l.NewProcess("proc-given-up")
			again := Value[laneResult](t, l.Resume())
			if state, reason := laneOf(t, again, "A"); state != "escalated" || reason != tc.reason {
				t.Errorf("resume: lane A: %s %s", state, reason)
			}
			if turns := turnsSince(l, before); len(turns) != 0 {
				t.Errorf("live agent turns on the resume: %+v", turns)
			}
			if n := len(l.Relay.Releases); n != releases {
				t.Errorf("%d releases after the resume, %d before: the given-up node was released again", n, releases)
			}
			if got := l.Relay.CloseReasons(); len(got) != 1 {
				t.Errorf("closes %v: the execution is closed once", got)
			}
			for _, ev := range rowsAfter(l, before) {
				if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted && (ev.Op == "crw.release" || ev.Op == "crw.escalate") {
					t.Errorf("%s ran again on the resume (%s)", ev.Op, ev.Path)
				}
			}
		})
	}
}

// TestResumeAfterATierStepBeforeTheCommitRunsNoFinishedExecutorAgain (x1436 item 5): two checks with the same blocking finding step the
// tier after one fix, before any commit. A commit holds only checked work, so nothing is committed: the executor-t1 starts fresh at the
// node's base, and the build and the fix of the attempt it leaves made only uncommitted trees, which that fresh attempt abandoned. A
// resume replays both as recorded facts and runs neither again. Held: the executor-t1's first turn is held by the host's usage limit,
// and the resume goes on with that call in place only. Closed: a resume of the closed run runs no executor at all. Rebuild: a fix that
// ends null_model leaves the build's checked tree uncommitted, the rebuild starts fresh at the node's base, and a resume of the closed
// run runs neither the build nor the rebuild again.
func TestResumeAfterATierStepBeforeTheCommitRunsNoFinishedExecutorAgain(t *testing.T) {
	same := Finding("P1", "the guard is missing")
	stepped := func(t *testing.T, l *Lab) {
		t.Helper()
		steps := l.StepRows()
		if len(steps) != 1 || steps[0].Head != "" {
			t.Fatalf("steps %+v: one step with no checkpoint commit", steps)
		}
		fix := callNamed(t, l, "fix:A:executor:r1")
		for _, c := range l.Rows("crw.commit") {
			if str(c.Data, "call") == fix.ID || str(c.Data, "call") == callNamed(t, l, "build:A:executor").ID {
				t.Errorf("a tree of the executor tier was committed: %v", c.Data)
			}
		}
		t1 := callNamed(t, l, "build:A:executor-t1")
		if alloc, _ := l.Prepared(t1.ID).Data["allocation"].(map[string]any); str(alloc, "kind") != "fresh" || str(alloc, "start_commit") != l.Base {
			t.Errorf("the executor-t1's allocation %v, want a fresh worktree at the node's base %s", alloc, l.Base)
		}
	}
	noFinishedExecutor := func(t *testing.T, l *Lab, before int) {
		t.Helper()
		for _, ev := range rowsAfter(l, before) {
			if ev.Kind == engine.KindCall && ev.Op == "agent" && ev.Event == engine.EvPrepared && (ev.Label == "build:A:executor" || ev.Label == "fix:A:executor:r1") {
				t.Errorf("the resume prepared %s again: a finished executor call ran live", ev.Label)
			}
		}
	}
	t.Run("held executor-t1", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		l.UnitChecks("A", NeedsChanges(same), NeedsChanges(same))
		l.On("^build:A:executor-t1$", func(c *Call) *fakehostPlan {
			if c.N > 1 {
				return nil
			}
			c.Edit()
			p := Fail("usageLimitExceeded")
			return &p
		})
		got := mustRunIncomplete(t, l)
		if state, reason := laneOf(t, got, "A"); state != "held" || reason != "host_infra_usageLimitExceeded" {
			t.Fatalf("lane A: %s %s; calls %v", state, reason, l.Labels())
		}
		stepped(t, l)
		before := l.LastSeq()
		l.NewProcess("proc-2")
		final := Value[laneResult](t, l.Resume())
		if final.State != "closed" || !slices.Equal(l.Relay.Landed, []string{"A"}) {
			t.Fatalf("resume: %+v, landed %v", final, l.Relay.Landed)
		}
		var live []string
		for _, ts := range turnsSince(l, before) {
			live = append(live, labelOf(l, ts.call))
		}
		if !slices.Equal(live, []string{"build:A:executor-t1", "reflection:M1"}) {
			t.Errorf("live turns %v, want the held executor-t1 and the close", live)
		}
		noFinishedExecutor(t, l, before)
		if n := len(l.Checker.Calls("A")); n != 3 {
			t.Errorf("%d unit checks, want the two before the step and the executor-t1's", n)
		}
	})
	t.Run("closed run", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		l.UnitChecks("A", NeedsChanges(same), NeedsChanges(same))
		if got := mustRun(t, l); got.State != "closed" {
			t.Fatalf("state %q", got.State)
		}
		stepped(t, l)
		before := l.LastSeq()
		l.NewProcess("proc-2")
		final := Value[laneResult](t, l.Resume())
		if final.State != "closed" || !slices.Equal(l.Relay.Landed, []string{"A"}) {
			t.Fatalf("resume: %+v, landed %v", final, l.Relay.Landed)
		}
		if live := turnsSince(l, before); len(live) != 0 {
			var labels []string
			for _, ts := range live {
				labels = append(labels, labelOf(l, ts.call))
			}
			t.Errorf("live turns %v on the resume of a closed run", labels)
		}
		noFinishedExecutor(t, l, before)
	})
	t.Run("rebuild after a fix that ends null_model", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		l.UnitChecks("A", NeedsChanges(same))
		l.Sequence("^fix:A:executor:r1$", func(c *Call) fakehostPlan {
			c.EditPath("a/scratch.txt", "an edit the null_model attempt left behind\n")
			return Garbage()
		})
		if got := mustRun(t, l); got.State != "closed" {
			t.Fatalf("state %q; calls %v", got.State, l.Labels())
		}
		rebuild := callNamed(t, l, "rebuild:A:executor:r2")
		if alloc, _ := l.Prepared(rebuild.ID).Data["allocation"].(map[string]any); str(alloc, "kind") != "fresh" || str(alloc, "start_commit") != l.Base {
			t.Errorf("the rebuild's allocation %v, want a fresh worktree at the node's base %s", alloc, l.Base)
		}
		if commits := l.Rows("crw.commit"); len(commits) != 1 || str(commits[0].Data, "call") != rebuild.ID {
			t.Errorf("commits %+v, want the rebuild's only", commits)
		}
		before := l.LastSeq()
		l.NewProcess("proc-2")
		if final := Value[laneResult](t, l.Resume()); final.State != "closed" || !slices.Equal(l.Relay.Landed, []string{"A"}) {
			t.Fatalf("resume: %+v, landed %v", final, l.Relay.Landed)
		}
		if live := turnsSince(l, before); len(live) != 0 {
			var labels []string
			for _, ts := range live {
				labels = append(labels, labelOf(l, ts.call))
			}
			t.Errorf("live turns %v on the resume of a closed run", labels)
		}
		noFinishedExecutor(t, l, before)
	})
}
