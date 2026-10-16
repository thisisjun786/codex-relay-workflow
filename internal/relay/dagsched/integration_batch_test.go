package dagsched

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The integration batch through the production path (CRW-965, criteria c2, c3, c6 and the evaluation's d11). Every
// candidate is a real commit in a real repository, accepted with dag-accept's commit path, marked by the batch, and
// the verification is a real stub script that runs in the merged worktree. Nothing is seeded in SQL.

// batchNode is one implementation node: the files its feature branch commits on top of dev.
type batchNode struct {
	name  string
	files map[string]string
}

// batchKit is a plan of implementation nodes over one repository whose dev branch is the base.
type batchKit struct {
	*releaseKit
	repo  *gitRepo
	base  string
	heads map[string]string
}

func newBatchKit(t *testing.T, nodes ...batchNode) *batchKit {
	t.Helper()
	k := &batchKit{releaseKit: newReleaseKit(t), repo: newGitRepo(t), heads: map[string]string{}}
	k.base = k.repo.git("rev-parse", "dev")
	var changes []doc
	for _, n := range nodes {
		k.repo.git("checkout", "-q", "-B", "feature-"+n.name, k.base)
		for path, body := range n.files {
			k.repo.write(path, body)
			k.repo.git("add", path)
		}
		k.repo.git("commit", "-q", "-m", "node "+n.name)
		k.heads[n.name] = k.repo.git("rev-parse", "HEAD")
		changes = append(changes, addRelNode(n.name, dag.NodeImplementation))
	}
	k.repo.git("checkout", "-q", "dev")
	k.putPlan("g", 0, "g-r1", changes...)
	for _, n := range nodes {
		r := k.reportNode("g", n.name, acceptOpts{})
		k.exec("INSERT INTO dag_verified_heads (event_id, relationship_id, execution_generation, verdict_turn_id, head_sha, recorded_by_task_id, recorded_at) VALUES (?,?,?,?,?,?,?)",
			r.Event, r.Acceptance.RelationshipID, 1, "verdict-turn", k.heads[n.name], "parent", k.clock())
	}
	return k
}

// acceptByCommit accepts one node through dag-accept's commit path with a record the writer would have sealed.
func (k *batchKit) acceptByCommit(node string) {
	k.t.Helper()
	head := k.heads[node]
	record := writeSealedRecord(k.t, k.repo.path, head, k.base, k.treeOf(head), "pass")
	if _, err := k.sched.Accept(context.Background(), "g", node, "parent", premergeWithRecord(k.sched, context.Background(), "g", node, "parent", AcceptInput{
		RuleVersion: VerifierRule{SkillsDigest: dig("skills"), Model: "m", Effort: "none"},
		Commit:      &CommitRef{Head: head, Base: k.base, Checkout: k.repo.path, Record: record},
	})); err != nil {
		k.t.Fatalf("accept %s: %v", node, err)
	}
}

// treeOf is the tree of a commit in the kit's repository.
func (k *batchKit) treeOf(commit string) string {
	k.t.Helper()
	return k.repo.git("rev-parse", commit+"^{tree}")
}

// writeSealedRecord writes a verification-record/1 in CRW-964's format, sealed by the same function the judge reads,
// and answers it as the "@file" spelling the accept path takes.
func writeSealedRecord(t *testing.T, checkout, head, base, tree, result string) string {
	t.Helper()
	keys, err := CommitVerificationKeys(context.Background(), checkout, head)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := SealVerificationRecord(VerificationRecord{Runner: "local", Repository: "owner/repo", BaseCommit: base, HeadCommit: head,
		TreeHash: tree, CiDigest: keys.CiDigest, Tools: copyPins(keys.Pins), Pins: copyPins(keys.Pins), GoFlags: "", GoEnv: "",
		PinMismatch: []string{}, Dependencies: keys.Dependencies, OS: runtime.GOOS, Arch: runtime.GOARCH, Result: result, Jobs: []json.RawMessage{}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "verification-record.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return "@" + path
}

// writeStubVerifier is a real verify command: it exits 1 when a bad.txt marker is present in the worktree it runs in,
// and otherwise writes its record through this test binary (TestVerifyRecordHelper).
func writeStubVerifier(t *testing.T) string {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The helper's two variables live in the script, not in this process's environment (t.Setenv is process-wide and forbids
	// t.Parallel): the verifier the batch runs sees exactly the variables it saw when they were set here.
	script := filepath.Join(t.TempDir(), "verify.sh")
	body := "#!/bin/sh\nCRW_TEST_RECORD_HELPER=1\nCRW_TEST_HELPER_BIN='" + strings.ReplaceAll(bin, "'", `'\''`) + "'\nexport CRW_TEST_RECORD_HELPER CRW_TEST_HELPER_BIN\nif [ -e bad.txt ]; then\n  echo 'bad.txt is present: the merged tree fails' >&2\n  exit 1\nfi\nexec \"$CRW_TEST_HELPER_BIN\" -test.run='^TestVerifyRecordHelper$' -test.count=1\n"
	writeExecutable(t, script, body)
	return script
}

// writeExecutable writes an executable file that a test then runs. The write holds syscall.ForkLock for reading, so no other test's fork
// can hold the write descriptor open when this one execs the file: Linux refuses such an exec with ETXTBSY, which is what keeps the stand-ins
// of tests that run beside others from flaking.
func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	syscall.ForkLock.RLock()
	err := os.WriteFile(path, []byte(body), 0o700)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
}

// stubVerifier runs the verify script in the directory the batch names, with the batch's environment added.
func stubVerifier(script string) IntegrationBatchVerifier {
	return func(ctx context.Context, dir string, env []string) error {
		cmd := exec.CommandContext(ctx, script)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

// updateIntegrationRef is the compare-and-swap the production command runs.
func updateIntegrationRef(ctx context.Context, checkout, ref, newCommit, oldCommit string) error {
	_, err := runGit(ctx, checkout, nil, "update-ref", "refs/heads/"+ref, newCommit, oldCommit)
	return err
}

// batchIn is the batch input the tests share.
func (k *batchKit) batchIn() IntegrationBatchInput {
	return IntegrationBatchInput{Plan: "g", Actor: "parent", Checkout: k.repo.path, IntegrationRef: "dev-int", BaseRef: "dev"}
}

// branchTip reads a local branch, and whether it exists.
func (k *batchKit) branchTip(name string) (string, bool) {
	tip, found, err := integrationBranchTip(context.Background(), k.repo.path, name)
	if err != nil {
		k.t.Fatal(err)
	}
	return tip, found
}

// stageRows reads the batch's stage rows of the plan.
func (k *batchKit) stageRows() []store.IntegrationStageRow {
	rows, err := store.IntegrationStagesOfPlan(context.Background(), k.sched.Store, "g")
	if err != nil {
		k.t.Fatal(err)
	}
	return rows
}

// countStage counts the stage rows of one kind.
func countStage(rows []store.IntegrationStageRow, stage string) int {
	n := 0
	for _, r := range rows {
		if r.Stage == stage {
			n++
		}
	}
	return n
}

// TestVerifyRecordHelper is the verification stub's record writer. It runs only when the stub sets its environment, so
// the ordinary test run skips it. It seals the record of the worktree's merged commit, naming the base's tree instead when
// CRW_TEST_WRONG_TREE is set, the case where the verifier was given a tree other than the merged one.
func TestVerifyRecordHelper(t *testing.T) {
	t.Parallel()
	if os.Getenv("CRW_TEST_RECORD_HELPER") != "1" {
		t.Skip("the verification stub's record writer")
	}
	ctx := context.Background()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	head, err := runGit(ctx, cwd, nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	head = strings.TrimSpace(head)
	base := os.Getenv("CRW_VERIFY_BASE")
	tree := ""
	if os.Getenv("CRW_TEST_WRONG_TREE") == "1" {
		tree, err = runGit(ctx, cwd, nil, "rev-parse", base+"^{tree}")
	} else {
		tree, err = runGit(ctx, cwd, nil, "rev-parse", head+"^{tree}")
	}
	if err != nil {
		t.Fatal(err)
	}
	keys, err := CommitVerificationKeys(ctx, cwd, head)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := SealVerificationRecord(VerificationRecord{Runner: "local", Repository: "owner/repo", BaseCommit: base, HeadCommit: head,
		TreeHash: strings.TrimSpace(tree), CiDigest: keys.CiDigest, Tools: copyPins(keys.Pins), Pins: copyPins(keys.Pins),
		PinMismatch: []string{}, Dependencies: keys.Dependencies, OS: runtime.GOOS, Arch: runtime.GOARCH, Result: "pass", Jobs: nil})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("CRW_VERIFY_RECORD"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// c2 (the evaluation's d11): the merged tree is verified where it is merged. Two clean candidates integrate, both are
// marked, and the branch is created by the batch (create-only, from nothing).
func TestIntegrationBatchIntegratesAndMarksEveryCandidate(t *testing.T) {
	t.Parallel()
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}}, batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
	k.acceptByCommit("a")
	k.acceptByCommit("b")
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Merged) != 2 || len(res.Split) != 0 {
		t.Fatalf("merged %d split %d; want 2 and 0: %+v", len(res.Merged), len(res.Split), res)
	}
	tip, found := k.branchTip("dev-int")
	if !found || tip != res.NewHead {
		t.Fatalf("the integration branch is %s (found %v); want %s", tip, found, res.NewHead)
	}
	files := k.repo.git("ls-tree", "-r", "--name-only", tip)
	if !strings.Contains(files, "a.txt") || !strings.Contains(files, "b.txt") {
		t.Fatalf("the integration branch holds %q; want a.txt and b.txt", files)
	}
	if n := countStage(k.stageRows(), "marked"); n != 2 {
		t.Fatalf("%d merged marks written; want 2", n)
	}
}

// c2: one failing candidate among several is isolated and the others integrate. The stub fails the merged tree that
// holds bad.txt, so the batch bisects to bad and leaves only it out.
func TestIntegrationBatchIsolatesTheFailingCandidate(t *testing.T) {
	t.Parallel()
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}},
		batchNode{name: "bad", files: map[string]string{"bad.txt": "bad\n"}},
		batchNode{name: "c", files: map[string]string{"c.txt": "c\n"}})
	for _, n := range []string{"a", "bad", "c"} {
		k.acceptByCommit(n)
	}
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Merged) != 2 || res.Merged[0].NodeID != "a" || res.Merged[1].NodeID != "c" {
		t.Fatalf("merged %+v; want a and c", res.Merged)
	}
	if len(res.Split) != 1 || res.Split[0].NodeID != "bad" || res.Split[0].Reason != "verification_failed" {
		t.Fatalf("split %+v; want bad with verification_failed", res.Split)
	}
	files := k.repo.git("ls-tree", "-r", "--name-only", res.NewHead)
	if strings.Contains(files, "bad.txt") || !strings.Contains(files, "a.txt") || !strings.Contains(files, "c.txt") {
		t.Fatalf("the branch holds %q; want a.txt and c.txt without bad.txt", files)
	}
}

// c2: a candidate that conflicted only with the candidate that was removed integrates on the retry.
func TestIntegrationBatchRetriesACandidateThatOnlyConflictedWithTheRemovedOne(t *testing.T) {
	t.Parallel()
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n", "bad.txt": "bad\n", "shared.txt": "a\n"}},
		batchNode{name: "b", files: map[string]string{"b.txt": "b\n", "shared.txt": "b\n"}})
	k.acceptByCommit("a")
	k.acceptByCommit("b")
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Merged) != 1 || res.Merged[0].NodeID != "b" {
		t.Fatalf("merged %+v; want b alone, integrated on the retry", res.Merged)
	}
	if len(res.Split) != 1 || res.Split[0].NodeID != "a" || res.Split[0].Reason != "verification_failed" {
		t.Fatalf("split %+v; want a with verification_failed", res.Split)
	}
}

// c3: the expected-head guard. The branch moved between the batch's read and its update, so the update is refused, the
// branch keeps the value the other writer gave it, and no ref_moved stage is written.
func TestIntegrationBatchRefusesAHeadThatMovedUnderIt(t *testing.T) {
	t.Parallel()
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	other := k.repo.git("rev-parse", "dev")
	k.repo.write("other.txt", "other\n")
	k.repo.git("add", "other.txt")
	k.repo.git("commit", "-q", "-m", "other writer")
	moved := k.repo.git("rev-parse", "HEAD")
	k.repo.git("checkout", "-q", "dev")
	deps := IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: func(ctx context.Context, checkout, ref, newCommit, oldCommit string) error {
		if _, err := runGit(ctx, checkout, nil, "update-ref", "refs/heads/"+ref, moved, ""); err != nil {
			t.Fatal(err)
		}
		return updateIntegrationRef(ctx, checkout, ref, newCommit, oldCommit)
	}}
	_, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), deps)
	if refusalReasonOf(err) != "stale_mark_context" {
		t.Fatalf("reason %q (err %v); want stale_mark_context", refusalReasonOf(err), err)
	}
	if tip, _ := k.branchTip("dev-int"); tip != moved {
		t.Fatalf("the branch is %s; the other writer's %s was overwritten (base %s)", tip, moved, other)
	}
	if n := countStage(k.stageRows(), "ref_moved"); n != 0 {
		t.Fatalf("%d ref_moved stages after a refused update", n)
	}
}

// d11: a verifier given a tree other than the merged one. The stub names the base's tree in its record, so no
// candidate passes, nothing is merged and the branch is not created.
func TestIntegrationBatchRefusesARecordOfAnotherTree(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	t.Setenv("CRW_TEST_WRONG_TREE", "1")
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Merged) != 0 || len(res.Split) != 1 || res.Split[0].Reason != "verification_failed" {
		t.Fatalf("merged %+v split %+v; want nothing merged and a verification_failed split", res.Merged, res.Split)
	}
	if _, found := k.branchTip("dev-int"); found {
		t.Fatal("the integration branch was created for a tree the verifier did not verify")
	}
}

// D-B: the integration is parent-only. A candidate of another relationship's parent is refused and nothing is written.
func TestIntegrationBatchRefusesANonParentActor(t *testing.T) {
	t.Parallel()
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	in := k.batchIn()
	in.Actor = "intruder"
	if _, err := k.sched.IntegrateBatch(context.Background(), in, IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}); err == nil {
		t.Fatal("a non-parent integrated a candidate")
	}
	if _, found := k.branchTip("dev-int"); found {
		t.Fatal("the integration branch was created by a non-parent")
	}
	if n := len(k.stageRows()); n != 0 {
		t.Fatalf("%d stage rows were written for a refused batch", n)
	}
}

// D-D: a candidate whose spec changed after its acceptance is stale, so the batch leaves it out of the candidates and
// merges the rest. The stale node is not a refusal of the batch: it is not ready.
func TestIntegrationBatchLeavesOutAStaleCandidate(t *testing.T) {
	t.Parallel()
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}}, batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
	k.acceptByCommit("a")
	k.acceptByCommit("b")
	k.invRevise("g", "a", "g-r2", invTitle("changed after the acceptance"))
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Merged) != 1 || res.Merged[0].NodeID != "b" {
		t.Fatalf("merged %+v; want b alone (a is stale)", res.Merged)
	}
}

// D-B: the batch goes through the coordinator-epoch fence dag-accept uses. A session that holds another epoch is refused
// before anything is read or written.
func TestIntegrationBatchRefusesAStaleEpoch(t *testing.T) {
	t.Parallel()
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	k.sched.ExpectedEpoch = 7
	_, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if refusalReasonOf(err) != "stale_coordinator_epoch" {
		t.Fatalf("reason %q (err %v); want stale_coordinator_epoch", refusalReasonOf(err), err)
	}
	if _, found := k.branchTip("dev-int"); found {
		t.Fatal("the integration branch was created under a stale epoch")
	}
}

// P1-1: a mark that could not be written leaves its batch visible as ref_moved with a pending mark, and the next run
// completes it from the frozen row without an error. The candidate is made stale between the verification and the
// mark, so its mark is refused; restoring it lets the completion write the mark.
func TestIntegrationBatchCompletesAPendingMarkFromItsFrozenRow(t *testing.T) {
	t.Parallel()
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	deps := IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef, AfterMove: func(context.Context) error {
		k.invRevise("g", "a", "g-r2", invTitle("changed before the mark"))
		return nil
	}}
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), deps)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Pending) != 1 || res.Pending[0] != "a" {
		t.Fatalf("pending %+v; want a", res.Pending)
	}
	if countStage(k.stageRows(), "mark_pending") != 1 {
		t.Fatal("the pending mark was not recorded")
	}
	in := k.batchIn()
	if err := k.sched.completePendingMarks(context.Background(), in); err != nil {
		t.Fatalf("completion while the candidate is still stale: %v", err)
	}
	if countStage(k.stageRows(), "marked") != 0 {
		t.Fatal("a stale candidate's mark was written")
	}
	k.invRevise("g", "a", "g-r3", func(n doc) {})
	if err := k.sched.completePendingMarks(context.Background(), in); err != nil {
		t.Fatalf("completion: %v", err)
	}
	if countStage(k.stageRows(), "marked") != 1 {
		t.Fatal("the pending mark was not completed from its frozen row")
	}
}

// P1-2: a batch that split every candidate moved nothing; running the same batch again is the same identity and must
// not collide with its own intent rows. The second run, with the verifier fixed, integrates the candidate.
func TestIntegrationBatchReRunsAfterAnUnmovedBatch(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	t.Setenv("CRW_TEST_WRONG_TREE", "1")
	first, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil || len(first.Merged) != 0 {
		t.Fatalf("first run: %+v, %v; want nothing merged", first, err)
	}
	t.Setenv("CRW_TEST_WRONG_TREE", "")
	second, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("re-run of the same batch: %v", err)
	}
	if len(second.Merged) != 1 || second.Merged[0].NodeID != "a" {
		t.Fatalf("re-run merged %+v; want a", second.Merged)
	}
}

// ---- CRW-1595 ----

// gitAt is git in dir, as a test reader.
func gitAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %v: %v\n%s", dir, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitFile is the content of a file of a checkout, and whether it exists.
func gitFile(t *testing.T, dir, name string) (string, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), true
}

// journalRows is the detail of the journal rows of one kind, in order.
func (k *batchKit) journalRows(kind string) []string {
	k.t.Helper()
	rows, err := k.s.DB.QueryContext(context.Background(), "SELECT detail FROM journal WHERE kind = ? ORDER BY seq", kind)
	if err != nil {
		k.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			k.t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// CRW-1595 (finding 1): a batch never moves the integration branch under a worktree that has it checked out — the
// worktree's files and index would stay on the old head while its branch moved. The refusal comes before the
// verification runs (no verifier call, no ref_moved or merged mark), and the checkout is left exactly as it was:
// the batch never resets or detaches a worktree for the user.
func TestCRW1595IntegrationRefusesACheckedOutBranch(t *testing.T) {
	t.Parallel()
	for _, place := range []string{"primary", "linked"} {
		t.Run(place, func(t *testing.T) {
			k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
			k.acceptByCommit("a")
			k.repo.git("branch", "dev-int", k.base)
			checked := k.repo.path
			if place == "linked" {
				// a path with a space and a newline proves the worktree list is read with -z
				checked = filepath.Join(t.TempDir(), "linked wt\nint")
				k.repo.git("worktree", "add", checked, "dev-int")
				t.Cleanup(func() {
					_, _ = runGit(context.Background(), k.repo.path, nil, "worktree", "remove", "--force", checked)
				})
			} else {
				k.repo.git("checkout", "-q", "dev-int")
			}
			tip, _ := k.branchTip("dev-int")
			status := gitAt(t, checked, "status", "--porcelain")
			staged := gitAt(t, checked, "diff", "--cached")
			baseTxt, hasBase := gitFile(t, checked, "base.txt")
			calls := 0
			deps := IntegrationBatchDeps{Update: updateIntegrationRef, Verify: func(ctx context.Context, dir string, env []string) error {
				calls++
				return stubVerifier(writeStubVerifier(t))(ctx, dir, env)
			}}
			_, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), deps)
			if err == nil || !strings.Contains(err.Error(), "checked out") {
				t.Fatalf("a batch whose branch is checked out: %v", err)
			}
			if calls != 0 {
				t.Fatalf("the verifier ran %d time(s) for a branch that is checked out", calls)
			}
			if now, _ := k.branchTip("dev-int"); now != tip {
				t.Fatalf("the branch moved to %s under its worktree", now)
			}
			if got := gitAt(t, checked, "status", "--porcelain"); got != status {
				t.Fatalf("the checkout's status changed: %q -> %q", status, got)
			}
			if got := gitAt(t, checked, "diff", "--cached"); got != staged {
				t.Fatalf("the checkout's index changed: %q -> %q", staged, got)
			}
			if got, has := gitFile(t, checked, "base.txt"); has != hasBase || got != baseTxt {
				t.Fatalf("the checkout's base.txt changed")
			}
			if n := countStage(k.stageRows(), "ref_moved") + countStage(k.stageRows(), "marked"); n != 0 {
				t.Fatalf("a refused batch wrote %d ref_moved/marked rows", n)
			}
		})
	}
}

// The same commit on a detached HEAD is no checkout of the branch, and a worktree that holds another branch is none of
// the batch's business: the batch integrates.
func TestCRW1595IntegrationAllowsADetachedWorktreeAndAnotherBranch(t *testing.T) {
	t.Parallel()
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	k.repo.git("branch", "dev-int", k.base)
	detached := filepath.Join(t.TempDir(), "detached")
	k.repo.git("worktree", "add", "--detach", detached, "dev-int")
	other := filepath.Join(t.TempDir(), "other branch")
	k.repo.git("worktree", "add", other, "feature-a")
	t.Cleanup(func() {
		for _, wt := range []string{detached, other} {
			_, _ = runGit(context.Background(), k.repo.path, nil, "worktree", "remove", "--force", wt)
		}
	})
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Merged) != 1 || res.Merged[0].NodeID != "a" {
		t.Fatalf("merged %+v; want a", res.Merged)
	}
}

// The branch is checked again inside the move's fenced transaction: a worktree that takes dev-int while the merged
// tree is being verified still stops the move — a late refusal, after a verification that passed.
func TestCRW1595IntegrationRechecksCheckoutInsideTheMove(t *testing.T) {
	t.Parallel()
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	k.repo.git("branch", "dev-int", k.base)
	linked := filepath.Join(t.TempDir(), "linked during verify")
	linkedAdded := false
	deps := IntegrationBatchDeps{Update: updateIntegrationRef, Verify: func(ctx context.Context, dir string, env []string) error {
		if !linkedAdded {
			linkedAdded = true
			k.repo.git("worktree", "add", linked, "dev-int")
			t.Cleanup(func() {
				_, _ = runGit(context.Background(), k.repo.path, nil, "worktree", "remove", "--force", linked)
			})
		}
		return stubVerifier(writeStubVerifier(t))(ctx, dir, env)
	}}
	_, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), deps)
	if err == nil || !strings.Contains(err.Error(), "checked out") {
		t.Fatalf("a branch taken between verification and move: %v", err)
	}
	if refusalReasonOf(err) != "disposition_conflict" {
		t.Fatalf("reason %q (err %v), want disposition_conflict", refusalReasonOf(err), err)
	}
	if tip, _ := k.branchTip("dev-int"); tip != k.base {
		t.Fatalf("the branch moved to %s under its worktree", tip)
	}
	if n := countStage(k.stageRows(), "ref_moved") + countStage(k.stageRows(), "marked"); n != 0 {
		t.Fatalf("%d ref_moved/marked rows after the refused move", n)
	}
}

// CRW-1595 (finding 2): a failed verification leaves bounded evidence — a generic journal row per failed attempt naming
// the batch, the verified head and the diagnostic (the command's output tail and the record's failed jobs and steps) —
// and the split carries the same detail to the caller, so a real failure and a flake can be told apart after the
// temporary worktree and its record are gone.
func TestCRW1595IntegrationFailureKeepsEvidence(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "bad", files: map[string]string{"bad.txt": "bad\n"}})
	k.acceptByCommit("bad")
	marker := filepath.Join(t.TempDir(), "record-path")
	script := filepath.Join(t.TempDir(), "verify.sh")
	writeExecutable(t, script, "#!/bin/sh\n"+
		"echo \"$CRW_VERIFY_RECORD\" > '"+marker+"'\n"+
		"cat > \"$CRW_VERIFY_RECORD\" <<'EOF'\n"+
		`{"schema":"verification-record/1","result":"fail","jobs":[{"name":"build","result":"fail","steps":[{"name":"go test ./pkg","result":"failed","reason":"--- FAIL: TestCRW1595Verifier (0.01s)"},{"name":"vet","result":"passed"},{"name":"docs","result":"skipped","reason":"no runner"},{"name":"na","result":"not_applicable"}]}]}`+"\n"+
		"EOF\n"+
		"echo 'verify stdout: --- FAIL: TestCRW1595Verifier'\n"+
		"echo 'verify stderr: the suite failed' >&2\n"+
		"exit 1\n")
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(),
		IntegrationBatchDeps{Verify: CommandVerifierEnv([]string{script}, func() []string { return nil }), Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Split) != 1 || res.Split[0].NodeID != "bad" || res.Split[0].Reason != "verification_failed" {
		t.Fatalf("split %+v; want bad verification_failed", res.Split)
	}
	detail := res.Split[0].Detail
	for _, want := range []string{"--- FAIL: TestCRW1595Verifier", "the suite failed", "no runner"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the split detail lacks %q: %q", want, detail)
		}
	}
	if !utf8.ValidString(detail) {
		t.Errorf("the split detail is not valid UTF-8")
	}
	// the journal holds one row per failed verification attempt, naming the batch and the verified head
	rows := k.journalRows("dag_integration_verification_failed")
	if len(rows) == 0 {
		t.Fatal("no verification failure was journaled")
	}
	for _, raw := range rows {
		var d map[string]any
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatalf("the journal detail is not JSON: %v (%s)", err, raw)
		}
		if d["batch_id"] != res.BatchID || d["plan"] != "g" || d["integration_ref"] != "dev-int" {
			t.Errorf("the journal row names %+v, want batch %s of plan g on dev-int", d, res.BatchID)
		}
		if d["verified_head"] == "" || d["attempt"] == nil {
			t.Errorf("the journal row names no verified head or attempt: %v", d)
		}
		if text, _ := d["detail"].(string); !strings.Contains(text, "TestCRW1595Verifier") {
			t.Errorf("the journal row's detail lacks the failure: %q", text)
		}
	}
	// the branch never moved and no mark was written
	if _, found := k.branchTip("dev-int"); found {
		t.Fatal("the branch was created over a failing verification")
	}
	if n := countStage(k.stageRows(), "marked") + countStage(k.stageRows(), "ref_moved"); n != 0 {
		t.Fatalf("%d marked/ref_moved rows for a batch that moved nothing", n)
	}
	// the temporary worktree and the verification files are gone on this path as on every path
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the verifier never wrote its record path: %v", err)
	}
	recordPath := strings.TrimSpace(string(raw))
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Errorf("the verification record %s still exists", recordPath)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(recordPath))); !os.IsNotExist(err) {
		t.Errorf("the temporary tree root of the verification still exists")
	}
}

// A record the judge refuses is diagnosed with the judge's reason: a record of another tree is no passing evidence and
// its refusal text lands in the split detail.
func TestCRW1595IntegrationJudgeRefusalKeepsEvidence(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	t.Setenv("CRW_TEST_WRONG_TREE", "1")
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(),
		IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Split) != 1 || res.Split[0].Reason != "verification_failed" {
		t.Fatalf("split %+v; want verification_failed", res.Split)
	}
	if !strings.Contains(res.Split[0].Detail, "names tree") {
		t.Fatalf("the split detail lacks the judge's reason: %q", res.Split[0].Detail)
	}
	if _, found := k.branchTip("dev-int"); found {
		t.Fatal("the branch was created for a tree the verifier did not verify")
	}
}

// An apparent PASS record cannot land a command that exited nonzero: the exit is the failure, and its text is the detail.
func TestCRW1595IntegrationNonzeroExitKeepsTheExitDetail(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "verify.sh")
	writeExecutable(t, script, "#!/bin/sh\nCRW_TEST_RECORD_HELPER=1\nCRW_TEST_HELPER_BIN='"+strings.ReplaceAll(bin, "'", `'\''`)+"'\nexport CRW_TEST_RECORD_HELPER CRW_TEST_HELPER_BIN\n"+
		"\"$CRW_TEST_HELPER_BIN\" -test.run='^TestVerifyRecordHelper$' -test.count=1\n"+
		"echo 'the verifier wrote its record then failed' >&2\nexit 1\n")
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(),
		IntegrationBatchDeps{Verify: CommandVerifierEnv([]string{script}, func() []string { return nil }), Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Split) != 1 || res.Split[0].Reason != "verification_failed" {
		t.Fatalf("split %+v; want verification_failed", res.Split)
	}
	if !strings.Contains(res.Split[0].Detail, "the verification command failed") || !strings.Contains(res.Split[0].Detail, "wrote its record then failed") {
		t.Fatalf("the split detail lacks the exit failure: %q", res.Split[0].Detail)
	}
	if _, found := k.branchTip("dev-int"); found {
		t.Fatal("the branch was created over a nonzero exit")
	}
}
