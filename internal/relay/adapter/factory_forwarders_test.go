package adapter

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/daemon"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The package-level forms of the host factory's methods (CRW-828): the executable reaches them through hostFactory in cli.go, and these forwarders exist for the tests.

func hostCommand(ctx context.Context, command, state, socket string, args map[string]any, clock delivery.Clock) (any, error) {
	return (hostFactory{}).hostCommand(ctx, command, state, socket, args, clock)
}

func daemonFactory(ctx context.Context, services dispatch.Services, s *store.Store) (*daemon.Daemon, error) {
	return (hostFactory{}).daemonFactory(ctx, services, s)
}

func observeTurn(ctx context.Context, state, socket, thread, turn string) (string, error) {
	return (hostFactory{}).observeTurn(ctx, state, socket, thread, turn)
}
