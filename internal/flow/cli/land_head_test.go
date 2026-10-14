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
)

// The landing path of a head made outside a project-lane run (CRW-1354): the gate-only workflow land-head takes the issue's node, the head
// the child made and the subagent threads that wrote it, and lands the head through the gates every head of a run goes through -
// crw.verify (verification-record/1 of the exact tree), crw.review (a reviewer of another model family than the authors, by the
// runtime's read-back), crw.accept (the relay's agent acceptance) and crw.integrate - writing the rows those operations write. No
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
// author pair the run read back from the subagent's thread and the run's worktree of the head), the head is verified, a GPT reviewer
// verifies it, the relay accepts it on its agent path and the integration merges it. The journal holds the rows of crw.verify,
// crw.review, crw.accept and crw.integrate and no executor call; the relay's acceptance judged the reviewer against the recorded author.
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
			if role := str(e.Data, "role"); role == "executor" || role == "executor-t1" || role == "architect" {
				t.Errorf("an author call was made by the gate-only run: %v", e.Data)
			}
		}
	}
	for _, op := range []string{"crw.release", "crw.verify", "crw.review", "crw.accept", "crw.integrate"} {
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
	if err := l.w.st.DB.QueryRow("SELECT reviewer_model, head FROM dag_agent_acceptances WHERE node_id = ?", l.nodeOf["A-1"]).Scan(&reviewer, &accepted); err != nil || !strings.HasPrefix(reviewer, "gpt") || accepted != head {
		t.Errorf("the acceptance: reviewer %q head %q: %v", reviewer, accepted, err)
	}
	if n := l.count("SELECT COUNT(*) FROM flow_calls WHERE run_id = ? AND role IN ('executor', 'executor-t1', 'architect')", spec.RunID); n != 0 {
		t.Errorf("%d author rows in flow_calls", n)
	}
	if tipHas := l.git(l.repo, "merge-base", "--is-ancestor", head, "integration"); tipHas != "" {
		t.Errorf("merge-base: %s", tipHas)
	}
}

// TestLandHeadKeepsTheIndependenceGate (CRW-1354): the same path with a subagent on the reviewer's model family (GPT) lands nothing: the
// review is refused independence_refused (the gate, an escalation of the configuration), the relay is not asked to accept, and a head whose
// authors the run cannot read back is independence_unproven. A gate refuses; nothing else does.
func TestLandHeadKeepsTheIndependenceGate(t *testing.T) {
	for _, c := range []struct {
		name, reason string
		authors      func(l *laneLab) []string
	}{
		{"same family", "independence_refused", func(l *laneLab) []string { return []string{l.authorThread("gpt-fake-worker", "high")} }},
		{"no author read back", "independence_unproven", func(l *laneLab) []string { return []string{"0190a000-0000-7000-8000-ffffffffffff"} }},
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
				t.Errorf("%d acceptance or integration rows after a refused review", n)
			}
			reviewed := false
			for _, e := range l.events() {
				if e.Kind == engine.KindCall && e.Op == ops.OpReview && e.Event == engine.EvCompleted {
					reviewed = str(e.Data, "state") == ops.StateRefused && str(e.Data, "reason") == c.reason
				}
			}
			if !reviewed {
				t.Errorf("no crw.review row refused %s", c.reason)
			}
			t.Logf("%s: %s", c.name, res.Value)
		})
	}
}
