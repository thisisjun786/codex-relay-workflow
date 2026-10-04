package adapter

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/daemon"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The executable owns cleanup composition; this package cannot import that
// command package without cycling through the scheduler's same-package tests.
type hostFactory struct{ configure func(*appserver.Client) }

func (f hostFactory) open(socket, state string, options Options) (*Adapter, error) {
	options.ConfigureClient = f.configure
	return Open(socket, state, options)
}

func observeTurn(ctx context.Context, state, socket, thread, turn string) (string, error) {
	return (hostFactory{}).observeTurn(ctx, state, socket, thread, turn)
}

func daemonFactory(ctx context.Context, services dispatch.Services, s *store.Store) (*daemon.Daemon, error) {
	return (hostFactory{}).daemonFactory(ctx, services, s)
}

func hostCommand(ctx context.Context, command, state, socket string, args map[string]any, clock delivery.Clock) (any, error) {
	return (hostFactory{}).hostCommand(ctx, command, state, socket, args, clock)
}
