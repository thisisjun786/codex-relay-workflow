package sync

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
)

var syncCommands = []string{"sync-target", "sync-next", "sync-claim", "sync-operation", "sync-reconcile", "sync-complete", "sync-fail", "sync-retry", "sync-status", "sync-progress", "packet-check"}

func Test23_CLI_RegistryIntegration(t *testing.T) {
	for _, name := range syncCommands {
		found := 0
		for _, command := range cli.Commands {
			if command.Name == name {
				found++
				if command.Run == nil {
					t.Fatalf("incomplete registry entry: %s", name)
				}
			}
		}
		if found != 1 {
			t.Fatalf("%s registered %d times", name, found)
		}
		if _, ok := argparse.Specs[name]; !ok {
			t.Fatalf("%s missing shared argparse spec", name)
		}
	}
}
