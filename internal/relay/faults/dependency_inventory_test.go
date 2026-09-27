package faults

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// The fault ledger is offline: it persists writes for a separate credential holder and must
// not acquire a network client transitively. Package net itself is permitted (google/uuid uses it).
func TestFaultsDependencyInventoryContainsNoNetworkClient(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Dir = "."
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps ./...: %v\n%s", err, raw)
	}
	forbidden := []string{"net/http", "net/rpc", "net/smtp", "github.com/coder/websocket"}
	var found []string
	for _, dependency := range strings.Fields(string(raw)) {
		for _, client := range forbidden {
			if dependency == client || strings.HasPrefix(dependency, client+"/") {
				found = append(found, dependency)
			}
		}
	}
	if len(found) > 0 {
		sort.Strings(found)
		t.Fatalf("internal/relay/faults must not depend on network client packages: %s", strings.Join(found, ", "))
	}
}
