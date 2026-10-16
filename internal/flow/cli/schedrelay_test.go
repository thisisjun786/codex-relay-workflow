package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// labRelay of a run: a SchedRelay over the lab's store for a run that pinned the handoff bytes raw at the plan's revision rev, with the
// run's journal (opened over the store) and the lab's verifications, as SchedulerLane builds it.
func (l *laneLab) relayFor(run string, raw []byte, rev int64) *SchedRelay {
	l.t.Helper()
	ref, err := PinHandoff(l.w.state, run, raw)
	if err != nil {
		l.t.Fatal(err)
	}
	j, err := journal.Open(context.Background(), l.w.st, run, journal.Identity{PlanID: l.plan, PlanRevision: rev, ActorTaskID: labOwner, EpochNonce: run}, journal.Options{Immediate: true})
	if err != nil {
		l.t.Fatal(err)
	}
	return &SchedRelay{Sched: &dagsched.Scheduler{Store: l.w.st, Tips: mergeturn.TargetReader{}, Ancestry: dagsched.GitAncestry{}.Ancestry}, RunID: run, Actor: labOwner, Nonce: run,
		Revision: rev, Handoff: ref, State: l.w.state, Repo: l.repo, CreatedAt: "2026-10-12T09:00:00Z", Journal: runJournal{j}, Verify: l.verifyMerged,
		Rule:     dagsched.RuleVersion{SkillsDigest: "sha256:lab-skills", Model: "claude-fake-haiku", Effort: "xhigh", PromptTemplate: "script:test", RelayBuild: "test"},
		Verifier: dagsched.VerifierRule{SkillsDigest: "sha256:lab-skills", Model: "gpt-fake-sol", Effort: "high"}}
}

// handoffBytes is the lab's handoff as it was compiled, edited by mutate.
func (l *laneLab) handoffBytes(mutate func(*compile.Input)) []byte {
	l.t.Helper()
	in := l.input
	raw, _ := json.Marshal(in)
	var copied compile.Input
	if err := json.Unmarshal(raw, &copied); err != nil {
		l.t.Fatal(err)
	}
	if mutate != nil {
		mutate(&copied)
	}
	out, err := json.Marshal(copied)
	if err != nil {
		l.t.Fatal(err)
	}
	return out
}

func (l *laneLab) snapshot(rev int64) dag.Snapshot {
	l.t.Helper()
	snap, _, err := (&dag.Repo{Store: l.w.st}).Snapshot(context.Background(), l.plan, rev)
	if err != nil {
		l.t.Fatal(err)
	}
	return snap
}

func nodeIDs(ms ops.Milestone) []string {
	var out []string
	for _, n := range ms.Nodes {
		out = append(out, n.ID)
	}
	return out
}

// TestSchedRelayMilestoneFromHandoffAndSnapshot (CRW-1277 c): the milestone of a run is the nodes of the plan at the revision the run
// pinned that carry the milestone's issues in the handoff: their ids, issues and titles from the plan, the edges from the milestone's own
// nodes as the nodes they wait for, the criteria, regions and group of their issues, the node's criteria digest and slice digest, and the
// milestone's title, criteria, surfaces, grade and required commands from its entry.
func TestSchedRelayMilestoneFromHandoffAndSnapshot(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1", After: []string{"A-1"}}, labIssue{Key: "B-1", Milestone: "M2", After: []string{"A-2"}})
	raw := l.handoffBytes(func(in *compile.Input) {
		in.Projects[0].Milestones[1].Surfaces, in.Projects[0].Milestones[1].Grade, in.Projects[0].Milestones[1].RequiredCommands = []string{"logic", "web"}, "C3", []string{"make e2e"}
		in.Projects[0].Issues[1].Group = "g1"
	})
	// the group changes the plan: compile the edited handoff into revision 2 first, as crw-plan does
	edited, _ := DecodeHandoff(raw)
	res, err := compile.Compile(edited, compile.Options{Bases: map[string]dag.State{l.plan: dag.StateFromSnapshot(l.snapshot(0))}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Plans[0].Change != nil {
		if _, err := (&dag.Repo{Store: l.w.st}).Put(ctx, *res.Plans[0].Change); err != nil {
			t.Fatal(err)
		}
	}
	snap := l.snapshot(0)
	r := l.relayFor("run-milestone01", raw, snap.Revision)
	m1, err := r.Milestone(ctx, l.plan, "M1")
	if err != nil {
		t.Fatal(err)
	}
	if got := nodeIDs(m1); !slices.Equal(got, []string{l.nodeOf["A-1"], l.nodeOf["A-2"]}) {
		t.Fatalf("the nodes of M1: %v", got)
	}
	byID := map[string]dag.SnapNode{}
	for _, n := range snap.Nodes {
		byID[n.NodeID] = n
	}
	a1, a2 := m1.Nodes[0], m1.Nodes[1]
	if a1.Issue != "A-1" || a1.Title != byID[a1.ID].Title || len(a1.After) != 0 || strings.Join(a1.Regions, ",") != "node-A-1.txt" || a1.Group != "" ||
		strings.Join(a1.Criteria, "|") != "the work of A-1 is in node-file of its node" || a1.CriteriaDigest != byID[a1.ID].CriteriaSetDigest ||
		a1.CriteriaDigest != compile.CriteriaDigest(a1.Criteria) || a1.SliceDigest != byID[a1.ID].SliceDigest || a1.SliceDigest == "" {
		t.Errorf("node A-1: %+v", a1)
	}
	if strings.Join(a2.After, ",") != a1.ID || a2.Group != "g1" || a2.SliceDigest != byID[a2.ID].SliceDigest {
		t.Errorf("node A-2: %+v", a2)
	}
	if m1.ID != "M1" || m1.Plan != l.plan || m1.Title != "milestone M1" || strings.Join(m1.Criteria, "|") != "M1 is integrated" || m1.CriteriaDigest != compile.CriteriaDigest(m1.Criteria) ||
		strings.Join(m1.Surfaces, ",") != "logic" || m1.Grade != "C2" || m1.RequiredCommands == nil || len(m1.RequiredCommands) != 0 {
		t.Errorf("milestone M1: %+v", m1)
	}
	// an edge from a node of another milestone is not a node the milestone's node waits for: the relay holds it back until it landed
	m2, err := r.Milestone(ctx, l.plan, "M2")
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Nodes) != 1 || m2.Nodes[0].Issue != "B-1" || len(m2.Nodes[0].After) != 0 || strings.Join(m2.Surfaces, ",") != "logic,web" || m2.Grade != "C3" || strings.Join(m2.RequiredCommands, ",") != "make e2e" {
		t.Errorf("milestone M2: %+v", m2)
	}
	// the run reads the copy it pinned: the live handoff file changing changes nothing
	if err := os.WriteFile(l.file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := l.relayFor("run-milestone02", raw, snap.Revision).Milestone(ctx, l.plan, "M1")
	if err != nil || !slices.Equal(nodeIDs(again), nodeIDs(m1)) {
		t.Errorf("the milestone read again: %+v %v", again, err)
	}
}

// TestSchedRelayMilestoneCarriesTheEndConditionCommands (CRW-1382 d2): a node of the milestone carries the commands of its issue's done
// condition, read from the body the handoff carries (never run), so the design-stage explorer can be given them; an issue whose body the
// handoff does not carry has none.
func TestSchedRelayMilestoneCarriesTheEndConditionCommands(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1"})
	raw := l.handoffBytes(func(in *compile.Input) {
		in.Projects[0].Issues[0].Description = "## 기준\n\n* c1: the node file holds the work.\n\n## 끝 조건\n\n* `go test ./internal/x -run '^TestNodeFile$'` passes.\n"
	})
	m, err := l.relayFor("run-endcond0001", raw, 1).Milestone(ctx, l.plan, "M1")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range m.Nodes {
		switch n.Issue {
		case "A-1":
			if len(n.EndCondition) != 1 || n.EndCondition[0] != "go test ./internal/x -run '^TestNodeFile$'" {
				t.Errorf("A-1's end condition: %q", n.EndCondition)
			}
		default:
			if len(n.EndCondition) != 0 {
				t.Errorf("%s carries no body and has the end condition %q", n.Issue, n.EndCondition)
			}
		}
	}
}

// TestMilestoneCarriesPlannedRegionAttributes (CRW-1461): a node whose planned regions rename or delete a symbol, or hold a region
// exclusive, carries its whole planned list with those attributes as planned_regions, in the order of its regions and with the same
// texts, so the lane admits it by what it plans; a node whose regions are all plain edits carries no planned_regions key.
func TestMilestoneCarriesPlannedRegionAttributes(t *testing.T) {
	l := newLaneLab(t,
		labIssue{Key: "A-1", Milestone: "M1", Regions: []compile.Region{{Path: "pkg/a.go", Kind: "symbol", Key: "F", Change: "delete"}, {Path: "b.go", Kind: "file", Change: "edit"}}},
		labIssue{Key: "A-2", Milestone: "M1", Regions: []compile.Region{{Path: "c.go", Kind: "file"}, {Path: "pkg/d.go", Kind: "symbol", Key: "G", Change: "edit"}}},
		labIssue{Key: "A-3", Milestone: "M1", After: []string{"A-1", "A-2"}, Regions: []compile.Region{{Path: "e.go", Kind: "file", Exclusive: true}}})
	m, err := l.relayFor("run-plannedat01", l.handoffBytes(nil), 1).Milestone(ctx, l.plan, "M1")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"A-1": `[{"text":"pkg/a.go#F","change":"delete"},"b.go"]`,
		"A-3": `[{"text":"e.go","exclusive":true}]`,
	}
	if len(m.Nodes) != 3 {
		t.Fatalf("the nodes of M1: %+v", m.Nodes)
	}
	for _, n := range m.Nodes {
		raw, err := json.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		got, has := fields["planned_regions"]
		w, planned := want[n.Issue]
		switch {
		case !planned && has:
			t.Errorf("%s plans only edits and carries planned_regions %s", n.Issue, got)
		case planned && string(got) != w:
			t.Errorf("%s's planned_regions: %s, want %s", n.Issue, got, w)
		}
		if planned {
			texts := []string{}
			for _, rg := range n.PlannedRegions {
				texts = append(texts, rg.Text)
			}
			if !slices.Equal(texts, n.Regions) {
				t.Errorf("%s's planned texts %q are not its regions %q", n.Issue, texts, n.Regions)
			}
		}
	}
}

// TestSchedRelayMilestoneRefusesHandoffNotAtPinnedRevision (CRW-1277 c): the run reads the plan at the revision it pinned, so a handoff
// that is not the one that revision was compiled from is refused, naming what to do: an issue with no node, and criteria whose digest is
// not the node's. A plan that moved on after the run pinned it does not change what the run reads.
func TestSchedRelayMilestoneRefusesHandoffNotAtPinnedRevision(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1", After: []string{"A-1"}})
	original := l.handoffBytes(nil)
	if _, err := l.relayFor("run-pinned0001", original, 1).Milestone(ctx, l.plan, "M1"); err != nil {
		t.Fatalf("the handoff the revision was compiled from: %v", err)
	}
	n := 0
	for name, mutate := range map[string]func(*compile.Input){
		"criteria the node does not carry": func(in *compile.Input) { in.Projects[0].Issues[0].Criteria = []string{"another criterion"} },
		"an issue the plan has no node of": func(in *compile.Input) {
			in.Projects[0].Issues = append(in.Projects[0].Issues, compile.Issue{Key: "A-9", Title: "late", Criteria: []string{"c"}, Milestone: "M1"})
		},
	} {
		n++
		_, err := l.relayFor(fmt.Sprintf("run-refused%04d", n), l.handoffBytes(mutate), 1).Milestone(ctx, l.plan, "M1")
		if err == nil || !strings.Contains(err.Error(), "compile and put the handoff first") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// the plan moves on: revision 2 changes the criteria of A-1
	moved := l.handoffBytes(func(in *compile.Input) { in.Projects[0].Issues[0].Criteria = []string{"the criterion of revision 2"} })
	in, _ := DecodeHandoff(moved)
	res, err := compile.Compile(in, compile.Options{Bases: map[string]dag.State{l.plan: dag.StateFromSnapshot(l.snapshot(0))}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&dag.Repo{Store: l.w.st}).Put(ctx, *res.Plans[0].Change); err != nil {
		t.Fatal(err)
	}
	if head := l.snapshot(0).Revision; head != 2 {
		t.Fatalf("the plan is at revision %d", head)
	}
	if ms, err := l.relayFor("run-pinned0002", original, 1).Milestone(ctx, l.plan, "M1"); err != nil || len(ms.Nodes) != 2 {
		t.Errorf("a run pinned at revision 1 reads revision 1: %+v %v", ms, err)
	}
	if _, err := l.relayFor("run-pinned0003", moved, 1).Milestone(ctx, l.plan, "M1"); err == nil || !strings.Contains(err.Error(), "compile and put the handoff first") {
		t.Errorf("the handoff of revision 2 under a run pinned at revision 1: %v", err)
	}
	if ms, err := l.relayFor("run-pinned0004", moved, 2).Milestone(ctx, l.plan, "M1"); err != nil || strings.Join(ms.Nodes[0].Criteria, "") != "the criterion of revision 2" {
		t.Errorf("the handoff of revision 2 under a run pinned at revision 2: %+v %v", ms, err)
	}
	// a run that pins no revision reads nothing
	if _, err := l.relayFor("run-pinned0005", original, 0).Milestone(ctx, l.plan, "M1"); err == nil || !strings.Contains(err.Error(), "pins no revision") {
		t.Errorf("no pinned revision: %v", err)
	}
}

// TestSchedRelayMilestoneRefusesMissingSurfacesOrGrade (CRW-1277 c): a milestone the handoff has no entry of, and an entry without surfaces
// or grade, are refused: the run defaults neither. Empty required commands are the repository's CI and are allowed.
func TestSchedRelayMilestoneRefusesMissingSurfacesOrGrade(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	cases := map[string]struct {
		mutate func(*compile.Input)
		want   string
	}{
		"no entry":       {func(in *compile.Input) { in.Projects[0].Milestones = nil }, "names no entry of milestone M1"},
		"no surfaces":    {func(in *compile.Input) { in.Projects[0].Milestones[0].Surfaces = nil }, "no surfaces, grade or criteria"},
		"no grade":       {func(in *compile.Input) { in.Projects[0].Milestones[0].Grade = "" }, "no surfaces, grade or criteria"},
		"a blank grade":  {func(in *compile.Input) { in.Projects[0].Milestones[0].Grade = " " }, "no surfaces, grade or criteria"},
		"no criteria":    {func(in *compile.Input) { in.Projects[0].Milestones[0].Criteria = nil }, "no surfaces, grade or criteria"},
		"another entry":  {func(in *compile.Input) { in.Projects[0].Milestones[0].Key = "M9" }, "names no entry of milestone M1"},
		"no project":     {func(in *compile.Input) { in.Projects[0].PlanID = "plan-9" }, "holds no project of plan"},
		"commands empty": {func(in *compile.Input) { in.Projects[0].Milestones[0].RequiredCommands = nil }, ""},
	}
	i := 0
	for name, tc := range cases {
		i++
		ms, err := l.relayFor(fmt.Sprintf("run-surfaces%02d", i), l.handoffBytes(tc.mutate), 1).Milestone(ctx, l.plan, "M1")
		switch {
		case tc.want == "" && (err != nil || ms.RequiredCommands == nil || len(ms.RequiredCommands) != 0):
			t.Errorf("%s: %+v %v", name, ms, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v", name, err)
		}
	}
	// a run started without a handoff has no milestone to read
	r := l.relayFor("run-surfaces99", l.handoffBytes(nil), 1)
	r.Handoff = HandoffRef{}
	if _, err := r.Milestone(ctx, l.plan, "M1"); err == nil || !strings.Contains(err.Error(), "--handoff") {
		t.Errorf("no handoff: %v", err)
	}
}

// TestSchedRelayMilestoneOrderMatchesReady (CRW-1277 c): the nodes of a milestone come in the order the scheduler offers ready nodes
// (dagsched.Ready: the longest chain below a node first, then the most nodes below it, then the node id), which the plan's edges into
// the next milestone decide.
func TestSchedRelayMilestoneOrderMatchesReady(t *testing.T) {
	// R-1, R-2 and R-3 are ready together; below R-2 hang two nodes in a chain, below R-3 one, below R-1 none
	l := newLaneLab(t, labIssue{Key: "R-1", Milestone: "M1"}, labIssue{Key: "R-2", Milestone: "M1"}, labIssue{Key: "R-3", Milestone: "M1"},
		labIssue{Key: "S-1", Milestone: "M2", After: []string{"R-2"}}, labIssue{Key: "S-2", Milestone: "M2", After: []string{"S-1"}}, labIssue{Key: "T-1", Milestone: "M2", After: []string{"R-3"}})
	ms, err := l.relayFor("run-order00001", l.handoffBytes(nil), 1).Milestone(ctx, l.plan, "M1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{l.nodeOf["R-2"], l.nodeOf["R-3"], l.nodeOf["R-1"]}
	if got := nodeIDs(ms); !slices.Equal(got, want) {
		t.Fatalf("the milestone's order is %v, want %v", got, want)
	}
	// the scheduler's own reading ranks the ready nodes the same way
	reading, err := (&dagsched.Scheduler{Store: l.w.st}).Read(ctx, l.plan, dagsched.ReadyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var ranked []dagsched.NodeReading
	for _, n := range reading.Nodes {
		if n.Rank != nil && slices.Contains(want, n.NodeID) {
			ranked = append(ranked, n)
		}
	}
	if len(ranked) != 3 {
		t.Fatalf("the reading ranks %d of the ready nodes: %+v", len(ranked), reading.Nodes)
	}
	var readyOrder []string
	for _, n := range reading.Ready {
		readyOrder = append(readyOrder, n.NodeID)
	}
	if !slices.Equal(readyOrder, want[:len(readyOrder)]) || len(readyOrder) == 0 {
		t.Errorf("the scheduler offers %v, the milestone lists %v", readyOrder, want)
	}
	for _, n := range ranked {
		i := slices.Index(want, n.NodeID)
		for _, m := range ranked {
			j := slices.Index(want, m.NodeID)
			before := n.Rank.CriticalPath > m.Rank.CriticalPath || (n.Rank.CriticalPath == m.Rank.CriticalPath && (n.Rank.Descendants > m.Rank.Descendants ||
				(n.Rank.Descendants == m.Rank.Descendants && n.NodeID < m.NodeID)))
			if i < j && !before {
				t.Errorf("%s (rank %+v) is listed before %s (rank %+v)", n.NodeID, *n.Rank, m.NodeID, *m.Rank)
			}
		}
	}
	if got := dagsched.OfferingOrder(l.snapshot(0)); !slices.Equal(got[:3], []string{l.nodeOf["R-2"], l.nodeOf["R-3"], l.nodeOf["S-1"]}) {
		t.Errorf("the offering order of the whole plan: %v", got)
	}
}

// TestSchedRelayReleaseAgentReservesSlotAndReplays (CRW-1277 d): a release is one agent execution of the node, held by the run's owner
// under the epoch the run claimed, with the slot reserved and the manifest frozen over the pinned handoff, the node's criteria and the
// run's directory; the same release again answers the same execution and writes nothing.
func TestSchedRelayReleaseAgentReservesSlotAndReplays(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1", After: []string{"A-1"}})
	r := l.relayFor("run-release001", l.handoffBytes(nil), 1)
	node := l.nodeOf["A-1"]
	res, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-1"})
	if err != nil || !res.Released || res.Replayed || res.ExecutionID == "" || res.SlotID == "" {
		t.Fatalf("release: %+v %v", res, err)
	}
	var actor, run, worktree, manifest, kind string
	var epoch int64
	if err := l.w.st.DB.QueryRow("SELECT actor_task_id, run_id, worktree, manifest_digest, kind, coordinator_epoch FROM dag_agent_executions WHERE execution_id = ?", res.ExecutionID).
		Scan(&actor, &run, &worktree, &manifest, &kind, &epoch); err != nil {
		t.Fatal(err)
	}
	if actor != labOwner || run != r.RunID || worktree != r.worktreeHandle(node) || kind != dagsched.AgentInitial || epoch != 1 {
		t.Errorf("the execution: actor %s run %s worktree %s kind %s epoch %d", actor, run, worktree, kind, epoch)
	}
	if n := l.count("SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node' AND subject_key = ? AND state = 'held'", l.plan+"/"+node); n != 1 {
		t.Errorf("%d held slots", n)
	}
	body, found, err := (&dag.Repo{Store: l.w.st}).ReadManifest(ctx, manifest)
	if err != nil || !found {
		t.Fatalf("the manifest: %v %v", found, err)
	}
	doc, _ := json.Marshal(body)
	// the pinned handoff is the volatile input, retained by the relay under its sha256
	for _, want := range []string{`"source":"crw-flow-compile-input/1"`, `"sha256":"` + r.Handoff.SHA256 + `"`, `"model":"claude-fake-haiku"`, `"skills_digest":"sha256:lab-skills"`, `"repository":"` + l.repo + `"`, `"ref":"integration"`} {
		if !strings.Contains(string(doc), want) {
			t.Errorf("the manifest does not name %q: %s", want, doc)
		}
	}
	before := l.count("SELECT COUNT(*) FROM dag_agent_executions") + l.count("SELECT COUNT(*) FROM execution_slots") + l.count("SELECT COUNT(*) FROM dag_input_manifests")
	again, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-1"})
	if err != nil || !again.Released || !again.Replayed || again.ExecutionID != res.ExecutionID || again.SlotID != res.SlotID {
		t.Fatalf("the release again: %+v %v", again, err)
	}
	// a new process of the run (a resume) is the same session: the claim is answered again and the release replayed
	resumed := l.relayFor("run-release001-b", l.handoffBytes(nil), 1)
	resumed.RunID, resumed.Nonce, resumed.Handoff = r.RunID, r.Nonce, r.Handoff
	if third, err := resumed.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-1"}); err != nil || !third.Replayed || third.ExecutionID != res.ExecutionID {
		t.Fatalf("the release of the resumed process: %+v %v", third, err)
	}
	if after := l.count("SELECT COUNT(*) FROM dag_agent_executions") + l.count("SELECT COUNT(*) FROM execution_slots") + l.count("SELECT COUNT(*) FROM dag_input_manifests"); after != before {
		t.Errorf("a replayed release wrote %d rows", after-before)
	}
	if stands, err := r.ReleaseStands(ctx, l.plan, node, res.ExecutionID); err != nil || !stands {
		t.Errorf("the release does not stand: %v", err)
	}
	if stands, _ := r.ReleaseStands(ctx, l.plan, node, "dag-another"); stands {
		t.Error("another execution stands")
	}
}

// TestSchedRelayReleaseComparesTheExpectedSlice (CRW-1288): the release carries the slice the flow compared to the relay, which
// releases nothing when the plan gives the node another (Released false, plan_changed, no execution), and a release answers the
// slice of the manifest the relay committed.
func TestSchedRelayReleaseComparesTheExpectedSlice(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	r := l.relayFor("run-slice00001", l.handoffBytes(nil), 1)
	node, slice := l.nodeOf["A-1"], ""
	for _, n := range l.snapshot(0).Nodes {
		if n.NodeID == node {
			slice = n.SliceDigest
		}
	}
	other, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-1", SliceDigest: "sha256:another"})
	if err != nil || other.Released || other.Reason != ops.ReasonPlanChanged {
		t.Fatalf("a release under another slice: %+v %v", other, err)
	}
	if n := l.count("SELECT COUNT(*) FROM dag_agent_executions") + l.count("SELECT COUNT(*) FROM dag_input_manifests"); n != 0 {
		t.Fatalf("the refused release wrote %d rows", n)
	}
	res, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-1", SliceDigest: slice})
	if err != nil || !res.Released || slice == "" || res.SliceDigest != slice {
		t.Fatalf("the release under slice %s: %+v %v", slice, res, err)
	}
}

// TestSchedRelayReplayUnderAnotherSliceIsNotReleased (CRW-1288): the relay committed a node's release under its slice and the run
// stopped before the journal row; a plan revision then gave the node another slice, and the run asks again with that slice. The
// execution the relay replays was built under the old slice, so the answer is not a release under the new one (Released false,
// plan_changed, no second execution); asked again with the slice it was committed under, the release is the idempotent replay.
func TestSchedRelayReplayUnderAnotherSliceIsNotReleased(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	r := l.relayFor("run-replay00001", l.handoffBytes(nil), 1)
	node := l.nodeOf["A-1"]
	sliceAt := func() string {
		for _, n := range l.snapshot(0).Nodes {
			if n.NodeID == node {
				return n.SliceDigest
			}
		}
		return ""
	}
	committed := sliceAt()
	first, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-1", SliceDigest: committed})
	if err != nil || !first.Released || first.SliceDigest != committed {
		t.Fatalf("the release under slice %s: %+v %v", committed, first, err)
	}
	in, _ := DecodeHandoff(l.handoffBytes(func(in *compile.Input) {
		in.Projects[0].Issues[0].Title = "issue A-1, retitled by a plan revision"
	}))
	res, err := compile.Compile(in, compile.Options{Bases: map[string]dag.State{l.plan: dag.StateFromSnapshot(l.snapshot(0))}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Plans[0].Change == nil {
		t.Fatal("the retitle compiled to no plan revision")
	}
	change := *res.Plans[0].Change
	change.CoordinatorEpoch = r.Sched.ExpectedEpoch
	if _, err := (&dag.Repo{Store: l.w.st}).Put(ctx, change); err != nil {
		t.Fatal(err)
	}
	now := sliceAt()
	if now == committed {
		t.Fatal("the plan revision did not change the node's slice")
	}
	again, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-1", SliceDigest: now})
	if err != nil || again.Released || again.Reason != ops.ReasonPlanChanged {
		t.Fatalf("the release asked under the new slice %s replays the one committed under %s: %+v %v, want Released false, plan_changed", now, committed, again, err)
	}
	if n := l.count("SELECT COUNT(*) FROM dag_agent_executions"); n != 1 {
		t.Fatalf("%d executions", n)
	}
	replay, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-1", SliceDigest: committed})
	if err != nil || !replay.Released || !replay.Replayed || replay.ExecutionID != first.ExecutionID || replay.SliceDigest != committed {
		t.Fatalf("the release asked again under the committed slice: %+v %v, want the replay of %s", replay, err, first.ExecutionID)
	}
}

// TestSchedRelayHeldBackReleaseIsNotAnError (CRW-1277 d): a node the scheduler holds back is Released false with the reason and no error
// (a node waiting for the node it comes after, a node whose region overlaps a running node's), so the lane asks again later; a request the
// relay refuses for what it is (a node the plan does not hold) is an error.
func TestSchedRelayHeldBackReleaseIsNotAnError(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1", After: []string{"A-1"}}, labIssue{Key: "C-1", Milestone: "M1"})
	r := l.relayFor("run-held000001", l.handoffBytes(nil), 1)
	waiting, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: l.nodeOf["A-2"], RunID: r.RunID, RequestID: "req-a2"})
	if err != nil || waiting.Released || !strings.HasPrefix(waiting.Reason, "wait:") {
		t.Fatalf("a node waiting for its predecessor: %+v %v", waiting, err)
	}
	if first, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: l.nodeOf["A-1"], RunID: r.RunID, RequestID: "req-a1", Regions: []string{"node-A-1.txt"}}); err != nil || !first.Released {
		t.Fatalf("the first node: %+v %v", first, err)
	}
	// C-1's admission regions name the file the running A-1 holds, so it overlaps it
	overlap, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: l.nodeOf["C-1"], RunID: r.RunID, RequestID: "req-c1", Regions: []string{"node-A-1.txt"}})
	if err != nil || overlap.Released || overlap.Reason == "" {
		t.Fatalf("a node whose region overlaps a running one: %+v %v", overlap, err)
	}
	if n := l.count("SELECT COUNT(*) FROM dag_agent_executions WHERE node_id IN (?, ?)", l.nodeOf["A-2"], l.nodeOf["C-1"]); n != 0 {
		t.Errorf("%d executions of held-back nodes", n)
	}
	t.Logf("held back: %q and %q", waiting.Reason, overlap.Reason)
	if _, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: "no-such-node", RunID: r.RunID}); err == nil {
		t.Error("the release of a node the plan does not hold is no error")
	}
	other := l.relayFor("run-held000002", l.handoffBytes(nil), 1)
	other.Repo = t.TempDir()
	if _, err := other.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: l.nodeOf["C-1"], RunID: other.RunID}); err == nil || !strings.Contains(err.Error(), "is not the repository") {
		t.Errorf("a target that is not the run's repository: %v", err)
	}
	if reason, held := holdBack(errors.New("a failure")); held || reason != "" {
		t.Errorf("a plain error is a hold-back: %q", reason)
	}
}

// TestRunClaimsPlanEpochAndStopsWhenLost (CRW-1277 e): before its first fenced write the run claims the plan's coordinator epoch under its
// owner thread with its own nonce, and every write after carries the epoch; a resumed process of the run is the same claim. Once another
// session claims the plan the run's next fenced write is refused, writes nothing, and the run stops with the reason in its result.
func TestRunClaimsPlanEpochAndStopsWhenLost(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1", After: []string{"A-1"}})
	r := l.relayFor("run-epoch00001", l.handoffBytes(nil), 1)
	if n := l.count("SELECT COUNT(*) FROM dag_coordinator_claims"); n != 0 {
		t.Fatalf("%d claims before the first write", n)
	}
	if _, err := r.Milestone(ctx, l.plan, "M1"); err != nil {
		t.Fatal(err)
	}
	if n := l.count("SELECT COUNT(*) FROM dag_coordinator_claims"); n != 0 {
		t.Fatalf("reading the milestone claimed the plan")
	}
	if res, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: l.nodeOf["A-1"], RunID: r.RunID, RequestID: "r1"}); err != nil || !res.Released {
		t.Fatalf("release: %+v %v", res, err)
	}
	if n := l.count("SELECT COUNT(*) FROM dag_coordinator_claims WHERE plan_id = ? AND epoch = 1 AND task_id = ? AND session_nonce = ?", l.plan, labOwner, r.Nonce); n != 1 || r.Sched.ExpectedEpoch != 1 {
		t.Fatalf("the claim: %d rows, expected epoch %d", n, r.Sched.ExpectedEpoch)
	}
	// another session of the owner claims the plan
	if _, err := (&dagsched.Scheduler{Store: l.w.st}).ClaimEpoch(ctx, l.plan, dagsched.ClaimInput{Actor: labOwner, SessionNonce: "session-2"}); err != nil {
		t.Fatal(err)
	}
	before := l.count("SELECT COUNT(*) FROM dag_agent_executions") + l.count("SELECT COUNT(*) FROM dag_agent_closures") + l.count("SELECT COUNT(*) FROM execution_slots WHERE state = 'held'")
	_, err := r.Close(ctx, ops.CloseRequest{Plan: l.plan, Node: l.nodeOf["A-1"], Actor: labOwner, Reason: "tiers_exhausted"})
	if !errors.Is(err, ErrEpochLost) || !strings.Contains(err.Error(), "stale_coordinator_epoch") {
		t.Fatalf("a close after the epoch was lost: %v", err)
	}
	// a resumed process of the run presents the run's nonce, which is no longer the newest claim
	resumed := l.relayFor("run-epoch00001-b", l.handoffBytes(nil), 1)
	resumed.Nonce = r.Nonce
	if _, err := resumed.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: l.nodeOf["A-1"], RunID: r.RunID}); !errors.Is(err, ErrEpochLost) {
		t.Fatalf("the resumed process after the epoch was lost: %v", err)
	}
	if after := l.count("SELECT COUNT(*) FROM dag_agent_executions") + l.count("SELECT COUNT(*) FROM dag_agent_closures") + l.count("SELECT COUNT(*) FROM execution_slots WHERE state = 'held'"); after != before {
		t.Errorf("a fenced write after the epoch was lost wrote %d rows", after-before)
	}

	// the run process: another session takes the plan right after the run released its first node; the run stops at its next fenced write
	run := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	run.after = func(r *SchedRelay, effect string) {
		if strings.HasPrefix(effect, "release:") {
			if _, err := (&dagsched.Scheduler{Store: r.Sched.Store}).ClaimEpoch(ctx, run.plan, dagsched.ClaimInput{Actor: labOwner, SessionNonce: "session-other"}); err != nil {
				t.Errorf("the other session's claim: %v", err)
			}
		}
	}
	spec := run.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", run.file)
	res := run.runToEnd(spec.RunID)
	var v laneValue
	_ = json.Unmarshal(res.Value, &v)
	if v.State == "closed" {
		t.Fatalf("the run closed its milestone after it lost the epoch: %s", res.Value)
	}
	// the lane's acceptance was refused at the fence with the reason, and the run wrote nothing at the relay after it
	recorded := false
	for _, e := range run.events() {
		raw, _ := json.Marshal(e.Data)
		if strings.Contains(string(raw), "lost the coordinator epoch") && strings.Contains(string(raw), "stale_coordinator_epoch") {
			recorded = true
		}
	}
	if !recorded {
		t.Errorf("the run's journal does not say it lost the epoch: %s", res.Value)
	}
	if n := run.count("SELECT COUNT(*) FROM dag_agent_acceptances") + run.count("SELECT COUNT(*) FROM dag_agent_closures") + run.count("SELECT COUNT(*) FROM dag_integration_batches"); n != 0 {
		t.Errorf("%d relay rows written after the epoch was lost", n)
	}
	t.Logf("the run ended %s %s with %s after the effects %v", res.Status, res.ErrorKind, res.Value, run.effects)
}

// TestSchedRelayAcceptIntegrateCloseOnTempStore (CRW-1277 f): over a temporary store and git, a milestone's nodes are accepted on the agent
// path (the relay reads the head from the node's worktree, judges the pre-merge and verification records and the review the run recorded by
// a separate reviewer call) and integrated by the dag-integrate wiring onto the target branch, which the observation marks integrated;
// the acceptance and the integration answer again as replays; AcceptState and ReleaseStands read them; a node given up is closed with its
// slot, a hold closes nothing, and a node never released has nothing to close.
func TestSchedRelayAcceptIntegrateCloseOnTempStore(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1", After: []string{"A-1"}}, labIssue{Key: "B-1", Milestone: "M2", After: []string{"A-2"}},
		labIssue{Key: "B-2", Milestone: "M2", After: []string{"B-1"}})
	spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	v := l.closed(l.runToEnd(spec.RunID))
	l.effectsOnce("A-1", "A-2")
	if v.Tip != l.git(l.repo, "rev-parse", "integration") {
		t.Errorf("the milestone closed over %s", v.Tip)
	}
	if l.verifications != 2 {
		t.Errorf("%d merged trees were verified", l.verifications)
	}
	// what the relay holds of each acceptance: the head read from git, the unit checker of another family (CRW-1465), the records
	for _, key := range []string{"A-1", "A-2"} {
		var head, base, repo, reviewer, premerge, verification, by string
		if err := l.w.st.DB.QueryRow("SELECT head, base, repository, reviewer_model, premerge_json, verification_json, accepted_by_task_id FROM dag_agent_acceptances WHERE node_id = ?", l.nodeOf[key]).
			Scan(&head, &base, &repo, &reviewer, &premerge, &verification, &by); err != nil {
			t.Fatal(err)
		}
		if repo != l.repo || reviewer != labCheckModel || by != labOwner || !strings.Contains(premerge, head) || !strings.Contains(verification, head) {
			t.Errorf("the acceptance of %s: head %s repo %s reviewer %s by %s", key, head, repo, reviewer, by)
		}
		if out, err := exec.Command("git", "-C", l.repo, "merge-base", "--is-ancestor", head, "integration").CombinedOutput(); err != nil {
			t.Errorf("the head of %s is not on the target branch: %s", key, out)
		}
	}
	if n := l.count("SELECT COUNT(*) FROM dag_integration_observations"); n < 2 {
		t.Errorf("%d observations of the landings", n)
	}

	// the run's relay, as a resumed process of the run builds it: the acceptance and the integration answer again
	j, err := journal.Open(ctx, l.w.st, spec.RunID, journal.Identity{}, journal.Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := HandoffOf(spec.Args)
	r := &SchedRelay{Sched: &dagsched.Scheduler{Store: l.w.st, Tips: mergeturn.TargetReader{}, Ancestry: dagsched.GitAncestry{}.Ancestry}, RunID: spec.RunID, Actor: labOwner, Nonce: spec.RunID,
		Revision: 1, Handoff: ref, State: l.w.state, CreatedAt: spec.CreatedAt, Journal: runJournal{j}, Verify: l.verifyMerged,
		Rule: dagsched.RuleVersion{SkillsDigest: "s", Model: "m", Effort: "e", PromptTemplate: "p", RelayBuild: "b"}, Verifier: dagsched.VerifierRule{SkillsDigest: "s", Model: "gpt-fake-sol", Effort: "high"}}
	var acceptRow, integrateRow, releaseRow engine.Event
	for _, ev := range j.Events() {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted && str(ev.Data, "node") == l.nodeOf["A-2"] {
			switch ev.Op {
			case ops.OpAccept:
				acceptRow = ev
			case ops.OpIntegrate:
				integrateRow = ev
			case ops.OpRelease:
				releaseRow = ev
			}
		}
	}
	st, found, err := r.AcceptState(ctx, str(acceptRow.Data, "acceptance_id"))
	if err != nil || !found || !st.Active || st.Stale || st.Head != str(acceptRow.Data, "head") {
		t.Errorf("the acceptance state: %+v %v %v", st, found, err)
	}
	if _, found, _ := r.AcceptState(ctx, "agent-acceptance-unknown"); found {
		t.Error("an acceptance the run never made is found")
	}
	if stands, err := r.ReleaseStands(ctx, l.plan, l.nodeOf["A-2"], str(releaseRow.Data, "execution_id")); err != nil || !stands {
		t.Errorf("the release of a node that landed does not stand: %v", err)
	}
	again, err := r.Integrate(ctx, ops.IntegrateRequest{Plan: l.plan, Node: l.nodeOf["A-2"], Actor: labOwner, RunID: spec.RunID, AcceptanceID: str(acceptRow.Data, "acceptance_id"), Head: str(acceptRow.Data, "head")})
	if err != nil || !again.Landed || !again.Replayed || again.Tip != l.git(l.repo, "rev-parse", "integration") || again.IntegrationRef != "integration" {
		t.Errorf("the integration again: %+v %v", again, err)
	}
	// an acceptance that is not the node's current one is no replay, even when the branch holds the head: its refusal stands
	if other, err := r.Integrate(ctx, ops.IntegrateRequest{Plan: l.plan, Node: l.nodeOf["A-2"], Actor: labOwner, RunID: spec.RunID, AcceptanceID: "agent-acceptance-unknown", Head: str(acceptRow.Data, "head")}); err == nil && other.Landed {
		t.Errorf("the integration of an acceptance that is not the current one landed: %+v", other)
	}
	if str(integrateRow.Data, "batch_commit") == "" {
		t.Error("the integration row names no batch commit")
	}
	if tip, err := r.IntegrationTip(ctx, l.plan); err != nil || tip != l.git(l.repo, "rev-parse", "integration") {
		t.Errorf("the integration tip: %s %v", tip, err)
	}

	// a node given up: B-1 is released, then closed with its slot; the close again is a replay; the release no longer stands
	b1 := l.nodeOf["B-1"]
	rel, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: b1, RunID: spec.RunID, RequestID: "req-b1"})
	if err != nil || !rel.Released {
		t.Fatalf("release B-1: %+v %v", rel, err)
	}
	if held, err := r.Close(ctx, ops.CloseRequest{Plan: l.plan, Node: b1, Actor: labOwner, Reason: "dispatch_unknown"}); err != nil || !held.Held || held.Closed {
		t.Errorf("a hold: %+v %v", held, err)
	}
	closed, err := r.Close(ctx, ops.CloseRequest{Plan: l.plan, Node: b1, Actor: labOwner, Reason: "tiers_exhausted"})
	if err != nil || !closed.Closed || !closed.SlotReleased || closed.ExecutionID != rel.ExecutionID || closed.State != "escalated" {
		t.Fatalf("the close: %+v %v", closed, err)
	}
	if n := l.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = ? AND state = 'held'", l.plan+"/"+b1); n != 0 {
		t.Errorf("the closed node holds %d slots", n)
	}
	if replay, err := r.Close(ctx, ops.CloseRequest{Plan: l.plan, Node: b1, Actor: labOwner, Reason: "tiers_exhausted"}); err != nil || !replay.Replayed {
		t.Errorf("the close again: %+v %v", replay, err)
	}
	if stands, _ := r.ReleaseStands(ctx, l.plan, b1, rel.ExecutionID); stands {
		t.Error("a closed execution stands")
	}
	if none, err := r.Close(ctx, ops.CloseRequest{Plan: l.plan, Node: l.nodeOf["B-2"], Actor: labOwner, Reason: "tiers_exhausted"}); err != nil || none.Closed || none.Held {
		t.Errorf("the close of a node never released: %+v %v", none, err)
	}
}

// TestSchedRelayReleaseChildRefusesUnwired (CRW-1277 g, CRW-1464): the run starts no `child` session. CRW-1285 gave the relay a managed
// child start behind host settings no start pins since CRW-1417; CRW-1464 deleted it, so the run's relay has no method that starts a
// `child` session and holds no host settings for one. A node sized for a child is an unreleased sized row of crw.release that asks the
// relay nothing (ops TestAChildSizedNodeHasStarted), and the lane escalates it as size_split.
func TestSchedRelayReleaseChildRefusesUnwired(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	r := l.relayFor("run-child00001", l.handoffBytes(nil), 1)
	typ := reflect.TypeOf(r)
	for _, name := range []string{"ReleaseChild", "Child"} {
		if _, ok := typ.MethodByName(name); ok {
			t.Errorf("the run's relay has a method %s", name)
		}
		if _, ok := typ.Elem().FieldByName(name); ok {
			t.Errorf("the run's relay has a field %s", name)
		}
	}
	if n := l.count("SELECT COUNT(*) FROM dag_agent_executions") + l.count("SELECT COUNT(*) FROM managed_start_requests") + l.count("SELECT COUNT(*) FROM dag_coordinator_claims"); n != 0 {
		t.Errorf("building the run's relay wrote %d rows", n)
	}
}

// TestShippedBinaryInstallsLaneOps (CRW-1277 h): the shipped binary installs the project lane's operations, so its `crw flow start
// project-lane` and `crw flow start night-batch` are not refused as blocked; the binary's deps report the lane operations.
func TestShippedBinaryInstallsLaneOps(t *testing.T) {
	old := OpsFor
	defer func() { OpsFor = old }()
	OpsFor = SchedulerLane
	if !Default().LaneOps() {
		t.Error("Default().LaneOps() is false with the shipped lane installed")
	}
	w := newWorld(t)
	nightDecision(t, w, "ud-night", batchJSON(t, nil), NightRun)
	w.close()
	crw := testsupport.CRW(t)
	for name, argv := range map[string][]string{
		"project lane": {"start", "project-lane", "--preview", "--json", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", defaultHandoff(t)},
		"night batch":  {"start", "night-batch", "--preview", "--json", "--decision", "ud-night"},
	} {
		cmd := exec.Command(crw, append([]string{"flow"}, append(argv, "--thread", labOwner, "--state", w.state)...)...)
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		_ = cmd.Run() // a preview whose preflight facts fail exits 1; the answer says why
		var ans StartAnswer
		if err := json.Unmarshal(out.Bytes(), &ans); err != nil {
			t.Fatalf("%s: %v\n%s%s", name, err, out.String(), errOut.String())
		}
		if ans.Plan.Blocked != "" || strings.Contains(out.String()+errOut.String(), "installs no") || ans.Plan.Workflow == "" {
			t.Errorf("%s: the shipped binary blocks the run: %q\n%s", name, ans.Plan.Blocked, errOut.String())
		}
	}
}

// TestNightBatchStartsFromDecisionOnTempStore (CRW-1277, F12's night batch on the real world): `crw flow start night-batch --decision D`
// over a temporary store and git with the fake host runs the production World with the lane operations of the shipped binary: the nodes
// the relay releases first are exactly the decision's issues, and the batch closes its milestone with every issue integrated.
func TestNightBatchStartsFromDecisionOnTempStore(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "CRW-11", Milestone: "M1"}, labIssue{Key: "CRW-12", Milestone: "M1", After: []string{"CRW-11"}}, labIssue{Key: "CRW-21", Milestone: "M2", After: []string{"CRW-12"}})
	nightDecision(t, l.w, "ud-night", batchJSON(t, func(b *NightBatch) { b.Issues, b.Handoff = []string{"CRW-11", "CRW-12"}, refOf(t, l.file) }), NightRun)
	spec := l.start("start", "night-batch", "--decision", "ud-night")
	if spec.TokenBudget != 750000 || spec.Decision != "ud-night" {
		t.Errorf("the spec: %+v", spec)
	}
	res := l.runToEnd(spec.RunID)
	var v struct {
		State    string            `json:"state"`
		Selected []string          `json:"selected"`
		Nodes    map[string]string `json:"nodes"`
	}
	if res.Status != engine.RunCompleted || json.Unmarshal(res.Value, &v) != nil || v.State != "closed" {
		t.Fatalf("the night batch: %s %s %s\n%s", res.Status, res.ErrorKind, res.Error, res.Value)
	}
	var released []string
	for _, e := range l.effects {
		if node, ok := strings.CutPrefix(e, "release:"); ok {
			released = append(released, l.issue[node])
		}
	}
	if !slices.Equal(released, []string{"CRW-11", "CRW-12"}) {
		t.Errorf("the relay released %v, the decision selected CRW-11 and CRW-12", released)
	}
	rows, err := l.w.st.DB.Query("SELECT node_id FROM dag_agent_executions ORDER BY decided_at, rowid")
	if err != nil {
		t.Fatal(err)
	}
	var executed []string
	for rows.Next() {
		var node string
		if err := rows.Scan(&node); err != nil {
			t.Fatal(err)
		}
		executed = append(executed, l.issue[node])
	}
	_ = rows.Close()
	if !slices.Equal(executed, []string{"CRW-11", "CRW-12"}) {
		t.Errorf("the executions are %v", executed)
	}
	l.effectsOnce("CRW-11", "CRW-12")
	if n := l.count("SELECT COUNT(*) FROM dag_agent_executions WHERE node_id = ?", l.nodeOf["CRW-21"]); n != 0 {
		t.Errorf("a node outside the decision was released")
	}
	if _, err := os.Stat(filepath.Join(RunDir(l.w.state, spec.RunID), HandoffFile)); err != nil {
		t.Errorf("the run pinned no handoff: %v", err)
	}
}

// commitHook is an engine.UpwardHook that counts the batches the engine was told of.
type commitHook struct {
	mu      sync.Mutex
	batches int
}

func (h *commitHook) RunStarted(context.Context, string) error { return nil }
func (h *commitHook) BatchCommitted(context.Context, string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.batches++
	return nil
}
func (h *commitHook) RunFinished(context.Context, string, string) error { return nil }

// TestRunJournalForwardsTheCommitNotice (CRW-1277 h): the run's journal under the lane's wrapper still tells the engine when it committed a
// batch (engine.CommitNotifier), so UpwardHook.BatchCommitted (CRW-1204: the plan summary and the escalations sent while the run lives)
// is called in a run of the shipped binary as it is without the lane operations; the notice is removed with SetOnCommit(nil).
func TestRunJournalForwardsTheCommitNotice(t *testing.T) {
	w := newWorld(t)
	j, err := journal.Open(context.Background(), w.st, "run-notice", journal.Identity{ActorTaskID: labOwner}, journal.Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	var wrapped engine.Journal = runJournal{j}
	wrapped.Append(engine.Event{RunID: "run-notice", Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "t", "script_digest": "d", "args_digest": "a"}})
	n, ok := wrapped.(engine.CommitNotifier)
	if !ok {
		t.Fatal("runJournal hides engine.CommitNotifier")
	}
	fired := 0
	n.SetOnCommit(func() { fired++ })
	// an intent row (a prepared call) is committed at once
	wrapped.Append(engine.Event{RunID: "run-notice", Kind: engine.KindCall, Op: "demo", CallID: "c1", Event: engine.EvPrepared})
	if fired == 0 {
		t.Errorf("the commit of a row was not told to the notice installed through the wrapper (journal: %v)", j.Err())
	}
	n.SetOnCommit(nil)
	before := fired
	wrapped.Append(engine.Event{RunID: "run-notice", Kind: engine.KindCall, Op: "demo", CallID: "c2", Event: engine.EvPrepared})
	if fired != before {
		t.Error("the notice fired after SetOnCommit(nil)")
	}
}

// TestRunJournalNamesTheCriteriaOfAReviewerCall: the completed row of a reviewer call is stored with the criteria digest its input
// snapshot named on the prepared row (reviewerCriteria), which the relay's agent acceptance reads. The role key reviewer-final is retired
// (CRW-1450), and the call an old run made under it still reads as a reviewer's: its completed row names its criteria as the reviewer's
// does. A call of another role, and a completed row that names its own criteria digest, are stored as they are.
func TestRunJournalNamesTheCriteriaOfAReviewerCall(t *testing.T) {
	for role, want := range map[string]string{"reviewer": "crit-1", "reviewer-final": "crit-1", "executor": ""} {
		mem := engine.NewMemoryJournal()
		var j engine.Journal = runJournal{mem}
		j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, Op: "agent", CallID: "c_" + role, Event: engine.EvPrepared,
			Data: map[string]any{"role": role, "node": "", "input_snapshot": "plan=plan-1;criteria=crit-1"}})
		got := j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, Op: "agent", CallID: "c_" + role, Event: engine.EvCompleted, Data: map[string]any{"result": "{}"}})
		if digest, _ := got.Data["criteria_digest"].(string); digest != want {
			t.Errorf("the completed row of a %s call names criteria digest %q, want %q", role, digest, want)
		}
	}
	mem := engine.NewMemoryJournal()
	var j engine.Journal = runJournal{mem}
	j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, Op: "agent", CallID: "c_rf", Event: engine.EvPrepared,
		Data: map[string]any{"role": "reviewer-final", "input_snapshot": "criteria=crit-1"}})
	got := j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, Op: "agent", CallID: "c_rf", Event: engine.EvCompleted, Data: map[string]any{"criteria_digest": "own"}})
	if got.Data["criteria_digest"] != "own" {
		t.Errorf("a completed row that names its own criteria digest was changed: %v", got.Data)
	}
}

// TestRunUnderLaneOperationsCallsBatchCommitted (CRW-1277 h): a run of the lane over ProductionWorld with the upward hook the binary installs
// hands every committed batch to the hook (F11/CRW-1204 regression of the journal wrapper: the engine never registered its notice).
func TestRunUnderLaneOperationsCallsBatchCommitted(t *testing.T) {
	hook := &commitHook{}
	prev := journal.UpwardFactory
	t.Cleanup(func() { journal.UpwardFactory = prev })
	journal.UpwardFactory = func(*store.Store, *journal.Journal, string, string, string) engine.UpwardHook { return hook }
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	l.closed(l.runToEnd(spec.RunID))
	hook.mu.Lock()
	defer hook.mu.Unlock()
	if hook.batches == 0 {
		t.Error("the upward hook was told of no committed batch during the run")
	}
}

// TestBuildLaneOfAWorkflowWithoutLaneOperations (CRW-1277 h): a script workflow that calls no crw.* operation needs no lane, so a role table
// without the executor and reviewer pairs does not fail its run (BuildLane answers an empty lane world); a workflow that calls one still
// refuses such a table.
func TestBuildLaneOfAWorkflowWithoutLaneOperations(t *testing.T) {
	w := newWorld(t)
	j, err := journal.Open(context.Background(), w.st, "run-nolane", journal.Identity{}, journal.Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.js")
	if err := os.WriteFile(plain, []byte("export const meta = { name: 'plain' }\nconst r = await agent.call('say hi', { agentType: 'executor' })\nreturn r.status\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	laneful := filepath.Join(dir, "laneful.js")
	if err := os.WriteFile(laneful, []byte("export const meta = { name: 'laneful' }\nawait crw.commit({ node: 'n1' })\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := func(script string) LaneRequest {
		return LaneRequest{WorldRequest: WorldRequest{Spec: Spec{Schema: SpecSchema, RunID: "run-nolane", Workflow: "x", Script: script, Thread: "thread-owner"}, State: w.state},
			Store: w.st, Journal: j, Policy: &engine.Policy{}}
	}
	got, err := BuildLane(context.Background(), req(plain), LaneOptions{})
	if err != nil {
		t.Fatalf("a workflow without lane operations: %v", err)
	}
	if got.Ops != nil || got.Journal != nil {
		t.Errorf("a workflow without lane operations got a lane: %d operations", len(got.Ops))
	}
	if _, err := BuildLane(context.Background(), req(laneful), LaneOptions{}); err == nil || !strings.Contains(err.Error(), "no pair") {
		t.Errorf("a workflow with lane operations and a role table without pairs: %v", err)
	}
}

// TestSchedRelayTargetIsTheShortBranch (CRW-1317): the branch an agent node integrates on is the target's short name, as compile puts
// it in the plan's edges: a handoff that writes refs/heads/dev integrates on dev, never on refs/heads/refs/heads/dev.
func TestSchedRelayTargetIsTheShortBranch(t *testing.T) {
	for _, ref := range []string{"refs/heads/dev", "dev"} {
		in := compile.Input{Schema: compile.InputSchema, Projects: []compile.Project{{Key: "P", PlanID: "plan-1", Target: &compile.Target{Repository: "/srv/shop", BaseRef: ref}}}}
		r := &SchedRelay{input: &in}
		checkout, branch, err := r.target("plan-1")
		if err != nil || checkout != "/srv/shop" || branch != "dev" {
			t.Errorf("base_ref %q: %q %q %v", ref, checkout, branch, err)
		}
	}
}

// pastRelationshipExecution gives a node of the lab's plan an execution of the retired relationship (`child` session) path, as an old run's
// child left it in the relay: a dag_node_executions row and no agent execution.
func (l *laneLab) pastRelationshipExecution(node string) {
	l.t.Helper()
	if _, err := l.w.st.DB.Exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES (?, ?, ?, 1, ?, 'initial', ?)",
		l.plan, node, "rel-old-"+node, "sha256:old-manifest-"+node, "req-old-"+node); err != nil {
		l.t.Fatal(err)
	}
}

// TestSchedRelayCloseLeavesARetiredRelationshipExecution (CRW-1415, PR #889 round 2): a node the retired `child` session path once executed
// (a relationship execution, no agent execution) has no agent execution for Close to close: Close answers nothing closed and no
// error, writes no agent closure, and does not take the plan's coordinator epoch for it.
func TestSchedRelayCloseLeavesARetiredRelationshipExecution(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	r := l.relayFor("run-close00001", l.handoffBytes(nil), 1)
	node := l.nodeOf["A-1"]
	l.pastRelationshipExecution(node)
	res, err := r.Close(ctx, ops.CloseRequest{Plan: l.plan, Node: node, Actor: labOwner, Reason: "size_split"})
	if err != nil || res.Closed || res.Held || res.SlotReleased {
		t.Fatalf("the close of a node with a retired relationship execution: %+v %v", res, err)
	}
	if n := l.count("SELECT COUNT(*) FROM dag_agent_executions") + l.count("SELECT COUNT(*) FROM dag_coordinator_claims"); n != 0 {
		t.Errorf("the close wrote %d rows", n)
	}
}

// TestChildSizedNodeWithARetiredRelationshipExecutionEscalates (CRW-1415, PR #889 round 2): a run of the project lane over a plan whose
// child-sized node the retired `child` session path once executed escalates the node as size_split with detail child_retired, with no
// item error and no close refusal, and the other node of the milestone lands. Only a DESIGN of before S-1 (CRW-1435) sizes a node for a
// child: the run is an old run's, of the project lane whose DESIGN is the current one less S-1 (child in the size_fit enum, no
// sub_tasks member), started from a script file, and its architect answers with no sub_tasks member.
func TestChildSizedNodeWithARetiredRelationshipExecutionEscalates(t *testing.T) {
	lane := legacyChildLane(t)
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1"})
	l.legacyChildDesign("A-2")
	node := l.nodeOf["A-2"]
	l.pastRelationshipExecution(node)
	spec := l.start("start", lane, "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	if spec.Script != lane || spec.Workflow != ProjectLaneWorkflow {
		t.Fatalf("the run is of %s (script %s), want the legacy project lane %s", spec.Workflow, spec.Script, lane)
	}
	res := l.runToEnd(spec.RunID)
	var v laneValue
	_ = json.Unmarshal(res.Value, &v)
	lanes := map[string]string{}
	for _, ln := range v.Lanes {
		lanes[ln.Node] = ln.State + "/" + ln.Reason
	}
	if lanes[node] != "escalated/size_split" || !strings.HasPrefix(lanes[l.nodeOf["A-1"]], "landed") {
		t.Fatalf("the lanes %v (run %s %s %s)\n%s", lanes, res.Status, res.ErrorKind, res.Error, res.Value)
	}
	var esc []engine.Event
	for _, ev := range l.events() {
		if ev.Kind == engine.KindFlow && ev.Event == engine.EvEscalation && str(ev.Data, "node") == node {
			esc = append(esc, ev)
		}
		if ev.Kind == engine.KindCall && ev.Op == ops.OpEscalate && ev.Event != engine.EvCompleted && ev.Event != engine.EvPrepared && str(ev.Data, "node") == node {
			t.Errorf("the escalation of %s ended %s: %v", node, ev.Event, ev.Data)
		}
	}
	if len(esc) != 1 || str(esc[0].Data, "detail") != "child_retired" {
		t.Errorf("the escalations of %s: %+v, want one with detail child_retired", node, esc)
	}
}

// legacyChildLane is the project lane of a run of before S-1 (CRW-1435), the only lane whose DESIGN sizes a node for a child: the current
// lane less S-1 (child in the size_fit enum, no sub_tasks member), written to a script file a run is started from.
func legacyChildLane(t *testing.T) string {
	t.Helper()
	text, err := workflows.Source(ProjectLaneWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	const designImport = "import { RECEIPT, DESIGN, DESIGN_AUDIT, REFLECTION } from 'crw:lib/schemas'\n"
	head, ok := strings.CutPrefix(string(text), designImport)
	at := strings.Index(head, "\nexport const meta = ")
	if !ok || at < 0 {
		t.Fatalf("project-lane.js no longer starts with the import of DESIGN this test replaces, followed by its meta line")
	}
	current, _ := ops.SchemaText(ops.SchemaDesign)
	var design map[string]any
	if err := json.Unmarshal([]byte(current), &design); err != nil {
		t.Fatal(err)
	}
	issue := design["properties"].(map[string]any)["issues"].(map[string]any)["items"].(map[string]any)
	delete(issue["properties"].(map[string]any), "sub_tasks")
	issue["properties"].(map[string]any)["size_fit"].(map[string]any)["enum"] = []any{"executor", "child", "split"}
	issue["required"] = slices.DeleteFunc(issue["required"].([]any), func(r any) bool { return r == "sub_tasks" })
	legacyDesign, _ := json.Marshal(design)
	end := at + 1 + strings.IndexByte(head[at+1:], '\n') + 1
	lane := filepath.Join(t.TempDir(), ProjectLaneWorkflow+".js")
	if err := os.WriteFile(lane, []byte("import { RECEIPT, DESIGN_AUDIT, REFLECTION } from 'crw:lib/schemas'\n"+head[:end]+"const DESIGN = "+string(legacyDesign)+"\n"+head[end:]), 0o600); err != nil {
		t.Fatal(err)
	}
	return lane
}

// legacyChildDesign makes the lab's architect answer a DESIGN of before S-1 that sizes the node of issue key for a child: no issue
// carries the sub_tasks member.
func (l *laneLab) legacyChildDesign(key string) {
	l.designEdit = func(d *ops.Design) {
		for i := range d.Issues {
			if d.Issues[i].Issue == key {
				d.Issues[i].SizeFit, d.Issues[i].SizeReason = "child", "regions_gt3"
			}
		}
	}
	l.turn = func(c labCall, _ fakehost.TurnRequest) (fakehost.TurnPlan, bool) {
		if c.Role != "architect" || strings.HasPrefix(c.Label, "reflection:") {
			return fakehost.TurnPlan{}, false
		}
		var m map[string]any
		raw, _ := json.Marshal(l.design())
		_ = json.Unmarshal(raw, &m)
		for _, is := range m["issues"].([]any) {
			delete(is.(map[string]any), "sub_tasks")
		}
		return labSay(m), true
	}
}

// TestIntegrationReuseLeavesPendingPlanChangesToRelease (CRW-1597): a node with no integrated acceptance answers
// not-integrated even when the slice the caller reads is an older pinned one - the plan supersession of a pending node is the
// ordinary release path's to decide, never a refusal of the reuse read.
func TestIntegrationReuseLeavesPendingPlanChangesToRelease(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	r := l.relayFor("run-reuse-pending", l.handoffBytes(nil), 1)
	ms, err := r.Milestone(context.Background(), l.plan, "M1")
	if err != nil || len(ms.Nodes) != 1 {
		t.Fatalf("milestone: %+v %v", ms, err)
	}
	node := ms.Nodes[0]
	node.SliceDigest = "an-older-pinned-slice"
	got, err := r.Integrated(context.Background(), l.plan, node)
	if err != nil || got.Integrated {
		t.Fatalf("a pending node must reach its ordinary release check: %+v %v", got, err)
	}
}
