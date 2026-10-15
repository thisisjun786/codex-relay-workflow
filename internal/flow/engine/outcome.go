package engine

import (
	"encoding/json"
	"strings"
)

// Outcome is what a script that ended completed came to, as its returned value says (CRW-1487): the milestone state a project lane or
// a milestone halt returns (closed, escalated, incomplete, held, stopped, superseded, ...), its reason, source, escalation and
// milestone, and the lanes that did not land. The run's finished row carries it (member outcome), so the completion notice and `crw flow
// ps`/`show` tell the state the run came to, and never call an escalated or incomplete run completed.
type Outcome struct {
	State      string        `json:"state"`
	Reason     string        `json:"reason,omitempty"`
	Source     string        `json:"source,omitempty"`
	Escalation string        `json:"escalation,omitempty"`
	Milestone  string        `json:"milestone,omitempty"`
	Lanes      []OutcomeLane `json:"lanes,omitempty"`
	// LanesMore counts the lanes that did not land beyond the first maxOutcomeLanes.
	LanesMore int `json:"lanes_more,omitempty"`
	// RunState is the state of the script's own returned object when the milestone state is nested in it (a night batch returns
	// {state: closed|incomplete, milestone: the project lane's result}); empty when the state is the value's own.
	RunState string `json:"run_state,omitempty"`
}

// OutcomeLane is a lane of the outcome that did not land.
type OutcomeLane struct {
	Node   string `json:"node"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

const (
	maxOutcomeLanes = 16
	maxOutcomeRunes = 120
)

// outcomeOf reads the outcome of a script's returned value: the top-level string members state, reason, source, escalation and
// milestone of an object, and from its lanes (an array of objects) each lane whose state is not landed. A value that is not an object,
// or has no string state, has no outcome (nil). Each string is made one line and capped at maxOutcomeRunes runes.
//
// A value whose milestone member is itself an object with a string state wraps the milestone's result (a night batch returns
// {state: closed|incomplete, milestone: the project lane's result, lanes}): the outcome is then read from that object, one level
// down, so the notice tells the milestone's state and reason, and the value's own state is kept as RunState.
func outcomeOf(value json.RawMessage) *Outcome {
	top := outcomeObject(value)
	if top == nil {
		return nil
	}
	state, ok := outcomeText(top["state"])
	if !ok || state == "" {
		return nil
	}
	if inner := outcomeObject(top["milestone"]); inner != nil {
		if o := outcomeOfObject(inner); o != nil {
			o.RunState = state
			return o
		}
	}
	return outcomeOfObject(top)
}

// outcomeObject is a JSON object's members, nil when the value is not an object.
func outcomeObject(value json.RawMessage) map[string]json.RawMessage {
	var top map[string]json.RawMessage
	if len(value) == 0 || json.Unmarshal(value, &top) != nil {
		return nil
	}
	return top
}

// outcomeOfObject is the outcome of one returned object (its own members only), nil when it has no string state.
func outcomeOfObject(top map[string]json.RawMessage) *Outcome {
	state, ok := outcomeText(top["state"])
	if !ok || state == "" {
		return nil
	}
	o := &Outcome{State: state}
	o.Reason, _ = outcomeText(top["reason"])
	o.Source, _ = outcomeText(top["source"])
	o.Escalation, _ = outcomeText(top["escalation"])
	o.Milestone, _ = outcomeText(top["milestone"])
	var lanes []json.RawMessage
	if json.Unmarshal(top["lanes"], &lanes) == nil {
		for _, raw := range lanes {
			var lane map[string]json.RawMessage
			if json.Unmarshal(raw, &lane) != nil || lane == nil {
				continue
			}
			ls, _ := outcomeText(lane["state"])
			if ls == "landed" {
				continue
			}
			if len(o.Lanes) == maxOutcomeLanes {
				o.LanesMore++
				continue
			}
			node, _ := outcomeText(lane["node"])
			reason, _ := outcomeText(lane["reason"])
			o.Lanes = append(o.Lanes, OutcomeLane{Node: node, State: ls, Reason: reason})
		}
	}
	return o
}

// outcomeText is a JSON string member made one line and capped; ok is false when the member is not a string.
func outcomeText(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxOutcomeRunes {
		s = string(r[:maxOutcomeRunes])
	}
	return s, true
}

// row is the outcome as the finished row stores it (plain JSON values, as the row reads back from any journal).
func (o *Outcome) row() map[string]any {
	b, err := json.Marshal(o)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// FinishedOutcome reads the outcome a run's finished row carries (the row's data), nil when it carries none.
func FinishedOutcome(data map[string]any) *Outcome {
	v, ok := data["outcome"]
	if !ok || v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var o Outcome
	if json.Unmarshal(b, &o) != nil || o.State == "" {
		return nil
	}
	return &o
}
