package hook

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestCRW1157CorrectionExecutionCause: a refusal carries the cause of the execution that refused. A named pipe as a body file is a
// post the guard cannot verify, whether the command or a script it runs names it. A script's post of a body file is judged by the
// file's content, not by where it lies (CRW-1578): a clean committed body is allowed, and a secret-shaped one is refused by the scan.
func TestCRW1157CorrectionExecutionCause(t *testing.T) {
	cwd, _, _ := gateScene(t)
	fifo := filepath.Join(cwd, "b.md")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	scripts := filepath.Join(filepath.Dir(cwd), "scripts")
	if err := os.MkdirAll(scripts, 0700); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"cd " + cwd + "; gh pr comment 1 --body-file ./b.md", "sh fifo-post.sh"} {
		if err := os.WriteFile(filepath.Join(scripts, "fifo-post.sh"), []byte("gh pr comment 1 --body-file "+fifo), 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(filepath.Join(scripts, "fifo-post.sh")) })
		reason := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, scripts, cmd)))
		if strings.Contains(reason, "outside") || strings.Contains(reason, "posting has not been established") || !strings.Contains(reason, "cannot be verified") {
			t.Errorf("wrong execution cause: %s", reason)
		}
	}
	// A script's explicit post of a clean body is allowed wherever the body lies; a secret-shaped body is the scan's refusal.
	clean, err := filepath.Abs(filepath.Join("testdata", "body", "clean.md"))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(cwd, "clean-post.sh")
	if err := os.WriteFile(script, []byte("gh pr comment 1 --body-file "+clean), 0600); err != nil {
		t.Fatal(err)
	}
	if reason := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, "sh "+script))); reason != "" {
		t.Errorf("a script's post of a clean body was refused: %s", reason)
	}
	secret := filepath.Join(cwd, "secret.md")
	if err := os.WriteFile(secret, []byte("GH_TOKEN="+strings.Repeat("a", 20)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script = filepath.Join(cwd, "secret-post.sh")
	if err := os.WriteFile(script, []byte("gh pr comment 1 --body-file "+secret), 0600); err != nil {
		t.Fatal(err)
	}
	reason := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, "sh "+script)))
	if !strings.Contains(reason, "("+githubPostRuleSecret+")") || strings.Contains(reason, strings.Repeat("a", 20)) {
		t.Errorf("a script's post of a secret-shaped body is not the scan's refusal: %s", reason)
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
