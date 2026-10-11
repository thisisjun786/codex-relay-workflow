package tracetest

import (
	"errors"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// RecordedScript is the workflow the Scenario runs: node n1 is built and reviewed, node n2 is built, stepped to executor-t1 and
// built again, and node n3 is built on a host that never answers (an unknown outcome the engine escalates).
const RecordedScript = `import { RECEIPT, VERDICT } from 'crw:lib/schemas'
export const meta = { name: 'scenario' }
phase('n1')
await agent.call('build n1', { agentType: 'executor', node: 'n1', label: 'build-n1', schema: RECEIPT })
await agent.call('review n1', { agentType: 'reviewer', node: 'n1', label: 'review-n1', schema: VERDICT })
phase('n2')
await agent.call('build n2', { agentType: 'executor', node: 'n2', label: 'build-n2' })
const step = await crw.tierStep('n2', { why: 'two failed heads' })
await agent.call('build n2 again', { agentType: 'executor-t1', node: 'n2', label: 'build-n2-t1', stepDecision: step.id })
phase('n3')
await agent.call('build n3', { agentType: 'executor', node: 'n3', label: 'build-n3' })
return 'done'
`

// Scenario is a synthetic store with one recorded run and the rows around it.
type Scenario struct {
	*Builder
	Run    string
	Plan   string
	Calls  map[string]string // label -> call id
	Result *engine.Result
}

// NewScenario records RecordedScript as run "run-a" of plan "plan-1" (n3's host never answers), and adds the DAG rows of nodes
// n1 (released, accepted on the reviewer call, integrated) and n2 (released, accepted), a failed integration batch followed by
// the batch that landed n1 and n2, deliveries with a busy recipient, a sync_outbox row and a merge train.
func NewScenario(b *Builder) *Scenario {
	b.T.Helper()
	tokens := map[string]int64{"build n1": 1000, "review n1": 400, "build n2": 700, "build n2 again": 900}
	fake := engine.NewFake()
	fake.Handler = func(r engine.FakeRequest) engine.FakeReply {
		prompt := strings.TrimSpace(r.Prompt)
		if prompt == "build n3" {
			return engine.FakeReply{Err: errors.New("the connection was lost")}
		}
		if n, ok := tokens[prompt]; ok {
			return engine.FakeReply{Tokens: &engine.Tokens{Input: n * 3 / 4, Output: n / 4, Total: n}}
		}
		return engine.FakeReply{}
	}
	s := &Scenario{Builder: b, Run: "run-a", Plan: "plan-1", Calls: map[string]string{}}
	s.Result = b.Run(s.Run, s.Plan, RecordedScript, fake, nil)
	for _, c := range s.Result.Calls {
		s.Calls[c.Label] = c.CallID
	}
	b.Execution("x-n1", s.Plan, "n1", "initial", "executor", "t0", s.Run, At(2))
	b.Execution("x-n2", s.Plan, "n2", "initial", "executor", "t0", s.Run, At(3))
	b.Execution("x-n3", s.Plan, "n3", "initial", "executor", "t0", s.Run, At(4))
	// the decisions the run recorded in rows of their own: the tier step of n2 and the escalation of n3 are the journal's
	// decisions again, and an inline decision is not a spawn
	b.StepDecision(s.Run, "n2", "executor", "executor-t1", "SP-TIER-1", At(50))
	b.SpawnDecision(s.Run, "inline", "n1", "", "SP-EXPL-2", "inline", At(51))
	b.Closure("execution", "x-n3", "escalated", "dispatch_unknown", At(60))
	b.Acceptance("acc-n1", s.Plan, "n1", "x-n1", s.Calls["review-n1"], At(100))
	b.Acceptance("acc-n2", s.Plan, "n2", "x-n2", s.Calls["review-n1"], At(105))
	// a failed batch, then the batch that landed both nodes
	b.IntegrationStage("batch-1", s.Plan, "intent", "", "", "head-1", At(110))
	b.AgentIntegrationStage("batch-1", s.Plan, "intent", "n1", "acc-n1", "x-n1", At(110))
	b.AgentIntegrationStage("batch-1", s.Plan, "intent", "n2", "acc-n2", "x-n2", At(110))
	b.IntegrationStage("batch-2", s.Plan, "intent", "", "", "head-2", At(120))
	b.AgentIntegrationStage("batch-2", s.Plan, "intent", "n1", "acc-n1", "x-n1", At(120))
	b.AgentIntegrationStage("batch-2", s.Plan, "intent", "n2", "acc-n2", "x-n2", At(120))
	b.IntegrationStage("batch-2", s.Plan, "ref_moved", "", "", "head-2", At(125))
	b.IntegrationBatch("batch-2", s.Plan, "refs/heads/integration", At(125))
	b.AgentIntegrationStage("batch-2", s.Plan, "marked", "n1", "acc-n1", "x-n1", At(126))
	b.AgentIntegrationStage("batch-2", s.Plan, "marked", "n2", "acc-n2", "x-n2", At(127))
	// deliveries
	b.Delivery("ev-1", "rel-1", "task-a", "dispatched", "", At(200), []string{At(204)}, 0, 0)
	b.Delivery("ev-2", "rel-1", "task-a", "dispatched", "", At(200), []string{At(210), At(212)}, 1, 1)
	b.Delivery("ev-3", "rel-2", "task-a", "deferred_busy", "busy_cap", At(205), nil, 2, 1)
	b.SyncOutbox("pending")
	b.SyncOutbox("done")
	b.MergeTrain()
	return s
}
