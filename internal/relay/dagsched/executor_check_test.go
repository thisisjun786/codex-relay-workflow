package dagsched

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
)

func TestExecutorCheckReadsAssignmentWithoutChangingIt(t *testing.T) {
	k, a := hostControlKit(t)
	ctx := context.Background()
	before := k.count("SELECT COUNT(*) FROM dag_agent_acceptances")
	got, err := k.sched.ExecutorCheckContext(ctx, a.Admission.Task.Identity.ExecutionID, "parent")
	if err != nil || got.Admission.TaskDigest != a.Admission.TaskDigest || got.Repository.Source != "https://github.com/owner/repo.git" {
		t.Fatalf("context: %+v %v", got, err)
	}
	if _, err = k.sched.ExecutorCheckContext(ctx, a.Admission.Task.Identity.ExecutionID, "stranger"); err == nil {
		t.Fatal("foreign owner read as authoritative")
	}
	if k.count("SELECT COUNT(*) FROM dag_agent_acceptances") != before {
		t.Fatal("check wrote acceptance")
	}
	k.replaceParent("parent-2")
	next := k.session("p", "parent-2", "session-2")
	adopted, err := next.Adopt(ctx, "p", "I", "parent-2")
	if err != nil {
		t.Fatal(err)
	}
	got, err = next.ExecutorCheckContext(ctx, adopted.ExecutionID, "parent-2")
	if err != nil || got.Admission.TaskDigest != a.Admission.TaskDigest {
		t.Fatalf("handover: %+v %v", got, err)
	}
	if _, err = k.sched.ExecutorCheckContext(ctx, a.Admission.Task.Identity.ExecutionID, "parent"); err == nil {
		t.Fatal("old owner remains current")
	}
}
func gateNamed(gates []executor.RequiredGate, name string) bool {
	for _, g := range gates {
		if g.Gate == name {
			return true
		}
	}
	return false
}
func TestExecutorCheckUsesActualGateJudgments(t *testing.T) {
	k := newAgentKit(t, agentSpec{name: "candidate", files: map[string]string{"candidate.txt": "work\n"}})
	ctx := context.Background()
	node := "candidate"
	in := k.input(node)
	verification, err := readVerificationRecord(in.Commit.Record)
	if err != nil {
		t.Fatal(err)
	}
	c := &executor.CollectedCandidate{Checkout: k.worktrees[node], Head: k.heads[node], Tree: k.treeOf(k.heads[node]), Base: k.base}
	id := executor.Identity{Plan: "g", Node: node, ExecutionID: k.execution(node), CriteriaDigest: releaseCriteriaDigest()}
	check := func(v, p []byte) []executor.RequiredGate {
		t.Helper()
		g, err := k.sched.CheckExecutorGates(ctx, id, "parent", c, v, p)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	gates := check(nil, nil)
	for _, name := range []string{"verification-record/1", "pre_merge", "review_or_unit_check"} {
		if !gateNamed(gates, name) {
			t.Fatalf("missing %s: %+v", name, gates)
		}
	}
	// A provider's narrative cannot substitute for flow_calls provenance.
	gates = check(verification, in.Premerge)
	if !gateNamed(gates, "review_or_unit_check") || gateNamed(gates, "verification-record/1") || gateNamed(gates, "pre_merge") {
		t.Fatalf("gates: %+v", gates)
	}
	k.author(node, authorModel)
	k.review(node, reviewerModel, "verified", releaseCriteriaDigest())
	if gates = check(verification, in.Premerge); len(gates) != 0 {
		t.Fatalf("genuine evidence: %+v", gates)
	}
	var record VerificationRecord
	if err := json.Unmarshal(verification, &record); err != nil {
		t.Fatal(err)
	}
	record.HeadCommit = strings.Repeat("a", 40)
	raw, err := SealVerificationRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if gates = check(raw, in.Premerge); !gateNamed(gates, "verification-record/1") {
		t.Fatalf("other head passed: %+v", gates)
	}
	k.review(node, reviewerModel, "needs_changes", releaseCriteriaDigest())
	if gates = check(verification, in.Premerge); !gateNamed(gates, "review_or_unit_check") {
		t.Fatal("later refusal ignored")
	}
	if k.agentAcceptanceRows() != 0 {
		t.Fatal("inspection wrote an acceptance")
	}
}

func TestExecutorCheckUsesCurrentUnitCheckEvidence(t *testing.T) {
	k := newAgentKit(t, agentSpec{name: "unit", files: map[string]string{"unit.txt": "work\n"}})
	in := k.input("unit")
	raw, err := readVerificationRecord(in.Commit.Record)
	if err != nil {
		t.Fatal(err)
	}
	id := executor.Identity{Plan: "g", Node: "unit", ExecutionID: k.execution("unit"), CriteriaDigest: releaseCriteriaDigest()}
	c := &executor.CollectedCandidate{Checkout: k.worktrees["unit"], Head: k.heads["unit"], Tree: k.treeOf(k.heads["unit"]), Base: k.base}
	k.unitCheck(unitCheckCall{node: "unit", res: passingUnitCheck(), commit: k.base, tree: c.Tree, criteria: id.CriteriaDigest})
	gates, err := k.sched.CheckExecutorGates(context.Background(), id, "parent", c, raw, in.Premerge)
	if err != nil || !gateNamed(gates, "review_or_unit_check") {
		t.Fatalf("wrong-head unit check: %+v %v", gates, err)
	}
	k.unitCheck(unitCheckCall{node: "unit", res: passingUnitCheck(), commit: c.Head, tree: c.Tree, criteria: id.CriteriaDigest})
	gates, err = k.sched.CheckExecutorGates(context.Background(), id, "parent", c, raw, in.Premerge)
	if err != nil || len(gates) != 0 {
		t.Fatalf("current unit check: %+v %v", gates, err)
	}
	k.unitCheck(unitCheckCall{node: "unit", res: passingUnitCheck(), commit: c.Head, tree: c.Tree, criteria: strings.Repeat("f", 64)})
	gates, err = k.sched.CheckExecutorGates(context.Background(), id, "parent", c, raw, in.Premerge)
	if err != nil || !gateNamed(gates, "review_or_unit_check") {
		t.Fatalf("stale criteria unit check: %+v %v", gates, err)
	}
	if k.agentAcceptanceRows() != 0 {
		t.Fatal("gate inspection accepted the candidate")
	}
}
