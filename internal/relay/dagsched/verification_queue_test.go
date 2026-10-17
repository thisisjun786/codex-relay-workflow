package dagsched_test

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

	"github.com/thisisjun786/codex-relay-workflow/internal/integrator"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// Exercise the queue's real receipt reader, stopping before any publication.
// Flow's command override must neither reject the queue's receipt nor let a
// repository-command receipt substitute for the queue's own verification.
func TestReviewFlowOverrideDoesNotChangeQueueStatusVerifier(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "dev")
	git("-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "base")
	head := git("rev-parse", "HEAD")
	keys, err := dagsched.CommitVerificationKeys(ctx, repo, head)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := dagsched.SealVerificationRecord(dagsched.VerificationRecord{
		Runner: "local", Repository: "o/r", BaseCommit: head, HeadCommit: head, TreeHash: keys.Tree,
		CiDigest: keys.CiDigest, Tools: keys.Pins, Pins: keys.Pins, Dependencies: keys.Dependencies,
		OS: runtime.GOOS, Arch: runtime.GOARCH, Result: "pass", PinMismatch: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(map[string]any{"repositories": map[string]any{repo: map[string]string{"verify_command": "true"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", cfg)
	calls := 0
	p := integrator.RequiredStatusPublisher{Context: "dev-gate", Git: func(context.Context, string, ...string) (string, error) {
		calls++
		return "", errors.New("stop before publication")
	}}
	err = p.Publish(ctx, repo, "origin", "dev", head, head, raw)
	if calls == 0 {
		t.Fatalf("queue's passing local receipt rejected before publication: %v", err)
	}
	record, err := dagsched.VerifyRepositoryCommand(ctx, repo, head, head, "true")
	if err != nil {
		t.Fatal(err)
	}
	calls = 0
	if err := p.Publish(ctx, repo, "origin", "dev", head, head, record.Raw); err == nil || calls != 0 {
		t.Fatalf("flow receipt substituted for queue verification: calls=%d err=%v", calls, err)
	}
}
