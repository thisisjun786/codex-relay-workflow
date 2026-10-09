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
	"testing"

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
		TreeHash: tree, CiDigest: keys.CiDigest, Tools: map[string]string{"go": "go1.27.1"}, Pins: map[string]string{}, GoFlags: "", GoEnv: "",
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
	t.Setenv("CRW_TEST_RECORD_HELPER", "1")
	t.Setenv("CRW_TEST_HELPER_BIN", bin)
	script := filepath.Join(t.TempDir(), "verify.sh")
	body := "#!/bin/sh\nif [ -e bad.txt ]; then\n  echo 'bad.txt is present: the merged tree fails' >&2\n  exit 1\nfi\nexec \"$CRW_TEST_HELPER_BIN\" -test.run='^TestVerifyRecordHelper$' -test.count=1\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
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
		TreeHash: strings.TrimSpace(tree), CiDigest: keys.CiDigest, Tools: map[string]string{"go": "go1.27.1"}, Pins: map[string]string{},
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
