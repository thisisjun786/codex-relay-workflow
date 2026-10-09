package adapter

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
)

// The executable owns cleanup composition; this package cannot import that
// command package without cycling through the scheduler's same-package tests.
type hostFactory struct{ configure func(*appserver.Client) }

func (f hostFactory) open(socket, state string, options Options) (*Adapter, error) {
	options.ConfigureClient = f.configure
	return Open(socket, state, options)
}
