package hook

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
)

// TestLeafRecoveryNamesTheChild (CRW-1478): a refusal read by a leaf (the payload carries agent_id or agent_type, so the reader is
// a subagent the 자식 spawned) tells it to report to the 자식 that spawned it, and carries no bare role word; the authorization-state
// refusal, which can run in any session, carries none either.
func TestLeafRecoveryNamesTheChild(t *testing.T) {
	const spawner = "the 자식 that spawned you"
	unread := githubPostSite{rule: githubPostRuleUnread, place: githubPostWhereCommand, post: true}
	leafs := map[string]string{
		"the GitHub post guard (unread rule)":   gateDeny(t, githubPostDenyPayload(unread, map[string]any{"agent_id": "a1"})),
		"the memory gate (unknown destination)": memoryGateReasonFor(MemoryWriteAttempt{Surface: "shell", Cause: "unknown-destination", Target: "x"}, "s1", "/tmp", true),
		"the memory gate (protected write)":     memoryGateReasonFor(MemoryWriteAttempt{Surface: "tool", Target: "note.md"}, "s1", "/tmp", true),
		"the command reader":                    commandUnreadableReason("unreadable-program", "a reason", true, ""),
	}
	for what, text := range leafs {
		if !strings.Contains(text, spawner) {
			t.Errorf("%s does not name %q: %q", what, spawner, text)
		}
		if hits := layername.Find(text); len(hits) != 0 {
			t.Errorf("%s: bare role words %v in %q", what, hits, text)
		}
	}
	if text := leafs["the memory gate (protected write)"]; !strings.Contains(text, "the 자식 must resolve authorization.") {
		t.Errorf("the protected-write refusal does not leave authorization to the 자식: %q", text)
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
