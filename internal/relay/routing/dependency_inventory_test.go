package routing

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

const (
	trackingPackage = "github.com/thisisjun786/codex-relay-workflow/internal/tracking"
	refLeafPackage  = trackingPackage + "/ref"
)

// The relay makes no tracker call, so routing must not acquire the GitHub client. It reads issue
// identifiers through the network-free leaf internal/tracking/ref, never internal/tracking itself.
// Package net itself is permitted (google/uuid uses it).
func TestRoutingDependencyInventoryContainsNoNetworkClient(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Dir = "."
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./...: %v\n%s", err, raw)
	}
	forbidden := []string{"net/http", "net/rpc", "net/smtp", "github.com/coder/websocket"}
	var found []string
	var hasLeaf, hasTracking bool
	for _, dependency := range strings.Fields(string(raw)) {
		for _, client := range forbidden {
			if dependency == client || strings.HasPrefix(dependency, client+"/") {
				found = append(found, dependency)
			}
		}
		switch dependency {
		case refLeafPackage:
			hasLeaf = true
		case trackingPackage:
			hasTracking = true
			found = append(found, dependency)
		}
	}
	if len(found) > 0 {
		sort.Strings(found)
		t.Fatalf("internal/relay/routing must not depend on network client packages: %s", strings.Join(found, ", "))
	}
	if !hasLeaf {
		t.Fatalf("internal/relay/routing must read issue identifiers through %s", refLeafPackage)
	}
	if hasTracking {
		t.Fatalf("internal/relay/routing must not import %s", trackingPackage)
	}
}
