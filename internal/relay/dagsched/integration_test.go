package dagsched

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
)

// gitRepo is a real repository: a base commit on dev and helpers to make branches, merge them and read heads.
type gitRepo struct {
	t    *testing.T
	path string
}

func newGitRepo(t *testing.T) *gitRepo { return newGitRepoAt(t, t.TempDir()) }

func newGitRepoAt(t *testing.T, path string) *gitRepo {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	r := &gitRepo{t: t, path: path}
	r.git("init", "-q", "-b", "dev")
	r.git("config", "user.email", "t@example.com")
	r.git("config", "user.name", "t")
	r.git("config", "commit.gpgsign", "false")
	r.commit("base.txt", "base")
	return r
}

func (r *gitRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.path}, args...)...)
	cmd.Env = append(cleanGitEnv(), "GIT_AUTHOR_DATE=2026-10-02T00:00:00Z", "GIT_COMMITTER_DATE=2026-10-02T00:00:00Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *gitRepo) commit(file, content string) string {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.path, file), []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.git("add", file)
	r.git("commit", "-q", "-m", "change "+file)
	return r.git("rev-parse", "HEAD")
}

// integrationKit is a release kit over a real repository: the tip reader and the ancestry check are the production ones.
type integrationKit struct {
	*releaseKit
	repo *gitRepo
}

func newIntegrationKit(t *testing.T) *integrationKit {
	t.Helper()
	k := &integrationKit{releaseKit: newReleaseKit(t), repo: newGitRepo(t)}
	k.sched.Tips = mergeturn.TargetReader{}
	k.sched.Ancestry = GitAncestry{}.Ancestry
	// the plan: I lands on dev, K waits for it; D is a terminal node with no outgoing edge
	k.putPlan("g", 0, "g-r1", addRelNode("I", dag.NodeImplementation), addRelNode("K", dag.NodeNonPR), addRelNode("D", dag.NodeImplementation),
		addEdge("ik", "I", "K", dag.EdgeIntegrated, doc{"target_repository": k.repo.path}))
	return k
}

func (k *integrationKit) observe(targets ...Target) (IntegrationResult, error) {
	k.t.Helper()
	return k.sched.ObserveIntegration(context.Background(), "g", "I", "parent", targets)
}

func (k *integrationKit) mark(a accepted) {
	k.t.Helper()
	k.exec("INSERT OR IGNORE INTO assignment_marks (relationship_id, mark, event_id, execution_generation, revision_hash, evidence, actor, marked_at) VALUES (?, 'merged', ?, 1, ?, 'merged', 'parent', ?)",
		a.Acceptance.RelationshipID, a.Event, a.Acceptance.RevisionHash, k.clock())
}

// Criterion c5 (P-INT), c8: integration is an ancestry FACT the relay reads from a real repository, bound to the parent's merged mark on the same revision: a merge commit on the branch is
// ancestry, a squashed copy is not, an unmerged head is not, and neither the observation alone nor the mark alone integrates the node.
func TestIntegratedOnRealGit(t *testing.T) {
	k := newIntegrationKit(t)
	repo := k.repo
	repo.git("checkout", "-q", "-b", "feature")
	feature := repo.commit("feature.txt", "feature")
	repo.git("checkout", "-q", "dev")
	k.declare("g", "I", "feature.txt")
	a := k.acceptNode("g", "I", acceptOpts{HeadSHA: feature, PR: 5, Forge: "owner/repo", Repository: repo.path})
	k.holdSlotsFor("g", "I")

	// not merged: the observation says so and the node stays unintegrated
	res, err := k.observe()
	if err != nil || len(res.Observations) != 1 || res.Observations[0].IsAncestor || res.Integrated || res.Observations[0].Method != "git merge-base --is-ancestor" {
		t.Fatalf("unmerged = %v %+v", err, res)
	}
	if n := k.read("g").node("K"); n.Reason != WaitEdge("ik") {
		t.Fatalf("K = %+v", n)
	}
	// merged with a merge commit, no mark yet: ancestry is true and the node is still not integrated
	repo.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	res, err = k.observe()
	if err != nil || !res.Observations[0].IsAncestor || res.Observations[0].Seq != 2 || res.Integrated || res.MarkPresent || res.SlotReleased {
		t.Fatalf("merged, unmarked = %v %+v", err, res)
	}
	if n := k.read("g").node("K"); n.Reason != WaitEdge("ik") {
		t.Fatalf("K = %+v, want it still waiting for the mark", n)
	}
	// the same reading again is a replay
	if again, err := k.observe(); err != nil || !again.Observations[0].Replayed || again.Observations[0].Seq != 2 || k.count("SELECT COUNT(*) FROM dag_integration_observations") != 2 {
		t.Fatalf("replay = %v %+v", err, again)
	}
	// the mark arrives: integrated, K is released and the slot goes back
	k.mark(a)
	res, err = k.observe()
	if err != nil || !res.Integrated || !res.MarkPresent || !res.SlotReleased {
		t.Fatalf("marked = %v %+v", err, res)
	}
	if n := k.read("g").node("K"); n.Disposition != DispReady {
		t.Fatalf("K = %+v, want ready", n)
	}
	if n := k.read("g").node("I"); n.State != StateIntegrated {
		t.Fatalf("I = %+v", n)
	}
	if k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = ? AND state = 'held'", SlotSubjectKey("g", "I")) != 0 {
		t.Fatal("the slot is still held")
	}
	// a further observation changes nothing and releases nothing twice
	if again, err := k.observe(); err != nil || again.SlotReleased != false || !again.Integrated {
		t.Fatalf("after integration = %v %+v", err, again)
	}
}

// A squash or rebase landing keeps the content and loses the head: the accepted head is not an ancestor, and when a landed merge turn says the head was merged the reading names the contradiction.
func TestSquashLandingIsNotAncestry(t *testing.T) {
	k := newIntegrationKit(t)
	repo := k.repo
	repo.git("checkout", "-q", "-b", "feature")
	feature := repo.commit("feature.txt", "feature")
	repo.git("checkout", "-q", "dev")
	repo.git("merge", "-q", "--squash", "feature")
	repo.git("commit", "-q", "-m", "squashed feature")
	k.declare("g", "I", "feature.txt")
	a := k.acceptNode("g", "I", acceptOpts{HeadSHA: feature, PR: 5, Forge: "owner/repo", Repository: repo.path})
	k.mark(a)
	res, err := k.observe()
	if err != nil || res.Observations[0].IsAncestor || res.Integrated {
		t.Fatalf("squashed = %v %+v", err, res)
	}
	k.landedTurn(repo.path, "dev", feature)
	if n := k.read("g").node("K"); n.Reason != BlockedIntegrationUnprovable {
		t.Fatalf("K = %+v, want blocked:integration_unprovable", n)
	}
}

// A terminal node has no outgoing edge, so its target arrives with the first observation (--target); a node with several targets integrates only when every one of them contains the head.
func TestObserveTargetsAreTheRequiredSet(t *testing.T) {
	k := newIntegrationKit(t)
	repo := k.repo
	repo.git("checkout", "-q", "-b", "feature")
	feature := repo.commit("feature.txt", "feature")
	repo.git("checkout", "-q", "dev")
	repo.git("branch", "release")
	repo.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	k.putPlan("t", 0, "t-r1", addRelNode("D", dag.NodeImplementation))
	k.declare("t", "D", "x.go")
	a := k.acceptNode("t", "D", acceptOpts{HeadSHA: feature, PR: 6, Forge: "owner/repo", Repository: repo.path})
	k.mark(a)
	if _, err := k.sched.ObserveIntegration(context.Background(), "t", "D", "parent", nil); refusalReason(err) != "malformed_receipt" {
		t.Fatalf("a terminal node with no target = %v", err)
	}
	dev, release := Target{repo.path, "dev"}, Target{repo.path, "release"}
	res, err := k.sched.ObserveIntegration(context.Background(), "t", "D", "parent", []Target{dev})
	if err != nil || !res.Integrated {
		t.Fatalf("a terminal node observed on its one target = %v %+v", err, res)
	}
	// observed on a second target that does not contain the head: the target stays required from now on
	res, err = k.sched.ObserveIntegration(context.Background(), "t", "D", "parent", []Target{release})
	if err != nil || res.Observations[0].IsAncestor || res.Integrated {
		t.Fatalf("a second target without the head = %v %+v", err, res)
	}
	if n := k.read("t").node("D"); n.State == StateIntegrated {
		t.Fatalf("D = %+v: a target once observed excuses nobody", n)
	}
	// and observing only dev again does not complete it
	if res, err := k.sched.ObserveIntegration(context.Background(), "t", "D", "parent", []Target{dev}); err != nil || res.Integrated {
		t.Fatalf("a subset = %v %+v", err, res)
	}
	// the order the targets are judged in decides nothing: a target without the head that sorts first still holds the node back
	repo.git("branch", "alpha", "dev~1")
	if res, err := k.sched.ObserveIntegration(context.Background(), "t", "D", "parent", []Target{{repo.path, "alpha"}}); err != nil || res.Observations[0].IsAncestor || res.Integrated {
		t.Fatalf("a first target without the head = %v %+v", err, res)
	}
	if res, err := k.sched.ObserveIntegration(context.Background(), "t", "D", "parent", []Target{dev}); err != nil || res.Integrated {
		t.Fatalf("dev satisfied and alpha not = %v %+v", err, res)
	}
	// and a target that sorts last and holds the head does not decide for the others either
	repo.git("branch", "zeta", "dev")
	if res, err := k.sched.ObserveIntegration(context.Background(), "t", "D", "parent", []Target{{repo.path, "zeta"}}); err != nil || !res.Observations[0].IsAncestor || res.Integrated {
		t.Fatalf("the last target holds the head, the first two do not = %v %+v", err, res)
	}
}

// An observation is a replay only when everything it says is what the latest one said: the same tip with another answer about ancestry is a new reading.
func TestObservationReplayComparesTheAnswer(t *testing.T) {
	k := newIntegrationKit(t)
	k.declare("g", "I", "x.go")
	k.acceptNode("g", "I", acceptOpts{HeadSHA: head1, PR: 5, Forge: "owner/repo", Repository: k.repo.path})
	answer := false
	k.sched.Ancestry = func(context.Context, string, string, string) (bool, string, error) { return answer, "scripted", nil }
	first, err := k.observe(Target{k.repo.path, "dev"})
	if err != nil || first.Observations[0].IsAncestor || first.Observations[0].Seq != 1 {
		t.Fatalf("first = %v %+v", err, first)
	}
	if again, err := k.observe(Target{k.repo.path, "dev"}); err != nil || !again.Observations[0].Replayed {
		t.Fatalf("the same answer = %v %+v", err, again)
	}
	answer = true
	flipped, err := k.observe(Target{k.repo.path, "dev"})
	if err != nil || flipped.Observations[0].Replayed || flipped.Observations[0].Seq != 2 || !flipped.Observations[0].IsAncestor {
		t.Fatalf("the same tip, another answer = %v %+v", err, flipped)
	}
}

// Contract 3.2: nothing new is integrated for a paused or cancelled relationship, and a pause that lands while the tip and the ancestry are being read is seen under the lock.
func TestObserveRefusesPausedAndCancelled(t *testing.T) {
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
	for _, status := range []string{"paused", "cancelled"} {
		k.exec("UPDATE relationships SET status = ?", status)
		if _, err := k.observe(); refusalReason(err) != "relationship_not_active" {
			t.Fatalf("%s: %v", status, err)
		}
	}
	if k.count("SELECT COUNT(*) FROM dag_integration_observations") != 0 || k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = ? AND state = 'held'", SlotSubjectKey("g", "I")) != 1 {
		t.Fatal("a refused observation wrote a row or returned the slot")
	}
	// a pause that lands after the reads and before the commit
	k.exec("UPDATE relationships SET status = 'active'")
	k.sched.testBeforeObserveTx = func() { k.exec("UPDATE relationships SET status = 'paused'") }
	if _, err := k.observe(); refusalReason(err) != "relationship_not_active" {
		t.Fatalf("a pause during the reads = %v", err)
	}
	if k.count("SELECT COUNT(*) FROM dag_integration_observations") != 0 {
		t.Fatal("an observation was written after the pause")
	}
	// the accepted output was replaced by another under the observation (another acceptance at the same head): the observation was read for the old one and is not written
	k.exec("UPDATE relationships SET status = 'active'")
	k.sched.testBeforeObserveTx = func() {
		k.exec("UPDATE dag_acceptances SET state = 'superseded'")
		b := a.Acceptance
		b.AcceptanceID, b.RevisionHash, b.SupersedesAcceptanceID, b.State = dig("replacement acceptance"), dig("replacement revision"), a.Acceptance.AcceptanceID, "active"
		k.insertAcceptance(b)
	}
	if _, err := k.observe(); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("another acceptance during the reads = %v", err)
	}
	k.sched.testBeforeObserveTx = nil
	k.exec("DELETE FROM dag_acceptances WHERE acceptance_id = ?", dig("replacement acceptance"))
	k.exec("UPDATE dag_acceptances SET state = 'active'")
	// the accepted head moved under the observation
	k.sched.testBeforeObserveTx = func() { k.exec("UPDATE dag_acceptances SET head_sha = ?", strings.Repeat("9", 40)) }
	if _, err := k.observe(); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("a head that moved during the reads = %v", err)
	}
	k.sched.testBeforeObserveTx = nil
	k.exec("UPDATE dag_acceptances SET head_sha = ?", feature)
	// the head changed under the observation
	k.exec("UPDATE relationships SET status = 'active'")
	k.sched.testBeforeObserveTx = func() { k.exec("UPDATE dag_acceptances SET state = 'superseded'") }
	if _, err := k.observe(); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("a head superseded during the reads = %v", err)
	}
	k.sched.testBeforeObserveTx = nil
	k.exec("UPDATE dag_acceptances SET state = 'active'")
	res, err := k.observe()
	if err != nil || !res.Integrated || !res.SlotReleased {
		t.Fatalf("active again = %v %+v", err, res)
	}
}

// The ancestry of a forge repository is the compare API's: a fake gh answers behind_by 0 (ahead or identical) and 2 (diverged).
func TestGitAncestryForgeCompare(t *testing.T) {
	dir := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho \"$*\" > "+p+".args\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, c := range map[string]struct {
		body string
		want bool
		fail bool
	}{
		"ahead":     {`echo '{"status":"ahead","behind_by":0}'`, true, false},
		"identical": {`echo '{"status":"identical","behind_by":0}'`, true, false},
		"diverged":  {`echo '{"status":"diverged","behind_by":2}'`, false, false},
		"behind":    {`echo '{"status":"behind","behind_by":3}'`, false, false},
		"no field":  {`echo '{"status":"ahead"}'`, false, true},
		"gh fails":  {`echo boom >&2; exit 1`, false, true},
	} {
		tip := strings.Repeat("2", 40)
		gh := script(name, c.body)
		got, method, err := GitAncestry{GH: gh}.Ancestry(context.Background(), "owner/repo", head1, tip)
		if (err != nil) != c.fail || got != c.want || (err == nil && method != "gh api compare behind_by") {
			t.Errorf("%s: %v %q %v, want %v (fail %v)", name, got, method, err, c.want, c.fail)
		}
		// the commit is the base of the comparison and the tip its head: with the two swapped the same answers would mean the opposite
		if args, err := os.ReadFile(gh + ".args"); err != nil || strings.TrimSpace(string(args)) != "api --method GET repos/owner/repo/compare/"+head1+"..."+tip {
			t.Errorf("%s: the comparison asked for was %q (%v)", name, args, err)
		}
	}
	// a repository that is neither a path nor owner/name has no ancestry
	if _, _, err := (GitAncestry{}).Ancestry(context.Background(), "not a repo", head1, head1); err == nil {
		t.Error("an unreadable repository answered")
	}
}

// A linked working tree has a .git file, not a directory, and a bare repository has no .git at all: git is asked about the checkout, so the ancestry is proven in either.
func TestAncestryInLinkedAndBareCheckouts(t *testing.T) {
	repo := newGitRepo(t)
	base := repo.git("rev-parse", "HEAD")
	repo.git("checkout", "-q", "-b", "feature")
	feature := repo.commit("feature.txt", "feature")
	repo.git("checkout", "-q", "dev")
	linked := filepath.Join(t.TempDir(), "linked")
	repo.git("worktree", "add", "-q", linked, "-b", "linked-branch", "dev")
	bare := filepath.Join(t.TempDir(), "bare.git")
	repo.git("clone", "-q", "--bare", repo.path, bare)
	for name, path := range map[string]string{"plain": repo.path, "linked": linked, "bare": bare} {
		in, method, err := GitAncestry{}.Ancestry(context.Background(), path, base, feature)
		if err != nil || !in || method != "git merge-base --is-ancestor" {
			t.Errorf("%s: the base in the feature = %v %q %v", name, in, method, err)
		}
		if out, _, err := (GitAncestry{}).Ancestry(context.Background(), path, feature, base); err != nil || out {
			t.Errorf("%s: the feature in the base = %v %v", name, out, err)
		}
	}
}
