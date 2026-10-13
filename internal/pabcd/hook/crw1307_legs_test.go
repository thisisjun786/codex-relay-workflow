package hook_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1307: through the real `crw hook pre-tool-use --leg <leg>` entry, each harness PreToolUse leg's refusal passes with the
// refusal as a warning the session sees and leaves a decision record (leg, rule, decision, session; no command text), and the
// managed-worktree deletion guard keeps its deny for a resolved deletion.
func TestCRW1307HarnessLegsWarnAndRecord(t *testing.T) {
	h := gateEnv(t)
	codexHome := os.Getenv("CODEX_HOME")
	t.Setenv("PLUGIN_ROOT", t.TempDir()) // decision records are written inside a plugin only
	bad := state.StatePath(h.cwd, "gate-bad")
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(codexHome, "worktrees", "zk3q", "repo")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".git"), []byte("gitdir: /fake/main/.git/worktrees/zk3q\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(codexHome, "memories"), 0o755); err != nil {
		t.Fatal(err)
	}
	const marker = "sekrit-marker"
	bash := func(cwd, command string) string {
		return gatePayload(t, cwd, "gate-w", "Bash", `{"command":`+jsonString(command)+`}`)
	}
	for _, c := range []struct {
		leg, raw, warning, rule, decision string
	}{
		{"pre-tool-use-guarding-interview-in-goal", gatePayload(t, h.cwd, "gate-2", "request_user_input", `{"questions":[]}`), "goal-active=unreadable", harness.RuleInterviewInGoal, harness.DecisionWarn},
		{"pre-tool-use-guarding-goal-complete", gatePayload(t, h.cwd, "gate-bad", "update_goal", `{"status":"complete"}`), "state is unreadable", "goal-complete-gate-01", harness.DecisionWarn},
		{"pre-tool-use-guarding-automation-ownership", gatePayload(t, h.cwd, "gate-4", "automation_update", `{"mode":"delete","id":"`+marker+`"}`), "Cannot verify automation ownership", "", harness.DecisionWarn},
		{"pre-tool-use-linting-apply-patch", gatePayload(t, h.cwd, "gate-5", "apply_patch", `{"command":"+debugger; // `+marker+`"}`), "[crw comment-lint]", "comment-lint", harness.DecisionWarn}, // justified: lint fixture
		{"pre-tool-use-guarding-memory-write", bash(h.cwd, "echo "+marker+" > "+filepath.Join(codexHome, "memories", "n.md")), "MEMORY-WRITE-GATE", "", harness.DecisionWarn},
		{"pre-tool-use-guarding-managed-worktree-deletion", bash(checkout, "rmdir "+checkout+" # "+marker), "WORKTREE-GUARD-03", "worktree-guard-03", harness.DecisionWarn},
		{"pre-tool-use-guarding-managed-worktree-deletion", bash(checkout, "python3 -c 'import shutil; shutil.rmtree(\"x\")' # "+marker), "WORKTREE-GUARD-03", "worktree-guard-03", harness.DecisionWarn},
		// goal-budget refuses nothing it reads (CRW-1133); its one refusal was an unreadable input, which is released.
		{"pre-tool-use-guarding-goal-budget", strings.Repeat(" ", harness.MaxStdinBytes+1), "could not be read", harness.RuleUnreadableInput, harness.DecisionRelease},
	} {
		t.Run(c.leg+"/"+c.rule, func(t *testing.T) {
			of := func() (out []map[string]any) {
				for _, r := range harness.ReadDecisions(codexHome) {
					if r["leg"] == c.leg {
						out = append(out, r)
					}
				}
				return out
			}
			before := len(of())
			out, code := gateRun(t, c.leg, c.raw)
			var a struct {
				Output map[string]any `json:"hookSpecificOutput"`
			}
			if code != 0 || json.Unmarshal([]byte(out), &a) != nil || a.Output["permissionDecision"] != nil {
				t.Fatalf("exit %d answer %q: not a pass", code, out)
			}
			ctx, _ := a.Output["additionalContext"].(string)
			if !strings.HasPrefix(ctx, harness.WarningPrefix) || !strings.Contains(ctx, c.warning) {
				t.Fatalf("warning %q, want %q", ctx, c.warning)
			}
			d := of()
			if len(d) != before+1 {
				t.Fatalf("records %v", d)
			}
			last := d[len(d)-1]
			if last["leg"] != c.leg || last["decision"] != c.decision || c.rule != "" && last["rule"] != c.rule || last["rule"] == "" {
				t.Fatalf("record %v", last)
			}
		})
	}
	// The resolved deletion stays a deny, and its block is recorded.
	out, code := gateRun(t, "pre-tool-use-guarding-managed-worktree-deletion", bash(checkout, "rm -rf "+checkout+" # "+marker))
	if code != 0 || !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, "WORKTREE-GUARD-03") {
		t.Fatalf("resolved deletion: %d %q", code, out)
	}
	blocks := 0
	for _, r := range harness.ReadDecisions(codexHome) {
		if r["decision"] == harness.DecisionBlock {
			if blocks++; r["rule"] != harness.RuleResolvedDeletion || r["leg"] != "pre-tool-use-guarding-managed-worktree-deletion" || r["sessionId"] != "gate-w" {
				t.Fatalf("block record %v", r)
			}
		}
	}
	if blocks != 1 {
		t.Fatalf("%d block records", blocks)
	}
	// No record holds the command text.
	_ = filepath.WalkDir(filepath.Join(codexHome, "crw", "hook-decisions"), func(p string, e os.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), marker) {
				t.Errorf("%s holds the command text: %s", p, b)
			}
		}
		return nil
	})
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
