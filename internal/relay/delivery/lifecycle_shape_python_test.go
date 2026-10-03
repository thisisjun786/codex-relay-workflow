package delivery

import (
	"context"
	"encoding/json"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
	"testing"
)

type shapeLifecycleHost struct {
	runtime, accepts, goal any
}

func (h shapeLifecycleHost) ReadThread(context.Context, string) (ThreadFacts, error) {
	return ThreadFacts{RuntimeStatus: h.runtime, CanAcceptInput: h.accepts}, nil
}
func (h shapeLifecycleHost) IsArchived(context.Context, string, any) (*bool, error) {
	value := false
	return &value, nil
}
func (h shapeLifecycleHost) ReadGoalStatus(context.Context, string) (any, error)   { return h.goal, nil }
func (shapeLifecycleHost) ListTurnIDs(context.Context, string, int) ([]any, error) { return nil, nil }
func (shapeLifecycleHost) ReadTurn(context.Context, string, string) (*TurnInfo, error) {
	return nil, nil
}
func (shapeLifecycleHost) SendMessage(context.Context, string, string, string, *TaskSettings) (Obj, error) {
	return nil, nil
}
func (shapeLifecycleHost) GetOperation(context.Context, string) (Obj, error) { return nil, nil }
func (shapeLifecycleHost) FindToken(context.Context, string, string, int, bool) (TokenScan, error) {
	return TokenScan{}, nil
}
func (shapeLifecycleHost) FindDispatchedTurn(context.Context, string, string, float64) (TurnPresence, error) {
	return TurnPresence{}, nil
}
func (shapeLifecycleHost) FindTokenSince(context.Context, string, string, []string, int) (TokenScan, error) {
	return TokenScan{}, nil
}
func (shapeLifecycleHost) FindTokenInTurn(context.Context, string, string, string, int) (TokenScan, error) {
	return TokenScan{}, nil
}
func (shapeLifecycleHost) RecipientFingerprint(context.Context, string) (string, error) {
	return "", nil
}

func Test28LifecycleContainerComparisonsMatchPython(t *testing.T) {
	values := []any{[]any{"active"}, map[string]any{"type": "active"}}
	cases := []shapeLifecycleHost{}
	for _, value := range values {
		cases = append(cases,
			shapeLifecycleHost{runtime: value, accepts: true},
			shapeLifecycleHost{runtime: "idle", accepts: value},
		)
	}
	got := make([]map[string]any, 0, len(cases))
	for _, host := range cases {
		observation := Observe(context.Background(), host, "thread", nil, true)
		got = append(got, map[string]any{"deliverable": observation.Deliverable, "reason": observation.WithholdReason})
	}
	actual, _ := json.Marshal(got)
	golden.Check(t, "lifecycle_shape", actual)
}
