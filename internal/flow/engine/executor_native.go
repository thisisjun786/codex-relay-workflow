package engine

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
)

type ExecutorProvider interface {
	ExecutorKind() executor.Kind
}

func (c *callRun) prepareExecutorAdmission() {
	p := c.plan
	if c.e.cfg.ExecutorAdmission == nil || !c.writesNodeWorktree() || p.refusal != nil {
		return
	}
	if c.e.cfg.Resume != nil && c.e.prior != nil {
		rows := c.e.prior.rowsOf(c.id, "agent")
		if stateOf(c.id, rows).held() {
			return
		}
	}
	if c.e.cfg.Session == nil {
		provider, ok := c.e.cfg.Backend.(ExecutorProvider)
		if !ok || provider.ExecutorKind() != executor.Codex {
			p.refusal = &Refusal{Reason: "host_config_executor_envelope", Detail: "the backend declares no codex executor"}
			return
		}
	}
	a, err := c.e.cfg.ExecutorAdmission(c.e.ctx, SnapshotRequest{RunID: c.e.runID, CallID: c.id, Node: p.node, Role: p.role, Head: p.opts.Head})
	if err != nil {
		p.refusal = &Refusal{Reason: "host_config_executor_envelope", Detail: err.Error()}
		return
	}
	fail := func(detail string) {
		p.refusal = &Refusal{Reason: "host_config_executor_envelope", Detail: detail}
	}
	if err := executor.ValidateAdmission(a); err != nil {
		fail("the admission is invalid: " + err.Error())
		return
	}
	frozen, err := executor.Freeze(a.Task)
	if err != nil {
		fail("the admission is invalid: " + err.Error())
		return
	}
	if frozen.TaskDigest != a.TaskDigest {
		fail("the admission digest was mutated")
		return
	}
	if a.Task.Executor.Kind != executor.Codex {
		fail("the admission binds a non-codex executor")
		return
	}
	if a.Task.Identity.Node != p.node {
		fail("the admission is of another node")
		return
	}
	if a.Task.Input.Prompt != p.prompt {
		fail("the admission is of another prompt")
		return
	}
	if a.Task.Requested.Model != p.spec.Pair.Model || a.Task.Requested.Effort != p.spec.Pair.Effort {
		fail("the admission asks for another model or effort")
		return
	}
	c.executorAdmission = &frozen
}

func (c *callRun) recordExecutorProvenance(terminal Event) {
	if c.executorAdmission == nil || c.threadID == "" {
		return
	}
	observed := func(v string) executor.Observation {
		if v != "" {
			return executor.Observation{Value: v}
		}
		return executor.Observation{UnknownReason: "not observed by the flow backend"}
	}
	provenance := executor.Provenance{
		Provider:      executor.Codex,
		NativeSession: executor.Observation{Value: c.threadID},
		NativeTurns:   append([]string{}, c.turnIDs...),
		Model:         observed(c.configured.Model),
		Effort:        observed(c.configured.Effort),
		CPUMethod:     executor.Observation{UnknownReason: "not observed by the flow backend"},
		Tools:         []executor.Tool{},
	}
	c.e.j.Append(Event{RunID: c.e.runID, Kind: KindFlow, Op: "executor", CallID: c.id, Event: "executor_native", Data: map[string]any{
		"admission":       *c.executorAdmission,
		"provenance":      provenance,
		"terminal_event":  terminal.Event,
		"native_call_seq": terminal.Seq,
	}})
	if err := journalErr(c.e.j); err != nil {
		c.e.journalFailed(err)
	}
}
