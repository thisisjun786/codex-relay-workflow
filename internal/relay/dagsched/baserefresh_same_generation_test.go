package dagsched

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pluginversion"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
)

// CRW-916. The lane case of a base refresh: the node's result was accepted, no bundle formed, and the merge lane has
// to refresh the branch inside its own turn (update-branch, or the plugin manifest's version line recorded again).
// The relationship stays at the generation the acceptance was recorded on, and the proof is the one the later
// generation gets - the pull request head is the accepted head plus merges of the base - so dag-base-refresh records
// it there. Everything the later-generation route refuses stays refused.

// sameGenerationRefresh is the parent's own refresh inside the lane turn: one clean merge of dev into the branch of
// the pull request, with the forge pointed at it. The acceptance's generation does not move.
func (s *refreshScenario) sameGenerationRefresh() string {
	s.t.Helper()
	s.repo.commit("other.txt", "dev one")
	head := s.mergeDev("merge dev", "", "")
	s.pointAt(head)
	return head
}

// markSameGenerationMerged is the parent's merged mark on the acceptance's own generation: the mark sits on the
// event and revision the acceptance was recorded on (the same generation, so the same revision).
func (s *refreshScenario) markSameGenerationMerged() {
	s.t.Helper()
	a := s.accepted.Acceptance
	s.exec("INSERT OR IGNORE INTO assignment_marks (relationship_id, mark, event_id, execution_generation, revision_hash, evidence, actor, marked_at) VALUES (?, 'merged', ?, ?, ?, 'merged', 'parent', ?)",
		a.RelationshipID, a.EventID, a.ExecutionGeneration, a.RevisionHash, s.clock())
}

// landSameGeneration merges the pull request into dev and marks the merge on the acceptance's own generation.
func (s *refreshScenario) landSameGeneration() {
	s.t.Helper()
	s.repo.git("checkout", "-q", "dev")
	s.repo.git("merge", "-q", "--no-ff", "-m", "merge the pull request", "feature")
	pr := openPR("owner/repo", 7, s.head)
	pr.State = "merged"
	s.forge.by["owner/repo#7"] = pr
	s.markSameGenerationMerged()
}

// newSameGenerationPluginVersionScenario is the plugin-manifest fixture of the later-generation tests without the
// hand-opened generation: node I is accepted at its head on generation 1 and stays there, so the parent's own
// refresh of the branch is the same-generation case.
func newSameGenerationPluginVersionScenario(t *testing.T) *refreshScenario {
	t.Helper()
	k := newIntegrationKit(t)
	repo := k.repo
	for _, f := range []struct{ path, content string }{
		{pluginversion.ManifestRepoPath, pluginVersionManifestText("0.4.0", "d")},
		{pluginversion.PluginRelative + "/LICENSE", "MIT\n"},
		{pluginversion.PluginRelative + "/skills/crw-run/SKILL.md", pluginVersionSkillText("crw-run")},
		{pluginversion.PluginRelative + "/skills/crw-plan/SKILL.md", pluginVersionSkillText("crw-plan")},
	} {
		pluginVersionWrite(t, repo, f.path, f.content)
	}
	pluginVersionRecordVersion(t, repo)
	repo.git("add", "-A")
	repo.git("commit", "-q", "-m", "plugin package")
	repo.git("checkout", "-q", "-b", "feature")
	h1 := pluginVersionCommit(t, repo, "previous work", map[string]string{
		pluginversion.PluginRelative + "/skills/crw-plan/SKILL.md": pluginVersionSkillText("crw-plan") + "previous paragraph.\n",
	}, false)
	repo.git("checkout", "-q", "dev")
	pluginVersionCommit(t, repo, "dev work", map[string]string{
		pluginversion.PluginRelative + "/skills/crw-run/SKILL.md": pluginVersionSkillText("crw-run") + "dev paragraph.\n",
	}, true)
	k.putPlan("g", 1, "g-r2", addRelNode("A", dag.NodeNonPR), addEdge("ia", "I", "A", dag.EdgeArtifactVerified, doc{"pins_code_head": true, "target_repository": forgeKitRepository, "target_base_ref": "dev"}))
	k.declare("g", "I", "feature.txt")
	a := k.acceptOnForge("g", "I", acceptOpts{HeadSHA: h1, PR: 7})
	k.holdSlotsFor("g", "I")
	n, _ := nodeOf(k.snapshot("g"), "I")
	k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, h1)
	return &refreshScenario{integrationKit: k, rid: a.Acceptance.RelationshipID, accepted: a, h1: h1, head: h1, criteria: n.CriteriaSetDigest, checkout: repo.path}
}

// Criterion c1 and c2 (a): a same-generation refresh of an accepted head by a dev merge is recorded, at the
// acceptance's own generation and on the event and revision it was recorded on; the same call again is a replay; and
// the readers that follow the stand read the refreshed head.
func TestSameGenerationRefreshOfAnAcceptedHeadIsRecorded(t *testing.T) {
	t.Parallel()
	s := newRefreshScenario(t)
	head := s.sameGenerationRefresh()
	a := s.accepted.Acceptance
	res, err := s.record()
	if err != nil || res.Replayed || res.Seq != 1 || res.Generation != a.ExecutionGeneration || res.HeadSHA != head ||
		res.AcceptanceID != a.AcceptanceID || res.EventID != a.EventID || res.RevisionHash != a.RevisionHash ||
		res.BaseRef != "dev" || res.BaseRepository != s.target() || len(res.Steps) != 1 || len(res.Resolved) != 0 || !strings.HasPrefix(res.RefreshID, "dbr-") {
		t.Fatalf("record = %v %+v", err, res)
	}
	if res.Steps[0].Previous != s.h1 || res.Steps[0].Head != head || res.Steps[0].BaseParent == "" {
		t.Fatalf("the chain is not the one merge from the accepted head: %+v", res.Steps)
	}
	if s.refreshRows() != 1 || !s.slotHeld() {
		t.Fatalf("rows = %d, slotHeld = %v, want one row and the slot still held", s.refreshRows(), s.slotHeld())
	}
	// the acceptance is not replaced, and what it stands on is the refreshed head at its own generation
	ctx := context.Background()
	stand, err := s.sched.standOf(ctx, s.s.Q(ctx), s.accepted.Acceptance)
	if err != nil || stand.Head != head || stand.RefreshID != res.RefreshID || stand.Generation != a.ExecutionGeneration || stand.EventID != a.EventID || stand.RevisionHash != a.RevisionHash {
		t.Fatalf("stand = %+v (%v), want the refreshed head at the acceptance's own generation", stand, err)
	}
	// the merge lane judges the pull request at the head the acceptance stands on
	if j, err := s.judge(); err != nil || !j.Eligible() || j.HeadSHA != head || j.ObservedHeadSHA != head || !strings.Contains(j.Reason, res.RefreshID) {
		t.Fatalf("judge = %v %+v, want the refreshed head eligible and the record named", err, j)
	}
	// the same call again is a replay of the same record
	again, err := s.record()
	if err != nil || !again.Replayed || again.RefreshID != res.RefreshID || again.Seq != 1 || s.refreshRows() != 1 {
		t.Fatalf("replay = %v %+v (rows %d)", err, again, s.refreshRows())
	}
}

// Criterion c2 (a): the same generation integrates on that generation's own merged mark, and the head it observes is
// the refreshed one.
func TestSameGenerationRefreshIntegratesOnTheGenerationsOwnMark(t *testing.T) {
	t.Parallel()
	s := newRefreshScenario(t)
	head := s.sameGenerationRefresh()
	if _, err := s.record(); err != nil {
		t.Fatal(err)
	}
	s.landSameGeneration()
	obs, err := s.observe()
	if err != nil || !obs.Integrated || !obs.MarkPresent || !obs.SlotReleased || len(obs.Observations) != 1 || obs.Observations[0].SubjectSHA != head || !obs.Observations[0].IsAncestor {
		t.Fatalf("observe = %v %+v", err, obs)
	}
	if n := s.read("g").node("I"); n.State != StateIntegrated || n.Disposition != DispDone || n.Reason != DoneIntegrated {
		t.Fatalf("I = %+v, want done:integrated", n)
	}
	if n := s.read("g").node("K"); n.Disposition != DispReady {
		t.Fatalf("K = %+v, want ready: its integrated edge is satisfied", n)
	}
}

// Criterion c1 and c2 (c): the base moves again inside the same generation, the whole chain is proved again from the
// accepted head, a second row is written, and the newest one is what the acceptance stands on.
func TestASecondSameGenerationRefreshAfterTheBaseMovesAgain(t *testing.T) {
	t.Parallel()
	s := newRefreshScenario(t)
	first := s.sameGenerationRefresh()
	one, err := s.record()
	if err != nil || one.Seq != 1 || one.HeadSHA != first || one.Generation != 1 {
		t.Fatalf("first record = %v %+v", err, one)
	}
	s.repo.commit("other-two.txt", "dev two")
	second := s.mergeDev("merge dev 2", "", "")
	s.pointAt(second)
	two, err := s.record()
	if err != nil || two.Replayed || two.Seq != 2 || two.HeadSHA != second || two.RefreshID == one.RefreshID || two.Generation != one.Generation || len(two.Steps) != 2 {
		t.Fatalf("second record = %v %+v", err, two)
	}
	if two.Steps[0].Previous != s.h1 || two.Steps[0].Head != two.Steps[1].Previous || two.Steps[1].Head != second {
		t.Fatalf("the second chain is not the two merges from the accepted head: %+v", two.Steps)
	}
	ctx := context.Background()
	stand, err := s.sched.standOf(ctx, s.s.Q(ctx), s.accepted.Acceptance)
	if err != nil || stand.Head != second || stand.RefreshID != two.RefreshID || s.refreshRows() != 2 {
		t.Fatalf("stand = %+v (%v), rows = %d, want the newest record's head", stand, err, s.refreshRows())
	}
	again, err := s.record()
	if err != nil || !again.Replayed || again.RefreshID != two.RefreshID || again.Seq != 2 || s.refreshRows() != 2 {
		t.Fatalf("replay of the second record = %v %+v (rows %d)", err, again, s.refreshRows())
	}
}

// Criterion c2 (b): a same-generation refresh whose head also records the plugin manifest's version line again is
// recorded, and the version-only step carries the built-in rule.
func TestSameGenerationRefreshWithThePluginVersionLine(t *testing.T) {
	t.Parallel()
	s := newSameGenerationPluginVersionScenario(t)
	merge, head := s.pluginVersion808Shape(t, nil)
	res, err := s.record()
	if err != nil || res.Replayed || res.Generation != 1 || res.HeadSHA != head || len(res.Resolved) != 0 || s.refreshRows() != 1 {
		t.Fatalf("record = %v %+v (rows %d)", err, res, s.refreshRows())
	}
	steps := pluginVersionStoredSteps(t, s)
	if len(steps) != 2 || steps[0].Head != merge || steps[0].BaseParent == "" || steps[1].Head != head || steps[1].Previous != merge || steps[1].BaseParent != "" {
		t.Fatalf("steps = %+v, want the merge and the version-only commit", steps)
	}
	if len(steps[1].Resolved) != 1 || steps[1].Resolved[0].Path != pluginversion.ManifestRepoPath || steps[1].Resolved[0].Rule != BuiltinPluginVersionRule {
		t.Fatalf("the version-only step does not carry the built-in rule: %+v", steps[1].Resolved)
	}
}

// Criterion c2: the same generation keeps every refusal the later generation has. Each case leaves no row, and the
// node is where it was.
func TestSameGenerationRefreshRefusesWhatIsNotARefresh(t *testing.T) {
	t.Parallel()
	t.Run("a head holding a commit of the child's own on top of the merge", func(t *testing.T) {
		s := newRefreshScenario(t)
		s.sameGenerationRefresh()
		s.repo.git("checkout", "-q", "feature")
		head := s.repo.commit("child.txt", "work of the child")
		s.repo.git("checkout", "-q", "dev")
		s.pointAt(head)
		if _, err := s.record(); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "("+RefreshNotAMerge+")") || s.refreshRows() != 0 || !s.slotHeld() {
			t.Fatalf("record = %v (rows %d)", err, s.refreshRows())
		}
	})
	t.Run("a node that landed", func(t *testing.T) {
		s := newRefreshScenario(t)
		s.sameGenerationRefresh()
		s.landSameGeneration()
		// the observation of the landing is what makes the node integrated; the mark alone integrates nothing
		if obs, err := s.observe(); err != nil || !obs.Integrated || !obs.MarkPresent {
			t.Fatalf("observe = %v %+v, want the accepted head contained with its own mark", err, obs)
		}
		if _, err := s.record(); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "landed") || s.refreshRows() != 0 {
			t.Fatalf("record = %v (rows %d)", err, s.refreshRows())
		}
	})
	t.Run("a stale node", func(t *testing.T) {
		s := newRefreshScenario(t)
		s.sameGenerationRefresh()
		s.rvReregister("g", "I", "g-r3", s.rid, dig("the second edition of I's criteria"), nil)
		if n := s.read("g").node("I"); n.Disposition != DispStale {
			t.Fatalf("I = %+v, want stale", n)
		}
		if _, err := s.record(); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "stale") || s.refreshRows() != 0 {
			t.Fatalf("record = %v (rows %d)", err, s.refreshRows())
		}
	})
	t.Run("the generation's ruling is not verified any more", func(t *testing.T) {
		s := newRefreshScenario(t)
		s.sameGenerationRefresh()
		s.exec("UPDATE verdicts SET verdict = 'needs_changes' WHERE event_id = ?", s.accepted.Acceptance.EventID)
		if _, err := s.record(); refusalReason(err) != "disposition_conflict" || s.refreshRows() != 0 {
			t.Fatalf("record = %v (rows %d)", err, s.refreshRows())
		}
	})
	t.Run("a pull request still at the accepted head", func(t *testing.T) {
		s := newRefreshScenario(t)
		if _, err := s.record(); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "still at generation 1") || s.refreshRows() != 0 {
			t.Fatalf("record = %v (rows %d)", err, s.refreshRows())
		}
	})
}

// The proof of the same generation is the later generation's proof: a hand resolution is still the parent's to name,
// exactly, and the record keeps it.
func TestSameGenerationRefreshNeedsTheHandResolvedFilesNamedExactly(t *testing.T) {
	t.Parallel()
	s := newRefreshScenario(t)
	s.repo.commit("other.txt", "dev one")
	s.mergeDev("merge dev 1", "", "")
	s.repo.write("shared.json", "version of dev\n")
	s.repo.git("add", "shared.json")
	s.repo.git("commit", "-q", "-m", "dev changes shared.json")
	head := s.mergeDev("merge dev 2, shared.json resolved", "shared.json", "version of the feature and of dev\n")
	s.pointAt(head)
	if _, err := s.record(); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "[shared.json]") || s.refreshRows() != 0 {
		t.Fatalf("record without the name = %v (rows %d)", err, s.refreshRows())
	}
	res, err := s.record("shared.json")
	if err != nil || res.Generation != 1 || res.HeadSHA != head || strings.Join(res.Resolved, ",") != "shared.json" || len(res.Steps) != 2 {
		t.Fatalf("record = %v %+v", err, res)
	}
}

// The acceptance's own generation is refreshed against the ruling that generation still holds: a newer report of that
// generation supersedes the accepted event, so the refresh is refused as the superseded revision it is and not recorded
// against a head no ruling covers.
func TestSameGenerationRefreshIsRefusedWhenANewerReportSupersedesTheAcceptedEvent(t *testing.T) {
	t.Parallel()
	s := newRefreshScenario(t)
	s.sameGenerationRefresh()
	a := s.accepted.Acceptance
	// a later report of the same generation, declared as replacing the accepted revision, acknowledged and ruled
	// verified under the same criteria: the generation's head is that report now.
	s.supersedeSameGenerationReport()
	if _, err := s.record(); refusalReason(err) != "superseded_revision" || s.refreshRows() != 0 {
		t.Fatalf("record = %v (rows %d), want superseded_revision and no row", err, s.refreshRows())
	}
	// the ruling on the acceptance is untouched: the refusal wrote nothing
	if got := rvRecords(s.releaseKit, "g", "I"); !strings.Contains(got, a.AcceptanceID) {
		t.Fatalf("the acceptance rows changed: %s", got)
	}
}

// Criterion c1, the issue's own condition 3: a bundle leader that fell back to the single lane is carried by the
// merge train's own open at the head the same-generation record names. The train reads the stand through
// internal/relay/mergeturn/train.go's standFor -> trainStandRefusal, so the real merge-train-open is called here
// over the scenario's store: a one-member bundle (the leader alone) is the single-lane answer, and the stand-head
// check that decides it runs before that answer is returned.
func TestABundleLeaderRefreshedInItsOwnGenerationOpensItsBundle(t *testing.T) {
	t.Parallel()
	s := newRefreshScenario(t)
	head := s.sameGenerationRefresh()
	ctx := context.Background()
	forge := &trainForgeStub{pull: mergeturn.TrainPullRequest{Number: 7, State: "open", BaseRef: "dev", HeadSHA: head}}

	// before the record the train refuses the leader: its acceptance stands on the accepted head, not on the head
	// its pull request shows
	if _, turn, err := s.askForTurn(); refusalReason(err) != "merge_candidate_moved" || turn != nil {
		t.Fatalf("the turn before the record = %v %v, want merge_candidate_moved", err, turn)
	}
	res, err := s.record()
	if err != nil {
		t.Fatal(err)
	}
	if res.HeadSHA != head || res.Generation != s.accepted.Acceptance.ExecutionGeneration {
		t.Fatalf("record = %+v, want the refreshed head at the acceptance's own generation", res)
	}
	_, turn, err := s.askForTurn()
	if err != nil || turn == nil {
		t.Fatalf("the turn after the record = %v %v", err, turn)
	}
	// merge-train-open, the real gate: the leader's own pull request alone, which is the single-lane answer
	answer, err := forgeLaneService(s.sched, s.s, s.lanePulls()).Open(ctx, turn["turnId"].(string), "parent", s.repo.git("rev-parse", "dev"), []int64{7}, s.readers, forge)
	if err != nil {
		t.Fatalf("merge-train-open refused the leader the same-generation record carried: %v", err)
	}
	if answer["lane"] != "single" || answer["pullRequest"] != int64(7) {
		t.Fatalf("the bundle answer = %v, want the single-lane answer for the leader's own pull request", answer)
	}
}

// trainForgeStub is the forge the merge train reads: only the pull requests of a one-member bundle are read, so the
// other three reads answer with a statement that is never reached.
type trainForgeStub struct{ pull mergeturn.TrainPullRequest }

func (f *trainForgeStub) PullRequest(_ context.Context, _ string, _ int64) (mergeturn.TrainPullRequest, error) {
	return f.pull, nil
}

func (f *trainForgeStub) Run(_ context.Context, _, _ string) (mergeturn.TrainRun, error) {
	return mergeturn.TrainRun{}, nil
}

func (f *trainForgeStub) Commit(_ context.Context, _, _ string) (mergeturn.TrainCommit, error) {
	return mergeturn.TrainCommit{}, nil
}

func (f *trainForgeStub) Compare(_ context.Context, _, _, _ string) (string, error) { return "", nil }

// supersedeSameGenerationReport adds a later report of the acceptance's generation that declares it replaces the
// accepted revision, acknowledged and ruled verified under the plan's criteria, so the generation's head is that
// report and the accepted event is history.
func (s *refreshScenario) supersedeSameGenerationReport() {
	s.t.Helper()
	a := s.accepted.Acceptance
	revision := dig("a newer report of the accepted generation " + s.rid)
	event := "evt-same-generation-newer-" + s.rid
	now := s.clock()
	s.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
		" VALUES (?, ?, ?, ?, 'ready_for_review', 'child', 'child', 'turn-2', 'completed', '{}', 'final', ?, ?)", event, s.rid, a.ExecutionGeneration, revision, now, now)
	s.exec("INSERT INTO revision_lineage (relationship_id, execution_generation, event_id, revision_hash, supersedes_hash, declared_by, recorded_at) VALUES (?, ?, ?, ?, ?, 'child', ?)",
		s.rid, a.ExecutionGeneration, event, revision, a.RevisionHash, now)
	s.exec("INSERT INTO acks (event_id, record, ack_turn_id, accepted, verified, ack_at) VALUES (?, '{}', 'ack-turn-2', 1, 'verified', ?)", event, now)
	s.exec("INSERT INTO ack_evidence (event_id, tier, observed_at) VALUES (?, 'host_read', ?)", event, now)
	s.exec("INSERT INTO verdicts (event_id, record, verdict, verdict_turn_id, decided_at) VALUES (?, '{}', 'verified', 'verdict-turn-2', ?)", event, now)
	s.exec("INSERT INTO verdict_context (event_id, set_digest, coverage, currency, head_event_id, head_revision, ack_evidence, recorded_at) VALUES (?, ?, '{}', 'current', ?, ?, '{}', ?)",
		event, s.criteria, event, revision, now)
}
