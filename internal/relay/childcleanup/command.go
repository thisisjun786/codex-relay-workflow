package childcleanup

import (
	"context"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// child-cleanup is the parent's step after the merge and the integration observation (crw-run merge-readiness): it releases the finished child and its sub-threads from the App Server.
// It reads the relay store and talks to the App Server named by the explicit --socket, so it cannot clean the wrong one; --dry-run only plans (and opens the store read-only). The answer is
// the report; a cleanup that is not complete (something running, left loaded, failed or unresolved) is the same payload with ok false under exit 2, and the parent runs it again later.
func init() {
	dispatch.Register(nil, dispatch.Command{Name: "child-cleanup", ReadOnlyWhen: func(args dispatch.Args) bool { return args.Bool("dry-run") }, Run: runCommand})
}

func runCommand(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	if services.SocketPath == "" {
		return nil, &dispatch.UsageError{Detail: "child-cleanup talks to the App Server and requires --socket", Code: contract.ExitUsage}
	}
	st, err := store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	client, err := appserver.Dial(ctx, services.SocketPath)
	if err != nil {
		return nil, dispatch.Host(err.Error())
	}
	defer client.Close()
	dryRun := args.Bool("dry-run")
	result, err := Execute(ctx, st, client, args.Text("relationship"), args.Text("actor"), dryRun)
	var refused *store.RefusedError
	if err != nil && !errors.As(err, &refused) {
		return nil, dispatch.Host(err.Error())
	} else if err != nil {
		return nil, err
	}
	complete := dryRun || result.Complete()
	answer := append(contract.OrderedObject{{Key: "ok", Value: complete}, {Key: "schema", Value: "child-cleanup/1"}, {Key: "relationship_id", Value: result.RelationshipID}}, result.Object()...)
	if !complete {
		return nil, &dispatch.PayloadExit{Payload: answer, Code: contract.ExitRefused}
	}
	return answer, nil
}
