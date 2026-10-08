package manage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// The branch candidates are driven through injected inputs only: a fake relay answering dag-ready, a
// temporary relay store, and the replaced gh and status-page seams. A plan is described as revisions
// and written through the DAG repository, because the branch reading goes through the relay canon
// (dag.SnapshotAt), which recomputes every node's slice digest and the plan's state digest from the
// rows: a plan written by hand does not read back, and a plan that does not agree with itself is
// refused rather than reported as a bundle.

const (
	branchTestPlan    = "p-branch"
	branchTestProject = "P-BRANCH"
	branchTestRepo    = "owner/repo"
	branchTestParent  = "parent-1"
)

var branchTestNow = time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

// branchTestStamp is one instant of the fixture clock in the shape the relay stores timestamps.
func branchTestStamp(minutes int) string {
	return branchTestNow.Add(time.Duration(minutes) * time.Minute).UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

// branchFixture is one plan's world: the relay answer, the store the plan and its marks live in, and
// the configuration the judgement reads.
type branchFixture struct {
	t        *testing.T
	dir      string
	stateDir string
	relayDir string
	env      *Env
	cfg      *Config
	section  map[string]any
	// ready is the issue keys the fake dag-ready answer lists as ready; waiting, when set, is the
	// issue keys the capacity state file remembers.
	ready   []string
	waiting []string
	// kind is the edge kind the next edge is added with, and the node kind the next node is added
	// with (a decision follows a non_pr node).
	kind      string
	store     *store.Store
	revisions []branchRevisionFixture
	written   int
	// nodes is the issue key each node id of the described plan carries.
	nodes map[string]string
	// passRevision is the plan revision the fake dag-ready answer names; 0 leaves the key out, which is
	// the head a real relay reads when it answers for the plan's newest revision.
	passRevision int64
}

// branchRevisionFixture is one revision of a described plan: the instant it is recorded at and the
// changes it carries.
type branchRevisionFixture struct {
	at      string
	changes []dag.Change
}

func branchNewFixture(t *testing.T, ready ...string) *branchFixture {
	t.Helper()
	coreTempHome(t)
	dir := t.TempDir()
	f := &branchFixture{t: t, dir: dir, stateDir: filepath.Join(dir, "state"), relayDir: filepath.Join(dir, "relay"),
		ready: ready, nodes: map[string]string{}}
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
	// The relay's own readiness judges a node with no registered parent as defer:ownership_unverified,
	// and the branch reading takes its readiness from the store, so every fixture registers the
	// project's parent: without it no node would read as ready and the readiness would say nothing.
	f.parent()
	// The reading also judges the host memory bound (dagsched HostMemoryFromEnvironment over e.Getenv),
	// so the fixture gives it a fake host with memory to spare: a reading that judged the machine the
	// test runs on would defer every candidate whenever the host is busy, and the test would say nothing
	// about the plan. A test that means a held candidate points the same variable at a short fake host.
	proc := filepath.Join(dir, "proc")
	if err := os.MkdirAll(filepath.Join(proc, "pressure"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		"meminfo":         "MemAvailable: 100000000 kB\nSwapTotal: 2000000 kB\nSwapFree: 2000000 kB\n",
		"pressure/memory": "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
	} {
		if err := os.WriteFile(filepath.Join(proc, path), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.env = &Env{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
		Getenv: func(key string) string {
			if key == dagsched.EnvHostProcRoot {
				return proc
			}
			return os.Getenv(key)
		},
		Now: func() time.Time { return branchTestNow }, Executable: filepath.Join(dir, "crw")}
	f.section = map[string]any{"plans": []map[string]any{{"plan": branchTestPlan, "project": branchTestProject, "parent": branchTestParent}}}
	f.load()
	return f
}

// close writes every revision the test described and releases the writer, so the read-only open
// under test sees a settled file.
func (f *branchFixture) close() {
	f.t.Helper()
	if f.store == nil {
		return
	}
	f.writePlan()
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
	// A row that names the plan or one of its nodes needs the plan written first, and the DAG
	// repository is the only writer that leaves rows the canon's reading accepts.
	f.writePlan()
	if _, err := f.store.DB.Exec(query, args...); err != nil {
		f.t.Fatalf("fixture insert: %v (%s)", err, query)
	}
}

// parent registers the live parent of the fixture's project. The relay's own readiness judges a node
// with no registered parent as defer:ownership_unverified, so a reading that takes its readiness from
// the store needs this row before any node can read as ready.
func (f *branchFixture) parent() {
	f.t.Helper()
	if _, err := f.store.DB.Exec("INSERT OR IGNORE INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, cwd, cxc_session, status, revision, created_at, updated_at) VALUES ('binding-parent','parent','project',?,'task-parent','host',NULL,NULL,'active',1,?,?)",
		branchTestProject, branchTestStamp(0), branchTestStamp(0)); err != nil {
		f.t.Fatalf("fixture parent binding: %v", err)
	}
}

// readyNode records one node as the relay's own readiness reads it: the node ran on a relationship,
// that relationship accepted a head under the plan's criteria, and the accepted event carries the
// artifact list its revision names. The branch reading takes its readiness from the store
// (dagsched Scheduler.Ready on its own snapshot querier, which is what dag-ready runs), so a node
// without these rows has no active acceptance and is not ready. The node's own incoming edges are
// left to the caller: a node with no satisfied predecessor is ready as soon as these rows exist.
func (f *branchFixture) readyNode(node string) *branchFixture {
	f.t.Helper()
	// the plan's own criteria digest for this node (branchFixture.node), which the relay's standing
	// check compares the acceptance and the registered criteria against.
	criteria := testsupport.Dig("criteria " + node)
	// one artifact under the relationship's root: the receipt names it, hashes to the revision the
	// acceptance records, and the file is really there.
	root := filepath.Join(f.dir, "artifacts", node)
	if err := os.MkdirAll(root, 0o755); err != nil {
		f.t.Fatal(err)
	}
	path := filepath.Join(root, node+".md")
	body := []byte("artifact of " + node + "\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		f.t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	revision, err := store.ManifestRevision([]store.ManifestEntry{{Path: path, SHA256: digest, Bytes: func() *int64 { n := int64(len(body)); return &n }()}})
	if err != nil {
		f.t.Fatal(err)
	}
	receipt := `{"manifest":[{"path":` + branchJSONString(path) + `,"sha256":"` + digest + `","bytes":` + branchItoa(len(body)) + `}]}`
	relationship := "rel-" + node
	now := branchTestStamp(0)
	if err := storeseed.RecordRelationship(context.Background(), f.store, store.Relationship{
		ID: relationship, IssueKey: criteria, Status: "active", ParentTaskID: "task-parent", ChildTaskID: "child-" + node,
		Generation: 1, ArtifactRoots: "[" + branchJSONString(root) + "]", AllowedRecipients: `["parent"]`,
		CreatedAt: now, UpdatedAt: now,
	}, store.Generation{RelationshipID: relationship, Number: 1, DispatchRequestID: "dispatch-" + relationship,
		AnchorState: store.AnchorBound, DispatchTurnID: sql.NullString{String: "turn-dispatch", Valid: true},
		OpenedAt: now, BoundAt: sql.NullString{String: now, Valid: true}}, "host", "host"); err != nil {
		f.t.Fatalf("fixture relationship for %s: %v", node, err)
	}
	f.writePlan()
	event := "event-" + node
	f.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES (?,?,1,?, 'ready_for_review','child',?,'turn-1','completed',?,'final',?,?)",
		event, relationship, revision, node, receipt, now, now)
	f.exec("INSERT INTO revision_lineage (relationship_id, execution_generation, event_id, revision_hash, declared_by, recorded_at) VALUES (?,1,?,?,'child',?)",
		relationship, event, revision, now)
	f.exec("INSERT INTO acks (event_id, record, ack_turn_id, accepted, verified, ack_at) VALUES (?,'{}','ack-turn',1,'verified',?)", event, now)
	f.exec("INSERT INTO ack_evidence (event_id, tier, observed_at) VALUES (?,'host_read',?)", event, now)
	f.exec("INSERT INTO verdicts (event_id, record, verdict, verdict_turn_id, decided_at) VALUES (?,'{}','verified','verdict-turn',?)", event, now)
	f.exec("INSERT INTO verdict_context (event_id, set_digest, coverage, currency, head_event_id, head_revision, ack_evidence, recorded_at) VALUES (?,?,'{}','current',?,?,'{}',?)",
		event, criteria, event, revision, now)
	f.exec("INSERT INTO canonical_criteria (relationship_id, criterion_id, title, required, set_digest, recorded_at) VALUES (?, 'c1', 'criterion', 1, ?, ?)", relationship, criteria, now)
	f.exec("INSERT INTO verification_mode (relationship_id, mode, recorded_at) VALUES (?,'managed',?)", relationship, now)
	snapshot, _, err := dag.SnapshotAt(context.Background(), f.store.Q(context.Background()), branchTestPlan, 0)
	if err != nil {
		f.t.Fatalf("fixture snapshot: %v", err)
	}
	var planNode dag.SnapNode
	for _, candidate := range snapshot.Nodes {
		if candidate.NodeID == node {
			planNode = candidate
		}
	}
	criteria = planNode.CriteriaSetDigest
	manifest := f.putManifest(node, criteria, planNode.SliceDigest, revision)
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind) VALUES (?,?,?,1,?,'initial')",
		branchTestPlan, node, relationship, manifest)
	// The acceptance's id is its own digest (dagsched AcceptanceDigest), which is what the relay's
	// standing check recomputes: a row whose id is not that digest is a tampered acceptance and opens
	// nothing, whatever the rest of the row says.
	acceptance := dagsched.Acceptance{PlanID: branchTestPlan, NodeID: node, ManifestDigest: manifest, RelationshipID: relationship,
		ExecutionGeneration: 1, EventID: event, RevisionHash: revision, CriteriaSetDigest: criteria, Verdict: "verified",
		HeadSHA: "head-" + node, Repository: branchTestRepo, PRNumber: 1, EvidenceDigest: "evidence-" + node,
		AckTier: "host_read", VerdictTurnID: "verdict-turn", RuleVersionJSON: "{}", AcceptedByTask: "task-parent",
		AcceptedAt: now, State: "active"}
	acceptance.AcceptanceID = dagsched.AcceptanceDigest(acceptance)
	f.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, repository, pr_number, evidence_digest, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state) VALUES (?,?,?,?,?,1,?,?,?, 'verified', ?, ?, 1, ?, 'host_read','verdict-turn','{}','task-parent',0,?, 'active')",
		acceptance.AcceptanceID, branchTestPlan, node, manifest, relationship, event, revision, criteria, acceptance.HeadSHA, branchTestRepo, acceptance.EvidenceDigest, now)
	// an artifact edge leaving an implementation node pins the accepted head, so the relay's reading
	// asks for the pull request the head came from: the forge row is what a pinned acceptance needs.
	f.exec("INSERT INTO dag_acceptance_forge (acceptance_id, forge_repository, pr_number) VALUES (?,?,1)",
		acceptance.AcceptanceID, branchTestRepo)
	return f
}

// capacityLimit declares one enforced execution limit of the fixture's project, so the relay's
// readiness can defer a candidate for want of a slot instead of counting it ready.
func (f *branchFixture) capacityLimit(dimension string, ceiling float64) *branchFixture {
	f.t.Helper()
	if err := f.store.DeclareExecutionLimit(context.Background(), store.ExecutionLimitsRow{
		LimitID: "limit-" + dimension, ScopeKind: "project", ScopeKey: branchTestProject, Dimension: dimension, Unit: dimension,
		Ceiling: ceiling, Enforce: 1, DeclaredBy: "task-parent", Source: "test", Revision: 1,
		DeclaredAt: branchTestStamp(0), UpdatedAt: branchTestStamp(0)}); err != nil {
		f.t.Fatalf("fixture execution limit: %v", err)
	}
	return f
}

// heldSlots records n held execution slots of the fixture's project, as a release would have reserved
// them, so an enforced ceiling leaves no free slot and the relay defers every candidate for capacity.
func (f *branchFixture) heldSlots(n int) *branchFixture {
	f.t.Helper()
	for i := 0; i < n; i++ {
		if err := f.store.InsertExecutionSlot(context.Background(), store.ExecutionSlotsRow{
			SlotID: "slot-" + branchItoa(i), SubjectKind: "dag_node", SubjectKey: "subject-" + branchItoa(i),
			ParentTaskID: "task-parent", ProjectKey: branchTestProject, Tenure: 1, State: "held",
			ReservedBy: "task-parent", ReservedAt: branchTestStamp(0)}); err != nil {
			f.t.Fatalf("fixture execution slot: %v", err)
		}
	}
	return f
}

// putManifest stores the input manifest a node consumes, which the relay's reading needs beside the
// acceptance: its digest is the row the acceptance and the execution both name.
func (f *branchFixture) putManifest(node, criteria, slice, revision string) string {
	f.t.Helper()
	body := map[string]any{
		"schema": dag.SchemaManifest, "node_id": node, "issue_key": criteria, "node_slice_digest": slice,
		"criteria_set_digest": criteria, "inputs": []any{}, "rule_version": map[string]any{"model": "m"},
		"plan_revision_no": 1, "coordinator_epoch": 0, "created_by_task_id": "task-parent", "created_at": branchTestStamp(0),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	digest, err := (&dag.Repo{Store: f.store}).PutManifest(context.Background(), raw)
	if err != nil {
		f.t.Fatalf("fixture manifest for %s: %v", node, err)
	}
	return digest
}

// branchJSONString is one string as JSON, for the receipt and the artifact roots.
func branchJSONString(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		return `"` + s + `"`
	}
	return string(raw)
}

// ensurePlan names the revision the next node or edge belongs to, creating the first revision when
// the test named none.
func (f *branchFixture) ensurePlan() int {
	if len(f.revisions) == 0 {
		f.revisions = append(f.revisions, branchRevisionFixture{at: branchTestStamp(0)})
	}
	return len(f.revisions) - 1
}

// revision names the revision the next node, edge or lifecycle change belongs to, so a test can
// describe a later revision and let a seam write it while a reading is in flight.
func (f *branchFixture) revision(rev int) {
	for len(f.revisions) < rev {
		f.revisions = append(f.revisions, branchRevisionFixture{at: branchTestStamp(0)})
	}
}

// change appends one change to the newest revision the test named. A revision the repository has
// already been given cannot take another change (the writer appends revisions, it never edits one),
// so the fixture refuses rather than describing a plan nobody writes.
func (f *branchFixture) change(c dag.Change) {
	f.t.Helper()
	i := f.ensurePlan()
	if i < f.written {
		f.t.Fatalf("the fixture describes a change of revision %d, which the repository has already been given", i+1)
	}
	f.revisions[i].changes = append(f.revisions[i].changes, c)
}

// node adds one live node of the plan at the newest revision the test named.
func (f *branchFixture) node(id, issue string) *branchFixture {
	f.t.Helper()
	kind := dag.NodeImplementation
	if f.kind == dag.EdgeDecision {
		// a decision edge leaves a non_pr node, and the writer refuses one that leaves an
		// implementation node
		kind = dag.NodeNonPR
	}
	f.change(dag.Change{Op: dag.OpAddNode, Node: &dag.Node{
		NodeID: id, IssueKey: issue, Kind: kind, CriteriaSetDigest: testsupport.Dig("criteria " + id)}})
	f.nodes[id] = issue
	return f
}

// edge adds one live edge of the plan at the newest revision the test named.
func (f *branchFixture) edge(id, from, to string) *branchFixture {
	f.t.Helper()
	kind := f.kind
	if kind == "" {
		kind = dag.EdgeArtifactVerified
	}
	edge := &dag.Edge{EdgeID: id, FromNodeID: from, ToNodeID: to, Kind: kind}
	switch kind {
	case dag.EdgeDecision:
		edge.DecisionSubject, edge.DecisionDigest, edge.RequiredAuthority = "subject", testsupport.Dig("decision "+id), []string{"owner"}
	default:
		edge.TargetRepository, edge.TargetBaseRef = branchTestRepo, "dev"
		// an artifact_verified edge leaving an implementation node consumes a code artifact, so it
		// must pin the verified head
		edge.PinsCodeHead = kind == dag.EdgeArtifactVerified
	}
	f.change(dag.Change{Op: dag.OpAddEdge, Edge: edge})
	return f
}

// integratedEdge describes one integrated edge from a node to a named target: the target is one the
// node's accepted head has to land in.
func (f *branchFixture) integratedEdge(id, from, to, baseRef string) *branchFixture {
	f.t.Helper()
	f.change(dag.Change{Op: dag.OpAddEdge, Edge: &dag.Edge{EdgeID: id, FromNodeID: from, ToNodeID: to,
		Kind: dag.EdgeIntegrated, TargetRepository: branchTestRepo, TargetBaseRef: baseRef}})
	return f
}

// retireEdge takes one edge out of the plan at the newest revision the test named.
func (f *branchFixture) retireEdge(id string) *branchFixture {
	f.t.Helper()
	f.change(dag.Change{Op: dag.OpRetireEdge, EdgeID: id})
	return f
}

// cancelNode cancels one node of the plan at the newest revision the test named: a cancelled node is
// not a live node.
func (f *branchFixture) cancelNode(id string) *branchFixture {
	f.t.Helper()
	f.change(dag.Change{Op: dag.OpCancelNode, NodeID: id})
	return f
}

// pauseNode pauses one node of the plan at the newest revision the test named: a paused node is
// still live.
func (f *branchFixture) pauseNode(id string) *branchFixture {
	f.t.Helper()
	f.change(dag.Change{Op: dag.OpPauseNode, NodeID: id})
	return f
}

// writePlan puts every revision the test described that the repository has not been given yet.
func (f *branchFixture) writePlan() {
	f.t.Helper()
	if f.store == nil {
		return
	}
	for f.written < len(f.revisions) {
		i := f.written
		at := f.revisions[i].at
		repo := &dag.Repo{Store: f.store, Now: func() string { return at }}
		rev := dag.Revision{PlanID: branchTestPlan, ProjectKey: branchTestProject,
			RequestID: fmt.Sprintf("%s-request-%d", branchTestPlan, i+1), ExpectedParent: int64(i),
			AuthorTaskID: "task-parent", Changes: f.revisions[i].changes}
		if _, err := repo.Put(context.Background(), rev); err != nil {
			f.t.Fatalf("put revision %d of %s: %v", i+1, branchTestPlan, err)
		}
		f.written = i + 1
	}
}

// nodeIDFor is the node id of the plan node implementing an issue key, so a fixture that names its
// ready set by issue key still hands the fake relay the node ids the reading judges readiness by.
func (f *branchFixture) nodeIDFor(key string) string {
	ids := make([]string, 0, len(f.nodes))
	for id := range f.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if f.nodes[id] == key {
			return id
		}
	}
	return "n" + key
}

// region declares one edit region of a node. The stated hold is what a declaration of this build
// writes, so the stored hold reads as the declarer's word, exactly as the scheduler reads it.
func (f *branchFixture) region(node, path, kind, key, change string, stated bool) *branchFixture {
	f.t.Helper()
	f.ensurePlan()
	f.exec("INSERT INTO dag_node_regions (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key, change, exclusive, declared_by, declared_at) VALUES (?,?,1,?,?,?,?,?,0,?,?)",
		branchTestPlan, node, branchTestRepo, path, kind, key, change, "task-parent", branchTestStamp(0))
	f.exec("INSERT INTO dag_node_region_holds (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key, stated) VALUES (?,?,1,?,?,?,?,?)",
		branchTestPlan, node, branchTestRepo, path, kind, key, stated)
	return f
}

// release records that one node was released for execution.
func (f *branchFixture) release(node string) *branchFixture {
	f.exec("INSERT INTO dag_releases (plan_id, node_id, manifest_digest, managed_request_id, coordinator_epoch, decided_at) VALUES (?,?,?,?,0,?)",
		branchTestPlan, node, "manifest-"+node, "request-"+node, branchTestStamp(0))
	return f
}

// executed records that one node was run by the relationship its acceptance names: the execution row
// is what the relay's integration judgement walks, so a node without one is not integrated whatever
// its observations say.
func (f *branchFixture) executed(node string) *branchFixture {
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind) VALUES (?,?,?,1,?, 'initial')",
		branchTestPlan, node, "rel-"+node, "manifest-"+node)
	return f
}

// integrated records a landed node: the relationship ran it, its accepted head is marked merged on
// the acceptance's own mark, and that head is observed contained in the base. The three are what the
// relay's integration judgement needs, so the node is not live.
func (f *branchFixture) integrated(node string) *branchFixture {
	f.t.Helper()
	f.accepted(node)
	f.executed(node)
	f.mergedMark("rel-" + node)
	f.observation(node, 1, true)
	return f
}

// accepted records the acceptance of one node, with the head it accepted and without an observation.
func (f *branchFixture) accepted(node string) *branchFixture {
	f.t.Helper()
	f.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state) VALUES (?,?,?,?,?,1,?,?,?,?,?,?,?,?,?,0,?,?)",
		"acceptance-"+node, branchTestPlan, node, "manifest-"+node, "rel-"+node, "event", "revision", "criteria",
		"verified", "head-"+node, "verified", "turn", "{}", "task-parent", branchTestStamp(0), "active")
	return f
}

// mergedMark records the parent's merged mark on the mark the acceptance carries (event, generation
// 1, revision), which is half of what the integration judgement needs beside a contained
// observation.
func (f *branchFixture) mergedMark(relationship string) *branchFixture {
	f.exec("INSERT OR IGNORE INTO assignment_marks (relationship_id, mark, event_id, execution_generation, revision_hash, evidence, actor, marked_at) VALUES (?, 'merged', 'event', 1, 'revision', 'merged', 'parent', ?)",
		relationship, branchTestStamp(0))
	return f
}

// observation records one integration observation of a node's acceptance in the default target
// (owner/repo#dev), of the head the acceptance accepted, at one sequence number.
func (f *branchFixture) observation(node string, seq int, ancestor bool) *branchFixture {
	f.t.Helper()
	f.exec("INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, observed_seq, reverted_by, observed_at) VALUES (?,?,?,?,?,?,?,?,?,NULL,?)",
		"observation-"+node+"-"+branchItoa(seq), "acceptance-"+node, branchTestRepo, "dev", "head-"+node, "tip",
		dagReviewFlag(ancestor), "ancestry", seq, branchTestStamp(0))
	return f
}

// publish writes the relay answer and the state the judgement reads, replaces the seams, and
// releases the store writer so the read-only open under test sees a settled file.
func (f *branchFixture) publish() *branchFixture {
	f.publishOpen()
	f.close()
	return f
}

// publishOpen writes the relay answer and the state the judgement reads and replaces the seams, but
// leaves the writer open, so a test can commit a revision while the reading is in flight.
func (f *branchFixture) publishOpen() *branchFixture {
	f.t.Helper()
	branchWriteReady(f.t, f, filepath.Join(f.dir, "ready.json"), f.ready, "within", f.passRevision)
	script := "#!/bin/sh\ncase \"$*\" in\n" +
		"  *dag-ready*) cat " + coreShellQuote(filepath.Join(f.dir, "ready.json")) + " ;;\n" +
		"  *) echo refused >&2; exit 2 ;;\nesac\n"
	syscall.ForkLock.RLock()
	writeErr := os.WriteFile(f.env.Executable, []byte(script), 0o700)
	syscall.ForkLock.RUnlock()
	if writeErr != nil {
		f.t.Fatal(writeErr)
	}
	waiting := f.ready
	if f.waiting != nil {
		waiting = f.waiting
	}
	state := map[string]any{"plans": map[string]any{branchTestPlan: map[string]any{
		"since": float64(branchTestNow.Add(-60 * time.Minute).Unix()), "waiting": waiting}}}
	branchWriteJSON(f.t, filepath.Join(f.stateDir, capacityStateFile), state)
	branchSeams(f.t)
	return f
}

// branchWriteReady writes one dag-ready answer whose ready set is the given issue keys. revision is
// the plan revision the answer names; 0 leaves the key out, which a reading takes as the head.
func branchWriteReady(t *testing.T, f *branchFixture, path string, keys []string, hostMemory string, revision int64) {
	t.Helper()
	ready := make([]any, len(keys))
	for i, key := range keys {
		ready[i] = map[string]any{"node_id": f.nodeIDFor(key), "issue_key": key, "disposition": "ready", "reason": nil}
	}
	answer := map[string]any{"ok": true, "schema": "dag-ready/1",
		"pass":  map[string]any{"free_slots": 0, "ceiling": 12, "held": 12, "host_memory": map[string]any{"state": hostMemory}},
		"ready": ready, "nodes": []any{}}
	if revision != 0 {
		answer["plan_revision"] = revision
	}
	branchWriteJSON(t, path, answer)
}

// branchSeams replaces the gh and status-page seams for one test: no merges and no incident, so the
// judgement rests on the waiting set the fixture states.
func branchSeams(t *testing.T) {
	t.Helper()
	exec, get := capacityExec, capacityHTTPGet
	t.Cleanup(func() { capacityExec, capacityHTTPGet = exec, get })
	capacityExec = func(context.Context, string, ...string) ([]byte, error) { return []byte("[]"), nil }
	capacityHTTPGet = func(context.Context, string) ([]byte, error) { return []byte(`{"incidents":[]}`), nil }
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

// A plan with nothing detachable carries an empty list, and a hold plan carries no key at all:
// the two are different answers and the document must keep them apart, because a reader cannot
// tell "nobody looked" from "nothing was there" if both read as the same JSON.
func TestBranchCandidatesKeepTheOmittedListApartFromTheEmptyOne(t *testing.T) {
	document := func(t *testing.T, plan CapacityPlan) map[string]any {
		t.Helper()
		data, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}

	// An expand_candidate plan whose one component is the whole plan: looked at, nothing detachable.
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.node("A", "CRW-1").node("B", "CRW-2")
	f.edge("e1", "A", "B")
	f.region("A", "a.go", "file", "", "edit", false)
	f.region("B", "b.go", "file", "", "edit", false)
	f.publish()
	plan := f.run()
	if plan.Verdict != capacityExpand {
		t.Fatalf("the plan is %s, want %s", plan.Verdict, capacityExpand)
	}
	doc := document(t, plan)
	empty, ok := doc["branches"].([]any)
	if !ok || len(empty) != 0 {
		t.Fatalf("branches = %v, want an empty list rather than no key", doc["branches"])
	}

	// The same plan held back by the host memory bound carries no key at all.
	branchWriteReady(t, f, filepath.Join(f.dir, "ready.json"), f.ready, "deferring", 0)
	hold := f.run()
	if hold.Verdict != capacityHold {
		t.Fatalf("the plan is %s, want %s", hold.Verdict, capacityHold)
	}
	if _, present := document(t, hold)["branches"]; present {
		t.Fatal("a hold plan without --branches-always carries a branches key")
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

// The thresholds are validated on the hold path too: a malformed section is refused whether or not
// this plan carries candidates, so a configuration mistake never hides behind a transient verdict.
func TestBranchCandidatesRefuseABadThresholdOnAHoldPlan(t *testing.T) {
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.node("A", "CRW-1").node("B", "CRW-2")
	f.region("A", "a.go", "file", "", "edit", false)
	f.region("B", "b.go", "file", "", "edit", false)
	f.publish()
	// The host memory bound makes the verdict hold, which is the path that skips the candidates.
	branchWriteReady(t, f, filepath.Join(f.dir, "ready.json"), []string{"CRW-1", "CRW-2"}, "deferring", 0)
	f.section["min_branch_nodes"] = "two"
	f.load()
	config := capacityConfig
	t.Cleanup(func() { capacityConfig = config })
	capacityConfig = func(*Env) *Config { return f.cfg }
	_, err := Capacity(context.Background(), f.env, f.cfg, false)
	if err == nil {
		t.Fatal("a hold plan accepted a min_branch_nodes that is not a number")
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

// A node the relay defers for want of a slot is part of the waiting set, so it counts in ready_count
// and reads as waiting. The run slot is held, so A, a root, is deferred for want of capacity. B waits
// on A's edge and is not in the waiting set; C is a root of its own and stays a bundle of one.
func TestBranchCandidatesCountADeferredNodeAsWaiting(t *testing.T) {
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.capacityLimit("runs", 1)
	f.heldSlots(1)
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3")
	f.edge("e1", "A", "B")
	for _, node := range []string{"A", "B", "C"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	plan := f.publish().run()
	candidates := branchList(t, plan)
	if len(candidates) != 1 {
		t.Fatalf("branches = %v, want the one bundle", branchSummaries(candidates))
	}
	if candidates[0].ReadyCount != 1 {
		t.Fatalf("ready_count = %d, want 1: A is deferred for want of capacity and counts as waiting", candidates[0].ReadyCount)
	}
	for _, node := range candidates[0].Nodes {
		if want := node.NodeID == "A"; node.Ready != want {
			t.Fatalf("node %s Ready = %v, want %v: only the deferred root is in the waiting set", node.NodeID, node.Ready, want)
		}
	}
}

// Every edge kind connects: the plan's edges are read whatever kind they are, so a bundle joined
// by an integrated or a decision edge is one bundle.
func TestBranchCandidatesConnectWhateverTheEdgeKind(t *testing.T) {
	// B waits on A's edge in the artifact and integrated kinds, and defers for want of an authority in the
	// decision kind, so only A reads ready in every kind. C is a root of its own.
	for _, kind := range []string{"artifact_verified", "integrated", "decision"} {
		t.Run(kind, func(t *testing.T) {
			f := branchNewFixture(t, "CRW-1", "CRW-2")
			f.kind = kind
			f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3")
			f.edge("e1", "A", "B")
			for _, node := range []string{"A", "B", "C"} {
				f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
			}
			plan := f.publish().run()
			branchWant(t, branchSummaries(branchList(t, plan)), "A+B pkg/A.go,pkg/B.go ready=1 edges=1")
		})
	}
}

// C1: a bundle joined only by an edge is a candidate, and the graph's two bundles are two of them.
func TestBranchCandidatesConnectByEdgesOnly(t *testing.T) {
	// Ready: A and C, the roots. B waits on e1 and D waits on e2, so each bundle has one ready node.
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("D", "CRW-4")
	f.edge("e1", "A", "B").edge("e2", "C", "D")
	for _, node := range []string{"A", "B", "C", "D"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	plan := f.publish().run()
	branchWant(t, branchSummaries(branchList(t, plan)),
		"A+B pkg/A.go,pkg/B.go ready=1 edges=1", "C+D pkg/C.go,pkg/D.go ready=1 edges=1")
}

// C1: a bundle joined only by overlapping regions is a candidate; regions in different places leave
// two of them.
func TestBranchCandidatesConnectByRegionsOnly(t *testing.T) {
	// B and D defer for an edit overlap with A and C, which are the roots. Each bundle has one ready node.
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("D", "CRW-4")
	f.region("A", "internal/pkg", "tree", "", "edit", false)
	f.region("B", "internal/pkg/x.go", "file", "", "edit", false)
	f.region("C", "cmd/svc", "tree", "", "edit", false)
	f.region("D", "cmd/svc/d.go", "file", "", "edit", false)
	plan := f.publish().run()
	branchWant(t, branchSummaries(branchList(t, plan)),
		"A+B internal/pkg,internal/pkg/x.go ready=1 edges=0", "C+D cmd/svc,cmd/svc/d.go ready=1 edges=0")
}

// C1: a node that declared no region joins every live node, so the same graph becomes one component
// and nothing can be taken out.
func TestBranchCandidatesUndeclaredNodeJoinsEveryLiveNode(t *testing.T) {
	// Ready: A and C, the roots of the two edges. B waits on e1 and D waits on e2. E is a root of its own.
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
	branchWant(t, declared, "A+B pkg/A.go,pkg/B.go ready=1 edges=1", "C+D pkg/C.go,pkg/D.go ready=1 edges=1")
	if got := branchSummaries(branchList(t, build(true).publish().run())); len(got) != 0 {
		t.Fatalf("with E undeclared, branches = %v, want none: E joins every live node, so the one component is the whole plan", got)
	}
}

// C1: a bundle that holds a released node is not a candidate.
func TestBranchCandidatesExcludeABundleWithAReleasedNode(t *testing.T) {
	// B waits on A's edge, so A is the only ready node of its bundle. C is released, which takes C+D out.
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("D", "CRW-4").node("E", "CRW-5")
	f.edge("e1", "A", "B").edge("e2", "C", "D")
	for _, node := range []string{"A", "B", "C", "D", "E"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	f.release("C")
	got := branchSummaries(branchList(t, f.publish().run()))
	branchWant(t, got, "A+B pkg/A.go,pkg/B.go ready=1 edges=1")
}

// C1: an integrated node is not live, so an edge into it joins nothing and its neighbour stands
// alone, below the floor.
func TestBranchCandidatesDoNotConnectThroughAnIntegratedNode(t *testing.T) {
	// C is integrated and leaves the plan. B waits on A's edge, so only A reads ready.
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("D", "CRW-4")
	f.edge("e1", "A", "B").edge("e2", "C", "D")
	for _, node := range []string{"A", "B", "C", "D"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	f.integrated("C")
	got := branchSummaries(branchList(t, f.publish().run()))
	branchWant(t, got, "A+B pkg/A.go,pkg/B.go ready=1 edges=1")
}

// An integration that a later observation contradicts does not integrate: the node stays live, so
// the edge it carries still joins. This is the scheduler's own rule (a positive observation with no
// later negative one), not merely "any ancestor observation ever recorded". C joins D, which joins
// E, so a C that stays live keeps D and E inside its component and D and E never become a bundle of
// their own.
func TestBranchCandidatesKeepANodeWhoseIntegrationWasSuperseded(t *testing.T) {
	// B waits on A's edge, so only A reads ready. C stays live after its superseded integration.
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("D", "CRW-4").node("E", "CRW-5")
	f.edge("e1", "A", "B").edge("e2", "C", "D").edge("e3", "D", "E")
	for _, node := range []string{"A", "B", "C", "D", "E"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	// C was observed contained and then, later, observed not contained: it is live again.
	f.integrated("C")
	f.observation("C", 2, false)
	got := branchSummaries(branchList(t, f.publish().run()))
	// If C read as integrated it would leave the graph and D+E would be a bundle of its own; C stays
	// live, so A+B is the only candidate.
	branchWant(t, got, "A+B pkg/A.go,pkg/B.go ready=1 edges=1")
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
	// Each bundle has one ready node: A, C, F and H are roots and B, D, E, G and I wait on their edges.
	// The cap of three drops {H,I}, which ranks last.
	f := branchNewFixture(t, "CRW-1", "CRW-2", "CRW-3", "CRW-4")
	// four bundles: {A,B} with A ready, {C,D,E} with C ready, {F,G} with F ready, {H,I} with H ready.
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
		"C+D+E pkg/C.go,pkg/D.go,pkg/E.go ready=1 edges=2",
		"A+B pkg/A.go,pkg/B.go ready=1 edges=1",
		"F+G pkg/F.go,pkg/G.go ready=1 edges=1")
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
	branchWriteReady(t, f, filepath.Join(f.dir, "ready.json"), []string{"CRW-1"}, "deferring", 0)
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
	// C+D+E+F has C ready and the rest waiting on their edges. A+B has A ready and B waiting on e1.
	// Both bundles hold one ready node, so the node count ranks the larger one first and
	// first; the floor then drops the smaller one and the cap keeps only what is left.
	f := branchNewFixture(t, "CRW-3", "CRW-4")
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("D", "CRW-4").node("E", "CRW-5").node("F", "CRW-6")
	f.edge("e1", "A", "B").edge("e2", "C", "D").edge("e3", "D", "E").edge("e4", "E", "F")
	for _, node := range []string{"A", "B", "C", "D", "E", "F"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	f.publish()
	branchWant(t, branchSummaries(branchList(t, f.run())),
		"C+D+E+F pkg/C.go,pkg/D.go,pkg/E.go,pkg/F.go ready=1 edges=3",
		"A+B pkg/A.go,pkg/B.go ready=1 edges=1")
	f.section["min_branch_nodes"] = 3
	f.load()
	branchWant(t, branchSummaries(branchList(t, f.run())),
		"C+D+E+F pkg/C.go,pkg/D.go,pkg/E.go,pkg/F.go ready=1 edges=3")
	f.section["max_branches"] = 1
	f.load()
	branchWant(t, branchSummaries(branchList(t, f.run())),
		"C+D+E+F pkg/C.go,pkg/D.go,pkg/E.go,pkg/F.go ready=1 edges=3")
}

// The reading takes readiness from its own snapshot. The earlier dag-ready pass answered for the same
// plan revision and listed X and Y as ready, and B's only predecessor A is integrated just before the
// reading takes its snapshot. The revision does not move, so a reading that kept the pass's answer
// would keep B waiting. B and C form one bundle with B ready, and X and Y form another with X ready.
func TestBranchCandidatesReadyFromTheSnapshotNotTheEarlierPass(t *testing.T) {
	f := branchNewFixture(t, "CRW-4", "CRW-5")
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("X", "CRW-4").node("Y", "CRW-5")
	f.integratedEdge("e1", "A", "B", "dev")
	f.edge("e2", "B", "C").edge("e3", "X", "Y")
	for _, node := range []string{"A", "B", "C", "X", "Y"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	f.readyNode("A")
	f.exec("INSERT OR IGNORE INTO assignment_marks (relationship_id, mark, event_id, execution_generation, revision_hash, evidence, actor, marked_at) SELECT relationship_id, 'merged', event_id, 1, revision_hash, 'merged', 'parent', '2026-10-07T09:00:00.000000+00:00' FROM dag_acceptances WHERE node_id = 'A' AND plan_id = 'p-branch'")
	f.passRevision = 1
	f.publishOpen()
	previous := branchPassSeam
	branchPassSeam = func() {
		f.exec("INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, observed_seq, reverted_by, observed_at) SELECT 'observation-A-1', acceptance_id, 'owner/repo', 'dev', head_sha, 'tip', 1, 'ancestry', 1, NULL, '2026-10-07T09:00:00.000000+00:00' FROM dag_acceptances WHERE node_id = 'A' AND plan_id = 'p-branch'")
	}
	t.Cleanup(func() { branchPassSeam = previous })
	plan := f.run()
	branchWant(t, branchSummaries(branchList(t, plan)),
		"B+C pkg/B.go,pkg/C.go ready=1 edges=1", "X+Y pkg/X.go,pkg/Y.go ready=1 edges=1")
}
