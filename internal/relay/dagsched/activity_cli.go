package dagsched

import (
	"context"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// dag-activity (docs/relay/dag-progress.md). It is registered here, beside the projection it runs, and opens the store the way dag-progress does: read-only, never created.

func init() {
	dispatch.Register(nil, dispatch.Command{Name: "dag-activity", ReadOnly: true, Run: runActivity})
}

func runActivity(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	s, err := openProgressStore(ctx, services.Selection.DBPath())
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	activity, err := (&Scheduler{Store: s}).ReadActivity(ctx, args.Text("plan"))
	if err != nil {
		var broken *InvariantError
		if errors.As(err, &broken) {
			return nil, dispatch.Host(broken.Error())
		}
		return nil, hostFailure(err)
	}
	return activity.Object(), nil
}
