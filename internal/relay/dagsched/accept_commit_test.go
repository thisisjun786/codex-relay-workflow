package dagsched

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The pull-request-less acceptance path (CRW-965, criterion c1). A node whose result is a commit is
// accepted with --commit/--base/--verification/--checkout: the relay proves in the local checkout that
// the commit is the head the generation's ruling fixed, that it descends from the base, and that the
// verification record names this commit's tree with a PASS it may reuse. Every failure is refused with
// its own reason, and the pull-request path is untouched.

// commitAcceptKit is a release kit over a real repository, with the plan's implementation node I and a
// terminal node D.
type commitAcceptKit struct {
	*releaseKit
	repo *gitRepo
	head string // the commit the generation's ruling fixed
	base string // the commit the head descends from
}

func newCommitAcceptKit(t *testing.T) *commitAcceptKit {
	t.Helper()
	k := &commitAcceptKit{releaseKit: newReleaseKit(t), repo: newGitRepo(t)}
	k.repo.git("checkout", "-q", "-b", "feature")
	k.head = k.repo.commit("feature.txt", "feature")
	k.repo.git("checkout", "-q", "dev")
	k.base = k.repo.git("rev-parse", "dev")
	k.putPlan("g", 0, "g-r1", addRelNode("I", dag.NodeImplementation), addRelNode("D", dag.NodeImplementation))
	return k
}

// report fixes the head the generation's ruling verified (dag_verified_heads), which is what the
// commit path compares the --commit value against.
func (k *commitAcceptKit) report(plan, node string) accepted {
	k.t.Helper()
	r := k.reportNode(plan, node, acceptOpts{})
	k.exec("INSERT INTO dag_verified_heads (event_id, relationship_id, execution_generation, verdict_turn_id, head_sha, recorded_by_task_id, recorded_at) VALUES (?,?,?,?,?,?,?)",
		r.Event, r.Acceptance.RelationshipID, 1, "verdict-turn", k.head, "parent", k.clock())
	return r
}

// record writes a sealed verification-record/1 in the CRW-964 writer's format (camelCase members, the digest the
// writer computes), naming a tree and a result. The extra keys are the camelCase members a test changes: result,
// pinMismatch.
func (k *commitAcceptKit) record(tree, result string, extra map[string]any) string {
	k.t.Helper()
	record := VerificationRecord{Runner: "local", Repository: "owner/repo", BaseCommit: k.base, HeadCommit: k.head, TreeHash: tree,
		CiDigest: "", Tools: map[string]string{"go": "go1.27.1"}, Pins: map[string]string{}, GoFlags: "", GoEnv: "",
		PinMismatch: []string{}, Dependencies: map[string]string{"go.sum": "", "web/package-lock.json": ""}, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Result: result, Jobs: []json.RawMessage{}}
	if pins, ok := extra["pinMismatch"].([]string); ok {
		record.PinMismatch = pins
	}
	if value, ok := extra["result"].(string); ok {
		record.Result = value
	}
	keys, err := CommitVerificationKeys(context.Background(), k.repo.path, k.head)
	if err != nil {
		k.t.Fatal(err)
	}
	record.CiDigest, record.Dependencies = keys.CiDigest, keys.Dependencies
	raw, err := SealVerificationRecord(record)
	if err != nil {
		k.t.Fatal(err)
	}
	path := filepath.Join(k.t.TempDir(), "verification-record.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		k.t.Fatal(err)
	}
	return "@" + path
}

// treeOf reads the tree of a commit the way the relay does.
func (k *commitAcceptKit) treeOf(commit string) string {
	k.t.Helper()
	out, err := exec.Command("git", "-C", k.repo.path, "rev-parse", commit+"^{tree}").Output()
	if err != nil {
		k.t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func (k *commitAcceptKit) acceptCommit(record string) (AcceptResult, error) {
	k.t.Helper()
	return k.sched.Accept(context.Background(), "g", "I", "parent", premergeWithRecord(k.sched, context.Background(), "g", "I", "parent", AcceptInput{
		RuleVersion: VerifierRule{SkillsDigest: dig("skills"), Model: "m", Effort: "none"},
		Commit:      &CommitRef{Head: k.head, Base: k.base, Checkout: k.repo.path, Record: record},
	}))

}

func refusalReasonOf(err error) string {
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		return refused.Reason
	}
	return ""
}

// c1: the happy path. The acceptance records the commit as its head and the checkout as its
// repository, and writes the verification record with it.
func TestCommitAcceptanceRecordsTheCommit(t *testing.T) {
	t.Parallel()
	k := newCommitAcceptKit(t)
	k.report("g", "I")
	res, err := k.acceptCommit(k.record(k.treeOf(k.head), "pass", nil))
	if err != nil {
		t.Fatalf("accept by commit: %v", err)
	}
	if res.HeadSHA != k.head || res.AcceptanceID == "" || res.Generation != 1 || res.Replayed || res.Revalidated {
		t.Fatalf("the acceptance = %+v", res)
	}
	var repo, head string
	if _, err := queryOne(context.Background(), k.s.Q(context.Background()), "SELECT repository, head_sha FROM dag_acceptances WHERE acceptance_id = ?", []any{res.AcceptanceID}, &repo, &head); err != nil {
		t.Fatal(err)
	}
	if repo != k.repo.path || head != k.head {
		t.Fatalf("the acceptance row = repository %q head %q", repo, head)
	}
	// no forge row: the absence is what distinguishes the commit path
	if k.count("SELECT COUNT(*) FROM dag_acceptance_forge WHERE acceptance_id = ?", res.AcceptanceID) != 0 {
		t.Fatal("the commit path wrote a forge identity")
	}
	// the verification record is stored with the acceptance
	row, found, err := store.AcceptanceVerification(context.Background(), k.s, res.AcceptanceID)
	if err != nil || !found || row.HeadCommit != k.head || row.TreeSHA != k.treeOf(k.head) || row.RecordDigest == "" || row.RecordJSON == "" {
		t.Fatalf("the stored verification = %+v found=%v err=%v", row, found, err)
	}
}

// c1: a repeat of the same call is a replay, not a second acceptance.
func TestCommitAcceptanceReplays(t *testing.T) {
	t.Parallel()
	k := newCommitAcceptKit(t)
	k.report("g", "I")
	record := k.record(k.treeOf(k.head), "pass", nil)
	first, err := k.acceptCommit(record)
	if err != nil {
		t.Fatal(err)
	}
	again, err := k.acceptCommit(record)
	if err != nil || !again.Replayed || again.AcceptanceID != first.AcceptanceID {
		t.Fatalf("the replay = %+v err=%v", again, err)
	}
	if n := k.count("SELECT COUNT(*) FROM dag_acceptances WHERE plan_id = 'g' AND node_id = 'I'"); n != 1 {
		t.Fatalf("%d acceptances were written", n)
	}
}

// c1: a commit that is not the head the generation's ruling fixed is refused head_not_receipt_head.
func TestCommitAcceptanceRefusesACommitThatIsNotTheRuledHead(t *testing.T) {
	t.Parallel()
	k := newCommitAcceptKit(t)
	k.report("g", "I")
	// another commit on the feature branch, which the ruling never fixed
	k.repo.git("checkout", "-q", "feature")
	other := k.repo.commit("other.txt", "other")
	k.repo.git("checkout", "-q", "dev")
	_, err := k.sched.Accept(context.Background(), "g", "I", "parent", premergeWithRecord(k.sched, context.Background(), "g", "I", "parent", AcceptInput{RuleVersion: VerifierRule{SkillsDigest: "s", Model: "m", Effort: "none"},
		Commit: &CommitRef{Head: other, Base: k.base, Checkout: k.repo.path, Record: k.record(k.treeOf(other), "pass", nil)}}))

	if got := refusalReasonOf(err); got != "head_not_receipt_head" {
		t.Fatalf("reason %q (err %v)", got, err)
	}
	if n := k.count("SELECT COUNT(*) FROM dag_acceptances"); n != 0 {
		t.Fatalf("%d acceptance rows were written", n)
	}
}

// c1: a commit that does not descend from the base is refused merge_base_mismatch.
func TestCommitAcceptanceRefusesACommitThatDoesNotDescendFromTheBase(t *testing.T) {
	t.Parallel()
	k := newCommitAcceptKit(t)
	k.report("g", "I")
	// an unrelated base: the base of the acceptance is a commit the head does not descend from
	k.repo.git("checkout", "-q", "-b", "unrelated", "dev")
	k.repo.commit("unrelated.txt", "unrelated")
	unrelated := k.repo.git("rev-parse", "HEAD")
	k.repo.git("checkout", "-q", "dev")
	_, err := k.sched.Accept(context.Background(), "g", "I", "parent", premergeWithRecord(k.sched, context.Background(), "g", "I", "parent", AcceptInput{RuleVersion: VerifierRule{SkillsDigest: "s", Model: "m", Effort: "none"},
		Commit: &CommitRef{Head: k.head, Base: unrelated, Checkout: k.repo.path, Record: k.record(k.treeOf(k.head), "pass", nil)}}))

	if got := refusalReasonOf(err); got != "merge_base_mismatch" {
		t.Fatalf("reason %q (err %v)", got, err)
	}
	if n := k.count("SELECT COUNT(*) FROM dag_acceptances"); n != 0 {
		t.Fatalf("%d acceptance rows were written", n)
	}
}

// c1: a record naming another tree is refused revision_mismatch.
func TestCommitAcceptanceRefusesARecordOfAnotherTree(t *testing.T) {
	t.Parallel()
	k := newCommitAcceptKit(t)
	k.report("g", "I")
	_, err := k.acceptCommit(k.record(k.treeOf(k.base), "pass", nil))
	if got := refusalReasonOf(err); got != "revision_mismatch" {
		t.Fatalf("reason %q (err %v)", got, err)
	}
	if n := k.count("SELECT COUNT(*) FROM dag_acceptances"); n != 0 {
		t.Fatalf("%d acceptance rows were written", n)
	}
}

// c1: a record whose result is not PASS, or which is not reusable, is refused disposition_conflict.
func TestCommitAcceptanceRefusesAFailedOrNonReusableRecord(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		extra map[string]any
	}{
		{"fail", map[string]any{"result": "fail"}},
		{"pin mismatch", map[string]any{"pinMismatch": []string{"go"}}},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			k := newCommitAcceptKit(t)
			k.report("g", "I")
			_, err := k.acceptCommit(k.record(k.treeOf(k.head), "pass", c.extra))
			if got := refusalReasonOf(err); got != "disposition_conflict" {
				t.Fatalf("reason %q (err %v)", got, err)
			}
			if n := k.count("SELECT COUNT(*) FROM dag_acceptances"); n != 0 {
				t.Fatalf("%d acceptance rows were written", n)
			}
		})
	}
}

// c1: a record that is not a readable verification-record/1 is refused malformed_receipt.
func TestCommitAcceptanceRefusesAMalformedRecord(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		raw  string
	}{
		{"not json", "{"},
		{"wrong schema", `{"schema":"other/1","head_commit":"` + strings.Repeat("a", 40) + `","tree":"` + strings.Repeat("b", 40) + `","result":"PASS"}`},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			k := newCommitAcceptKit(t)
			k.report("g", "I")
			path := filepath.Join(t.TempDir(), "record.json")
			if err := os.WriteFile(path, []byte(c.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := k.acceptCommit("@" + path)
			if got := refusalReasonOf(err); got != "malformed_receipt" {
				t.Fatalf("reason %q (err %v)", got, err)
			}
			if n := k.count("SELECT COUNT(*) FROM dag_acceptances"); n != 0 {
				t.Fatalf("%d acceptance rows were written", n)
			}
		})
	}
}

// c1: the existing pull-request path is unchanged: a commit-path call is refused when the generation's
// ruling fixed no head at all.
func TestCommitAcceptanceRefusesWhenTheRulingFixedNoHead(t *testing.T) {
	t.Parallel()
	k := newCommitAcceptKit(t)
	k.reportNode("g", "I", acceptOpts{}) // no dag_verified_heads row
	_, err := k.acceptCommit(k.record(k.treeOf(k.head), "pass", nil))
	if got := refusalReasonOf(err); got != "head_not_receipt_head" {
		t.Fatalf("reason %q (err %v)", got, err)
	}
}
