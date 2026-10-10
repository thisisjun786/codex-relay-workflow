package spawn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1119: a recognized helper spawn whose routing cannot be decided because the role store is unusable is denied with the store's
// error, where the oracle's silent catch allowed it on the main model. Other roles and other hook events are untouched.
func TestSpawnHookDeniesARoleWhoseSettingsAreUnusable(t *testing.T) {
	spawn := func(ws, agent, message string) string {
		return `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"s-1","cwd":"` + ws + `","tool_input":{"agent_type":"` + agent + `","message":` + spawnHookQuote(message) + `}}`
	}
	for _, c := range []struct{ name, store, role string }{
		{"broken json", "{ not json ]", "executor"},
		{"directory", "", "executor"},
		{"role not an object", `{"roles":{"explorer":[]}}`, "explorer"},
		{"trim-empty primary model", `{"roles":{"explorer":{"mode":"model","model":"  "}}}`, "explorer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rig := spawnHookNewRig(t, nil, spawnHookCase{})
			home, _ := rig.env("CRW_HOME")
			path := filepath.Join(home, "subagents.json")
			if c.store == "" {
				spawnHookMust(t, os.Mkdir(path, 0o700))
			} else {
				spawnHookMust(t, os.WriteFile(path, []byte(c.store), 0o600))
			}
			got := RunSpawnAttachHook(spawn(rig.ws, c.role, "TASK: look around"), rig.env)
			if !strings.Contains(got, `"permissionDecision":"deny"`) || !strings.Contains(got, "unusable helper role settings") {
				t.Fatalf("unusable %s spawn = %q", c.role, got)
			}
			// A managed start marker of that role is denied as well, before any ledger is read.
			got = RunSpawnAttachHook(spawn(rig.ws, c.role, "[CRW-DISPATCH:one:att-1]\nTASK: look around"), rig.env)
			if !strings.Contains(got, `"permissionDecision":"deny"`) {
				t.Fatalf("managed spawn = %q", got)
			}
			// An event that is not a spawn is not affected.
			if got := RunSpawnAttachHook(`{"hook_event_name":"PostToolUse","tool_name":"spawn_agent","tool_input":{"message":"x"}}`, rig.env); got != "" {
				t.Fatalf("unrelated event = %q", got)
			}
			if c.store != "" {
				if data, _ := os.ReadFile(path); string(data) != c.store {
					t.Fatalf("the store changed: %q", data)
				}
			}
		})
	}
	// A broken role does not stop another role's spawn.
	rig := spawnHookNewRig(t, nil, spawnHookCase{})
	home, _ := rig.env("CRW_HOME")
	spawnHookMust(t, os.WriteFile(filepath.Join(home, "subagents.json"), []byte(`{"roles":{"explorer":{"mode":"model","model":" "},"executor":{"mode":"model","model":"vendor/exec"}}}`), 0o600))
	got := RunSpawnAttachHook(spawn(rig.ws, "executor", "TASK: build it"), rig.env)
	if !strings.Contains(got, `"permissionDecision":"allow"`) || !strings.Contains(got, `"model":"vendor/exec"`) {
		t.Fatalf("executor spawn beside a broken explorer = %q", got)
	}
}

func spawnHookQuote(s string) string { return spawnHookRouteStringify(s) }
