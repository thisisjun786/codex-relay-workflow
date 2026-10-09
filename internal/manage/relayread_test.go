package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The marker strings a settings record, a delivery body and a verdict memo carry in these tests:
// none of them may reach the projection, whatever the projection reads.
const (
	relayReadSettingsMarker   = "RELAY-READ-SETTINGS-MARKER"
	relayReadDeliveryMarker   = "RELAY-READ-DELIVERY-MARKER"
	relayReadConversationMark = "RELAY-READ-CONVERSATION-MARKER"
	relayReadVerdictMark      = "RELAY-READ-VERDICT-MARKER"
)

// relayReadRunCommand runs the command over the fixture state and returns its exit status and
// streams.
func relayReadRunCommand(t *testing.T, state string, args ...string) (int, string, string) {
	t.Helper()
	env := dagReviewEnv(t)
	var stdout, stderr bytes.Buffer
	env.Stdout, env.Stderr = &stdout, &stderr
	code := relayReadCommand.Run(context.Background(), env, append([]string{"--state", state}, args...))
	return code, stdout.String(), stderr.String()
}

// relayReadWithStore runs the relay readers the projection is built from over one read-only
// snapshot of the store, so a test compares the projection with its own sources.
func relayReadWithStore(t *testing.T, path string, run func(ctx context.Context, st *store.Store)) {
	t.Helper()
	handle, err := store.OpenInPlace(context.Background(), path, 5*time.Second)
	if err != nil {
		t.Fatalf("open the store read-only: %v", err)
	}
	defer handle.Close()
	if err := handle.ReadSnapshot(context.Background(), func(ctx context.Context, st *store.Store) error {
		run(ctx, st)
		return nil
	}); err != nil {
		t.Fatalf("read the store: %v", err)
	}
}

// relayReadJSONWithoutReadAt is the document with readAt removed: two reads happen at different
// instants, so that one field is the only one a comparison may not pin.
func relayReadJSONWithoutReadAt(t *testing.T, data []byte) string {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("the document is not JSON: %v\n%s", err, data)
	}
	delete(document, "readAt")
	out, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// relayReadProjectedField is one field of a projected value, read by the JSON name the document
// carries, so the comparison is against what a consumer of the document actually sees.
func relayReadProjectedField(t *testing.T, value any, key string) any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %#v: %v", value, err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	return document[key]
}

// relayReadProjectedKey is one field of a projected value together with whether the document carries
// the key at all, so an explicit null is told from a field that is missing.
func relayReadProjectedKey(t *testing.T, value any, key string) (any, bool) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %#v: %v", value, err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	got, present := document[key]
	return got, present
}

// relayReadSourceField is one field of a relay reader's own record.
func relayReadSourceField(t *testing.T, record any, key string) any {
	t.Helper()
	object, ok := record.(contract.OrderedObject)
	if !ok {
		t.Fatalf("the source record is %T, not an ordered object", record)
	}
	return object.Get(key)
}

func relayReadRelationshipByID(t *testing.T, projection RelayProjection, id string) RelayRelationship {
	t.Helper()
	for _, item := range projection.Relationships {
		if item.RelationshipID == id {
			return item
		}
	}
	t.Fatalf("the projection carries no relationship %q: %+v", id, projection.Relationships)
	return RelayRelationship{}
}

func relayReadPlanByID(t *testing.T, projection RelayProjection, id string) RelayPlan {
	t.Helper()
	for _, item := range projection.Plans {
		if item.PlanID == id {
			return item
		}
	}
	t.Fatalf("the projection carries no plan %q: %+v", id, projection.Plans)
	return RelayPlan{}
}

// relayReadField fails the test unless the projected value equals its source's.
func relayReadField(t *testing.T, label string, got, want any) {
	t.Helper()
	if !relayReadEqual(got, want) {
		t.Errorf("%s = %#v, want %#v", label, got, want)
	}
}

func relayReadEqual(a, b any) bool {
	left, lerr := json.Marshal(a)
	right, rerr := json.Marshal(b)
	if lerr != nil || rerr != nil {
		return false
	}
	return string(left) == string(right)
}

// relayReadFailingWriter refuses every write, which is the JSON output write failure.
type relayReadFailingWriter struct{}

func (relayReadFailingWriter) Write([]byte) (int, error) {
	return 0, errors.New("the output is closed")
}

// relayReadSidecars reports whether the store's write-ahead log and shared-memory index exist.
func relayReadSidecars(t *testing.T, path string) string {
	t.Helper()
	report := ""
	for _, suffix := range []string{"-wal", "-shm"} {
		_, err := os.Stat(path + suffix)
		switch {
		case err == nil:
			report += suffix + "=present "
		case errors.Is(err, os.ErrNotExist):
			report += suffix + "=absent "
		default:
			t.Fatalf("examine %s: %v", path+suffix, err)
		}
	}
	return strings.TrimSpace(report)
}

// ---------------------------------------------------------------- the fixture

// relayReadEverything is a temporary store holding one live relationship, one project binding, one
// plan and one merge turn: the four sections of the projection all read something.
func relayReadEverything(t *testing.T) *dagReviewFixture {
	t.Helper()
	f := dagReviewNewFixture(t)
	f.relayReadRelationship("rel-1", "CRW-1", "active", "parent-1", "child-1", 1)
	f.relayReadScope("rel-1", "project-1")
	f.relayReadEvent("evt-1", "rel-1", "hash-1", "ready_for_review")
	f.relayReadLineage("evt-1", "rel-1", "hash-1", "")
	f.relayReadCriteria("rel-1", "criteria-1")
	f.relayReadBinding("bnd-1", "parent", "project", "project-1", "parent-1", "host-1", "/tmp/work", "active", 1)
	f.relayReadPutPlan("plan-1", "project-1", "A")
	f.laneTurn("turn-1", "owner/repo#dev", "holder-1", "waiting", "rel-1", 7, dagReviewAt(0), dagReviewAt(0), "")
	return f
}

func (f *dagReviewFixture) relayReadRelationship(rid, issue, status, parent, child string, generation int) {
	f.exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id,"+
		" child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at)"+
		" VALUES (?,?,?,?,?,?,?,?,'[]','[]',?,?)",
		rid, issue, status, parent, "host-1", child, "host-1", generation, dagReviewAt(0), dagReviewAt(0))
	// The relationship points at its generation, so the generation row has to be retained: the
	// registry refuses a relationship whose generation the store no longer carries.
	f.exec("INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state,"+
		" dispatch_turn_id, reason, opened_at, bound_at) VALUES (?,?,?,?,?,NULL,?,?)",
		rid, generation, "dispatch-"+rid, "bound", "turn-"+rid, dagReviewAt(0), dagReviewAt(0))
}

func (f *dagReviewFixture) relayReadScope(rid, project string) {
	f.exec("INSERT INTO relationship_scope (relationship_id, project_key, recorded_at) VALUES (?,?,?)", rid, project, dagReviewAt(0))
}

func (f *dagReviewFixture) relayReadBinding(bindingID, role, scopeKind, scopeKey, task, host, cwd, status string, revision int) {
	f.exec("INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, cwd, cxc_session,"+
		" status, revision, created_at, updated_at) VALUES (?,?,?,?,?,?,?,NULL,?,?,?,?)",
		bindingID, role, scopeKind, scopeKey, task, host, cwd, status, revision, dagReviewAt(0), dagReviewAt(0))
}

func (f *dagReviewFixture) relayReadEvent(eventID, rid, revisionHash, outcome string) {
	f.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer,"+
		" turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
		" VALUES (?,?,1,?,?,'child','thread-1','turn-1','completed','{}','final',?,?)",
		eventID, rid, revisionHash, outcome, dagReviewAt(0), dagReviewAt(0))
}

func (f *dagReviewFixture) relayReadLineage(eventID, rid, revisionHash, supersedes string) {
	f.exec("INSERT INTO revision_lineage (relationship_id, execution_generation, event_id, revision_hash, supersedes_hash,"+
		" declared_by, recorded_at) VALUES (?,1,?,?,?,'child',?)",
		rid, eventID, revisionHash, dagReviewNull(supersedes), dagReviewAt(0))
}

func (f *dagReviewFixture) relayReadCriteria(rid, digest string) {
	f.exec("INSERT INTO canonical_criteria (relationship_id, criterion_id, title, required, source_ref, set_digest, recorded_at)"+
		" VALUES (?,?,'criterion',1,NULL,?,?)", rid, "criterion-1", digest, dagReviewAt(0))
}

func (f *dagReviewFixture) relayReadVerdict(eventID, verdict, setDigest string) {
	f.exec("INSERT INTO verdicts (event_id, record, verdict, next_generation, verdict_turn_id, decided_at)"+
		" VALUES (?,?,?,NULL,'turn-1',?)", eventID, `{"executionGeneration": 1}`, verdict, dagReviewAt(0))
	f.exec("INSERT INTO verdict_context (event_id, set_digest, coverage, findings, reason, currency, ack_evidence, recorded_at)"+
		" VALUES (?,?,'full',?,?,'current','ack',?)", eventID, setDigest, relayReadVerdictMark, relayReadVerdictMark, dagReviewAt(0))
}

// relayReadPutPlan writes a one-revision plan through the DAG repository, so the plan rows, the
// revision's change list and the node fold agree the way the scheduler's own reading requires. A
// hand-written row set would make Progress refuse the plan as corrupt, which is not what these
// tests are about.
func (f *dagReviewFixture) relayReadPutPlan(planID, project string, nodes ...string) {
	f.t.Helper()
	changes := make([]any, 0, len(nodes))
	for _, node := range nodes {
		changes = append(changes, map[string]any{"op": dag.OpAddNode, "node": map[string]any{
			"node_id": node, "issue_key": "CRW-" + node, "kind": dag.NodeImplementation,
			"criteria_set_digest": testsupport.Dig("criteria " + node)}})
	}
	raw, err := json.Marshal(map[string]any{
		"schema": dag.SchemaRevision, "plan_id": planID, "project_key": project,
		"request_id": planID + "-r1", "expected_parent_revision": 0,
		"author_task_id": "task-parent", "changes": changes,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	revision, err := dag.DecodeRevision(raw)
	if err != nil {
		f.t.Fatalf("decode the plan revision: %v", err)
	}
	if _, err := (&dag.Repo{Store: f.store}).Put(context.Background(), revision); err != nil {
		f.t.Fatalf("put the plan revision: %v", err)
	}
}

// relayReadMarkerRows writes the private bodies a projection must never carry: a settings record, a
// rendered delivery message and a verdict memo. The verdict row matters: without it the verdict
// context is empty and the assertion that the marker never reaches the projection would hold for
// every implementation, including one that carried the memo.
func (f *dagReviewFixture) relayReadMarkerRows() {
	f.relayReadVerdict("evt-1", "verified", "criteria-1")
	f.exec("INSERT INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?)",
		"parent-1", `{"marker":"`+relayReadSettingsMarker+`"}`, "test", dagReviewAt(0))
	// The delivery marker goes in the columns a delivery body actually fills (dispatch_evidence,
	// hold_reason and provenance), so the assertion that it never reaches the projection is not
	// vacuous: the assignment view reads these columns into its delivery record, which the
	// projection drops.
	f.exec("INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state,"+
		" dispatch_evidence, hold_reason, provenance, created_at, updated_at)"+
		" VALUES (?,?,'completion','parent-1','thread-1','queued',?,?,?,?,?)",
		"evt-1", "rel-1", relayReadDeliveryMarker, relayReadDeliveryMarker, relayReadDeliveryMarker, dagReviewAt(0), dagReviewAt(0))
	f.exec("INSERT INTO attempt_messages (request_id, event_id, attempt_no, kind, message, rendered_at)"+
		" VALUES (?,?,1,'completion',?,?)", "request-1", "evt-1", relayReadConversationMark, dagReviewAt(0))
}

// relayReadDropPlans removes the plan table, the way a store that predates the DAG zone lacks it.
// The table is only the plan section's source, so exactly that section must read as unknown.
func (f *dagReviewFixture) relayReadDropPlans() {
	f.t.Helper()
	ctx := context.Background()
	conn, err := f.store.DB.Conn(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		f.t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "DROP TABLE dag_plans"); err != nil {
		f.t.Fatal(err)
	}
}

// ---------------------------------------------------------------- C1

// C1: a read uses only the temporary store: the file's bytes and mtime are what they were and no
// write-ahead log or shared-memory index appears.
func TestRelayReadLeavesTheStoreFileUnchanged(t *testing.T) {
	f := relayReadEverything(t)
	f.close()

	before := dagReviewFileState(t, f.path)
	sidecarsBefore := relayReadSidecars(t, f.path)
	projection, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	// A no-op would leave the file alone too, so the immutability claim is only meaningful beside
	// a read that happened.
	if len(projection.Relationships) != 1 || len(projection.Bindings) != 1 || len(projection.Plans) != 1 || len(projection.MergeTurns) != 1 {
		t.Fatalf("the read did not read the store: %+v", projection)
	}
	if after := dagReviewFileState(t, f.path); after != before {
		t.Errorf("the read changed the store file:\n before %s\n after  %s", before, after)
	}
	if after := relayReadSidecars(t, f.path); after != sidecarsBefore {
		t.Errorf("the read changed the store's sidecars: before %q, after %q", sidecarsBefore, after)
	}
}

// ---------------------------------------------------------------- C2

// C2: every projected field equals the relay reader it comes from.
func TestRelayReadStateMatchesItsSources(t *testing.T) {
	f := relayReadEverything(t)
	f.close()

	projection, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	if len(projection.Failures) != 0 {
		t.Fatalf("the read recorded failures: %+v", projection.Failures)
	}
	relayReadWithStore(t, f.path, func(ctx context.Context, st *store.Store) {
		state, err := registry.NewAssignmentView(&registry.Registry{Store: st}).State(ctx, "rel-1")
		if err != nil {
			t.Fatalf("AssignmentView.State: %v", err)
		}
		item := relayReadRelationshipByID(t, projection, "rel-1")
		for _, key := range []string{"relationshipId", "issueKey", "parentTaskId", "childTaskId",
			"relationshipStatus", "executionGeneration", "state", "nextExpectedAction"} {
			relayReadField(t, "relationship."+key, relayReadProjectedField(t, item, key), state.Get(key))
		}
		if item.Head == nil {
			t.Fatalf("the head is missing: projected %+v, source %#v", item.Head, state.Get("head"))
		}
		for _, key := range []string{"eventId", "revisionHash", "evidence", "competitors", "detail"} {
			relayReadField(t, "head."+key, relayReadProjectedField(t, *item.Head, key), relayReadSourceField(t, state.Get("head"), key))
		}
		verdict, ok := state.Get("lastVerdict").(contract.OrderedObject)
		if !ok {
			if item.LastVerdict != nil {
				t.Errorf("lastVerdict = %+v, want null", item.LastVerdict)
			}
		} else {
			if item.LastVerdict == nil {
				t.Fatalf("the verdict is missing: projected %+v, source %#v", item.LastVerdict, verdict)
			}
			for _, key := range []string{"verdict", "eventId", "executionGeneration", "decidedAt"} {
				relayReadField(t, "lastVerdict."+key, relayReadProjectedField(t, *item.LastVerdict, key), verdict.Get(key))
			}
		}

		progress, err := (&dagsched.Scheduler{Store: st}).Progress(ctx, st.Q(ctx), "plan-1")
		if err != nil {
			t.Fatalf("dagsched Progress: %v", err)
		}
		plan := relayReadPlanByID(t, projection, "plan-1")
		relayReadField(t, "plan.planId", plan.PlanID, progress.Reading.PlanID)
		relayReadField(t, "plan.projectKey", plan.ProjectKey, progress.ProjectKey)
		relayReadField(t, "plan.revision", plan.Revision, progress.Denominator.Revision)
		relayReadField(t, "plan.denominator", plan.Denominator, progress.Denominator.Nodes)
		relayReadField(t, "plan.blocked", plan.Blocked, progress.Blocked.Nodes)
		if len(plan.Stages) != len(progress.Stages) {
			t.Errorf("plan.stages has %d entries, want %d", len(plan.Stages), len(progress.Stages))
		}
		for _, stage := range progress.Stages {
			relayReadField(t, "plan.stages."+stage.Stage, plan.Stages[stage.Stage], stage.Nodes)
		}

		turn, err := (&mergeturn.Service{Store: st}).Turn(ctx, "turn-1")
		if err != nil {
			t.Fatalf("mergeturn Turn: %v", err)
		}
		if len(projection.MergeTurns) != 1 {
			t.Fatalf("the projection carries %d merge turns, want 1", len(projection.MergeTurns))
		}
		projectedTurn := projection.MergeTurns[0]
		relayReadField(t, "mergeTurn.turnId", projectedTurn.TurnID, turn["turnId"])
		relayReadField(t, "mergeTurn.repository", projectedTurn.Repository, turn["repository"])
		relayReadField(t, "mergeTurn.prNumber", projectedTurn.PRNumber, turn["prNumber"])
		relayReadField(t, "mergeTurn.holderTaskId", projectedTurn.HolderTaskID, turn["holderTaskId"])
		relayReadField(t, "mergeTurn.state", projectedTurn.State, turn["state"])
		relayReadField(t, "mergeTurn.requestedAt", projectedTurn.RequestedAt, turn["requestedAt"])
		relayReadField(t, "mergeTurn.updatedAt", projectedTurn.UpdatedAt, turn["updatedAt"])

		owners, err := (&registry.Registry{Store: st}).Owners(ctx, "project", "project-1")
		if err != nil {
			t.Fatalf("Registry.Owners: %v", err)
		}
		if len(owners) != len(projection.Bindings) {
			t.Fatalf("the projection carries %d bindings, Owners reads %d", len(projection.Bindings), len(owners))
		}
		for i, owner := range owners {
			projected := projection.Bindings[i]
			for _, key := range []string{"bindingId", "role", "scopeKind", "scopeKey", "taskId", "hostId", "cwd", "status", "revision"} {
				relayReadField(t, "binding."+key, relayReadProjectedField(t, projected, key), owner.Get(key))
			}
		}
	})
}

// ---------------------------------------------------------------- C3

// C3: a forked head is reported with the relay's own ambiguity, never picked.
func TestRelayReadForkedHeadKeepsTheRelayAmbiguity(t *testing.T) {
	f := dagReviewNewFixture(t)
	f.relayReadRelationship("rel-1", "CRW-1", "active", "parent-1", "child-1", 1)
	f.relayReadScope("rel-1", "project-1")
	f.relayReadEvent("evt-0", "rel-1", "hash-0", "ready_for_review")
	f.relayReadEvent("evt-1", "rel-1", "hash-1", "ready_for_review")
	f.relayReadEvent("evt-2", "rel-1", "hash-2", "ready_for_review")
	f.relayReadLineage("evt-1", "rel-1", "hash-1", "hash-0")
	f.relayReadLineage("evt-2", "rel-1", "hash-2", "hash-0")
	f.close()

	projection, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	item := relayReadRelationshipByID(t, projection, "rel-1")
	if item.Head == nil {
		t.Fatal("the relationship carries no head")
	}
	if item.Head.Evidence != "fork" {
		t.Errorf("head.evidence = %v, want fork", item.Head.Evidence)
	}
	if item.Head.EventID != nil {
		t.Errorf("a forked head was picked: eventId = %v", item.Head.EventID)
	}
	competitors, _ := item.Head.Competitors.([]any)
	if len(competitors) != 3 {
		t.Errorf("head.competitors = %#v, want the three revisions", item.Head.Competitors)
	}
	if item.State != "ambiguous" {
		t.Errorf("state = %v, want ambiguous", item.State)
	}
	relayReadWithStore(t, f.path, func(ctx context.Context, st *store.Store) {
		state, err := registry.NewAssignmentView(&registry.Registry{Store: st}).State(ctx, "rel-1")
		if err != nil {
			t.Fatalf("AssignmentView.State: %v", err)
		}
		relayReadField(t, "head.evidence", item.Head.Evidence, relayReadSourceField(t, state.Get("head"), "evidence"))
		relayReadField(t, "head.detail", item.Head.Detail, relayReadSourceField(t, state.Get("head"), "detail"))
		relayReadField(t, "head.competitors", item.Head.Competitors, relayReadSourceField(t, state.Get("head"), "competitors"))
		relayReadField(t, "state", item.State, state.Get("state"))
	})
}

// C3: a head behind its criteria keeps the state the assignment view gave it.
func TestRelayReadStaleHeadKeepsTheAssignmentViewState(t *testing.T) {
	f := dagReviewNewFixture(t)
	f.relayReadRelationship("rel-1", "CRW-1", "active", "parent-1", "child-1", 1)
	f.relayReadScope("rel-1", "project-1")
	f.relayReadEvent("evt-1", "rel-1", "hash-1", "ready_for_review")
	f.relayReadLineage("evt-1", "rel-1", "hash-1", "")
	f.relayReadCriteria("rel-1", "criteria-2")
	f.relayReadVerdict("evt-1", "verified", "criteria-1")
	f.close()

	projection, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	item := relayReadRelationshipByID(t, projection, "rel-1")
	if item.State != "re_review_needed" {
		t.Errorf("state = %v, want the assignment view's own re_review_needed", item.State)
	}
	if item.LastVerdict == nil {
		t.Fatal("the relationship carries no last verdict")
	}
	relayReadField(t, "lastVerdict.verdict", item.LastVerdict.Verdict, "verified")
	relayReadWithStore(t, f.path, func(ctx context.Context, st *store.Store) {
		state, err := registry.NewAssignmentView(&registry.Registry{Store: st}).State(ctx, "rel-1")
		if err != nil {
			t.Fatalf("AssignmentView.State: %v", err)
		}
		relayReadField(t, "state", item.State, state.Get("state"))
	})
}

// C3: an absent store is the named error and the command's store exit.
func TestRelayReadAbsentStoreIsNamedAndExitsThree(t *testing.T) {
	coreTempHome(t)
	dir := t.TempDir()
	if _, err := RelayReadState(context.Background(), dir, RelayReadOptions{}); !errors.Is(err, ErrRelayStoreAbsent) {
		t.Fatalf("err = %v, want ErrRelayStoreAbsent", err)
	}
	code, stdout, stderr := relayReadRunCommand(t, dir)
	if code != relayReadStoreExit {
		t.Errorf("exit = %d, want %d (stderr: %s)", code, relayReadStoreExit, stderr)
	}
	if stdout != "" {
		t.Errorf("an absent store printed a document: %q", stdout)
	}
	if !strings.Contains(stderr, ErrRelayStoreAbsent.Error()) {
		t.Errorf("the error name is missing from stderr: %q", stderr)
	}
}

// C3: an unconfigured state directory is its own named error.
func TestRelayReadUnconfiguredStateIsNamed(t *testing.T) {
	if _, err := RelayReadState(context.Background(), "", RelayReadOptions{}); !errors.Is(err, ErrRelayStateUnconfigured) {
		t.Fatalf("err = %v, want ErrRelayStateUnconfigured", err)
	}
}

// A context that has already ended is that context's error, not a projection and not a store
// failure: the read produced nothing, and it says so rather than reporting a relay with no rows.
func TestRelayReadEndedContextIsItsOwnError(t *testing.T) {
	f := relayReadEverything(t)
	f.close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	projection, err := RelayReadState(ctx, f.dir, RelayReadOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !relayReadEqual(projection, RelayProjection{}) {
		t.Errorf("an ended context returned a projection: %+v", projection)
	}
}

// C3: a JSON output that cannot be written is the store exit, not a silent success.
func TestRelayReadOutputWriteFailureExitsThree(t *testing.T) {
	f := relayReadEverything(t)
	f.close()
	env := dagReviewEnv(t)
	var stderr bytes.Buffer
	env.Stdout, env.Stderr = relayReadFailingWriter{}, &stderr
	if code := relayReadCommand.Run(context.Background(), env, []string{"--state", f.dir}); code != relayReadStoreExit {
		t.Errorf("exit = %d, want %d (stderr: %s)", code, relayReadStoreExit, stderr.String())
	}
}

// ---------------------------------------------------------------- C4

// C4: a section whose source is missing is null with one failure line, never an empty success.
func TestRelayReadMissingTableIsNullWithOneFailure(t *testing.T) {
	f := relayReadEverything(t)
	f.relayReadDropPlans()
	f.close()

	projection, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	if projection.Plans != nil {
		t.Errorf("plans = %+v, want null", projection.Plans)
	}
	if len(projection.Failures) != 1 {
		t.Fatalf("failures = %+v, want one line", projection.Failures)
	}
	if projection.Failures[0].Section != "plans" {
		t.Errorf("failure section = %q, want plans", projection.Failures[0].Section)
	}
	if projection.Failures[0].Reason == "" {
		t.Error("the failure carries no reason")
	}
	// The other sections must still have been read: one missing table is not a blank projection.
	if len(projection.Relationships) != 1 || len(projection.Bindings) != 1 || len(projection.MergeTurns) != 1 {
		t.Errorf("the other sections were not read: %+v", projection)
	}
	code, stdout, stderr := relayReadRunCommand(t, f.dir)
	if code != relayReadUnknownExit {
		t.Errorf("exit = %d, want %d (stderr: %s)", code, relayReadUnknownExit, stderr)
	}
	if !strings.Contains(stdout, `"plans":null`) {
		t.Errorf("the document does not report the unread section as null: %s", stdout)
	}
}

// ---------------------------------------------------------------- C5

// C5: the command prints exactly what RelayReadState returns.
func TestRelayReadCLIOutputEqualsRelayReadState(t *testing.T) {
	f := relayReadEverything(t)
	f.close()
	code, stdout, stderr := relayReadRunCommand(t, f.dir)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	state, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if relayReadJSONWithoutReadAt(t, []byte(stdout)) != relayReadJSONWithoutReadAt(t, data) {
		t.Errorf("the command document and RelayReadState's differ:\n cli   %s\n state %s", stdout, data)
	}
}

// C5: the command reports its usage as a usage error and its help as success.
func TestRelayReadCLIUsageIsTwo(t *testing.T) {
	env := dagReviewEnv(t)
	var stdout, stderr bytes.Buffer
	env.Stdout, env.Stderr = &stdout, &stderr
	if code := relayReadCommand.Run(context.Background(), env, []string{"--nope"}); code != usageExit {
		t.Errorf("exit = %d, want %d", code, usageExit)
	}
	if !strings.Contains(stderr.String(), "usage: crw manage relay-read") {
		t.Errorf("the usage line is missing: %q", stderr.String())
	}
	stdout.Reset()
	if code := relayReadCommand.Run(context.Background(), env, []string{"-h"}); code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "usage: crw manage relay-read") {
		t.Errorf("the help line is missing: %q", stdout.String())
	}
}

// ---------------------------------------------------------------- the options

// relayReadTwoProjects is two projects, each with a live relationship, a binding, a plan and a
// merge turn, plus one closed relationship: what the selectors narrow.
func relayReadTwoProjects(t *testing.T) *dagReviewFixture {
	t.Helper()
	f := dagReviewNewFixture(t)
	for _, project := range []string{"project-1", "project-2"} {
		rid := "rel-" + project
		f.relayReadRelationship(rid, "CRW-"+project, "active", "parent-1", "child-"+project, 1)
		f.relayReadScope(rid, project)
		f.relayReadBinding("bnd-"+project, "parent", "project", project, "parent-1", "host-1", "/tmp/work", "active", 1)
		f.relayReadPutPlan("plan-"+project, project, "A")
		f.relayReadTurn("turn-"+project, project, "holder-"+project, "waiting", rid)
	}
	// A closed turn and a superseded binding, which a default read leaves out and --all keeps.
	f.relayReadTurn("turn-closed", "project-1", "holder-closed", "landed", "rel-project-1")
	f.relayReadBinding("bnd-superseded", "parent", "project", "project-1", "parent-old", "host-1", "/tmp/old", "superseded", 1)
	f.relayReadRelationship("rel-closed", "CRW-closed", "cancelled", "parent-1", "child-closed", 1)
	f.relayReadScope("rel-closed", "project-1")
	return f
}

// relayReadTurn writes one merge turn on a project.
func (f *dagReviewFixture) relayReadTurn(turnID, project, holder, state, rid string) {
	f.exec("INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id,"+
		" relationship_id, pr_number, candidate_head, declared_ready, state, tenure, requested_at, held_at, closed_at, updated_at)"+
		" VALUES (?,?,'owner/repo','dev',?,?,'host-1',?,7,'head',0,?,1,?,?,NULL,?)",
		turnID, "owner/repo#dev-"+project, project, holder, rid, state, dagReviewAt(0), dagReviewAt(0), dagReviewAt(0))
}

// relayReadExecution records that a relationship executes a node of a plan, which is the link a
// plan selector uses to choose the merge turns that belong to it.
func (f *dagReviewFixture) relayReadExecution(planID, nodeID, rid string) {
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind)"+
		" VALUES (?,?,?,1,'manifest','initial')", planID, nodeID, rid)
}

// A plan selector narrows the merge turns to the ones whose relationship executes a node of that
// plan, so a narrowed projection does not carry another plan's lanes.
func TestRelayReadPlanSelectorNarrowsMergeTurns(t *testing.T) {
	f := relayReadTwoProjects(t)
	f.relayReadExecution("plan-project-1", "A", "rel-project-1")
	f.relayReadExecution("plan-project-2", "A", "rel-project-2")
	f.close()

	projection, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{Plans: []string{"plan-project-1"}})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	if len(projection.MergeTurns) != 1 || projection.MergeTurns[0].TurnID != "turn-project-1" {
		t.Errorf("--plan did not narrow the merge turns: %+v", projection.MergeTurns)
	}
	if len(projection.Relationships) != 1 || projection.Relationships[0].RelationshipID != "rel-project-1" {
		t.Errorf("--plan did not narrow the relationships: %+v", projection.Relationships)
	}
}

// A plan the caller named that the store holds but the project selector excluded is reported as an
// item whose source could not be read, rather than silently dropped: a named target never vanishes
// from the projection without a mark.
func TestRelayReadPlanOutsideTheSelectedProjectIsUnknown(t *testing.T) {
	f := relayReadTwoProjects(t)
	f.close()

	projection, err := RelayReadState(context.Background(), f.dir,
		RelayReadOptions{Plans: []string{"plan-project-1"}, Projects: []string{"project-2"}})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	if len(projection.Plans) != 1 || projection.Plans[0].PlanID != "plan-project-1" {
		t.Fatalf("plans = %+v, want the named plan reported", projection.Plans)
	}
	if projection.Plans[0].Read.State != relayReadReadUnknown {
		t.Errorf("read = %+v, want unknown", projection.Plans[0].Read)
	}
}

// The options choose the target list: an empty option is everything the store carries, --project
// narrows to one project's relationships, bindings and merge turns, --plan narrows to one plan,
// and --all keeps what a default read leaves out.
func TestRelayReadOptionsChooseTheTargets(t *testing.T) {
	f := relayReadTwoProjects(t)
	f.close()

	all, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	if len(all.Relationships) != 2 || len(all.Bindings) != 2 || len(all.Plans) != 2 || len(all.MergeTurns) != 2 {
		t.Fatalf("the default read is not the live store: %+v", all)
	}

	byProject, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{Projects: []string{"project-1"}})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	if len(byProject.Relationships) != 1 || byProject.Relationships[0].RelationshipID != "rel-project-1" {
		t.Errorf("--project did not narrow the relationships: %+v", byProject.Relationships)
	}
	if len(byProject.Plans) != 1 || byProject.Plans[0].PlanID != "plan-project-1" {
		t.Errorf("--project did not narrow the plans: %+v", byProject.Plans)
	}
	if len(byProject.Bindings) != 1 || byProject.Bindings[0].ScopeKey != "project-1" {
		t.Errorf("--project did not narrow the bindings: %+v", byProject.Bindings)
	}
	if len(byProject.MergeTurns) != 1 || byProject.MergeTurns[0].TurnID != "turn-project-1" {
		t.Errorf("--project did not narrow the merge turns: %+v", byProject.MergeTurns)
	}

	byPlan, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{Plans: []string{"plan-project-2"}})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	if len(byPlan.Plans) != 1 || byPlan.Plans[0].PlanID != "plan-project-2" {
		t.Errorf("--plan did not narrow the plans: %+v", byPlan.Plans)
	}

	closed, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{IncludeClosed: true})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	found := false
	for _, item := range closed.Relationships {
		if item.RelationshipID == "rel-closed" {
			found = true
		}
	}
	if !found {
		t.Errorf("--all left the closed relationship out: %+v", closed.Relationships)
	}
	if len(closed.MergeTurns) != 3 {
		t.Errorf("--all did not keep the closed merge turn: %+v", closed.MergeTurns)
	}
	if len(closed.Bindings) != 3 {
		t.Errorf("--all did not keep the superseded binding: %+v", closed.Bindings)
	}
}

// A plan the caller named but the store does not carry is an item whose source could not be read:
// it is reported with an unknown read mark and the command exits 1, rather than an empty section
// that reads as a plan with nothing in it.
func TestRelayReadRequestedPlanTheStoreLacksIsUnknown(t *testing.T) {
	f := relayReadEverything(t)
	f.close()

	projection, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{Plans: []string{"plan-absent"}})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	if len(projection.Plans) != 1 || projection.Plans[0].PlanID != "plan-absent" {
		t.Fatalf("plans = %+v, want the requested plan reported", projection.Plans)
	}
	if projection.Plans[0].Read.State != relayReadReadUnknown {
		t.Errorf("read = %+v, want unknown", projection.Plans[0].Read)
	}
	code, _, stderr := relayReadRunCommand(t, f.dir, "--plan", "plan-absent")
	if code != relayReadUnknownExit {
		t.Errorf("exit = %d, want %d (stderr: %s)", code, relayReadUnknownExit, stderr)
	}
}

// An option that needs a value must not swallow the next option: "--plan --all" is a malformed
// command line, not a request for a plan named "--all".
func TestRelayReadOptionValueThatLooksLikeAnOptionIsUsage(t *testing.T) {
	for _, args := range [][]string{
		{"--plan", "--all"}, {"--project", "--all"}, {"--state", "--all"},
		{"--plan"}, {"--project"}, {"--state"},
		{"--plan="}, {"--project="}, {"--state="},
	} {
		env := dagReviewEnv(t)
		var stdout, stderr bytes.Buffer
		env.Stdout, env.Stderr = &stdout, &stderr
		if code := relayReadCommand.Run(context.Background(), env, args); code != usageExit {
			t.Errorf("%q: exit = %d, want %d", args, code, usageExit)
		}
		if !strings.Contains(stderr.String(), "usage: crw manage relay-read") {
			t.Errorf("%q: the usage line is missing: %q", args, stderr.String())
		}
	}
}

// The read must not depend on the process's working directory: a caller that names an absolute
// state directory still gets its relationships read when the working directory has been removed.
func TestRelayReadReadsWithTheWorkingDirectoryRemoved(t *testing.T) {
	f := relayReadEverything(t)
	f.close()
	gone := t.TempDir()
	t.Chdir(gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	projection, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	item := relayReadRelationshipByID(t, projection, "rel-1")
	if item.Read.State != relayReadReadOK {
		t.Errorf("the relationship read %q with the working directory removed: %s", item.Read.State, item.Read.Reason)
	}
	if item.State == nil {
		t.Errorf("the relationship state is missing: %+v", item)
	}
}

// An item whose own source cannot be read carries an unknown read mark and keeps its place in the
// section, rather than being dropped: a relationship that points at a generation the store no
// longer retains is exactly that case.
func TestRelayReadUnreadableItemKeepsItsPlace(t *testing.T) {
	f := dagReviewNewFixture(t)
	f.relayReadRelationship("rel-ok", "CRW-OK", "active", "parent-1", "child-1", 1)
	f.relayReadScope("rel-ok", "project-1")
	// A relationship whose generation row is absent: the assignment view refuses it.
	f.exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id,"+
		" child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at)"+
		" VALUES ('rel-broken','CRW-BROKEN','active','parent-1','host-1','child-2','host-1',1,'[]','[]',?,?)",
		dagReviewAt(1), dagReviewAt(1))
	f.relayReadScope("rel-broken", "project-1")
	f.close()

	projection, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	if len(projection.Relationships) != 2 {
		t.Fatalf("the projection carries %d relationships, want both kept: %+v", len(projection.Relationships), projection.Relationships)
	}
	broken := relayReadRelationshipByID(t, projection, "rel-broken")
	if broken.Read.State != relayReadReadUnknown || broken.Read.Reason == "" {
		t.Errorf("rel-broken = %+v, want an unknown mark with a reason", broken.Read)
	}
	if ok := relayReadRelationshipByID(t, projection, "rel-ok"); ok.Read.State != relayReadReadOK || ok.State == nil {
		t.Errorf("rel-ok = %+v, want it read beside the broken one", ok)
	}
	code, _, stderr := relayReadRunCommand(t, f.dir)
	if code != relayReadUnknownExit {
		t.Errorf("exit = %d, want %d (stderr: %s)", code, relayReadUnknownExit, stderr)
	}
}

// ---------------------------------------------------------------- C6

// The two list predicates this command restates from the relay's own rules — which relationships
// are live and which merge turns have not closed — are pinned to the shipped schema's guard-index
// predicates, so a change to the relay's live or open vocabulary cannot silently narrow this
// projection: the constants stop matching the shipped schema and this test fails.
func TestRelayReadPredicatesMatchTheShippedSchema(t *testing.T) {
	_, guards, err := store.SchemaStatements()
	if err != nil {
		t.Fatalf("read the shipped schema: %v", err)
	}
	wanted := map[string]string{
		"scope_bindings_one_live_owner": relayReadLiveRelationshipFilter,
		"merge_turns_one_live_claim":    relayReadOpenTurnFilter,
	}
	found := map[string]bool{}
	for _, statement := range guards {
		for index, predicate := range wanted {
			if !strings.Contains(statement, "CREATE UNIQUE INDEX IF NOT EXISTS "+index+" ") {
				continue
			}
			found[index] = true
			if got, want := relayReadNormalizePredicate(statement), relayReadNormalizePredicate(predicate); got != want {
				t.Errorf("%s: the shipped predicate is %q and this command's is %q; the projection would silently narrow",
					index, got, want)
			}
		}
	}
	for index := range wanted {
		if !found[index] {
			t.Errorf("the shipped schema carries no guard index %s; this command's predicate is no longer pinned", index)
		}
	}
}

// relayReadNormalizePredicate is the WHERE clause of a guard index or a bare predicate with its
// spacing made uniform, so a formatting difference is not read as a vocabulary difference.
func relayReadNormalizePredicate(statement string) string {
	if at := strings.Index(statement, " WHERE "); at >= 0 {
		statement = statement[at+len(" WHERE "):]
	}
	return strings.Join(strings.Fields(strings.ReplaceAll(statement, "','", "', '")), " ")
}

// C6: the marker strings a settings record, a delivery body and a verdict memo carry never reach
// the projection.
func TestRelayReadHidesSettingsAndDeliveryBodies(t *testing.T) {
	f := relayReadEverything(t)
	f.relayReadMarkerRows()
	f.close()

	projection, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	data, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := relayReadRunCommand(t, f.dir)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, marker := range []string{relayReadSettingsMarker, relayReadDeliveryMarker, relayReadConversationMark, relayReadVerdictMark} {
		if strings.Contains(string(data), marker) {
			t.Errorf("the projection carries %s", marker)
		}
		if strings.Contains(stdout, marker) {
			t.Errorf("the command document carries %s", marker)
		}
	}
}

// ---------------------------------------------------------------- CRW-1042

// relayReadWorkReport writes one submission of a work report for a relationship's revision: the
// record the relay reads a pull request from. pr is the pull request number, or nil when the
// submission names none.
func (f *dagReviewFixture) relayReadWorkReport(eventID string, submission int, rid string, generation int, revisionHash, repository string, pr any) {
	f.exec("INSERT INTO work_reports (event_id, submission_no, relationship_id, execution_generation, revision_hash, repository, pr_number, pr_url, head_sha, cxc_status, cxc_reason, contract_version, summary, next_action, recorded_at)"+
		" VALUES (?,?,?,?,?,?,?,?,?,'DONE','proved','v1','done','merge',?)",
		eventID, submission, rid, generation, revisionHash, repository, pr, dagReviewNull(""), "head-1", dagReviewAt(0))
}

// CRW-1042: a relationship carries the pull request its current head names, as the relay's own
// record of that head gives it, and null when the relay holds none.
func TestRelayReadRelationshipCarriesItsPullRequest(t *testing.T) {
	f := relayReadEverything(t)
	f.relayReadWorkReport("evt-1", 1, "rel-1", 1, "hash-1", "owner/repo", 7)
	f.relayReadRelationship("rel-2", "CRW-2", "active", "parent-2", "child-2", 1)
	f.close()

	projection, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	with := relayReadProjectedField(t, relayReadRelationshipByID(t, projection, "rel-1"), "pullRequest")
	pr, ok := with.(map[string]any)
	if !ok {
		t.Fatalf("rel-1 pullRequest = %#v, want the pull request owner/repo#7", with)
	}
	if pr["repository"] != "owner/repo" || pr["number"] != float64(7) {
		t.Fatalf("rel-1 pullRequest = %#v, want repository owner/repo and number 7", pr)
	}
	if without := relayReadProjectedField(t, relayReadRelationshipByID(t, projection, "rel-2"), "pullRequest"); without != nil {
		t.Fatalf("rel-2 has no head and no report, pullRequest = %#v, want null", without)
	}
	if n := len(projection.Failures); n != 0 {
		t.Fatalf("the read recorded failures: %+v", projection.Failures)
	}
}

// CRW-1042: the newest submission of the head decides the link: a later submission that names no
// pull request withdraws the link an earlier one gave.
func TestRelayReadNewestReportWithoutPullRequestIsNoLink(t *testing.T) {
	f := relayReadEverything(t)
	f.relayReadWorkReport("evt-1", 1, "rel-1", 1, "hash-1", "owner/repo", 7)
	f.relayReadWorkReport("evt-1", 2, "rel-1", 1, "hash-1", "owner/repo", nil)
	f.close()

	projection, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	item := relayReadRelationshipByID(t, projection, "rel-1")
	if got, present := relayReadProjectedKey(t, item, "pullRequest"); !present || got != nil {
		t.Fatalf("pullRequest = %#v (present %v), want an explicit null: the newest report names no pull request", got, present)
	}
	if item.Read.State != relayReadReadOK {
		t.Fatalf("read = %+v, want ok: a withdrawn link is a successful read of no pull request", item.Read)
	}
}

// CRW-1042: before the later submission that names no pull request, the same head carries one: the
// withdrawal above is a change of what the relay reads, not an absence from the start.
func TestRelayReadReportWithPullRequestIsTheLinkUntilWithdrawn(t *testing.T) {
	f := relayReadEverything(t)
	f.relayReadWorkReport("evt-1", 1, "rel-1", 1, "hash-1", "owner/repo", 7)
	f.close()

	projection, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	item := relayReadRelationshipByID(t, projection, "rel-1")
	if item.PullRequest == nil || item.PullRequest.Number != 7 || item.Read.State != relayReadReadOK {
		t.Fatalf("rel-1 = %+v, want pull request #7 and an ok read", item)
	}
}

// CRW-1042: an acceptance that names a pull request number but has no forge identity recorded gives
// no usable link: the repository of the acceptance row can be a local checkout, so the item is marked
// unknown with a reason and carries no half-filled pull request.
func TestRelayReadAcceptanceWithoutForgeIdentityIsUnknown(t *testing.T) {
	f := relayReadEverything(t)
	f.acceptance("plan-1", "A", "acc-1", "rel-1", dagReviewAt(0))
	f.exec("UPDATE dag_acceptances SET repository = 'owner/repo', pr_number = 9 WHERE acceptance_id = 'acc-1'")
	f.close()

	projection, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	item := relayReadRelationshipByID(t, projection, "rel-1")
	if item.Read.State != relayReadReadUnknown || item.Read.Reason == "" {
		t.Fatalf("read = %+v, want unknown with a reason: the acceptance names #9 but records no forge repository", item.Read)
	}
	if item.PullRequest != nil {
		t.Fatalf("pullRequest = %+v, want none rather than a number without a repository", item.PullRequest)
	}
}

// CRW-1042: an active acceptance that names the relationship decides its pull request before the
// work report of the head does, the way the relay reads a node's link: the forge row's repository and
// number win over the report's.
func TestRelayReadAcceptedRelationshipNamesTheAcceptedPullRequest(t *testing.T) {
	f := relayReadEverything(t)
	f.relayReadWorkReport("evt-1", 1, "rel-1", 1, "hash-1", "owner/repo", 7)
	f.acceptance("plan-1", "A", "acc-1", "rel-1", dagReviewAt(0))
	f.exec("INSERT INTO dag_acceptance_forge (acceptance_id, forge_repository, pr_number) VALUES (?,?,?)", "acc-1", "owner/forge", 9)
	f.close()

	projection, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	pr, ok := relayReadProjectedField(t, relayReadRelationshipByID(t, projection, "rel-1"), "pullRequest").(map[string]any)
	if !ok {
		t.Fatalf("rel-1 has an accepted pull request, pullRequest = %#v", pr)
	}
	if pr["repository"] != "owner/forge" || pr["number"] != float64(9) {
		t.Fatalf("rel-1 pullRequest = %#v, want the forge row owner/forge#9", pr)
	}
}

// CRW-1042: two active acceptances that name one relationship (distinct nodes and revisions, which the
// store allows) give no pull request: the relay does not pick one of them, the item is unknown with a
// reason, and the command exits 1.
func TestRelayReadRelationshipNamedByTwoActiveAcceptancesIsUnknown(t *testing.T) {
	f := relayReadEverything(t)
	f.acceptance("plan-1", "A", "acc-1", "rel-1", dagReviewAt(0))
	f.exec("INSERT INTO dag_acceptance_forge (acceptance_id, forge_repository, pr_number) VALUES (?,?,?)", "acc-1", "owner/forge", 9)
	f.exec("INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state)"+
		" VALUES ('acc-2','plan-1','B','manifest-B','rel-1',1,'event','revision-2','criteria','verified',NULL,'verified','turn','{}','task-parent',0,?,'active')", dagReviewAt(0))
	f.exec("INSERT INTO dag_acceptance_forge (acceptance_id, forge_repository, pr_number) VALUES (?,?,?)", "acc-2", "owner/forge", 10)
	f.close()

	projection, err := RelayReadState(context.Background(), f.dir, RelayReadOptions{})
	if err != nil {
		t.Fatalf("RelayReadState: %v", err)
	}
	item := relayReadRelationshipByID(t, projection, "rel-1")
	if item.Read.State != relayReadReadUnknown || !strings.Contains(item.Read.Reason, "2 active acceptances") {
		t.Fatalf("read = %+v, want unknown naming the 2 active acceptances", item.Read)
	}
	if item.PullRequest != nil {
		t.Fatalf("pullRequest = %+v, want none: neither acceptance may be picked", item.PullRequest)
	}
	if got, present := relayReadProjectedKey(t, item, "pullRequest"); !present || got != nil {
		t.Fatalf("pullRequest = %#v (present %v), want an explicit null beside the unknown read", got, present)
	}
	code, stdout, stderr := relayReadRunCommand(t, f.dir)
	if code != relayReadUnknownExit {
		t.Fatalf("exit = %d, want %d (stdout: %s stderr: %s)", code, relayReadUnknownExit, stdout, stderr)
	}
}
