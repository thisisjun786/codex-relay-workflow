package engine

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/google/jsonschema-go/jsonschema"
)

// receiptSchema is the executor RECEIPT (workflows/lib/schemas.js, export RECEIPT): crw.classify reads a completed
// result only when its output satisfies it. A test pins the two texts to each other.
const receiptSchema = `{
  "type": "object",
  "properties": {
    "outcome": {"type": "string", "enum": ["ready_for_review", "needs_research", "blocked_scope", "failed", "budget_exhausted"]},
    "files": {"type": "array", "items": {"type": "string"}},
    "verification": {"type": "array", "items": {
      "type": "object",
      "properties": {"cmd": {"type": "string"}, "exit": {"type": "integer"}, "summary": {"type": "string"}},
      "required": ["cmd", "exit", "summary"],
      "additionalProperties": false
    }},
    "summary": {"type": "string"},
    "gap": {"type": ["string", "null"]},
    "research_question": {"type": ["string", "null"]},
    "plan_mismatch": {"type": ["string", "null"]}
  },
  "required": ["outcome", "files", "verification", "summary", "gap", "research_question", "plan_mismatch"],
  "additionalProperties": false
}`

var receiptResolved = sync.OnceValues(func() (*jsonschema.Resolved, error) {
	var sch jsonschema.Schema
	if err := json.Unmarshal([]byte(receiptSchema), &sch); err != nil {
		return nil, err
	}
	return sch.Resolve(nil)
})

// validReceipt reports why output is not an executor RECEIPT, or "" when it is.
func validReceipt(output json.RawMessage) string {
	resolved, err := receiptResolved()
	if err != nil {
		return err.Error()
	}
	var v any
	if err := json.Unmarshal(output, &v); err != nil {
		return "the output is not JSON"
	}
	if err := resolved.Validate(v); err != nil {
		return err.Error()
	}
	return ""
}

// Outcome states crw.classify returns.
const (
	StateReady      = "ready"
	StateResearch   = "research"
	StateScope      = "scope"
	StateSelfFailed = "self_failed"
	StateNullModel  = "null_model"
	StateHostHeld   = "host_held"
	StateNullUnk    = "null_unknown"
	StateRefused    = "refused"
	StateSuperseded = "superseded"
)

// Classify maps an executor CallResult to the one state it is in, from its status and reason and, when it completed,
// the outcome of its RECEIPT (engine.controlFlow.outcomeStates). ok is false for a completed result whose output is
// not a complete RECEIPT. The role is not judged here: crw.classify refuses a result of another role family.
func Classify(res CallResult) (state string, ok bool) {
	switch res.Status {
	case StatusNullModel:
		return StateNullModel, true
	case StatusHeld:
		return StateHostHeld, true
	case StatusUnknown:
		return StateNullUnk, true
	case StatusRefused:
		return StateRefused, true
	case StatusSuperseded:
		return StateSuperseded, true
	case StatusCompleted:
		if validReceipt(res.Output) != "" {
			return "", false
		}
		var receipt struct {
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal(res.Output, &receipt); err != nil {
			return "", false
		}
		switch receipt.Outcome {
		case "ready_for_review":
			return StateReady, true
		case "needs_research":
			return StateResearch, true
		case "blocked_scope":
			return StateScope, true
		case "failed", "budget_exhausted":
			return StateSelfFailed, true
		}
	}
	return "", false
}

// classify is crw.classify(result). It takes the CallResult agent.call returned or that result's handle and nothing
// else: a bare object, a copy of a CallResult, a string that is no handle of this run or null is a TypeError, and there
// is no implicit "last call". Like every crw.* call it takes a position of its path, so it is refused (and the run ends
// script_error) while another call or join of the path has not settled.
func (s *scriptVM) classify(call goja.FunctionCall) goja.Value {
	v := call.Argument(0)
	var res CallResult
	found := false
	if obj, ok := v.(*goja.Object); ok {
		if ref := s.results[obj]; ref != nil {
			res, found = ref.res, true
		}
	} else if str, ok := v.Export().(string); ok {
		res, found = s.e.byHandle[str]
	}
	if !found {
		kind := "a bare value"
		switch {
		case goja.IsNull(v):
			kind = "null"
		case goja.IsUndefined(v):
			kind = "undefined"
		default:
			if _, ok := v.(*goja.Object); ok {
				kind = "an object that is not a CallResult"
			} else if str, ok := v.Export().(string); ok {
				kind = fmt.Sprintf("the string %q, which is no handle of a call of this run", str)
			}
		}
		s.throwType("crw.classify takes the CallResult agent.call returned or its handle, got %s", kind)
	}
	// A result that was refused before its role resolved has none; every other result must be an executor's.
	if spec, known := s.e.cfg.Policy.Roles[res.Meta.Role]; res.Meta.Role != "" && (!known || spec.Family != "executor") {
		s.throwType("crw.classify: call %s ran as %q, not as an executor", res.CallID, res.Meta.Role)
	}
	state, ok := Classify(res)
	if !ok {
		detail := ""
		if res.Status == StatusCompleted {
			detail = ": " + validReceipt(res.Output)
		}
		s.throwType("crw.classify: the output of call %s is not an executor RECEIPT%s", res.CallID, detail)
	}
	path, position := s.open("crw.classify")
	id := s.e.callID(path.id, position)
	// the state is a function of the result it classifies: a resumed run records it once, at the position it was recorded at
	digest := digestJSON(mustJSON(map[string]any{"op": "crw.classify", "of": res.CallID, "state": state, "joins": path.joins}))
	if _, ok := s.e.replayableOp(path, id, "crw.classify", digest); !ok {
		s.e.markLive(path, position)
		s.e.j.Append(Event{RunID: s.e.runID, Kind: KindCall, CallID: id, Path: path.id, Position: position, Op: "crw.classify", Label: "crw.classify", Event: EvCompleted,
			Data: map[string]any{"result": mustJSONString(state), "of": res.CallID, "input_digest": digest}})
	}
	path.settle(position)
	return s.vm.ToValue(state)
}

// clock is crw.clock(): the time, journaled, as an RFC 3339 string. It occupies a position of its path like a call.
func (s *scriptVM) clock(goja.FunctionCall) goja.Value {
	path, position := s.open("crw.clock")
	id := s.e.callID(path.id, position)
	now := s.e.cfg.Now().UTC().Format(time.RFC3339Nano)
	digest := digestJSON(mustJSON(map[string]any{"op": "crw.clock", "joins": path.joins}))
	if prior, ok := s.e.replayableOp(path, id, "crw.clock", digest); ok {
		// the time is journaled: a resumed script reads the time it read before, not the time of the resume
		var recorded string
		if json.Unmarshal([]byte(rowStr(prior.Data, "result")), &recorded) == nil {
			now = recorded
		}
	} else {
		s.e.markLive(path, position)
		s.e.j.Append(Event{RunID: s.e.runID, Kind: KindCall, CallID: id, Path: path.id, Position: position, Op: "crw.clock", Label: "crw.clock", Event: EvCompleted,
			Data: map[string]any{"result": mustJSONString(now), "input_digest": digest}})
	}
	promise, resolve, _ := s.vm.NewPromise()
	complete := s.e.loop.Begin(s.rt)
	complete(func() {
		path.settle(position)
		if err := resolve(now); err != nil {
			s.e.failResolve(err)
		}
	})
	return s.vm.ToValue(promise)
}

func mustJSONString(v string) string { return string(mustJSON(v)) }
