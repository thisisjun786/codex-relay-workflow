package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
)

// The landing path of a head made outside a project-lane run (CRW-1354): the gate-only workflow land-head takes the issue's node, the head
// the child made and the subagent threads that wrote it, and lands the head through the gates every head of a run goes through -
// crw.verify (verification-record/1 of the exact tree), crw.unitCheck of the committed range base..head (the review tool run in Go,
// CRW-1468; the relay takes it only from another model family than the authors, by the runtime's read-back), crw.accept (the relay's
// agent acceptance) and crw.integrate - writing the rows those operations write. No
// executor call is made and none is recorded: the authors are the threads the child names, whose pair the run reads back from the host
// and the release records on the node's execution, which is what the relay's acceptance judges the reviewer against.

// outsideHead makes a commit on a branch of the lab's repository outside any run: the work of the issue's node, as a subagent of the child
// wrote it, on a worktree of its own. It answers the commit.
func (l *laneLab) outsideHead(key, branch string) string {
	l.t.Helper()
	dir := filepath.Join(l.t.TempDir(), "outside")
	l.git(l.repo, "worktree", "add", "-q", "-b", branch, dir, "integration")
	if err := os.WriteFile(filepath.Join(dir, "node-"+key+".txt"), []byte("the work of "+key+", made outside a run\n"), 0o644); err != nil {
		l.t.Fatal(err)
	}
	l.git(dir, "add", "-A")
	l.git(dir, "commit", "-q", "-m", "outside: "+key)
	return l.git(dir, "rev-parse", "HEAD")
}

// authorThread is a subagent thread of the child on the fake App Server that ran on the pair.
func (l *laneLab) authorThread(model, effort string) string {
	l.t.Helper()
	owner := l.host.Threads().Seed(fakehost.SeedThread{Cwd: l.repo, Model: "gpt-fake-orchestrator", Effort: "high", Turns: []fakehost.SeedTurn{{Text: "the child works outside a run"}}})
	return l.host.Threads().Spawn(fakehost.SubagentSpec{Parent: owner, AgentType: "worker", Model: model, Effort: effort, Message: "write the work of the node", Plan: fakehost.TurnPlan{Message: "done"}})
}

type landValue struct {
	State, Node, Reason, Head, Acceptance, Tip string
	Escalation                                 string
}

func landArgs(l *laneLab, key, head string, authors ...string) string {
	raw, _ := json.Marshal(map[string]any{"plan": l.plan, "milestone": "M1", "issue": key, "head": head, "authors": authors})
	return string(raw)
}

// TestLandHeadLandsAHeadMadeOutsideARun (CRW-1354): over the fake App Server, a head the child's subagent (a Claude pair) made on a branch
// outside any run lands on the target branch through land-head: the node is released to the run (one agent execution, recording the
// author pair the run read back from the subagent's thread and the run's worktree of the head), the head is verified, a unit check of
// another family (the lab's grok checker) verifies its committed range, the relay accepts it on its agent path and the integration merges
// it. The journal holds the rows of crw.verify, crw.unitCheck, crw.accept and crw.integrate and no executor or reviewer call; the
// relay's acceptance judged the check against the recorded author.
func TestLandHeadLandsAHeadMadeOutsideARun(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	head := l.outsideHead("A-1", "outside-a1")
	author := l.authorThread("claude-fake-sonnet", "high")
	spec := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	res := l.runToEnd(spec.RunID)
	var v landValue
	if res.Status != engine.RunCompleted || json.Unmarshal(res.Value, &v) != nil || v.State != "landed" || v.Head != head || v.Acceptance == "" {
		for _, e := range l.events() {
			if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation {
				t.Logf("escalation: %v", e.Data)
			}
		}
		t.Fatalf("the head made outside a run did not land: %s %s %s\n%s", res.Status, res.ErrorKind, res.Error, res.Value)
	}
	ops := map[string]int{}
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Event == engine.EvCompleted && strings.HasPrefix(e.Op, "crw.") {
			ops[e.Op]++
		}
		if e.Kind == engine.KindCall && e.Event == engine.EvPrepared {
			if role := str(e.Data, "role"); role == "executor" || role == "executor-t1" || role == "architect" || role == "reviewer" {
				t.Errorf("an author call was made by the gate-only run: %v", e.Data)
			}
		}
	}
	if ops["crw.review"] != 0 {
		t.Errorf("%d crw.review rows, want none (rows %v)", ops["crw.review"], ops)
	}
	for _, op := range []string{"crw.release", "crw.verify", "crw.unitCheck", "crw.accept", "crw.integrate"} {
		if ops[op] != 1 {
			t.Errorf("%d %s rows, want 1 (rows %v)", ops[op], op, ops)
		}
	}
	l.effectsOnce("A-1")
	var model, effort string
	if err := l.w.st.DB.QueryRow("SELECT author_model, author_effort FROM dag_agent_executions WHERE node_id = ?", l.nodeOf["A-1"]).Scan(&model, &effort); err != nil || model != "claude-fake-sonnet" || effort != "high" {
		t.Errorf("the execution records the author pair %q %q: %v", model, effort, err)
	}
	var reviewer, accepted string
	if err := l.w.st.DB.QueryRow("SELECT reviewer_model, head FROM dag_agent_acceptances WHERE node_id = ?", l.nodeOf["A-1"]).Scan(&reviewer, &accepted); err != nil || reviewer != labCheckModel || accepted != head {
		t.Errorf("the acceptance: reviewer %q head %q: %v", reviewer, accepted, err)
	}
	if n := l.count("SELECT COUNT(*) FROM flow_calls WHERE run_id = ? AND role IN ('executor', 'executor-t1', 'architect', 'reviewer')", spec.RunID); n != 0 {
		t.Errorf("%d author rows in flow_calls", n)
	}
	if tipHas := l.git(l.repo, "merge-base", "--is-ancestor", head, "integration"); tipHas != "" {
		t.Errorf("merge-base: %s", tipHas)
	}
}

// unitCheckRowsOf are the completed crw.unitCheck rows of the current process's journal for a node.
func (l *laneLab) unitCheckRowsOf(node string) []engine.Event {
	var out []engine.Event
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Op == ops.OpUnitCheck && e.Event == engine.EvCompleted && str(e.Data, "node") == node {
			out = append(out, e)
		}
	}
	return out
}

// TestLandHeadUsesTheCommittedModeUnitCheck (CRW-1468): land-head judges the head it checked out by one unit check of the committed
// range base..head (crw.unitCheck in the committed mode, the review tool run in Go), not by a reviewer call. The check is given the land
// checkout and a prompt that names the range; its row records the commit, the base of the land release and receipt no_executor (a head
// made outside a run has no executor), and the relay's acceptance rests on it (basis unit_check, the check's model as the reviewer's).
// No reviewer call is prepared and no crw.review row is written. A check the tool gave no judgment for its quota holds the node through
// crw.hold with source unit_check: the run ends held and keeps its release (the execution is not closed and the slot stays).
func TestLandHeadUsesTheCommittedModeUnitCheck(t *testing.T) {
	t.Run("landed", func(t *testing.T) {
		l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
		head := l.outsideHead("A-1", "outside-a1")
		author := l.authorThread("claude-fake-sonnet", "high")
		spec := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
		if v := landValueOf(t, l.runToEnd(spec.RunID)); v.State != "landed" || v.Head != head || v.Acceptance == "" {
			t.Fatalf("the land on a committed-mode unit check: %+v", v)
		}
		node := l.nodeOf["A-1"]
		rels := l.releaseRowsOf(node)
		if len(rels) != 1 || str(rels[0].Data, "base") == "" || str(rels[0].Data, "worktree") == "" {
			t.Fatalf("the land release row: %+v", rels)
		}
		base, wt := str(rels[0].Data, "base"), str(rels[0].Data, "worktree")
		checks := l.unitCheckRowsOf(node)
		if len(checks) != 1 {
			t.Fatalf("%d crw.unitCheck rows of %s, want 1", len(checks), node)
		}
		row := checks[0].Data
		receipt, _ := row["receipt"].(map[string]any)
		if str(row, "role") != "unit-check" || str(row, "mode") != ops.UnitCheckCommitted || str(row, "commit") != head || str(row, "base") != base || str(row, "state") != "ok" ||
			str(row, "verdict") != "verified" || str(receipt, "state") != ops.ReceiptNoExecutor || str(row, "execution_id") == "" {
			t.Errorf("the unit-check row: %+v", row)
		}
		calls := l.uc.requests()
		if len(calls) != 1 || calls[0].Node != node || calls[0].Mode != ops.UnitCheckCommitted || calls[0].Request.Worktree != wt ||
			!strings.Contains(calls[0].Request.Prompt, "Review the changes between "+base+" and "+head) {
			t.Errorf("the lab's checker was given %+v (base %s, worktree %s)", calls, base, wt)
		}
		for _, e := range l.events() {
			if e.Kind == engine.KindCall && e.Op == ops.OpReview {
				t.Errorf("a crw.review row: %s %v", e.Event, e.Data)
			}
			if e.Kind == engine.KindCall && e.Event == engine.EvPrepared && str(e.Data, "role") == "reviewer" {
				t.Errorf("a reviewer call was prepared: %v", e.Data)
			}
			if e.Kind == engine.KindCall && e.Op == ops.OpAccept && e.Event == engine.EvCompleted && (str(e.Data, "basis") != "unit_check" || str(e.Data, "reviewer_call") != checks[0].CallID) {
				t.Errorf("the acceptance does not rest on the unit check %s: %v", checks[0].CallID, e.Data)
			}
		}
		if n := l.count("SELECT COUNT(*) FROM flow_calls WHERE run_id = ? AND role = 'reviewer'", spec.RunID); n != 0 {
			t.Errorf("%d reviewer rows in flow_calls", n)
		}
		var reviewer, accepted string
		if err := l.w.st.DB.QueryRow("SELECT reviewer_model, head FROM dag_agent_acceptances WHERE node_id = ?", node).Scan(&reviewer, &accepted); err != nil || reviewer != labCheckModel || accepted != head {
			t.Errorf("the acceptance: reviewer %q head %q: %v", reviewer, accepted, err)
		}
		l.effectsOnce("A-1")
	})
	t.Run("quota holds", func(t *testing.T) {
		l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
		l.outsideHead("A-1", "outside-a1")
		author := l.authorThread("claude-fake-sonnet", "high")
		l.UnitChecks("A-1", ops.UnitCheckCommitted, labCheck{State: reviewadapter.StateQuota})
		spec := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
		res := l.runToEnd(spec.RunID)
		var v struct{ State, Reason, Source string }
		if json.Unmarshal(res.Value, &v) != nil || v.State != "held" || v.Reason != "unit_check_quota" || v.Source != "unit_check" {
			t.Fatalf("a land whose unit check had no quota: %s %s %s\n%s", res.Status, res.ErrorKind, res.Error, res.Value)
		}
		node := l.nodeOf["A-1"]
		held := 0
		for _, e := range l.events() {
			if e.Kind == engine.KindCall && e.Op == ops.OpHold && e.Event == engine.EvCompleted && str(e.Data, "node") == node {
				if str(e.Data, "source") != "unit_check" || str(e.Data, "reason") != "unit_check_quota" {
					t.Errorf("the hold row: %v", e.Data)
				}
				held++
			}
			if e.Kind == engine.KindCall && e.Event == engine.EvCompleted && (e.Op == ops.OpReview || e.Op == ops.OpAccept || e.Op == ops.OpEscalate) {
				t.Errorf("a %s row after a held unit check: %v", e.Op, e.Data)
			}
		}
		if held != 1 {
			t.Errorf("%d crw.hold rows of %s, want 1", held, node)
		}
		if checks := l.unitCheckRowsOf(node); len(checks) != 1 || str(checks[0].Data, "hold") != "unit_check_quota" {
			t.Errorf("the unit-check rows: %+v", checks)
		}
		if n := l.count("SELECT COUNT(*) FROM dag_agent_closures WHERE subject_kind = 'execution' AND state = 'cancelled'"); n != 0 {
			t.Errorf("the held land gave its release back: %d cancelled closures", n)
		}
		if n := l.count("SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node' AND subject_key = ? AND state = 'held'", l.plan+"/"+node); n != 1 {
			t.Errorf("the held land holds %d slots of the node, want 1", n)
		}
		if n := l.count("SELECT COUNT(*) FROM dag_agent_acceptances"); n != 0 {
			t.Errorf("%d acceptances after a held unit check", n)
		}
	})
}

// TestLandHeadKeepsTheIndependenceGate (CRW-1354, CRW-1468): the same path with a subagent on the unit check's model family (a GPT
// author, and the lab's checker answering as a GPT pair) lands nothing: the check verifies the range, and the relay refuses the
// acceptance on it independence_refused (the gate, an escalation of the configuration that gives the release back), so nothing is
// accepted or integrated; a head one of whose authors the run cannot read back is independence_unproven. A gate refuses; nothing else does.
func TestLandHeadKeepsTheIndependenceGate(t *testing.T) {
	for _, c := range []struct {
		name, reason string
		authors      func(l *laneLab) []string
	}{
		{"same family", "independence_refused", func(l *laneLab) []string {
			l.uc.setDefault(labCheck{Family: "gpt", Model: "gpt-fake-sol", Effort: "high"})
			return []string{l.authorThread("gpt-fake-worker", "high")}
		}},
		{"no author read back", "independence_unproven", func(l *laneLab) []string { return []string{"0190a000-0000-7000-8000-ffffffffffff"} }},
		// post-evaluation d2: an author of the head whose pair is not read back may be of the check's family, so one observed author of
		// another family does not prove the check independent
		{"one author not read back", "independence_unproven", func(l *laneLab) []string {
			return []string{l.authorThread("claude-fake-sonnet", "high"), "0190a000-0000-7000-8000-ffffffffffff"}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
			l.outsideHead("A-1", "outside-a1")
			spec := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", c.authors(l)...), "--handoff", l.file)
			res := l.runToEnd(spec.RunID)
			var v landValue
			if res.Status != engine.RunCompleted || json.Unmarshal(res.Value, &v) != nil || v.State != "escalated" || v.Reason != c.reason || v.Escalation == "" {
				t.Fatalf("a land of a head whose author is %s: %s %s %s\n%s", c.name, res.Status, res.ErrorKind, res.Error, res.Value)
			}
			if n := l.count("SELECT COUNT(*) FROM dag_agent_acceptances") + l.count("SELECT COUNT(*) FROM dag_integration_batches"); n != 0 {
				t.Errorf("%d acceptance or integration rows after a refused acceptance", n)
			}
			node := l.nodeOf["A-1"]
			if checks := l.unitCheckRowsOf(node); len(checks) != 1 || str(checks[0].Data, "state") != "ok" || str(checks[0].Data, "verdict") != "verified" {
				t.Errorf("the unit check of the head: %+v", checks)
			}
			escalated := false
			for _, e := range l.events() {
				if e.Kind == engine.KindCall && e.Op == ops.OpReview {
					t.Errorf("a crw.review row: %s %v", e.Event, e.Data)
				}
				if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation && str(e.Data, "node") == node {
					escalated = str(e.Data, "reason") == c.reason && str(e.Data, "ask") == "configuration" && str(e.Data, "class") == "configuration_fault"
				}
			}
			if !escalated {
				t.Errorf("no escalation of the configuration %s", c.reason)
			}
			if n := l.count("SELECT COUNT(*) FROM dag_agent_closures WHERE subject_kind = 'execution' AND state = 'cancelled' AND reason = 'land_ended'"); n != 1 {
				t.Errorf("the refused land gave its release back %d times, want 1", n)
			}
			t.Logf("%s: %s", c.name, res.Value)
		})
	}
}

// outsideFix makes another head of the node's work outside any run, on a branch of its own: the child's change after a land that did not
// pass its gates.
func (l *laneLab) outsideFix(key, branch, text string) string {
	l.t.Helper()
	dir := filepath.Join(l.t.TempDir(), "outside-fix")
	l.git(l.repo, "worktree", "add", "-q", "-b", branch, dir, "integration")
	if err := os.WriteFile(filepath.Join(dir, "node-"+key+".txt"), []byte(text), 0o644); err != nil {
		l.t.Fatal(err)
	}
	l.git(dir, "add", "-A")
	l.git(dir, "commit", "-q", "-m", "outside fix: "+key)
	return l.git(dir, "rev-parse", "HEAD")
}

// needsChanges scripts the next committed-mode unit check of a node: a check that needs changes, with a blocking finding and one that
// blocks nothing. It answers the ids of the findings.
func (l *laneLab) needsChanges(key string) []string {
	l.t.Helper()
	findings := []reviewadapter.Finding{
		{Severity: "high", Impact: "core_function", File: "node-" + key + ".txt", Line: 1, Text: "the work is incomplete"},
		{Severity: "low", Impact: "style", File: "node-" + key + ".txt", Line: 1, Text: "the line has no full stop"},
	}
	l.UnitChecks(key, ops.UnitCheckCommitted, labCheck{Verdict: reviewadapter.VerdictNeedsChanges, Findings: findings})
	return []string{reviewadapter.FindingID(findings[0].File, findings[0].Text), reviewadapter.FindingID(findings[1].File, findings[1].Text)}
}

// landValueOf is the value of a land-head run that ended without being killed.
func landValueOf(t *testing.T, res Result) landValue {
	t.Helper()
	var v landValue
	if res.Status != engine.RunCompleted || json.Unmarshal(res.Value, &v) != nil {
		t.Fatalf("the land run: %s %s %s\n%s", res.Status, res.ErrorKind, res.Error, res.Value)
	}
	return v
}

// TestLandHeadLandsAgainAfterALandThatDidNotPass (CRW-1354, post-evaluation d1): the gate-only lane fixes nothing, so a head that does not
// pass its gates is the child's to change and land again - in a new land-head run. A land that ends escalated gives its release back:
// the relay closes the node's execution (land_ended, cancelled, the slot returned in the same transaction), so the next land of the
// node is a new release - a new execution of the new run, its worktree and the pair of the authors it names - and lands through the same
// gates. Two firsts: a unit check that needs changes (land_review_needs_changes, the ids of all its findings carried on the escalation
// and in the land's value: the child fixes them outside), and authors the run could not read back (independence_unproven).
func TestLandHeadLandsAgainAfterALandThatDidNotPass(t *testing.T) {
	for _, c := range []struct {
		name, reason string
		needs        bool
		authors      func(l *laneLab) []string
	}{
		{"review needs changes", "land_review_needs_changes", true, func(l *laneLab) []string { return []string{l.authorThread("claude-fake-sonnet", "high")} }},
		{"authors not read back", "independence_unproven", false, func(l *laneLab) []string { return []string{"0190a000-0000-7000-8000-ffffffffffff"} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
			var carried []string
			if c.needs {
				carried = l.needsChanges("A-1")
			}
			first := l.outsideHead("A-1", "outside-a1")
			run1 := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", c.authors(l)...), "--handoff", l.file)
			res1 := l.runToEnd(run1.RunID)
			if v := landValueOf(t, res1); v.State != "escalated" || v.Reason != c.reason || v.Head != first && c.needs {
				t.Fatalf("the first land: %+v, want escalated %s", v, c.reason)
			}
			if c.needs {
				var v struct{ Carried []string }
				if json.Unmarshal(res1.Value, &v) != nil || strings.Join(v.Carried, ",") != strings.Join(carried, ",") {
					t.Errorf("the land carries %q, want %q", v.Carried, carried)
				}
				found := false
				for _, e := range l.events() {
					if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation && str(e.Data, "reason") == c.reason {
						ev, _ := e.Data["evidence"].(map[string]any)
						got := []string{}
						if list, ok := ev["carried"].([]any); ok {
							for _, x := range list {
								s, _ := x.(string)
								got = append(got, s)
							}
						}
						detail := str(e.Data, "detail")
						found = strings.Join(got, ",") == strings.Join(carried, ",") && strings.Contains(detail, carried[0]+" node-A-1.txt the work is incomplete")
					}
				}
				if !found {
					t.Errorf("the escalation does not carry the findings %q", carried)
				}
			}
			var exec1 string
			if err := l.w.st.DB.QueryRow("SELECT execution_id FROM dag_agent_executions WHERE node_id = ?", l.nodeOf["A-1"]).Scan(&exec1); err != nil {
				t.Fatal(err)
			}
			if n := l.count("SELECT COUNT(*) FROM dag_agent_closures WHERE subject_kind = 'execution' AND subject_id = ? AND state = 'cancelled' AND reason = 'land_ended' AND slot_released = 1", exec1); n != 1 {
				t.Errorf("the escalated land did not give its release back: %d closures land_ended of %s", n, exec1)
			}
			if n := l.count("SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node' AND subject_key = ? AND state = 'held'", l.plan+"/"+l.nodeOf["A-1"]); n != 0 {
				t.Errorf("the node still holds %d slots after the escalated land", n)
			}

			second := l.outsideFix("A-1", "outside-a1-fix", "the work of A-1, changed after the land\n")
			run2 := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1-fix", l.authorThread("claude-fake-sonnet", "high")), "--handoff", l.file)
			v := landValueOf(t, l.runToEnd(run2.RunID))
			if v.State != "landed" || v.Head != second || v.Acceptance == "" {
				for _, e := range l.events() {
					if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation {
						t.Logf("escalation: %v", e.Data)
					}
				}
				t.Fatalf("the second land of the node did not land: %+v", v)
			}
			var exec2, run, model, wt string
			if err := l.w.st.DB.QueryRow("SELECT execution_id, run_id, author_model, worktree FROM dag_agent_executions WHERE node_id = ? AND supersedes_execution_id = ?", l.nodeOf["A-1"], exec1).Scan(&exec2, &run, &model, &wt); err != nil {
				t.Fatalf("no execution of the second land follows the first: %v", err)
			}
			if run != run2.RunID || model != "claude-fake-sonnet" || !strings.Contains(wt, run2.RunID) {
				t.Errorf("the second land's execution: run %q (want %s), author %q, worktree %q", run, run2.RunID, model, wt)
			}
			var accepted, execution string
			if err := l.w.st.DB.QueryRow("SELECT head, execution_id FROM dag_agent_acceptances WHERE node_id = ?", l.nodeOf["A-1"]).Scan(&accepted, &execution); err != nil || accepted != second || execution != exec2 {
				t.Errorf("the acceptance: head %q of %q (want %s of %s): %v", accepted, execution, second, exec2, err)
			}
			if out := l.git(l.repo, "merge-base", "--is-ancestor", second, "integration"); out != "" {
				t.Errorf("merge-base: %s", out)
			}
		})
	}
}

// TestLandHeadOfANodeAnotherRunHolds (CRW-1354, post-evaluation d1/d3): a land of a node whose current execution another run holds - a
// run that has not ended - releases nothing: the relay would answer that run's execution again, whose head only a review of that run can
// have accepted, so the land ends unreleased (land_node_held) naming the run and its execution, before any gate runs and with nothing
// written at the relay (no verification, no unit check). The run that holds the node lands it when it goes on (crw flow resume).
func TestLandHeadOfANodeAnotherRunHolds(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	l.outsideHead("A-1", "outside-a1")
	author := l.authorThread("claude-fake-sonnet", "high")
	// the first land's process dies right after the relay accepted its head, before it recorded the acceptance: the run has not ended
	l.killAt = l.writes + 2
	run1 := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	if _, killed := l.process(run1.RunID, false); !killed {
		t.Fatal("the first land was not killed")
	}
	l.killAt = 0
	executions := l.count("SELECT COUNT(*) FROM dag_agent_executions")
	run2 := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	v := landValueOf(t, l.runToEnd(run2.RunID))
	if v.State != "unreleased" || v.Reason != "land_node_held" {
		t.Fatalf("a land of a node another run holds: %+v", v)
	}
	if res, _ := ReadResult(l.w.state, run2.RunID); !strings.Contains(string(res.Value), run1.RunID) {
		t.Errorf("the land does not name the run that holds the node: %s", res.Value)
	}
	if n := l.count("SELECT COUNT(*) FROM dag_agent_executions"); n != executions {
		t.Errorf("%d executions after the refused land, %d before", n, executions)
	}
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Event == engine.EvCompleted && (e.Op == ops.OpVerify || e.Op == ops.OpUnitCheck || e.Op == ops.OpReview || e.Op == ops.OpAccept) {
			t.Errorf("a gate ran for a node another run holds: %s", e.Op)
		}
	}

	// the run that holds the node goes on where it stopped and lands it
	if code, killed := l.process(run1.RunID, true); code != 0 || killed {
		t.Fatalf("the resume of %s: exit %d (killed %v)", run1.RunID, code, killed)
	}
	res, ok := ReadResult(l.w.state, run1.RunID)
	if !ok {
		t.Fatalf("run %s left no result", run1.RunID)
	}
	if v := landValueOf(t, res); v.State != "landed" {
		t.Fatalf("the resumed land of the run that held the node: %+v", v)
	}
	l.effectsOnce("A-1")
}
