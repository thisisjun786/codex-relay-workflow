package dagsched

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The findings of the first audit of the acceptance, integration and correction code (wp5): each test reproduces a path the audit found and fails on the code that had it.

// reserveAgain returns the first tenure of a node's slot the way an operator's slot-release does, and reserves the node again as a replay of its release would.
func (f *fixture) reserveAgain(plan, node string) {
	f.t.Helper()
	key := SlotSubjectKey(plan, node)
	if _, err := f.s.ReleaseExecutionSlot(context.Background(), "slot-"+plan+"-"+node, "released", f.clock(), "parent", "operator", "held"); err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.InsertExecutionSlot(context.Background(), store.ExecutionSlotsRow{SlotID: "slot-" + plan + "-" + node + "-2", SubjectKind: SlotSubjectKind, SubjectKey: key, ParentTaskID: "parent", ProjectKey: "P-TEST",
		Tenure: 2, State: "held", ReservedBy: "parent", ReservedAt: f.clock()}); err != nil {
		f.t.Fatal(err)
	}
}

// A slot that was returned and reserved again has two tenures, and a release that does not name one is refused: the acceptance and the integration must return the newest by name.
func TestSlotWithSeveralTenuresIsReturnedByName(t *testing.T) {
	t.Run("accept", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.reportNode("rp", "A", acceptOpts{})
		k.holdSlotsFor("rp", "A")
		k.reserveAgain("rp", "A")
		res, err := k.accept("rp", "A", AcceptInput{})
		if err != nil || !res.SlotReleased {
			t.Fatalf("accept = %v %+v", err, res)
		}
		if k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = ? AND tenure = 2 AND state = 'released'", SlotSubjectKey("rp", "A")) != 1 ||
			k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = ? AND state = 'held'", SlotSubjectKey("rp", "A")) != 0 {
			t.Fatal("the newest tenure is still held")
		}
	})
	t.Run("integration", func(t *testing.T) {
		k := newIntegrationKit(t)
		repo := k.repo
		repo.git("checkout", "-q", "-b", "feature")
		feature := repo.commit("feature.txt", "feature")
		repo.git("checkout", "-q", "dev")
		repo.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
		k.declare("g", "I", "feature.txt")
		a := k.acceptNode("g", "I", acceptOpts{HeadSHA: feature, PR: 5, Forge: "owner/repo", Repository: repo.path})
		k.mark(a)
		k.holdSlotsFor("g", "I")
		k.reserveAgain("g", "I")
		res, err := k.observe()
		if err != nil || !res.Integrated || !res.SlotReleased {
			t.Fatalf("observe = %v %+v", err, res)
		}
		if k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = ? AND state = 'held'", SlotSubjectKey("g", "I")) != 0 {
			t.Fatal("the newest tenure is still held")
		}
	})
}

// The targets an acceptance is judged against are the plan's as it stands when the transaction commits: an outgoing edge that changes while the pull request or the tips are being read
// counts, because the verification and the readings describe the plan they were made under.
func TestTargetsAreJudgedAgainstThePlanNow(t *testing.T) {
	t.Run("accept", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.declare("rp", "I", "i.go")
		k.reportNode("rp", "I", acceptOpts{})
		k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, head1)
		read := k.sched.PRs
		k.sched.PRs = func(ctx context.Context, repository string, number int64) (PullRequest, error) {
			pr, err := read(ctx, repository, number)
			// the edge that named where I lands is replaced by one that names another repository
			k.putPlan("rp", int(k.snapshot("rp").Revision), "rp-retarget", doc{"op": dag.OpRetireEdge, "edge_id": "ij"},
				addEdge("ij2", "I", "J", dag.EdgeArtifactVerified, doc{"pins_code_head": true, "target_repository": "other/repo", "target_base_ref": "dev"}))
			return pr, err
		}
		_, err := k.accept("rp", "I", AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 7}})
		if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "other/repo") {
			t.Fatalf("accept = %v, want the target of the plan as it is now to decide", err)
		}
		if k.acceptCount() != 0 {
			t.Fatal("an acceptance was written against a target the plan no longer names")
		}
	})
	t.Run("integration", func(t *testing.T) {
		k := newIntegrationKit(t)
		repo := k.repo
		repo.git("checkout", "-q", "-b", "feature")
		feature := repo.commit("feature.txt", "feature")
		repo.git("checkout", "-q", "dev")
		repo.git("branch", "release")
		repo.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
		k.declare("g", "I", "feature.txt")
		a := k.acceptNode("g", "I", acceptOpts{HeadSHA: feature, PR: 5, Forge: "owner/repo", Repository: repo.path})
		k.mark(a)
		k.holdSlotsFor("g", "I")
		// while the tips are read a second integrated edge appears: its branch does not hold the head and nobody has observed it
		k.sched.testBeforeObserveTx = func() {
			k.putPlan("g", int(k.snapshot("g").Revision), "g-r2", addRelNode("L", dag.NodeNonPR), addEdge("il", "I", "L", dag.EdgeIntegrated, doc{"target_repository": repo.path, "target_base_ref": "release"}))
		}
		res, err := k.observe(Target{repo.path, "dev"})
		if err != nil || res.Integrated || res.SlotReleased {
			t.Fatalf("observe = %v %+v, want a node that still has a target to land on to stay unintegrated", err, res)
		}
		if k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = ? AND state = 'held'", SlotSubjectKey("g", "I")) != 1 {
			t.Fatal("the slot was returned before the node landed everywhere")
		}
	})
}

// A repeated acceptance is a replay only when it reads the same pull request at the same head: a head that moved on the forge, or another pull request, is refused whether the call would
// replay or revalidate, and a pull request that has since been merged at the accepted head is still the same output.
func TestAcceptReplayComparesTheForge(t *testing.T) {
	setup := func(t *testing.T) *releaseKit {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.declare("rp", "I", "i.go")
		k.reportNode("rp", "I", acceptOpts{})
		k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, head1)
		if _, err := k.accept("rp", "I", AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 7}}); err != nil {
			t.Fatal(err)
		}
		return k
	}
	named := AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 7}}
	t.Run("the same reading is a replay", func(t *testing.T) {
		k := setup(t)
		if res, err := k.accept("rp", "I", named); err != nil || !res.Replayed || res.HeadSHA != head1 {
			t.Fatalf("replay = %v %+v", err, res)
		}
	})
	t.Run("the head moved on the forge", func(t *testing.T) {
		k := setup(t)
		k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, strings.Repeat("2", 40))
		if _, err := k.accept("rp", "I", named); refusalReason(err) != "merge_candidate_moved" {
			t.Fatalf("replay at another head = %v", err)
		}
	})
	t.Run("another pull request", func(t *testing.T) {
		k := setup(t)
		k.forge.by["owner/repo#8"] = openPR("owner/repo", 8, head1)
		if _, err := k.accept("rp", "I", AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 8}}); refusalReason(err) != "disposition_conflict" {
			t.Fatalf("replay on another pull request = %v", err)
		}
	})
	t.Run("a revalidation reads the forge too", func(t *testing.T) {
		k := setup(t)
		digest := dig("criteria of the second ruling")
		n := relNode("I", dag.NodeImplementation)
		n["criteria_set_digest"] = digest
		k.putPlan("rp", int(k.snapshot("rp").Revision), "rp-r2", doc{"op": dag.OpUpdateNode, "node": n})
		k.exec("UPDATE canonical_criteria SET set_digest = ?", digest)
		k.exec("UPDATE verdict_context SET set_digest = ?", digest)
		k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, strings.Repeat("2", 40))
		if _, err := k.accept("rp", "I", named); refusalReason(err) != "merge_candidate_moved" {
			t.Fatalf("revalidation at another head = %v", err)
		}
		if k.count("SELECT COUNT(*) FROM dag_acceptance_revalidations") != 0 {
			t.Fatal("a revalidation was recorded for a head the acceptance does not hold")
		}
	})
	t.Run("merged at the accepted head", func(t *testing.T) {
		k := setup(t)
		merged := openPR("owner/repo", 7, head1)
		merged.State = "merged"
		k.forge.by["owner/repo#7"] = merged
		if res, err := k.accept("rp", "I", named); err != nil || !res.Replayed {
			t.Fatalf("replay after the merge = %v %+v", err, res)
		}
	})
}

// A relationship the registry replaced is nobody's to accept or correct, whatever its status says.
func TestSupersededRelationshipAcceptsAndCorrectsNothing(t *testing.T) {
	k := newReleaseKit(t)
	rid := k.correctionKit()
	k.exec("UPDATE relationships SET superseded_by = NULL")
	k.exec("PRAGMA foreign_keys = OFF")
	k.exec("UPDATE relationships SET superseded_by = 'rel-successor' WHERE relationship_id = ?", rid)
	if _, err := k.accept("rp", "A", AcceptInput{}); refusalReason(err) != "relationship_not_active" {
		t.Fatalf("accept = %v", err)
	}
	if _, err := k.sched.PrepareCorrection(context.Background(), "rp", "A", "parent", ManifestInput{RuleVersion: k.request(false).RuleVersion}, VerifyOptions{}); refusalReason(err) != "relationship_not_active" {
		t.Fatalf("prepare = %v", err)
	}
	k.openCorrection(rid, nil)
	if _, err := k.correct(""); refusalReason(err) != "relationship_not_active" {
		t.Fatalf("correct = %v", err)
	}
	if k.acceptCount() != 0 {
		t.Fatal("a superseded relationship's result was accepted")
	}
}

// ruleNeedsChanges rules the seeded report needs_changes through the relay's own verdict writer, as the parent's verdict command does, so the generation, the revision request the child
// receives and the findings stored are the writer's. It returns the event the child is sent.
func (k *releaseKit) ruleNeedsChanges(relationship string, findings ...delivery.Obj) string {
	k.t.Helper()
	event := "evt-" + relationship
	k.exec("DELETE FROM verdict_context WHERE event_id = ?", event)
	k.exec("DELETE FROM verdicts WHERE event_id = ?", event)
	k.exec("UPDATE relationships SET allowed_recipients = ? WHERE relationship_id = ?", `["parent","child-A"]`, relationship)
	var childTask string
	if err := k.s.DB.QueryRow("SELECT child_task_id FROM relationships WHERE relationship_id = ?", relationship).Scan(&childTask); err != nil {
		k.t.Fatal(err)
	}
	k.exec("UPDATE relationships SET allowed_recipients = ? WHERE relationship_id = ?", `["parent","`+childTask+`"]`, relationship)
	list := make([]any, len(findings))
	for i, f := range findings {
		list[i] = f
	}
	ack := delivery.NewAck(delivery.NewService(k.s, delivery.SystemClock{}))
	if _, err := ack.RecordVerdict(context.Background(), event, "needs_changes", "verdict-turn-real", nil, list, nil, releaseCriteriaDigest()); err != nil {
		k.t.Fatalf("the relay's own verdict writer refused the ruling: %v", err)
	}
	var revision string
	if err := k.s.DB.QueryRow("SELECT event_id FROM events WHERE relationship_id = ? AND outcome = 'revision_request'", relationship).Scan(&revision); err != nil {
		k.t.Fatal(err)
	}
	return revision
}

func restoration(note string) delivery.Obj {
	return delivery.Obj{{Key: "id", Value: "c1"}, {Key: "verdict", Value: "needs_changes"}, {Key: "note", Value: note}, {Key: "restoration", Value: true}}
}

// Criterion c6 through the relay's own writers: the manifest the parent prepared reaches the child as a file it can read, named in the revision request the child receives, and the
// generation the ruling opened is bound to exactly that manifest.
func TestCorrectionReachesTheChildThroughTheRealVerdictWriter(t *testing.T) {
	k := newReleaseKit(t)
	rid := k.correctionKit()
	prepared := k.prepare()
	data, err := os.ReadFile(prepared.FrozenPath)
	if err != nil {
		t.Fatalf("the child's copy of the manifest is not there: %v", err)
	}
	for _, want := range []string{prepared.FrozenPath, prepared.ManifestDigest, shaOf(data), "blocked_needs_input"} {
		if !strings.Contains(prepared.Instruction, want) {
			t.Fatalf("the instruction %q does not name %q", prepared.Instruction, want)
		}
	}
	revision := k.ruleNeedsChanges(rid, restoration(prepared.Instruction))
	message, err := delivery.Preview(context.Background(), k.s, revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"revision request", prepared.FrozenPath, prepared.ManifestDigest, "blocked_needs_input"} {
		if !strings.Contains(message, want) {
			t.Fatalf("the message the child receives does not carry %q:\n%s", want, message)
		}
	}
	res, err := k.correct("")
	if err != nil || res.ManifestDigest != prepared.ManifestDigest || res.Generation != 2 || res.CarriedOver {
		t.Fatalf("correction = %v %+v", err, res)
	}
	if k.count("SELECT COUNT(*) FROM relationships") != 1 {
		t.Fatal("a correction made another child")
	}
	// the digest given the second time is checked against the bound one too
	if _, err := k.correct(dig("another manifest")); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("a replay with another digest = %v", err)
	}
	if again, err := k.correct(prepared.ManifestDigest); err != nil || !again.Replayed {
		t.Fatalf("a replay with the right digest = %v %+v", err, again)
	}
}

// What the child is told must be unambiguous and must be about this node: two manifests in the restoration block, a manifest prepared for the same node id of another plan, a manifest
// prepared before the plan changed the node, and a copy the child cannot read are all refused, and nothing is bound.
func TestCorrectionRefusesWhatTheChildCouldNotUse(t *testing.T) {
	bound := func(k *releaseKit) int {
		return k.count("SELECT COUNT(*) FROM dag_node_executions WHERE execution_generation = 2")
	}
	t.Run("two manifests in one note", func(t *testing.T) {
		k := newReleaseKit(t)
		rid := k.correctionKit()
		prepared := k.prepare()
		k.ruleNeedsChanges(rid, restoration(prepared.Instruction+" and, in the same note, manifest "+dig("an invented one")))
		if _, err := k.correct(""); refusalReason(err) != "disposition_conflict" || bound(k) != 0 {
			t.Fatalf("correction = %v (%d bound)", err, bound(k))
		}
	})
	t.Run("the same node id in another plan", func(t *testing.T) {
		k := newReleaseKit(t)
		rid := k.correctionKit()
		other := doc{"node_id": "A", "issue_key": "CRW-OTHER", "kind": dag.NodeNonPR, "criteria_set_digest": releaseCriteriaDigest()}
		k.putPlan("other", 0, "other-r1", doc{"op": dag.OpAddNode, "node": other})
		snap := k.snapshot("other")
		node, _ := nodeOf(snap, "A")
		body, blocked, err := k.sched.BuildManifest(context.Background(), k.s.Q(context.Background()), "other", snap, node,
			ManifestInput{RuleVersion: k.request(false).RuleVersion, CreatedByTaskID: "parent", CreatedAt: k.clock()}, VerifyOptions{SkipFileBytes: true})
		if err != nil || len(blocked) != 0 {
			t.Fatalf("manifest of the other plan: %v %v", err, blocked)
		}
		raw, _ := json.Marshal(body)
		if _, err := k.repo.PutManifest(context.Background(), raw); err != nil {
			t.Fatal(err)
		}
		k.ruleNeedsChanges(rid, restoration("Please read manifest "+body["manifest_digest"].(string)))
		if _, err := k.correct(""); refusalReason(err) != "disposition_conflict" || bound(k) != 0 {
			t.Fatalf("correction = %v (%d bound)", err, bound(k))
		}
	})
	t.Run("the plan changed the node after the manifest was prepared", func(t *testing.T) {
		k := newReleaseKit(t)
		rid := k.correctionKit()
		prepared := k.prepare()
		n := relNode("A", dag.NodeNonPR)
		n["criteria_set_digest"] = dig("changed after the manifest was prepared")
		k.putPlan("rp", int(k.snapshot("rp").Revision), "rp-r2", doc{"op": dag.OpUpdateNode, "node": n})
		k.ruleNeedsChanges(rid, restoration(prepared.Instruction))
		if _, err := k.correct(""); refusalReason(err) != "disposition_conflict" || bound(k) != 0 {
			t.Fatalf("correction = %v (%d bound)", err, bound(k))
		}
	})
	t.Run("the note names another path for the copy", func(t *testing.T) {
		k := newReleaseKit(t)
		rid := k.correctionKit()
		prepared := k.prepare()
		k.ruleNeedsChanges(rid, restoration(strings.Replace(prepared.Instruction, prepared.FrozenPath, "/nowhere/"+filepath.Base(prepared.FrozenPath), 1)))
		if _, err := k.correct(""); refusalReason(err) != "disposition_conflict" || bound(k) != 0 {
			t.Fatalf("correction = %v (%d bound)", err, bound(k))
		}
	})
	t.Run("the note gives another hash for the copy", func(t *testing.T) {
		k := newReleaseKit(t)
		rid := k.correctionKit()
		prepared := k.prepare()
		data, _ := os.ReadFile(prepared.FrozenPath)
		k.ruleNeedsChanges(rid, restoration(strings.Replace(prepared.Instruction, "file sha256 "+shaOf(data), "file sha256 "+dig("another file"), 1)))
		if _, err := k.correct(""); refusalReason(err) != "disposition_conflict" || bound(k) != 0 {
			t.Fatalf("correction = %v (%d bound)", err, bound(k))
		}
	})
	t.Run("the note is about another generation", func(t *testing.T) {
		k := newReleaseKit(t)
		rid := k.correctionKit()
		prepared := k.prepare()
		k.ruleNeedsChanges(rid, restoration(strings.Replace(prepared.Instruction, "Correction generation 2 of", "Correction generation 7 of", 1)))
		if _, err := k.correct(""); refusalReason(err) != "disposition_conflict" || bound(k) != 0 {
			t.Fatalf("correction = %v (%d bound)", err, bound(k))
		}
	})
}

// Preparing a correction twice from the same inputs is the same manifest: the copy the child reads is the body the store holds, so the line the ruling carries and the file always agree,
// whatever time the second preparation was built at.
func TestPreparingACorrectionTwiceAgrees(t *testing.T) {
	k := newReleaseKit(t)
	rid := k.correctionKit()
	first := k.prepare()
	k.clock() // time moves between the two preparations
	second := k.prepare()
	if first.ManifestDigest != second.ManifestDigest || first.Instruction != second.Instruction || first.FrozenPath != second.FrozenPath {
		t.Fatalf("two preparations from the same inputs differ: %+v and %+v", first, second)
	}
	k.ruleNeedsChanges(rid, restoration(second.Instruction))
	res, err := k.correct("")
	if err != nil || res.ManifestDigest != first.ManifestDigest || res.CarriedOver {
		t.Fatalf("correction = %v %+v", err, res)
	}
}
