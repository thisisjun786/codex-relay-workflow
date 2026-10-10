package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// FakeThread is a thread of the fake backend.
type FakeThread struct {
	ID    string
	Pair  Pair
	Role  string
	Turns []string
}

// FakeRequest is one request the fake backend received.
type FakeRequest struct {
	Kind     string // "thread" or "turn"
	CallID   string
	Op       string
	ThreadID string
	Prompt   string
	Repair   bool
}

// FakeReply is what a fake turn answers. The zero value is the default answer.
type FakeReply struct {
	// Text replaces the default message (a value that satisfies the schema, or a short sentence without one).
	Text string
	// Fail answers with a terminal failed or interrupted turn.
	Fail *TurnFailure
	// Err answers the request with an error instead of a turn (an *RPCError or a transport failure).
	Err error
	// Tokens overrides the token usage.
	Tokens *Tokens
}

// Fake is a deterministic in-process backend. Thread and turn ids derive from the call id, so two runs of a script
// name them identically whatever order the calls complete in. Handler decides a turn's answer; without one every turn
// answers the default message. StartHook decides a thread's creation.
type Fake struct {
	Handler   func(FakeRequest) FakeReply
	StartHook func(ThreadSpec) (ThreadInfo, error)

	mu       sync.Mutex
	threads  map[string]*FakeThread
	requests []FakeRequest
}

// NewFake returns a fake backend that answers every turn with the default message.
func NewFake() *Fake { return &Fake{threads: map[string]*FakeThread{}} }

// Requests returns every request received, in arrival order.
func (f *Fake) Requests() []FakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeRequest(nil), f.requests...)
}

// Thread returns one thread by id.
func (f *Fake) Thread(id string) (FakeThread, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.threads[id]
	if !ok {
		return FakeThread{}, false
	}
	return FakeThread{ID: t.ID, Pair: t.Pair, Role: t.Role, Turns: append([]string(nil), t.Turns...)}, true
}

// StartThread implements Backend.
func (f *Fake) StartThread(ctx context.Context, spec ThreadSpec) (ThreadInfo, error) {
	f.mu.Lock()
	f.requests = append(f.requests, FakeRequest{Kind: "thread", CallID: spec.CallID})
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return ThreadInfo{}, &TransportError{Err: err}
	}
	info := ThreadInfo{ID: "thr_" + spec.CallID, Pair: spec.Pair}
	if f.StartHook != nil {
		var err error
		if info, err = f.StartHook(spec); err != nil {
			return ThreadInfo{}, err
		}
		if info.ID == "" {
			info.ID = "thr_" + spec.CallID
		}
	}
	f.mu.Lock()
	f.threads[info.ID] = &FakeThread{ID: info.ID, Pair: info.Pair, Role: spec.Role}
	f.mu.Unlock()
	return info, nil
}

// RunTurn implements Backend.
func (f *Fake) RunTurn(ctx context.Context, spec TurnSpec) (TurnOutcome, error) {
	prompt := spec.Input
	if _, rest, ok := strings.Cut(prompt, "\n\n"); ok {
		prompt = rest
	}
	req := FakeRequest{Kind: "turn", CallID: spec.CallID, Op: spec.Op, ThreadID: spec.ThreadID, Prompt: prompt, Repair: spec.Repair}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	th := f.threads[spec.ThreadID]
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return TurnOutcome{}, &TransportError{Err: err}
	}
	if th == nil {
		return TurnOutcome{}, &RPCError{Message: "thread " + spec.ThreadID + " not found"}
	}
	var reply FakeReply
	if f.Handler != nil {
		reply = f.Handler(req)
		if err := ctx.Err(); err != nil {
			return TurnOutcome{}, &TransportError{Err: err}
		}
	}
	if reply.Err != nil {
		return TurnOutcome{}, reply.Err
	}
	f.mu.Lock()
	th.Turns = append(th.Turns, spec.Op)
	turnID := fmt.Sprintf("%s/turn-%d", th.ID, len(th.Turns))
	f.mu.Unlock()
	if spec.Started != nil {
		spec.Started(turnID)
	}
	out := TurnOutcome{TurnID: turnID, Failed: reply.Fail}
	if reply.Fail == nil {
		out.Text = reply.Text
		if out.Text == "" {
			out.Text = defaultMessage(prompt, spec.Schema)
		}
	}
	out.Tokens = Tokens{Input: int64(len(prompt)/4 + 1), Output: int64(len(out.Text)/4 + 1)}
	if reply.Tokens != nil {
		out.Tokens = *reply.Tokens
	}
	if out.Tokens.Total == 0 {
		out.Tokens.Total = out.Tokens.Input + out.Tokens.Output
	}
	return out, nil
}

func defaultMessage(prompt string, schema []byte) string {
	if len(schema) == 0 {
		line, _, _ := strings.Cut(prompt, "\n")
		if len(line) > 60 {
			line = line[:60]
		}
		return "fake answer to: " + line
	}
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil {
		return "{}"
	}
	data, _ := json.Marshal(synthesize(root, root, 0))
	return string(data)
}

// synthesize builds the smallest value that satisfies a strict schema.
func synthesize(root, n map[string]any, depth int) any {
	if depth > 20 {
		return nil
	}
	if c, ok := n["const"]; ok {
		return c
	}
	if e, ok := n["enum"].([]any); ok && len(e) > 0 {
		return e[0]
	}
	if ref, ok := n["$ref"].(string); ok {
		if name, ok := strings.CutPrefix(ref, "#/$defs/"); ok {
			if defs, ok := root["$defs"].(map[string]any); ok {
				if d, ok := defs[name].(map[string]any); ok {
					return synthesize(root, d, depth+1)
				}
			}
		}
		return nil
	}
	if branches, ok := n["anyOf"].([]any); ok && len(branches) > 0 {
		if m, ok := branches[0].(map[string]any); ok {
			return synthesize(root, m, depth+1)
		}
	}
	typ := ""
	switch t := n["type"].(type) {
	case string:
		typ = t
	case []any:
		for _, e := range t {
			if s, _ := e.(string); s != "null" && typ == "" {
				typ = s
			}
		}
		if typ == "" {
			typ = "null"
		}
	}
	switch typ {
	case "object":
		out := map[string]any{}
		props, _ := n["properties"].(map[string]any)
		for k, v := range props {
			if m, ok := v.(map[string]any); ok {
				out[k] = synthesize(root, m, depth+1)
			}
		}
		return out
	case "array":
		out := []any{}
		if min, _ := n["minItems"].(float64); min > 0 {
			if items, ok := n["items"].(map[string]any); ok {
				for i := 0; i < int(min); i++ {
					out = append(out, synthesize(root, items, depth+1))
				}
			}
		}
		return out
	case "string":
		return ""
	case "integer", "number":
		if min, ok := n["minimum"].(float64); ok && min > 0 {
			return min
		}
		return 0
	case "boolean":
		return false
	}
	return nil
}
