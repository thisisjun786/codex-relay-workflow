package dagsched

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// Contract 8.2: a stale result or stale criteria never opens an edge. After a node's acceptance each change below must close what the acceptance opened (or, for an archived relationship,
// leave the durable acceptance standing), with the reason the contract names.
func TestStaleResultNeverOpensAnEdge(t *testing.T) {
	cases := []struct {
		name   string
		reason string // "" = still satisfied
		mutate func(k *releaseKit, a accepted)
	}{
		{name: "baseline", reason: ""},
		{name: "a newer revision supersedes the accepted one", reason: BlockedStaleHead, mutate: func(k *releaseKit, a accepted) { k.supersedeReport(a.Acceptance.RelationshipID, "A", "rp", "newer") }},
		{name: "the criteria were registered again", reason: BlockedStaleCriteria, mutate: func(k *releaseKit, a accepted) {
			k.exec("UPDATE canonical_criteria SET set_digest = ?", dig("registered again"))
		}},
		{name: "a new generation opened", reason: BlockedStaleHead, mutate: func(k *releaseKit, a accepted) {
			k.exec("UPDATE relationships SET execution_generation = 2")
		}},
		{name: "the relationship ended (archived): the durable acceptance stands", reason: "", mutate: func(k *releaseKit, a accepted) {
			k.exec("UPDATE relationships SET status = 'archived'")
		}},
		{name: "the predecessor was cancelled", reason: BlockedPredecessorCancelled, mutate: func(k *releaseKit, a accepted) {
			k.exec("UPDATE relationships SET status = 'cancelled'")
		}},
		{name: "the plan changed the node's criteria", reason: BlockedStaleCriteria, mutate: func(k *releaseKit, a accepted) {
			n := relNode("A", dag.NodeNonPR)
			n["criteria_set_digest"] = dig("another plan criterion")
			k.putPlan("rp", int(k.snapshot("rp").Revision), "rp-r2", doc{"op": dag.OpUpdateNode, "node": n})
		}},
		{name: "the acceptance was superseded", reason: WaitEdge("ab"), mutate: func(k *releaseKit, a accepted) {
			k.exec("UPDATE dag_acceptances SET state = 'superseded'")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			k.reportNode("rp", "A", acceptOpts{})
			res, err := k.accept("rp", "A", AcceptInput{})
			if err != nil {
				t.Fatal(err)
			}
			if st := k.status("rp", "ab"); !st.Satisfied || st.AcceptanceID != res.AcceptanceID {
				t.Fatalf("the edge right after the acceptance = %+v", st)
			}
			a := accepted{Acceptance: Acceptance{RelationshipID: res.RelationshipID}}
			if c.mutate != nil {
				c.mutate(k, a)
			}
			st := k.status("rp", "ab")
			if c.reason == "" && !st.Satisfied {
				t.Fatalf("the edge = %+v, want it still satisfied", st)
			}
			if c.reason != "" && (st.Satisfied || st.Reason != c.reason) {
				t.Fatalf("the edge = %+v, want %s", st, c.reason)
			}
		})
	}
}

// Contract 8.2 on an integrated edge: a landing is a fact about a commit, not a reason to trust the result it belongs to. After the head landed (observation and merged mark), anything that
// makes the acceptance stale, altered or foreign closes what the landing opened, exactly as it does on an artifact edge.
func TestStaleResultNeverOpensAnIntegratedEdge(t *testing.T) {
	pinned := acceptOpts{HeadSHA: head1, PR: 7, Forge: "owner/repo", Repository: "owner/repo"}
	cases := []struct {
		name   string
		reason string // "" = still satisfied
		mutate func(f *fixture, a accepted)
	}{
		{name: "baseline"},
		{name: "the criteria were registered again", reason: BlockedStaleCriteria, mutate: func(f *fixture, a accepted) {
			f.exec("UPDATE canonical_criteria SET set_digest = ?", dig("registered again"))
		}},
		{name: "the plan changed the node's criteria", reason: BlockedStaleCriteria, mutate: func(f *fixture, a accepted) {
			n := nodeDoc("impl-a", dag.NodeImplementation)
			n["criteria_set_digest"] = dig("another plan criterion")
			f.putPlan("p1", int(f.snapshot("p1").Revision), "p1-r2", doc{"op": dag.OpUpdateNode, "node": n})
		}},
		{name: "the acceptance row was altered", reason: BlockedAcceptanceTampered, mutate: func(f *fixture, a accepted) {
			f.exec("UPDATE dag_acceptances SET revision_hash = ?", dig("another revision"))
		}},
		{name: "a required column of the acceptance is blank", reason: BlockedAcceptanceIncomplete, mutate: func(f *fixture, a accepted) {
			f.exec("UPDATE dag_acceptances SET verdict_turn_id = ''")
		}},
		{name: "the acceptance is not tied to an execution of the node", reason: BlockedInputUnaccepted, mutate: func(f *fixture, a accepted) {
			f.exec("DELETE FROM dag_node_executions")
		}},
		{name: "the latest merge check was altered", reason: BlockedEvidenceMismatch, mutate: func(f *fixture, a accepted) {
			evidence := EvidenceBody{Required: []string{"dev-gate"}}.JSON()
			f.exec("INSERT INTO dag_merge_checks (check_id, acceptance_id, check_seq, head_sha, observed_head_sha, base_tip_sha, checks_digest, evidence_json, failed_required_json, round_no, outcome, reason, recorded_at)"+
				" VALUES ('dmc-x', ?, 1, ?, ?, ?, ?, ?, '[]', 1, 'eligible', 'x', 't')", a.Acceptance.AcceptanceID, head1, head1, head1, dig("not the digest"), evidence)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			forkJoinPlan(f, "p1")
			a := f.acceptNode("p1", "impl-a", pinned)
			f.integrate(a, "owner/repo", "dev", true, true)
			if st := f.status("p1", "e3"); !st.Satisfied {
				t.Fatalf("the edge right after the landing = %+v", st)
			}
			if c.mutate != nil {
				c.mutate(f, a)
			}
			st := f.status("p1", "e3")
			if c.reason == "" && !st.Satisfied {
				t.Fatalf("the edge = %+v, want it still satisfied", st)
			}
			if c.reason != "" && (st.Satisfied || st.Reason != c.reason) {
				t.Fatalf("the edge = %+v, want %s", st, c.reason)
			}
		})
	}
}
