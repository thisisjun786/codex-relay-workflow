package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"regexp/syntax"
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
		if pat, ok := n["pattern"].(string); ok {
			return synthesizeString(pat)
		}
		return ""
	case "integer", "number":
		return synthesizeNumber(n, typ == "integer")
	case "boolean":
		return false
	}
	return nil
}

// synthesizeNumber picks a number that satisfies minimum, maximum, exclusiveMinimum, exclusiveMaximum and multipleOf:
// 0 when that is allowed, else a value at or near the bound that keeps 0 out. Only the bounds the schema names take
// part. A number without multipleOf is continuous, so a range that holds no whole number still has an answer; an
// integer or a multipleOf is found by walking the multiples away from that bound.
func synthesizeNumber(n map[string]any, integer bool) any {
	num := func(k string) (float64, bool) { v, ok := n[k].(float64); return v, ok }
	type bound struct {
		v         float64
		set, excl bool
	}
	// The tighter of the inclusive and the exclusive bound on each side.
	var lower, upper bound
	if v, ok := num("minimum"); ok {
		lower = bound{v, true, false}
	}
	if v, ok := num("exclusiveMinimum"); ok && (!lower.set || v >= lower.v) {
		lower = bound{v, true, true}
	}
	if v, ok := num("maximum"); ok {
		upper = bound{v, true, false}
	}
	if v, ok := num("exclusiveMaximum"); ok && (!upper.set || v <= upper.v) {
		upper = bound{v, true, true}
	}
	step, hasStep := num("multipleOf")
	if !hasStep || step <= 0 {
		step, hasStep = 0, false
	}
	ok := func(v float64) bool {
		switch {
		case math.IsInf(v, 0) || math.IsNaN(v):
			return false
		case lower.set && (v < lower.v || lower.excl && v == lower.v):
			return false
		case upper.set && (v > upper.v || upper.excl && v == upper.v):
			return false
		case integer && v != math.Trunc(v):
			return false
		case hasStep && math.Abs(v/step-math.Round(v/step)) > 1e-9:
			return false
		}
		return true
	}
	if ok(0) {
		return 0
	}
	// 0 is a whole number and a multiple of anything, so a bound keeps it out: the lower one (the range lies above 0)
	// or else the upper one (below 0). Start at that bound and move into the range.
	from, to, dir := upper, lower, -1.0
	if lower.set && (lower.v > 0 || lower.excl && lower.v == 0) {
		from, to, dir = lower, upper, 1.0
	}
	if !from.set {
		return 0
	}
	if !integer && !hasStep {
		cands := []float64{from.v}
		if to.set {
			cands = append(cands, from.v+(to.v-from.v)/2)
		} else {
			cands = append(cands, from.v+dir*math.Max(1, math.Abs(from.v)/2))
		}
		cands = append(cands, math.Nextafter(from.v, dir*math.Inf(1)))
		for _, c := range cands {
			if ok(c) {
				return c
			}
		}
		return 0
	}
	unit := step
	if unit == 0 {
		unit = 1
	}
	// One multiple at a time (a fractional multipleOf of an integer needs a few more).
	k := math.Ceil(from.v / unit)
	if dir < 0 {
		k = math.Floor(from.v / unit)
	}
	for i := 0; i < 10000; i++ {
		if v := (k + dir*float64(i)) * unit; ok(v) {
			return v
		}
	}
	// Past 2^53 every float is whole and adding 1 changes nothing: the next float is the next integer.
	if v := math.Nextafter(from.v, dir*math.Inf(1)); !hasStep && ok(v) {
		return v
	}
	return 0
}

// synthesizeString builds a short string that matches the pattern (an unanchored pattern matches anywhere), or "" when
// the pattern cannot be read or no string was found.
func synthesizeString(pattern string) string {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return ""
	}
	if re.MatchString("") {
		return ""
	}
	tree, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return ""
	}
	if cand := shortest(tree.Simplify()); re.MatchString(cand) {
		return cand
	}
	for _, cand := range []string{"a", "A", "0", "x", "a0", "a-0", "1"} {
		if re.MatchString(cand) {
			return cand
		}
	}
	return ""
}

// shortest writes a string for a regular expression tree: the first alternative, the first character of a class, the
// minimum repeat count (at least reps for a repeat that has none).
func shortest(re *syntax.Regexp) string {
	switch re.Op {
	case syntax.OpLiteral:
		return string(re.Rune)
	case syntax.OpCharClass:
		if len(re.Rune) >= 2 {
			return string(re.Rune[0])
		}
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return "a"
	case syntax.OpCapture:
		return shortest(re.Sub[0])
	case syntax.OpConcat:
		var b strings.Builder
		for _, sub := range re.Sub {
			b.WriteString(shortest(sub))
		}
		return b.String()
	case syntax.OpAlternate:
		return shortest(re.Sub[0])
	case syntax.OpStar, syntax.OpQuest:
		return ""
	case syntax.OpPlus:
		return shortest(re.Sub[0])
	case syntax.OpRepeat:
		return strings.Repeat(shortest(re.Sub[0]), re.Min)
	}
	return ""
}
