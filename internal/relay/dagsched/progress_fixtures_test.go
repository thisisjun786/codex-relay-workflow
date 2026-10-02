package dagsched

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// progHead2 is a second accepted head: two implementation nodes of one fixture cannot pin the same commit.
const progHead2 = "2222222222222222222222222222222222222222"

// progress is one query of a plan in its own read transaction, the way dag-progress asks it.
func (f *fixture) progress(plan string) Progress {
	f.t.Helper()
	p, err := f.sched.ReadProgress(context.Background(), plan)
	if err != nil {
		f.t.Fatalf("progress %s: %v", plan, err)
	}
	return p
}

// nodeNamed is one node's projection.
func (p Progress) nodeNamed(id string) NodeProgress {
	for _, n := range p.Nodes {
		if n.NodeID == id {
			return n
		}
	}
	panic("no node " + id + " in the progress document")
}

// stageNamed is one stage's count and members.
func (p Progress) stageNamed(name string) StageCount {
	for _, s := range p.Stages {
		if s.Stage == name {
			return s
		}
	}
	panic("no stage " + name + " in the progress document")
}

// byStage is the members of every stage that has any, sorted, as a test states what it expects.
func (p Progress) byStage() map[string][]string {
	out := map[string][]string{}
	for _, s := range p.Stages {
		if s.Nodes > 0 {
			out[s.Stage] = append([]string(nil), s.NodeIDs...)
			sort.Strings(out[s.Stage])
		}
	}
	return out
}

// printed is the document as the command prints it.
func (p Progress) printed() string { return pyjson.Dumps(p.Object(), pyjson.Options{Indent: 2}) }

// decoded is the printed document as generic JSON, so a test reads it as a consumer would.
func (p Progress) decoded(t testing.TB) map[string]any {
	t.Helper()
	v, err := pyjson.Loads(p.printed(), pyjson.LoadOptions{})
	if err != nil {
		t.Fatalf("the printed document is not JSON: %v", err)
	}
	return toGeneric(v)
}

func toGeneric(v any) map[string]any {
	out := map[string]any{}
	if o, ok := v.(pyjson.Object); ok {
		for _, f := range o {
			out[f.Key] = genericValue(f.Value)
		}
	}
	return out
}

func genericValue(v any) any {
	switch x := v.(type) {
	case pyjson.Object:
		return toGeneric(x)
	case []any:
		list := make([]any, len(x))
		for i, e := range x {
			list[i] = genericValue(e)
		}
		return list
	}
	return v
}

// finalReport records a final ready_for_review event of a relationship, with the lineage that makes it the head of its generation: the child reported, nothing was acknowledged or ruled.
func (f *fixture) finalReport(rid, event, revision string) {
	f.t.Helper()
	now := f.clock()
	f.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
		" VALUES (?, ?, 1, ?, 'ready_for_review', 'child', 'child', 'turn-1', 'completed', '{}', 'final', ?, ?)", event, rid, revision, now, now)
	f.exec("INSERT INTO revision_lineage (relationship_id, execution_generation, event_id, revision_hash, declared_by, recorded_at) VALUES (?, 1, ?, ?, 'child', ?)", rid, event, revision, now)
}

// seedReceived is a node whose child has reported and nobody has judged: the relay's word is received.
func (f *fixture) seedReceived(plan, node string) (relationship, event string) {
	f.t.Helper()
	relationship = f.startNode(plan, node)
	event = "evt-" + relationship
	f.finalReport(relationship, event, dig("revision "+relationship))
	return relationship, event
}

// workReport records the work report an earlier build wrote for an event: the repository, the pull request and the head it names.
func (f *fixture) workReport(relationship, event string, generation int64, revision string, number int64, head string) {
	f.t.Helper()
	f.exec("INSERT INTO work_reports (event_id, submission_no, relationship_id, execution_generation, revision_hash, repository, pr_number, pr_url, head_sha, cxc_status, cxc_reason, contract_version, summary, next_action, recorded_at)"+
		" VALUES (?, 1, ?, ?, ?, 'owner/repo', ?, ?, ?, 'DONE', 'proved', 'v1', 'done', 'merge', ?)",
		event, relationship, generation, revision, number, "https://forge.example/owner/repo/pull/"+itoa64(number), head, f.clock())
}

// childBlocked records the child's own final blocked_needs_input receipt.
func (f *fixture) childBlocked(relationship string) {
	f.t.Helper()
	now := f.clock()
	f.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
		" VALUES (?, ?, 1, ?, 'blocked_needs_input', 'child', 'child', 'turn-b', 'completed', 'needs a decision', 'final', ?, ?)", "evt-blocked-"+relationship, relationship, dig("blocked "+relationship), now, now)
}

// evict records the merge check that left an accepted node's pull request out of the merge lane (the retry on the same head failed again).
func (f *fixture) evict(a accepted) {
	f.t.Helper()
	f.exec("INSERT INTO dag_merge_checks (check_id, acceptance_id, check_seq, head_sha, observed_head_sha, base_tip_sha, checks_digest, evidence_json, failed_required_json, round_no, outcome, reason, recorded_at)"+
		" VALUES (?, ?, 1, ?, ?, ?, ?, '{}', '[]', 2, 'evicted', 'a required check failed again on the same head', ?)", "dmc-"+a.Acceptance.AcceptanceID[:8], a.Acceptance.AcceptanceID, a.Acceptance.HeadSHA, a.Acceptance.HeadSHA, a.Acceptance.HeadSHA, dig("checks"), f.clock())
}

// unsizedReport seeds a verified, accepted non_pr node whose receipt declares its artifact without a size: only the file's own size says how large it is, so a manifest rebuilt from
// the store reaches for a stat of the file.
func (f *fixture) unsizedAccepted(plan, node string, inputs []any) accepted {
	f.t.Helper()
	snap := f.snapshot(plan)
	n, ok := nodeOf(snap, node)
	if !ok {
		f.t.Fatalf("no node %s", node)
	}
	rid := "rel-" + plan + "-" + node
	now := f.clock()
	root := f.t.TempDir()
	file := filepath.Join(root, node+".md")
	content := []byte("unsized artifact of " + node + "\n")
	if err := os.WriteFile(file, content, 0o600); err != nil {
		f.t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	revision, err := store.ManifestRevision([]store.ManifestEntry{{Path: file, SHA256: hex.EncodeToString(sum[:])}})
	if err != nil {
		f.t.Fatal(err)
	}
	f.relationshipOf(rid, n.IssueKey, node, root, now)
	event := "evt-" + rid
	receipt := "{\"manifest\":[{\"path\":" + jsonString(file) + ",\"sha256\":\"" + hex.EncodeToString(sum[:]) + "\"}]}"
	f.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
		" VALUES (?, ?, 1, ?, 'ready_for_review', 'child', ?, 'turn-1', 'completed', ?, 'final', ?, ?)", event, rid, revision, "child-"+node, receipt, now, now)
	f.exec("INSERT INTO revision_lineage (relationship_id, execution_generation, event_id, revision_hash, declared_by, recorded_at) VALUES (?, 1, ?, ?, 'child', ?)", rid, event, revision, now)
	f.exec("INSERT INTO acks (event_id, record, ack_turn_id, accepted, verified, ack_at) VALUES (?, '{}', 'ack-turn', 1, 'verified', ?)", event, now)
	f.exec("INSERT INTO ack_evidence (event_id, tier, observed_at) VALUES (?, 'host_read', ?)", event, now)
	f.exec("INSERT INTO verdicts (event_id, record, verdict, verdict_turn_id, decided_at) VALUES (?, '{}', 'verified', 'verdict-turn', ?)", event, now)
	f.exec("INSERT INTO verdict_context (event_id, set_digest, coverage, currency, head_event_id, head_revision, ack_evidence, recorded_at) VALUES (?, ?, '{}', 'current', ?, ?, '{}', ?)",
		event, n.CriteriaSetDigest, event, revision, now)
	f.exec("INSERT INTO canonical_criteria (relationship_id, criterion_id, title, required, set_digest, recorded_at) VALUES (?, 'c1', 'criterion', 1, ?, ?)", rid, n.CriteriaSetDigest, now)
	f.exec("INSERT INTO verification_mode (relationship_id, mode, recorded_at) VALUES (?, 'managed', ?)", rid, now)
	if inputs == nil {
		inputs = []any{}
	}
	manifest := f.putManifest(snap, node, inputs)
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES (?, ?, ?, 1, ?, 'initial', ?)",
		plan, node, rid, manifest, ReleaseRequestID(plan, node, manifest))
	a := Acceptance{PlanID: plan, NodeID: node, ManifestDigest: manifest, RelationshipID: rid, ExecutionGeneration: 1, EventID: event, RevisionHash: revision,
		CriteriaSetDigest: n.CriteriaSetDigest, Verdict: "verified", AckTier: "host_read", VerdictTurnID: "verdict-turn", RuleVersionJSON: "{}", AcceptedByTask: "parent", AcceptedAt: f.clock(), State: "active"}
	a.AcceptanceID = AcceptanceDigest(a)
	f.insertAcceptance(a)
	return accepted{Acceptance: a, Event: event, Manifest: manifest, Root: root, Files: []string{file}}
}

// relationshipOf records an active relationship bound to a generation, as a release leaves it.
func (f *fixture) relationshipOf(rid, issue, node, root, now string) {
	f.t.Helper()
	f.exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at)"+
		" VALUES (?, ?, 'active', 'parent', 'host', ?, 'host', 1, ?, '[\"parent\"]', ?, ?)", rid, issue, "child-"+node, "["+jsonString(root)+"]", now, now)
	f.exec("INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, opened_at, bound_at) VALUES (?, 1, ?, 'bound', 'turn-dispatch', ?, ?)", rid, "dispatch-"+rid, now, now)
}

// withMark is the context Progress reads under, for a test that calls the layers below it.
func withMark(ctx context.Context) context.Context { return withStoreOnly(ctx) }

// countStats counts the artifact stats a test provokes; the counter is restored when the test ends.
func countStats(t *testing.T) *int {
	t.Helper()
	calls := 0
	original := statArtifact
	statArtifact = func(path string) (os.FileInfo, error) {
		calls++
		return original(path)
	}
	t.Cleanup(func() { statArtifact = original })
	return &calls
}

// stageSet is the expectation helper: it compares a document's non-empty stages with the stated ones.
func wantStages(t *testing.T, p Progress, want map[string][]string) {
	t.Helper()
	got := p.byStage()
	for stage, ids := range want {
		sort.Strings(ids)
		if !equalStrings(got[stage], ids) {
			t.Errorf("stage %s = %v, want %v\n%s", stage, got[stage], ids, p.brief())
		}
	}
	for stage, ids := range got {
		if _, ok := want[stage]; !ok {
			t.Errorf("stage %s = %v, want it empty\n%s", stage, ids, p.brief())
		}
	}
	total := 0
	for _, s := range p.Stages {
		total += s.Nodes
	}
	if total != p.Denominator.Nodes {
		t.Errorf("the stage counts add up to %d, the denominator is %d: the stages must partition the live nodes", total, p.Denominator.Nodes)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// brief is one line per node.
func (p Progress) brief() string {
	out := ""
	for _, n := range p.Nodes {
		out += n.NodeID + "[" + n.Stage + " " + n.State + " " + n.Disposition + " " + n.Reason + "] "
	}
	return out
}
