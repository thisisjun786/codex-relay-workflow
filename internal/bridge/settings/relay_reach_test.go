package settings

import "testing"

// CRW-1298: the one rule for whether a relay child reaches the relay daemon's control.sock (bridge creation and relay register ask it).
func TestRelayChildReaches(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sandbox string
		policy  map[string]any
		reaches bool
	}{
		{"read-only sandbox", "read-only", nil, false},
		{"read-only sandbox with a policy that would reach", "read-only", map[string]any{"type": "dangerFullAccess"}, false},
		{"workspace-write sandbox without a policy", "workspace-write", nil, false},
		{"workspace-write, networkAccess false", "workspace-write", map[string]any{"type": "workspaceWrite", "networkAccess": false}, false},
		{"workspace-write, networkAccess not a bool", "workspace-write", map[string]any{"type": "workspaceWrite", "networkAccess": "yes"}, false},
		{"workspace-write, networkAccess true", "workspace-write", map[string]any{"type": "workspaceWrite", "networkAccess": true}, true},
		{"readOnly policy even with network", "", map[string]any{"type": "readOnly", "networkAccess": true}, false},
		{"externalSandbox policy", "", map[string]any{"type": "externalSandbox", "networkAccess": "enabled"}, false},
		{"danger-full-access sandbox", "danger-full-access", nil, true},
		{"dangerFullAccess policy alone", "", map[string]any{"type": "dangerFullAccess"}, true},
		{"nothing stated", "", nil, false},
	} {
		if got := RelayChildReaches(tc.sandbox, tc.policy); got != tc.reaches {
			t.Errorf("%s: RelayChildReaches(%q, %v) = %v, want %v", tc.name, tc.sandbox, tc.policy, got, tc.reaches)
		}
	}
}
