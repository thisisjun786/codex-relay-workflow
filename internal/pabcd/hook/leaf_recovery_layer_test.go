package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
)

// TestLeafRecoveryNamesNoSpawnerLayer (CRW-1478, CRW-1448 crosscheck): a refusal read by a leaf tells it to report to the session
// that spawned it, and names no layer. The hooks decide a leaf from the payload alone (it carries agent_id or agent_type) and never
// read who spawned it: a standalone session and a flow engine thread (internal/role/spawn/flow_bound.go) spawn subagents too, so a
// leaf whose spawner is not the 자식 must not be told 자식. The authorization-state refusal, which can run in any session, carries
// no bare role word either.
func TestLeafRecoveryNamesNoSpawnerLayer(t *testing.T) {
	const spawner = "the session that spawned you"
	cwd, root, env := gateScene(t)
	if err := os.WriteFile(filepath.Join(cwd, "hidden.pyc"), make([]byte, 32), 0o644); err != nil {
		t.Fatal(err)
	}
	// An explorer a flow engine thread spawned, and a helper a standalone session spawned: neither spawner is the 자식.
	spawned := map[string]map[string]any{
		"an engine thread's explorer":   {"agent_id": "explorer-1", "agent_type": "explorer"},
		"a standalone session's helper": {"agent_id": "helper-1"},
	}
	unread := githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, post: true}
	for who, ids := range spawned {
		payload := func(cwd string, over map[string]any) string {
			merged := map[string]any{}
			for k, v := range ids {
				merged[k] = v
			}
			for k, v := range over {
				merged[k] = v
			}
			return gatePayload(t, cwd, merged)
		}
		bash := map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "python3 -m unittest"}}
		texts := map[string]string{
			"the memory gate (protected write)": gateDeny(t, HandleMemoryWriteGate(payload(cwd, map[string]any{"tool_name": "Write",
				"tool_input": map[string]any{"file_path": root + "/x"}}), env)),
			"the memory gate (unknown destination)": gateDeny(t, HandleMemoryWriteGate(payload("", map[string]any{"tool_name": "Write",
				"tool_input": map[string]any{"file_path": "relative-note.md"}}), env)),
			"the memory gate's command reader":    gateDeny(t, HandleMemoryWriteGate(payload(cwd, bash), env)),
			"the GitHub guard's command reader":   githubPostAnswerReason(t, HandleGitHubPostGuard(payload(cwd, bash))),
			"the GitHub post guard (unread rule)": gateDeny(t, githubPostDenyPayload(unread, ids)),
		}
		for what, text := range texts {
			if !strings.Contains(text, spawner) {
				t.Errorf("%s, %s: does not name %q: %q", who, what, spawner, text)
			}
			for _, layer := range []string{"총괄", "부모", "자식", "서브에이전트"} {
				if strings.Contains(text, layer) {
					t.Errorf("%s, %s: names the spawner's layer (%s), which the hook does not know: %q", who, what, layer, text)
				}
			}
			if hits := layername.Find(text); len(hits) != 0 {
				t.Errorf("%s, %s: bare role words %v in %q", who, what, hits, text)
			}
		}
		if text := texts["the memory gate (protected write)"]; !strings.Contains(text, "; that session must resolve authorization.") {
			t.Errorf("%s: the protected-write refusal does not leave authorization to the session that spawned it: %q", who, text)
		}
	}
	for _, leaf := range []bool{false, true} {
		text := memoryGateReasonFor(MemoryWriteAttempt{Surface: "tool", Cause: "authorization-state", Target: "note.md"}, "s1", "/tmp", leaf)
		if hits := layername.Find(text); len(hits) != 0 {
			t.Errorf("the authorization-state refusal (leaf %v): bare role words %v in %q", leaf, hits, text)
		}
		if !strings.Contains(text, "Report this state failure to the session above this one or to the session owner; retry after state repair.") {
			t.Errorf("the authorization-state refusal (leaf %v) lost its recovery: %q", leaf, text)
		}
	}
}
