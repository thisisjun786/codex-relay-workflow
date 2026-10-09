package migrate

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/configguard"
)

func TestParseScope(t *testing.T) {
	for in, want := range map[string]Scope{"": ScopeProject, "project": ScopeProject, "user": ScopeUser, "codex": ScopeCodex, "all": ScopeAll} {
		if got, err := ParseScope(in); err != nil || got != want {
			t.Errorf("ParseScope(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseScope("everything"); err == nil || !ScopeAll.Has(ScopeCodex) || ScopeUser.Has(ScopeProject) || !ScopeUser.Has(ScopeUser) {
		t.Error("unknown scope accepted, or Scope.Has wrong")
	}
}

func TestCodexLeaf(t *testing.T) {
	const stamp = "2026-10-05T05-49-04-123Z"
	for in, want := range map[string]string{
		".codexclaw-install.json":                          ".crw-install.json",
		"codexclaw-self-heal.json":                         "crw-self-heal.json",
		"config.toml.codexclaw-" + stamp + ".bak":          "config.toml.crw-" + stamp + ".bak",
		"config.toml.codexclaw-2026-10-05T05:49:04.1Z.bak": "config.toml.crw-2026-10-05T05:49:04.1Z.bak",
		"config.toml":                                      "",
		"config.toml.codexclaw-.bak":                       "",
		"config.toml.codexclaw-xyz.bak":                    "",
		"config.toml.codexclaw-" + stamp + ".bak.bak":      "",
		"config.toml.codexclaw-" + stamp:                   "",
		".crw-install.json":                                "",
	} {
		if got, ok := CodexLeaf(in); got != want || ok != (want != "") {
			t.Errorf("CodexLeaf(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	// The destination names are the ones CRW's own owner writes; a drift in either would orphan the copied records.
	if got, _ := CodexLeaf(".codexclaw-install.json"); got != configguard.InstallManifestName {
		t.Errorf("install manifest maps to %q, configguard writes %q", got, configguard.InstallManifestName)
	}
	if got, _ := CodexLeaf("codexclaw-self-heal.json"); got != configguard.SelfHealMarkerName {
		t.Errorf("self-heal marker maps to %q, configguard writes %q", got, configguard.SelfHealMarkerName)
	}
}
