package managed

import (
	"strings"
	"testing"
)

// CRW-466: a request's settings may state the MCP profile the child runs under.
func TestSettingsMayStateAnMCPProfileAsBoundedText(t *testing.T) {
	dir := t.TempDir()
	settings := func(profile any, stated bool) map[string]any {
		m := map[string]any{"sandbox": map[string]any{"type": "readOnly", "networkAccess": false}, "approvalPolicy": "never", "cwd": dir, "runtimeWorkspaceRoots": []any{dir}, "model": "m", "reasoningEffort": "xhigh",
			"environments": []any{map[string]any{"environmentId": "local", "cwd": dir, "runtimeWorkspaceRoots": []any{dir}}}}
		if stated {
			m["mcpProfile"] = profile
		}
		return m
	}
	if err := validateSettings(settings("ui-qa", true), "child.settings"); err != nil {
		t.Fatalf("a stated profile was refused: %v", err)
	}
	if err := validateSettings(settings(nil, false), "child.settings"); err != nil {
		t.Fatalf("a request that states none was refused: %v", err)
	}
	for name, bad := range map[string]any{"blank": " ", "a number": 7, "null": nil, "too long": strings.Repeat("p", 129)} {
		if err := validateSettings(settings(bad, true), "child.settings"); err == nil || !strings.Contains(err.Error(), "mcpProfile") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}
