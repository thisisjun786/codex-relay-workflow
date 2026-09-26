package contracttest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func runAppServer(t *testing.T, scenario Scenario) (map[string]any, error) {
	host := fakehost.Start(t)
	host.Respond("project/read", fakehost.Reply{Result: map[string]any{"project": map[string]any{"id": "fixture-project"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := appserver.Dial(ctx, host.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("appserver handshake: %w", err)
	}
	defer client.Close()
	steps, ok := scenario.Run["steps"].([]any)
	if !ok {
		return nil, fmt.Errorf("appserver steps: %w", ErrFixture)
	}
	results := make([]any, 0, len(steps))
	for _, raw := range steps {
		step, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("appserver step: %w", ErrFixture)
		}
		method, ok := step["method"].(string)
		if !ok {
			return nil, fmt.Errorf("appserver method: %w", ErrFixture)
		}
		params, _ := step["params"].(map[string]any)
		value, err := client.Call(ctx, method, params)
		if err != nil {
			return nil, err
		}
		var decoded any
		if err := json.Unmarshal(value, &decoded); err != nil {
			return nil, err
		}
		results = append(results, decoded)
	}
	calls := make([]any, 0, len(host.Requests()))
	for _, request := range host.Requests() {
		var params any
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return nil, err
		}
		calls = append(calls, []any{request.Method, params})
	}
	return map[string]any{"exit": float64(0), "results": results, "calls": calls}, nil
}
