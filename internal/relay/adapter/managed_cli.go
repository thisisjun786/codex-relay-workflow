package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func managedStart(services cli.Services, args cli.Args, raw []byte) (out any, err error) {
	ctx := context.Background()
	request, err := managed.ParseRequest(raw)
	if err != nil {
		return nil, err
	}
	policy := registry.EnvironmentRolePolicy()
	observer, err := workerObserver(services)
	if err != nil {
		return nil, err
	}
	readiness := func(ctx context.Context, req map[string]any) (string, error) { return observer.Ready(ctx, req, policy) }
	reason, err := readiness(ctx, request)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return nil, &cli.PayloadExit{Code: contract.ExitRefused, Payload: contract.OrderedObject{{Key: "schema", Value: managed.Schema}, {Key: "state", Value: "refused"}, {Key: "stage", Value: "preflight"}, {Key: "requestId", Value: request["requestId"]}, {Key: "reason", Value: reason}}}
	}
	bridgePolicy, err := execution.FromEnvironment(map[string]string{execution.EnvPolicy: os.Getenv(execution.EnvPolicy), execution.EnvDigest: os.Getenv(execution.EnvDigest)})
	if err != nil {
		return nil, err
	}
	a, err := Open(services.SocketPath, services.Selection.Path, Options{Policy: bridgePolicy, Clock: delivery.CommandClock})
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, a.Close()) }()
	s, err := store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	marker, _ := args.String("marker-root")
	engine := managed.Start{Store: s, Adapter: Managed{a}, Socket: services.SocketPath, MarkerRoot: marker, StateSelector: services.Selection.Path, Readiness: readiness}
	if delivery.CommandClock != nil {
		engine.Now = delivery.CommandClock.ISO
	}
	answer, err := engine.Run(ctx, raw)
	if err != nil {
		return nil, err
	}
	if field(answer, "state") != "admitted" {
		return nil, &cli.PayloadExit{Payload: answer, Code: contract.ExitRefused}
	}
	return answer, nil
}

// workerObserver is the worker-policy reader managed-start admits through, over the scope
// registry the service itself resolves (service.ResolveScope, the scope directory as pathlib
// spells it, as cmd_managed_start's _service_for does), and this executable's installation.
func workerObserver(services cli.Services) (WorkerObservation, error) {
	scope, err := service.ResolveScope()
	if err != nil {
		return WorkerObservation{}, err
	}
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err != nil {
		return WorkerObservation{}, err
	}
	return WorkerObservation{State: services.Selection.Path, Socket: services.SocketPath, Scope: scope.Root, Authority: scope.Authority, Installation: filepath.Dir(executable)}, nil
}
