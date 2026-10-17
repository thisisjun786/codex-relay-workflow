package dagsched

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReviewCustomVerifierDoesNotNeedCIPins(t *testing.T) {
	r := newGitRepo(t)
	if err := os.MkdirAll(filepath.Join(r.path, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.write(".github/workflows/ci.yml", "name: CI\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/setup-node@v4\n        with:\n          node-version-file: .nvmrc\n")
	r.commit(".nvmrc", "22\n")
	r.git("add", ".github")
	r.git("commit", "-m", "node version file")
	head := r.git("rev-parse", "HEAD")
	ctx := context.Background()
	commandConfig(t, r.path, "true", "")
	rec, err := VerifyRepositoryCommand(ctx, r.path, head, head, "true")
	if err != nil || rec.Result != "pass" {
		t.Fatalf("repository-owned check blocked by CI pin reader: %v", err)
	}
	if rec.Schema != "verification-record/1" {
		t.Errorf("receipt schema %q", rec.Schema)
	}
	keys, err := CommitVerificationKeys(ctx, r.path, head)
	if err != nil {
		t.Fatalf("the command's keys were refused: %v", err)
	}
	if keys.Pins == nil || len(keys.Pins) != 0 {
		t.Errorf("the repository-command keys carry pins: %v", keys.Pins)
	}
	keys.Base, keys.OS, keys.Arch = head, runtime.GOOS, runtime.GOARCH
	if _, err := JudgeVerificationRecord(rec.Raw, keys); err != nil {
		t.Fatalf("the command's receipt does not judge: %v", err)
	}
	if _, err := QueueVerificationKeys(ctx, r.path, head); err == nil {
		t.Error("the queue's keys dropped the declared pins of the same commit")
	}
	commandConfig(t, r.path, "", "")
	if _, err := CommitVerificationKeys(ctx, r.path, head); err == nil {
		t.Error("without a configured command the declared pin refusal went silent")
	}
}

func TestRepositoryCommandDoesNotInterpretCIToolPins(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"a malformed toolchain":    {"go.mod": "module example.com/m\n\ngo 1.27\n\ntoolchain not-a-toolchain\n"},
		"an unreadable pin script": {"scripts/ci/secrets.sh": "scan_version=$UNREADABLE\n"},
	} {
		t.Run(name, func(t *testing.T) {
			repo, head := pinnedCommit(t, files)
			ctx := context.Background()
			commandConfig(t, repo.path, "true", "")
			rec, err := VerifyRepositoryCommand(ctx, repo.path, head, head, "true")
			if err != nil || rec.Result != "pass" {
				t.Fatalf("the command was blocked by CI pin interpretation: %v", err)
			}
			if rec.TreeHash == "" || rec.TreeHash != repo.git("rev-parse", head+"^{tree}") {
				t.Errorf("the receipt's tree %q", rec.TreeHash)
			}
			if len(rec.Dependencies) == 0 {
				t.Error("the receipt dropped the commit's dependency digests")
			}
			keys, err := CommitVerificationKeys(ctx, repo.path, head)
			if err != nil {
				t.Fatalf("the command's keys: %v", err)
			}
			keys.Base, keys.OS, keys.Arch = head, runtime.GOOS, runtime.GOARCH
			if _, err := JudgeVerificationRecord(rec.Raw, keys); err != nil {
				t.Fatalf("the receipt does not judge: %v", err)
			}
			if _, err := QueueVerificationKeys(ctx, repo.path, head); err == nil {
				t.Error("the queue skipped the same commit's pin refusal")
			}
			commandConfig(t, repo.path, "", "true")
			if _, err := CommitVerificationKeys(ctx, repo.path, head); err == nil {
				t.Error("an integrate_command-only configuration dropped pin enforcement")
			}
		})
	}
}
