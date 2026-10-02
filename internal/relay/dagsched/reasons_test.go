package dagsched

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// consumes is the manifest input that names an accepted predecessor.
func consumes(edge, from string, a accepted) doc {
	return doc{"edge_id": edge, "kind": dag.EdgeArtifactVerified, "from_node_id": from, "acceptance_id": a.Acceptance.AcceptanceID,
		"relationship_id": a.Acceptance.RelationshipID, "event_id": a.Event, "revision_hash": a.Acceptance.RevisionHash, "execution_generation": int64(1)}
}

// managedRow records a managed-start request the way the engine leaves it in a given state.
func (f *fixture) managedRow(request, issue, state, receipt string) {
	f.t.Helper()
	var status any
	if receipt != "" {
		status = receipt
	}
	f.exec("INSERT INTO managed_start_requests (request_id, issue_key, request_fingerprint, fingerprint_version, workspace, marker_root, socket_identity, create_request_id, dispatch_request_id,"+
		" state, revision, receipt_status, created_at, updated_at) VALUES (?, ?, 'fp', '1', 'ws', 'mr', 'sock', ?, ?, ?, 0, ?, 't', 't')", request, issue, "create-"+request, "dispatch-"+request, state, status)
}

// releaseRow records a decided release (the intent the release path writes before it starts a child).
func (f *fixture) releaseRow(plan, node string) (digest, request string) {
	f.t.Helper()
	digest = dig("manifest " + plan + node)
	request = ReleaseRequestID(node, digest)
	f.exec("INSERT INTO dag_releases (plan_id, node_id, manifest_digest, managed_request_id, coordinator_epoch, decided_at) VALUES (?, ?, ?, ?, 0, ?)", plan, node, digest, request, f.clock())
	return digest, request
}

// foreignRelationship is an open relationship for an issue that no DAG release made.
func (f *fixture) foreignRelationship(issue string) {
	f.t.Helper()
	now := f.clock()
	if err := storeseed.RecordRelationship(context.Background(), f.s, store.Relationship{ID: "rel-foreign-" + issue, IssueKey: issue, Status: "active", ParentTaskID: "other-parent", ChildTaskID: "other-child",
		Generation: 1, ArtifactRoots: "[]", AllowedRecipients: "[\"other-parent\"]", CreatedAt: now, UpdatedAt: now},
		store.Generation{RelationshipID: "rel-foreign-" + issue, Number: 1, DispatchRequestID: "dispatch-foreign-" + issue, AnchorState: store.AnchorBound,
			DispatchTurnID: nullText("turn-x"), OpenedAt: now, BoundAt: nullText(now)}, "host", "host"); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) mergeTurn(id, repository, ref, state, head string) {
	f.t.Helper()
	f.exec("INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, candidate_head, state, tenure, requested_at, updated_at) VALUES (?, 'tgt', ?, ?, 'P-TEST', 'parent', 'host', ?, ?, 1, 't', 't')",
		id, repository, ref, head, state)
}

// Every reason a reading gives is a member of the closed set, and each situation below yields its own reason: a reading that said only "waiting" would
// leave the parent guessing. The table is the activation scenario of criterion c1 for the wait reasons that need no release machinery.
func TestReadyReasonsAreClosed(t *testing.T) {
	type tc struct {
		name   string
		setup  func(f *fixture)
		node   string
		reason string // "" = ready
		state  string
	}
	baseline := func(f *fixture) {
		forkJoinPlan(f, "p1")
		f.projectParent()
	}
	design := func(f *fixture) accepted { // research accepted: design is the node under test
		baseline(f)
		return f.acceptNode("p1", "research", acceptOpts{})
	}
	cases := []tc{
		{name: "an edge not yet satisfied", setup: baseline, node: "design", reason: WaitEdge("e0"), state: StateWaiting},
		{name: "a node with no incoming edge is ready", setup: baseline, node: "research", state: StateReady},
		{name: "no registered parent", setup: func(f *fixture) { forkJoinPlan(f, "p1") }, node: "research", reason: DeferOwnershipUnverified, state: StateReady},
		{name: "an open relationship of another owner for the issue", setup: func(f *fixture) { baseline(f); f.foreignRelationship("CRW-research") }, node: "research", reason: SkipAlreadyOwned, state: StateReady},
		{name: "a managed start in flight for the issue", setup: func(f *fixture) { baseline(f); f.managedRow("req-foreign", "CRW-research", "reserved", "") }, node: "research", reason: SkipAlreadyOwned, state: StateReady},
		{name: "a released managed start does not own the issue", setup: func(f *fixture) { baseline(f); f.managedRow("req-old", "CRW-research", "released", "") }, node: "research", state: StateReady},
		{name: "research accepted: design is ready", setup: func(f *fixture) { design(f) }, node: "design", state: StateReady},
		{name: "decision pending", setup: func(f *fixture) { baseline(f); f.acceptNode("p1", "join", acceptOpts{}) }, node: "ship", reason: DeferAuthorityPending, state: StateWaiting},
		{name: "the predecessor was cancelled", setup: func(f *fixture) { baseline(f); f.acceptNode("p1", "research", acceptOpts{Status: "cancelled"}) }, node: "design", reason: BlockedPredecessorCancelled, state: StateWaiting},
		{name: "the predecessor's artifact changed on disk", setup: func(f *fixture) {
			a := design(f)
			if err := os.WriteFile(a.Files[0], []byte("tampered\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, node: "design", reason: BlockedInputHashMismatch, state: StateWaiting},
		{name: "the predecessor's artifact changed without changing its size", setup: func(f *fixture) {
			a := design(f)
			if err := os.WriteFile(a.Files[0], []byte("artifact of rEsearch\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, node: "design", reason: BlockedInputHashMismatch, state: StateWaiting},
		{name: "the receipt declares an empty list of artifacts", setup: func(f *fixture) {
			a := design(f)
			f.exec("UPDATE events SET receipt = '{\"manifest\":[]}' WHERE event_id = ?", a.Event)
		}, node: "design", reason: BlockedInputMissing, state: StateWaiting},
		{name: "the predecessor's artifact grew", setup: func(f *fixture) {
			a := design(f)
			f.exec("UPDATE events SET receipt = replace(receipt, '\"bytes\":21', '\"bytes\":22') WHERE event_id = ?", a.Event)
		}, node: "design", reason: BlockedInputHashMismatch, state: StateWaiting},
		{name: "the predecessor's artifact is gone", setup: func(f *fixture) {
			a := design(f)
			if err := os.Remove(a.Files[0]); err != nil {
				t.Fatal(err)
			}
		}, node: "design", reason: BlockedInputMissing, state: StateWaiting},
		{name: "the receipt names a file outside the predecessor's roots", setup: func(f *fixture) {
			a := design(f)
			outside := f.t.TempDir() + "/elsewhere.md"
			if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			f.exec("UPDATE events SET receipt = replace(receipt, ?, ?) WHERE event_id = ?", a.Files[0], outside, a.Event)
		}, node: "design", reason: BlockedInputOutOfScope, state: StateWaiting},
		{name: "the receipt declares no artifact at all", setup: func(f *fixture) {
			baseline(f)
			f.acceptNode("p1", "research", acceptOpts{NoManifest: true})
		}, node: "design", reason: BlockedInputMissing, state: StateWaiting},
		{name: "the frozen copy of the manifest is unreadable", setup: func(f *fixture) {
			a := design(f)
			f.exec("UPDATE events SET manifest_ref = ? WHERE event_id = ?", f.t.TempDir()+"/nope", a.Event)
		}, node: "design", reason: BlockedInputMissing, state: StateWaiting},
		{name: "an accepted predecessor whose criteria changed", setup: func(f *fixture) {
			design(f)
			node := nodeDoc("research", dag.NodeNonPR)
			node["criteria_set_digest"] = dig("new criteria")
			f.putPlan("p1", 1, "p1-r2", doc{"op": dag.OpUpdateNode, "node": node})
		}, node: "design", reason: BlockedStaleCriteria, state: StateWaiting},
		{name: "the merge lane is moving the branch a join builds on", setup: func(f *fixture) {
			baseline(f)
			a := f.acceptNode("p1", "impl-a", pinnedOpts)
			b := f.acceptNode("p1", "impl-b", acceptOpts{HeadSHA: "2222222222222222222222222222222222222222", PR: 8, Forge: "owner/repo", Repository: "owner/repo"})
			f.integrate(a, "owner/repo", "dev", true, true)
			f.integrate(b, "owner/repo", "dev", true, true)
			f.mergeTurn("mt-moving", "owner/repo", "dev", "merging", "3333333333333333333333333333333333333333")
		}, node: "join", reason: DeferMergeWindow, state: StateReady},
		{name: "a merge turn on another branch does not close the window", setup: func(f *fixture) {
			baseline(f)
			a := f.acceptNode("p1", "impl-a", pinnedOpts)
			b := f.acceptNode("p1", "impl-b", acceptOpts{HeadSHA: "2222222222222222222222222222222222222222", PR: 8, Forge: "owner/repo", Repository: "owner/repo"})
			f.integrate(a, "owner/repo", "dev", true, true)
			f.integrate(b, "owner/repo", "dev", true, true)
			f.mergeTurn("mt-other", "owner/repo", "release", "merging", "3333333333333333333333333333333333333333")
		}, node: "join", state: StateReady},
		{name: "a merge turn with an unknown effect is a moving tip", setup: func(f *fixture) {
			baseline(f)
			a := f.acceptNode("p1", "impl-a", pinnedOpts)
			b := f.acceptNode("p1", "impl-b", acceptOpts{HeadSHA: "2222222222222222222222222222222222222222", PR: 8, Forge: "owner/repo", Repository: "owner/repo"})
			f.integrate(a, "owner/repo", "dev", true, true)
			f.integrate(b, "owner/repo", "dev", true, true)
			f.mergeTurn("mt-unknown", "owner/repo", "dev", "unknown", "3333333333333333333333333333333333333333")
		}, node: "join", reason: DeferMergeWindow, state: StateReady},
		{name: "a landed merge turn is not moving anything", setup: func(f *fixture) {
			baseline(f)
			a := f.acceptNode("p1", "impl-a", pinnedOpts)
			b := f.acceptNode("p1", "impl-b", acceptOpts{HeadSHA: "2222222222222222222222222222222222222222", PR: 8, Forge: "owner/repo", Repository: "owner/repo"})
			f.integrate(a, "owner/repo", "dev", true, true)
			f.integrate(b, "owner/repo", "dev", true, true)
			f.mergeTurn("mt-landed", "owner/repo", "dev", "landed", "3333333333333333333333333333333333333333")
		}, node: "join", state: StateReady},
		{name: "an unproven integration", setup: func(f *fixture) {
			baseline(f)
			f.acceptNode("p1", "impl-a", pinnedOpts)
		}, node: "join", reason: WaitEdge("e3"), state: StateWaiting},
		{name: "the pull request head moved after acceptance", setup: func(f *fixture) {
			baseline(f)
			a := f.acceptNode("p1", "impl-a", pinnedOpts)
			f.mergeCheck(a, "9999999999999999999999999999999999999999", false)
		}, node: "stack2", reason: BlockedStaleHead, state: StateWaiting},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			c.setup(f)
			reading := f.read("p1")
			n := reading.node(c.node)
			if n.Reason != c.reason {
				t.Fatalf("%s = %s (%s), want reason %q: %s", c.node, n.Reason, n.Detail, c.reason, reading.brief())
			}
			if c.reason == "" && n.Disposition != DispReady {
				t.Fatalf("%s = %+v, want ready", c.node, n)
			}
			if n.State != c.state {
				t.Fatalf("%s state %q, want %q", c.node, n.State, c.state)
			}
			for _, other := range reading.Nodes {
				if other.Reason != "" && !ReasonsClosed(other.Reason) {
					t.Errorf("%s carries %q, which is not in the closed set", other.NodeID, other.Reason)
				}
				if other.Reason == "" && other.Disposition != DispReady {
					t.Errorf("%s has no reason and is %s", other.NodeID, other.Disposition)
				}
				if !strings.HasPrefix(other.Reason, other.Disposition) && other.Disposition != DispReady {
					t.Errorf("%s: reason %q does not belong to disposition %q", other.NodeID, other.Reason, other.Disposition)
				}
			}
		})
	}
}

// B-14 (E-25): a node must not be built from two versions of one predecessor, however far down the second version enters. G consumes E and C;
// E rests on B, which rested on the first acceptance of A, and C rests on the second. Every edge into G is satisfied on its own.
func TestReadyFrankenbuildClosure(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("fk", 0, "fk-r1", addNode("A", dag.NodeNonPR), addNode("B", dag.NodeNonPR), addNode("E", dag.NodeNonPR), addNode("C", dag.NodeNonPR), addNode("G", dag.NodeNonPR),
		addEdge("ab", "A", "B", dag.EdgeArtifactVerified, nil), addEdge("be", "B", "E", dag.EdgeArtifactVerified, nil), addEdge("ac", "A", "C", dag.EdgeArtifactVerified, nil),
		addEdge("eg", "E", "G", dag.EdgeArtifactVerified, nil), addEdge("cg", "C", "G", dag.EdgeArtifactVerified, nil))
	a1 := f.acceptNode("fk", "A", acceptOpts{})
	b := f.acceptNode("fk", "B", acceptOpts{Inputs: []any{consumes("ab", "A", a1)}})
	e := f.acceptNode("fk", "E", acceptOpts{Inputs: []any{consumes("be", "B", b)}})
	if g := f.read("fk").node("C"); g.Disposition != DispReady {
		t.Fatalf("C after A's first acceptance = %+v", g)
	}
	f.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE acceptance_id = ?", a1.Acceptance.AcceptanceID)
	a2 := f.acceptNode("fk", "A", acceptOpts{Suffix: "-2"})
	f.acceptNode("fk", "C", acceptOpts{Inputs: []any{consumes("ac", "A", a2)}})
	_ = e
	// B still rests on the superseded acceptance, so B's own successors are stale; but E (accepted on B) and C (accepted on A's second acceptance) are
	// each current for their direct inputs, and only the closure sees that G would mix A's two versions.
	reading := f.read("fk")
	if g := reading.node("G"); g.Reason != BlockedInconsistentInputs {
		t.Fatalf("G = %+v (%s), want %s", g, reading.brief(), BlockedInconsistentInputs)
	}
	// the control: both branches on the second acceptance read clean.
	f2 := newFixture(t)
	f2.projectParent()
	f2.putPlan("fk", 0, "fk-r1", addNode("A", dag.NodeNonPR), addNode("C", dag.NodeNonPR), addNode("G", dag.NodeNonPR),
		addEdge("ac", "A", "C", dag.EdgeArtifactVerified, nil), addEdge("ag", "A", "G", dag.EdgeArtifactVerified, nil), addEdge("cg", "C", "G", dag.EdgeArtifactVerified, nil))
	acc := f2.acceptNode("fk", "A", acceptOpts{})
	f2.acceptNode("fk", "C", acceptOpts{Inputs: []any{consumes("ac", "A", acc)}})
	if g := f2.read("fk").node("G"); g.Disposition != DispReady {
		t.Fatalf("G with consistent inputs = %+v", g)
	}
}

// The store half of the input checks does not read files: it is what the release path re-judges under its lock.
func TestReadySkipArtifactBytes(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	f.projectParent()
	a := f.acceptNode("p1", "research", acceptOpts{})
	if err := os.WriteFile(a.Files[0], []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	full := f.read("p1")
	if got := full.node("design").Reason; got != BlockedInputHashMismatch {
		t.Fatalf("full reading: design = %s", got)
	}
	skipped, err := f.sched.Read(context.Background(), "p1", ReadyOptions{SkipArtifactBytes: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := skipped.node("design"); got.Disposition != DispReady {
		t.Fatalf("store half: design = %+v, want ready (the bytes are the file half's)", got)
	}
}

// The states of nodes somebody already owns (contract 3.1).
func TestReadyOwnedNodeStates(t *testing.T) {
	type tc struct {
		name   string
		setup  func(f *fixture)
		node   string
		state  string
		reason string
	}
	cases := []tc{
		{name: "running", setup: func(f *fixture) { f.startNode("p1", "research") }, node: "research", state: StateRunning, reason: SkipAlreadyOwned},
		{name: "a release decided, nothing started", setup: func(f *fixture) { f.releaseRow("p1", "research") }, node: "research", state: StateReleasing, reason: SkipAlreadyOwned},
		{name: "a reserved managed start", setup: func(f *fixture) {
			_, req := f.releaseRow("p1", "research")
			f.managedRow(req, "CRW-research", "reserved", "")
		}, node: "research", state: StateReleasing, reason: SkipAlreadyOwned},
		{name: "an armed start whose outcome is unknown", setup: func(f *fixture) {
			_, req := f.releaseRow("p1", "research")
			f.managedRow(req, "CRW-research", "create_armed", "unknown")
		}, node: "research", state: StateCreationUnknown, reason: BlockedCreationUnknown},
		{name: "an armed start that was never answered", setup: func(f *fixture) {
			_, req := f.releaseRow("p1", "research")
			f.managedRow(req, "CRW-research", "create_armed", "")
		}, node: "research", state: StateCreationUnknown, reason: BlockedCreationUnknown},
		{name: "an armed start that was accepted but not bound", setup: func(f *fixture) {
			_, req := f.releaseRow("p1", "research")
			f.managedRow(req, "CRW-research", "create_armed", "accepted")
		}, node: "research", state: StateReleasing, reason: SkipAlreadyOwned},
		{name: "an attached start whose bind never ran", setup: func(f *fixture) {
			_, req := f.releaseRow("p1", "research")
			f.managedRow(req, "CRW-research", "attached", "accepted")
		}, node: "research", state: StateReleasing, reason: SkipAlreadyOwned},
		{name: "a released managed start", setup: func(f *fixture) {
			_, req := f.releaseRow("p1", "research")
			f.managedRow(req, "CRW-research", "released", "")
		}, node: "research", state: StateReleasing, reason: BlockedReleaseAbandoned},
		{name: "the child reported blocked_needs_input", setup: func(f *fixture) {
			rid := f.startNode("p1", "research")
			f.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
				" VALUES ('evt-blocked', ?, 1, ?, 'blocked_needs_input', 'child', 'child-research', 'turn-2', 'completed', '{\"note\":\"hash mismatch\"}', 'final', 'z', 'z')", rid, dig("blocked"))
		}, node: "research", state: StateReported, reason: BlockedInputUnverifiedAtUse},
		{name: "a later report supersedes the blocked receipt", setup: func(f *fixture) {
			rid := f.startNode("p1", "research")
			f.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
				" VALUES ('evt-blocked', ?, 1, ?, 'blocked_needs_input', 'child', 'child-research', 'turn-2', 'completed', '{}', 'final', 'a', 'a')", rid, dig("blocked"))
			f.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
				" VALUES ('evt-report', ?, 1, ?, 'ready_for_review', 'child', 'child-research', 'turn-3', 'completed', '{}', 'final', 'b', 'b')", rid, dig("report"))
		}, node: "research", state: StateReported, reason: SkipAlreadyOwned},
		{name: "an execution row whose relationship is not in the store", setup: func(f *fixture) {
			snap := f.snapshot("p1")
			m := f.putManifest(snap, "research", []any{})
			f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind) VALUES ('p1', 'research', 'rel-ghost', 1, ?, 'initial')", m)
		}, node: "research", state: StateAmbiguousNode, reason: BlockedAmbiguousHead},
		{name: "paused", setup: func(f *fixture) {
			f.startNode("p1", "research")
			f.exec("UPDATE relationships SET status = 'paused'")
		}, node: "research", state: StatePausedNode, reason: SkipAlreadyOwned},
		{name: "cancelled", setup: func(f *fixture) {
			f.startNode("p1", "research")
			f.exec("UPDATE relationships SET status = 'cancelled'")
		}, node: "research", state: StateCancelled, reason: SkipAlreadyOwned},
		{name: "accepted non_pr", setup: func(f *fixture) { f.acceptNode("p1", "research", acceptOpts{}) }, node: "research", state: StateAccepted, reason: DoneAccepted},
		{name: "accepted implementation, not landed", setup: func(f *fixture) { f.acceptNode("p1", "impl-a", pinnedOpts) }, node: "impl-a", state: StateAccepted, reason: DoneAccepted},
		{name: "integrated implementation", setup: func(f *fixture) {
			a := f.acceptNode("p1", "impl-a", pinnedOpts)
			f.integrate(a, "owner/repo", "dev", true, true)
		}, node: "impl-a", state: StateIntegrated, reason: DoneIntegrated},
		{name: "evicted from the merge lane", setup: func(f *fixture) {
			a := f.acceptNode("p1", "impl-a", pinnedOpts)
			f.mergeCheck(a, head1, true)
			f.exec("INSERT INTO dag_merge_checks (check_id, acceptance_id, check_seq, head_sha, observed_head_sha, base_tip_sha, checks_digest, evidence_json, failed_required_json, round_no, outcome, reason, recorded_at)"+
				" VALUES ('chk-evict', ?, 2, ?, ?, 'tip', ?, ?, '[\"test\"]', 2, 'evicted', 'again', 't')", a.Acceptance.AcceptanceID, head1, head1,
				EvidenceDigest(EvidenceBody{Checks: []CheckRow{{Name: "test", RunID: "2", HeadSHA: head1, Conclusion: "failure", Attempt: 2}}, Required: []string{"test"}, ReviewDigest: dig("review")}),
				EvidenceBody{Checks: []CheckRow{{Name: "test", RunID: "2", HeadSHA: head1, Conclusion: "failure", Attempt: 2}}, Required: []string{"test"}, ReviewDigest: dig("review")}.JSON())
		}, node: "impl-a", state: StateAccepted, reason: BlockedEvicted},
		{name: "a landing whose effect is unknown", setup: func(f *fixture) {
			f.acceptNode("p1", "impl-a", pinnedOpts)
			f.mergeTurn("mt-unknown", "owner/repo", "dev", "unknown", head1)
		}, node: "impl-a", state: StateAccepted, reason: BlockedEffectUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			forkJoinPlan(f, "p1")
			f.projectParent()
			c.setup(f)
			reading := f.read("p1")
			n := reading.node(c.node)
			if n.State != c.state || n.Reason != c.reason {
				t.Fatalf("%s = {state %s, reason %s, detail %s}, want {%s, %s}", c.node, n.State, n.Reason, n.Detail, c.state, c.reason)
			}
			if !ReasonsClosed(n.Reason) {
				t.Fatalf("%q is not in the closed set", n.Reason)
			}
		})
	}
}
