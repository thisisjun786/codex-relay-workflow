package dagsched

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// The commands of the coordinator epoch, adoption and restart (docs/relay/dag-scheduler.md). The commands that decide carry --expect-epoch (cli.go: openScheduler reads it); these three
// are the ones that raise the epoch, bind a replaced parent's child, and show a restarted parent what the store holds.

func init() {
	dispatch.Register(nil,
		dispatch.Command{Name: "dag-coordinator-claim", Run: runClaim},
		dispatch.Command{Name: "dag-adopt", Run: runAdopt},
		// dag-restart reads, and only reads.
		dispatch.Command{Name: "dag-restart", ReadOnly: true, Run: runRestart},
	)
}

func runClaim(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	sched, closeStore, err := openScheduler(ctx, services, args)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	result, err := sched.ClaimEpoch(ctx, args.Text("plan"), ClaimInput{Project: args.Text("project"), Actor: args.Text("actor"), SessionNonce: args.Text("session-nonce")})
	if err != nil {
		return nil, hostFailure(err)
	}
	return result.Object(), nil
}

func runAdopt(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	sched, closeStore, err := openScheduler(ctx, services, args)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	result, err := sched.Adopt(ctx, args.Text("plan"), args.Text("node"), args.Text("actor"))
	if err != nil {
		return nil, hostFailure(err)
	}
	return result.Object(), nil
}

func runRestart(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	sched, closeStore, err := openScheduler(ctx, services, args)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	report, err := sched.Restart(ctx, args.Text("plan"), args.Text("actor"))
	if err != nil {
		return nil, hostFailure(err)
	}
	return report.Object(), nil
}
