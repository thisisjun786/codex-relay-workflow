package faults

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
)

// Every observation's facts carry the relay package's version (faultsweep.INSTALLATION.version
// in the Python reference, codex_session_relay.__version__). That is the relay component's
// version in the one compatibility definition, internal/runtime/definition (the only copy since
// todo 44 removed scripts/crw_runtime/components.json); the Go constant is read against it so a
// later bump cannot drift silently.
// ownership.CompatibilityBuild is the fence identity and is not checked here.
func TestRelayPackageVersion_is_the_relay_component_version(t *testing.T) {
	relay, ok := definition.Of(definition.Relay)
	if !ok {
		t.Fatalf("the definition has no %s component", definition.Relay)
	}
	if relay.Version != RelayPackageVersion {
		t.Fatalf("definition %s version %q, go %q", definition.Relay, relay.Version, RelayPackageVersion)
	}
}
