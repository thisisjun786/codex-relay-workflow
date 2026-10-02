package dagsched

import (
	"context"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
)

// ProductionStarter runs a release's managed-start request through the production engine (managed.HostStart, which carries the worker-policy readiness check, the host adapter
// and the ledger that calling managed.Start directly would skip) with the command's own services and arguments. It needs the explicit --state and --socket a managed start
// requires, so what a release freezes (the selectors) is what the engine fingerprints.
func ProductionStarter(services dispatch.Services, args dispatch.Args) Starter {
	return func(ctx context.Context, raw []byte) (StartAnswer, error) {
		if services.SocketPath == "" || services.Selection.Source != "flag" {
			return StartAnswer{}, &dispatch.UsageError{Detail: "dag-release starts a managed task and requires explicit --state and --socket", Code: contract.ExitUsage}
		}
		if managed.HostStart == nil {
			return StartAnswer{}, dispatch.Host("this build registers no host adapter, so it cannot start a managed task")
		}
		out, err := managed.HostStart(ctx, services, args, raw)
		var payload *dispatch.PayloadExit
		// a refusal or an incomplete start is an answer; a host failure (exit 3) is not, and is returned as the failure it is.
		if errors.As(err, &payload) && payload.Code == contract.ExitRefused && orderedString(payload.Payload, "state") != "" {
			return answerOf(payload.Payload), nil
		}
		if err != nil {
			return StartAnswer{}, err
		}
		return answerOf(out), nil
	}
}

// answerOf reads a managed-start receipt: where it ended and why.
func answerOf(v any) StartAnswer {
	object, _ := v.(contract.OrderedObject)
	return StartAnswer{State: orderedString(object, "state"), Stage: orderedString(object, "stage"), Reason: orderedString(object, "reason"), Answer: object}
}
