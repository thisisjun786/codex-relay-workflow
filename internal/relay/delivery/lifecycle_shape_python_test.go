package delivery

import (
	"encoding/json"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
	"testing"
)

type shapeLifecycleHost struct {
	runtime, accepts, goal any
}

func (h shapeLifecycleHost) ReadThread(string) (ThreadFacts, error) {
	return ThreadFacts{RuntimeStatus: h.runtime, CanAcceptInput: h.accepts}, nil
}
func (h shapeLifecycleHost) IsArchived(string, any) (*bool, error) {
	value := false
	return &value, nil
}
func (h shapeLifecycleHost) ReadGoalStatus(string) (any, error)       { return h.goal, nil }
func (shapeLifecycleHost) ListTurnIDs(string, int) ([]any, error)     { return nil, nil }
func (shapeLifecycleHost) ReadTurn(string, string) (*TurnInfo, error) { return nil, nil }
func (shapeLifecycleHost) SendMessage(string, string, string, *TaskSettings) (Obj, error) {
	return nil, nil
}
func (shapeLifecycleHost) GetOperation(string) (Obj, error) { return nil, nil }
func (shapeLifecycleHost) FindToken(string, string, int, bool) (TokenScan, error) {
	return TokenScan{}, nil
}
func (shapeLifecycleHost) FindDispatchedTurn(string, string, float64) (TurnPresence, error) {
	return TurnPresence{}, nil
}
func (shapeLifecycleHost) FindTokenSince(string, string, []string, int) (TokenScan, error) {
	return TokenScan{}, nil
}
func (shapeLifecycleHost) FindTokenInTurn(string, string, string, int) (TokenScan, error) {
	return TokenScan{}, nil
}
func (shapeLifecycleHost) RecipientFingerprint(string) (string, error) { return "", nil }

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
		observation := Observe(host, "thread", nil, true)
		got = append(got, map[string]any{"deliverable": observation.Deliverable, "reason": observation.WithholdReason})
	}
	actual, _ := json.Marshal(got)
	golden.Check(t, "lifecycle_shape", actual)
}
