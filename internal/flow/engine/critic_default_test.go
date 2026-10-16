package engine

import (
	"os"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// TestCriticDefaultPairIsTheSameEverywhere (CRW-1434): the critic's compiled default pair is one pair in the three places
// that name it. The execution policy fills it into a file that declares no critic (execution.DefaultCriticPair), the helper
// role store gives it to a store without a critic member, and the fake backend's table puts the critic on the same model
// family, which is not the architect's: the design audit is independent of the design by model family.
func TestCriticDefaultPairIsTheSameEverywhere(t *testing.T) {
	withRoleStore(t, `{"roles": {}}`)
	snap := role.ReadFlowSnapshot(host.LookupEnv(os.LookupEnv))
	critic, ok := snap.Role(role.Critic)
	if !ok {
		t.Fatalf("the role store's snapshot has no critic: %+v", snap.Roles)
	}
	if critic.State != role.FlowRoleCompiledDefault || critic.Model != execution.DefaultCriticPair.Model || critic.Effort != execution.DefaultCriticPair.Effort {
		t.Fatalf("store critic %+v, execution default %+v", critic, execution.DefaultCriticPair)
	}
	// The table of a production run has the critic at that pair, in a family of its own, read-only, named by agentType.
	table := PolicyFromRoleHelper(snap)
	spec := table.Roles["critic"]
	if spec.Pair != (Pair{execution.DefaultCriticPair.Model, execution.DefaultCriticPair.Effort}) || spec.Family != "critic" || spec.Sandbox != SandboxReadOnly || table.States["critic"] != "compiled_default" {
		t.Fatalf("role-helper critic %+v (%s)", spec, table.States["critic"])
	}
	// The fake backend's critic: on the default's model family, not the architect's, and alone in its role family.
	fake := FakePolicy()
	fc, ok := fake.Roles["critic"]
	if !ok {
		t.Fatal("the fake backend's table has no critic")
	}
	if family := execution.ModelFamily(fc.Pair.Model); family != execution.ModelFamily(execution.DefaultCriticPair.Model) || family == execution.ModelFamily(fake.Roles["architect"].Pair.Model) {
		t.Fatalf("fake critic %+v, architect %+v", fc, fake.Roles["architect"])
	}
	if fc.Family != "critic" || fc.Sandbox != SandboxReadOnly || fc.Pair.Effort != execution.DefaultCriticPair.Effort || !slices.Equal(fc.ModelAliases, execution.DefaultModelAliases("critic")) {
		t.Fatalf("fake critic %+v", fc)
	}
	// A call names the critic by agentType; no alias row leads to it (ocx-gpt-6-1-sol is the reviewer's at any effort).
	for _, p := range []*Policy{fake, table} {
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
	// An execution policy file written before the critic still gives the table a critic, at the default pair.
	fromFile := loadPolicy(t, policyFile("claude-haiku", "xhigh"))
	if got := fromFile.Roles["critic"]; got.Pair != spec.Pair || got.Family != "critic" {
		t.Fatalf("a six-role policy file's critic %+v", got)
	}
}
