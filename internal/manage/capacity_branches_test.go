package manage

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The branch candidates are driven through injected inputs only: a fake relay answering
// dag-plan-show and dag-ready, a temporary relay store built through the exported store schema,
// and the replaced gh and status-page seams.

const (
	branchTestPlan   = "p-branch"
	branchTestRepo   = "owner/repo"
	branchTestParent = "parent-1"
)

var branchTestNow = time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

// branchFixture is one plan's world: the two relay answers, the store the regions and the
// releases are written to, and the configuration the judgement reads.
type branchFixture struct {
	t        *testing.T
	dir      string
	stateDir string
	relayDir string
	env      *Env
	cfg      *Config
	section  map[string]any
	ready    []string
	waiting  []string
	store    *store.Store
	planned  bool
	kind     string
}

func branchNewFixture(t *testing.T, ready ...string) *branchFixture {
	t.Helper()
	coreTempHome(t)
	dir := t.TempDir()
	f := &branchFixture{t: t, dir: dir, stateDir: filepath.Join(dir, "state"), relayDir: filepath.Join(dir, "relay"), ready: ready}
	for _, path := range []string{f.stateDir, f.relayDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	opened, err := store.Open(context.Background(), filepath.Join(f.relayDir, "relay.sqlite3"), filepath.Join(dir, "app-server-control.sock"))
	if err != nil {
		t.Fatalf("create the fixture store: %v", err)
	}
	f.store = opened
	t.Cleanup(f.close)
	f.env = &Env{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard, Getenv: os.Getenv,
		Now: func() time.Time { return branchTestNow }, Executable: filepath.Join(dir, "crw")}
	f.section = map[string]any{"plans": []map[string]any{{"plan": branchTestPlan, "project": "P-BRANCH", "parent": branchTestParent}}}
	f.load()
	return f
}

// close releases the writer, so the read-only open under test sees a settled file.
func (f *branchFixture) close() {
	f.t.Helper()
	if f.store == nil {
		return
	}
	if err := f.store.Close(); err != nil {
		f.t.Fatalf("close the fixture store: %v", err)
	}
	f.store = nil
}

func (f *branchFixture) load() {
	f.t.Helper()
	raw, err := json.Marshal(f.section)
	if err != nil {
		f.t.Fatal(err)
	}
	cfg := coreDefaults(f.env)
	cfg.Repository, cfg.StateDir, cfg.Relay.State = branchTestRepo, f.stateDir, f.relayDir
	cfg.raw = map[string]json.RawMessage{"capacity": raw}
	f.cfg = cfg
}

func (f *branchFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.DB.Exec(query, args...); err != nil {
		f.t.Fatalf("fixture insert: %v (%s)", err, query)
	}
}

// ensurePlan writes the plan and its first revision once, before any row that references them.
func (f *branchFixture) ensurePlan() {
	f.t.Helper()
	if f.planned {
		return
	}
	f.exec("INSERT INTO dag_plans (plan_id, project_key, created_by_task_id, created_at) VALUES (?,?,?,?)",
		branchTestPlan, "P-BRANCH", "task-parent", branchTestNow.Format(time.RFC3339))
	f.exec("INSERT INTO dag_plan_revisions (plan_id, revision_no, parent_revision_no, request_id, request_digest, change_json, state_digest, coordinator_epoch, author_task_id, recorded_at) VALUES (?,1,0,?,?,?,?,0,?,?)",
		branchTestPlan, branchTestPlan+"-request-1", "digest", "{}", "state", "task-parent", branchTestNow.Format(time.RFC3339))
	f.planned = true
}

// node adds one live node of the plan, as the plan holds it.
func (f *branchFixture) node(id, issue string) *branchFixture {
	f.t.Helper()
	f.ensurePlan()
	f.exec("INSERT INTO dag_nodes (plan_id, node_id, introduced_rev, retired_rev, slice_digest, issue_key, node_kind, title, criteria_set_digest, supersedes_node_id) VALUES (?,?,1,NULL,?,?,?,?,?,NULL)",
		branchTestPlan, id, "slice-"+id, issue, "implementation", issue, "criteria")
	return f
}

// edge adds one live edge of the plan, of a kind that says nothing about the connection.
func (f *branchFixture) edge(id, from, to string) *branchFixture {
	f.t.Helper()
	kind := f.kind
	if kind == "" {
		kind = "artifact_verified"
	}
	switch kind {
	case "decision":
		f.exec("INSERT INTO dag_edges (plan_id, edge_id, introduced_rev, retired_rev, from_node_id, to_node_id, kind, decision_subject, decision_digest, required_authority, pins_code_head) VALUES (?,?,1,NULL,?,?,?,'subject','digest','[\"owner\"]',0)",
			branchTestPlan, id, from, to, kind)
	default:
		f.exec("INSERT INTO dag_edges (plan_id, edge_id, introduced_rev, retired_rev, from_node_id, to_node_id, kind, target_repository, target_base_ref, pins_code_head) VALUES (?,?,1,NULL,?,?,?,?,?,0)",
			branchTestPlan, id, from, to, kind, branchTestRepo, "dev")
	}
	return f
}

// region declares one edit region of a node. The stated hold is what a declaration of this build
// writes, so the stored hold reads as the declarer's word, exactly as the scheduler reads it.
func (f *branchFixture) region(node, path, kind, key, change string, stated bool) *branchFixture {
	f.t.Helper()
	f.ensurePlan()
	f.exec("INSERT INTO dag_node_regions (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key, change, exclusive, declared_by, declared_at) VALUES (?,?,1,?,?,?,?,?,0,?,?)",
		branchTestPlan, node, branchTestRepo, path, kind, key, change, "task-parent", branchTestNow.Format(time.RFC3339))
	f.exec("INSERT INTO dag_node_region_holds (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key, stated) VALUES (?,?,1,?,?,?,?,?)",
		branchTestPlan, node, branchTestRepo, path, kind, key, stated)
	return f
}

// release records that one node was released for execution.
func (f *branchFixture) release(node string) *branchFixture {
	f.exec("INSERT INTO dag_releases (plan_id, node_id, manifest_digest, managed_request_id, coordinator_epoch, decided_at) VALUES (?,?,?,?,0,?)",
		branchTestPlan, node, "manifest-"+node, "request-"+node, branchTestNow.Format(time.RFC3339))
	return f
}

// integrated records a landed node: its accepted head is observed contained in the base.
func (f *branchFixture) integrated(node string) *branchFixture {
	f.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state) VALUES (?,?,?,?,?,1,?,?,?,?,?,?,?,?,0,?,?)",
		"acceptance-"+node, branchTestPlan, node, "manifest-"+node, "rel-"+node, "event", "revision", "criteria",
		"verified", "verified", "turn", "{}", "task-parent", branchTestNow.Format(time.RFC3339), "active")
	f.exec("INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, observed_seq, reverted_by, observed_at) VALUES (?,?,?,?,?,?,1,?,1,NULL,?)",
		"observation-"+node, "acceptance-"+node, branchTestRepo, "dev", "subject", "tip", "ancestry", branchTestNow.Format(time.RFC3339))
	return f
}

// publish writes the relay's answer and the state the judgement reads, then releases the store
// writer so the read-only open under test sees a settled file.
func (f *branchFixture) publish() *branchFixture {
	f.t.Helper()
	branchWriteReady(f.t, filepath.Join(f.dir, "ready.json"), f.ready, "within")
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"  *dag-ready*) cat " + coreShellQuote(filepath.Join(f.dir, "ready.json")) + " ;;\n" +
		"  *) echo refused >&2; exit 2 ;;\nesac\n"
	if err := os.WriteFile(f.env.Executable, []byte(script), 0o700); err != nil {
		f.t.Fatal(err)
	}
	waiting := f.ready
	if f.waiting != nil {
		waiting = f.waiting
	}
	state := map[string]any{"plans": map[string]any{branchTestPlan: map[string]any{
		"since": float64(branchTestNow.Add(-60 * time.Minute).Unix()), "waiting": waiting}}}
	branchWriteJSON(f.t, filepath.Join(f.stateDir, capacityStateFile), state)
	branchSeams(f.t)
	f.close()
	return f
}

// branchWriteReady writes one dag-ready answer whose ready set is the given issue keys.
func branchWriteReady(t *testing.T, path string, keys []string, hostMemory string) {
	t.Helper()
	ready := make([]any, len(keys))
	for i, key := range keys {
		ready[i] = map[string]any{"node_id": "n" + key, "issue_key": key, "disposition": "ready", "reason": nil}
	}
	branchWriteJSON(t, path, map[string]any{"ok": true, "schema": "dag-ready/1",
		"pass":  map[string]any{"free_slots": 0, "ceiling": 12, "held": 12, "host_memory": map[string]any{"state": hostMemory}},
		"ready": ready, "nodes": []any{}})
}

// branchSeams replaces the gh and status-page seams for one test: no merges and no incident, so the
// judgement rests on the waiting set the fixture states.
func branchSeams(t *testing.T) {
	t.Helper()
	exec, get := capacityExec, capacityHTTPGet
	t.Cleanup(func() { capacityExec, capacityHTTPGet = exec, get })
	capacityExec = func(context.Context, string, ...string) ([]byte, error) { return []byte("[]"), nil }
	capacityHTTPGet = func(context.Context, string) ([]byte, error) { return []byte("{\"incidents\":[]}"), nil }
}

func branchWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// run judges the plan the fixture describes and returns it.
func (f *branchFixture) run() CapacityPlan {
	f.t.Helper()
	config := capacityConfig
	f.t.Cleanup(func() { capacityConfig = config })
	capacityConfig = func(*Env) *Config { return f.cfg }
	report, err := Capacity(context.Background(), f.env, f.cfg, false)
	if err != nil {
		f.t.Fatalf("Capacity: %v", err)
	}
	if len(report.Plans) != 1 {
		f.t.Fatalf("the report holds %d plans, want 1", len(report.Plans))
	}
	return report.Plans[0]
}

// branchList is the candidate list of a plan that carries one.
func branchList(t *testing.T, plan CapacityPlan) []BranchCandidate {
	t.Helper()
	if plan.Branches == nil {
		t.Fatalf("the plan carries no branches: %+v", plan)
	}
	return []BranchCandidate(*plan.Branches)
}

// branchSummaries is every candidate as its node ids, its regions, its waiting count and the edges
// inside it: the shape the assertions read.
func branchSummaries(list []BranchCandidate) []string {
	out := make([]string, len(list))
	for i, c := range list {
		ids := make([]string, len(c.Nodes))
		for j, node := range c.Nodes {
			ids[j] = node.NodeID
		}
		out[i] = strings.Join(ids, "+") + " " + strings.Join(c.Regions, ",") +
			" ready=" + branchItoa(c.ReadyCount) + " edges=" + branchItoa(c.EdgesInside)
	}
	return out
}

// branchItoa is the small decimal spelling the summaries read, kept here so the assertions carry
// no dependency beyond the standard library.
func branchItoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func branchWant(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("branches = %v, want %v", got, want)
	}
}

// A threshold this build cannot read is a refusal, never a silent fall back to the default: a
// bundle reported against the wrong floor is a wrong answer, not a missing one.
func TestBranchCandidatesRefuseAnUnreadableThreshold(t *testing.T) {
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.node("A", "CRW-1").node("B", "CRW-2")
	f.region("A", "a.go", "file", "", "edit", false)
	f.region("B", "b.go", "file", "", "edit", false)
	f.publish()
	f.section["min_branch_nodes"] = "two"
	f.load()
	config := capacityConfig
	t.Cleanup(func() { capacityConfig = config })
	capacityConfig = func(*Env) *Config { return f.cfg }
	if _, err := Capacity(context.Background(), f.env, f.cfg, false); err == nil {
		t.Fatal("a min_branch_nodes that is not a number judged the plan anyway")
	}
}

// An explicit threshold is honored, and a count below zero is refused: a negative floor or cap is
// a mistake in the configuration, not a request for the default.
func TestBranchCandidatesHonorAndRefuseExplicitThresholds(t *testing.T) {
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3")
	f.region("A", "a.go", "file", "", "edit", false)
	f.region("B", "b.go", "file", "", "edit", false)
	f.region("C", "c.go", "file", "", "edit", false)
	f.publish()
	// A floor of one admits each single node as its own bundle, and none of them is the whole plan.
	f.section["min_branch_nodes"] = 1
	f.load()
	if got := branchSummaries(branchList(t, f.run())); len(got) != 3 {
		t.Fatalf("with min_branch_nodes 1 the three single nodes are candidates: %v", got)
	}
	f.section["min_branch_nodes"] = 0
	f.load()
	if got := branchSummaries(branchList(t, f.run())); len(got) != 3 {
		t.Fatalf("with min_branch_nodes 0 the single nodes are still candidates: %v", got)
	}
	f.section["min_branch_nodes"] = -1
	f.load()
	config := capacityConfig
	t.Cleanup(func() { capacityConfig = config })
	capacityConfig = func(*Env) *Config { return f.cfg }
	if _, err := Capacity(context.Background(), f.env, f.cfg, false); err == nil {
		t.Fatal("a negative min_branch_nodes judged the plan anyway")
	}
	f.section["min_branch_nodes"] = nil
	f.section["max_branches"] = -2
	f.load()
	capacityConfig = func(*Env) *Config { return f.cfg }
	if _, err := Capacity(context.Background(), f.env, f.cfg, false); err == nil {
		t.Fatal("a negative max_branches judged the plan anyway")
	}
}

// A command run forgets the flag it was given, so a process that runs the command many times
// retains no Env and a later run of the same process is not handed an earlier run's flag.
func TestBranchCandidatesForgetTheAlwaysFlagAfterTheRun(t *testing.T) {
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.node("A", "CRW-1").node("B", "CRW-2")
	f.region("A", "a.go", "file", "", "edit", false)
	f.region("B", "b.go", "file", "", "edit", false)
	f.publish()
	config := capacityConfig
	t.Cleanup(func() { capacityConfig = config })
	capacityConfig = func(*Env) *Config { return f.cfg }
	f.env.Stdout, f.env.Stderr = io.Discard, io.Discard
	if code := capacityCommand.Run(context.Background(), f.env, []string{"--branches-always", "--dry-run"}); code != 0 {
		t.Fatalf("the run with the flag: exit %d", code)
	}
	if branchAlwaysFor(f.env) {
		t.Fatal("the flag outlived the command run that was given it")
	}
	branchAlwaysMemoMu.Lock()
	retained := len(branchAlwaysMemo)
	branchAlwaysMemoMu.Unlock()
	if retained != 0 {
		t.Fatalf("the memo holds %d environments after the run, want none", retained)
	}
}

// The waiting set is the ready nodes plus the ones deferred for want of capacity, so a bundle
// whose member is only deferred still counts it.
func TestBranchCandidatesCountADeferredNodeAsWaiting(t *testing.T) {
	f := branchNewFixture(t, "CRW-1")
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3")
	f.edge("e1", "A", "B")
	for _, node := range []string{"A", "B", "C"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	f.waiting = []string{"CRW-1", "CRW-2"}
	// A second node deferred for want of a slot is part of the waiting set, and it is the node the
	// plan does not list as ready.
	f.publish()
	branchWriteJSON(t, filepath.Join(f.dir, "ready.json"), map[string]any{"ok": true, "schema": "dag-ready/1",
		"pass":  map[string]any{"free_slots": 0, "ceiling": 12, "held": 12, "host_memory": map[string]any{"state": "within"}},
		"ready": []any{map[string]any{"node_id": "nCRW-1", "issue_key": "CRW-1", "disposition": "ready", "reason": nil}},
		"nodes": []any{map[string]any{"node_id": "nCRW-2", "issue_key": "CRW-2", "disposition": "defer", "reason": "defer:no_capacity"}}})
	plan := f.run()
	candidates := branchList(t, plan)
	if len(candidates) != 1 {
		t.Fatalf("branches = %v, want the one bundle", branchSummaries(candidates))
	}
	if candidates[0].ReadyCount != 2 {
		t.Fatalf("ready_count = %d, want 2: one ready node and one deferred for want of capacity", candidates[0].ReadyCount)
	}
	for _, node := range candidates[0].Nodes {
		if !node.Ready {
			t.Fatalf("node %s reads not waiting, want every member of the waiting set to read waiting", node.NodeID)
		}
	}
}

// Every edge kind connects: the plan's edges are read whatever kind they are, so a bundle joined
// by an integrated or a decision edge is one bundle.
func TestBranchCandidatesConnectWhateverTheEdgeKind(t *testing.T) {
	for _, kind := range []string{"artifact_verified", "integrated", "decision"} {
		t.Run(kind, func(t *testing.T) {
			f := branchNewFixture(t, "CRW-1", "CRW-2")
			f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3")
			f.kind = kind
			f.edge("e1", "A", "B")
			for _, node := range []string{"A", "B", "C"} {
				f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
			}
			plan := f.publish().run()
			branchWant(t, branchSummaries(branchList(t, plan)), "A+B pkg/A.go,pkg/B.go ready=2 edges=1")
		})
	}
}

// C1: a bundle joined only by an edge is a candidate, and the graph's two bundles are two of them.
func TestBranchCandidatesConnectByEdgesOnly(t *testing.T) {
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("D", "CRW-4")
	f.edge("e1", "A", "B").edge("e2", "C", "D")
	for _, node := range []string{"A", "B", "C", "D"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	plan := f.publish().run()
	branchWant(t, branchSummaries(branchList(t, plan)),
		"A+B pkg/A.go,pkg/B.go ready=2 edges=1", "C+D pkg/C.go,pkg/D.go ready=0 edges=1")
}

// C1: a bundle joined only by overlapping regions is a candidate; regions in different places leave
// two of them.
func TestBranchCandidatesConnectByRegionsOnly(t *testing.T) {
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("D", "CRW-4")
	f.region("A", "internal/pkg", "tree", "", "edit", false)
	f.region("B", "internal/pkg/x.go", "file", "", "edit", false)
	f.region("C", "cmd/svc", "tree", "", "edit", false)
	f.region("D", "cmd/svc/d.go", "file", "", "edit", false)
	plan := f.publish().run()
	branchWant(t, branchSummaries(branchList(t, plan)),
		"A+B internal/pkg,internal/pkg/x.go ready=2 edges=0", "C+D cmd/svc,cmd/svc/d.go ready=0 edges=0")
}

// C1: a node that declared no region joins every live node, so the same graph becomes one component
// and nothing can be taken out.
func TestBranchCandidatesUndeclaredNodeJoinsEveryLiveNode(t *testing.T) {
	build := func(undeclared bool) *branchFixture {
		f := branchNewFixture(t, "CRW-1", "CRW-2")
		f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("D", "CRW-4").node("E", "CRW-5")
		f.edge("e1", "A", "B").edge("e2", "C", "D")
		for _, node := range []string{"A", "B", "C", "D"} {
			f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
		}
		if !undeclared {
			f.region("E", "pkg/E.go", "file", "", "edit", false)
		}
		return f
	}
	declared := branchSummaries(branchList(t, build(false).publish().run()))
	branchWant(t, declared, "A+B pkg/A.go,pkg/B.go ready=2 edges=1", "C+D pkg/C.go,pkg/D.go ready=0 edges=1")
	if got := branchSummaries(branchList(t, build(true).publish().run())); len(got) != 0 {
		t.Fatalf("with E undeclared, branches = %v, want none: E joins every live node, so the one component is the whole plan", got)
	}
}

// C1: a bundle that holds a released node is not a candidate.
func TestBranchCandidatesExcludeABundleWithAReleasedNode(t *testing.T) {
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("D", "CRW-4").node("E", "CRW-5")
	f.edge("e1", "A", "B").edge("e2", "C", "D")
	for _, node := range []string{"A", "B", "C", "D", "E"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	f.release("C")
	got := branchSummaries(branchList(t, f.publish().run()))
	branchWant(t, got, "A+B pkg/A.go,pkg/B.go ready=2 edges=1")
}

// C1: an integrated node is not live, so an edge into it joins nothing and its neighbour stands
// alone, below the floor.
func TestBranchCandidatesDoNotConnectThroughAnIntegratedNode(t *testing.T) {
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("D", "CRW-4")
	f.edge("e1", "A", "B").edge("e2", "C", "D")
	for _, node := range []string{"A", "B", "C", "D"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	f.integrated("C")
	got := branchSummaries(branchList(t, f.publish().run()))
	branchWant(t, got, "A+B pkg/A.go,pkg/B.go ready=2 edges=1")
}

// C1: when the whole plan is one component there is no candidate, and the plan still carries the
// empty list rather than no list at all.
func TestBranchCandidatesOfOneComponentAreNone(t *testing.T) {
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3")
	f.edge("e1", "A", "B").edge("e2", "B", "C")
	for _, node := range []string{"A", "B", "C"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	plan := f.publish().run()
	if got := branchSummaries(branchList(t, plan)); len(got) != 0 {
		t.Fatalf("branches = %v, want none: the component is the whole plan", got)
	}
}

// C2: the same input gives the same branches in the same order, ranked by waiting count, then node
// count, then the smallest node id, and capped at max_branches.
func TestBranchCandidatesOrderCapAndDeterminism(t *testing.T) {
	f := branchNewFixture(t, "CRW-1", "CRW-2", "CRW-3", "CRW-4")
	// four bundles: {A,B} with two waiting, {C,D,E} with two, {F,G} with none, {H,I} with none.
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("D", "CRW-4").node("E", "CRW-5").
		node("F", "CRW-6").node("G", "CRW-7").node("H", "CRW-8").node("I", "CRW-9")
	for _, pair := range [][2]string{{"A", "B"}, {"C", "D"}, {"D", "E"}, {"F", "G"}, {"H", "I"}} {
		f.edge("e-"+pair[0]+pair[1], pair[0], pair[1])
	}
	for _, node := range []string{"A", "B", "C", "D", "E", "F", "G", "H", "I"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	plan := f.publish().run()
	got := branchSummaries(branchList(t, plan))
	branchWant(t, got,
		"C+D+E pkg/C.go,pkg/D.go,pkg/E.go ready=2 edges=2",
		"A+B pkg/A.go,pkg/B.go ready=2 edges=1",
		"F+G pkg/F.go,pkg/G.go ready=0 edges=1")
	again := branchSummaries(branchList(t, f.run()))
	if strings.Join(again, " | ") != strings.Join(got, " | ") {
		t.Fatalf("the second run = %v, want the first run %v", again, got)
	}
}

// The document carries the issue's keys, one node per member with its readiness, and a hold plan
// carries no branches unless --branches-always was given.
func TestBranchCandidatesDocumentKeysAndTheAlwaysFlag(t *testing.T) {
	// Two waiting nodes keep the judgement persistent; CRW-3 is node C, which stands alone and is
	// below the floor, so the one candidate holds A and B with a single waiting node.
	f := branchNewFixture(t, "CRW-1", "CRW-3")
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3")
	f.edge("e1", "A", "B")
	for _, node := range []string{"A", "B", "C"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	f.publish()
	plan := f.run()
	if plan.Verdict != capacityExpand {
		t.Fatalf("the plan is %s, want the clear scenario %s", plan.Verdict, capacityExpand)
	}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	branches, ok := doc["branches"].([]any)
	if !ok || len(branches) != 1 {
		t.Fatalf("branches = %v, want one entry", doc["branches"])
	}
	entry, _ := branches[0].(map[string]any)
	for _, key := range []string{"nodes", "ready_count", "regions", "edges_inside"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("the entry carries no %q: %v", key, entry)
		}
	}
	if entry["ready_count"] != float64(1) || entry["edges_inside"] != float64(1) {
		t.Errorf("the entry = %v, want one waiting node and the one edge inside", entry)
	}
	nodes, _ := entry["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("nodes = %v, want two", entry["nodes"])
	}
	first, _ := nodes[0].(map[string]any)
	for _, key := range []string{"node_id", "issue_key", "ready"} {
		if _, ok := first[key]; !ok {
			t.Errorf("the node carries no %q: %v", key, first)
		}
	}
	if first["ready"] != true || first["issue_key"] != "CRW-1" {
		t.Errorf("the first node = %v, want CRW-1 and waiting", first)
	}

	// A hold plan carries no branches at all, and --branches-always carries them.
	// The host memory bound is what makes the plan a hold: the waiting set stays as it is.
	branchWriteReady(t, filepath.Join(f.dir, "ready.json"), []string{"CRW-1"}, "deferring")
	hold := f.run()
	if hold.Verdict != capacityHold || hold.Branches != nil {
		t.Fatalf("the hold plan = %+v, want a hold that carries no branches", hold)
	}
	branchAlwaysSet(f.env, true)
	t.Cleanup(func() { branchAlwaysSet(f.env, false) })
	if got := branchSummaries(branchList(t, f.run())); len(got) != 1 {
		t.Fatalf("with --branches-always the hold plan carries %v, want the one bundle", got)
	}
}

// The thresholds come from the capacity section, so a floor of three drops the two-node bundle and
// a cap of one keeps the first.
func TestBranchCandidatesThresholdsComeFromTheSection(t *testing.T) {
	// The waiting set is inside the larger bundle, so the ready count and the node count rank it
	// first; the floor then drops the smaller one and the cap keeps only what is left.
	f := branchNewFixture(t, "CRW-3", "CRW-4")
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("D", "CRW-4").node("E", "CRW-5").node("F", "CRW-6")
	f.edge("e1", "A", "B").edge("e2", "C", "D").edge("e3", "D", "E").edge("e4", "E", "F")
	for _, node := range []string{"A", "B", "C", "D", "E", "F"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	f.publish()
	branchWant(t, branchSummaries(branchList(t, f.run())),
		"C+D+E+F pkg/C.go,pkg/D.go,pkg/E.go,pkg/F.go ready=2 edges=3",
		"A+B pkg/A.go,pkg/B.go ready=0 edges=1")
	f.section["min_branch_nodes"] = 3
	f.load()
	branchWant(t, branchSummaries(branchList(t, f.run())),
		"C+D+E+F pkg/C.go,pkg/D.go,pkg/E.go,pkg/F.go ready=2 edges=3")
	f.section["max_branches"] = 1
	f.load()
	branchWant(t, branchSummaries(branchList(t, f.run())),
		"C+D+E+F pkg/C.go,pkg/D.go,pkg/E.go,pkg/F.go ready=2 edges=3")
}
