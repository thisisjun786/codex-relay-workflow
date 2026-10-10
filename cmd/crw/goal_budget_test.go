package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1133. The installed create_goal leg (crw hook pre-tool-use --leg pre-tool-use-guarding-goal-budget,
// behind a switch that says crw) no longer refuses a create_goal for naming anything beside objective: the
// fields and their values belong to the host's tool contract, so a user's token_budget reaches it. The leg
// answers nothing for a create_goal, whatever the input holds.
func TestInstalledCreateGoalLegPassesTheUsersBudgetToTheHost(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CODEX_SQLITE_HOME", filepath.Join(home, "sqlite"))
	t.Setenv("PLUGIN_ROOT", "")
	switchOn(t, codexHome)
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	stdin := os.Stdin
	t.Cleanup(func() { os.Stdin = stdin })

	for _, c := range []struct {
		name  string
		input string
	}{
		{"objective only", `{"objective":"ship the feature"}`},
		{"a positive token_budget", `{"objective":"ship the feature","token_budget":200000}`},
		{"a token_budget the host will refuse", `{"objective":"ship the feature","token_budget":-5}`},
		{"a token_budget that is not a number", `{"objective":"ship the feature","token_budget":"lots"}`},
		{"a field no host supports", `{"objective":"ship the feature","priority":"urgent"}`},
		{"no objective at all", `{"token_budget":1000}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "session_id": "budget-s1", "cwd": work,
				"tool_name": "create_goal", "tool_input": json.RawMessage(c.input)})
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(home, "payload.json")
			if err := os.WriteFile(file, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(file)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			os.Stdin = f
			var out, errOut strings.Builder
			code := run(context.Background(), "crw", []string{"hook", "pre-tool-use", "--leg", "pre-tool-use-guarding-goal-budget"}, &out, &errOut)
			if code != 0 || out.Len() != 0 || errOut.Len() != 0 {
				t.Fatalf("the create_goal leg answered exit %d, stdout %q, stderr %q; want silence (the host validates the fields)", code, out.String(), errOut.String())
			}
		})
	}
}
