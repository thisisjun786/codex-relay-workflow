package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
)

// The landing path of a head made outside a project-lane run (CRW-1354): the workflow land-head takes the issue's node, the head
// the child made and the subagent threads that wrote it, and lands the head through the gates every head of a run goes through -
// crw.verify (verification-record/1 of the exact tree), crw.unitCheck of the committed range base..head (the review tool run in Go,
// CRW-1468; a separate review invocation whose model is recorded as provenance), crw.accept (the relay's
// agent acceptance) and crw.integrate - writing the rows those operations write. The authors are the threads the child names, whose pair
// the run reads back from the host and the release records on the node's execution, which is what the relay's acceptance judges the
// reviewer against. A check that needs changes is fixed in the run (CRW-1490): an executor call on the release's head, committed and
// checked again, up to the cap of fix rounds; a finding that is only missing evidence is asked of an explorer and checked again on the
// same head.

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
// the lab's grok checker verifies its committed range, the relay accepts it on its agent path and the integration merges
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

// Model families do not restrict a separately checked head; its original author records remain (CRW-1515).
func TestLandHeadPreservesAuthorsWithoutFamilyGate(t *testing.T) {
	for _, c := range []struct {
		name    string
		authors func(l *laneLab) []string
	}{
		{"same family", func(l *laneLab) []string {
			l.uc.setDefault(labCheck{Family: "gpt", Model: "gpt-fake-sol", Effort: "high"})
			return []string{l.authorThread("gpt-fake-worker", "high")}
		}},
		{"no author read back", func(l *laneLab) []string { return []string{"0190a000-0000-7000-8000-ffffffffffff"} }},
		{"one author not read back", func(l *laneLab) []string {
			return []string{l.authorThread("claude-fake-sonnet", "high"), "0190a000-0000-7000-8000-ffffffffffff"}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
			l.outsideHead("A-1", "outside-a1")
			authorIDs := c.authors(l)
			spec := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", authorIDs...), "--handoff", l.file)
			res := l.runToEnd(spec.RunID)
			var v landValue
			if res.Status != engine.RunCompleted || json.Unmarshal(res.Value, &v) != nil || v.State != "landed" || v.Acceptance == "" {
				t.Fatalf("a land of a head whose author is %s: %s %s %s\n%s", c.name, res.Status, res.ErrorKind, res.Error, res.Value)
			}
			if n := l.count("SELECT COUNT(*) FROM dag_agent_acceptances") + l.count("SELECT COUNT(*) FROM dag_integration_batches"); n != 2 {
				t.Errorf("%d acceptance or integration rows, want 2", n)
			}
			node := l.nodeOf["A-1"]
			if checks := l.unitCheckRowsOf(node); len(checks) != 1 || str(checks[0].Data, "state") != "ok" || str(checks[0].Data, "verdict") != "verified" {
				t.Errorf("the unit check of the head: %+v", checks)
			}
			for _, e := range l.events() {
				if e.Kind == engine.KindCall && e.Op == ops.OpReview {
					t.Errorf("a crw.review row: %s %v", e.Event, e.Data)
				}
			}
			rows := l.releaseRowsOf(node)
			if len(rows) != 1 {
				t.Fatalf("release rows: %+v", rows)
			}
			raw, err := json.Marshal(rows[0].Data["authors"])
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range authorIDs {
				if !strings.Contains(string(raw), id) {
					t.Errorf("original author %s missing from %s", id, raw)
				}
			}
			if n := l.count("SELECT COUNT(*) FROM dag_agent_closures WHERE subject_kind = 'execution' AND state = 'cancelled' AND reason = 'land_ended'"); n != 0 {
				t.Errorf("the landed execution was cancelled %d times", n)
			}
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

// TestLandHeadLandsAgainAfterALandThatDidNotPass (CRW-1354, post-evaluation d1; CRW-1490): the lane fixes a head that needs changes in
// the run, up to the cap of fix rounds, so a head that still does not pass its gates is the child's to change and land again - in a new
// land-head run. A land that ends escalated gives its release back:
// the relay closes the node's execution (land_ended, cancelled, the slot returned in the same transaction), so the next land of the
// node is a new release - a new execution of the new run, its worktree and the pair of the authors it names - and lands through the same
// gates. A land whose fix rounds ran out carries the last findings on the escalation and in its value for correction outside the run.
func TestLandHeadLandsAgainAfterALandThatDidNotPass(t *testing.T) {
	for _, c := range []struct {
		name, reason string
		needs        bool
		round        int
		authors      func(l *laneLab) []string
	}{
		{"fix rounds ran out", "land_fix_rounds", true, 2, func(l *laneLab) []string { return []string{l.authorThread("claude-fake-sonnet", "high")} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
			var carried []string
			if c.needs {
				carried = l.needsChanges("A-1")
			}
			first := l.outsideHead("A-1", "outside-a1")
			authors := c.authors(l)
			args := landArgs(l, "A-1", "outside-a1", authors...)
			if c.needs {
				args = landArgsRound(l, "A-1", "outside-a1", c.round, authors...)
			}
			run1 := l.start("start", "land-head", "--args", args, "--handoff", l.file)
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

// landArgsRound are the args of a land-head run of a fix round (CRW-1490): landArgs with fix_round.
func landArgsRound(l *laneLab, key, head string, round int, authors ...string) string {
	raw, _ := json.Marshal(map[string]any{"plan": l.plan, "milestone": "M1", "issue": key, "head": head, "authors": authors, "fix_round": round})
	return string(raw)
}

// landFixValue is the value of a land-head run whose unit check needed changes: the fix round it ran as, the findings it carries and
// the fix packet it hands the child.
type landFixValue struct {
	State, Reason, Escalation, Head, Fix string
	FixRound                             *int `json:"fix_round"`
	Carried                              []string
}

// landEscalationOf is the escalation row of a node with a reason in the current process's journal.
func (l *laneLab) landEscalationOf(node, reason string) (map[string]any, bool) {
	l.t.Helper()
	var row map[string]any
	for _, e := range l.events() {
		if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation && str(e.Data, "node") == node && str(e.Data, "reason") == reason {
			row = e.Data
		}
	}
	return row, row != nil
}

// landFixCalls counts the executor calls the current process's journal prepared for a node whose label is land-fix:<node>:<round>,
// and answers their labels in order.
func landFixCalls(l *laneLab, node string) []string {
	var out []string
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Event == engine.EvPrepared && strings.HasPrefix(e.Label, "land-fix:"+node+":") {
			out = append(out, e.Label)
		}
	}
	return out
}

// preparedCalls counts the calls of the given roles the current process's journal prepared.
func (l *laneLab) preparedCalls(roles ...string) int {
	n := 0
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Event == engine.EvPrepared && slices.Contains(roles, str(e.Data, "role")) {
			n++
		}
	}
	return n
}

// TestLandHeadNeedsChangesRunsFixWithCarriedFindings (CRW-1490): a land-head run of fix round 0 (no fix_round given) whose
// committed-mode unit check needs changes fixes in the run instead of handing the child a fix packet: one executor call
// (land-fix:<node>:r1) is given the carried findings - each by id with its file and text - and the range of the land (base..head);
// its work on the head the release checked out is committed and checked again, and a verified recheck lands the fixed head through
// the gates of the same run. Nothing escalates, the node is released once and the release is never given back: the run ends landed.
// The fixed head descends from the head the child made: the node's file at the landed head still holds the outside work, plus the
// fix the call wrote.
func TestLandHeadNeedsChangesRunsFixWithCarriedFindings(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	carried := l.needsChanges("A-1")
	original := l.outsideHead("A-1", "outside-a1")
	author := l.authorThread("claude-fake-sonnet", "high")
	spec := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	res := l.runToEnd(spec.RunID)
	v := landValueOf(t, res)
	if v.State != "landed" || v.Head == "" || v.Head == original || v.Acceptance == "" {
		for _, e := range l.events() {
			if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation {
				t.Logf("escalation: %v", e.Data)
			}
		}
		t.Fatalf("a land whose unit check needed changes did not fix in-run and land: %s %s %s\n%s", res.Status, res.ErrorKind, res.Error, res.Value)
	}
	node := l.nodeOf["A-1"]
	label := "land-fix:" + node + ":r1"
	if labels := landFixCalls(l, node); len(labels) != 1 || labels[0] != label {
		t.Errorf("the land-fix calls of %s: %v, want [%s]", node, labels, label)
	}
	if n := l.preparedCalls("reviewer", "critic", "crosscheck"); n != 0 {
		t.Errorf("%d reviewer or crosscheck calls in the land run, want none", n)
	}
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Op == ops.OpCrosscheck {
			t.Errorf("a crw.crosscheck row in the land run: %s %v", e.Event, e.Data)
		}
	}
	if _, ok := l.landEscalationOf(node, "land_review_needs_changes"); ok {
		t.Errorf("the run escalated land_review_needs_changes: the fix is the run's, not the child's")
	}
	rels := l.releaseRowsOf(node)
	if len(rels) != 1 {
		t.Fatalf("%d release rows of %s, want 1: the fix runs on the one release", len(rels), node)
	}
	if n := l.count("SELECT COUNT(*) FROM dag_agent_closures WHERE subject_kind = 'execution' AND state = 'cancelled'"); n != 0 {
		t.Errorf("%d cancelled closures, want 0: the in-run fix keeps the release", n)
	}
	l.mu.Lock()
	prompt := l.texts[label]
	l.mu.Unlock()
	base := str(rels[0].Data, "base")
	for _, want := range []string{carried[0], carried[1], "node-A-1.txt", "the work is incomplete", "the line has no full stop", base + ".." + original} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the fix call's packet lacks %q:\n%s", want, prompt)
		}
	}
	content := l.git(l.repo, "show", v.Head+":node-A-1.txt")
	if !strings.Contains(content, "the work of A-1, made outside a run\n") || !strings.Contains(content, label) {
		t.Errorf("the file of the node at the landed head %s is %q, want the outside work and the fix", v.Head, content)
	}
}

// TestLandHeadLandsAfterFixAndVerifiedRecheck (CRW-1490): the needs_changes → fix → recheck loop of one land-head run: the first
// committed check of the head the child made needs changes, the fix call's work is committed and verified, the recheck of the fixed
// head over the same base verifies it, and the acceptance and the integration land it - all in the one run, on the one release.
// The fixed head descends from the head the child made, and the acceptance is stored for it.
func TestLandHeadLandsAfterFixAndVerifiedRecheck(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	l.needsChanges("A-1")
	original := l.outsideHead("A-1", "outside-a1")
	author := l.authorThread("claude-fake-sonnet", "high")
	spec := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	v := landValueOf(t, l.runToEnd(spec.RunID))
	if v.State != "landed" || v.Head == "" || v.Head == original || v.Acceptance == "" {
		for _, e := range l.events() {
			if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation {
				t.Logf("escalation: %v", e.Data)
			}
		}
		t.Fatalf("the land after the fix and the verified recheck: %+v", v)
	}
	node := l.nodeOf["A-1"]
	rels := l.releaseRowsOf(node)
	if len(rels) != 1 {
		t.Fatalf("%d release rows of %s, want 1", len(rels), node)
	}
	base := str(rels[0].Data, "base")
	checks := l.unitCheckRowsOf(node)
	if len(checks) != 2 {
		t.Fatalf("%d crw.unitCheck rows of %s, want 2", len(checks), node)
	}
	if str(checks[0].Data, "commit") != original || str(checks[0].Data, "verdict") != "needs_changes" || str(checks[0].Data, "base") != base {
		t.Errorf("the check of the head the child made: %+v", checks[0].Data)
	}
	if str(checks[1].Data, "commit") != v.Head || str(checks[1].Data, "verdict") != "verified" || str(checks[1].Data, "base") != base {
		t.Errorf("the recheck of the fixed head: %+v", checks[1].Data)
	}
	var verifies []engine.Event
	fixID, seqFix, seqCommit, seqAccept, seqIntegrate := "", 0, 0, 0, 0
	for _, e := range l.events() {
		if e.Kind != engine.KindCall {
			continue
		}
		switch {
		case e.Event == engine.EvPrepared && e.Label == "land-fix:"+node+":r1":
			fixID = e.CallID
		case fixID != "" && e.Event == engine.EvCompleted && e.CallID == fixID:
			seqFix = e.Seq
		case e.Event == engine.EvCompleted && e.Op == ops.OpCommit && str(e.Data, "node") == node:
			seqCommit = e.Seq
		case e.Event == engine.EvCompleted && e.Op == ops.OpVerify && str(e.Data, "node") == node:
			verifies = append(verifies, e)
		case e.Event == engine.EvCompleted && e.Op == ops.OpAccept:
			seqAccept = e.Seq
		case e.Event == engine.EvCompleted && e.Op == ops.OpIntegrate:
			seqIntegrate = e.Seq
		}
	}
	if len(verifies) != 2 || str(verifies[0].Data, "head") != original || str(verifies[1].Data, "head") != v.Head ||
		verifies[0].Data["ok"] != true || verifies[1].Data["ok"] != true {
		t.Errorf("the verifications: %+v, want a pass of %s and a pass of %s", verifies, original, v.Head)
	}
	seqVerify2 := 0
	if len(verifies) == 2 {
		seqVerify2 = verifies[1].Seq
	}
	steps := []struct {
		name string
		seq  int
	}{
		{"the first check", checks[0].Seq},
		{"the fix call", seqFix},
		{"the commit", seqCommit},
		{"the verification of the fixed head", seqVerify2},
		{"the recheck", checks[1].Seq},
		{"the acceptance", seqAccept},
		{"the integration", seqIntegrate},
	}
	for i, step := range steps {
		if step.seq == 0 {
			t.Errorf("no row of %s", step.name)
			continue
		}
		if i > 0 && steps[i-1].seq > 0 && step.seq <= steps[i-1].seq {
			t.Errorf("the order: %s (seq %d) is not after %s (seq %d)", step.name, step.seq, steps[i-1].name, steps[i-1].seq)
		}
	}
	if out := l.git(l.repo, "merge-base", "--is-ancestor", original, v.Head); out != "" {
		t.Errorf("the fixed head does not descend from the head the child made: %s", out)
	}
	if out := l.git(l.repo, "merge-base", "--is-ancestor", v.Head, "integration"); out != "" {
		t.Errorf("the landed head is not on the target: %s", out)
	}
	var accepted string
	if err := l.w.st.DB.QueryRow("SELECT head FROM dag_agent_acceptances WHERE node_id = ?", node).Scan(&accepted); err != nil || accepted != v.Head {
		t.Errorf("the acceptance: head %q, want %s: %v", accepted, v.Head, err)
	}
	l.effectsOnce("A-1")
}

// TestLandHeadEscalatesWhenTheFixRoundsPassTheCap (CRW-1490): the run fixes in-run at most LAND_FIX_ROUNDS (2) times over the fix
// rounds it started with: a land that starts at fix_round r may run 2-r fixes, and a check that still needs changes then ends
// escalated land_fix_rounds with ask land_fix_rounds - a Question for the 부모 - with the carried findings of the last check and no
// fix packet, and gives the node's release back. Nothing is accepted or integrated.
func TestLandHeadEscalatesWhenTheFixRoundsPassTheCap(t *testing.T) {
	for _, c := range []struct {
		round, fixes, checks int
	}{
		{0, 2, 3},
		{1, 1, 2},
		{2, 0, 1},
	} {
		t.Run(fmt.Sprintf("fix_round %d", c.round), func(t *testing.T) {
			l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
			var carried []string
			for i := 0; i < c.checks; i++ {
				carried = l.needsChanges("A-1")
			}
			l.outsideHead("A-1", "outside-a1")
			spec := l.start("start", "land-head", "--args", landArgsRound(l, "A-1", "outside-a1", c.round, l.authorThread("claude-fake-sonnet", "high")), "--handoff", l.file)
			res := l.runToEnd(spec.RunID)
			var v landFixValue
			if res.Status != engine.RunCompleted || json.Unmarshal(res.Value, &v) != nil || v.State != "escalated" || v.Reason != "land_fix_rounds" || v.Escalation == "" {
				t.Fatalf("a land whose fix rounds passed the cap: %s %s %s\n%s", res.Status, res.ErrorKind, res.Error, res.Value)
			}
			if v.Fix != "" || v.FixRound == nil || *v.FixRound != 2 || strings.Join(v.Carried, ",") != strings.Join(carried, ",") {
				t.Errorf("the land's value: fix %q fix_round %v carried %q, want no fix packet, 2 and %q", v.Fix, v.FixRound, v.Carried, carried)
			}
			node := l.nodeOf["A-1"]
			if checks := l.unitCheckRowsOf(node); len(checks) != c.checks {
				t.Errorf("%d unit checks of %s, want %d", len(checks), node, c.checks)
			}
			if labels := landFixCalls(l, node); len(labels) != c.fixes {
				t.Errorf("the land-fix calls of %s: %v, want %d", node, labels, c.fixes)
			}
			row, ok := l.landEscalationOf(node, "land_fix_rounds")
			if !ok {
				t.Fatalf("no escalation land_fix_rounds of %s", node)
			}
			if ev, _ := row["evidence"].(map[string]any); str(row, "ask") != "land_fix_rounds" || ev["fix_round"] != float64(2) {
				t.Errorf("the escalation: ask %q evidence %v, want ask land_fix_rounds and fix_round 2", str(row, "ask"), ev)
			}
			if _, ok := l.landEscalationOf(node, "land_review_needs_changes"); ok {
				t.Errorf("a land past the fix rounds escalated land_review_needs_changes too")
			}
			for _, e := range l.events() {
				if e.Kind == engine.KindCall && e.Event == engine.EvCompleted && (e.Op == ops.OpAccept || e.Op == ops.OpIntegrate) {
					t.Errorf("a %s row after the fix rounds passed the cap: %v", e.Op, e.Data)
				}
			}
			if n := l.count("SELECT COUNT(*) FROM dag_agent_closures WHERE subject_kind = 'execution' AND state = 'cancelled' AND reason = 'land_ended'"); n != 1 {
				t.Errorf("the land gave its release back %d times, want 1", n)
			}
		})
	}
}

// evidenceRouteRounds counts the completed crw.exploreRoute rows of the current process's journal that routed questions of the
// kind evidence.
func evidenceRouteRounds(l *laneLab) int {
	n := 0
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Op == ops.OpExploreRoute && e.Event == engine.EvCompleted && str(e.Data, "kind") == ops.ExploreEvidence {
			n++
		}
	}
	return n
}

// TestLandHeadEvidenceMissingAsksAndRechecks (CRW-1490): a finding whose defect is missing verification evidence
// (kind evidence_missing, with the question the check wants answered) is not a fix: no executor call runs and nothing is committed -
// the run asks the question of an explorer (crw.explore of the kind evidence over the released head) and checks the same head again
// with the answer and its evidence in the recheck's prompt. A verified recheck lands the head the child made, unchanged. The ask is
// a fix round: a check that still needs changes after LAND_FIX_ROUNDS asks ends land_fix_rounds as a fixer's would. A needs_changes
// check with no findings, or with an ordinary finding among them, is no evidence question: it takes the executor path.
func TestLandHeadEvidenceMissingAsksAndRechecks(t *testing.T) {
	missing := func(t *testing.T) reviewadapter.Finding {
		var f reviewadapter.Finding
		if err := json.Unmarshal([]byte(`{"severity":"P2","impact":"criterion_unmet","file":"node-A-1.txt","line":1,"text":"the verification evidence is missing","kind":"evidence_missing","evidence_question":{"question":"Which command proves the node behavior?","files_needed":["node-A-1.txt"]}}`), &f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	scriptExplorer := func(l *laneLab) {
		l.turn = func(c labCall, _ fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
			if c.Role != "explorer" {
				return fakehost.TurnPlan{}, false
			}
			return labSay(map[string]any{
				"answer":   "the focused command passed",
				"evidence": []string{"go test ./node: PASS"},
				"verified": []string{"go test ./node: PASS"},
				"inferred": []string{},
				"unknown":  []string{},
			}), true
		}
	}
	needsEvidence := func(l *laneLab, f reviewadapter.Finding, times int) {
		for i := 0; i < times; i++ {
			l.UnitChecks("A-1", ops.UnitCheckCommitted, labCheck{Verdict: reviewadapter.VerdictNeedsChanges, Findings: []reviewadapter.Finding{f}})
		}
	}
	commits := func(l *laneLab, node string) int {
		n := 0
		for _, e := range l.events() {
			if e.Kind == engine.KindCall && e.Op == ops.OpCommit && e.Event == engine.EvCompleted && str(e.Data, "node") == node {
				n++
			}
		}
		return n
	}
	t.Run("verified after the evidence", func(t *testing.T) {
		l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
		needsEvidence(l, missing(t), 1)
		head := l.outsideHead("A-1", "outside-a1")
		author := l.authorThread("claude-fake-sonnet", "high")
		scriptExplorer(l)
		spec := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
		v := landValueOf(t, l.runToEnd(spec.RunID))
		if v.State != "landed" || v.Head != head || v.Acceptance == "" {
			for _, e := range l.events() {
				if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation {
					t.Logf("escalation: %v", e.Data)
				}
			}
			t.Fatalf("a land whose check lacked only evidence: %+v", v)
		}
		node := l.nodeOf["A-1"]
		if n := l.preparedCalls("executor", "executor-t1"); n != 0 {
			t.Errorf("%d executor calls, want 0: missing evidence is asked, not fixed", n)
		}
		if n := commits(l, node); n != 0 {
			t.Errorf("%d commits, want 0: the head the child made lands unchanged", n)
		}
		if n := evidenceRouteRounds(l); n != 1 {
			t.Errorf("%d evidence route rounds, want 1", n)
		}
		calls := l.uc.requests()
		if len(calls) != 2 {
			t.Fatalf("%d unit checks, want 2", len(calls))
		}
		for _, want := range []string{"Which command proves the node behavior?", "the focused command passed", "go test ./node: PASS"} {
			if !strings.Contains(calls[1].Request.Prompt, want) {
				t.Errorf("the recheck's prompt lacks %q:\n%s", want, calls[1].Request.Prompt)
			}
		}
		if str(l.unitCheckRowsOf(node)[0].Data, "verdict") != "needs_changes" || str(l.unitCheckRowsOf(node)[1].Data, "verdict") != "verified" {
			t.Errorf("the checks: %+v", l.unitCheckRowsOf(node))
		}
		verifies := 0
		for _, e := range l.events() {
			if e.Kind == engine.KindCall && e.Op == ops.OpVerify && e.Event == engine.EvCompleted && str(e.Data, "node") == node {
				verifies++
			}
		}
		if verifies != 1 {
			t.Errorf("%d verifications of the node, want 1 (the original head)", verifies)
		}
		if n := l.count("SELECT COUNT(*) FROM dag_agent_closures WHERE subject_kind = 'execution' AND state = 'cancelled'"); n != 0 {
			t.Errorf("%d cancelled closures, want 0", n)
		}
	})
	t.Run("a check not given the prompt holds", func(t *testing.T) {
		l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
		l.UnitChecks("A-1", ops.UnitCheckCommitted, labCheck{Verdict: reviewadapter.VerdictNeedsChanges, Findings: []reviewadapter.Finding{missing(t)}},
			labCheck{Verdict: reviewadapter.VerdictVerified, OmitPrompt: true})
		l.outsideHead("A-1", "outside-a1")
		author := l.authorThread("claude-fake-sonnet", "high")
		scriptExplorer(l)
		spec := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
		res := l.runToEnd(spec.RunID)
		var v struct{ State, Reason, Source string }
		if json.Unmarshal(res.Value, &v) != nil || v.State != "held" || v.Reason != "unit_check_no_answer" || v.Source != "unit_check" {
			t.Fatalf("a recheck the tool was not given the evidence of: %s %s %s\n%s", res.Status, res.ErrorKind, res.Error, res.Value)
		}
		if n := l.preparedCalls("executor", "executor-t1"); n != 0 {
			t.Errorf("%d executor calls, want 0", n)
		}
		if n := l.count("SELECT COUNT(*) FROM dag_agent_acceptances"); n != 0 {
			t.Errorf("%d acceptances of a check that read no evidence", n)
		}
		if n := l.count("SELECT COUNT(*) FROM dag_agent_closures WHERE subject_kind = 'execution' AND state = 'cancelled'"); n != 0 {
			t.Errorf("the held land gave its release back: %d cancelled closures", n)
		}
	})
	t.Run("evidence rounds pass the cap", func(t *testing.T) {
		l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
		needsEvidence(l, missing(t), 3)
		head := l.outsideHead("A-1", "outside-a1")
		author := l.authorThread("claude-fake-sonnet", "high")
		scriptExplorer(l)
		spec := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
		res := l.runToEnd(spec.RunID)
		var v landFixValue
		if res.Status != engine.RunCompleted || json.Unmarshal(res.Value, &v) != nil || v.State != "escalated" || v.Reason != "land_fix_rounds" || v.Head != head ||
			v.FixRound == nil || *v.FixRound != 2 || v.Fix != "" {
			t.Fatalf("a land whose evidence rounds passed the cap: %s %s %s\n%s", res.Status, res.ErrorKind, res.Error, res.Value)
		}
		node := l.nodeOf["A-1"]
		if checks := l.unitCheckRowsOf(node); len(checks) != 3 {
			t.Errorf("%d unit checks of %s, want 3", len(checks), node)
		}
		// the same question at the same head may reuse the explorer's answer, so the asks are two route rounds, not necessarily
		// two explorer calls
		if n := evidenceRouteRounds(l); n != 2 {
			t.Errorf("%d evidence route rounds, want 2", n)
		}
		if n := l.preparedCalls("executor", "executor-t1"); n != 0 || commits(l, node) != 0 {
			t.Errorf("%d executor calls and %d commits, want 0 and 0", n, commits(l, node))
		}
		if n := l.count("SELECT COUNT(*) FROM dag_agent_acceptances"); n != 0 {
			t.Errorf("%d acceptances, want 0", n)
		}
		if n := l.count("SELECT COUNT(*) FROM dag_agent_closures WHERE subject_kind = 'execution' AND state = 'cancelled' AND reason = 'land_ended'"); n != 1 {
			t.Errorf("the land gave its release back %d times, want 1", n)
		}
	})
	t.Run("a check without findings is no evidence question", func(t *testing.T) {
		l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
		l.UnitChecks("A-1", ops.UnitCheckCommitted, labCheck{Verdict: reviewadapter.VerdictNeedsChanges})
		l.outsideHead("A-1", "outside-a1")
		author := l.authorThread("claude-fake-sonnet", "high")
		scriptExplorer(l)
		spec := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
		v := landValueOf(t, l.runToEnd(spec.RunID))
		if v.State != "landed" {
			t.Fatalf("a land whose check needed changes without a finding: %+v", v)
		}
		if n := evidenceRouteRounds(l); n != 0 {
			t.Errorf("%d evidence route rounds for a check with no finding, want 0", n)
		}
		if labels := landFixCalls(l, l.nodeOf["A-1"]); len(labels) == 0 {
			t.Errorf("no land-fix call: a check that needs changes without evidence questions takes the executor path")
		}
	})
	t.Run("an ordinary finding among them takes the executor path", func(t *testing.T) {
		l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
		l.UnitChecks("A-1", ops.UnitCheckCommitted, labCheck{Verdict: reviewadapter.VerdictNeedsChanges,
			Findings: []reviewadapter.Finding{missing(t), {Severity: "P1", Impact: "core_function", File: "node-A-1.txt", Line: 2, Text: "the work is wrong"}}})
		l.outsideHead("A-1", "outside-a1")
		author := l.authorThread("claude-fake-sonnet", "high")
		scriptExplorer(l)
		spec := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
		v := landValueOf(t, l.runToEnd(spec.RunID))
		if v.State != "landed" {
			t.Fatalf("a land whose check had an ordinary finding: %+v", v)
		}
		if n := evidenceRouteRounds(l); n != 0 {
			t.Errorf("%d evidence route rounds for a mixed check, want 0", n)
		}
		if labels := landFixCalls(l, l.nodeOf["A-1"]); len(labels) == 0 {
			t.Errorf("no land-fix call: a check with an ordinary finding takes the executor path")
		}
	})
}

// A repair keeps the original author records while a verified separate review can land any model family (CRW-1515).
func TestLandHeadFixKeepsOriginalAuthorMetadata(t *testing.T) {
	for _, c := range []struct {
		name    string
		authors func(l *laneLab) []string
	}{
		{"an original author the run cannot read back", func(l *laneLab) []string { return []string{"0190a000-0000-7000-8000-ffffffffffff"} }},
		{"an original author of the checker's family", func(l *laneLab) []string { return []string{l.authorThread("grok-fake-sol", "high")} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
			l.needsChanges("A-1")
			l.outsideHead("A-1", "outside-a1")
			spec := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", c.authors(l)...), "--handoff", l.file)
			res := l.runToEnd(spec.RunID)
			var v landValue
			if res.Status != engine.RunCompleted || json.Unmarshal(res.Value, &v) != nil || v.State != "landed" || v.Acceptance == "" {
				t.Fatalf("a land whose head needed changes, by authors %s: %s %s %s\n%s", c.name, res.Status, res.ErrorKind, res.Error, res.Value)
			}
			node := l.nodeOf["A-1"]
			if labels := landFixCalls(l, node); len(labels) != 1 {
				t.Errorf("the land-fix calls of %s: %v, want 1", node, labels)
			}
			checks := l.unitCheckRowsOf(node)
			if len(checks) != 2 || str(checks[0].Data, "verdict") != "needs_changes" || str(checks[1].Data, "verdict") != "verified" {
				t.Errorf("the checks: %+v, want needs_changes then a verified recheck", checks)
			}
			if n := l.count("SELECT COUNT(*) FROM dag_agent_acceptances"); n != 1 {
				t.Errorf("%d acceptances, want 1", n)
			}
			if n := l.count("SELECT COUNT(*) FROM dag_agent_closures WHERE subject_kind = 'execution' AND state = 'cancelled' AND reason = 'land_ended'"); n != 0 {
				t.Errorf("the landed execution was cancelled %d times", n)
			}
		})
	}
}

// TestLandHeadRefusesAFixRoundThatIsNotANonNegativeInteger (CRW-1490): args.fix_round is the round of the child's fix, 0 when not
// given and otherwise a non-negative integer. A land-head run given the string "1", -1, 1.5 or null refuses at its start
// (fix_round_invalid): it reads no milestone, releases nothing, runs no gate and hands no fix packet, so a fix round that is not a
// count cannot change the cap of the fix rounds.
func TestLandHeadRefusesAFixRoundThatIsNotANonNegativeInteger(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	l.needsChanges("A-1")
	l.outsideHead("A-1", "outside-a1")
	author := l.authorThread("claude-fake-sonnet", "high")
	for _, round := range []any{"1", -1, 1.5, nil} {
		raw, _ := json.Marshal(map[string]any{"plan": l.plan, "milestone": "M1", "issue": "A-1", "head": "outside-a1", "authors": []string{author}, "fix_round": round})
		spec := l.start("start", "land-head", "--args", string(raw), "--handoff", l.file)
		res := l.runToEnd(spec.RunID)
		var v struct{ State, Reason, Detail, Fix string }
		if res.Status != engine.RunCompleted || json.Unmarshal(res.Value, &v) != nil || v.State != "refused" || v.Reason != "fix_round_invalid" || v.Fix != "" {
			t.Errorf("fix_round %#v: %s %s %s\n%s", round, res.Status, res.ErrorKind, res.Error, res.Value)
			continue
		}
		for _, e := range l.events() {
			if e.Kind == engine.KindCall && strings.HasPrefix(e.Op, "crw.") || e.Kind == engine.KindFlow && e.Event == engine.EvEscalation {
				t.Errorf("fix_round %#v: the refused run has the row %s %s %s", round, e.Kind, e.Op, e.Event)
			}
		}
	}
	if n := l.count("SELECT COUNT(*) FROM dag_agent_closures"); n != 0 {
		t.Errorf("%d closures after runs that refused at their start", n)
	}
}
