package hook

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCRW1157CorrectionExecutionCause(t *testing.T) {
	cwd, _, _ := gateScene(t)
	t.Setenv("TMPDIR", cwd)
	fifo := filepath.Join(cwd, "b.md")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(cwd), "outside")
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"cd " + cwd + "; gh pr comment 1 --body-file ./b.md", "sh fifo-post.sh"} {
		if err := os.WriteFile(filepath.Join(outside, "fifo-post.sh"), []byte("gh pr comment 1 --body-file "+fifo), 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(filepath.Join(outside, "fifo-post.sh")) })
		reason := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, outside, cmd)))
		if strings.Contains(reason, "outside") || strings.Contains(reason, "posting has not been established") || !strings.Contains(reason, "cannot be verified") {
			t.Errorf("wrong execution cause: %s", reason)
		}
	}
	// A script's explicit post and out-of-root body carry their actual refusal.
	script := filepath.Join(cwd, "outside-post.sh")
	if err := os.WriteFile(script, []byte("gh pr comment 1 --body-file "+filepath.Join(outside, "body.md")), 0600); err != nil {
		t.Fatal(err)
	}
	reason := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, "sh "+script)))
	if !strings.Contains(reason, "outside") || !strings.Contains(reason, "TMPDIR") {
		t.Errorf("lost carried post cause: %s", reason)
	}
}

func TestCRW1157CorrectionRecoveryDoesNotPromiseReceipt(t *testing.T) {
	cwd, _, env := gateScene(t)
	raw := gateBash(t, cwd, "python3 -m http.server")
	for _, reason := range []string{gateDeny(t, HandleMemoryWriteGate(raw, env)), githubPostAnswerReason(t, HandleGitHubPostGuard(raw))} {
		if strings.Contains(reason, "receipt test --") {
			t.Errorf("invalid unconditional receipt route: %s", reason)
		}
		if !strings.Contains(reason, "readable script") {
			t.Errorf("no applicable recovery: %s", reason)
		}
	}
}
