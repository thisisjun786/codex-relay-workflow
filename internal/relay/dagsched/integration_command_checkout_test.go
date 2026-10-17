package dagsched

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func commandConfigMulti(t *testing.T, entries map[string]map[string]string) {
	t.Helper()
	repos := map[string]any{}
	for repo, entry := range entries {
		repos[repo] = entry
	}
	raw, err := json.Marshal(map[string]any{"repositories": repos})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", path)
}

func linkedCheckout(t *testing.T, k *batchKit, nodes ...string) string {
	t.Helper()
	linked := filepath.Join(t.TempDir(), "linked")
	k.repo.git("worktree", "add", "--detach", linked, k.base)
	main := k.repo.path
	k.repo.path = linked
	for _, node := range nodes {
		k.acceptByCommit(node)
	}
	k.repo.path = main
	return linked
}

func assertCommandReceipt(t *testing.T, k *batchKit, res IntegrationBatchResult, repo, command, stdout string, tip string) {
	t.Helper()
	rec := res.Verification
	if rec.CommandVerification == nil {
		t.Fatalf("the receipt has no command evidence: %+v", rec)
	}
	if rec.CommandVerification.Command != command || *rec.CommandVerification.ExitCode != 0 {
		t.Fatalf("the receipt ran %q exit %v, want %q exit 0", rec.CommandVerification.Command, *rec.CommandVerification.ExitCode, command)
	}
	if rec.CommandVerification.LogDigest != sha256Digest([]byte(stdout)) {
		t.Fatalf("the receipt's log digest %q, want the %q output's", rec.CommandVerification.LogDigest, stdout)
	}
	if rec.Repository != repo || rec.HeadCommit != tip {
		t.Fatalf("the receipt names %q %q, want %q %q", rec.Repository, rec.HeadCommit, repo, tip)
	}
	if rec.TreeHash != k.repo.git("rev-parse", tip+"^{tree}") {
		t.Fatalf("the receipt's tree %q is not %s's", rec.TreeHash, tip)
	}
}

func TestReviewCheckoutOverrideSurvivesBatchWorktree(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a"}})
	linked := linkedCheckout(t, k, "a")
	commandConfigMulti(t, map[string]map[string]string{
		k.repo.path: {"verify_command": "exit 23"},
		linked:      {"verify_command": "printf linked"},
	})
	in := k.batchIn()
	in.Checkout = linked
	res, err := k.sched.IntegrateBatch(context.Background(), in, IntegrationDeps(stubVerifier(writeStubVerifier(t))))
	if err != nil || len(res.Merged) != 1 {
		t.Fatalf("exact-path override failed: %+v %v", res, err)
	}
	assertCommandReceipt(t, k, res, linked, "printf linked", "linked", res.NewHead)
}

func TestReviewCheckoutOverrideSurvivesBatchWorktreeIntegrate(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a"}})
	linked := linkedCheckout(t, k, "a")
	script := filepath.Join(t.TempDir(), "integrate.sh")
	body := "set -eu\ngit -C '" + k.repo.path + "' merge --no-ff --no-edit -m command-merge \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	commandConfigMulti(t, map[string]map[string]string{
		k.repo.path: {"verify_command": "exit 23"},
		linked:      {"verify_command": "printf linked", "integrate_command": "sh '" + script + "'"},
	})
	in := k.batchIn()
	in.Checkout = linked
	in.IntegrationRef = "dev"
	res, err := k.sched.IntegrateBatch(context.Background(), in, IntegrationDeps(stubVerifier(writeStubVerifier(t))))
	if err != nil || len(res.Merged) != 1 {
		t.Fatalf("integrate-command override failed: %+v %v", res, err)
	}
	assertCommandReceipt(t, k, res, linked, "printf linked", "linked", res.NewHead)
	if got := k.repo.git("rev-parse", "dev"); got != res.NewHead {
		t.Fatalf("dev = %s, want the command's merge %s", got, res.NewHead)
	}
}

func TestRepositoryIntegrationVerifierStillRejectsChangedCommand(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a"}})
	commandConfig(t, k.repo.path, "printf before", "")
	verifier := repositoryIntegrationVerifier(k.repo.path, "printf before")
	commandConfig(t, k.repo.path, "printf after", "")
	recordPath := filepath.Join(t.TempDir(), "record.json")
	err := verifier(context.Background(), "", []string{"CRW_VERIFY_HEAD=" + k.base, "CRW_VERIFY_BASE=" + k.base, "CRW_VERIFY_RECORD=" + recordPath})
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("a changed configured command was not refused: %v", err)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Errorf("a refused command wrote a receipt: %v", err)
	}
}
