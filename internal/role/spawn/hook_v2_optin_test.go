package spawn

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// CRW-1260 (pre-merge evaluation of 91fb1493): a ciphertext V2 call is managed only when the claim opted its attempt in to being
// named by a call's task_name (v2TaskName:true). A task_name that only happens to equal a claimed dispatch's id is not that opt-in:
// a direct native call the user asked for stays unmanaged, keeps its own model and effort, and consumes no issuance.

// v2OptinField is one field of the updatedInput of an allow answer.
func v2OptinField(t *testing.T, answer, input, key string) any {
	t.Helper()
	v, err := pyjson.Loads(spawnReapplyUpdated(t, answer, input), pyjson.LoadOptions{Surrogates: true, Numbers: pyjson.SpelledNumbers})
	spawnHookMust(t, err)
	return v.(pyjson.Object).Get(key)
}

func TestSpawnHookV2TaskNameAloneDoesNotManageADirectCall(t *testing.T) {
	const store = `{"roles":{"reviewer":{"mode":"model","model":"configured/model","effort":"high"}}}`
	input := `{"task_name":"review_job","agent_type":"reviewer","fork_turns":"none","model":"user/model","reasoning_effort":"low","message":` + strconv.Quote(spawnV2Fernet()) + `}`
	for _, tc := range []struct{ name, extra string }{
		{"a claimed attempt that did not opt in", ``},
		{"an opted-out claim", `,"v2TaskName":false`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := spawnHookCase{}
			c.Env.Store = &[]string{store}[0]
			rig := spawnHookNewRig(t, nil, c)
			file := v2ChainLedgerOf(t, rig.ws, "review_job", "reviewer", tc.extra)
			before, err := os.ReadFile(file)
			spawnHookMust(t, err)
			answer := RunSpawnAttachHook(v2ChainStdin(rig.ws, "direct-1", input), rig.env)
			if !strings.Contains(answer, `"permissionDecision":"allow"`) {
				t.Fatalf("the direct call was not allowed: %q", answer)
			}
			if after, _ := os.ReadFile(file); string(after) != string(before) {
				t.Fatal("the direct call consumed the claimed attempt's issuance")
			}
			if m, e := v2OptinField(t, answer, input, "model"), v2OptinField(t, answer, input, "reasoning_effort"); m != "user/model" || e != "low" {
				t.Fatalf("the direct call's pair became %v/%v", m, e)
			}
		})
	}
	// An attempt already issued to another call does not turn the direct call into a refused second managed call.
	c := spawnHookCase{}
	c.Env.Store = &[]string{store}[0]
	rig := spawnHookNewRig(t, nil, c)
	file := v2ChainLedgerOf(t, rig.ws, "review_job", "reviewer", "")
	data, err := os.ReadFile(file)
	spawnHookMust(t, err)
	spawnHookMust(t, os.WriteFile(file, []byte(strings.Replace(string(data), `"spawnIssued":false,"toolUseId":null`, `"spawnIssued":true,"toolUseId":"managed-1"`, 1)), 0o644))
	if answer := RunSpawnAttachHook(v2ChainStdin(rig.ws, "direct-2", input), rig.env); !strings.Contains(answer, `"permissionDecision":"allow"`) {
		t.Fatalf("the direct call was refused because of another call's issuance: %q", answer)
	}
}

// The delegation reference transports a reviewer (or an architect) as an explorer with a CRW-ROLE header where the live schema has
// no such agent type. Under V2 the header is in the ciphertext, so an explorer-typed call naming an opted-in dispatch of any role
// an explorer-typed call can carry is issued that dispatch's attempt; an executor dispatch is not an explorer-typed call's.
func TestSpawnHookV2ExplorerTransportIssuesTheNamedDispatch(t *testing.T) {
	const store = `{"roles":{"reviewer":{"mode":"model","model":"gpt-6.1-sol","effort":"high"}}}`
	cipher := strconv.Quote(spawnV2Fernet())
	for _, tc := range []struct {
		kind   string
		issued bool
	}{{"reviewer", true}, {"architect", true}, {"explorer", true}, {"executor", false}} {
		t.Run(tc.kind, func(t *testing.T) {
			c := spawnHookCase{}
			c.Env.Store = &[]string{store}[0]
			rig := spawnHookNewRig(t, nil, c)
			file := v2ChainLedger(t, rig.ws, "review_job", tc.kind)
			input := `{"task_name":"review_job","agent_type":"explorer","fork_turns":"none","message":` + cipher + `}`
			answer := RunSpawnAttachHook(v2ChainStdin(rig.ws, "native-1", input), rig.env)
			if !strings.Contains(answer, `"permissionDecision":"allow"`) {
				t.Fatalf("hook = %q", answer)
			}
			issued, tool := v2ChainRead(t, file)
			if issued != tc.issued || tc.issued && tool != "native-1" {
				t.Fatalf("issued = %v to %q, want %v", issued, tool, tc.issued)
			}
		})
	}
}

// The reminder a direct V2 call of a role with a configured fallback gets states the V2 rule of the managed path; a V1 call's
// reminder stays the oracle's.
func TestSpawnHookV2DirectCallReminderStatesTheTaskNameRule(t *testing.T) {
	const store = `{"roles":{"reviewer":{"mode":"model","model":"gpt-6.1-sol","effort":"high","fallback":{"model":"p/f","effort":null}}}}`
	const rule = "claim with v2TaskName:true and spawn with task_name equal to the dispatchId"
	for _, tc := range []struct {
		name, input string
		want        bool
	}{
		{"V2 ciphertext", `{"task_name":"probe","agent_type":"reviewer","fork_turns":"none","message":` + strconv.Quote(spawnV2Fernet()) + `}`, true},
		{"V2 plaintext", `{"task_name":"probe","agent_type":"reviewer","fork_turns":"none","message":"TASK: review"}`, true},
		{"V1", `{"agent_type":"reviewer","message":"TASK: review"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := spawnHookCase{}
			c.Env.Store = &[]string{store}[0]
			rig := spawnHookNewRig(t, nil, c)
			answer := RunSpawnAttachHook(v2ChainStdin(rig.ws, "direct-1", tc.input), rig.env)
			if !strings.Contains(answer, "This direct spawn is not managed by first-fallback tracking") {
				t.Fatalf("no direct-call reminder: %q", answer)
			}
			if got := strings.Contains(answer, rule); got != tc.want {
				t.Fatalf("the reminder states the V2 rule: %v, want %v: %q", got, tc.want, answer)
			}
		})
	}
}
