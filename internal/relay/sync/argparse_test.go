package sync

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

var syncCommands = []string{"sync-target", "sync-next", "sync-claim", "sync-operation", "sync-reconcile", "sync-complete", "sync-fail", "sync-retry", "sync-status", "sync-progress", "packet-check"}

func Test23_CLI_RegistryIntegration(t *testing.T) {
	for _, name := range syncCommands {
		if !dispatch.Registered(name) {
			t.Fatalf("%s is not registered", name)
		}
		if _, ok := argparse.Specs[name]; !ok {
			t.Fatalf("%s missing shared argparse spec", name)
		}
	}
}
