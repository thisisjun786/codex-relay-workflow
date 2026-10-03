package dagsched

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// The scenario of CRW-430, built on real git and the relay's own writers where the existing kits have them: a node I accepted at its head on generation 1; the same child then refreshes the base in a
// generation opened by hand (three merges of dev into its branch, the middle one with a hand resolved conflict); the pull request lands and the parent marks the merge on generation 2's event.
type refreshScenario struct {
	*integrationKit
	rid               string
	accepted          accepted
	h1                string // the head the parent accepted
	head              string // the head of the pull request after the base was refreshed
	event2, revision2 string // the report of generation 2
	criteria          string
}

// tryGit is git that may fail (a merge with a conflict): the output and the error.
func (r *gitRepo) tryGit(args ...string) (string, error) {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.path}, args...)...)
	cmd.Env = append(cleanGitEnv(), "GIT_AUTHOR_DATE=2026-10-02T00:00:00Z", "GIT_COMMITTER_DATE=2026-10-02T00:00:00Z")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (r *gitRepo) write(file, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.path, file), []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// newRefreshScenario is the node accepted at generation 1 with its slot held, before anything else happened.
func newRefreshScenario(t *testing.T) *refreshScenario { return newRefreshScenarioWith(t, nil) }

// newRefreshScenarioWith is newRefreshScenario with a step on the branch of the pull request before the head the parent accepts is committed (a file to add, an attribute to commit).
func newRefreshScenarioWith(t *testing.T, onBranch func(repo *gitRepo)) *refreshScenario {
	t.Helper()
	k := newIntegrationKit(t)
	repo := k.repo
	repo.commit("shared.json", "version 0\n")
	repo.git("checkout", "-q", "-b", "feature")
	repo.write("shared.json", "version of the feature\n")
	repo.git("add", "shared.json")
	if onBranch != nil {
		onBranch(repo)
	}
	h1 := repo.commit("feature.txt", "feature")
	repo.git("checkout", "-q", "dev")
	// a node that waits for I's verified result (an artifact edge), beside K, which waits for its landing
	k.putPlan("g", 1, "g-r2", addRelNode("A", dag.NodeNonPR), addEdge("ia", "I", "A", dag.EdgeArtifactVerified, doc{"pins_code_head": true, "target_repository": k.repo.path, "target_base_ref": "dev"}))
	k.declare("g", "I", "feature.txt")
	a := k.acceptNode("g", "I", acceptOpts{HeadSHA: h1, PR: 7, Forge: "owner/repo", Repository: repo.path})
	k.holdSlotsFor("g", "I")
	n, _ := nodeOf(k.snapshot("g"), "I")
	k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, h1)
	return &refreshScenario{integrationKit: k, rid: a.Acceptance.RelationshipID, accepted: a, h1: h1, head: h1, criteria: n.CriteriaSetDigest}
}

// openGeneration is the coordinator opening generation 2 by hand and the child's report there, ruled verified under the plan's criteria.
func (s *refreshScenario) openGeneration() {
	s.t.Helper()
	rvOpenByHand(s.t, s.releaseKit, s.rid, "refresh-"+s.rid, true)
	s.event2 = s.rvReportGeneration(s.rid, "I", 2, s.criteria)
	if err := s.s.DB.QueryRow("SELECT revision_hash FROM events WHERE event_id = ?", s.event2).Scan(&s.revision2); err != nil {
		s.t.Fatal(err)
	}
}

// mergeDev merges dev into the branch of the pull request with a merge commit and returns the new head; with a conflict the merge is committed with the resolution given.
func (s *refreshScenario) mergeDev(message, resolvedFile, resolved string) string {
	s.t.Helper()
	repo := s.repo
	repo.git("checkout", "-q", "feature")
	if _, err := repo.tryGit("merge", "-q", "--no-ff", "-m", message, "dev"); err != nil {
		if resolvedFile == "" {
			s.t.Fatalf("merge of dev: %v", err)
		}
		repo.write(resolvedFile, resolved)
		repo.git("add", resolvedFile)
		repo.git("commit", "-q", "-m", message)
	}
	head := repo.git("rev-parse", "HEAD")
	repo.git("checkout", "-q", "dev")
	return head
}

// refreshBase is what the child does in generation 2: three merges of dev, the second one a conflict on shared.json that it resolves by hand. The pull request now shows the last of them.
func (s *refreshScenario) refreshBase() {
	s.t.Helper()
	repo := s.repo
	repo.commit("other.txt", "dev one")
	s.mergeDev("merge dev 1", "", "")
	repo.write("shared.json", "version of dev\n")
	repo.git("add", "shared.json")
	repo.git("commit", "-q", "-m", "dev changes shared.json")
	s.mergeDev("merge dev 2, shared.json resolved", "shared.json", "version of the feature and of dev\n")
	repo.commit("other-two.txt", "dev three")
	s.head = s.mergeDev("merge dev 3", "", "")
	s.forge.by["owner/repo#7"] = openPR("owner/repo", 7, s.head)
}

// land is the merge of the pull request into dev with a merge commit and the parent's merged mark on generation 2's event.
func (s *refreshScenario) land() {
	s.t.Helper()
	s.repo.git("checkout", "-q", "dev")
	s.repo.git("merge", "-q", "--no-ff", "-m", "merge the pull request", "feature")
	pr := openPR("owner/repo", 7, s.head)
	pr.State = "merged"
	s.forge.by["owner/repo#7"] = pr
	s.markGeneration2()
}

func (s *refreshScenario) markGeneration2() {
	s.t.Helper()
	s.exec("INSERT OR IGNORE INTO assignment_marks (relationship_id, mark, event_id, execution_generation, revision_hash, evidence, actor, marked_at) VALUES (?, 'merged', ?, 2, ?, 'merged', 'parent', ?)",
		s.rid, s.event2, s.revision2, s.clock())
}

func (s *refreshScenario) record(resolved ...string) (RefreshResult, error) {
	s.t.Helper()
	return s.sched.RecordBaseRefresh(context.Background(), "g", "I", "parent", RefreshInput{Resolved: resolved})
}

func (s *refreshScenario) slotHeld() bool {
	return s.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = ? AND state = 'held'", SlotSubjectKey("g", "I")) == 1
}

// The state CRW-430 is about, on the code without a record of the refresh: the head the parent accepted is contained in dev, the merged mark sits on generation 2, so the node reads is_ancestor true,
// integrated false and mark_present false, holds its slot, and the hand-opened generation is refused by dag-correct because the result is current. Nothing here is a defect to fix by itself: the
// relay does not integrate a node on a generation's mark until a base refresh is recorded.
func TestWithoutARecordTheRefreshGenerationLeavesTheNodeUnintegrated(t *testing.T) {
	s := newRefreshScenario(t)
	s.openGeneration()
	s.refreshBase()
	s.land()
	res, err := s.observe()
	if err != nil || len(res.Observations) != 1 || !res.Observations[0].IsAncestor || res.Integrated || res.MarkPresent || res.SlotReleased {
		t.Fatalf("observe = %v %+v, want the accepted head contained and nothing integrated", err, res)
	}
	if !s.slotHeld() {
		t.Fatal("the slot was returned without an integration")
	}
	if n := s.read("g").node("I"); n.State == StateIntegrated || n.Disposition == DispDone && n.Reason == DoneIntegrated {
		t.Fatalf("I = %+v, want it not integrated", n)
	}
	_, err = s.sched.RecordCorrection(context.Background(), "g", "I", "parent", "")
	if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "is not stale") {
		t.Fatalf("dag-correct = %v, want disposition_conflict naming that the result is not stale", err)
	}
}
