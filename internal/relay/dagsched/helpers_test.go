package dagsched

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// dig is a stand-in for a digest: 64 lowercase hex characters derived from a name.
func dig(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

type doc = map[string]any

// A fixture is a go-owned relay store (the shape every store has: built from the frozen fixture) with a scheduler over it and a
// deterministic clock. The clock is the only source of recorded_at values, so what a test reads back is a function of the calls it made.
type fixture struct {
	t     *testing.T
	s     *store.Store
	path  string
	repo  *dag.Repo
	sched *Scheduler
	mu    sync.Mutex // the clock is read from concurrent releases
	tick  int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureAt(t, filepath.Join(t.TempDir(), "relay.sqlite3"))
}

// newFixtureAt is a fixture over a store at a given path (the CLI tests need the path the binary will open).
func newFixtureAt(t *testing.T, path string) *fixture { return newFixtureOn(t, path, "") }

// newFixtureOn is a fixture over a store created for an App Server socket (a store a real adapter serves must record the socket it serves).
func newFixtureOn(t *testing.T, path, socket string) *fixture {
	t.Helper()
	testsupport.Create(t, path, socket, "go")
	s, err := store.Open(context.Background(), path, socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := &fixture{t: t, s: s, path: path}
	f.repo = &dag.Repo{Store: s, Now: f.clock}
	f.sched = &Scheduler{Store: s, Now: f.clock}
	return f
}

func (f *fixture) clock() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tick++
	return fmt.Sprintf("2026-10-02T00:%02d:%02d.000000+00:00", f.tick/60, f.tick%60)
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.s.DB.ExecContext(context.Background(), query, args...); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.s.DB.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// The plan documents: nodes are non_pr or implementation, edges of the three kinds (the shape CRW-183's tests use).
func nodeDoc(id, kind string) doc {
	return doc{"node_id": id, "issue_key": "CRW-" + id, "kind": kind, "criteria_set_digest": dig("criteria " + id)}
}
func addNode(id, kind string) doc { return doc{"op": dag.OpAddNode, "node": nodeDoc(id, kind)} }
func addEdge(id, from, to, kind string, extra doc) doc {
	e := doc{"edge_id": id, "from_node_id": from, "to_node_id": to, "kind": kind}
	switch kind {
	case dag.EdgeIntegrated:
		e["target_repository"], e["target_base_ref"] = "owner/repo", "dev"
	case dag.EdgeDecision:
		e["decision_subject"], e["decision_digest"], e["required_authority"] = "merge holds", dig("subject "+id), []any{"user", "owner"}
	}
	for k, v := range extra {
		e[k] = v
	}
	return doc{"op": dag.OpAddEdge, "edge": e}
}

func (f *fixture) putPlan(plan string, parent int, request string, changes ...doc) dag.Result {
	f.t.Helper()
	cs := make([]any, len(changes))
	for i, c := range changes {
		cs[i] = c
	}
	raw, err := json.Marshal(doc{"schema": dag.SchemaRevision, "plan_id": plan, "project_key": "P-TEST", "request_id": request,
		"expected_parent_revision": parent, "author_task_id": "task-test", "changes": cs})
	if err != nil {
		f.t.Fatal(err)
	}
	rev, err := dag.DecodeRevision(raw)
	if err != nil {
		f.t.Fatalf("decode: %v", err)
	}
	res, err := f.repo.Put(context.Background(), rev)
	if err != nil {
		f.t.Fatalf("put: %v", err)
	}
	return res
}

func (f *fixture) snapshot(plan string) dag.Snapshot {
	f.t.Helper()
	snap, _, err := f.repo.Snapshot(context.Background(), plan, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return snap
}

func (f *fixture) edge(snap dag.Snapshot, id string) dag.SnapEdge {
	f.t.Helper()
	for _, e := range snap.Edges {
		if e.EdgeID == id {
			return e
		}
	}
	f.t.Fatalf("no edge %s", id)
	return dag.SnapEdge{}
}

func (f *fixture) status(plan, edge string) EdgeStatus {
	f.t.Helper()
	snap := f.snapshot(plan)
	st, err := f.sched.edgeStatus(context.Background(), f.s.Q(context.Background()), plan, snap, f.edge(snap, edge))
	if err != nil {
		f.t.Fatal(err)
	}
	return st
}

// minimalManifest is the smallest manifest dag.CheckManifest accepts for a node of the snapshot, consuming the given inputs.
func (f *fixture) putManifest(snap dag.Snapshot, node string, inputs []any) string {
	f.t.Helper()
	n, ok := nodeOf(snap, node)
	if !ok {
		f.t.Fatalf("no node %s", node)
	}
	body := doc{"schema": dag.SchemaManifest, "node_id": node, "issue_key": n.IssueKey, "node_slice_digest": n.SliceDigest,
		"criteria_set_digest": n.CriteriaSetDigest, "inputs": inputs, "rule_version": doc{"model": "m"},
		"plan_revision_no": snap.Revision, "coordinator_epoch": 0, "created_by_task_id": "task-test", "created_at": "2026-10-02T00:00:00Z"}
	raw, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	digest, err := f.repo.PutManifest(context.Background(), raw)
	if err != nil {
		f.t.Fatalf("manifest: %v", err)
	}
	return digest
}

// accepted is what acceptNode wrote.
type accepted struct {
	Acceptance Acceptance
	Event      string
	Manifest   string
	Root       string   // the relationship's artifact root
	Files      []string // the artifacts of the receipt (non_pr nodes), by absolute path
}

type acceptOpts struct {
	Inputs     []any  // the manifest the node consumed
	HeadSHA    string // implementation nodes: the head the acceptance pinned
	PR         int64
	Forge      string // forge slug (a dag_acceptance_forge row when set)
	Repository string // the edge target recorded
	Status     string // relationship status, active by default
	Suffix     string // appended to the relationship id: a second acceptance of one node needs a second relationship
	NoManifest bool   // a non_pr node's receipt declares no artifacts
}

// acceptNode writes the rows a verified, accepted result leaves, shaped as the intake, ack and verdict writers shape them (the
// seed-versus-writer equivalence is pinned by TestSeedMatchesRealWriters in 040): a relationship with its bound generation, the final
// ready_for_review event and its lineage, the confirmed ack and its evidence, the verified verdict and its context, the registered criteria, the
// execution row, the stored manifest and the acceptance.
func (f *fixture) acceptNode(plan, node string, o acceptOpts) accepted {
	f.t.Helper()
	snap := f.snapshot(plan)
	n, ok := nodeOf(snap, node)
	if !ok {
		f.t.Fatalf("no node %s", node)
	}
	rid := "rel-" + plan + "-" + node + o.Suffix
	status := o.Status
	if status == "" {
		status = "active"
	}
	now := f.clock()
	root := f.t.TempDir()
	var files []string
	receipt := "{}"
	if n.Kind == dag.NodeNonPR && !o.NoManifest {
		file := filepath.Join(root, node+".md")
		content := []byte("artifact of " + node + o.Suffix + "\n")
		if err := os.WriteFile(file, content, 0o600); err != nil {
			f.t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		files = append(files, file)
		receipt = fmt.Sprintf(`{"manifest":[{"path":%s,"sha256":"%s","bytes":%d}]}`, jsonString(file), hex.EncodeToString(sum[:]), len(content))
	}
	if err := storeseed.RecordRelationship(context.Background(), f.s, store.Relationship{ID: rid, IssueKey: n.IssueKey, Status: status, ParentTaskID: "parent", ChildTaskID: "child-" + node,
		Generation: 1, ArtifactRoots: "[" + jsonString(root) + "]", AllowedRecipients: "[\"parent\"]", CreatedAt: now, UpdatedAt: now},
		store.Generation{RelationshipID: rid, Number: 1, DispatchRequestID: "dispatch-" + rid, AnchorState: store.AnchorBound,
			DispatchTurnID: sql.NullString{String: "turn-dispatch", Valid: true}, OpenedAt: now, BoundAt: sql.NullString{String: now, Valid: true}}, "host", "host"); err != nil {
		f.t.Fatal(err)
	}
	revision := dig("revision " + rid)
	event := "evt-" + rid
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
	inputs := o.Inputs
	if inputs == nil {
		inputs = []any{}
	}
	manifest := f.putManifest(snap, node, inputs)
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES (?, ?, ?, 1, ?, 'initial', ?)",
		plan, node, rid, manifest, ReleaseRequestID(node, manifest))
	a := Acceptance{PlanID: plan, NodeID: node, ManifestDigest: manifest, RelationshipID: rid, ExecutionGeneration: 1, EventID: event, RevisionHash: revision,
		CriteriaSetDigest: n.CriteriaSetDigest, Verdict: "verified", HeadSHA: o.HeadSHA, Repository: o.Repository, PRNumber: o.PR,
		AckTier: "host_read", VerdictTurnID: "verdict-turn", RuleVersionJSON: "{}", AcceptedByTask: "parent", AcceptedAt: f.clock(), State: "active"}
	if n.Kind == dag.NodeImplementation {
		a.EvidenceDigest = dig("evidence " + rid)
	}
	a.AcceptanceID = AcceptanceDigest(a)
	f.insertAcceptance(a)
	if o.Forge != "" {
		f.exec("INSERT INTO dag_acceptance_forge (acceptance_id, forge_repository, pr_number) VALUES (?, ?, ?)", a.AcceptanceID, o.Forge, o.PR)
	}
	return accepted{Acceptance: a, Event: event, Manifest: manifest, Root: root, Files: files}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (f *fixture) insertAcceptance(a Acceptance) {
	f.t.Helper()
	var pr any
	if a.PRNumber > 0 {
		pr = a.PRNumber
	}
	f.exec("INSERT INTO dag_acceptances ("+acceptanceColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		a.AcceptanceID, a.PlanID, a.NodeID, a.ManifestDigest, a.RelationshipID, a.ExecutionGeneration, a.EventID, a.RevisionHash, a.CriteriaSetDigest, a.Verdict,
		nullable(a.HeadSHA), nullable(a.Repository), pr, nullable(a.OutputManifestRef), nullable(a.EvidenceDigest), a.AckTier, a.VerdictTurnID, a.RuleVersionJSON,
		a.AcceptedByTask, a.CoordinatorEpoch, a.AcceptedAt, nullable(a.SupersedesAcceptanceID), a.State)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// projectParent registers the one parent of the plan's project: a release reserves under it.
func (f *fixture) projectParent() {
	f.t.Helper()
	now := f.clock()
	if err := storeseed.InsertScopeBinding(context.Background(), f.s, store.ScopeBindingsRow{BindingID: "bind-parent", Role: "parent", ScopeKind: "project", ScopeKey: "P-TEST",
		TaskID: "parent", HostID: "host", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		f.t.Fatal(err)
	}
}

// startNode records a running execution of a node: its relationship, bound to the generation the release made, and the execution row. It writes no
// event, so the relay's state word for it is requested.
func (f *fixture) startNode(plan, node string) string {
	f.t.Helper()
	snap := f.snapshot(plan)
	n, ok := nodeOf(snap, node)
	if !ok {
		f.t.Fatalf("no node %s", node)
	}
	rid := "rel-" + plan + "-" + node
	now := f.clock()
	if err := storeseed.RecordRelationship(context.Background(), f.s, store.Relationship{ID: rid, IssueKey: n.IssueKey, Status: "active", ParentTaskID: "parent", ChildTaskID: "child-" + node,
		Generation: 1, ArtifactRoots: "[" + jsonString(f.t.TempDir()) + "]", AllowedRecipients: "[\"parent\"]", CreatedAt: now, UpdatedAt: now},
		store.Generation{RelationshipID: rid, Number: 1, DispatchRequestID: "dispatch-" + rid, AnchorState: store.AnchorBound,
			DispatchTurnID: sql.NullString{String: "turn-dispatch", Valid: true}, OpenedAt: now, BoundAt: sql.NullString{String: now, Valid: true}}, "host", "host"); err != nil {
		f.t.Fatal(err)
	}
	manifest := f.putManifest(snap, node, []any{})
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES (?, ?, ?, 1, ?, 'initial', ?)",
		plan, node, rid, manifest, ReleaseRequestID(node, manifest))
	return rid
}

// holdSlots records n held execution slots of the project, as releases would have reserved them.
func (f *fixture) holdSlots(n int) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("held-%d", i)
		if err := f.s.InsertExecutionSlot(context.Background(), store.ExecutionSlotsRow{SlotID: "slot-" + key, SubjectKind: "dag_node", SubjectKey: key, ParentTaskID: "parent", ProjectKey: "P-TEST",
			Tenure: 1, State: "held", ReservedBy: "parent", ReservedAt: f.clock()}); err != nil {
			f.t.Fatal(err)
		}
	}
}

// declareLimit declares an enforced limit of a scope.
func (f *fixture) declareLimit(scopeKind, scopeKey, dimension string, ceiling float64) {
	f.t.Helper()
	at := f.clock()
	if err := f.s.DeclareExecutionLimit(context.Background(), store.ExecutionLimitsRow{LimitID: "lim-" + scopeKind + "-" + dimension, ScopeKind: scopeKind, ScopeKey: scopeKey,
		Dimension: dimension, Unit: dimension, Ceiling: ceiling, Enforce: 1, DeclaredBy: "parent", Source: "test", Revision: 1, DeclaredAt: at, UpdatedAt: at}); err != nil {
		f.t.Fatal(err)
	}
}

// read is the ready set of a plan.
func (f *fixture) read(plan string) Reading {
	f.t.Helper()
	reading, err := f.sched.Read(context.Background(), plan, ReadyOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	return reading
}

// node is one node's reading.
func (r Reading) node(id string) NodeReading {
	for _, n := range r.Nodes {
		if n.NodeID == id {
			return n
		}
	}
	panic("no node " + id + " in the reading")
}

func (r Reading) readyIDs() []string {
	out := make([]string, len(r.Ready))
	for i, n := range r.Ready {
		out[i] = n.NodeID
	}
	return out
}

// declare records a declaration of one tree/file region per path for a node.
func (f *fixture) declare(plan, node string, paths ...string) RegionDeclaration {
	f.t.Helper()
	var regions []Region
	for _, p := range paths {
		regions = append(regions, Region{Repository: "owner/repo", Path: p, Kind: "file", Change: "edit"})
	}
	d, err := f.sched.DeclareRegions(context.Background(), plan, node, "parent", regions)
	if err != nil {
		f.t.Fatal(err)
	}
	return d
}

// brief is one line per node: id, state, disposition, reason.
func (r Reading) brief() string {
	var b strings.Builder
	for _, n := range r.Nodes {
		fmt.Fprintf(&b, "%s[%s %s %s] ", n.NodeID, n.State, n.Disposition, n.Reason)
	}
	return b.String()
}

func contextBackground() context.Context { return context.Background() }

func nullText(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
