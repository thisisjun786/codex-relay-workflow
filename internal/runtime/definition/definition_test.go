package definition_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
)

// The definition names the two components by their console scripts, each with a licence that is
// in the checkout, and the links the installer places beside crw are those console scripts, in
// that order (the completion hook's entry point left them, decision 66). It carries no per-target digest, so no build
// has to regenerate it.
func TestDefinitionNamesTheComponentsTheInstallerPlaces(t *testing.T) {
	if definition.Version != 1 {
		t.Fatalf("definitionVersion %d: the host record states 1 (decision 34)", definition.Version)
	}
	if len(definition.Components) != 2 || definition.Components[0].Name != definition.Bridge || definition.Components[1].Name != definition.Relay {
		t.Fatalf("components %+v", definition.Components)
	}
	for _, g := range definition.Components {
		if g.ConsoleScript != g.Name || g.Version == "" {
			t.Errorf("%s: console script %q, version %q", g.Name, g.ConsoleScript, g.Version)
		}
		if _, err := os.Stat(filepath.Join(golden.Root(), g.LicencePath)); err != nil {
			t.Errorf("%s: licence %s: %v", g.Name, g.LicencePath, err)
		}
	}
	if bridge, _ := definition.Of(definition.Bridge); bridge.IdentityTool != "get_capabilities" {
		t.Errorf("the bridge is identified by %q", bridge.IdentityTool)
	}
	links := definition.Links()
	if len(links) != 2 || links[0] != "codex-session-relay" || links[1] != "codex-thread-bridge" {
		t.Fatalf("links %v", links)
	}
}
