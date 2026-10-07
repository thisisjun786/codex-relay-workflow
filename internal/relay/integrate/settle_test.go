package integrate

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// settleRepo is a temporary checkout with a base commit and one branch per candidate, each adding one file.
type settleRepo struct {
	dir  string
	base string
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=crw-test", "-c", "user.email=crw-test@example.invalid"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newSettleRepo makes a checkout whose base branch holds base.txt.
func newSettleRepo(t *testing.T) settleRepo {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	writeFile(t, dir, "base.txt", "base\n")
	gitIn(t, dir, "add", "base.txt")
	gitIn(t, dir, "commit", "-q", "-m", "base")
	return settleRepo{dir: dir, base: gitIn(t, dir, "rev-parse", "HEAD")}
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// candidate commits one file on its own branch off the base and returns it as a candidate.
func (r settleRepo) candidate(t *testing.T, node, file string) dagsched.Candidate {
	t.Helper()
	gitIn(t, r.dir, "checkout", "-q", "-B", "cand-"+node, r.base)
	writeFile(t, r.dir, file, node+"\n")
	gitIn(t, r.dir, "add", file)
	gitIn(t, r.dir, "commit", "-q", "-m", node)
	return dagsched.Candidate{NodeID: node, AcceptanceID: "acc-" + node, HeadSHA: gitIn(t, r.dir, "rev-parse", "HEAD")}
}

// recordingVerifier passes a merged tree unless it holds bad.txt, and counts its runs.
func recordingVerifier(runs *int) verifier {
	return func(ctx context.Context, checkout, tree string) ([]byte, error) {
		*runs++
		cmd := exec.CommandContext(ctx, "git", "ls-tree", "-r", "--name-only", tree)
		cmd.Dir = checkout
		out, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		result := "PASS"
		if strings.Contains(string(out), "bad.txt") {
			result = "FAIL"
		}
		return json.Marshal(map[string]any{"schema": dagsched.VerificationRecordSchema, "head_commit": "h", "base_commit": "b", "tree": tree, "result": result, "reusable": true})
	}
}

func TestSettleVerifiesOnceWhenTheMergedTreePasses(t *testing.T) {
	repo := newSettleRepo(t)
	candidates := []dagsched.Candidate{repo.candidate(t, "a", "a.txt"), repo.candidate(t, "b", "b.txt"), repo.candidate(t, "c", "c.txt")}
	runs := 0
	st, err := settle(context.Background(), BatchInput{Checkout: repo.dir}, batchRunner{verifier: recordingVerifier(&runs)}, repo.base, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.merged) != 3 || len(st.split) != 0 {
		t.Fatalf("merged %d, split %d; want 3 and 0", len(st.merged), len(st.split))
	}
	if runs != 1 {
		t.Fatalf("the merged tree was verified %d times; want once", runs)
	}
	if st.record.Result != "PASS" {
		t.Fatalf("record result %q; want PASS", st.record.Result)
	}
}

// The red-first case of the criteria: one failing candidate among several is isolated and the others integrate.
func TestSettleSplitsOutTheFailingCandidateAndIntegratesTheRest(t *testing.T) {
	repo := newSettleRepo(t)
	candidates := []dagsched.Candidate{repo.candidate(t, "a", "a.txt"), repo.candidate(t, "b", "bad.txt"), repo.candidate(t, "c", "c.txt")}
	runs := 0
	st, err := settle(context.Background(), BatchInput{Checkout: repo.dir}, batchRunner{verifier: recordingVerifier(&runs)}, repo.base, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.merged) != 2 || st.merged[0].NodeID != "a" || st.merged[1].NodeID != "c" {
		t.Fatalf("merged %+v; want a and c", st.merged)
	}
	if len(st.split) != 1 || st.split[0].NodeID != "b" || st.split[0].Reason != "verification_failed" {
		t.Fatalf("split %+v; want b with verification_failed", st.split)
	}
	if st.record.Result != "PASS" {
		t.Fatalf("the branch may only move to a PASS tree, got %q", st.record.Result)
	}
	files := gitIn(t, repo.dir, "ls-tree", "-r", "--name-only", st.tree)
	if strings.Contains(files, "bad.txt") || !strings.Contains(files, "a.txt") || !strings.Contains(files, "c.txt") {
		t.Fatalf("the settled tree holds %q; want a.txt and c.txt without bad.txt", files)
	}
}

// The expected-head guard: the branch moves only from the head the batch read. A head that moved under the
// batch is refused and the ref keeps the value another writer gave it.
func TestUpdateRefRefusesAHeadThatMovedUnderTheBatch(t *testing.T) {
	repo := newSettleRepo(t)
	read := repo.base
	gitIn(t, repo.dir, "branch", "dev-int", read)
	moved := repo.candidate(t, "other", "other.txt")
	gitIn(t, repo.dir, "update-ref", "refs/heads/dev-int", moved.HeadSHA)
	mine := repo.candidate(t, "mine", "mine.txt").HeadSHA
	if err := updateRef(context.Background(), repo.dir, "dev-int", mine, read); err == nil {
		t.Fatal("the update moved the branch from a head it had not read")
	}
	if got := gitIn(t, repo.dir, "rev-parse", "refs/heads/dev-int"); got != moved.HeadSHA {
		t.Fatalf("the branch is %s; the writer that moved it was overwritten", got)
	}
}
