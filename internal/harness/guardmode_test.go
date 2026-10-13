package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secretCommand = "rm -rf /tmp/sekrit-command-text"

func denyOf(reason string) string {
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "permissionDecision": "deny",
		"permissionDecisionReason": reason, "additionalContext": reason}})
	return string(b) + "\n"
}

// warning is the warning of a warn answer, or fails the test when the answer is not one.
func warning(t *testing.T, answer string) string {
	t.Helper()
	var a struct {
		Output map[string]any `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(answer), &a) != nil || a.Output["hookEventName"] != "PreToolUse" || a.Output["permissionDecision"] != nil ||
		len(a.Output) != 2 {
		t.Fatalf("not a warn answer: %q", answer)
	}
	ctx, _ := a.Output["additionalContext"].(string)
	if !strings.HasPrefix(ctx, WarningPrefix) {
		t.Fatalf("warning without its prefix: %q", answer)
	}
	return strings.TrimPrefix(ctx, WarningPrefix)
}

func TestGuardModeTableKeepsOnlyTheIrreversibleRulesAtBlock(t *testing.T) {
	for _, c := range []struct {
		leg, rule string
		want      GuardMode
	}{
		{"pre-tool-use-guarding-child-push", RuleSharedBranchHistory, GuardBlock},
		{"pre-tool-use-guarding-github-post", RuleSecretInGitHubText, GuardBlock},
		{"pre-tool-use-guarding-managed-worktree-deletion", RuleResolvedDeletion, GuardBlock},
		{"pre-tool-use-guarding-child-push", "", GuardWarn},
		{"pre-tool-use-guarding-github-post", "inline-github-body", GuardWarn},
		{"pre-tool-use-guarding-github-post", "unreadable-github-post", GuardWarn},
		{"pre-tool-use-guarding-managed-worktree-deletion", "worktree-guard-03", GuardWarn},
		// A block rule of one leg is not a block rule of another.
		{"pre-tool-use-guarding-memory-write", RuleResolvedDeletion, GuardWarn},
		{"pre-tool-use-guarding-automation-ownership", RuleSecretInGitHubText, GuardWarn},
		{"pre-tool-use-attaching-skills", RuleSharedBranchHistory, GuardWarn},
		{"pre-tool-use-guarding-goal-complete", "goal-complete-gate-01", GuardWarn},
	} {
		if got := Mode(c.leg, c.rule); got != c.want {
			t.Errorf("%s/%s: %v, want %v", c.leg, c.rule, got, c.want)
		}
	}
}

func TestSettleGuardWarnsAndRecordsARefusalTheTableDoesNotKeep(t *testing.T) {
	env, home, _ := hookEnv(t)
	raw := `{"hook_event_name":"PreToolUse","session_id":"s1","agent_id":"a1","tool_name":"Bash","tool_input":{"command":"` + secretCommand + `"}}`
	reason := "[crw: WORKTREE-GUARD-03] cannot analyze this command (analysis-unavailable)."
	got := SettleGuard("pre-tool-use-guarding-managed-worktree-deletion", raw, denyOf(reason), "", lookup(env))
	if w := warning(t, got); w != reason {
		t.Fatalf("warning %q, want the refusal %q", w, reason)
	}
	d := ReadDecisions(home)
	if len(d) != 1 || d[0]["leg"] != "pre-tool-use-guarding-managed-worktree-deletion" || d[0]["rule"] != "worktree-guard-03" ||
		d[0]["decision"] != DecisionWarn || d[0]["sessionId"] != "s1" || d[0]["agentId"] != "a1" || d[0]["decidedAt"] == nil {
		t.Fatalf("records: %v", d)
	}
	assertNoCommandText(t, home)
}

func TestSettleGuardKeepsABlockRuleAndRecordsIt(t *testing.T) {
	env, home, _ := hookEnv(t)
	raw := `{"hook_event_name":"PreToolUse","session_id":"s1","tool_name":"Bash","tool_input":{"command":"` + secretCommand + `"}}`
	deny := denyOf("[crw: WORKTREE-GUARD-03] blocked `rm -r .`: it deletes this session's own worktree")
	if got := SettleGuard("pre-tool-use-guarding-managed-worktree-deletion", raw, deny, RuleResolvedDeletion, lookup(env)); got != deny {
		t.Fatalf("block rule answered %q", got)
	}
	if d := ReadDecisions(home); len(d) != 1 || d[0]["rule"] != RuleResolvedDeletion || d[0]["decision"] != DecisionBlock {
		t.Fatalf("records: %v", d)
	}
	// The same rule named by a leg the table does not keep it for warns.
	if got := SettleGuard("pre-tool-use-guarding-memory-write", raw, deny, RuleResolvedDeletion, lookup(env)); got == deny {
		t.Fatalf("a block rule of another leg blocked")
	}
	assertNoCommandText(t, home)
}

func TestSettleGuardLeavesAnswersThatAreNoRefusal(t *testing.T) {
	env, home, _ := hookEnv(t)
	for _, answer := range []string{"", "\n", "not json",
		`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"message":"x"}}}` + "\n",
		`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","additionalContext":"[crw IDLE-EDIT] ..."}}` + "\n"} {
		if got := SettleGuard("pre-tool-use-linting-apply-patch", "{}", answer, "", lookup(env)); got != answer {
			t.Errorf("%q became %q", answer, got)
		}
	}
	if d := ReadDecisions(home); len(d) != 0 {
		t.Fatalf("records: %v", d)
	}
}

func TestRuleOfNamesTheRuleNeverTheCommand(t *testing.T) {
	for reason, want := range map[string]string{
		"GitHub post blocked (inline-github-body) at command: check the text":                    "inline-github-body",
		"GOAL-COMPLETE-GATE-01: cannot complete":                                                 "goal-complete-gate-01",
		"[crw comment-lint] `debugger` statement (line: debugger;)":                              "comment-lint",
		"[crw CHILD-PUSH-GUARD] 통합은 부모·통합기 몫: Refused: git push origin HEAD:dev does not push x": "child-push-guard",
		"LEAF-TOPOLOGY-01: a sub-agent cannot spawn":                                             "leaf-topology-01",
		"rm -rf /": "refusal",
		"[crw comment-lint] `as any` cast (line: const X-RAY-01 = y as any)": "comment-lint",
		strings.Repeat("x", 70) + " (quoted-command-word)":                   "refusal",
		"": "refusal",
	} {
		if got := RuleOf(reason); got != want {
			t.Errorf("%q: %q, want %q", reason, got, want)
		}
	}
}

func TestRecordDecisionNeedsAPlugin(t *testing.T) {
	env, home, _ := hookEnv(t)
	delete(env, "PLUGIN_ROOT")
	if RecordDecision(`{"session_id":"s1"}`, "pre-tool-use-guarding-memory-write", "memory-write-gate-01", DecisionWarn, lookup(env)) {
		t.Fatal("recorded outside a plugin")
	}
	if _, err := os.Stat(filepath.Join(home, "crw", "hook-decisions")); !os.IsNotExist(err) {
		t.Fatalf("decision directory: %v", err)
	}
}

func assertNoCommandText(t *testing.T, home string) {
	t.Helper()
	_ = filepath.WalkDir(filepath.Join(home, "crw"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), "sekrit") {
				t.Errorf("%s holds the command text: %s", p, b)
			}
		}
		return nil
	})
}
