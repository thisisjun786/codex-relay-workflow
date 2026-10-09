package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

func TestCRW1157DenialCauses(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, cmd := range []string{`ruby -e 'puts 1'`, `"$FOO" a`, `python3 -m http.server`, `python3 -c 'open(variable,"w")'`} {
		reason := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, cmd), env))
		for _, bad := range []string{"file under the Codex memories directory", "allow-write", "ask the user"} {
			if strings.Contains(reason, bad) {
				t.Errorf("%s: misleading %q", cmd, bad)
			}
		}
		if !strings.Contains(strings.ToLower(reason), "cannot") || !strings.Contains(reason, "MEMORY-WRITE-GATE") || len(reason) > 700 {
			t.Errorf("cause/bound: %s", reason)
		}
	}
	raw := gatePayload(t, cwd, map[string]any{"agent_id": "leaf", "agent_type": "executor", "tool_name": "Write", "tool_input": map[string]any{"file_path": root + "/x"}})
	reason := gateDeny(t, HandleMemoryWriteGate(raw, env))
	if !strings.Contains(reason, "parent") || strings.Contains(reason, "ask the user") || strings.Contains(reason, "allow-write") {
		t.Errorf("leaf recovery: %s", reason)
	}
	gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
	reason = gateDeny(t, memoryGateHandle(raw, env, func(string, state.State) error { return errors.New("secret-value") }))
	if !strings.Contains(reason, "authorization") || strings.Contains(reason, "secret-value") || len(reason) > 700 {
		t.Errorf("state failure: %s", reason)
	}
	// An actual unknown memory note retains the parent grant route.
	if r := gateDeny(t, HandleMemoryWriteGate(gatePayload(t, cwd, map[string]any{"session_id": "other"}), env)); !strings.Contains(r, "allow-write") {
		t.Errorf("note recovery: %s", r)
	}
}

func TestCRW1157GithubRecovery(t *testing.T) {
	cwd, _, _ := gateScene(t)
	for _, cmd := range []string{`"$FOO" a`, `python3 -m http.server`, `$(echo ls) -l`} {
		reason := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, cwd, cmd)))
		if strings.Contains(reason, "GitHub post blocked") || strings.Contains(reason, "--body-file") || !strings.Contains(strings.ToLower(reason), "cannot") || !strings.Contains(reason, "unreadable-github-post") {
			t.Errorf("unreadable recovery: %s", reason)
		}
	}
	// A lexical cwd outside the trusted temp roots does not need a real file to be rejected.
	outside, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "clean.md"), []byte("clean"), 0600); err != nil {
		t.Fatal(err)
	}
	reason := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, outside, "gh pr comment 1 --body-file ./clean.md")))
	if !strings.Contains(reason, "TMPDIR") || !strings.Contains(reason, "outside") || len(reason) > 700 {
		t.Errorf("temp recovery: %s", reason)
	}
}

func TestCRW1157LongTargetKeepsBasename(t *testing.T) {
	target := strings.Repeat("한", 1000) + "/memories/n.md"
	reason := memoryGateReason(MemoryWriteAttempt{Surface: "edit", Target: target}, strings.Repeat("s", 200), strings.Repeat("한", 1000))
	if len(reason) > 700 || !strings.Contains(reason, "/memories/n.md)") {
		t.Error("bounded diagnosis must retain the target basename")
	}
}

func TestCRW1157UnknownBodyDirectory(t *testing.T) {
	reason := githubPostAnswerReason(t, HandleGitHubPostGuard(gateBash(t, "", "gh pr comment 1 --body-file relative.md")))
	if strings.Contains(reason, "outside") || !strings.Contains(reason, "cannot be verified") {
		t.Errorf("unknown directory was treated as known: %s", reason)
	}
}
