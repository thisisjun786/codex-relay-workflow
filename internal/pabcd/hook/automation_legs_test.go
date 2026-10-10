package hook_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// automationLegPayload is one PreToolUse automation_update call.
func automationLegPayload(cwd, session, tool, input string) harness.Call {
	return harness.Call{Raw: `{"hook_event_name":"PreToolUse","session_id":"` + session +
		`","cwd":"` + cwd + `","tool_name":"` + tool + `","tool_input":` + input + `}`}
}

// The automation ownership leg is wired to the port: the row keeps its Guard registration, the
// caller's own stored heartbeat passes in silence, and a foreign or missing store denies through
// the harness envelope. The leg reads the process environment, so the test points HOME, CODEX_HOME
// and CRW_HOME at temporary directories and proves the leg wrote nothing into them (the real homes are not
// observed: the host's own Codex sessions write there, CRW-1170).
func TestAutomationOwnershipLegAnswersThroughTheEnvelope(t *testing.T) {
	homes := testsupport.SandboxAccountHomes(t)
	dir := t.TempDir()
	cwd := filepath.Join(dir, "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(homes.Codex, "automations", "heartbeat-one")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "version = 1\nid = \"heartbeat-one\"\nkind = \"heartbeat\"\ntarget_thread_id = \"s1\"\n"
	if err := os.WriteFile(filepath.Join(store, "automation.toml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	homes.Rebase()

	leg := worktreeLeg(t, "pre-tool-use-guarding-automation-ownership")
	if leg.Stage != harness.Guard || leg.Recover || leg.Slug != "pre-tool-use-automation-ownership" {
		t.Errorf("the leg's registration changed: %+v", leg)
	}

	own := automationLegPayload(cwd, "s1", "mcp__codex_app__automation_update", `{"mode":"delete","id":"heartbeat-one"}`)
	if got := leg.Handle(own); got != "" {
		t.Errorf("the caller's own heartbeat is answered: %q", got)
	}

	foreign := automationLegPayload(cwd, "s2", "mcp__codex_app__automation_update", `{"mode":"delete","id":"heartbeat-one"}`)
	out := leg.Handle(foreign)
	if !strings.HasPrefix(out, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny"`) || !strings.HasSuffix(out, "\n") || !strings.Contains(out, "AUTOMATION-OWNERSHIP-01") {
		t.Errorf("a foreign delete: %q", out)
	}

	var envelope struct {
		Specific struct {
			Reason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("the answer is not the envelope: %v", err)
	}
	if envelope.Specific.Reason != "AUTOMATION-OWNERSHIP-01: Automation ownership is missing, unsupported, conflicting, or belongs to another task." {
		t.Errorf("the reason is not the oracle's: %q", envelope.Specific.Reason)
	}

	missing := automationLegPayload(cwd, "s1", "mcp__codex_app__automation_update", `{"mode":"delete","id":"absent"}`)
	if got := leg.Handle(missing); !strings.Contains(got, "Cannot verify automation ownership") {
		t.Errorf("a store the leg cannot read: %q", got)
	}

	if got := leg.Handle(automationLegPayload(cwd, "s1", "Bash", `{"command":"ls"}`)); got != "" {
		t.Errorf("an unrelated tool: %q", got)
	}
}
