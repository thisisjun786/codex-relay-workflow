package dagsched

import (
	"context"
	"errors"
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// dag-progress (docs/relay/dag-progress.md). It is registered here, beside the projection it runs, and not in cli.go with the commands that write.

func init() {
	// ReadOnly: the command never writes, never creates the store and never takes the write gate.
	dispatch.Register(nil, dispatch.Command{Name: "dag-progress", ReadOnly: true, Run: runProgress})
}

func runProgress(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	s, err := openProgressStore(ctx, services.Selection.DBPath())
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	progress, err := (&Scheduler{Store: s}).ReadProgress(ctx, args.Text("plan"))
	if err != nil {
		var broken *InvariantError
		if errors.As(err, &broken) {
			return nil, dispatch.Host(broken.Error())
		}
		return nil, hostFailure(err)
	}
	return progress.Object(), nil
}

// openProgressStore opens the store for a query that must change nothing. A read-only command ordinarily opens through store.Open, which opens a store this runtime may write with the writer's
// opener (the schema script, the index guards, the settlement backfill and the write gate): enough to change a store that still owes one of those repairs. This opens it mode=ro with query_only
// instead, after the same refusal every read-only command gives for a state directory with no store (a read never creates one).
func openProgressStore(ctx context.Context, path string) (*store.Store, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, &store.RefusedError{Reason: store.ReasonStoreAbsent, Detail: "no relay store exists at " + path + "; a read-only command never creates one"}
	}
	return store.OpenReadOnlyStore(ctx, path)
}
