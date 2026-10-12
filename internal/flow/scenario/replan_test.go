package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows"
)

// The continuity of the architect and the re-plan of a milestone (CRW-1202, F09) on the whole stack of the scenario harness: the real
// agent backend on the fake App Server host, a temporary store and repository, the project lane from its architect to its report.

// replanResult is what a milestone that re-planned at its close came to.
type replanResult struct {
	State      string   `json:"state"`
	Revision   string   `json:"revision"`
	Reasons    []string `json:"reasons"`
	Escalation string   `json:"escalation"`
	Reflection string   `json:"reflection"`
	Final      string   `json:"final"`
	Source     string   `json:"source"`
	Reason     string   `json:"reason"`
}

func runReplan(t *testing.T, l *Lab) replanResult {
	t.Helper()
	res := l.Run()
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s: %s: %s\ncalls: %v", res.Status, res.ErrorKind, res.Error, l.Labels())
	}
	return Value[replanResult](t, res)
}

// turnReq is a turn/start the host received: the call it belongs to, the thread it went to and the directory it named.
type turnReq struct {
	Call, Label, Thread, Cwd, Text string
}

func (l *Lab) turnReqs() []turnReq {
	labels := map[string]string{}
	for _, c := range l.Calls() {
		labels[c.ID] = c.Label
	}
	var out []turnReq
	for _, r := range l.Requests("turn/start") {
		var p map[string]any
		_ = json.Unmarshal(r.Params, &p)
		text := inputText(p)
		tr := turnReq{Thread: str(p, "threadId"), Cwd: str(p, "cwd"), Text: text}
		if m := opLine.FindStringSubmatch(strings.SplitN(text, "\n", 2)[0]); m != nil {
			tr.Call, tr.Label = m[1], labels[m[1]]
		}
		out = append(out, tr)
	}
	return out
}

// turnsOf are the turns of the calls whose label starts with prefix.
func (l *Lab) turnsOf(prefix string) []turnReq {
	var out []turnReq
	for _, tr := range l.turnReqs() {
		if strings.HasPrefix(tr.Label, prefix) {
			out = append(out, tr)
		}
	}
	return out
}

// architectLabels are the labels of the architect's calls, in order.
func architectLabels(l *Lab) []string {
	var out []string
	for _, c := range l.CallsOf("architect") {
		out = append(out, c.Label)
	}
	return out
}

func noCallLabelled(t *testing.T, l *Lab, prefix string) {
	t.Helper()
	for _, label := range l.Labels() {
		if strings.HasPrefix(label, prefix) {
			t.Errorf("a call labelled %q was made: %v", label, l.Labels())
		}
	}
}

// replanTurnResumes asserts the replan call continues the architect call before it, on the thread of the DESIGN.
func replanTurnResumes(t *testing.T, l *Lab, replan, previous string) {
	t.Helper()
	call, prev := callNamed(t, l, replan), callNamed(t, l, previous)
	if call.Resume != prev.ID {
		t.Errorf("%s continues %q, want the architect's turn before it, %s (%s)", replan, call.Resume, previous, prev.ID)
	}
	if call.Role != "architect" {
		t.Errorf("%s ran as %s", replan, call.Role)
	}
}

// architectThread is the thread of the architect's DESIGN.
func architectThread(t *testing.T, l *Lab) string {
	t.Helper()
	turns := l.turnsOf("architect")
	if len(turns) == 0 {
		t.Fatal("the architect was not sent a turn")
	}
	return turns[0].Thread
}

// The close judges the range: reviewer-final MISALIGNED is a reason to re-plan whatever the reflection says, and the re-plan is the next
// turn of the architect's thread - the turn after its reflection - audited again before any revision is recorded.
func TestReviewerFinalMisalignedReplansSameArchitect(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	l.On("^reviewer-final:M1$", func(*Call) *fakehostPlan { p := Say(VerdictFinal(false)); return &p })
	got := runReplan(t, l)
	if got.State != "replanned" || got.Revision != "r1" || !slices.Equal(got.Reasons, []string{"reviewer_final_misaligned"}) || got.Reflection != "ALIGNED" || got.Final != "MISALIGNED" {
		t.Fatalf("%+v; calls %v", got, l.Labels())
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1", "build:B:executor", "review:B:r1", "reviewer-final:M1", "reflection:M1", "replan:close", "design-audit:replan-close:r1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	// the architect has one thread: the DESIGN, the reflection and the delta are its turns in this order
	replanTurnResumes(t, l, "replan:close", "reflection:M1")
	replanTurnResumes(t, l, "reflection:M1", "architect")
	thread := architectThread(t, l)
	for _, tr := range l.turnReqs() {
		if (tr.Label == "reflection:M1" || tr.Label == "replan:close") && tr.Thread != thread {
			t.Errorf("%s went to thread %s, the architect's is %s", tr.Label, tr.Thread, thread)
		}
	}
	// the delta is a revision after the audit approved it, and the ledger's handle is the delta's call
	rows := l.Rows("crw.designLedger")
	if len(rows) != 2 || str(rows[1].Data, "revision") != "r1" || str(rows[1].Data, "architect_call") != callNamed(t, l, "replan:close").ID {
		t.Fatalf("ledger rows %+v", rows)
	}
	audit := callNamed(t, l, "design-audit:replan-close:r1")
	if audit.Seq > rows[1].Seq {
		t.Errorf("the revision was recorded before its audit")
	}
	if rules := ruleCounts(l); rules["SP-ARCH-2"] == 0 || rules["SP-ARCH-3"] != 1 {
		t.Errorf("rules %v", rules)
	}
	if reasons := escalationReasons(l); !slices.Equal(reasons, []string{"replan_correction"}) {
		t.Errorf("escalations %v: the correction issues are the orchestrator's", reasons)
	}
}

func ruleCounts(l *Lab) map[string]int {
	out := map[string]int{}
	for _, r := range l.SpawnRows() {
		out[r.Rule]++
	}
	return out
}

// spawnRowsOf are the rows of a rule and decision.
func spawnRowsOf(l *Lab, rule, decision string) []SpawnRow {
	var out []SpawnRow
	for _, r := range l.SpawnRows() {
		if r.Rule == rule && r.Decision == decision {
			out = append(out, r)
		}
	}
	return out
}

// A MISALIGNED reflection is a reason of its own: the architect re-plans on the thread it reflected on.
func TestReflectionMisalignedReplans(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	l.On("^reflection:M1$", func(*Call) *fakehostPlan { p := Say(Reflection(false)); return &p })
	got := runReplan(t, l)
	if got.State != "replanned" || !slices.Equal(got.Reasons, []string{"reflection_misaligned"}) || got.Reflection != "MISALIGNED" || got.Final != "ALIGNED" {
		t.Fatalf("%+v; calls %v", got, l.Labels())
	}
	replanTurnResumes(t, l, "replan:close", "reflection:M1")
	// the delta is asked what the reflection found
	if text := textOfCall(l, callNamed(t, l, "replan:close").ID); !strings.Contains(text, "D1: the range departs from D1") {
		t.Errorf("the re-plan packet does not carry the decisions the reflection found departed from:\n%s", text)
	}
	if rows := spawnRowsOf(l, "SP-ARCH-2", "spawn"); len(rows) != 1 || !strings.Contains(rows[0].Reason, "reflection_misaligned") {
		t.Errorf("re-plan decisions %+v", rows)
	}
}

// DESIGN, a MISALIGNED REFLECTION and the DESIGN delta are three turns of one architect thread, one after the other, each the turn
// after the one the ledger's handle names.
func TestReplanSeesReflectionTurn(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.On("^reflection:M1$", func(*Call) *fakehostPlan { p := Say(Reflection(false)); return &p })
	got := runReplan(t, l)
	if got.State != "replanned" {
		t.Fatalf("%+v; calls %v", got, l.Labels())
	}
	if labels := architectLabels(l); !slices.Equal(labels, []string{"architect", "reflection:M1", "replan:close"}) {
		t.Fatalf("architect calls %v", labels)
	}
	var seq []string
	thread := ""
	for _, tr := range l.turnReqs() {
		switch tr.Label {
		case "architect", "reflection:M1", "replan:close":
			if thread == "" {
				thread = tr.Thread
			}
			if tr.Thread != thread {
				t.Errorf("%s went to thread %s, not %s", tr.Label, tr.Thread, thread)
			}
			seq = append(seq, tr.Label)
		}
	}
	if !slices.Equal(seq, []string{"architect", "reflection:M1", "replan:close"}) {
		t.Errorf("turns %v", seq)
	}
	replanTurnResumes(t, l, "reflection:M1", "architect")
	replanTurnResumes(t, l, "replan:close", "reflection:M1")
	// the handle the ledger holds moves with the turns: the reflection's, then the delta's
	led := l.Rows("crw.designLedger")
	refl := l.Rows("crw.reflection")
	if len(led) != 2 || len(refl) != 1 || str(refl[0].Data, "architect_call") != callNamed(t, l, "reflection:M1").ID || str(led[1].Data, "architect_call") != callNamed(t, l, "replan:close").ID {
		t.Errorf("ledger rows %+v, reflection rows %+v", led, refl)
	}
}

// A reflection, a re-plan or a reviewer-final call that did not complete is not ALIGNED: held and unknown hold the milestone, refused
// and null_model escalate, and nothing re-plans on a verdict that was never given.
func TestReflectionFailureNotAligned(t *testing.T) {
	t.Run("held", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		l.Sequence("^reflection:M1$", func(*Call) fakehostPlan { return Fail("serverOverloaded") })
		got := runReplan(t, l)
		if got.State != "held" || got.Source != "reflection" || got.Reason != "host_infra_serverOverloaded" {
			t.Fatalf("%+v; calls %v", got, l.Labels())
		}
		noCallLabelled(t, l, "replan")
		if rows := l.Rows("crw.hold"); len(rows) != 1 || str(rows[0].Data, "node") != "milestone:M1" || str(rows[0].Data, "source") != "reflection" {
			t.Errorf("hold rows %+v", rows)
		}
		if rows := l.Rows("crw.reflection"); len(rows) != 1 || str(rows[0].Data, "state") != "held" || str(rows[0].Data, "alignment") != "" {
			t.Errorf("reflection rows %+v", rows)
		}
		if rows := l.Rows("crw.replanSignals"); len(rows) != 0 && str(rows[len(rows)-1].Data, "phase") == "close" {
			t.Errorf("the close counted the signals over a reflection nobody gave")
		}
		if len(l.Rows("crw.reportUp")) != 0 {
			t.Errorf("the milestone reported")
		}
	})
	t.Run("unknown", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		l.Threads.DropAck("turn/start", func(_ int, params map[string]any) bool { return strings.Contains(inputText(params), "Reflection at the close") })
		got := runReplan(t, l)
		if got.State != "held" || got.Source != "reflection" {
			t.Fatalf("%+v; calls %v", got, l.Labels())
		}
		noCallLabelled(t, l, "replan")
	})
	t.Run("refused", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		l.Sequence("^reflection:M1$", func(*Call) fakehostPlan {
			return fakehostPlan{StartError: map[string]any{"code": -32600, "message": "bad request", "data": map[string]any{"codexErrorInfo": "badRequest"}}}
		})
		got := runReplan(t, l)
		if got.State != "escalated" || got.Source != "reflection" {
			t.Fatalf("%+v; calls %v", got, l.Labels())
		}
		noCallLabelled(t, l, "replan")
		if reasons := escalationReasons(l); !slices.Equal(reasons, []string{"reflection_refused"}) {
			t.Errorf("escalations %v", reasons)
		}
	})
	t.Run("null_model", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		l.Sequence("^reflection:M1$", func(*Call) fakehostPlan { return Garbage() })
		got := runReplan(t, l)
		if got.State != "escalated" || got.Source != "reflection" {
			t.Fatalf("%+v; calls %v", got, l.Labels())
		}
		noCallLabelled(t, l, "replan")
		if reasons := escalationReasons(l); !slices.Equal(reasons, []string{"reflection_null_model"}) {
			t.Errorf("escalations %v", reasons)
		}
	})
	t.Run("a MISALIGNED answer that names no decision", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		l.On("^reflection:M1$", func(*Call) *fakehostPlan {
			p := Say(map[string]any{"verdict": "MISALIGNED", "misaligned": []any{}})
			return &p
		})
		got := runReplan(t, l)
		if got.State != "escalated" || got.Source != "reflection" {
			t.Fatalf("%+v; calls %v", got, l.Labels())
		}
		noCallLabelled(t, l, "replan")
	})
}

// reviewer-final that did not complete halts the milestone: held holds it (a resume asks the same call again, as a new turn on its
// thread), and the architect is not asked to reflect on a range nobody judged.
func TestReviewerFinalHeldHaltsMilestone(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.On("^reviewer-final:M1$", func(c *Call) *fakehostPlan {
		if c.N > 1 {
			return nil
		}
		p := Fail("serverOverloaded")
		return &p
	})
	got := runReplan(t, l)
	if got.State != "held" || got.Source != "reviewer_final" || got.Reason != "host_infra_serverOverloaded" {
		t.Fatalf("%+v; calls %v", got, l.Labels())
	}
	noCallLabelled(t, l, "reflection")
	noCallLabelled(t, l, "replan")
	if rows := l.Rows("crw.hold"); len(rows) != 1 || str(rows[0].Data, "node") != "milestone:M1" || str(rows[0].Data, "source") != "reviewer_final" {
		t.Errorf("hold rows %+v", rows)
	}
	if len(l.Rows("crw.milestoneClose")) != 0 || len(l.Rows("crw.reportUp")) != 0 {
		t.Errorf("the milestone closed over a final review nobody gave")
	}
	// the cause is gone: the resume asks the same call again, as the next turn of its thread, and the milestone closes
	before := l.LastSeq()
	l.NewProcess("proc-2")
	res := l.Resume()
	final := Value[replanResult](t, res)
	if final.State != "closed" {
		t.Fatalf("resume: %s; calls %v", res.Value, l.Labels())
	}
	var sent []string
	for _, ts := range turnsSince(l, before) {
		sent = append(sent, fmt.Sprintf("%s%s", labelOf(l, ts.call), ts.op[strings.LastIndex(ts.op, "/"):]))
	}
	if !slices.Equal(sent, []string{"reviewer-final:M1/t2", "reflection:M1/t1"}) {
		t.Errorf("live turns %v, want the held final review as its next turn, and the reflection after it", sent)
	}
}

// ---- the signals of the dag ----

// mismatching scripts the executor calls of the nodes (their build and every fix) to report a plan mismatch.
func mismatching(l *Lab, nodes ...string) {
	for _, node := range nodes {
		l.On("^(build|fix):"+node+":", func(c *Call) *fakehostPlan {
			c.Edit()
			p := Say(ReceiptWith("ready_for_review", map[string]any{"plan_mismatch": "the plan assumed something else: " + c.Label}))
			return &p
		})
	}
}

// reviewsAfterFix scripts the verdicts of a node: needs_changes (a fix and a second RECEIPT follow), then verified.
func fixedOnce(l *Lab, node string) {
	reviews(l, node, Verdict(false, Defect("d1", "correctness", "P2", "fix this")), Verdict(true))
}

// threeNodes are A, which B and C wait for.
func chainNodes() []NodeSpec {
	return []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}, After: []string{"A"}},
		{ID: "C", Issue: "CRW-C", Title: "issue C", Regions: []string{"c/"}, After: []string{"A"}},
	}
}

func labelsWithPrefix(l *Lab, prefix string) []string {
	var out []string
	for _, label := range l.Labels() {
		if strings.HasPrefix(label, prefix) {
			out = append(out, label)
		}
	}
	return out
}

// releaseRevisions are the revisions the nodes were released under, in the order of the releases.
func releaseRevisions(l *Lab) map[string]string {
	out := map[string]string{}
	for _, ev := range l.Rows("crw.release") {
		if flagOf(ev.Data, "released") {
			out[str(ev.Data, "node")] = str(ev.Data, "revision")
		}
	}
	return out
}

func flagOf(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

// Two RECEIPTs of lanes released under the revision that report a plan mismatch are a reason to re-plan, whatever the alignment says:
// the milestone re-plans in the middle of the run, the nodes that have not started take the revision, and the close finds nothing more.
func TestPlanMismatchTwiceReplansEvenIfAligned(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	mismatching(l, "A")
	fixedOnce(l, "A")
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q; calls %v; escalations %v", got.State, l.Labels(), escalationReasons(l))
	}
	if labels := labelsWithPrefix(l, "replan:"); !slices.Equal(labels, []string{"replan:s2"}) {
		t.Fatalf("re-plan calls %v in %v", labels, l.Labels())
	}
	replanTurnResumes(t, l, "replan:s2", "architect")
	revs := releaseRevisions(l)
	if revs["A"] != "r0" || revs["B"] != "r1" {
		t.Errorf("release revisions %v: A started under r0, B had not started when the milestone re-planned", revs)
	}
	// the close found the range aligned, and counted the mismatches under the revision that stands: none
	closes := l.Rows("crw.replanSignals")
	last := closes[len(closes)-1]
	if str(last.Data, "phase") != "close" || num(last.Data["plan_mismatch"]) != 0 || last.Data["due"] != false {
		t.Errorf("the close counted %+v", last.Data)
	}
	var due []map[string]any
	for _, r := range closes {
		if r.Data["due"] == true {
			due = append(due, r.Data)
		}
	}
	if len(due) != 1 || num(due[0]["plan_mismatch"]) != 2 || num(due[0]["signal"]) != 2 {
		t.Errorf("rows that found a reason: %+v", due)
	}
	if rows := spawnRowsOf(l, "SP-ARCH-2", "spawn"); len(rows) != 1 || rows[0].Reason != "plan_mismatch" {
		t.Errorf("re-plan decisions %+v", rows)
	}
}

// One RECEIPT with a plan mismatch is not a reason: no re-plan call, and the decision to skip is a row with its reason.
func TestPlanMismatchOnceNoReplan(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	mismatching(l, "A")
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	noCallLabelled(t, l, "replan")
	if rows := spawnRowsOf(l, "SP-ARCH-2", "spawn"); len(rows) != 0 {
		t.Errorf("re-plan decisions %+v", rows)
	}
	var one bool
	for _, r := range l.Rows("crw.replanSignals") {
		if num(r.Data["plan_mismatch"]) == 1 && r.Data["due"] == false {
			one = true
		}
		if r.Data["due"] == true {
			t.Errorf("a reason found: %+v", r.Data)
		}
	}
	if !one {
		t.Errorf("no row counted the one mismatch: %+v", l.Rows("crw.replanSignals"))
	}
	if rows := spawnRowsOf(l, "SP-ARCH-2", "skip"); len(rows) == 0 || rows[0].Reason != "no_replan_signal" {
		t.Errorf("skip decisions %+v", rows)
	}
}

// A run in which nothing asks for a re-plan decides to skip it at every signal and at the close, and each decision is a row.
func TestNoSignalRecordsSkip(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	noCallLabelled(t, l, "replan")
	skips := spawnRowsOf(l, "SP-ARCH-2", "skip")
	// a signal for the RECEIPT of each lane, and the close
	if len(skips) != 3 {
		t.Fatalf("skip decisions %+v", skips)
	}
	for _, r := range skips {
		if r.Reason != "no_replan_signal" || r.Kind != "signal" {
			t.Errorf("skip %+v", r)
		}
	}
	if n := len(l.Rows("crw.signalBind")); n != 2 {
		t.Errorf("%d signals, want one for each lane's RECEIPT", n)
	}
	for _, r := range l.Rows("crw.signalDone") {
		if str(r.Data, "outcome") != "continue" {
			t.Errorf("signal %+v", r.Data)
		}
	}
}

// Receipts of lanes released under an older revision do not count in the number of the revision that stands.
func TestOldRevisionReceiptNotCounted(t *testing.T) {
	l := NewLab(t, chainNodes()...)
	mismatching(l, "A", "B")
	fixedOnce(l, "A")
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q; calls %v; escalations %v", got.State, l.Labels(), escalationReasons(l))
	}
	// A's two RECEIPTs re-planned once (r1); B, released under r1, reported one more: three mismatches in the journal, one under r1
	if labels := labelsWithPrefix(l, "replan:"); !slices.Equal(labels, []string{"replan:s2"}) {
		t.Fatalf("re-plan calls %v in %v", labels, l.Labels())
	}
	revs := releaseRevisions(l)
	if revs["A"] != "r0" || revs["B"] != "r1" || revs["C"] != "r1" {
		t.Errorf("release revisions %v", revs)
	}
	rows := l.Rows("crw.replanSignals")
	last := rows[len(rows)-1]
	if num(last.Data["plan_mismatch"]) != 1 || last.Data["due"] != false || str(last.Data, "revision") != "r1" {
		t.Errorf("the close counted %+v", last.Data)
	}
}

// Only the nodes that have not started take the new revision: the lanes that started keep the packet of the revision they were
// released under, and the DESIGN delta is what the others are built from.
func TestReplanRefreshesOnlyUnstartedPackets(t *testing.T) {
	l := NewLab(t, chainNodes()...)
	mismatching(l, "A")
	fixedOnce(l, "A")
	l.On("^replan:", func(*Call) *fakehostPlan {
		d := l.designFor()
		for i := range d.Issues {
			d.Issues[i].Approach = "approach of " + d.Issues[i].Node + " after the re-plan"
		}
		p := Say(d)
		return &p
	})
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q; calls %v; escalations %v", got.State, l.Labels(), escalationReasons(l))
	}
	packet := func(label string) string { return textOfCall(l, callNamed(t, l, label).ID) }
	for _, label := range []string{"build:A:executor", "fix:A:executor:r1"} {
		if text := packet(label); label == "build:A:executor" && (!strings.Contains(text, "DESIGN REVISION: r0") || strings.Contains(text, "after the re-plan")) {
			t.Errorf("%s was not built from r0:\n%s", label, text)
		}
	}
	for _, label := range []string{"build:B:executor", "build:C:executor"} {
		if text := packet(label); !strings.Contains(text, "DESIGN REVISION: r1") || !strings.Contains(text, "after the re-plan") {
			t.Errorf("%s was not built from r1:\n%s", label, text)
		}
	}
	// the facts of every call of a lane carry the revision the lane was released under
	for _, ev := range l.Events() {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvPrepared && ev.Op == "agent" && str(ev.Data, "node") != "" {
			want := "design=r1:"
			if str(ev.Data, "node") == "A" {
				want = "design=r0:"
			}
			if !strings.HasPrefix(str(ev.Data, "input_snapshot"), want) {
				t.Errorf("%s: input snapshot %q, want it to start with %s", ev.Label, str(ev.Data, "input_snapshot"), want)
			}
		}
	}
}

// A re-plan that needs changes goes back to the architect on the same thread, and no node that has not started is released under a
// revision that was not audited again: the release comes after the approving audit and the revision's row.
func TestReplanReauditsDesignBeforeRelease(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	mismatching(l, "A")
	fixedOnce(l, "A")
	l.Sequence("^design-audit:replan-s2:r\\d+$",
		func(*Call) fakehostPlan {
			return Say(map[string]any{"verdict": "needs_changes", "findings": []any{map[string]any{"ref": "D1", "severity": "P2", "finding": "state the order of the nodes", "evidence": []string{}}}, "summary": "one thing to fix"})
		},
		func(*Call) fakehostPlan {
			return Say(map[string]any{"verdict": "approved", "findings": []any{}, "summary": "the delta holds"})
		})
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q; calls %v; escalations %v", got.State, l.Labels(), escalationReasons(l))
	}
	want := []string{"replan:s2", "design-audit:replan-s2:r1", "design-fix:replan-s2:r1", "design-audit:replan-s2:r2"}
	var got2 []string
	for _, label := range l.Labels() {
		if strings.Contains(label, "replan-s2") || label == "replan:s2" {
			got2 = append(got2, label)
		}
	}
	if !slices.Equal(got2, want) {
		t.Fatalf("calls of the re-plan %v, want %v", got2, want)
	}
	// the architect goes on, on the same thread: the delta, the fix of the delta
	replanTurnResumes(t, l, "replan:s2", "architect")
	replanTurnResumes(t, l, "design-fix:replan-s2:r1", "replan:s2")
	thread := architectThread(t, l)
	for _, tr := range l.turnReqs() {
		if (tr.Label == "replan:s2" || tr.Label == "design-fix:replan-s2:r1") && tr.Thread != thread {
			t.Errorf("%s went to thread %s, the architect's is %s", tr.Label, tr.Thread, thread)
		}
	}
	// the order of the rows: the approving audit, the revision, the release of the node that had not started
	approving := callNamed(t, l, "design-audit:replan-s2:r2")
	var ledgerSeq, releaseSeq int
	for _, ev := range l.Rows("crw.designLedger") {
		if str(ev.Data, "revision") == "r1" {
			ledgerSeq = ev.Seq
		}
	}
	for _, ev := range l.Rows("crw.release") {
		if str(ev.Data, "node") == "B" {
			releaseSeq = ev.Seq
		}
	}
	if !(approving.Seq < ledgerSeq && ledgerSeq < releaseSeq) {
		t.Errorf("the approving audit is at %d, the revision at %d, the release of B at %d", approving.Seq, ledgerSeq, releaseSeq)
	}
	if rev := releaseRevisions(l); rev["B"] != "r1" {
		t.Errorf("B was released under %v", rev)
	}
	// the first audit did not approve: no revision was recorded over it
	for _, ev := range l.Rows("crw.designAudit") {
		if str(ev.Data, "state") == "needs_changes" && ev.Seq > ledgerSeq {
			t.Errorf("an audit that asked for changes is after the revision")
		}
	}
}

// A re-plan whose DESIGN is never approved does not release anything under it: the milestone escalates and the nodes that had not
// started stay unreleased.
func TestReplanNeverApprovedReleasesNothing(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	mismatching(l, "A")
	fixedOnce(l, "A")
	l.Sequence("^design-audit:replan-s2:r\\d+$", func(*Call) fakehostPlan {
		return Say(map[string]any{"verdict": "needs_changes", "findings": []any{map[string]any{"ref": "D1", "severity": "P2", "finding": "no", "evidence": []string{}}}, "summary": "no"})
	})
	res := l.Run()
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s: %s", res.Status, res.Error)
	}
	got := Value[laneResult](t, res)
	if got.State != "incomplete" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	if state, _ := laneOf(t, got, "B"); state != "blocked" && state != "halted" {
		t.Errorf("lane B is %s", state)
	}
	if revs := releaseRevisions(l); revs["B"] != "" {
		t.Errorf("B was released under %v", revs)
	}
	if reasons := escalationReasons(l); !slices.Contains(reasons, "design_audit_rounds") {
		t.Errorf("escalations %v", reasons)
	}
	if rows := l.Rows("crw.signalDone"); len(rows) == 0 || str(rows[len(rows)-1].Data, "outcome") != "halt" {
		t.Errorf("signal rows %+v", rows)
	}
}

// Two lanes whose RECEIPTs report a plan mismatch at nearly the same time make one re-plan: the second signal counts the
// RECEIPTs under the revision that stands, and the lanes released under the one before it do not count.
func TestTwoNearSimultaneousReceiptsOneReplan(t *testing.T) {
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}},
		{ID: "C", Issue: "CRW-C", Title: "issue C", Regions: []string{"c/"}},
		{ID: "D", Issue: "CRW-D", Title: "issue D", Regions: []string{"d/"}, After: []string{"A", "C"}},
	}
	l := NewLab(t, nodes...)
	mismatching(l, "A", "C")
	fixedOnce(l, "A")
	fixedOnce(l, "C")
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q; calls %v; escalations %v", got.State, l.Labels(), escalationReasons(l))
	}
	if labels := labelsWithPrefix(l, "replan:"); len(labels) != 1 {
		t.Fatalf("%d re-plan turns: %v", len(labels), labels)
	}
	if rev := releaseRevisions(l); rev["A"] != "r0" || rev["C"] != "r0" || rev["D"] != "r1" {
		t.Errorf("release revisions %v", rev)
	}
	// every RECEIPT of A and C is a trigger of exactly one signal
	seen := map[string]int{}
	for _, b := range l.Rows("crw.signalBind") {
		members, _ := b.Data["members"].([]any)
		for _, m := range members {
			seen[str(m.(map[string]any), "call")]++
		}
	}
	for call, n := range seen {
		if n != 1 {
			t.Errorf("call %s is a trigger of %d signals", call, n)
		}
	}
	if len(seen) < 4 {
		t.Errorf("%d triggers, want the builds and the fixes of A and C", len(seen))
	}
}

// More than a quarter of the slices of the plan changed since the revision is a reason; exactly a quarter is not.
func TestSliceChangeOver25Replans(t *testing.T) {
	l := NewLab(t, fourNodes()...)
	l.On("^build:A:executor$", func(c *Call) *fakehostPlan {
		// the plan is revised while A works: two of the four slices change
		for _, id := range []string{"B", "C"} {
			l.Relay.ChangeNode(id, func(n *NodeSpec) { n.Regions = append(slices.Clone(n.Regions), id+"-more/") })
		}
		return nil
	})
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q; calls %v; escalations %v", got.State, l.Labels(), escalationReasons(l))
	}
	if labels := labelsWithPrefix(l, "replan:"); !slices.Equal(labels, []string{"replan:s1"}) {
		t.Fatalf("re-plan calls %v in %v", labels, l.Labels())
	}
	var due map[string]any
	for _, r := range l.Rows("crw.replanSignals") {
		if r.Data["due"] == true {
			due = r.Data
		}
	}
	if due == nil || num(due["slices_changed"]) != 2 || num(due["slices_total"]) != 4 || !slices.Equal(anyStrings(due["reasons"]), []string{"slice_change"}) {
		t.Errorf("the row that found the reason: %+v", due)
	}
	// the revision starts counting from the plan the architect saw: the same change is not a reason again
	if rows := spawnRowsOf(l, "SP-ARCH-2", "spawn"); len(rows) != 1 {
		t.Errorf("re-plan decisions %+v", rows)
	}
}

func TestSliceChangeExactly25NoReplan(t *testing.T) {
	l := NewLab(t, fourNodes()...)
	l.On("^build:A:executor$", func(c *Call) *fakehostPlan {
		l.Relay.ChangeNode("B", func(n *NodeSpec) { n.Regions = append(slices.Clone(n.Regions), "b-more/") })
		return nil
	})
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q; calls %v; escalations %v", got.State, l.Labels(), escalationReasons(l))
	}
	noCallLabelled(t, l, "replan")
	var seen bool
	for _, r := range l.Rows("crw.replanSignals") {
		if num(r.Data["slices_changed"]) == 1 && num(r.Data["slices_total"]) == 4 {
			seen = true
		}
		if r.Data["due"] == true {
			t.Errorf("a reason found at exactly a quarter: %+v", r.Data)
		}
	}
	if !seen {
		t.Errorf("no row counted the change: %+v", l.Rows("crw.replanSignals"))
	}
}

func anyStrings(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, e := range list {
		s, _ := e.(string)
		out = append(out, s)
	}
	return out
}

// fourNodes are A, which B, C and D wait for.
func fourNodes() []NodeSpec {
	return []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}, After: []string{"A"}},
		{ID: "C", Issue: "CRW-C", Title: "issue C", Regions: []string{"c/"}, After: []string{"A"}},
		{ID: "D", Issue: "CRW-D", Title: "issue D", Regions: []string{"d/"}, After: []string{"A"}},
	}
}

// ---- the resume of a run that re-planned ----

// A run that completed with a re-plan in the middle of it is resumed unchanged: every signal, the DESIGN delta, its audit, the
// revision and the lanes released under it replay from the journal, and no call is live.
func TestResumeAfterCompletedRunWithInFlightReplanZeroLive(t *testing.T) {
	l := NewLab(t, chainNodes()...)
	mismatching(l, "A")
	fixedOnce(l, "A")
	first := mustRun(t, l)
	if first.State != "closed" {
		t.Fatalf("state %q; calls %v; escalations %v", first.State, l.Labels(), escalationReasons(l))
	}
	if len(labelsWithPrefix(l, "replan:")) != 1 {
		t.Fatalf("calls %v", l.Labels())
	}
	calls := len(l.Calls())
	tip := l.Tip()
	before := l.LastSeq()
	l.NewProcess("proc-2")
	res := l.Resume()
	got := Value[laneResult](t, res)
	if got.State != "closed" || got.Tip != tip {
		t.Fatalf("resume: %+v", got)
	}
	if live := l.LiveCalls(before); len(live) != 0 {
		t.Errorf("the resume made %d live calls: %+v", len(live), live)
	}
	if turns := turnsSince(l, before); len(turns) != 0 {
		t.Errorf("the resume sent turns %+v", turns)
	}
	if n := len(l.Calls()); n != calls {
		t.Errorf("%d calls were prepared (%d before the resume): %v", n, calls, l.Labels())
	}
	for _, ev := range rowsAfter(l, before) {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted && strings.HasPrefix(ev.Op, "crw.") && ev.Op != "crw.classify" {
			t.Errorf("an operation ran live: %s at %s", ev.Op, ev.Path)
		}
		if ev.Event == engine.EvSuperseded {
			t.Errorf("a call was superseded: %s %s", ev.Label, ev.Path)
		}
	}
	if revs := l.Rows("crw.designLedger"); len(revs) != 2 {
		t.Errorf("%d ledger rows", len(revs))
	}
}

// execEdited runs the project lane with its text edited: the resume of a script that changed since the run.
func (l *Lab) execEdited(edit func(string) string, resume bool) *engine.Result {
	l.t.Helper()
	text, err := workflows.Source("project-lane")
	if err != nil {
		l.t.Fatal(err)
	}
	edited := edit(string(text))
	if edited == string(text) {
		l.t.Fatal("the edit changed nothing")
	}
	prog, err := script.Load(script.Source{Name: "project-lane.js", Text: []byte(edited), Dir: workflows.FS})
	if err != nil {
		l.t.Fatal(err)
	}
	cfg := l.Config(resume)
	cfg.Args = l.Args()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	res, err := engine.Run(ctx, prog, cfg)
	if err != nil {
		l.t.Fatal(err)
	}
	l.journal.flush()
	return res
}

// editBuildOf changes the build prompt of a node: the node's first executor call is another call than the one the run made.
func editBuildOf(node string) func(string) string {
	return func(text string) string {
		return strings.Replace(text, "return agent.call(packet(node, plan, checkpoint, notes), opts)",
			"return agent.call(packet(node, plan, checkpoint, notes) + (node === '"+node+"' ? '\\n\\nEDITED PROMPT' : ''), opts)", 1)
	}
}

// mismatchingWhile is mismatching, for as long as the condition holds.
func mismatchingWhile(l *Lab, cond func() bool, nodes ...string) {
	for _, node := range nodes {
		l.On("^(build|fix):"+node+":", func(c *Call) *fakehostPlan {
			if !cond() {
				return nil
			}
			c.Edit()
			p := Say(ReceiptWith("ready_for_review", map[string]any{"plan_mismatch": "the plan assumed something else: " + c.Label}))
			return &p
		})
	}
}

// superseded says whether the last row of a call is a superseded one.
func supersededCalls(l *Lab, since int) map[string]bool {
	out := map[string]bool{}
	for _, ev := range rowsAfter(l, since) {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvSuperseded {
			out[ev.CallID] = true
		}
	}
	return out
}

// supersededWorld is the run the two resume scenarios start from: node A, whose RECEIPTs report two plan mismatches (signal 2 makes
// r1), and node Z, whose lane starts after r1 was recorded, so that the nodes waiting for Z are released under r1. The review of A's
// last round is held (the host is overloaded): A is not accepted when the run ends, incomplete.
type supersededWorld struct {
	l      *Lab
	edited atomic.Bool
}

// supersededSetup says how the world is built.
type supersededSetup struct {
	// After are the nodes that wait for Z, with the slices of the plan they have when the process ends.
	After []string
	// Change is the node whose slice of the plan changes when r1 is recorded, so that it is released with another slice than the
	// one the revision that stands (r0) was made against.
	Change string
	// Stop ends the process at a row (a crash); nil lets the run end, incomplete, with A held.
	Stop func(engine.Event) bool
	// Cap is the executors the lane runs at once: a process that is to end at a row of one lane has no other lane in flight.
	Cap int
}

func newSupersededWorld(t *testing.T, setup supersededSetup) *supersededWorld {
	t.Helper()
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}},
		{ID: "Z", Issue: "CRW-Z", Title: "issue Z", Regions: []string{"z/"}},
	}
	for _, id := range setup.After {
		nodes = append(nodes, NodeSpec{ID: id, Issue: "CRW-" + id, Title: "issue " + id, Regions: []string{strings.ToLower(id) + "/"}, After: []string{"Z"}})
	}
	w := &supersededWorld{l: NewLab(t, nodes...)}
	l := w.l
	l.Cap = setup.Cap
	// the rule that holds the last review of A back is asked before the one that answers it
	l.On("^review:A:r2$", func(*Call) *fakehostPlan {
		if w.edited.Load() {
			return nil
		}
		p := Fail("serverOverloaded")
		return &p
	})
	mismatchingWhile(l, func() bool { return !w.edited.Load() }, "A")
	fixedOnce(l, "A")
	// the lane of Z starts after the revision was recorded
	zGate := make(chan struct{})
	l.On("^build:Z:executor$", func(c *Call) *fakehostPlan {
		if w.edited.Load() {
			return nil
		}
		c.Edit()
		p := Say(Receipt("ready_for_review"))
		p.Gate = zGate
		return &p
	})
	opened, stopped := false, false
	l.OnRow(func(ev engine.Event) {
		if !opened && ev.Kind == engine.KindCall && ev.Op == "crw.designLedger" && ev.Event == engine.EvCompleted && str(ev.Data, "revision") == "r1" {
			opened = true
			if setup.Change != "" {
				l.Relay.ChangeNode(setup.Change, func(n *NodeSpec) { n.Regions = append(slices.Clone(n.Regions), "changed/") })
			}
			close(zGate)
		}
		if !stopped && setup.Stop != nil && setup.Stop(ev) {
			stopped = true
			l.Kill()
		}
	})
	res := l.Run()
	if setup.Stop != nil {
		if !stopped {
			t.Fatalf("the run never reached the row it was to end at; calls %v", l.Labels())
		}
		return w
	}
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s: %s", res.Status, res.Error)
	}
	if got := Value[laneResult](t, res); got.State != "incomplete" {
		t.Fatalf("state %q: %s; calls %v", got.State, res.Value, l.Labels())
	}
	return w
}

// resume runs the project lane again in a new process with the build prompt of A edited, and the model of A saying what it
// does after the edit: no plan mismatch.
func (w *supersededWorld) resume(t *testing.T) (*engine.Result, int) {
	t.Helper()
	l := w.l
	l.OnRow(nil)
	w.edited.Store(true)
	before := l.LastSeq()
	l.NewProcess("proc-2")
	return l.execEdited(editBuildOf("A"), true), before
}

// The RECEIPTs of lane A reported two plan mismatches, which re-planned (signal 2, r1) and released B and C under r1; the lanes of B and
// C landed, and the process ended before A was accepted. A resume with the build prompt of A changed runs A again: the signals A's
// RECEIPTs made are the suffix, r1 is superseded and so is the call that made it, and the triggers that replayed - the RECEIPTs of Z, B
// and C - are counted by the signals that run now. B and C, whose slices of the plan are the ones the revision that stands was made
// against, keep their replay.
func TestSupersededTriggerSupersedesSignalAndRevision(t *testing.T) {
	w := newSupersededWorld(t, supersededSetup{After: []string{"B", "C"}})
	l := w.l
	if len(labelsWithPrefix(l, "replan:")) != 1 {
		t.Fatalf("calls %v", l.Labels())
	}
	if rev := releaseRevisions(l); rev["B"] != "r1" || rev["C"] != "r1" || rev["Z"] != "r0" {
		t.Fatalf("release revisions %v", rev)
	}
	replan := callNamed(t, l, "replan:s2")
	var ledgerR1 string
	for _, ev := range l.Rows("crw.designLedger") {
		if str(ev.Data, "revision") == "r1" {
			ledgerR1 = ev.CallID
		}
	}
	res, before := w.resume(t)
	got := Value[laneResult](t, res)
	if got.State != "closed" {
		t.Fatalf("resume: %s; calls %v; escalations %v", res.Value, l.Labels(), escalationReasons(l))
	}
	sup := supersededCalls(l, before)
	if !sup[replan.ID] || !sup[ledgerR1] {
		t.Errorf("the re-plan call %s and the ledger row of r1 %s are superseded: %v", replan.ID, ledgerR1, sup)
	}
	supRevs := l.FlowRows(ops.EvRevisionSuperseded)
	if len(supRevs) != 1 || str(supRevs[0].Data, "revision") != "r1" || num(supRevs[0].Data["signal"]) != 2 {
		t.Errorf("revision_superseded rows %+v", supRevs)
	}
	// nothing re-planned again: the mismatches A reported are gone with its suffix, and the revision that stands is r0 (or the
	// next one the ledger names, r2, if a re-plan had been made)
	if labels := labelsWithPrefix(l, "replan:"); !slices.Equal(labels, []string{"replan:s2"}) {
		t.Errorf("re-plan calls %v", labels)
	}
	for _, c := range l.LiveCalls(before) {
		if strings.Contains(c.Label, ":B:") || strings.Contains(c.Label, ":C:") || strings.HasPrefix(c.Label, "replan") || strings.Contains(c.Label, ":Z:") {
			t.Errorf("the resume ran %q live", c.Label)
		}
	}
	// the signals of the resume take the triggers in the journal's order: those of Z, B and C replayed, those of A ran again
	var members []string
	for _, b := range l.Rows("crw.signalBind") {
		if b.Seq <= before {
			continue
		}
		list, _ := b.Data["members"].([]any)
		for _, m := range list {
			members = append(members, str(m.(map[string]any), "node"))
		}
	}
	for _, n := range []string{"A", "Z", "B", "C"} {
		if !slices.Contains(members, n) {
			t.Errorf("the triggers of the signals of the resume: %v, want %s among them", members, n)
		}
	}
	if i, k := slices.Index(members, "Z"), slices.Index(members, "A"); i < 0 || k < 0 || i > k {
		t.Errorf("the triggers %v: the RECEIPT of Z replayed from before the RECEIPT of A ran again", members)
	}
	rows := l.Rows("crw.replanSignals")
	last := rows[len(rows)-1]
	if str(last.Data, "phase") != "close" || num(last.Data["plan_mismatch"]) != 0 || last.Data["due"] != false {
		t.Errorf("the close counted %+v", last.Data)
	}
	if rebinds := l.FlowRows(ops.EvReleaseRebind); len(rebinds) != 2 {
		t.Errorf("release_rebind rows %+v", rebinds)
	}
}

// A lane that was released under a revision a resume superseded is compared, once the first signal that runs again has ended, with
// the revision that stands: a node whose slice of the plan is the one that revision was made against keeps its replay (a release_rebind
// row, no live call); a node whose slice is another is released again under the revision that stands - with a new packet when it had not
// started, from its first position when it had started and was not accepted - and is refused when an acceptance rests on it.
func TestLaneUnderSupersededRevisionRebindsOrReruns(t *testing.T) {
	releaseOf := func(l *Lab, node string, since int) []engine.Event {
		var out []engine.Event
		for _, ev := range l.Events() {
			if ev.Seq > since && ev.Kind == engine.KindCall && ev.Op == "crw.release" && str(ev.Data, "node") == node {
				out = append(out, ev)
			}
		}
		return out
	}
	rows := []string{"P", "Q", "R"}
	t.Run("the slice is the same: the replay is kept", func(t *testing.T) {
		w := newSupersededWorld(t, supersededSetup{After: []string{"B", "C"}})
		res, before := w.resume(t)
		if got := Value[laneResult](t, res); got.State != "closed" {
			t.Fatalf("resume: %s", res.Value)
		}
		rebinds := w.l.FlowRows(ops.EvReleaseRebind)
		nodes := []string{}
		for _, ev := range rebinds {
			nodes = append(nodes, str(ev.Data, "node"))
			if str(ev.Data, "from_revision") != "r1" || str(ev.Data, "to_revision") != "r0" {
				t.Errorf("rebind %+v", ev.Data)
			}
		}
		slices.Sort(nodes)
		if !slices.Equal(nodes, []string{"B", "C"}) {
			t.Errorf("release_rebind rows for %v", nodes)
		}
		for _, c := range w.l.LiveCalls(before) {
			if strings.Contains(c.Label, ":B:") || strings.Contains(c.Label, ":C:") || strings.HasPrefix(c.Label, "review:B") || strings.HasPrefix(c.Label, "review:C") {
				t.Errorf("the lane of %q ran live", c.Label)
			}
		}
		if len(releaseOf(w.l, "B", before)) != 0 || len(releaseOf(w.l, "C", before)) != 0 {
			t.Errorf("B or C was released again")
		}
	})
	t.Run("the slice changed, the node had not started: a new packet", func(t *testing.T) {
		stop := func(ev engine.Event) bool {
			return ev.Kind == engine.KindCall && ev.Op == "crw.release" && ev.Event == engine.EvCompleted && str(ev.Data, "node") == "B" && flagOf(ev.Data, "released")
		}
		w := newSupersededWorld(t, supersededSetup{After: append([]string{"B"}, rows...), Change: "B", Stop: stop, Cap: 1})
		for _, c := range w.l.Calls() {
			if c.Node == "B" {
				t.Fatalf("the node had started: %s", c.Label)
			}
		}
		old := releaseOf(w.l, "B", 0)
		if len(old) != 1 || str(old[0].Data, "revision") != "r1" {
			t.Fatalf("release rows of B %+v", old)
		}
		res, before := w.resume(t)
		if got := Value[laneResult](t, res); got.State != "closed" {
			t.Fatalf("resume: %s; calls %v; escalations %v", res.Value, w.l.Labels(), escalationReasons(w.l))
		}
		now := releaseOf(w.l, "B", before)
		if len(now) != 1 || str(now[0].Data, "revision") != "r0" {
			t.Errorf("release rows of B after the resume: %+v", now)
		}
		for _, ev := range w.l.FlowRows(ops.EvReleaseRebind) {
			if str(ev.Data, "node") == "B" {
				t.Errorf("B was kept under the revision that stands: %+v", ev.Data)
			}
		}
		if text := textOfCall(w.l, callNamed(t, w.l, "build:B:executor").ID); !strings.Contains(text, "DESIGN REVISION: r0") {
			t.Errorf("the packet of B is not the one of the revision that stands:\n%s", text)
		}
	})
	t.Run("the slice changed, the node had started and was not accepted: it runs from its first position", func(t *testing.T) {
		stop := func(ev engine.Event) bool {
			return ev.Kind == engine.KindCall && ev.Op == "agent" && ev.Event == engine.EvCompleted && ev.Label == "build:B:executor"
		}
		w := newSupersededWorld(t, supersededSetup{After: append([]string{"B"}, rows...), Change: "B", Stop: stop, Cap: 1})
		if len(w.l.Rows("crw.accept")) > 0 {
			for _, ev := range w.l.Rows("crw.accept") {
				if str(ev.Data, "node") == "B" {
					t.Fatalf("B was accepted")
				}
			}
		}
		oldBuild := callNamed(t, w.l, "build:B:executor")
		res, before := w.resume(t)
		if got := Value[laneResult](t, res); got.State != "closed" {
			t.Fatalf("resume: %s; calls %v; escalations %v", res.Value, w.l.Labels(), escalationReasons(w.l))
		}
		live := map[string]bool{}
		for _, c := range w.l.LiveCalls(before) {
			live[c.Label] = true
		}
		if !live["build:B:executor"] || !live["review:B:r1"] {
			t.Errorf("live calls %v: the lane of B runs from its first position", live)
		}
		if !supersededCalls(w.l, before)[oldBuild.ID] {
			t.Errorf("the build of B the first process made was not superseded")
		}
		if now := releaseOf(w.l, "B", before); len(now) != 1 || str(now[0].Data, "revision") != "r0" {
			t.Errorf("release rows of B after the resume: %+v", now)
		}
	})
	t.Run("the slice changed and the node was accepted: refused with the route", func(t *testing.T) {
		stop := func(ev engine.Event) bool {
			return ev.Kind == engine.KindCall && ev.Op == "crw.accept" && ev.Event == engine.EvCompleted && str(ev.Data, "node") == "B"
		}
		w := newSupersededWorld(t, supersededSetup{After: append([]string{"B"}, rows...), Change: "B", Stop: stop, Cap: 1})
		res, before := w.resume(t)
		if got := Value[laneResult](t, res); got.State != "incomplete" {
			t.Fatalf("resume: %s; calls %v", res.Value, w.l.Labels())
		}
		var refused []engine.Event
		for _, ev := range rowsAfter(w.l, before) {
			if ev.Kind == engine.KindCall && ev.Op == "crw.release" && ev.Event == engine.EvRefused && str(ev.Data, "detail") != "" {
				refused = append(refused, ev)
			}
		}
		if len(refused) != 1 || !strings.Contains(str(refused[0].Data, "detail"), "accepted_node_changed") || !strings.Contains(str(refused[0].Data, "detail"), "update_node") || !strings.Contains(str(refused[0].Data, "detail"), "dag-correct") {
			t.Fatalf("refusals %+v", refused)
		}
		for _, c := range w.l.LiveCalls(before) {
			if c.Node == "B" || strings.Contains(c.Label, ":B:") || strings.HasPrefix(c.Label, "review:B") {
				t.Errorf("the lane of B ran %q although an acceptance rests on it", c.Label)
			}
		}
	})
}

// A signal is made of the triggers its row names, in the order of the journal, and a resume replays it with that membership: the lanes
// replay in an order of their own, and the order the host would finish their calls in now is not the order the signal was made in.
// Here B's build finished before A's (A's turn was slow), so the first signal is B's; the lanes replay A first.
func TestSignalMembershipFromJournal(t *testing.T) {
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}},
	}
	l := NewLab(t, nodes...)
	var resumed atomic.Bool
	l.On("^review:", func(*Call) *fakehostPlan {
		if resumed.Load() {
			return nil
		}
		p := Fail("serverOverloaded")
		return &p
	})
	l.On("^build:A:executor$", func(c *Call) *fakehostPlan {
		if resumed.Load() {
			return nil
		}
		c.Edit()
		p := Say(Receipt("ready_for_review"))
		p.Delay = 400 * time.Millisecond
		return &p
	})
	res := l.Run()
	first := Value[laneResult](t, res)
	if first.State != "incomplete" {
		t.Fatalf("state %q: %s; calls %v", first.State, res.Value, l.Labels())
	}
	membersOf := func(since int) (calls []string, nodes []string) {
		for _, b := range l.Rows("crw.signalBind") {
			if b.Seq <= since {
				continue
			}
			list, _ := b.Data["members"].([]any)
			for _, m := range list {
				calls = append(calls, str(m.(map[string]any), "call"))
				nodes = append(nodes, str(m.(map[string]any), "node"))
			}
		}
		return calls, nodes
	}
	recorded, order := membersOf(0)
	if !slices.Equal(order, []string{"B", "A"}) {
		t.Fatalf("the signals of the first process were made of %v, want B's RECEIPT first", order)
	}
	binds := l.Rows("crw.signalBind")
	before := l.LastSeq()
	resumed.Store(true)
	l.NewProcess("proc-2")
	got := Value[laneResult](t, l.Resume())
	if got.State != "closed" {
		t.Fatalf("resume: %+v; calls %v", got, l.Labels())
	}
	// the signals of the first process were replayed: their rows stand, they were not made again, and nobody superseded them
	for _, b := range l.Rows("crw.signalBind") {
		if b.Seq > before {
			if k := num(b.Data["k"]); k <= 2 {
				t.Errorf("signal %d was made again by the resume: %+v", k, b.Data)
			}
		}
	}
	sup := supersededCalls(l, before)
	for _, b := range binds {
		if sup[b.CallID] {
			t.Errorf("signal %d was superseded", num(b.Data["k"]))
		}
	}
	for _, c := range l.LiveCalls(before) {
		if strings.HasPrefix(c.Label, "build:") {
			t.Errorf("the resume ran %q again", c.Label)
		}
	}
	// and what they were made of is what the first process recorded
	again, order2 := membersOf(0)
	if !slices.Equal(again[:len(recorded)], recorded) || !slices.Equal(order2[:2], []string{"B", "A"}) {
		t.Errorf("members %v / %v after the resume, recorded %v", again, order2, recorded)
	}
	if rows := l.FlowRows(ops.EvRevisionSuperseded); len(rows) != 0 {
		t.Errorf("revision_superseded rows %+v", rows)
	}
}

// ---- the directory of an architect turn ----

// headIn is the HEAD of the worktree at dir, read while the turn that was sent there runs.
func headIn(l *Lab, dir string) string {
	out, err := l.gitErr(dir, "rev-parse", "HEAD")
	if err != nil {
		return "error: " + err.Error()
	}
	return strings.TrimSpace(out)
}

// The REFLECTION turn runs at the integrated head the final gate pinned, in a detached worktree of its own that the turn names as
// its cwd: the packet and the call's row carry the head, and the worktree is gone when the call has its terminal row.
func TestReflectionRunsAtIntegratedHead(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	var seenHead, seenCwd atomic.Value
	l.On("^reflection:M1$", func(c *Call) *fakehostPlan {
		seenCwd.Store(c.Cwd)
		seenHead.Store(headIn(l, c.Cwd))
		return nil
	})
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	h := l.Tip()
	call := callNamed(t, l, "reflection:M1")
	want := filepath.Join(l.State, "flow", l.RunID, "calls", call.ID, "wt")
	turns := l.turnsOf("reflection:")
	if len(turns) != 1 || turns[0].Cwd != want {
		t.Fatalf("the reflection turn named %+v, want cwd %s", turns, want)
	}
	if cwd, _ := seenCwd.Load().(string); cwd != want {
		t.Errorf("the directory of the call is %q, want %s", cwd, want)
	}
	if head, _ := seenHead.Load().(string); head != h {
		t.Errorf("the worktree of the turn was at %q, want the integrated head %s", head, h)
	}
	prepared := l.Prepared(call.ID)
	if str(prepared.Data, "head") != h {
		t.Errorf("the row of the call records head %v, want %s", prepared.Data["head"], h)
	}
	if alloc, _ := prepared.Data["allocation"].(map[string]any); str(alloc, "path") != want || str(alloc, "start_commit") != h || str(alloc, "kind") != engine.PlaceCall {
		t.Errorf("allocation %v", alloc)
	}
	// the packet: the head, the range from the head the DESIGN ran at, and the head each node landed with
	text := textOfCall(l, call.ID)
	first, _ := l.Rows("crw.designLedger")[0].Data["base_tip"].(string)
	if !strings.Contains(text, "HEAD: "+h) || !strings.Contains(text, "RANGE: "+first+".."+h) || !strings.Contains(text, "DESIGN RAN AT: "+first) {
		t.Errorf("the reflection packet:\n%s", text)
	}
	for _, ev := range l.Rows("crw.integrate") {
		if !strings.Contains(text, str(ev.Data, "node")+": "+str(ev.Data, "head")) {
			t.Errorf("the packet does not name the head %s landed with:\n%s", str(ev.Data, "node"), text)
		}
	}
	// the call has ended: its worktree is gone, and the DESIGN call's is not shared with it
	if _, err := os.Stat(want); err == nil {
		t.Errorf("the worktree of the ended reflection is still there")
	}
	arch := callNamed(t, l, "architect")
	if arch.ID == call.ID {
		t.Fatal("the same call")
	}
}

// The re-plan turn of a signal runs at the tip of the integration target that the signal row recorded when it decided to re-plan: the
// node that landed before it is in that worktree.
func TestReplanCwdIsCurrentTip(t *testing.T) {
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}},
		{ID: "Z", Issue: "CRW-Z", Title: "issue Z", Regions: []string{"z/"}},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}, After: []string{"A"}},
	}
	l := NewLab(t, nodes...)
	// A's lane is slow, so Z has landed when A's RECEIPTs make the signal (this rule is asked before the one that answers the fixes)
	l.On("^build:A:executor$", func(c *Call) *fakehostPlan {
		c.Edit()
		p := Say(ReceiptWith("ready_for_review", map[string]any{"plan_mismatch": "the plan assumed something else"}))
		p.Delay = 400 * time.Millisecond
		return &p
	})
	mismatching(l, "A")
	fixedOnce(l, "A")
	var seenHead, seenCwd atomic.Value
	l.On("^replan:s3$", func(c *Call) *fakehostPlan {
		seenCwd.Store(c.Cwd)
		seenHead.Store(headIn(l, c.Cwd))
		return nil
	})
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q; calls %v; escalations %v", got.State, l.Labels(), escalationReasons(l))
	}
	var tip string
	for _, b := range l.Rows("crw.signalBind") {
		if num(b.Data["k"]) == 3 {
			tip, _ = b.Data["tip"].(string)
		}
	}
	var zBatch string
	for _, ev := range l.Rows("crw.integrate") {
		if str(ev.Data, "node") == "Z" {
			zBatch = str(ev.Data, "batch_commit")
		}
	}
	base := l.Rows("crw.designLedger")[0].Data["base_tip"].(string)
	if tip == "" || tip != zBatch || tip == base {
		t.Fatalf("the signal row recorded tip %q; Z landed %s, the DESIGN ran at %s", tip, zBatch, base)
	}
	call := callNamed(t, l, "replan:s3")
	want := filepath.Join(l.State, "flow", l.RunID, "calls", call.ID, "wt")
	turns := l.turnsOf("replan:")
	if len(turns) != 1 || turns[0].Cwd != want {
		t.Fatalf("the re-plan turn named %+v, want cwd %s", turns, want)
	}
	if head, _ := seenHead.Load().(string); head != tip {
		t.Errorf("the worktree of the re-plan turn was at %q, want the tip the signal recorded, %s", head, tip)
	}
	if prepared := l.Prepared(call.ID); str(prepared.Data, "head") != tip {
		t.Errorf("the row of the call records head %v, want %s", prepared.Data["head"], tip)
	}
	text := textOfCall(l, call.ID)
	if !strings.Contains(text, "HEAD: "+tip) || !strings.Contains(text, "RANGE: "+base+".."+tip) || !strings.Contains(text, "Z: ") {
		t.Errorf("the re-plan packet:\n%s", text)
	}
	// the delta's own audit turns, and the fix of it, do not name the directory of the turn before them
	for _, tr := range l.turnsOf("design-fix:replan-s3") {
		if tr.Cwd == want {
			t.Errorf("%s names the directory of the re-plan turn", tr.Label)
		}
	}
}
