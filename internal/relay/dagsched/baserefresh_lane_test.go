package dagsched

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// CRW-447: once the parent has recorded a base refresh (dag-base-refresh), the head the acceptance stands on is the head of its pull request. The merge judgement, the merge request, the release
// freshness check and the artifact_verified edge compare the pull request with that head and not with the accepted one, so the refreshed pull request goes through the merge lane and nothing reads
// stale_head for it. What no record covers (a head that moved on, a head that holds more than merges of the base, a row nobody proved) is refused exactly as before.

func (s *refreshScenario) judge() (JudgeResult, error) {
	s.t.Helper()
	return s.sched.Judge(context.Background(), "g", "I", "parent", JudgeInput{})
}

func (s *refreshScenario) askForTurn() (JudgeResult, map[string]any, error) {
	s.t.Helper()
	return s.sched.RequestMergeTurn(context.Background(), "g", "I", "parent", MergeRequestInput{Host: "host"})
}

func (s *refreshScenario) turns() int { return s.count("SELECT COUNT(*) FROM merge_turns") }

// artifactEdge is the edge that waits for I's verified result and pins its code head.
func (s *refreshScenario) artifactEdge() EdgeStatus { return s.status("g", "ia") }

// pointAt makes the forge show the pull request at a head, with the one required check green on it.
func (s *refreshScenario) pointAt(head string) {
	s.t.Helper()
	s.head = head
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, head)
}

// moveOn is the child merging the base once more: another commit on dev and a fourth merge of it, which the forge shows and no record covers.
func (s *refreshScenario) moveOn() string {
	s.t.Helper()
	s.repo.commit("other-four.txt", "dev four")
	head := s.mergeDev("merge dev 4", "", "")
	s.pointAt(head)
	return head
}

// report is the work report of generation 2 naming a head: the merge lane compares the candidate it is asked about with the head the newest generation's report names.
func (s *refreshScenario) report(head string) {
	s.t.Helper()
	s.exec("INSERT INTO work_reports (event_id, submission_no, relationship_id, execution_generation, revision_hash, repository, pr_number, pr_url, head_sha, cxc_status, cxc_reason, contract_version, summary, next_action, recorded_at)"+
		" VALUES (?, 1, ?, 2, ?, 'owner/repo', 7, NULL, ?, 'DONE', 'proved', 'v1', 'done', 'merge', 't')", s.event2, s.rid, s.revision2, head)
}

// recordRow writes a base refresh row that digests to its id, as the record would have, for a head the test names (the proof is not read by the readers this file exercises).
func (s *refreshScenario) recordRow(seq int, head string) {
	s.t.Helper()
	proof, resolved := "{}", "[]"
	id := refreshDigest(s.accepted.Acceptance.AcceptanceID, s.rid, 2, s.event2, s.revision2, head, s.repo.path, "dev", head, proof, resolved)
	s.exec("INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json,"+
		" recorded_by_task_id, coordinator_epoch, recorded_at) VALUES (?, ?, ?, ?, 2, ?, ?, ?, ?, 'dev', ?, ?, ?, 'someone', 0, 't')",
		id, s.accepted.Acceptance.AcceptanceID, seq, s.rid, s.event2, s.revision2, head, s.repo.path, head, proof, resolved)
}

// recordedScenario is the node whose child refreshed the base in generation 2 and whose refresh the parent recorded; the pull request is open at the refreshed head.
func recordedScenario(t *testing.T) (*refreshScenario, RefreshResult) {
	t.Helper()
	s := newRefreshScenario(t)
	s.openGeneration()
	s.refreshBase()
	rec, err := s.record("shared.json")
	if err != nil {
		t.Fatalf("record = %v", err)
	}
	return s, rec
}

func (s *refreshScenario) laneService() *mergeturn.Service {
	return &mergeturn.Service{Store: s.s, Registry: &registry.Registry{Store: s.s}, Now: s.sched.now, Delivery: mergeturn.StoreDelivery{Store: s.s}}
}

// Criterion c1: a node whose base refresh is recorded has its pull request judged and merged through the merge lane, and the edge that waits for its verified result no longer reads blocked:stale_head.
// Before the record the same pull request is stale_head and gets no turn. The work report of generation 2 names the refreshed head, so the lane's own check, which is not changed, compares the candidate
// with it.
func TestARefreshedNodeIsJudgedAndMergedInTheLane(t *testing.T) {
	s := newRefreshScenario(t)
	ctx := context.Background()
	s.openGeneration()
	s.refreshBase()
	s.report(s.head)

	// no record yet: the pull request is at a head the acceptance does not stand on
	if res, err := s.judge(); err != nil || res.Outcome != OutcomeStaleHead || res.ObservedHeadSHA != s.head || res.HeadSHA != s.h1 {
		t.Fatalf("judge before the record = %v %+v, want stale_head", err, res)
	}
	if _, turn, err := s.askForTurn(); refusalReason(err) != "merge_candidate_moved" || turn != nil || s.turns() != 0 {
		t.Fatalf("request before the record = %v %v (turns %d)", err, turn, s.turns())
	}
	if st := s.artifactEdge(); st.Satisfied || st.Reason != BlockedStaleHead {
		t.Fatalf("the edge before the record = %+v, want blocked:stale_head", st)
	}
	if n := s.read("g").node("A"); n.Disposition == DispReady || n.Reason != BlockedStaleHead {
		t.Fatalf("A before the record = %+v, want blocked:stale_head", n)
	}

	rec, err := s.record("shared.json")
	if err != nil {
		t.Fatal(err)
	}
	// the head the acceptance stands on is the pull request's now; the stale_head observation of that head made before the record was made under the accepted head and says nothing about now
	if st := s.artifactEdge(); !st.Satisfied {
		t.Fatalf("the edge after the record = %+v, want it satisfied", st)
	}
	if n := s.read("g").node("A"); n.Disposition != DispReady {
		t.Fatalf("A after the record = %+v, want ready", n)
	}
	res, err := s.judge()
	if err != nil || !res.Eligible() || res.ObservedHeadSHA != s.head || res.HeadSHA != s.head || !strings.Contains(res.Reason, rec.RefreshID) {
		t.Fatalf("judge after the record = %v %+v, want eligible at the refreshed head and saying which record it stands on", err, res)
	}
	if st := s.artifactEdge(); !st.Satisfied {
		t.Fatalf("the edge after the judgement = %+v", st)
	}

	// the lane's own turn is for the refreshed head, and the lane is not changed
	_, turn, err := s.askForTurn()
	if err != nil || turn == nil || turn["candidateHead"] != s.head || turn["relationshipId"] != s.rid {
		t.Fatalf("request after the record = %v %v", err, turn)
	}
	service := s.laneService()
	id := turn["turnId"].(string)
	if grant, _ := turn["grant"].(map[string]any); grant != nil {
		if _, err := service.Acknowledge(ctx, id, "parent", grant["grantId"].(string), "re-read the store before merging"); err != nil {
			t.Fatalf("acknowledge: %v", err)
		}
	}
	green := contract.OrderedObject{{Key: "hasNextPage", Value: false}, {Key: "pagesRead", Value: json.Number("1")}, {Key: "totalCount", Value: json.Number("0")}, {Key: "threadsSeen", Value: []any{}}, {Key: "unresolved", Value: json.Number("0")}}
	var list []any
	for _, c := range s.forge.by["owner/repo#7"].Checks {
		list = append(list, contract.OrderedObject{{Key: "runId", Value: c.RunID}, {Key: "name", Value: c.Name}, {Key: "headSha", Value: c.HeadSHA}, {Key: "conclusion", Value: c.Conclusion}, {Key: "attempt", Value: json.Number("1")}})
	}
	if _, err := service.Check(ctx, id, "parent", s.head, s.repo.git("rev-parse", "dev"), list, green, []string{"test"}, mergeturn.TargetReader{}); err != nil {
		t.Fatalf("the lane's check of the refreshed head: %v", err)
	}
	s.land()
	if _, err := service.Land(ctx, id, "parent", s.repo.git("rev-parse", "dev"), "", "merged by the forge", mergeturn.TargetReader{}); err != nil {
		t.Fatalf("landing the refreshed head: %v", err)
	}
	if st := s.artifactEdge(); !st.Satisfied {
		t.Fatalf("the edge after the landing = %+v", st)
	}
	obs, err := s.observe()
	if err != nil || !obs.Integrated || !obs.MarkPresent || !obs.SlotReleased || obs.Observations[0].SubjectSHA != s.head {
		t.Fatalf("observe after the lane landed it = %v %+v", err, obs)
	}
	if n := s.read("g").node("I"); n.State != StateIntegrated || n.Reason != DoneIntegrated {
		t.Fatalf("I = %+v, want done:integrated", n)
	}
	if n := s.read("g").node("K"); n.Disposition != DispReady {
		t.Fatalf("K = %+v, want ready: its integrated edge is satisfied", n)
	}
	// every landed tree has an eligible judgement of the very head that landed
	if n := s.count("SELECT COUNT(*) FROM merge_turns m WHERE m.state = 'landed' AND NOT EXISTS (SELECT 1 FROM dag_merge_checks c WHERE c.outcome = 'eligible' AND c.observed_head_sha = m.candidate_head)"); n != 0 {
		t.Fatalf("%d landed turns without an eligible judgement of their head", n)
	}
	if s.count("SELECT COUNT(*) FROM merge_turns WHERE state = 'landed' AND candidate_head = ?", s.head) != 1 {
		t.Fatal("the lane did not land the refreshed head")
	}
}

// Criterion c2: what no record covers is refused as before. A pull request that moved on after the record, a head that holds more than merges of the base (the record is refused, so there is none), and
// a row that does not digest to its id (nobody proved it) each leave the pull request stale_head, the request without a turn, and the edge blocked.
func TestARefreshedPullRequestNoRecordCoversIsRefusedAsBefore(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) *refreshScenario
	}{
		{"the pull request moved on after the record", func(t *testing.T) *refreshScenario {
			s, _ := recordedScenario(t)
			s.moveOn()
			return s
		}},
		{"the head holds the child's own work on top of the merges", func(t *testing.T) *refreshScenario {
			s := newRefreshScenario(t)
			s.openGeneration()
			s.refreshBase()
			s.repo.git("checkout", "-q", "feature")
			head := s.repo.commit("child.txt", "work of the child")
			s.repo.git("checkout", "-q", "dev")
			s.pointAt(head)
			if _, err := s.record("shared.json"); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "("+RefreshNotAMerge+")") {
				t.Fatalf("record = %v, want it refused as not a refresh", err)
			}
			return s
		}},
		{"the head carries a file the merges did not bring", func(t *testing.T) *refreshScenario {
			s := newRefreshScenario(t)
			s.openGeneration()
			s.repo.commit("other.txt", "dev one")
			s.repo.git("checkout", "-q", "feature")
			s.repo.git("merge", "-q", "--no-ff", "--no-commit", "dev")
			s.repo.write("smuggled.txt", "not from the base")
			s.repo.git("add", "smuggled.txt")
			s.repo.git("commit", "-q", "-m", "merge dev with more")
			head := s.repo.git("rev-parse", "HEAD")
			s.repo.git("checkout", "-q", "dev")
			s.pointAt(head)
			if _, err := s.record(); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "("+RefreshTreeDiffers+")") {
				t.Fatalf("record = %v, want it refused as not a refresh", err)
			}
			return s
		}},
		{"a refresh row nobody proved", func(t *testing.T) *refreshScenario {
			s := newRefreshScenario(t)
			s.openGeneration()
			s.refreshBase()
			s.exec("INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json,"+
				" recorded_by_task_id, coordinator_epoch, recorded_at) VALUES ('dbr-0123456789abcdef0123456789abcdef', ?, 1, ?, 2, ?, ?, ?, ?, 'dev', ?, '{}', '[]', 'someone', 0, 't')",
				s.accepted.Acceptance.AcceptanceID, s.rid, s.event2, s.revision2, s.head, s.repo.path, s.head)
			return s
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := c.setup(t)
			res, err := s.judge()
			if err != nil || res.Outcome != OutcomeStaleHead || res.ObservedHeadSHA != s.head {
				t.Fatalf("judge = %v %+v, want stale_head", err, res)
			}
			if _, turn, err := s.askForTurn(); refusalReason(err) != "merge_candidate_moved" || turn != nil || s.turns() != 0 {
				t.Fatalf("request = %v %v (turns %d), want merge_candidate_moved and no turn", err, turn, s.turns())
			}
			if st := s.artifactEdge(); st.Satisfied || st.Reason != BlockedStaleHead {
				t.Fatalf("the edge = %+v, want blocked:stale_head", st)
			}
		})
	}
}

// What the record says is the head of the acceptance, and the newest record wins: a pull request at the head of an older record is stale_head, one at the newest is not. A check made under an earlier
// record's head is history once a newer record lands; a check made under a head the acceptance never stood on is not a reading of it.
func TestTheNewestRecordIsTheHeadThePullRequestIsJudgedAgainst(t *testing.T) {
	s, first := recordedScenario(t)
	older := s.head
	if res, err := s.judge(); err != nil || !res.Eligible() || res.HeadSHA != older {
		t.Fatalf("judge at the first record = %v %+v", err, res)
	}
	newer := s.moveOn()
	if res, err := s.judge(); err != nil || res.Outcome != OutcomeStaleHead || res.HeadSHA != older || res.ObservedHeadSHA != newer {
		t.Fatalf("judge of the head no record covers = %v %+v", err, res)
	}
	if st := s.artifactEdge(); st.Satisfied || st.Reason != BlockedStaleHead {
		t.Fatalf("the edge while the pull request is past the record = %+v", st)
	}
	second, err := s.record("shared.json")
	if err != nil || second.RefreshID == first.RefreshID || second.HeadSHA != newer {
		t.Fatalf("second record = %v %+v", err, second)
	}
	if st := s.artifactEdge(); !st.Satisfied {
		t.Fatalf("the edge whose latest check was made under the first record = %+v, want it satisfied", st)
	}
	if res, err := s.judge(); err != nil || !res.Eligible() || res.HeadSHA != newer || res.ObservedHeadSHA != newer || !strings.Contains(res.Reason, second.RefreshID) {
		t.Fatalf("judge at the newest record = %v %+v", err, res)
	}
	s.pointAt(older)
	if res, err := s.judge(); err != nil || res.Outcome != OutcomeStaleHead || res.ObservedHeadSHA != older || res.HeadSHA != newer {
		t.Fatalf("judge at the head of the older record = %v %+v, want stale_head", err, res)
	}
	if st := s.artifactEdge(); st.Satisfied || st.Reason != BlockedStaleHead {
		t.Fatalf("the edge with the pull request back at the older head = %+v", st)
	}
	// a check made under a head the acceptance never stood on is no reading of it
	s.exec("UPDATE dag_merge_checks SET head_sha = ? WHERE check_seq = (SELECT MAX(check_seq) FROM dag_merge_checks)", strings.Repeat("9", 40))
	s.pointAt(newer)
	if st := s.artifactEdge(); st.Satisfied || st.Reason != BlockedStaleHead {
		t.Fatalf("the edge with a check of a head nobody stood on = %+v", st)
	}
}

// The edge asks where the relay last saw the pull request. What it saw of the accepted head before the record was seen under the accepted head: history. The same observation repeated after the record is
// a new row under the head the acceptance stands on, and it is a move.
func TestAnObservationMadeBeforeTheRecordIsHistoryAndTheSameOneAfterItIsAMove(t *testing.T) {
	s := newRefreshScenario(t)
	if res, err := s.judge(); err != nil || !res.Eligible() || res.ObservedHeadSHA != s.h1 {
		t.Fatalf("judge at the accepted head = %v %+v", err, res)
	}
	s.openGeneration()
	s.refreshBase()
	if _, err := s.record("shared.json"); err != nil {
		t.Fatal(err)
	}
	if st := s.artifactEdge(); !st.Satisfied {
		t.Fatalf("the edge with only an older observation of the accepted head = %+v, want it satisfied", st)
	}
	// the pull request is back at the accepted head after the record: that is a move off the head the acceptance stands on
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.h1)
	if res, err := s.judge(); err != nil || res.Outcome != OutcomeStaleHead || res.ObservedHeadSHA != s.h1 || res.HeadSHA != s.head {
		t.Fatalf("judge at the accepted head after the record = %v %+v, want stale_head", err, res)
	}
	if st := s.artifactEdge(); st.Satisfied || st.Reason != BlockedStaleHead {
		t.Fatalf("the edge after that observation = %+v, want blocked:stale_head", st)
	}
}

// The same stale observation made before the record and after it is two rows: it is judged against two different heads, and the first must not stand for the second.
func TestTheSameObservationBeforeAndAfterTheRecordAreTwoRows(t *testing.T) {
	s := newRefreshScenario(t)
	s.openGeneration()
	s.refreshBase()
	elsewhere := strings.Repeat("e", 40)
	refreshed := s.head
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, elsewhere)
	if res, err := s.judge(); err != nil || res.Outcome != OutcomeStaleHead || res.ObservedHeadSHA != elsewhere {
		t.Fatalf("judge before the record = %v %+v", err, res)
	}
	s.pointAt(refreshed)
	if _, err := s.record("shared.json"); err != nil {
		t.Fatal(err)
	}
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, elsewhere)
	res, err := s.judge()
	if err != nil || res.Outcome != OutcomeStaleHead || res.Replayed || res.CheckSeq != 2 || res.HeadSHA != refreshed {
		t.Fatalf("judge after the record = %v %+v, want a second row under the head the record names", err, res)
	}
	if st := s.artifactEdge(); st.Satisfied || st.Reason != BlockedStaleHead {
		t.Fatalf("the edge = %+v, want blocked:stale_head", st)
	}
	// and the same thing again is the replay it always was
	if again, err := s.judge(); err != nil || !again.Replayed || again.CheckSeq != 2 {
		t.Fatalf("the same observation again = %v %+v", err, again)
	}
}

// A head that was evicted stays evicted in the history. A pull request back at it after a record moved the acceptance on is stale_head and is observed as such, not answered from the old eviction while the
// latest row still says eligible at the refreshed head.
func TestAnEvictedHeadTheAcceptanceNoLongerStandsOnIsObservedAsStale(t *testing.T) {
	s := newRefreshScenario(t)
	failing := func(head string, attempt int64) PullRequest {
		pr := openPR("owner/repo", 7, head)
		pr.Checks = []Check{{RunID: "1", Name: "test", HeadSHA: head, Conclusion: "failure", Attempt: attempt}}
		return pr
	}
	s.forge.by["owner/repo#7"] = failing(s.h1, 1)
	if res, err := s.judge(); err != nil || res.Outcome != OutcomeRetrySameSHA {
		t.Fatalf("first failure = %v %+v", err, res)
	}
	s.forge.by["owner/repo#7"] = failing(s.h1, 2)
	if res, err := s.judge(); err != nil || res.Outcome != OutcomeEvicted {
		t.Fatalf("second failure = %v %+v", err, res)
	}
	s.openGeneration()
	s.refreshBase()
	if _, err := s.record("shared.json"); err != nil {
		t.Fatal(err)
	}
	if res, err := s.judge(); err != nil || !res.Eligible() || res.ObservedHeadSHA != s.head {
		t.Fatalf("judge at the refreshed head = %v %+v", err, res)
	}
	// the evicted head again
	s.forge.by["owner/repo#7"] = failing(s.h1, 2)
	res, err := s.judge()
	if err != nil || res.Outcome != OutcomeStaleHead || res.ObservedHeadSHA != s.h1 || res.HeadSHA != s.head {
		t.Fatalf("judge at the evicted head = %v %+v, want stale_head under the refreshed head", err, res)
	}
	if st := s.artifactEdge(); st.Satisfied || st.Reason != BlockedStaleHead {
		t.Fatalf("the edge = %+v, want blocked:stale_head", st)
	}
	// the eviction itself is still there: the same head under the accepted head's own judgement stays evicted
	if s.count("SELECT COUNT(*) FROM dag_merge_checks WHERE outcome = 'evicted' AND observed_head_sha = ?", s.h1) != 1 {
		t.Fatal("the eviction of the accepted head is not in the history")
	}
}

// The refresh the judgement read is still the one the acceptance stands on when the judgement is written: a record that lands in between is a reading that is older than the history.
func TestAJudgementReadBeforeANewerRecordIsNotWritten(t *testing.T) {
	s, _ := recordedScenario(t)
	s.sched.testBeforeJudgeTx = func() {
		s.sched.testBeforeJudgeTx = nil
		s.recordRow(2, strings.Repeat("c", 40))
	}
	before := s.count("SELECT COUNT(*) FROM dag_merge_checks")
	if _, err := s.judge(); refusalReason(err) != "merge_candidate_moved" || s.count("SELECT COUNT(*) FROM dag_merge_checks") != before {
		t.Fatalf("judge = %v, want merge_candidate_moved and no row", err)
	}
}

// The request reads the head the acceptance stands on again where it creates the turn: a record that lands between the judgement and the request leaves the eligible judgement of another head, and no turn.
func TestAMergeRequestReadsTheStandAgainWhereItCreatesTheTurn(t *testing.T) {
	s, _ := recordedScenario(t)
	if res, err := s.judge(); err != nil || !res.Eligible() {
		t.Fatalf("judge = %v %+v", err, res)
	}
	s.sched.testBetweenJudgeAndAsk = func() {
		s.sched.testBetweenJudgeAndAsk = nil
		s.recordRow(2, strings.Repeat("c", 40))
	}
	if _, turn, err := s.askForTurn(); refusalReason(err) != "disposition_conflict" || turn != nil || s.turns() != 0 {
		t.Fatalf("request = %v %v (turns %d), want no turn", err, turn, s.turns())
	}
}

// The release freshness check reads the pull request of a pinned predecessor against the head its acceptance stands on: a recorded refresh releases the successor, and a pull request that moved on after the
// record does not (merge_candidate_moved, with the stale_head observation kept). A record that lands while the forge is read writes no observation.
func TestReleaseFreshnessFollowsTheRecordedRefresh(t *testing.T) {
	t.Run("the pull request is at the refreshed head", func(t *testing.T) {
		s, _ := recordedScenario(t)
		if res, err := s.release("g", "A"); err != nil || s.count("SELECT COUNT(*) FROM dag_releases WHERE plan_id = 'g' AND node_id = 'A'") != 1 {
			t.Fatalf("release of the successor = %v %+v", err, res)
		}
		if n := s.count("SELECT COUNT(*) FROM dag_merge_checks WHERE outcome = 'stale_head'"); n != 0 {
			t.Fatalf("%d stale_head observations for a pull request at the head the acceptance stands on", n)
		}
	})
	t.Run("the pull request moved on", func(t *testing.T) {
		s, rec := recordedScenario(t)
		moved := s.moveOn()
		_, err := s.release("g", "A")
		if refusalReason(err) != "merge_candidate_moved" || !strings.Contains(err.Error(), rec.RefreshID) {
			t.Fatalf("release of the successor = %v, want merge_candidate_moved naming the record", err)
		}
		var observed, judged string
		if err := s.s.DB.QueryRow("SELECT observed_head_sha, head_sha FROM dag_merge_checks WHERE outcome = 'stale_head'").Scan(&observed, &judged); err != nil || observed != moved || judged != rec.HeadSHA {
			t.Fatalf("stale_head observation = %q under %q: %v", observed, judged, err)
		}
	})
	t.Run("a record lands while the forge is read", func(t *testing.T) {
		s, _ := recordedScenario(t)
		moved := s.moveOn()
		s.forge.onRead = func() { s.recordRow(2, moved) }
		if _, err := s.release("g", "A"); refusalReason(err) != "merge_candidate_moved" || s.count("SELECT COUNT(*) FROM dag_merge_checks") != 0 {
			t.Fatalf("release of the successor = %v, want merge_candidate_moved and no row", err)
		}
	})
}

// A merge turn of unknown effect for a head the acceptance stands on, or stood on before a record moved it, blocks the node's reading as one for the accepted head does.
func TestAMergeTurnOfUnknownEffectForARefreshedHeadBlocksTheNode(t *testing.T) {
	s, _ := recordedScenario(t)
	first := s.head
	if _, turn, err := s.askForTurn(); err != nil || turn == nil {
		t.Fatalf("request = %v %v", err, turn)
	}
	s.exec("UPDATE merge_turns SET state = 'unknown' WHERE candidate_head = ?", first)
	if n := s.read("g").node("I"); n.Disposition != DispBlocked || n.Reason != BlockedEffectUnknown {
		t.Fatalf("I = %+v, want blocked:effect_unknown", n)
	}
	// a second record moves the acceptance on: the turn of the first head is still the node's
	s.recordRow(2, strings.Repeat("d", 40))
	if n := s.read("g").node("I"); n.Disposition != DispBlocked || n.Reason != BlockedEffectUnknown {
		t.Fatalf("I after a second record = %+v, want blocked:effect_unknown", n)
	}
}

// The merge-order readers follow the head the acceptance stands on: the node whose turn is for the refreshed head is in the turn lane, and the current accepted head is that head.
func TestTheMergeLaneReadersFollowTheRecordedHead(t *testing.T) {
	s := newRefreshScenario(t)
	ctx := context.Background()
	s.openGeneration()
	s.refreshBase()
	q := s.s.Q(ctx)
	headOf := func() string {
		acc, has, err := loadActiveAcceptance(ctx, q, "g", "I")
		if err != nil || !has {
			t.Fatalf("acceptance = %v %v", has, err)
		}
		rel, found, err := currentRelationshipOf(ctx, q, "g", "I")
		if err != nil || !found {
			t.Fatalf("relationship = %v %v", found, err)
		}
		head, err := s.sched.currentAcceptanceHead(ctx, q, acc, true, rel, true)
		if err != nil {
			t.Fatal(err)
		}
		return head
	}
	if got := headOf(); got != "" {
		t.Fatalf("the current accepted head before the record = %q, want none: the relationship is past the generation it stands on", got)
	}
	if _, err := s.record("shared.json"); err != nil {
		t.Fatal(err)
	}
	if got := headOf(); got != s.head {
		t.Fatalf("the current accepted head after the record = %q, want the refreshed head %q", got, s.head)
	}
	if _, turn, err := s.askForTurn(); err != nil || turn == nil {
		t.Fatalf("request = %v %v", err, turn)
	}
	acc, _, _ := loadActiveAcceptance(ctx, q, "g", "I")
	pos, err := s.sched.lanePosition(ctx, q, "g", orderHolder{NodeID: "I", State: StateAccepted, Disp: DispDone, Acc: acc, HasAcc: true}, headOf())
	if err != nil || pos.lane != LaneTurn {
		t.Fatalf("lane position = %v %+v, want the turn lane", err, pos)
	}
}
