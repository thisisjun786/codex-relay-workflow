package dagsched

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

const head1 = "1111111111111111111111111111111111111111"

// forkJoinPlan is the plan the edge tests share (contract 8.1 E-01 plus a stacked pair):
//
//	research -(e0 artifact_verified)-> design -(e1 artifact_verified)-> impl-a -(e3 integrated)-> join -(e5 decision)-> ship
//	design -(e2 artifact_verified)-> impl-b -(e4 integrated)-> join
//	impl-a -(e8 artifact_verified, code pin)-> stack2
//
// An artifact_verified edge that leaves an implementation node always pins its head (the plan validator's rule).
func forkJoinPlan(f *fixture, plan string) {
	f.t.Helper()
	pin := doc{"pins_code_head": true, "target_repository": "owner/repo", "target_base_ref": "dev"}
	f.putPlan(plan, 0, plan+"-r1",
		addNode("design", dag.NodeNonPR), addNode("impl-a", dag.NodeImplementation), addNode("impl-b", dag.NodeImplementation),
		addNode("join", dag.NodeNonPR), addNode("ship", dag.NodeNonPR), addNode("research", dag.NodeNonPR), addNode("stack2", dag.NodeImplementation),
		addEdge("e0", "research", "design", dag.EdgeArtifactVerified, nil), addEdge("e1", "design", "impl-a", dag.EdgeArtifactVerified, nil), addEdge("e2", "design", "impl-b", dag.EdgeArtifactVerified, nil),
		addEdge("e3", "impl-a", "join", dag.EdgeIntegrated, nil), addEdge("e4", "impl-b", "join", dag.EdgeIntegrated, nil),
		addEdge("e5", "join", "ship", dag.EdgeDecision, nil),
		addEdge("e8", "impl-a", "stack2", dag.EdgeArtifactVerified, pin))
}

func TestEdgeArtifactVerified(t *testing.T) {
	type tc struct {
		name   string
		edge   string
		reason string // "" = satisfied
		setup  func(f *fixture, a accepted)
		node   string
		opts   acceptOpts
	}
	pinned := acceptOpts{HeadSHA: head1, PR: 7, Forge: "owner/repo", Repository: "owner/repo"}
	cases := []tc{
		{name: "baseline: accepted, current", edge: "e1", node: "design"},
		{name: "no acceptance at all", edge: "e1", node: "", reason: WaitEdge("e1")},
		{name: "a column the identity covers was altered", edge: "e1", node: "design", reason: BlockedAcceptanceTampered,
			setup: func(f *fixture, a accepted) {
				f.exec("UPDATE dag_acceptances SET revision_hash = ? WHERE acceptance_id = ?", dig("other"), a.Acceptance.AcceptanceID)
			}},
		{name: "a required column is blank", edge: "e1", node: "design", reason: BlockedAcceptanceIncomplete,
			setup: func(f *fixture, a accepted) {
				f.exec("UPDATE dag_acceptances SET ack_tier = '' WHERE acceptance_id = ?", a.Acceptance.AcceptanceID)
			}},
		{name: "the acceptance is not tied to an execution of the node", edge: "e1", node: "design", reason: BlockedInputUnaccepted,
			setup: func(f *fixture, a accepted) { f.exec("DELETE FROM dag_node_executions") }},
		{name: "the plan changed the node's criteria", edge: "e1", node: "design", reason: BlockedStaleCriteria,
			setup: func(f *fixture, a accepted) {
				node := nodeDoc("design", dag.NodeNonPR)
				node["criteria_set_digest"] = dig("new criteria")
				f.putPlan("p1", 1, "p1-r2", doc{"op": dag.OpUpdateNode, "node": node})
			}},
		{name: "the registered criteria were replaced", edge: "e1", node: "design", reason: BlockedStaleCriteria,
			setup: func(f *fixture, a accepted) {
				f.exec("UPDATE canonical_criteria SET set_digest = ?", dig("registered again"))
			}},
		{name: "re-verified under the new criteria: the revalidation makes the edge current again", edge: "e1", node: "design",
			setup: func(f *fixture, a accepted) {
				node := nodeDoc("design", dag.NodeNonPR)
				node["criteria_set_digest"] = dig("new criteria")
				f.putPlan("p1", 1, "p1-r2", doc{"op": dag.OpUpdateNode, "node": node})
				f.exec("UPDATE canonical_criteria SET set_digest = ?", dig("new criteria"))
				f.exec("INSERT INTO dag_acceptance_revalidations (revalidation_id, acceptance_id, criteria_set_digest, event_id, verdict_turn_id, reval_seq, revalidated_by, revalidated_at) VALUES ('rv1', ?, ?, ?, 'vt2', 1, 'parent', 't')",
					a.Acceptance.AcceptanceID, dig("new criteria"), a.Event)
			}},
		{name: "the latest revalidation is the effective digest (B, then C, then B again)", edge: "e1", node: "design",
			setup: func(f *fixture, a accepted) {
				node := nodeDoc("design", dag.NodeNonPR)
				node["criteria_set_digest"] = dig("criteria B")
				f.putPlan("p1", 1, "p1-r2", doc{"op": dag.OpUpdateNode, "node": node})
				f.exec("UPDATE canonical_criteria SET set_digest = ?", dig("criteria B"))
				for i, d := range []string{"criteria C", "criteria B"} {
					f.exec("INSERT INTO dag_acceptance_revalidations (revalidation_id, acceptance_id, criteria_set_digest, event_id, verdict_turn_id, reval_seq, revalidated_by, revalidated_at) VALUES (?, ?, ?, ?, 'vt', ?, 'parent', 't')",
						fmt.Sprintf("rv%d", i+1), a.Acceptance.AcceptanceID, dig(d), a.Event, i+1)
				}
			}},
		{name: "an older revalidation does not stand in for the latest (B, then C)", edge: "e1", node: "design", reason: BlockedStaleCriteria,
			setup: func(f *fixture, a accepted) {
				node := nodeDoc("design", dag.NodeNonPR)
				node["criteria_set_digest"] = dig("criteria B")
				f.putPlan("p1", 1, "p1-r2", doc{"op": dag.OpUpdateNode, "node": node})
				f.exec("UPDATE canonical_criteria SET set_digest = ?", dig("criteria B"))
				for i, d := range []string{"criteria B", "criteria C"} {
					f.exec("INSERT INTO dag_acceptance_revalidations (revalidation_id, acceptance_id, criteria_set_digest, event_id, verdict_turn_id, reval_seq, revalidated_by, revalidated_at) VALUES (?, ?, ?, ?, 'vt', ?, 'parent', 't')",
						fmt.Sprintf("rv%d", i+1), a.Acceptance.AcceptanceID, dig(d), a.Event, i+1)
				}
			}},
		{name: "a revision superseded the accepted head", edge: "e1", node: "design", reason: BlockedStaleHead,
			setup: func(f *fixture, a accepted) {
				f.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at) VALUES ('evt-2', ?, 1, ?, 'ready_for_review', 'child', 'child-design', 'turn-2', 'completed', '{}', 'final', 't', 't')",
					a.Acceptance.RelationshipID, dig("second"))
				f.exec("INSERT INTO revision_lineage (relationship_id, execution_generation, event_id, revision_hash, supersedes_hash, declared_by, recorded_at) VALUES (?, 1, 'evt-2', ?, ?, 'child', 't')",
					a.Acceptance.RelationshipID, dig("second"), a.Acceptance.RevisionHash)
			}},
		{name: "the relationship moved to a new generation", edge: "e1", node: "design", reason: BlockedStaleHead,
			setup: func(f *fixture, a accepted) { f.exec("UPDATE relationships SET execution_generation = 2") }},
		{name: "an archived relationship: the durable acceptance stands", edge: "e1", node: "design",
			setup: func(f *fixture, a accepted) {
				f.exec("UPDATE relationships SET status = 'archived'")
				f.exec("UPDATE relationships SET execution_generation = 2")
			}},
		{name: "a cancelled predecessor", edge: "e1", node: "design", reason: BlockedPredecessorCancelled,
			setup: func(f *fixture, a accepted) { f.exec("UPDATE relationships SET status = 'cancelled'") }},
		{name: "an implementation acceptance without its evidence digest", edge: "e8", node: "impl-a", reason: BlockedAcceptanceIncomplete, opts: pinned,
			setup: func(f *fixture, a accepted) { f.exec("UPDATE dag_acceptances SET evidence_digest = NULL") }},
		{name: "an acceptance without its time", edge: "e1", node: "design", reason: BlockedAcceptanceIncomplete,
			setup: func(f *fixture, a accepted) { f.exec("UPDATE dag_acceptances SET accepted_at = ''") }},
		{name: "a pinned acceptance without its head", edge: "e8", node: "impl-a", reason: BlockedAcceptanceIncomplete, opts: acceptOpts{PR: 7, Forge: "owner/repo", Repository: "owner/repo"}},
		{name: "a pinned acceptance without its forge row", edge: "e8", node: "impl-a", reason: BlockedAcceptanceIncomplete, opts: acceptOpts{HeadSHA: head1, PR: 7, Repository: "owner/repo"}},
		{name: "a pinned acceptance for another repository", edge: "e8", node: "impl-a", reason: BlockedAcceptanceIncomplete, opts: acceptOpts{HeadSHA: head1, PR: 7, Forge: "owner/repo", Repository: "elsewhere/repo"}},
		{name: "a complete pinned acceptance", edge: "e8", node: "impl-a", opts: pinned},
		{name: "the latest merge check saw another head", edge: "e8", node: "impl-a", reason: BlockedStaleHead, opts: pinned,
			setup: func(f *fixture, a accepted) { f.mergeCheck(a, "2222222222222222222222222222222222222222", true) }},
		{name: "the latest merge check no longer digests to what was recorded", edge: "e8", node: "impl-a", reason: BlockedEvidenceMismatch, opts: pinned,
			setup: func(f *fixture, a accepted) {
				f.mergeCheck(a, head1, true)
				f.exec("UPDATE dag_merge_checks SET checks_digest = ?", dig("forged"))
			}},
		{name: "the latest merge check saw the accepted head", edge: "e8", node: "impl-a", opts: pinned,
			setup: func(f *fixture, a accepted) { f.mergeCheck(a, head1, true) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			forkJoinPlan(f, "p1")
			var a accepted
			if c.node != "" {
				a = f.acceptNode("p1", c.node, c.opts)
			}
			if c.setup != nil {
				c.setup(f, a)
			}
			got := f.status("p1", c.edge)
			if c.reason == "" {
				if !got.Satisfied || got.Since == "" || got.AcceptanceID == "" {
					t.Fatalf("want satisfied with a stored time and the acceptance id, got %+v", got)
				}
				return
			}
			if got.Satisfied || got.Reason != c.reason {
				t.Fatalf("want %s, got %+v", c.reason, got)
			}
			if !ReasonsClosed(got.Reason) {
				t.Fatalf("%s is not a closed reason", got.Reason)
			}
		})
	}
}

// mergeCheck appends the relay's observation of the accepted pull request: its head and the evidence it was judged on.
func (f *fixture) mergeCheck(a accepted, observedHead string, eligible bool) {
	f.t.Helper()
	body := EvidenceBody{Checks: []CheckRow{{Name: "test", RunID: "1", HeadSHA: observedHead, Conclusion: "success", Attempt: 1}}, Required: []string{"test"}, ReviewDigest: dig("review")}
	seq := f.count("SELECT COALESCE(MAX(check_seq), 0) + 1 FROM dag_merge_checks WHERE acceptance_id = ?", a.Acceptance.AcceptanceID)
	f.exec("INSERT INTO dag_merge_checks (check_id, acceptance_id, check_seq, head_sha, observed_head_sha, base_tip_sha, checks_digest, evidence_json, failed_required_json, round_no, outcome, reason, recorded_at)"+
		" VALUES (?, ?, ?, ?, ?, 'tip', ?, ?, '[]', 1, 'eligible', 'test', 't')",
		fmt.Sprintf("chk-%s-%d", a.Acceptance.AcceptanceID[:8], seq), a.Acceptance.AcceptanceID, seq, a.Acceptance.HeadSHA, observedHead, EvidenceDigest(body), body.JSON())
}

// E-25: what an accepted node consumed must still be what its predecessors' acceptances are. The manifest names the research acceptance's real
// id, which only exists once research is accepted, so the case is built in that order.
func TestEdgeStalePredecessorFrankenbuild(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	research := f.acceptNode("p1", "research", acceptOpts{})
	input := doc{"edge_id": "e0", "kind": dag.EdgeArtifactVerified, "from_node_id": "research", "acceptance_id": research.Acceptance.AcceptanceID,
		"relationship_id": research.Acceptance.RelationshipID, "event_id": research.Event, "revision_hash": research.Acceptance.RevisionHash, "execution_generation": int64(1)}
	f.acceptNode("p1", "design", acceptOpts{Inputs: []any{input}})
	if st := f.status("p1", "e1"); !st.Satisfied {
		t.Fatalf("design accepted on research's current acceptance: want satisfied, got %+v", st)
	}
	// research is accepted again after a correction: the old acceptance is superseded, so what design consumed is no longer current.
	f.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE acceptance_id = ?", research.Acceptance.AcceptanceID)
	if st := f.status("p1", "e1"); st.Satisfied || st.Reason != BlockedStalePredecessor {
		t.Fatalf("want %s after research's acceptance was superseded, got %+v", BlockedStalePredecessor, st)
	}
}

// Node and edge ids are plan-local (dag_zone.go): the same ids in two plans are different nodes, and the contract's SQL, which
// predates the plan id, would let one plan's acceptance open another's edge.
func TestPredicatesAreScopedByPlan(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	forkJoinPlan(f, "p2")
	f.acceptNode("p2", "design", acceptOpts{})
	a := f.acceptNode("p2", "impl-a", acceptOpts{HeadSHA: head1, PR: 7, Forge: "owner/repo", Repository: "owner/repo"})
	f.integrate(a, "owner/repo", "dev", true, true)
	for _, edge := range []string{"e1", "e3"} {
		if st := f.status("p1", edge); st.Satisfied || st.Reason != WaitEdge(edge) {
			t.Errorf("plan p1 edge %s: want %s, got %+v", edge, WaitEdge(edge), st)
		}
		if st := f.status("p2", edge); !st.Satisfied {
			t.Errorf("plan p2 edge %s: want satisfied, got %+v", edge, st)
		}
	}
}

// integrate records an ancestor observation of the accepted head and, optionally, the parent's merged mark on the same revision.
func (f *fixture) integrate(a accepted, repository, ref string, ancestor, mark bool) {
	f.t.Helper()
	seq := f.count("SELECT COALESCE(MAX(observed_seq), 0) + 1 FROM dag_integration_observations WHERE acceptance_id = ? AND repository = ? AND base_ref = ?", a.Acceptance.AcceptanceID, repository, ref)
	flag := 0
	if ancestor {
		flag = 1
	}
	f.exec("INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, observed_seq, observed_at) VALUES (?, ?, ?, ?, ?, 'tip', ?, 'git merge-base --is-ancestor', ?, ?)",
		fmt.Sprintf("obs-%s-%s-%s-%d", a.Acceptance.AcceptanceID[:8], strings.ReplaceAll(repository, "/", "_"), ref, seq), a.Acceptance.AcceptanceID, repository, ref, a.Acceptance.HeadSHA, flag, seq, f.clock())
	if mark {
		f.exec("INSERT OR IGNORE INTO assignment_marks (relationship_id, mark, event_id, execution_generation, revision_hash, evidence, actor, marked_at) VALUES (?, 'merged', ?, ?, ?, 'merged', 'parent', ?)",
			a.Acceptance.RelationshipID, a.Event, a.Acceptance.ExecutionGeneration, a.Acceptance.RevisionHash, f.clock())
	}
}

func (f *fixture) landedTurn(repository, ref, head string) {
	f.t.Helper()
	f.exec("INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, candidate_head, state, tenure, requested_at, updated_at) VALUES (?, 'tgt', ?, ?, 'P-TEST', 'parent', 'host', ?, 'landed', 1, 't', 't')",
		"mtn-"+head[:8], repository, ref, head)
}

// linkTurn records a merge turn and names it as the carrier of every observation the fixture wrote.
func (f *fixture) linkTurn(state, repository, ref, head string) {
	f.t.Helper()
	id := fmt.Sprintf("mtn-%s-%s-%s-%s", state, strings.ReplaceAll(repository, "/", "_"), ref, head[:6])
	f.exec("INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, candidate_head, state, tenure, requested_at, updated_at) VALUES (?, 'tgt', ?, ?, 'P-TEST', 'parent', 'host', ?, ?, 1, 't', 't')",
		id, repository, ref, head, state)
	f.exec("UPDATE dag_integration_observations SET merge_turn_id = ?", id)
}

func TestEdgeIntegrated(t *testing.T) {
	pinned := acceptOpts{HeadSHA: head1, PR: 7, Forge: "owner/repo", Repository: "owner/repo"}
	type tc struct {
		name   string
		reason string
		setup  func(f *fixture, a accepted)
	}
	cases := []tc{
		{name: "no observation", reason: WaitEdge("e3")},
		{name: "contained, but the parent recorded no merge", reason: WaitEdge("e3"), setup: func(f *fixture, a accepted) { f.integrate(a, "owner/repo", "dev", true, false) }},
		{name: "contained and marked merged", setup: func(f *fixture, a accepted) { f.integrate(a, "owner/repo", "dev", true, true) }},
		{name: "contained, but marked on another revision", reason: WaitEdge("e3"), setup: func(f *fixture, a accepted) {
			f.integrate(a, "owner/repo", "dev", true, false)
			f.exec("INSERT INTO assignment_marks (relationship_id, mark, event_id, execution_generation, revision_hash, evidence, actor, marked_at) VALUES (?, 'merged', 'other-event', 1, ?, 'merged', 'parent', 't')", a.Acceptance.RelationshipID, dig("other"))
		}},
		{name: "contained in another branch only", reason: WaitEdge("e3"), setup: func(f *fixture, a accepted) { f.integrate(a, "owner/repo", "release", true, true) }},
		{name: "not contained after a merge turn landed this head (a squash)", reason: BlockedIntegrationUnprovable, setup: func(f *fixture, a accepted) {
			f.integrate(a, "owner/repo", "dev", false, true)
			f.landedTurn("owner/repo", "dev", head1)
		}},
		{name: "not contained and nothing landed", reason: WaitEdge("e3"), setup: func(f *fixture, a accepted) { f.integrate(a, "owner/repo", "dev", false, true) }},
		{name: "a later observation says the head is no longer contained", reason: WaitEdge("e3"), setup: func(f *fixture, a accepted) {
			f.integrate(a, "owner/repo", "dev", true, true)
			f.integrate(a, "owner/repo", "dev", false, false)
		}},
		{name: "contained again after losing it", setup: func(f *fixture, a accepted) {
			f.integrate(a, "owner/repo", "dev", true, true)
			f.integrate(a, "owner/repo", "dev", false, false)
			f.integrate(a, "owner/repo", "dev", true, false)
		}},
		{name: "a cancelled predecessor whose merge had already landed stays integrated", setup: func(f *fixture, a accepted) {
			f.integrate(a, "owner/repo", "dev", true, true)
			f.exec("UPDATE relationships SET status = 'cancelled'")
		}},
		{name: "a cancelled predecessor that never landed", reason: BlockedPredecessorCancelled, setup: func(f *fixture, a accepted) {
			f.exec("UPDATE relationships SET status = 'cancelled'")
		}},
		{name: "the observation names a merge turn that does not exist", reason: WaitEdge("e3"), setup: func(f *fixture, a accepted) {
			f.integrate(a, "owner/repo", "dev", true, true)
			f.exec("UPDATE dag_integration_observations SET merge_turn_id = 'mtn-nothing'")
		}},
		{name: "the observation is carried by a turn that landed this head on this target", setup: func(f *fixture, a accepted) {
			f.integrate(a, "owner/repo", "dev", true, true)
			f.linkTurn("landed", "owner/repo", "dev", head1)
		}},
		{name: "the carrying turn has not landed (it is merging)", reason: WaitEdge("e3"), setup: func(f *fixture, a accepted) {
			f.integrate(a, "owner/repo", "dev", true, true)
			f.linkTurn("merging", "owner/repo", "dev", head1)
		}},
		{name: "the carrying turn landed another head", reason: WaitEdge("e3"), setup: func(f *fixture, a accepted) {
			f.integrate(a, "owner/repo", "dev", true, true)
			f.linkTurn("landed", "owner/repo", "dev", "3333333333333333333333333333333333333333")
		}},
		{name: "the carrying turn landed on another repository", reason: WaitEdge("e3"), setup: func(f *fixture, a accepted) {
			f.integrate(a, "owner/repo", "dev", true, true)
			f.linkTurn("landed", "elsewhere/repo", "dev", head1)
		}},
		{name: "the carrying turn landed on another branch", reason: WaitEdge("e3"), setup: func(f *fixture, a accepted) {
			f.integrate(a, "owner/repo", "dev", true, true)
			f.linkTurn("landed", "owner/repo", "release", head1)
		}},
		{name: "an old negative observation, then a positive one still waiting for its mark, is not a squash", reason: WaitEdge("e3"), setup: func(f *fixture, a accepted) {
			f.integrate(a, "owner/repo", "dev", false, false)
			f.landedTurn("owner/repo", "dev", head1)
			f.integrate(a, "owner/repo", "dev", true, false)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			forkJoinPlan(f, "p1")
			a := f.acceptNode("p1", "impl-a", pinned)
			if c.setup != nil {
				c.setup(f, a)
			}
			got := f.status("p1", "e3")
			if c.reason == "" {
				if !got.Satisfied || got.Since == "" {
					t.Fatalf("want satisfied, got %+v", got)
				}
				return
			}
			if got.Satisfied || got.Reason != c.reason {
				t.Fatalf("want %s, got %+v", c.reason, got)
			}
		})
	}
}

// A terminal node has no outgoing edge, so its target is not an edge's: integratedAt is keyed by the acceptance and the target.
func TestIntegratedAtIsKeyedByTarget(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	a := f.acceptNode("p1", "ship", acceptOpts{HeadSHA: head1, PR: 7, Forge: "owner/repo", Repository: "owner/repo"})
	f.integrate(a, "elsewhere/repo", "main", true, true)
	ctx := context.Background()
	got, err := f.sched.integratedAt(ctx, f.s.Q(ctx), "p1", a.Acceptance, "elsewhere/repo", "main")
	if err != nil || !got.Satisfied {
		t.Fatalf("a terminal node's own target: %+v %v", got, err)
	}
	if other, _ := f.sched.integratedAt(ctx, f.s.Q(ctx), "p1", a.Acceptance, "owner/repo", "dev"); other.Satisfied {
		t.Error("integration at one target satisfied another")
	}
}

func (f *fixture) decision(subject, digest, disposition, kind string, revision int) {
	f.t.Helper()
	f.exec("INSERT INTO dag_decisions (decision_id, plan_id, subject, digest, disposition, authority_kind, authority_ref, revision, state, recorded_by_task_id, coordinator_epoch, recorded_at) VALUES (?, 'p1', ?, ?, ?, ?, 'ref', ?, 'active', 'parent', 0, 't')",
		fmt.Sprintf("dec-%d", revision), subject, digest, disposition, kind, revision)
}

func TestEdgeDecision(t *testing.T) {
	digest := dig("subject e5")
	type tc struct {
		name   string
		reason string
		setup  func(f *fixture)
	}
	// ownerSince is when the project's owner binding began; a directive is decided at 00:05, so an owner who began later was not the owner when it was settled.
	directive := func(f *fixture, ownerSince string, supersededAt string, chosen bool) {
		now := "2026-10-01T00:00:00Z"
		ctx := context.Background()
		for _, b := range []store.ScopeBindingsRow{
			{BindingID: "b-sup", Role: "supervisor", ScopeKind: "initiative", ScopeKey: "INIT", TaskID: "sup", HostID: "host", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now},
			{BindingID: "b-own", Role: "parent", ScopeKind: "project", ScopeKey: "P-TEST", TaskID: "parent", HostID: "host", Status: "active", Revision: 1, CreatedAt: ownerSince, UpdatedAt: ownerSince},
		} {
			if err := storeseed.InsertScopeBinding(ctx, f.s, b); err != nil {
				t.Fatal(err)
			}
		}
		if err := storeseed.InsertScopeLink(ctx, f.s, store.ScopeLinksRow{LinkID: "l1", LinkKind: "execution", UpperKind: "initiative", UpperKey: "INIT", UpperTaskID: "sup", LowerKind: "project", LowerKey: "P-TEST", LowerTaskID: "parent", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if supersededAt != "" {
			if err := storeseed.ArchiveScopeBinding(ctx, f.s, "b-own", "archived", "b-next", supersededAt); err != nil {
				t.Fatal(err)
			}
		}
		disposition := sql.NullString{String: "chosen", Valid: chosen}
		f.exec("INSERT INTO scope_directives (directive_id, scope_kind, scope_key, from_task_id, from_scope_key, link_id, link_kind, digest, revision, disposition, decided_by, decided_at, recorded_at) VALUES ('d1', 'project', 'P-TEST', 'sup', 'INIT', 'l1', 'execution', ?, 1, ?, 'parent', '2026-10-01T00:05:00Z', ?)", digest, disposition, now)
	}
	cases := []tc{
		{name: "nothing recorded", reason: DeferAuthorityPending},
		{name: "an approved decision by a named authority", setup: func(f *fixture) { f.decision("merge holds", digest, "approved", "user", 1) }},
		{name: "the other named authority", setup: func(f *fixture) { f.decision("merge holds", digest, "approved", "owner", 1) }},
		{name: "an authority the edge does not name", reason: BlockedDecisionMismatch, setup: func(f *fixture) { f.decision("merge holds", digest, "approved", "stranger", 1) }},
		{name: "another digest", reason: BlockedDecisionMismatch, setup: func(f *fixture) { f.decision("merge holds", dig("elsewhere"), "approved", "user", 1) }},
		{name: "a rejected decision is no approval", reason: DeferAuthorityPending, setup: func(f *fixture) { f.decision("merge holds", digest, "rejected", "user", 1) }},
		{name: "a decision about another subject", reason: DeferAuthorityPending, setup: func(f *fixture) { f.decision("other", digest, "approved", "user", 1) }},
		{name: "a supervisor directive settled by the project's owner", setup: func(f *fixture) { directive(f, "2026-10-01T00:00:00Z", "", true) }},
		{name: "a directive not yet settled", reason: DeferAuthorityPending, setup: func(f *fixture) { directive(f, "2026-10-01T00:00:00Z", "", false) }},
		{name: "a directive settled by someone who became owner later", reason: DeferAuthorityPending, setup: func(f *fixture) { directive(f, "2026-10-01T00:09:00Z", "", true) }},
		{name: "a directive settled after the owner binding was superseded", reason: DeferAuthorityPending, setup: func(f *fixture) { directive(f, "2026-10-01T00:00:00Z", "2026-10-01T00:02:00Z", true) }},
		{name: "a directive settled by the owner who was replaced afterwards", setup: func(f *fixture) { directive(f, "2026-10-01T00:00:00Z", "2026-10-01T00:09:00Z", true) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			forkJoinPlan(f, "p1")
			if c.setup != nil {
				c.setup(f)
			}
			got := f.status("p1", "e5")
			if c.reason == "" {
				if !got.Satisfied || got.Since == "" {
					t.Fatalf("want satisfied, got %+v", got)
				}
				return
			}
			if got.Satisfied || got.Reason != c.reason {
				t.Fatalf("want %s, got %+v", c.reason, got)
			}
		})
	}
}

// Rule 6 binds the consumed acceptance to the plan and to the node the manifest names: another plan's acceptance, or another node's, is no
// predecessor of this one.
func TestEdgeStalePredecessorIsScopedByPlanAndNode(t *testing.T) {
	consume := func(t *testing.T, build func(f *fixture) accepted, from string) EdgeStatus {
		f := newFixture(t)
		forkJoinPlan(f, "p1")
		forkJoinPlan(f, "p2")
		other := build(f)
		input := doc{"edge_id": "e0", "kind": dag.EdgeArtifactVerified, "from_node_id": from, "acceptance_id": other.Acceptance.AcceptanceID,
			"relationship_id": other.Acceptance.RelationshipID, "event_id": other.Event, "revision_hash": other.Acceptance.RevisionHash, "execution_generation": int64(1)}
		f.acceptNode("p1", "design", acceptOpts{Inputs: []any{input}})
		return f.status("p1", "e1")
	}
	t.Run("an active acceptance of the same node in another plan", func(t *testing.T) {
		got := consume(t, func(f *fixture) accepted { return f.acceptNode("p2", "research", acceptOpts{}) }, "research")
		if got.Satisfied || got.Reason != BlockedStalePredecessor {
			t.Fatalf("want %s, got %+v", BlockedStalePredecessor, got)
		}
	})
	t.Run("an active acceptance of another node of the plan", func(t *testing.T) {
		got := consume(t, func(f *fixture) accepted { return f.acceptNode("p1", "join", acceptOpts{}) }, "research")
		if got.Satisfied || got.Reason != BlockedStalePredecessor {
			t.Fatalf("want %s, got %+v", BlockedStalePredecessor, got)
		}
	})
	t.Run("the control: the node's own active acceptance in this plan", func(t *testing.T) {
		got := consume(t, func(f *fixture) accepted { return f.acceptNode("p1", "research", acceptOpts{}) }, "research")
		if !got.Satisfied {
			t.Fatalf("want satisfied, got %+v", got)
		}
	})
}
