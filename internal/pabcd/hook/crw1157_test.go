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
		// A command the reader cannot read observed no protected write, so it carries the reader's name and cause (CRW-1178);
		// a destination the gate cannot verify keeps the gate's.
		wantName := "[crw command-reader]"
		if strings.Contains(reason, "unknown-destination") {
			wantName = "MEMORY-WRITE-GATE"
		}
		if !strings.Contains(strings.ToLower(reason), "cannot") || !strings.Contains(reason, wantName) || len(reason) > 700 {
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
	// A confirmed memory note keeps the parent grant route.
	if r := gateDeny(t, HandleMemoryWriteGate(gatePayload(t, cwd, map[string]any{"session_id": "other"}), env)); !strings.Contains(r, "allow-write") {
		t.Errorf("note recovery: %s", r)
	}
}

// An actual write request (an edit tool call) whose destination cannot be resolved keeps the parent grant route while saying the
// cause is unknown-destination; a leaf is sent to its parent, and a general shell command never receives the grant route.
func TestCRW1157UnknownDestinationEditKeepsGrantRoute(t *testing.T) {
	_, _, env := gateScene(t)
	// The grant is offered only where the gate reads the state: an absolute working directory (here one that is not a directory, so
	// a relative path has no known destination) and a session id. With no cwd the route is not offered (CRW-1178).
	missing := filepath.Join(t.TempDir(), "missing")
	unknown := gatePayload(t, missing, map[string]any{"tool_name": "Write", "tool_input": map[string]any{"file_path": "relative-note.md", "content": "x"}})
	reason := gateDeny(t, HandleMemoryWriteGate(unknown, env))
	for _, want := range []string{"MEMORY-WRITE-GATE", "unknown-destination", "allow-write --session " + gateSession, "permits one write"} {
		if !strings.Contains(reason, want) {
			t.Errorf("unknown edit destination lacks %q: %s", want, reason)
		}
	}
	if strings.Contains(reason, "under the Codex memories directory") || len(reason) > 700 {
		t.Errorf("unknown edit destination claims a confirmed protected write or exceeds the bound: %s", reason)
	}
	leaf := gatePayload(t, "", map[string]any{"agent_id": "leaf", "agent_type": "executor", "tool_name": "Write", "tool_input": map[string]any{"file_path": "relative-note.md"}})
	reason = gateDeny(t, HandleMemoryWriteGate(leaf, env))
	if !strings.Contains(reason, "unknown-destination") || !strings.Contains(reason, "parent") || strings.Contains(reason, "allow-write") || strings.Contains(reason, "ask the user") {
		t.Errorf("leaf unknown edit destination: %s", reason)
	}
	cwd, _, _ := gateScene(t)
	reason = gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, `python3 -c 'open(variable,"w")'`), env))
	if strings.Contains(reason, "allow-write") || !strings.Contains(reason, "unknown-destination") {
		t.Errorf("general command unknown destination: %s", reason)
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
	// A lexical cwd outside the trusted temp roots does not need a real file to be rejected; the directory is fixed so the
	// test does not depend on where the checkout or TMPDIR lives.
	outside := "/"
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
