package engine

import (
	"encoding/json"

	"github.com/dop251/goja"
)

// tierStep is crw.tierStep(node, evidence): the one place a node may move from the executor to executor-t1. This build
// holds only its interface, which the lint rule and the parity corpus rest on: the call takes a position of its path,
// journals a step decision and resolves with {id, checkpoint}. The id is what the executor-t1 call carries as
// stepDecision. The SP-TIER-1 evidence rules, the append-only flow_step_decisions row and the checkpoint worktree
// arrive with the issue that owns crw.tierStep, so checkpoint is null here and the evidence argument is recorded by
// its digest only.
func (s *scriptVM) tierStep(call goja.FunctionCall) goja.Value {
	// A node that holds a call with an unknown outcome (recovery_hold), or that a changed restore stopped, takes no tier
	// step: the step would let the next build run on the node while a request of it may still be at work on the host.
	if node, ok := call.Argument(0).Export().(string); ok {
		if reason := s.e.nodeStopReason(node); reason != "" {
			s.throwType("crw.tierStep: node %q takes no tier step (%s)", node, reason)
		}
	}
	path, position := s.open("crw.tierStep")
	id := s.e.callID(path.id, position)
	stepID := "step_" + id[len("c_"):]
	args := make([]json.RawMessage, 0, len(call.Arguments))
	for _, a := range call.Arguments {
		raw, err := s.jsonOfErr(a)
		if err != nil {
			path.settle(position)
			s.throwType("crw.tierStep: the arguments cannot be serialised: %v", err)
		}
		args = append(args, raw)
	}
	argsJSON, _ := json.Marshal(args)
	if s.e.recorded(id, "crw.tierStep") {
		// a resumed run does not record a call the journal already holds: the step decision is the one it recorded
		return s.stepResult(path, position, stepID)
	}
	s.e.j.Append(Event{RunID: s.e.runID, Kind: KindCall, CallID: id, Path: path.id, Position: position, Op: "crw.tierStep", Label: "crw.tierStep", Event: EvCompleted,
		Data: map[string]any{"result": string(mustJSON(map[string]any{"id": stepID, "checkpoint": nil})), "step_decision": stepID, "evidence_digest": digestJSON(argsJSON)}})
	return s.stepResult(path, position, stepID)
}

// stepResult resolves the call with the step decision {id, checkpoint}.
func (s *scriptVM) stepResult(path *pathState, position int, stepID string) goja.Value {
	step := s.vm.NewObject()
	_ = step.Set("id", stepID)
	_ = step.Set("checkpoint", goja.Null())
	_, _ = s.freeze(goja.Undefined(), step)
	promise, resolve, _ := s.vm.NewPromise()
	complete := s.e.loop.Begin(s.rt)
	complete(func() {
		path.settle(position)
		if err := resolve(step); err != nil {
			s.e.failResolve(err)
		}
	})
	return s.vm.ToValue(promise)
}
