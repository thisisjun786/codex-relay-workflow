package adapter

import "testing"

// Capture #2 is the CXC contract case of a host that refuses thread/name/set after the shell existed: CXC
// failed the creation there. CRW-1313 makes a refused name a recorded warning (titleWarning title_not_set)
// and the creation accepted (docs/port-cxc/known-defects/CRW-1313.md), so its golden records an accepted
// receipt that keeps the created id, and its replay answers the same receipt. The partial failure this test
// pins moves to a refused first turn (capture #3): the creation fails with the created id retained, and the
// replay of the same request answers the failed receipt without asking the host again.
func Test28_BAD_16_CreationRetainsPartialFailure(t *testing.T) {
	t.Parallel()
	capture(t, scenario{actions: [][]any{{"create", "create-readonly", "bootstrap", "gpt-5.4", ""}}})
	capture(t, scenario{settings: authorized(), answers: []map[string]any{{"creation": true}, {"rpcError": map[string]any{"code": "naming_failed", "message": "naming failed after the shell existed"}}}, actions: [][]any{{"create", "create-stable-1", "bootstrap", "gpt-5.4", ""}, {"operation", "create-stable-1"}, {"create", "create-stable-1", "bootstrap", "gpt-5.4", ""}}})
	capture(t, scenario{settings: authorized(), answers: []map[string]any{{"creation": true}, {}, {"rpcError": map[string]any{"code": "turn_failed", "message": "first turn failed after the shell existed"}}}, actions: [][]any{{"create", "create-stable-2", "bootstrap", "gpt-5.4", "", "hello"}, {"operation", "create-stable-2"}, {"create", "create-stable-2", "bootstrap", "gpt-5.4", "", "hello"}}})
}
func Test28_BAD_18_DeclaredChildPolicy(t *testing.T) {
	t.Parallel()
	capture(t, scenario{settings: authorized(), actions: [][]any{{"create", "create-child-refused", "bootstrap", "gpt-5.4", "child"}}})
	capture(t, scenario{settings: authorized(), policy: `{"roles":{"child":{"model":"gpt-5.4","reasoningEffort":"medium"}}}`, answers: []map[string]any{{"creation": true}, {}}, actions: [][]any{{"create", "create-child-wrong-pair", "child", "devin/swe-2", "child"}, {"create", "create-child-1", "child", "gpt-5.4", "child"}}})
}
