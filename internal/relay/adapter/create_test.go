package adapter

import "testing"

func Test28_BAD_16_CreationRetainsPartialFailure(t *testing.T) {
	t.Parallel()
	capture(t, scenario{actions: [][]any{{"create", "create-readonly", "bootstrap", "gpt-5.4", ""}}})
	capture(t, scenario{settings: authorized(), answers: []map[string]any{{"creation": true}, {"rpcError": map[string]any{"code": "naming_failed", "message": "naming failed after the shell existed"}}}, actions: [][]any{{"create", "create-stable-1", "bootstrap", "gpt-5.4", ""}, {"operation", "create-stable-1"}, {"create", "create-stable-1", "bootstrap", "gpt-5.4", ""}}})
}
func Test28_BAD_18_DeclaredChildPolicy(t *testing.T) {
	t.Parallel()
	capture(t, scenario{settings: authorized(), actions: [][]any{{"create", "create-child-refused", "bootstrap", "gpt-5.4", "child"}}})
	capture(t, scenario{settings: authorized(), policy: `{"roles":{"child":{"model":"gpt-5.4","reasoningEffort":"medium"}}}`, answers: []map[string]any{{"creation": true}, {}}, actions: [][]any{{"create", "create-child-wrong-pair", "child", "devin/swe-2", "child"}, {"create", "create-child-1", "child", "gpt-5.4", "child"}}})
}
