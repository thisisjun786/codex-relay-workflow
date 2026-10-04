package role

import (
	"context"
	"encoding/json"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

type DispatchHost interface {
	Call(context.Context, string, map[string]any) (json.RawMessage, error)
}

func CheckedDispatch(ctx context.Context, cwd string, input any, env host.LookupEnv, h DispatchHost) (DispatchResult, error) {
	return RunDispatch(cwd, input, env)
}
