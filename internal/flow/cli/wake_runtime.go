package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// currentWake reads the current worker through the existing service observer,
// which checks its process incarnation, scope locks, store and policy publication.
func currentWake(ctx context.Context, r *Reader, socket, thread string) (*delivery.WakeExpectation, error) {
	if socket == "" {
		var err error
		socket, err = store.DefaultSocket()
		if err != nil {
			return nil, err
		}
	}
	selection, err := ResolveState(r.State, socket)
	if err != nil {
		return nil, err
	}
	scope, err := service.ResolveScope()
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	observed := service.ObserveWorkerPolicy(ctx, selection, socket, filepath.Dir(executable), scope)
	if observed.Get("observed") != true {
		return nil, fmt.Errorf("current worker: %v", observed.Get("reason"))
	}
	worker, _ := observed.Get("worker").(service.Object)
	pid, _ := worker.Get("pid").(json.Number)
	n, err := pid.Int64()
	if err != nil {
		return nil, err
	}
	want, err := delivery.ObserveWakeConfiguration(ctx, r.St, socket, thread, supervisor.KindSessionStepWaiting, int(n), nil)
	if err != nil {
		return nil, err
	}
	policy, _ := observed.Get("policy").(service.Object)
	incarnation := fmt.Sprintf("%v:%d:%v", worker.Get("bootId"), n, worker.Get("startTicks"))
	if want.Identity.ServiceInstance != incarnation {
		return nil, fmt.Errorf("worker changed during wake preflight")
	}
	if policy.Get("digest") != want.Identity.RolePolicy {
		return nil, fmt.Errorf("caller and worker role policies differ")
	}
	return want, nil
}
