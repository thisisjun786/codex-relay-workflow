package spawn

import (
	"encoding/base64"
	"strconv"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// CRW-1260: under multi-agent V2 a spawn that names only agent_type follows the role's pair. The hook fills the role's model and
// reasoning_effort into the V2 input exactly as it does into a V1 one; the placement study's V2 child on "inferhub/kimi-k3"
// without an effort was the reviewer role as configured (model only), not a pool default. What the hook cannot fill is the pair of
// a role left on the main model and a full-history fork (codex-rs rejects model and effort there).

// spawnV2Fernet is a message of the shape of a native V2 task ciphertext (version byte 0x80, 73 bytes).
func spawnV2Fernet() string {
	raw := make([]byte, 73)
	raw[0] = 0x80
	for i := 1; i < len(raw); i++ {
		raw[i] = byte(i * 7)
	}
	return base64.URLEncoding.EncodeToString(raw)
}

func spawnV2Pair(t *testing.T, store, input string) (model, effort pyjson.Field, answered bool) {
	t.Helper()
	c := spawnHookCase{}
	c.Env.Store = &store
	rig := spawnHookNewRig(t, nil, c)
	answer := RunSpawnAttachHook(spawnReapplyPayload(rig.ws, "call-1", input), rig.env)
	if answer == "" {
		return pyjson.Field{}, pyjson.Field{}, false
	}
	updated, err := pyjson.Loads(spawnReapplyUpdated(t, answer, input), pyjson.LoadOptions{Surrogates: true, Numbers: pyjson.SpelledNumbers})
	spawnHookMust(t, err)
	o := updated.(pyjson.Object)
	for _, f := range o {
		switch f.Key {
		case "model":
			model = f
		case "reasoning_effort":
			effort = f
		}
	}
	return model, effort, true
}

func TestSpawnHookV2AgentTypeOnlyTakesTheRolePair(t *testing.T) {
	const pair = `{"roles":{"reviewer":{"mode":"model","model":"gpt-6.1-sol","effort":"high"}}}`
	const modelOnly = `{"roles":{"reviewer":{"mode":"model","model":"inferhub/kimi-k3","effort":null}}}`
	cipher := strconv.Quote(spawnV2Fernet())
	for _, tc := range []struct {
		name, store, input, model, effort string
	}{
		{"fresh", pair, `{"task_name":"independent_review","agent_type":"reviewer","fork_turns":"none","message":"review the diff"}`, "gpt-6.1-sol", "high"},
		{"fresh, native task ciphertext", pair, `{"task_name":"independent_review","agent_type":"reviewer","fork_turns":"none","message":` + cipher + `}`, "gpt-6.1-sol", "high"},
		{"partial fork", pair, `{"task_name":"independent_review","agent_type":"reviewer","fork_turns":"2","message":"review the diff"}`, "gpt-6.1-sol", "high"},
		{"partial fork, native task ciphertext", pair, `{"task_name":"independent_review","agent_type":"reviewer","fork_turns":"2","message":` + cipher + `}`, "gpt-6.1-sol", "high"},
		{"role with a model and no effort", modelOnly, `{"task_name":"independent_review","agent_type":"reviewer","fork_turns":"none","message":"review the diff"}`, "inferhub/kimi-k3", ""},
		{"the caller's own pair stays", pair, `{"task_name":"independent_review","agent_type":"reviewer","fork_turns":"none","model":"mine/model","reasoning_effort":"low","message":"review the diff"}`, "mine/model", "low"},
		{"full-history fork", pair, `{"task_name":"independent_review","agent_type":"reviewer","fork_turns":"all","message":"review the diff"}`, "", ""},
		{"fork_turns left out is a full fork", pair, `{"task_name":"independent_review","agent_type":"reviewer","message":"review the diff"}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model, effort, answered := spawnV2Pair(t, tc.store, tc.input)
			if !answered && tc.model != "" {
				t.Fatal("the hook gave no answer")
			}
			if got, _ := model.Value.(string); got != tc.model {
				t.Errorf("model = %v, want %q", model.Value, tc.model)
			}
			if got, _ := effort.Value.(string); got != tc.effort {
				t.Errorf("reasoning_effort = %v, want %q", effort.Value, tc.effort)
			}
		})
	}
}

// A role left on the main model has no pair to fill: the hook sets neither field, in V2 as in V1, and the child's pair is then the
// host's (the inheritance of an unset pair by a fresh V2 child is the host's and is not asserted here).
func TestSpawnHookV2RoleOnMainModelFillsNothing(t *testing.T) {
	model, effort, _ := spawnV2Pair(t, `{"roles":{"reviewer":{"mode":"default"}}}`,
		`{"task_name":"independent_review","agent_type":"reviewer","fork_turns":"none","message":"review the diff"}`)
	if model.Key != "" || effort.Key != "" {
		t.Fatalf("model %v effort %v", model, effort)
	}
}
