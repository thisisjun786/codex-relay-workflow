package journal

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// LoadLeanIn reads the same rows as LoadLean using the caller's transaction.
// A binding release must judge resumable runs in the snapshot it changes.
func LoadLeanIn(ctx context.Context, q store.Querier, run string) ([]engine.Event, error) {
	return loadLean(ctx, q, run)
}
