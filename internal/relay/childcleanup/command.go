package childcleanup

import (
	"context"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// child-cleanup is the parent's step after the merge and the integration observation (crw-run merge-readiness): it releases the finished child and its sub-threads from the App Server. It reads the
// relay store and talks to the App Server named by the explicit --socket, so it cannot clean the wrong one, and connects only after the gate has judged the relationship; --dry-run only plans (and opens
// the store read-only). The answer is the report; a cleanup that is not complete (something running, left loaded, failed or unresolved) is the same payload with ok false under exit 2, and the parent runs
// it again later. A host failure after some threads were released is exit 3 with the report of what was done and the error as "stopped" (the thread being handled when it happened is not in items).
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
	client := appserver.New(services.SocketPath, appserver.DefaultBounds) // connects at its first call
	defer client.Close()
	dryRun := args.Bool("dry-run")
	report, err := Execute(ctx, st, client, args.Text("relationship"), args.Text("actor"), dryRun)
	return answerOf(args.Text("relationship"), report, err, dryRun)
}

// answerOf is the command's answer: a refusal as it is, a host failure before anything was done as the host envelope, and otherwise the report (ok false and exit 2 when the cleanup is not complete,
// exit 3 when a host failure stopped it after some threads were released).
func answerOf(relationship string, report Report, err error, dryRun bool) (any, error) {
	answer := func(ok bool) contract.OrderedObject {
		return append(contract.OrderedObject{{Key: "ok", Value: ok}, {Key: "schema", Value: "child-cleanup/1"}, {Key: "relationship_id", Value: relationship}}, report.Object()...)
	}
	var refused *store.RefusedError
	switch {
	case errors.As(err, &refused):
		return nil, err
	case err != nil && len(report.Items) == 0:
		return nil, dispatch.Host(err.Error())
	case err != nil:
		return nil, &dispatch.PayloadExit{Payload: answer(false), Code: contract.ExitHost}
	case !dryRun && !report.Complete():
		return nil, &dispatch.PayloadExit{Payload: answer(false), Code: contract.ExitRefused}
	}
	return answer(true), nil
}
