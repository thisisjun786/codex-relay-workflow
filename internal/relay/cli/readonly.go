package cli

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// readOnlyContext marks ctx for a read-only form (argparse.ReadOnlyForm), so every store.Open
// under it serves the command as Services.store does: never creating a store, and reading
// through Store(read_only=True) wherever this runtime may not write.
func readOnlyContext(ctx context.Context, line []string) context.Context {
	if argparse.ReadOnlyForm(line) {
		return store.WithReadOnlyCommand(ctx)
	}
	return ctx
}
