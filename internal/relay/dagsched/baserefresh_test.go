package dagsched

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (s *refreshScenario) refreshRows() int {
	return s.count("SELECT COUNT(*) FROM dag_base_refreshes")
}

// Criteria c1 and c2 (CRW-430): the node whose accepted result is current, whose child opened generation 2 by hand only to refresh the base, and whose pull request landed with the merged mark on generation 2,
// is integrated once the relay has proved the refresh, and its slot goes back. The proof is the relay's own reading of git: three merges of dev, the second with a hand resolved conflict the parent names.
func TestBaseRefreshIntegratesTheNodeAndReturnsItsSlot(t *testing.T) {
	s := newRefreshScenario(t)
	s.openGeneration()
	s.refreshBase()
	s.land()
	others := map[string]string{"K": rvRecords(s.releaseKit, "g", "K"), "D": rvRecords(s.releaseKit, "g", "D"), "A": rvRecords(s.releaseKit, "g", "A")}
	acceptance := rvRecords(s.releaseKit, "g", "I")
	relationships := s.count("SELECT COUNT(*) FROM relationships")
	generations := s.count("SELECT COUNT(*) FROM generations")

	res, err := s.record("shared.json")
	if err != nil || res.Replayed || res.Seq != 1 || res.Generation != 2 || res.HeadSHA != s.head || res.EventID != s.event2 || res.RevisionHash != s.revision2 || res.AcceptanceID != s.accepted.Acceptance.AcceptanceID ||
		len(res.Steps) != 3 || strings.Join(res.Resolved, ",") != "shared.json" || !strings.HasPrefix(res.RefreshID, "dbr-") || res.BaseRef != "dev" || res.BaseRepository != s.repo.path {
		t.Fatalf("record = %v %+v", err, res)
	}
	if res.Steps[0].Previous != s.h1 || res.Steps[2].Head != s.head || res.Steps[0].Head != res.Steps[1].Previous || res.Steps[1].Head != res.Steps[2].Previous || len(res.Steps[0].Resolved) != 0 || len(res.Steps[2].Resolved) != 0 {
		t.Fatalf("the chain is not the three merges from the accepted head to the head, oldest first: %+v", res.Steps)
	}
	if got := res.Steps[1].Resolved; len(got) != 1 || got[0].Path != "shared.json" || got[0].Blob != s.repo.git("rev-parse", res.Steps[1].Head+":shared.json") {
		t.Fatalf("the resolved file is not recorded with the blob the head holds: %+v", got)
	}
	if got := rvRecords(s.releaseKit, "g", "I"); got != acceptance {
		t.Fatalf("recording the refresh changed the acceptance rows:\nbefore %s\nafter  %s", acceptance, got)
	}
	if s.slotHeld() == false {
		t.Fatal("the record itself returned the slot: only an integration does")
	}

	obs, err := s.observe()
	if err != nil || !obs.Integrated || !obs.MarkPresent || !obs.SlotReleased || len(obs.Observations) != 1 || obs.Observations[0].SubjectSHA != s.head || !obs.Observations[0].IsAncestor {
		t.Fatalf("observe after the record = %v %+v", err, obs)
	}
	if s.slotHeld() {
		t.Fatal("the slot of the integrated node is still held")
	}
	reading := s.read("g")
	if n := reading.node("I"); n.State != StateIntegrated || n.Disposition != DispDone || n.Reason != DoneIntegrated {
		t.Fatalf("I = %+v, want done:integrated", n)
	}
	if n := reading.node("K"); n.Disposition != DispReady {
		t.Fatalf("K = %+v, want ready: its integrated edge is satisfied", n)
	}
	if again, err := s.observe(); err != nil || !again.Integrated || again.SlotReleased {
		t.Fatalf("a further observation = %v %+v", err, again)
	}
	for node, before := range others {
		if now := rvRecords(s.releaseKit, "g", node); now != before {
			t.Fatalf("the records of the other node %s changed:\nbefore %s\nafter  %s", node, before, now)
		}
	}
	if s.count("SELECT COUNT(*) FROM relationships") != relationships || s.count("SELECT COUNT(*) FROM generations") != generations {
		t.Fatal("the path made a relationship or a generation")
	}
	// a node that landed is never run again, and its refresh is not recorded twice
	if _, err := s.record("shared.json"); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "landed") {
		t.Fatalf("record after the landing = %v", err)
	}
}

// The relay proves where a hand resolution can sit and cannot read what a hand put there, so the parent names exactly the files, after reading them.
func TestBaseRefreshNeedsTheHandResolvedFilesNamedExactly(t *testing.T) {
	s := newRefreshScenario(t)
	s.openGeneration()
	s.refreshBase()
	s.land()
	for name, named := range map[string][]string{"none named": nil, "another file": {"other.txt"}, "one too many": {"shared.json", "other.txt"}} {
		_, err := s.record(named...)
		if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "[shared.json]") || s.refreshRows() != 0 {
			t.Fatalf("%s: %v (rows %d)", name, err, s.refreshRows())
		}
	}
	if _, err := s.record("shared.json"); err != nil || s.refreshRows() != 1 {
		t.Fatalf("record = %v (rows %d)", err, s.refreshRows())
	}
}

// A refresh with no hand resolution names none, and naming a file nobody resolved is refused as well.
func TestBaseRefreshOfCleanMergesNamesNoFile(t *testing.T) {
	s := newRefreshScenario(t)
	s.openGeneration()
	s.repo.commit("other.txt", "dev one")
	s.head = s.mergeDev("merge dev 1", "", "")
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.head)
	if _, err := s.record("shared.json"); refusalReason(err) != "disposition_conflict" || s.refreshRows() != 0 {
		t.Fatalf("a file nobody resolved = %v", err)
	}
	res, err := s.record()
	if err != nil || len(res.Steps) != 1 || len(res.Resolved) != 0 {
		t.Fatalf("record = %v %+v", err, res)
	}
	// open pull request: the record exists before the merge, and the node integrates when it lands and the parent marks it
	if obs, err := s.observe(); err != nil || obs.Integrated || obs.Observations[0].SubjectSHA != s.head || obs.Observations[0].IsAncestor {
		t.Fatalf("observe before the landing = %v %+v", err, obs)
	}
	s.land()
	if obs, err := s.observe(); err != nil || !obs.Integrated || !obs.SlotReleased {
		t.Fatalf("observe after the landing = %v %+v", err, obs)
	}
}

// Criterion c1 (what the path refuses): a later generation whose head holds anything but merges of the base is not a refresh. Each case leaves no record and the node as it was.
func TestBaseRefreshRefusesWhatIsMoreThanMergesOfTheBase(t *testing.T) {
	cases := []struct {
		name, code string
		build      func(s *refreshScenario)
	}{
		{"a commit of the child's own on top of the merges", RefreshNotAMerge, func(s *refreshScenario) {
			s.refreshBase()
			s.repo.git("checkout", "-q", "feature")
			s.head = s.repo.commit("child.txt", "work of the child")
			s.repo.git("checkout", "-q", "dev")
		}},
		{"a merge that carries a file git does not merge from its parents", RefreshTreeDiffers, func(s *refreshScenario) {
			s.repo.commit("other.txt", "dev one")
			s.repo.git("checkout", "-q", "feature")
			s.repo.git("merge", "-q", "--no-ff", "--no-commit", "dev")
			s.repo.write("smuggled.txt", "not from the base")
			s.repo.git("add", "smuggled.txt")
			s.repo.git("commit", "-q", "-m", "merge dev with more")
			s.head = s.repo.git("rev-parse", "HEAD")
			s.repo.git("checkout", "-q", "dev")
		}},
		{"a merge that edits a file next to the files git could not merge", RefreshTreeDiffers, func(s *refreshScenario) {
			s.repo.write("shared.json", "version of dev\n")
			s.repo.git("add", "shared.json")
			s.repo.git("commit", "-q", "-m", "dev changes shared.json")
			s.repo.git("checkout", "-q", "feature")
			if _, err := s.repo.tryGit("merge", "-q", "--no-ff", "-m", "merge dev", "dev"); err == nil {
				t.Fatal("the merge was expected to conflict")
			}
			s.repo.write("shared.json", "resolved\n")
			s.repo.write("feature.txt", "feature, edited while resolving")
			s.repo.git("add", "-A")
			s.repo.git("commit", "-q", "-m", "merge dev")
			s.head = s.repo.git("rev-parse", "HEAD")
			s.repo.git("checkout", "-q", "dev")
		}},
		{"a merge that commits git's own conflict markers", RefreshTreeDiffers, func(s *refreshScenario) {
			s.repo.write("shared.json", "version of dev\n")
			s.repo.git("add", "shared.json")
			s.repo.git("commit", "-q", "-m", "dev changes shared.json")
			s.repo.git("checkout", "-q", "feature")
			if _, err := s.repo.tryGit("merge", "-q", "--no-ff", "-m", "merge dev", "dev"); err == nil {
				t.Fatal("the merge was expected to conflict")
			}
			s.repo.git("add", "shared.json")
			s.repo.git("commit", "-q", "-m", "merge dev")
			s.head = s.repo.git("rev-parse", "HEAD")
			s.repo.git("checkout", "-q", "dev")
		}},
		{"a merge of a branch that is not the base", RefreshNotFromBase, func(s *refreshScenario) {
			s.repo.git("checkout", "-q", "-b", "side", "dev")
			s.repo.commit("side.txt", "another branch")
			s.repo.git("checkout", "-q", "feature")
			s.repo.git("merge", "-q", "--no-ff", "-m", "merge side", "side")
			s.head = s.repo.git("rev-parse", "HEAD")
			s.repo.git("checkout", "-q", "dev")
		}},
		{"a merge of a branch the base itself merged", RefreshNotFromBase, func(s *refreshScenario) {
			s.repo.git("checkout", "-q", "-b", "landed-side", "dev")
			s.repo.commit("landed-side.txt", "a branch the base merged")
			s.repo.git("checkout", "-q", "dev")
			s.repo.git("merge", "-q", "--no-ff", "-m", "the base merges the side branch", "landed-side")
			s.repo.git("checkout", "-q", "feature")
			s.repo.git("merge", "-q", "--no-ff", "-m", "merge the side branch itself", "landed-side")
			s.head = s.repo.git("rev-parse", "HEAD")
			s.repo.git("checkout", "-q", "dev")
		}},
		{"the parents the other way round", RefreshNotBuiltOnAccepted, func(s *refreshScenario) {
			s.repo.commit("other.txt", "dev one")
			s.repo.git("checkout", "-q", "-b", "swapped", "dev")
			s.repo.git("merge", "-q", "--no-ff", "-m", "merge the feature into dev", "feature")
			s.head = s.repo.git("rev-parse", "HEAD")
			s.repo.git("checkout", "-q", "dev")
		}},
		{"a head that is the accepted head", RefreshNoUpdate, func(s *refreshScenario) {}},
		{"more merges than the bound", RefreshChainTooLong, func(s *refreshScenario) {
			for i := 0; i <= MaxRefreshHops; i++ {
				s.repo.commit("bulk.txt", strings.Repeat("x", i+1))
				s.head = s.mergeDev("merge dev", "", "")
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newRefreshScenario(t)
			s.openGeneration()
			c.build(s)
			s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.head)
			acceptance := rvRecords(s.releaseKit, "g", "I")
			_, err := s.record()
			if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "("+c.code+")") {
				// a chain that needs a named file reads as the naming refusal, which is the same refusal reason
				if _, again := s.record("shared.json"); refusalReason(again) != "disposition_conflict" || !strings.Contains(again.Error(), "("+c.code+")") {
					t.Fatalf("record = %v / %v, want a disposition_conflict naming %s", err, again, c.code)
				}
			}
			if s.refreshRows() != 0 || rvRecords(s.releaseKit, "g", "I") != acceptance || !s.slotHeld() {
				t.Fatalf("a refused refresh left rows (%d), changed the acceptance or returned the slot", s.refreshRows())
			}
		})
	}
}

// Where the node has another route or the generation is not one the parent verified, nothing is recorded: a stale node goes through its own route (which this path leaves as it was), a node that
// landed is done, a generation nobody ruled verified is not a result, and only the parent of the relationship records.
func TestBaseRefreshIsRefusedWhereTheNodeIsNotInThatState(t *testing.T) {
	ready := func(t *testing.T) *refreshScenario {
		s := newRefreshScenario(t)
		s.openGeneration()
		s.refreshBase()
		return s
	}
	t.Run("the relationship is still at the accepted generation", func(t *testing.T) {
		s := newRefreshScenario(t)
		if _, err := s.record(); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "still at generation 1") {
			t.Fatalf("record = %v", err)
		}
	})
	t.Run("the later generation was not acknowledged", func(t *testing.T) {
		s := ready(t)
		s.exec("DELETE FROM acks WHERE event_id = ?", s.event2)
		if _, err := s.record("shared.json"); refusalReason(err) != "not_acknowledged" || s.refreshRows() != 0 {
			t.Fatalf("record = %v", err)
		}
	})
	t.Run("the later generation was ruled needs_changes", func(t *testing.T) {
		s := ready(t)
		s.exec("UPDATE verdicts SET verdict = 'needs_changes' WHERE event_id = ?", s.event2)
		if _, err := s.record("shared.json"); refusalReason(err) != "disposition_conflict" || s.refreshRows() != 0 {
			t.Fatalf("record = %v", err)
		}
	})
	t.Run("a task that is not the parent", func(t *testing.T) {
		s := ready(t)
		if _, err := s.sched.RecordBaseRefresh(context.Background(), "g", "I", "someone-else", RefreshInput{Resolved: []string{"shared.json"}}); refusalReason(err) != "scope_role_mismatch" || s.refreshRows() != 0 {
			t.Fatalf("record = %v", err)
		}
	})
	t.Run("a paused relationship", func(t *testing.T) {
		s := ready(t)
		s.exec("UPDATE relationships SET status = 'paused' WHERE relationship_id = ?", s.rid)
		if _, err := s.record("shared.json"); refusalReason(err) != "relationship_not_active" || s.refreshRows() != 0 {
			t.Fatalf("record = %v", err)
		}
	})
	t.Run("a pull request that was closed", func(t *testing.T) {
		s := ready(t)
		pr := openPR("owner/repo", 7, s.head)
		pr.State = "closed"
		s.forge.by["owner/repo#7"] = pr
		if _, err := s.record("shared.json"); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "closed") || s.refreshRows() != 0 {
			t.Fatalf("record = %v", err)
		}
	})
	t.Run("a pull request based on a branch the node does not land on", func(t *testing.T) {
		s := ready(t)
		pr := openPR("owner/repo", 7, s.head)
		pr.BaseRef = "release"
		s.forge.by["owner/repo#7"] = pr
		if _, err := s.record("shared.json"); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "release") || s.refreshRows() != 0 {
			t.Fatalf("record = %v", err)
		}
	})
	t.Run("a commit the checkout does not hold", func(t *testing.T) {
		s := ready(t)
		s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, strings.Repeat("a", 40))
		if _, err := s.record("shared.json"); refusalReason(err) != "merge_target_unreadable" || !strings.Contains(err.Error(), "git fetch") || s.refreshRows() != 0 {
			t.Fatalf("record = %v", err)
		}
	})
	t.Run("a node that is not an implementation node, has no acceptance, or is not in the plan", func(t *testing.T) {
		s := ready(t)
		for node, reason := range map[string]string{"K": "disposition_conflict", "D": "disposition_conflict", "nope": "unregistered_scope"} {
			if _, err := s.sched.RecordBaseRefresh(context.Background(), "g", node, "parent", RefreshInput{}); refusalReason(err) != reason {
				t.Fatalf("node %s: %v, want %s", node, err, reason)
			}
		}
	})
	t.Run("a stale node keeps its route and is not refreshed", func(t *testing.T) {
		s := newRefreshScenario(t)
		s.rvReregister("g", "I", "g-r3", s.rid, dig("the second edition of I's criteria"), nil)
		reading := s.read("g")
		if n := reading.node("I"); n.Disposition != DispStale {
			t.Fatalf("I = %+v, want stale", n)
		}
		route := rvAction(t, reading, "I")
		if route != rvRevalidate {
			t.Fatalf("the route of a node whose criteria alone changed = %q, want %q", route, rvRevalidate)
		}
		if _, err := s.record(); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "stale") || s.refreshRows() != 0 {
			t.Fatalf("record on a stale node = %v", err)
		}
		if after := rvAction(t, s.read("g"), "I"); after != route {
			t.Fatalf("the refused record moved the route of the stale node: %q -> %q", route, after)
		}
	})
}

// The record is an append: the same chain again is a replay, a pull request that moved on (the child merged the base once more) is the next record proved again from the accepted head, and the node
// integrates on the head the newest record names.
func TestBaseRefreshReplayAndAPullRequestThatMovedOn(t *testing.T) {
	s := newRefreshScenario(t)
	s.openGeneration()
	s.refreshBase()
	first, err := s.record("shared.json")
	if err != nil || first.Seq != 1 {
		t.Fatalf("record = %v %+v", err, first)
	}
	again, err := s.record("shared.json")
	if err != nil || !again.Replayed || again.RefreshID != first.RefreshID || again.Seq != 1 || s.refreshRows() != 1 {
		t.Fatalf("replay = %v %+v (rows %d)", err, again, s.refreshRows())
	}
	s.repo.commit("other-four.txt", "dev four")
	// the base moved on: the replay answers with what the first record holds, not with the tip read now
	if moved, err := s.record("shared.json"); err != nil || !moved.Replayed || moved.BaseTipSHA != first.BaseTipSHA || len(moved.Steps) != 3 || moved.RefreshID != first.RefreshID || strings.Join(moved.Resolved, ",") != "shared.json" ||
		moved.Steps[1].Resolved[0].Blob != first.Steps[1].Resolved[0].Blob {
		t.Fatalf("replay after the base moved = %v %+v, want the first record's content", err, moved)
	}
	oldHead := s.head
	s.head = s.mergeDev("merge dev 4", "", "")
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.head)
	second, err := s.record("shared.json")
	if err != nil || second.Replayed || second.Seq != 2 || second.RefreshID == first.RefreshID || len(second.Steps) != 4 || second.Steps[2].Head != oldHead || second.Steps[3].Head != s.head {
		t.Fatalf("record after the pull request moved on = %v %+v", err, second)
	}
	s.land()
	obs, err := s.observe()
	if err != nil || !obs.Integrated || !obs.SlotReleased || obs.Observations[0].SubjectSHA != s.head {
		t.Fatalf("observe = %v %+v, want integrated on the newest head", err, obs)
	}
}

// What the readers trust: a row is a refresh only when it digests to its id; the rows are append-only; and a store that predates the table (a read-only open never creates the zone) reads as a node with
// no refresh.
func TestBaseRefreshRowsAreTrustedOnlyWhenTheyDigestToTheirId(t *testing.T) {
	forge := func(s *refreshScenario, seq int, id, head string) {
		s.exec("INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json,"+
			" recorded_by_task_id, coordinator_epoch, recorded_at) VALUES (?, ?, ?, ?, 2, ?, ?, ?, ?, 'dev', ?, '{}', '[]', 'someone', 0, 't')",
			id, s.accepted.Acceptance.AcceptanceID, seq, s.rid, s.event2, s.revision2, head, s.repo.path, head)
	}
	t.Run("a row written by hand opens nothing", func(t *testing.T) {
		s := newRefreshScenario(t)
		s.openGeneration()
		s.refreshBase()
		s.land()
		forge(s, 1, "dbr-0123456789abcdef0123456789abcdef", s.head)
		obs, err := s.observe()
		if err != nil || obs.Integrated || obs.SlotReleased || obs.Observations[0].SubjectSHA != s.h1 {
			t.Fatalf("observe = %v %+v, want the accepted head observed and no integration", err, obs)
		}
	})
	t.Run("a forged newer row does not displace the proved one", func(t *testing.T) {
		s := newRefreshScenario(t)
		s.openGeneration()
		s.refreshBase()
		s.land()
		if _, err := s.record("shared.json"); err != nil {
			t.Fatal(err)
		}
		forge(s, 2, "dbr-fedcba9876543210fedcba9876543210", s.h1)
		obs, err := s.observe()
		if err != nil || !obs.Integrated || obs.Observations[0].SubjectSHA != s.head {
			t.Fatalf("observe = %v %+v", err, obs)
		}
	})
	t.Run("rows are never updated or deleted", func(t *testing.T) {
		s := newRefreshScenario(t)
		s.openGeneration()
		s.refreshBase()
		if _, err := s.record("shared.json"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.s.DB.Exec("UPDATE dag_base_refreshes SET head_sha = ?", s.h1); err == nil {
			t.Fatal("a refresh was updated")
		}
		if _, err := s.s.DB.Exec("DELETE FROM dag_base_refreshes"); err == nil {
			t.Fatal("a refresh was deleted")
		}
	})
	t.Run("a store without the table", func(t *testing.T) {
		s := newRefreshScenario(t)
		s.openGeneration()
		s.refreshBase()
		s.land()
		s.exec("DROP TABLE dag_base_refreshes")
		obs, err := s.observe()
		if err != nil || obs.Integrated || obs.Observations[0].SubjectSHA != s.h1 {
			t.Fatalf("observe on a store that predates the table = %v %+v", err, obs)
		}
		if n := s.read("g").node("I"); n.State == StateIntegrated {
			t.Fatalf("I = %+v", n)
		}
	})
}

// An edge that waits for the node's verified result (artifact_verified) keeps reading the accepted generation: this path moves what integration is judged on and nothing else, so while the relationship is at
// the later generation the edge reads blocked:stale_head before and after the record, as the release of a successor that pins the accepted head reads merge_candidate_moved.
func TestBaseRefreshLeavesAnArtifactEdgeReadingTheAcceptedGeneration(t *testing.T) {
	s := newRefreshScenario(t)
	if st := s.status("g", "ia"); !st.Satisfied {
		t.Fatalf("the edge before generation 2 = %+v", st)
	}
	s.openGeneration()
	s.refreshBase()
	if st := s.status("g", "ia"); st.Satisfied || st.Reason != BlockedStaleHead {
		t.Fatalf("the edge while generation 2 is open = %+v, want blocked:stale_head", st)
	}
	if _, err := s.record("shared.json"); err != nil {
		t.Fatal(err)
	}
	if st := s.status("g", "ia"); st.Satisfied || st.Reason != BlockedStaleHead {
		t.Fatalf("the edge after the record = %+v, want it unchanged", st)
	}
}

// Whoever claimed the plan after this session holds the epoch: the record is fenced like every command that decides.
func TestBaseRefreshIsFencedByTheCoordinatorEpoch(t *testing.T) {
	s := newRefreshScenario(t)
	s.openGeneration()
	s.refreshBase()
	claim := s.claim(s.sched, "g", "parent", "nonce-one")
	s.sched.ExpectedEpoch = claim.Epoch + 1
	if _, err := s.record("shared.json"); refusalReason(err) != "stale_coordinator_epoch" || s.refreshRows() != 0 {
		t.Fatalf("record under another epoch = %v (rows %d)", err, s.refreshRows())
	}
	s.sched.ExpectedEpoch = claim.Epoch
	if _, err := s.record("shared.json"); err != nil || s.refreshRows() != 1 {
		t.Fatalf("record under the epoch held = %v (rows %d)", err, s.refreshRows())
	}
}

// The report the parent ruled verified names the head it describes; the head the forge shows has to be that head (a report that names none leaves nothing to compare, an abbreviation counts).
func TestBaseRefreshWantsTheForgeHeadTheReportNames(t *testing.T) {
	report := func(s *refreshScenario, head string) {
		s.exec("INSERT INTO work_reports (event_id, submission_no, relationship_id, execution_generation, revision_hash, repository, pr_number, pr_url, head_sha, cxc_status, cxc_reason, contract_version, summary, next_action, recorded_at)"+
			" VALUES (?, 1, ?, 2, ?, 'owner/repo', 7, NULL, ?, 'DONE', 'proved', 'v1', 'done', 'merge', 't')", s.event2, s.rid, s.revision2, head)
	}
	t.Run("another head", func(t *testing.T) {
		s := newRefreshScenario(t)
		s.openGeneration()
		s.refreshBase()
		report(s, s.h1)
		if _, err := s.record("shared.json"); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "names the head") || s.refreshRows() != 0 {
			t.Fatalf("record = %v (rows %d)", err, s.refreshRows())
		}
	})
	t.Run("the head, abbreviated", func(t *testing.T) {
		s := newRefreshScenario(t)
		s.openGeneration()
		s.refreshBase()
		report(s, s.head[:12])
		if _, err := s.record("shared.json"); err != nil || s.refreshRows() != 1 {
			t.Fatalf("record = %v (rows %d)", err, s.refreshRows())
		}
	})
}

// A proof that finishes after another record landed does not put the acceptance back: the call is refused and repeated.
func TestBaseRefreshProvedWhileAnotherRecordLandsIsRefused(t *testing.T) {
	s := newRefreshScenario(t)
	s.openGeneration()
	s.refreshBase()
	slow := s.head
	s.sched.testBeforeRefreshTx = func() {
		s.sched.testBeforeRefreshTx = nil
		s.repo.commit("other-late.txt", "dev late")
		s.head = s.mergeDev("merge dev late", "", "")
		s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.head)
		if _, err := s.record("shared.json"); err != nil {
			t.Errorf("the record that landed first = %v", err)
		}
		s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, slow)
	}
	if _, err := s.record("shared.json"); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "while this one was being proved") {
		t.Fatalf("the slow record = %v", err)
	}
	if s.refreshRows() != 1 {
		t.Fatalf("rows = %d, want only the record that landed first", s.refreshRows())
	}
	var head string
	if err := s.s.DB.QueryRow("SELECT head_sha FROM dag_base_refreshes").Scan(&head); err != nil || head == slow {
		t.Fatalf("the stand holds %s (%v), want the newer head", head, err)
	}
}

// The path leaves the correction route of a result that is current as it was: dag-correct still refuses it after the record.
func TestBaseRefreshDoesNotOpenTheCorrectionRoute(t *testing.T) {
	s := newRefreshScenario(t)
	s.openGeneration()
	s.refreshBase()
	if _, err := s.record("shared.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sched.RecordCorrection(context.Background(), "g", "I", "parent", ""); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "is not stale") {
		t.Fatalf("dag-correct = %v", err)
	}
}

// The documentation says what was built: the command, the closed codes, the readers and the limits.
func TestBaseRefreshIsDocumented(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/relay/dag-scheduler.md")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, want := range []string{"dag-base-refresh", "dag_base_refreshes", RefreshNoUpdate, RefreshNotBuiltOnAccepted, RefreshNotAMerge, RefreshNotFromBase, RefreshTreeDiffers, RefreshChainTooLong, "--resolved", "--checkout"} {
		if !strings.Contains(page, want) {
			t.Errorf("docs/relay/dag-scheduler.md does not mention %s", want)
		}
	}
}

func TestCLIBaseRefreshRefusals(t *testing.T) {
	state, _ := cliState(t)
	if out, code := crw(t, state, "dag-base-refresh", "--plan", "p1", "--node", "research", "--actor", "parent"); code != 2 || parseOut(t, out)["reason"] != "disposition_conflict" {
		t.Fatalf("a non_pr node: exit %d\n%s", code, out)
	}
	if out, code := crw(t, state, "dag-base-refresh", "--plan", "p1", "--node", "nope", "--actor", "parent"); code != 2 || parseOut(t, out)["reason"] != "unregistered_scope" {
		t.Fatalf("an unknown node: exit %d\n%s", code, out)
	}
	if out, code := crw(t, state, "dag-base-refresh", "--plan", "p1", "--actor", "parent"); code != 2 {
		t.Fatalf("without --node: exit %d\n%s", code, out)
	}
}

// A conflict marker is as wide as the conflict-marker-size attribute of the first parent says: a file committed with three-character markers is as unresolved as one with seven.
func TestBaseRefreshRefusesCommittedMarkersOfTheWidthTheAttributesSet(t *testing.T) {
	s := newRefreshScenarioWith(t, func(repo *gitRepo) {
		repo.write(".gitattributes", "shared.json conflict-marker-size=3\n")
		repo.git("add", ".gitattributes")
	})
	s.openGeneration()
	s.repo.write("shared.json", "version of dev\n")
	s.repo.git("add", "shared.json")
	s.repo.git("commit", "-q", "-m", "dev changes shared.json")
	s.repo.git("checkout", "-q", "feature")
	if _, err := s.repo.tryGit("merge", "-q", "--no-ff", "-m", "merge dev", "dev"); err == nil {
		t.Fatal("the merge was expected to conflict")
	}
	if raw, err := os.ReadFile(filepath.Join(s.repo.path, "shared.json")); err != nil || !strings.Contains(string(raw), "<<< ") || strings.Contains(string(raw), "<<<<<<<") {
		t.Fatalf("git did not write three character markers: %q (%v)", raw, err)
	}
	s.repo.git("add", "shared.json")
	s.repo.git("commit", "-q", "-m", "merge dev")
	s.head = s.repo.git("rev-parse", "HEAD")
	s.repo.git("checkout", "-q", "dev")
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.head)
	if _, err := s.record("shared.json"); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "conflict markers") || s.refreshRows() != 0 {
		t.Fatalf("record = %v (rows %d)", err, s.refreshRows())
	}
}

// A conflict that has no markers (a file under the binary merge) is resolved by keeping one side: the resolved file is then the one git wrote, and it is a resolution all the same, named by the parent.
func TestBaseRefreshAcceptsAConflictResolvedByKeepingASide(t *testing.T) {
	s := newRefreshScenarioWith(t, func(repo *gitRepo) {
		repo.write(".gitattributes", "data.bin merge=binary\n")
		repo.write("data.bin", "from the feature")
		repo.git("add", ".gitattributes", "data.bin")
	})
	s.openGeneration()
	s.repo.commit("data.bin", "from dev")
	s.repo.git("checkout", "-q", "feature")
	if _, err := s.repo.tryGit("merge", "-q", "--no-ff", "-m", "merge dev", "dev"); err == nil {
		t.Fatal("the merge was expected to conflict")
	}
	s.repo.git("checkout", "--ours", "data.bin")
	s.repo.git("add", "data.bin")
	s.repo.git("commit", "-q", "-m", "merge dev, the feature's data.bin kept")
	s.head = s.repo.git("rev-parse", "HEAD")
	s.repo.git("checkout", "-q", "dev")
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.head)
	if _, err := s.record(); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "[data.bin]") {
		t.Fatalf("record without naming the file = %v", err)
	}
	res, err := s.record("data.bin")
	if err != nil || len(res.Steps) != 1 || len(res.Steps[0].Resolved) != 1 || res.Steps[0].Resolved[0].Path != "data.bin" || res.Steps[0].Resolved[0].Blob != s.repo.git("rev-parse", s.head+":data.bin") {
		t.Fatalf("record = %v %+v", err, res)
	}
}
