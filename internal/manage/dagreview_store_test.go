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
	_ "github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
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
type dagReviewFixture struct {
	t     *testing.T
	dir   string
	path  string
	store *store.Store
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
	f := &dagReviewFixture{t: t, dir: dir, path: path, store: st}
	t.Cleanup(f.close)
	return f
}

func (f *dagReviewFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.DB.Exec(query, args...); err != nil {
		f.t.Fatalf("fixture insert: %v\n%s", err, query)
	}
}

// close releases the writer, so the read-only review under test sees a settled file.
func (f *dagReviewFixture) close() {
	f.t.Helper()
	if f.store == nil {
		return
	}
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

func (f *dagReviewFixture) plan(planID, project string) {
	f.exec("INSERT INTO dag_plans (plan_id, project_key, created_by_task_id, created_at) VALUES (?,?,?,?)",
		planID, project, "task-parent", dagReviewAt(0))
}

func (f *dagReviewFixture) revision(planID string, rev int, at string) {
	f.exec("INSERT INTO dag_plan_revisions (plan_id, revision_no, parent_revision_no, request_id, request_digest, change_json, state_digest, coordinator_epoch, author_task_id, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
		planID, rev, rev-1, fmt.Sprintf("%s-request-%d", planID, rev), "digest", "{}", "state", 0, "task-parent", at)
}

func (f *dagReviewFixture) node(planID, nodeID, issue string) {
	f.exec("INSERT INTO dag_nodes (plan_id, node_id, introduced_rev, retired_rev, slice_digest, issue_key, node_kind, title, criteria_set_digest, supersedes_node_id) VALUES (?,?,1,NULL,?,?,?,?,?,NULL)",
		planID, nodeID, "slice-"+nodeID, issue, "implementation", issue, "criteria")
}

func (f *dagReviewFixture) edge(planID, edgeID, from, to, kind string, introducedRev int) {
	f.exec("INSERT INTO dag_edges (plan_id, edge_id, introduced_rev, retired_rev, from_node_id, to_node_id, kind, target_repository, target_base_ref, pins_code_head) VALUES (?,?,?,NULL,?,?,?,?,?,0)",
		planID, edgeID, introducedRev, from, to, kind, "owner/repo", "dev")
}

func (f *dagReviewFixture) release(planID, nodeID, digest, at string) {
	f.exec("INSERT INTO dag_releases (plan_id, node_id, manifest_digest, managed_request_id, coordinator_epoch, decided_at) VALUES (?,?,?,?,0,?)",
		planID, nodeID, digest, "request-"+digest, at)
}

func (f *dagReviewFixture) acceptance(planID, nodeID, acceptanceID, relationshipID, at string) {
	f.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state) VALUES (?,?,?,?,?,1,'event','revision','criteria','verified','verified','turn','{}','task-parent',0,?,'active')",
		acceptanceID, planID, nodeID, "manifest-"+nodeID, relationshipID, at)
}

func (f *dagReviewFixture) observation(acceptanceID, observedAt string, isAncestor bool, revertedBy string) {
	f.exec("INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, observed_seq, reverted_by, observed_at) VALUES (?,?,?,?,?,?,?,?,1,?,?)",
		"observation-"+acceptanceID, acceptanceID, "owner/repo", "dev", "subject", "tip", dagReviewFlag(isAncestor), "ancestry", dagReviewNull(revertedBy), observedAt)
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
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind) VALUES (?,?,?,1,'manifest','initial')",
		planID, nodeID, relationshipID)
}

func (f *dagReviewFixture) relationship(relationshipID, issue, createdAt string) {
	f.exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES (?,?,'active','parent','host','child','host',1,'[]','[]',?,?)",
		relationshipID, issue, createdAt, createdAt)
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
