package scenario

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows"
)

// ---- the old reviewer path (kept until Z-1) ----

// The project lane judges a node by its unit check (CRW-1465) and calls no reviewer, but the old reviewer path stays: crw:lib/lane keeps
// crw.review (the reviewer thread and its record), crw.reviewHalt and crw.exploreEvidence, and crw.accept, the audit and the fix packet
// keep reading a reviewer's VERDICT, for the journals of old runs. The scenarios of that path that the node lane's tests ran before
// CRW-1465 run here against it, unchanged but for reviewerLane: the project lane with the node lane of dfc9d675 in place of its own
// (testdata/reviewer-node-lane.js). Fix round x1436 item 2 (criterion c6: no test is deleted).

// reviewerLane has the lab run the project lane with the old reviewer path's node lane: the embedded project lane's text with its node
// lane (from the comment that opens it to the build phase) replaced by testdata/reviewer-node-lane.js.
func reviewerLane(t *testing.T, l *Lab) {
	t.Helper()
	text, err := workflows.Source("project-lane")
	if err != nil {
		t.Fatal(err)
	}
	node, err := os.ReadFile("testdata/reviewer-node-lane.js")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(text, []byte("// The lane of one issue node."))
	end := bytes.Index(text, []byte("\nphase('build')"))
	if start < 0 || end < start {
		t.Fatalf("the project lane has no node lane between its opening comment and phase('build') (%d, %d): reviewerLane needs a new marker", start, end)
	}
	l.Source = slices.Concat(text[:start:start], node, text[end:])
}

// reviews scripts the verdicts of the reviewer calls of a node, one for each round.
func reviews(l *Lab, node string, answers ...map[string]any) {
	plans := make([]func(*Call) fakehostPlan, len(answers))
	for i, a := range answers {
		a := a
		plans[i] = func(*Call) fakehostPlan { return Say(a) }
	}
	l.Sequence("^review:"+node+":r\\d+$", plans...)
}

func reviewerOnly(t *testing.T, l *Lab, wantLabels []string) {
	t.Helper()
	if labels := l.Labels(); !slices.Equal(labels, wantLabels) {
		t.Fatalf("calls %v, want %v", labels, wantLabels)
	}
	for _, c := range l.Calls() {
		if strings.HasPrefix(c.Label, "fix:") {
			t.Errorf("an executor fix %q after a review that was not a verdict", c.Label)
		}
	}
	noTierStep(t, l)
}

// Defect is a defect of a verdict. Its impact follows the severity, one word of the closed set (CRW-1438): a P0-P2
// defect is core_function, a P3 defect minor_separable, so the fixture blocks exactly as its severity says.
func Defect(ref, kind, severity, finding string) map[string]any {
	impact := "core_function"
	if severity == "P3" {
		impact = "minor_separable"
	}
	return map[string]any{"ref": ref, "kind": kind, "severity": severity, "introduced": false, "impact": impact, "inPromise": false, "file": "node.txt", "line": 1, "finding": finding, "evidence_question": nil}
}

// EvidenceDefect is a defect of kind evidence_missing with its question.
func EvidenceDefect(ref, question string, files ...string) map[string]any {
	d := Defect(ref, "evidence_missing", "P2", "cannot tell from the head: "+question)
	if files == nil {
		files = []string{}
	}
	d["evidence_question"] = map[string]any{"question": question, "files_needed": files}
	return d
}

// A reviewer turn the host failed is a held call: the node is held, there is no fix and no tier evidence.
func TestReviewerHeldHoldsNoFix(t *testing.T) {
	l := NewLab(t, oneNode()...)
	reviewerLane(t, l)
	l.Sequence("^review:A:r1$", func(*Call) fakehostPlan { return Fail("serverOverloaded") })
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "held" || reason != "host_infra_serverOverloaded" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	reviewerOnly(t, l, []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1"})
	if rows := l.Rows("crw.hold"); len(rows) != 1 || str(rows[0].Data, "source") != "review" {
		t.Errorf("hold rows %+v", rows)
	}
	if rows := l.Rows("crw.review"); len(rows) != 1 || str(rows[0].Data, "state") != "held" {
		t.Errorf("review rows %+v", rows)
	}
}

// A reviewer call whose outcome is unknown holds the node: nothing is sent again, no fix, no tier evidence.
func TestReviewerUnknownHoldsNoFix(t *testing.T) {
	l := NewLab(t, oneNode()...)
	reviewerLane(t, l)
	l.Threads.DropAck("turn/start", func(_ int, params map[string]any) bool { return strings.Contains(inputText(params), "Review the head") })
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "held" || reason != "dispatch_unknown" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	reviewerOnly(t, l, []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1"})
	if got := l.Relay.CloseReasons(); !slices.Equal(got, []string{"A:dispatch_unknown"}) {
		t.Errorf("closes %v", got)
	}
}

// A reviewer that is refused - of the author's own model family (independence_refused), or by the host's configuration -
// escalates as a configuration fault, and nothing is fixed.
func TestReviewerRefusedEscalatesNoFix(t *testing.T) {
	t.Run("independence_refused", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		reviewerLane(t, l)
		// the executor runs on the reviewer's model family: its review is no independent second opinion
		spec := l.Policy.Roles["executor"]
		spec.Pair.Model = "gpt-fake-builder"
		l.Policy.Roles["executor"] = spec
		got := mustRunIncomplete(t, l)
		if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "independence_refused" {
			t.Fatalf("lane A: %s %s", state, reason)
		}
		reviewerOnly(t, l, []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1"})
		rows := l.FlowRows(engine.EvEscalation)
		if len(rows) != 1 || str(rows[0].Data, "class") != "configuration_fault" || str(rows[0].Data, "reason") != "independence_refused" {
			t.Errorf("escalations %+v", rows)
		}
		if r := l.Rows("crw.review"); len(r) != 1 || str(r[0].Data, "state") != "refused" {
			t.Errorf("review rows %+v", r)
		}
	})
	t.Run("host configuration", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		reviewerLane(t, l)
		l.Sequence("^review:A:r1$", func(*Call) fakehostPlan {
			return fakehostPlan{StartError: map[string]any{"code": -32600, "message": "bad request", "data": map[string]any{"codexErrorInfo": "badRequest"}}}
		})
		got := mustRunIncomplete(t, l)
		if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "host_config_badRequest" {
			t.Fatalf("lane A: %s %s", state, reason)
		}
		reviewerOnly(t, l, []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1"})
	})
}

// A reviewer that answers null_model is asked once more on a fresh reviewer thread; a second null_model escalates, and a verdict
// of the second thread is the review.
func TestReviewerNullModelRetriesFreshReviewerThenEscalates(t *testing.T) {
	t.Run("escalates", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		reviewerLane(t, l)
		l.Sequence("^review:A:r1", func(*Call) fakehostPlan { return Garbage() })
		got := mustRunIncomplete(t, l)
		if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "reviewer_null_model" {
			t.Fatalf("lane A: %s %s", state, reason)
		}
		reviewerOnly(t, l, []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1", "review:A:r1:retry"})
		first, second := callNamed(t, l, "review:A:r1"), callNamed(t, l, "review:A:r1:retry")
		if first.ID == second.ID || second.Resume != "" {
			t.Errorf("the retry is not a fresh reviewer thread: %+v", second)
		}
		if l.Prepared(first.ID).Data["cwd"] == nil {
			t.Log("reviewer calls carry their own directories")
		}
	})
	t.Run("the retry answers", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		reviewerLane(t, l)
		l.Sequence("^review:A:r1$", func(*Call) fakehostPlan { return Garbage() })
		got := mustRun(t, l)
		if got.State != "closed" {
			t.Fatalf("state %q", got.State)
		}
		if labels := l.Labels(); !slices.Equal(labels, []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1", "review:A:r1:retry", "reflection:M1"}) {
			t.Fatalf("calls %v", labels)
		}
		noTierStep(t, l)
	})
}

// d3: the explorer of an evidence question reads the committed head the reviewer asked about, not the integration base.
func TestEvidenceExplorerReadsTheCommittedHead(t *testing.T) {
	l := NewLab(t, oneNode()...)
	reviewerLane(t, l)
	reviews(l, "A", Verdict(false, EvidenceDefect("e1", "does the retry loop stop at the deadline?", "node-A.txt")), Verdict(true))
	var seen struct{ cwd, file string }
	l.On("^explore:A:", func(c *Call) *fakehostPlan {
		seen.cwd = c.Cwd
		_, err := l.gitErr(c.Cwd, "cat-file", "-e", "HEAD:node-A.txt")
		if err == nil {
			seen.file = "present"
		}
		return nil
	})
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	commits := l.Rows("crw.commit")
	if len(commits) == 0 {
		t.Fatal("no commit row")
	}
	head := str(commits[0].Data, "commit")
	exp := callNamed(t, l, l.Labels()[slices.IndexFunc(l.Labels(), func(s string) bool { return strings.HasPrefix(s, "explore:A:") })])
	prep := l.Prepared(exp.ID)
	if str(prep.Data, "head") != head {
		t.Errorf("the explorer call names head %q, the reviewed head is %s", str(prep.Data, "head"), head)
	}
	if seen.file != "present" {
		t.Errorf("the explorer's worktree %q does not hold the head's file node-A.txt: it was made at the integration base", seen.cwd)
	}
}

// A verdict with a defect of kind evidence_missing sends its evidence_question to an explorer before the node's next round; the
// answer goes into the fix packet and into the next reviewer packet, and no model interprets the finding again.
func TestEvidenceMissingSpawnsExplorerThenReviewerPacket(t *testing.T) {
	l := NewLab(t, oneNode()...)
	reviewerLane(t, l)
	reviews(l, "A", Verdict(false, EvidenceDefect("e1", "does the retry loop stop at the deadline?", "retry.go", "deadline.go")), Verdict(true))
	l.On("^explore:A:", func(*Call) *fakehostPlan {
		p := Say(map[string]any{"answer": "the loop stops at the deadline in retry.go:44", "evidence": []string{"retry.go:44"}, "verified": []string{"retry.go"}, "inferred": []string{}, "unknown": []string{}})
		return &p
	})
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	labels := l.Labels()
	if len(labels) < 7 || !strings.HasPrefix(labels[4], "explore:A:") || labels[5] != "fix:A:executor:r1" || !strings.HasPrefix(labels[6], "review:A:") {
		t.Fatalf("calls %v: want the explorer between the review and the fix, then the next review", labels)
	}
	answer := "the loop stops at the deadline in retry.go:44"
	fix, next := callNamed(t, l, "fix:A:executor:r1"), callNamed(t, l, labels[6])
	if text := textOfCall(l, fix.ID); !strings.Contains(text, answer) {
		t.Errorf("the fix packet does not carry the explorer's answer:\n%s", text)
	}
	if text := textOfCall(l, next.ID); !strings.Contains(text, answer) {
		t.Errorf("the next reviewer packet does not carry the explorer's answer:\n%s", text)
	}
	if text := textOfCall(l, callNamed(t, l, labels[4]).ID); !strings.Contains(text, "does the retry loop stop at the deadline?") || !strings.Contains(text, "retry.go") {
		t.Errorf("the explorer was not given the evidence question and its files:\n%s", text)
	}
	found := false
	for _, r := range l.SpawnRows() {
		if r.Rule == ops.RuleExpl3 && r.Decision == "spawn" && r.Node == "A" {
			found = true
		}
	}
	if !found {
		t.Errorf("no SP-EXPL-3 spawn decision: %+v", l.SpawnRows())
	}
	noTierStep(t, l)
}

// Four reviewed rounds on the tier without a verified verdict, each with a finding of its own: SP-TIER-1 (d).
func TestRoundCapStepsTier(t *testing.T) {
	l := NewLab(t, oneNode()...)
	reviewerLane(t, l)
	reviews(l, "A",
		Verdict(false, Defect("d1", "correctness", "P2", "first finding")),
		Verdict(false, Defect("d2", "test_gap", "P2", "second finding")),
		Verdict(false, Defect("d3", "scope", "P2", "third finding")),
		Verdict(false, Defect("d4", "maintainability", "P2", "fourth finding")),
		Verdict(true))
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q: %+v", got.State, got)
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1", "fix:A:executor:r1", "review:A:r2", "fix:A:executor:r2", "review:A:r3", "fix:A:executor:r3", "review:A:r4", "build:A:executor-t1", "review:A:r5", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	onlyStep(t, l, "A", ops.TierEvidenceRounds)
}

// TestReviewerPathFreshThreadCarriesTheExplorerFindings (x1436 round 2 item 3): on the old reviewer path the explorer's answers to an
// evidence question are notes the lane keeps, and a fresh executor thread of the node - the rebuild after a fix that ends null_model,
// the executor-t1 after a tier step - gets them in its packet (FINDINGS OF THE EXPLORER), as before CRW-1469: packet() still takes
// the old path's notes where the node lane passes its last state.
func TestReviewerPathFreshThreadCarriesTheExplorerFindings(t *testing.T) {
	const answer = "the loop stops at the deadline in retry.go:44"
	explorer := func(l *Lab) {
		l.On("^explore:A:", func(*Call) *fakehostPlan {
			p := Say(map[string]any{"answer": answer, "evidence": []string{"retry.go:44"}, "verified": []string{"retry.go"}, "inferred": []string{}, "unknown": []string{}})
			return &p
		})
	}
	carries := func(t *testing.T, l *Lab, label string) {
		t.Helper()
		text := textOfCall(l, callNamed(t, l, label).ID)
		if !strings.Contains(text, "FINDINGS OF THE EXPLORER") || !strings.Contains(text, answer) {
			t.Errorf("the packet of %s does not carry the explorer's answer:\n%s", label, text)
		}
	}
	t.Run("rebuild after a null_model fix", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		reviewerLane(t, l)
		reviews(l, "A", Verdict(false, EvidenceDefect("e1", "does the retry loop stop at the deadline?", "retry.go")), Verdict(true))
		explorer(l)
		l.Sequence("^fix:A:executor:r1$", func(*Call) fakehostPlan { return Garbage() })
		if got := mustRun(t, l); got.State != "closed" {
			t.Fatalf("state %q; calls %v", got.State, l.Labels())
		}
		carries(t, l, "rebuild:A:executor:r2")
	})
	t.Run("executor-t1 after a tier step", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		reviewerLane(t, l)
		same := EvidenceDefect("e1", "does the retry loop stop at the deadline?", "retry.go")
		reviews(l, "A", Verdict(false, same), Verdict(false, same), Verdict(true))
		explorer(l)
		if got := mustRun(t, l); got.State != "closed" {
			t.Fatalf("state %q; calls %v", got.State, l.Labels())
		}
		carries(t, l, "build:A:executor-t1")
	})
}
