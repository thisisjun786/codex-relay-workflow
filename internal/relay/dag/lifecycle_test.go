package dag

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func lifeNodeOf(t *testing.T, r *Repo, plan, node string) string {
	t.Helper()
	snap, _, err := r.Snapshot(context.Background(), plan, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range snap.Nodes {
		if n.NodeID == node {
			return n.Lifecycle
		}
	}
	t.Fatalf("no node %s", node)
	return ""
}

func twoNodes() doc { return revDoc("plan", "r1", 0, addNode("n", NodeNonPR), addNode("m", NodeNonPR)) }

// A lifecycle change is a revision of the log and nothing else: the node and edge rows do not move, no row is updated, and the plan the revision leaves
// has the same slice digests and (until it is paused) the same state digest.
func TestALifecycleChangeWritesOnlyARevision(t *testing.T) {
	r, s, _ := newRepo(t)
	first := mustPut(t, r, forkJoin("plan", "r1"))
	before := zoneRows(t, s.DB)
	paused := mustPut(t, r, revDoc("plan", "r2", 1, lifeChange(OpPauseNode, "design")))
	after := zoneRows(t, s.DB)
	for table, rows := range before {
		if table == "dag_plan_revisions" {
			if len(after[table]) != len(rows)+1 {
				t.Fatalf("%s holds %d rows, want %d", table, len(after[table]), len(rows)+1)
			}
			continue
		}
		if fmt.Sprint(rows) != fmt.Sprint(after[table]) {
			t.Errorf("a pause changed %s", table)
		}
	}
	if !reflect.DeepEqual(first.NodeDigests, paused.NodeDigests) {
		t.Fatal("a pause moved a slice digest")
	}
	if paused.StateDigest == first.StateDigest {
		t.Fatal("a paused plan digests as the active one")
	}
	back := mustPut(t, r, revDoc("plan", "r3", 2, lifeChange(OpResumeNode, "design")))
	if back.StateDigest != first.StateDigest || back.StateDigest != forkJoinStateDigest {
		t.Fatalf("the plan after the resume digests to %s, want %s", back.StateDigest, first.StateDigest)
	}
}

// The transition table: what each change may do to a node that is active, paused, cancelled or archived. A cancelled or archived node is final.
func TestNodeLifecycleTransitions(t *testing.T) {
	type move struct {
		to string
		ok bool
	}
	starts := []struct {
		name string
		path []string
		want map[string]move
	}{
		{"active", nil, map[string]move{OpPauseNode: {LifePaused, true}, OpResumeNode: {}, OpCancelNode: {LifeCancelled, true}, OpArchiveNode: {LifeArchived, true}}},
		{"paused", []string{OpPauseNode}, map[string]move{OpPauseNode: {}, OpResumeNode: {"", true}, OpCancelNode: {LifeCancelled, true}, OpArchiveNode: {LifeArchived, true}}},
		{"resumed", []string{OpPauseNode, OpResumeNode}, map[string]move{OpPauseNode: {LifePaused, true}, OpResumeNode: {}, OpCancelNode: {LifeCancelled, true}, OpArchiveNode: {LifeArchived, true}}},
		{"cancelled", []string{OpCancelNode}, map[string]move{OpPauseNode: {}, OpResumeNode: {}, OpCancelNode: {}, OpArchiveNode: {}}},
		{"cancelled from paused", []string{OpPauseNode, OpCancelNode}, map[string]move{OpPauseNode: {}, OpResumeNode: {}, OpCancelNode: {}, OpArchiveNode: {}}},
		{"archived", []string{OpArchiveNode}, map[string]move{OpPauseNode: {}, OpResumeNode: {}, OpCancelNode: {}, OpArchiveNode: {}}},
	}
	for _, start := range starts {
		for op, want := range start.want {
			t.Run(start.name+" then "+op, func(t *testing.T) {
				r, _, _ := newRepo(t)
				mustPut(t, r, twoNodes())
				head := 1
				for _, step := range start.path {
					mustPut(t, r, revDoc("plan", fmt.Sprintf("r%d", head+1), head, lifeChange(step, "n")))
					head++
				}
				_, err := r.Put(context.Background(), decode(t, revDoc("plan", "rx", head, lifeChange(op, "n"))))
				if !want.ok {
					was := lifeNodeOf(t, r, "plan", "n")
					if p := rejected(t, err); !hasRule(p, RuleInvalidLifecycleTransition, "changes[0].op") || len(p.Violations) != 1 {
						t.Fatalf("want one invalid_lifecycle_transition, got %v", p.Violations)
					}
					if got := lifeNodeOf(t, r, "plan", "n"); got != was {
						t.Fatalf("a refused change moved the node from %q to %q", was, got)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := lifeNodeOf(t, r, "plan", "n"); got != want.to {
					t.Fatalf("the node is %q, want %q", got, want.to)
				}
				if got := lifeNodeOf(t, r, "plan", "m"); got != "" {
					t.Fatalf("the change named n and node m is %q", got)
				}
			})
		}
	}
}

func TestPlanLifecycleTransitions(t *testing.T) {
	r, _, _ := newRepo(t)
	mustPut(t, r, twoNodes())
	if p := rejected(t, putErr(t, r, revDoc("plan", "rx", 1, lifeChange(OpResumePlan, "")))); !hasRule(p, RuleInvalidLifecycleTransition, "changes[0].op") {
		t.Fatalf("resuming an active plan: %v", p.Violations)
	}
	mustPut(t, r, revDoc("plan", "r2", 1, lifeChange(OpPausePlan, "")))
	if p := rejected(t, putErr(t, r, revDoc("plan", "ry", 2, lifeChange(OpPausePlan, "")))); !hasRule(p, RuleInvalidLifecycleTransition, "changes[0].op") {
		t.Fatalf("pausing a paused plan: %v", p.Violations)
	}
	// the plan's state is its own: pausing a node of a paused plan and cancelling it are the node's changes
	mustPut(t, r, revDoc("plan", "r3", 2, lifeChange(OpCancelNode, "n")))
	snap, _, err := r.Snapshot(context.Background(), "plan", 0)
	if err != nil || snap.PlanState != LifePaused || lifeNodeOf(t, r, "plan", "n") != LifeCancelled {
		t.Fatalf("snapshot = %+v %v", snap, err)
	}
	mustPut(t, r, revDoc("plan", "r4", 3, lifeChange(OpResumePlan, "")))
	if snap, _, _ = r.Snapshot(context.Background(), "plan", 0); snap.PlanState != "" || lifeNodeOf(t, r, "plan", "n") != LifeCancelled {
		t.Fatalf("after the plan resumed: %+v", snap)
	}
	if old, _, err := r.Snapshot(context.Background(), "plan", 2); err != nil || old.PlanState != LifePaused || lifeNodeOf(t, r, "plan", "m") != "" {
		t.Fatalf("revision 2 reads back as %+v %v", old, err)
	}
}

func putErr(t *testing.T, r *Repo, d doc) error {
	t.Helper()
	_, err := r.Put(context.Background(), decode(t, d))
	return err
}

// What a lifecycle change may share a revision with, and what it may address.
func TestLifecycleChangesAreJudgedLikeEveryOtherChange(t *testing.T) {
	for _, c := range []struct {
		name    string
		changes []doc
		rule    string
		path    string
	}{
		{"a node that is not in the plan", []doc{lifeChange(OpPauseNode, "ghost")}, RuleUnknownNode, "changes[0].node_id"},
		{"a node added by the same revision", []doc{addNode("c", NodeNonPR), lifeChange(OpPauseNode, "c")}, RuleUnknownNode, "changes[1].node_id"},
		{"two lifecycle changes of one node", []doc{lifeChange(OpPauseNode, "n"), lifeChange(OpCancelNode, "n")}, RuleConflictingChanges, "changes[1].node_id"},
		{"a lifecycle change and an update of one node", []doc{{"op": OpUpdateNode, "node": nodeDoc("n", NodeNonPR)}, lifeChange(OpPauseNode, "n")}, RuleConflictingChanges, "changes[1].node_id"},
		{"a lifecycle change and the retirement of one node", []doc{lifeChange(OpPauseNode, "n"), {"op": OpRetireNode, "node_id": "n"}}, RuleConflictingChanges, "changes[1].node_id"},
		{"two changes of the plan's lifecycle", []doc{lifeChange(OpPausePlan, ""), lifeChange(OpResumePlan, "")}, RuleConflictingChanges, "changes[1].op"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, s, _ := newRepo(t)
			mustPut(t, r, twoNodes())
			before := zoneRows(t, s.DB)
			p := rejected(t, putErr(t, r, revDoc("plan", "rx", 1, c.changes...)))
			if !hasRule(p, c.rule, c.path) {
				t.Fatalf("want %s at %s, got %v", c.rule, c.path, p.Violations)
			}
			if fmt.Sprint(before) != fmt.Sprint(zoneRows(t, s.DB)) {
				t.Fatal("a refused revision wrote rows")
			}
		})
	}
}

// The lifecycle belongs to the node id: editing a node does not reset it, and a node that leaves the plan takes it along; its replacement is a new
// node, active.
func TestLifecycleFollowsTheNodeIDAcrossEditsAndReplacement(t *testing.T) {
	r, _, _ := newRepo(t)
	mustPut(t, r, twoNodes())
	mustPut(t, r, revDoc("plan", "r2", 1, lifeChange(OpPauseNode, "n"), lifeChange(OpCancelNode, "m")))
	edited := nodeDoc("n", NodeNonPR)
	edited["title"] = "n, edited"
	res := mustPut(t, r, revDoc("plan", "r3", 2, doc{"op": OpUpdateNode, "node": edited}))
	if got := lifeNodeOf(t, r, "plan", "n"); got != LifePaused {
		t.Fatalf("an edit of a paused node left it %q", got)
	}
	if res.NodeDigests["n"] == "" {
		t.Fatal("the edited node has no digest")
	}
	mustPut(t, r, revDoc("plan", "r4", 3, doc{"op": OpReplaceNode, "node": nodeDoc("m2", NodeNonPR), "supersedes_node_id": "m"}))
	snap, _, err := r.Snapshot(context.Background(), "plan", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range snap.Nodes {
		if n.NodeID == "m" {
			t.Fatal("the replaced node is still in the plan")
		}
		if n.NodeID == "m2" && n.Lifecycle != "" {
			t.Fatalf("the replacement of a cancelled node starts %q, want active", n.Lifecycle)
		}
	}
	// retiring a paused node leaves a plan that verifies, and the retired id stays retired
	mustPut(t, r, revDoc("plan", "r5", 4, doc{"op": OpRetireNode, "node_id": "n"}))
	if err := r.VerifyLog(context.Background(), "plan"); err != nil {
		t.Fatal(err)
	}
	if p := rejected(t, putErr(t, r, revDoc("plan", "r6", 5, lifeChange(OpResumeNode, "n")))); !hasRule(p, RuleUnknownNode, "changes[0].node_id") {
		t.Fatalf("a lifecycle change of a retired node: %v", p.Violations)
	}
}

// The log replays to the plan with its lifecycle, from the empty plan and from any snapshot, and the plan as of a revision is the fold up to it.
func TestReplayCarriesTheLifecycle(t *testing.T) {
	r, _, _ := newRepo(t)
	mustPut(t, r, forkJoin("plan", "r1"))
	for i, ops := range [][]doc{
		{lifeChange(OpPauseNode, "design")}, {lifeChange(OpPausePlan, "")}, {lifeChange(OpCancelNode, "impl-a"), lifeChange(OpResumeNode, "design")},
		{lifeChange(OpResumePlan, ""), lifeChange(OpArchiveNode, "ship")},
	} {
		mustPut(t, r, revDoc("plan", fmt.Sprintf("l%d", i), i+1, ops...))
	}
	head, _, err := r.Snapshot(context.Background(), "plan", 0)
	if err != nil {
		t.Fatal(err)
	}
	if head.PlanState != "" || lifeNodeOf(t, r, "plan", "design") != "" || lifeNodeOf(t, r, "plan", "impl-a") != LifeCancelled || lifeNodeOf(t, r, "plan", "ship") != LifeArchived {
		t.Fatalf("head = %+v", head)
	}
	if err := r.VerifyLog(context.Background(), "plan"); err != nil {
		t.Fatal(err)
	}
	events := allEvents(t, r, 0)
	empty := Snapshot{PlanID: "plan", ProjectKey: "P-TEST"}
	if got, err := Replay(empty, events); err != nil || !reflect.DeepEqual(got, head) {
		t.Fatalf("replay from the empty plan = %v\n got  %+v\n head %+v", err, got, head)
	}
	for cursor := 1; cursor <= len(events); cursor++ {
		base, _, err := r.Snapshot(context.Background(), "plan", int64(cursor))
		if err != nil {
			t.Fatal(err)
		}
		got, err := Replay(base, events[cursor:])
		if err != nil || !reflect.DeepEqual(got, head) {
			t.Fatalf("replay from the snapshot at revision %d = %v\n got  %+v\n head %+v", cursor, err, got, head)
		}
	}
}

// The lifecycle is read back from the log and the digest each revision recorded covers it: a log whose lifecycle changes were edited does not read as a plan.
func TestATamperedLifecycleChangeIsTheHostsFailure(t *testing.T) {
	r, s, _ := newRepo(t)
	mustPut(t, r, forkJoin("plan", "r1"))
	mustPut(t, r, revDoc("plan", "r2", 1, lifeChange(OpPauseNode, "design")))
	for _, statement := range []string{"DROP TRIGGER dag_plan_revisions_no_update",
		"UPDATE dag_plan_revisions SET change_json = '[{\"op\":\"pause_node\",\"node_id\":\"ship\"}]' WHERE revision_no = 2"} {
		if _, err := s.DB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := r.Snapshot(context.Background(), "plan", 0)
	var corrupt *CorruptError
	if !errors.As(err, &corrupt) {
		t.Fatalf("a plan whose pause was moved to another node read as %v", err)
	}
}

// What a reader accepts of a stored lifecycle change is what the writer would have accepted: a change that is not a legal transition, or that names a node the plan never
// held, is the host's failure even when the digest of the revision is still the one it recorded (a doubled pause leaves the plan as the first pause did).
func TestAStoredLifecycleChangeThatTheWriterWouldRefuseIsTheHostsFailure(t *testing.T) {
	for name, changeJSON := range map[string]string{
		"a doubled pause":            "[{\"op\":\"pause_node\",\"node_id\":\"design\"},{\"op\":\"pause_node\",\"node_id\":\"design\"}]",
		"a node the plan never held": "[{\"op\":\"pause_node\",\"node_id\":\"design\"},{\"op\":\"cancel_node\",\"node_id\":\"ghost\"}]",
	} {
		t.Run(name, func(t *testing.T) {
			r, s, _ := newRepo(t)
			mustPut(t, r, forkJoin("plan", "r1"))
			mustPut(t, r, revDoc("plan", "r2", 1, lifeChange(OpPauseNode, "design")))
			for _, statement := range []string{"DROP TRIGGER dag_plan_revisions_no_update",
				"UPDATE dag_plan_revisions SET change_json = '" + changeJSON + "' WHERE revision_no = 2"} {
				if _, err := s.DB.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			var corrupt *CorruptError
			if _, _, err := r.Snapshot(context.Background(), "plan", 0); !errors.As(err, &corrupt) {
				t.Fatalf("a plan whose log holds %s read as %v", name, err)
			}
		})
	}
}

// A Go caller of Put cannot store what a document would be refused for.
func TestACheckedLifecycleChangeNeedsItsNode(t *testing.T) {
	r, _, _ := newRepo(t)
	mustPut(t, r, twoNodes())
	_, err := r.Put(context.Background(), Revision{PlanID: "plan", ProjectKey: "P-TEST", RequestID: "rx", ExpectedParent: 1, AuthorTaskID: "task-test",
		Changes: []Change{{Op: OpPauseNode}}})
	if p := rejected(t, err); !hasRule(p, RuleEmptyValue, "changes[0].node_id") {
		t.Fatalf("a pause without a node: %v", p.Violations)
	}
}

// The document reader is strict about the new changes as it is about the others.
func TestLifecycleChangesAreReadStrictly(t *testing.T) {
	for name, c := range map[string]struct {
		change doc
		rule   string
	}{
		"a node change without its node":    {doc{"op": OpPauseNode}, RuleMissingField},
		"a node change with an empty node":  {doc{"op": OpCancelNode, "node_id": ""}, RuleEmptyValue},
		"a node change with a bad node id":  {doc{"op": OpArchiveNode, "node_id": "not an id"}, RuleBadIdentifier},
		"a node change with a reason":       {doc{"op": OpPauseNode, "node_id": "n", "reason": "x"}, RuleUnknownField},
		"a plan change that names a node":   {doc{"op": OpPausePlan, "node_id": "n"}, RuleUnknownField},
		"a plan change with a stray field":  {doc{"op": OpResumePlan, "plan_id": "plan"}, RuleUnknownField},
		"an op that is none of the changes": {doc{"op": "freeze_node", "node_id": "n"}, RuleUnknownOp},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeRevision(raw(t, revDoc("plan", "r", 1, c.change)))
			if p := rejected(t, err); !hasRule(p, c.rule, "changes[0]") {
				t.Fatalf("want %s, got %v", c.rule, p.Violations)
			}
		})
	}
	if _, err := DecodeRevision(raw(t, revDoc("plan", "r", 1, doc{"op": OpPausePlan}))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(allOps), OpArchiveNode) {
		t.Fatal("the reader does not know archive_node")
	}
}
