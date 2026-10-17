package engine

import (
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// TestCRW1589_CriticFollowsTheReviewerPair (CRW-1589): a role store that declares no critic gives the critic the
// reviewer's pair — the compiled default of CRW-1434 is gone with the other-family constraint of CRW-1515. A critic
// call resolves onto that pair, a run records it as the call's configured pair, and the same inheritance fills the
// critic of a policy file written before it. unit-check stays a tool name only: it is no role of the table, no call
// resolves to it.
func TestCRW1589_CriticFollowsTheReviewerPair(t *testing.T) {
	withRoleStore(t, `{"roles": {
  "architect": {"mode": "model", "model": "anthropic/claude-opus-5-5", "effort": "xhigh"},
  "executor": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "xhigh"},
  "reviewer": {"mode": "model", "model": "gpt-6-astra", "effort": "xhigh"},
  "explorer": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "high"}}}`)
	snap := role.ReadFlowSnapshot(host.LookupEnv(os.LookupEnv))
	critic, ok := snap.Role(role.Critic)
	if !ok || critic.Model != "gpt-6-astra" || critic.Effort != "xhigh" || critic.State != "reviewer" {
		t.Fatalf("the snapshot's critic %+v (present %v), want the reviewer pair", critic, ok)
	}
	// The table of a production run resolves a critic call onto that pair, in a family of its own, read-only, named by agentType.
	table := PolicyFromRoleHelper(snap)
	res, refusal := table.ResolveFresh(CallOpts{AgentType: "critic"})
	if refusal != nil || res.Role != "critic" || res.Spec.Pair != (Pair{"gpt-6-astra", "xhigh"}) || res.Spec.Family != "critic" || res.Spec.Sandbox != SandboxReadOnly {
		t.Fatalf("agentType critic: %+v %+v", res, refusal)
	}
	// A run records the pair the call ran on: the reviewer's, not a compiled default.
	h := newHarness(t)
	h.cfg.Policy = table
	got := value[struct {
		Status string `json:"status"`
		Pair   Pair   `json:"pair"`
	}](t, h.run(`const r = await agent.call('audit the design', { agentType: 'critic', label: 'audit' }); return { status: r.status, pair: r.meta.configuredPair }`))
	if got.Status != "completed" || got.Pair != (Pair{"gpt-6-astra", "xhigh"}) {
		t.Fatalf("the critic call ran on %+v", got)
	}
	// unit-check is a tool run, not an agent role: it is no role of the table and no call resolves to it.
	if _, ok := table.Roles["unit-check"]; ok {
		t.Error("unit-check is a role of the table: a call could spawn it")
	}
	if res, refusal := table.ResolveFresh(CallOpts{AgentType: "unit-check"}); refusal == nil || res.Role != "" {
		t.Errorf("agentType unit-check resolved to %+v, want a refusal", res)
	}
	// A call names the critic by agentType; no alias row leads to it (ocx-gpt-6-1-sol is the reviewer's at any effort).
	for _, p := range []*Policy{FakePolicy(), table} {
		res, refusal := p.ResolveFresh(CallOpts{AgentType: "critic"})
		if refusal != nil || res.Role != "critic" {
			t.Fatalf("agentType critic: %+v %+v", res, refusal)
		}
		for _, row := range p.Aliases {
			if row.Role == "critic" {
				t.Errorf("an alias row leads to the critic: %+v", row)
			}
		}
	}
	// It spawns as a reviewer: it has no definition file of its own.
	if got := agentTypeOf("critic"); got != "reviewer" {
		t.Errorf("the critic spawns as agent_type %q", got)
	}
	// The fake backend's critic is the reviewer's placeholder pair, as a store without a critic resolves it.
	fake := FakePolicy()
	if fc := fake.Roles["critic"]; fc.Pair != fake.Roles["reviewer"].Pair || fc.Family != "critic" || fc.Sandbox != SandboxReadOnly {
		t.Errorf("fake critic %+v, reviewer %+v", fc, fake.Roles["reviewer"])
	}
	// An execution policy file written before the critic still gives the table a critic: its own reviewer's pair.
	fromFile := loadPolicy(t, policyFile("claude-haiku", "xhigh"))
	if got := fromFile.Roles["critic"]; got.Pair != fromFile.Roles["reviewer"].Pair || got.Family != "critic" {
		t.Fatalf("a five-role policy file's critic %+v, reviewer %+v", got, fromFile.Roles["reviewer"])
	}
}
