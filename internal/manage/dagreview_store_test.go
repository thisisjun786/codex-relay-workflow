package manage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	// The fixture builds a real store, so this test binary links internal/relay/store and must
	// also link internal/testsupport: that package refuses a database below a live relay state
	// directory before any TestMain runs, so a test that forgot isolation is refused rather than
	// reading the operator's live state (internal/testsupport/livestate_test.go).
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The fixture clock: every row this file writes is placed relative to one base instant, so a
// threshold test reads as "older than the stall" rather than as an absolute date.
const dagReviewBaseInstant = "2026-10-06T00:00:00.000000+00:00"

// dagReviewNow is the instant every fixture test reviews at: one hour after the base.
func dagReviewNow() time.Time {
	base, err := time.Parse(time.RFC3339Nano, dagReviewBaseInstant)
	if err != nil {
		panic(err)
	}
	return base.Add(time.Hour)
}

// dagReviewAt is the fixture instant minutes after the base, in the relay's own timestamp form.
func dagReviewAt(minutes int) string {
	return dagReviewNow().Add(time.Duration(minutes-60) * time.Minute).UTC().Format("2006-01-02T15:04:05.000000+00:00")
}

// dagReviewFixture is a temporary relay store built row by row. It is created through the
// exported store schema, so the fixture has the real store shape and internal/relay is never
// edited to make a test possible.
//
// A plan is described as a header, its revisions, its nodes and its edges, and written through the
// DAG repository (dag.Repo.Put) when the fixture closes: the scheduler's own reading verifies every
// node's slice digest and the plan's state digest against the revision log, so a review that reads
// a plan through the scheduler needs a plan that reads back. Every other row is inserted directly.
type dagReviewFixture struct {
	t     *testing.T
	dir   string
	path  string
	store *store.Store
	plans map[string]*dagReviewPlanFixture
	order []string
	// written is set by the first writePlans, so a test that needs raw plan rows afterwards can
	// write them itself and the close does not write the plan a second time.
	written bool
}

// dagReviewPlanFixture is one plan a test described: its header and its revisions in order.
type dagReviewPlanFixture struct {
	planID, project string
	createdAt       string
	revisions       []dagReviewRevisionFixture
}

// dagReviewRevisionFixture is one revision of a described plan: the instant it is recorded at and
// the changes it carries.
type dagReviewRevisionFixture struct {
	at      string
	changes []dag.Change
}

func dagReviewNewFixture(t *testing.T) *dagReviewFixture {
	t.Helper()
	coreTempHome(t)
	dir := t.TempDir()
	path := filepath.Join(dir, dagReviewStoreFile)
	st, err := store.Open(context.Background(), path, filepath.Join(dir, "app-server-control.sock"))
	if err != nil {
		t.Fatalf("create the fixture store: %v", err)
	}
	f := &dagReviewFixture{t: t, dir: dir, path: path, store: st, plans: map[string]*dagReviewPlanFixture{}}
	t.Cleanup(f.close)
	return f
}

func (f *dagReviewFixture) exec(query string, args ...any) {
	f.t.Helper()
	// A row that names a plan or one of its nodes needs the plan written first, and the DAG
	// repository is the only writer that leaves rows the scheduler's reading accepts. Every raw
	// insert therefore materializes the described plans before it runs.
	f.writePlans()
	if _, err := f.store.DB.Exec(query, args...); err != nil {
		f.t.Fatalf("fixture insert: %v\n%s", err, query)
	}
}

// close writes every plan the test described and releases the writer, so the read-only review
// under test sees a settled file.
func (f *dagReviewFixture) close() {
	f.t.Helper()
	if f.store == nil {
		return
	}
	f.writePlans()
	if err := f.store.Close(); err != nil {
		f.t.Fatalf("close the fixture store: %v", err)
	}
	f.store = nil
}

func dagReviewFlag(value bool) int {
	if value {
		return 1
	}
	return 0
}

func dagReviewNull(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// planFixture is the described plan, created on first mention.
func (f *dagReviewFixture) planFixture(planID string) *dagReviewPlanFixture {
	if p, seen := f.plans[planID]; seen {
		return p
	}
	p := &dagReviewPlanFixture{planID: planID, project: "project-1", createdAt: dagReviewAt(0)}
	f.plans[planID] = p
	f.order = append(f.order, planID)
	return p
}

func (f *dagReviewFixture) plan(planID, project string) {
	f.planFixture(planID).project = project
}

// revision records that the plan has a revision at the given instant. A node or an edge added
// afterwards goes into the newest revision the test named, which is how the fixture reads: a test
// that adds a node after naming revision 2 is adding a node of revision 2.
func (f *dagReviewFixture) revision(planID string, rev int, at string) {
	p := f.planFixture(planID)
	for len(p.revisions) < rev {
		p.revisions = append(p.revisions, dagReviewRevisionFixture{at: at})
	}
	p.revisions[rev-1].at = at
}

// revisionIndex is the newest revision the test named, creating revision 1 when it named none.
func (f *dagReviewFixture) revisionIndex(planID string) int {
	p := f.planFixture(planID)
	if len(p.revisions) == 0 {
		p.revisions = append(p.revisions, dagReviewRevisionFixture{at: dagReviewAt(0)})
	}
	return len(p.revisions) - 1
}

func (f *dagReviewFixture) node(planID, nodeID, issue string) {
	p := f.planFixture(planID)
	i := f.revisionIndex(planID)
	p.revisions[i].changes = append(p.revisions[i].changes, dag.Change{Op: dag.OpAddNode, Node: &dag.Node{
		NodeID: nodeID, IssueKey: issue, Kind: dag.NodeImplementation, CriteriaSetDigest: testsupport.Dig("criteria " + nodeID)}})
}

// retire adds the change that takes a node out of the plan at the named revision.
func (f *dagReviewFixture) retire(planID, nodeID string, rev int) {
	p := f.planFixture(planID)
	p.revisions[rev-1].changes = append(p.revisions[rev-1].changes, dag.Change{Op: dag.OpRetireNode, NodeID: nodeID})
}

// pauseNode adds the change that pauses one node at the named revision. A node the plan paused
// before it was ever released has no release and no execution, so the reading gives it the paused
// stage while it owns nothing.
func (f *dagReviewFixture) pauseNode(planID, nodeID string, rev int) {
	p := f.planFixture(planID)
	p.revisions[rev-1].changes = append(p.revisions[rev-1].changes, dag.Change{Op: dag.OpPauseNode, NodeID: nodeID})
}

func (f *dagReviewFixture) edge(planID, edgeID, from, to, kind string, introducedRev int) {
	f.addEdge(planID, edgeID, from, to, kind, introducedRev, "", "")
}

// addEdge adds one edge to the named revision. An integrated edge names the target it orders; the
// other kinds carry none, because only an integrated edge hands a result to a branch.
func (f *dagReviewFixture) addEdge(planID, edgeID, from, to, kind string, introducedRev int, repository, baseRef string) {
	p := f.planFixture(planID)
	for len(p.revisions) < introducedRev {
		p.revisions = append(p.revisions, dagReviewRevisionFixture{at: dagReviewAt(0)})
	}
	edge := &dag.Edge{EdgeID: edgeID, FromNodeID: from, ToNodeID: to, Kind: kind}
	if kind == dag.EdgeIntegrated {
		edge.TargetRepository, edge.TargetBaseRef = "owner/repo", "dev"
	}
	if repository != "" {
		edge.TargetRepository, edge.TargetBaseRef = repository, baseRef
	}
	p.revisions[introducedRev-1].changes = append(p.revisions[introducedRev-1].changes, dag.Change{Op: dag.OpAddEdge, Edge: edge})
}

// writePlans puts every plan the test described through the DAG repository, then moves the plan
// header and each revision onto the fixture clock. The repository stamps them with the wall clock,
// and a threshold test reads as "older than the stall" only against the fixture's base instant.
func (f *dagReviewFixture) writePlans() {
	f.t.Helper()
	if f.written {
		return
	}
	f.written = true
	ctx := context.Background()
	for _, planID := range f.order {
		p := f.plans[planID]
		for i, rv := range p.revisions {
			at := rv.at
			repo := &dag.Repo{Store: f.store, Now: func() string { return at }}
			rev := dag.Revision{PlanID: p.planID, ProjectKey: p.project, RequestID: fmt.Sprintf("%s-request-%d", p.planID, i+1),
				ExpectedParent: int64(i), AuthorTaskID: "task-parent", Changes: rv.changes}
			if _, err := repo.Put(ctx, rev); err != nil {
				f.t.Fatalf("put revision %d of %s: %v", i+1, p.planID, err)
			}
		}
	}
}

func (f *dagReviewFixture) release(planID, nodeID, digest, at string) {
	f.exec("INSERT INTO dag_releases (plan_id, node_id, manifest_digest, managed_request_id, coordinator_epoch, decided_at) VALUES (?,?,?,?,0,?)",
		planID, nodeID, digest, "request-"+digest, at)
}

func (f *dagReviewFixture) acceptance(planID, nodeID, acceptanceID, relationshipID, at string) {
	f.acceptanceHead(planID, nodeID, acceptanceID, relationshipID, "", at)
}

// acceptanceHead records an active acceptance of a node whose accepted head is a commit. The
// scheduler integrates a node only when it has a head, so a test of a landing needs one; the event
// and the revision are the fixed pair the mergedMark helper names.
func (f *dagReviewFixture) acceptanceHead(planID, nodeID, acceptanceID, relationshipID, headSHA, at string) {
	f.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state) VALUES (?,?,?,?,?,1,'event','revision','criteria','verified',?,'verified','turn','{}','task-parent',0,?,'active')",
		acceptanceID, planID, nodeID, "manifest-"+nodeID, relationshipID, dagReviewNull(headSHA), at)
}

// observation records one integration observation of the acceptance in the default target
// (owner/repo#dev). The sequence counts up per acceptance and target, so a later call is a later
// observation of the same target, which is what tells a withdrawn landing from a fresh one.
func (f *dagReviewFixture) observation(acceptanceID, observedAt string, isAncestor bool, revertedBy string) {
	f.observationIn(acceptanceID, "owner/repo", "dev", observedAt, isAncestor, revertedBy)
}

// observationIn records one integration observation in a named target. The subject it observes is
// the acceptance's own accepted head, because that is what the scheduler's integration rule
// compares an observation against; the sequence counts up per acceptance and target, so a later
// call is a later observation of the same target.
func (f *dagReviewFixture) observationIn(acceptanceID, repository, baseRef, observedAt string, isAncestor bool, revertedBy string) {
	f.t.Helper()
	f.writePlans()
	var head string
	if err := f.store.DB.QueryRow("SELECT COALESCE(head_sha, '') FROM dag_acceptances WHERE acceptance_id = ?", acceptanceID).Scan(&head); err != nil {
		f.t.Fatalf("read the accepted head of %s: %v", acceptanceID, err)
	}
	var seq int
	if err := f.store.DB.QueryRow("SELECT COALESCE(MAX(observed_seq), 0) + 1 FROM dag_integration_observations WHERE acceptance_id = ? AND repository = ? AND base_ref = ?",
		acceptanceID, repository, baseRef).Scan(&seq); err != nil {
		f.t.Fatalf("count the observations of %s: %v", acceptanceID, err)
	}
	f.exec("INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, observed_seq, reverted_by, observed_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		fmt.Sprintf("observation-%s-%d", acceptanceID, seq), acceptanceID, repository, baseRef, head, "tip", dagReviewFlag(isAncestor), "ancestry", seq, dagReviewNull(revertedBy), observedAt)
}

// mergedMark records the parent's merged mark on the acceptance's event, generation and revision,
// which is half of what the scheduler's integration rule needs beside a contained observation.
func (f *dagReviewFixture) mergedMark(relationshipID, at string) {
	f.exec("INSERT OR IGNORE INTO assignment_marks (relationship_id, mark, event_id, execution_generation, revision_hash, evidence, actor, marked_at) VALUES (?, 'merged', 'event', 1, 'revision', 'merged', 'parent', ?)",
		relationshipID, at)
}

func (f *dagReviewFixture) region(planID, nodeID, path, kind, key, change string, exclusive bool) {
	f.exec("INSERT INTO dag_node_regions (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key, change, exclusive, declared_by, declared_at) VALUES (?,?,1,'owner/repo',?,?,?,?,?,'task-child',?)",
		planID, nodeID, path, kind, key, change, dagReviewFlag(exclusive), dagReviewAt(0))
}

func (f *dagReviewFixture) grade(planID, nodeID, path, kind, key, grade, rule string) {
	f.exec("INSERT INTO dag_node_region_grades (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key, grade, rule) VALUES (?,?,1,'owner/repo',?,?,?,?,?)",
		planID, nodeID, path, kind, key, grade, rule)
}

func (f *dagReviewFixture) execution(planID, nodeID, relationshipID string) {
	f.executionKind(planID, nodeID, relationshipID, 1, dagReviewExecutionInitial)
}

// executionKind records one execution of a node: the generation it ran and the kind of run it was
// (initial, correction, or a parent_handover from dag-adopt).
func (f *dagReviewFixture) executionKind(planID, nodeID, relationshipID string, generation int, kind string) {
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind) VALUES (?,?,?,?,?,?)",
		planID, nodeID, relationshipID, generation, "manifest", kind)
}

// boundExecution records a node released to an active relationship: the relationship with its
// bound generation (the registry refuses one whose generation the store does not carry), and the
// execution row that binds them. It returns the relationship id.
func (f *dagReviewFixture) boundExecution(planID, nodeID, kind string, generation int) string {
	relationshipID := "relationship-" + nodeID
	f.relayReadRelationship(relationshipID, "CRW-"+nodeID, "active", "parent", "child-"+nodeID, generation)
	f.executionKind(planID, nodeID, relationshipID, generation, kind)
	return relationshipID
}

// closedRelease records a release whose managed start was abandoned and then ended by
// dag-release-close: the intent no longer owns the node, so the scheduler reads it as planned.
func (f *dagReviewFixture) closedRelease(planID, nodeID, digest, at string) {
	f.release(planID, nodeID, digest, at)
	f.exec("INSERT INTO dag_release_recoveries (plan_id, node_id, manifest_digest, abandoned_request_id, action, slot_released, reason, recorded_by, recorded_at) VALUES (?,?,?,?,'closed',1,'the managed start was abandoned','parent',?)",
		planID, nodeID, digest, "request-"+digest, at)
}

// relationship records a live relationship and the generation it stands on. The generation row is
// written with it because the registry refuses a relationship whose generation the store does not
// carry, and the scheduler reads a node's relationship through the registry.
func (f *dagReviewFixture) relationship(relationshipID, issue, createdAt string) {
	f.exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES (?,?,'active','parent','host','child','host',1,'[]','[]',?,?)",
		relationshipID, issue, createdAt, createdAt)
	f.exec("INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at) VALUES (?,1,?,'bound',?,NULL,?,?)",
		relationshipID, "dispatch-"+relationshipID, "turn-"+relationshipID, createdAt, createdAt)
}

func (f *dagReviewFixture) scope(relationshipID, project string) {
	f.exec("INSERT INTO relationship_scope (relationship_id, project_key, recorded_at) VALUES (?,?,?)",
		relationshipID, project, dagReviewAt(0))
}

func (f *dagReviewFixture) laneTurn(turnID, targetKey, holder, state, relationshipID string, prNumber int, heldAt, updatedAt, closedAt string) {
	f.exec("INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, relationship_id, pr_number, candidate_head, declared_ready, state, tenure, requested_at, held_at, closed_at, updated_at) VALUES (?,?,'owner/repo','dev','project',?,'host',?,?,'head',1,?,1,?,?,?,?)",
		turnID, targetKey, holder, relationshipID, prNumber, state, heldAt, heldAt, dagReviewNull(closedAt), updatedAt)
}

// dagReviewEnv is the Env a fixture test runs with: discarded streams and the fixture clock.
func dagReviewEnv(t *testing.T) *Env {
	t.Helper()
	return &Env{
		Stdin:      strings.NewReader(""),
		Stdout:     io.Discard,
		Stderr:     io.Discard,
		Getenv:     os.Getenv,
		Now:        dagReviewNow,
		Executable: "crw",
	}
}

// dagReviewConfig points a review at the fixture state directory, with the dag_review section
// the test asks for.
func dagReviewConfig(t *testing.T, state string, plans []string, stallMinutes int) *Config {
	t.Helper()
	cfg := &Config{Relay: coreRelay{State: state}, raw: map[string]json.RawMessage{}}
	section := map[string]any{}
	if plans != nil {
		section["plans"] = plans
	}
	if stallMinutes != 0 {
		section["stall_minutes"] = stallMinutes
	}
	if len(section) > 0 {
		encoded, err := json.Marshal(section)
		if err != nil {
			t.Fatal(err)
		}
		cfg.raw["dag_review"] = encoded
	}
	return cfg
}

// dagReviewFileState is what a read must leave alone: the file's bytes and its mtime.
func dagReviewFileState(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%s %d %s", hex.EncodeToString(sum[:]), info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano))
}

// C2: reading the store through the product leaves the file's bytes and mtime exactly as they
// were, because the open is mode=ro with query_only and runs no schema statement.
func TestDagReviewReadLeavesTheStoreFileUnchanged(t *testing.T) {
	f := dagReviewNewFixture(t)
	f.plan("plan-1", "project-1")
	f.revision("plan-1", 1, dagReviewAt(0))
	f.node("plan-1", "A", "CRW-1")
	f.release("plan-1", "A", "manifest-1", dagReviewAt(5))
	f.close()

	before := dagReviewFileState(t, f.path)
	review, err := DagReview(context.Background(), dagReviewEnv(t), dagReviewConfig(t, f.dir, nil, 0))
	if err != nil {
		t.Fatalf("DagReview: %v", err)
	}
	// The review must have actually read the store: a no-op would leave the file alone too, so
	// the immutability claim is only meaningful beside a reading that happened.
	if len(review.Plans) != 1 || review.Plans[0].Plan != "plan-1" || review.Plans[0].Released != 1 {
		t.Fatalf("the review did not read the plan: %+v", review.Plans)
	}
	if after := dagReviewFileState(t, f.path); after != before {
		t.Errorf("the read changed the store file:\n before %s\n after  %s", before, after)
	}
}

// A state directory with no store file is a read failure, not an empty review.
func TestDagReviewStoreWithoutAFileIsAnError(t *testing.T) {
	coreTempHome(t)
	dir := t.TempDir()
	if _, err := DagReview(context.Background(), dagReviewEnv(t), dagReviewConfig(t, dir, nil, 0)); err == nil {
		t.Fatal("a state directory with no relay.sqlite3 read as a review")
	}
}

// The stall threshold comes from the section, so a lane inside it is not stalled and one past
// it is.
func TestDagReviewStallThresholdComesFromTheSection(t *testing.T) {
	f := dagReviewNewFixture(t)
	f.plan("plan-1", "project-1")
	f.revision("plan-1", 1, dagReviewAt(0))
	f.node("plan-1", "A", "CRW-A")
	f.laneTurn("turn-1", "owner/repo#dev", "holder-1", "holding", "", 5, dagReviewAt(50), dagReviewAt(50), "")
	f.close()

	if found := dagReviewFind(dagReviewRunReview(t, f, nil, 0), dagReviewKindLaneTurnStalled); len(found) != 0 {
		t.Errorf("a lane inside the default threshold was reported: %+v", found)
	}
	if found := dagReviewFind(dagReviewRunReview(t, f, nil, 5), dagReviewKindLaneTurnStalled); len(found) != 1 {
		t.Errorf("a lane past the configured threshold was not reported: %+v", found)
	}
}
