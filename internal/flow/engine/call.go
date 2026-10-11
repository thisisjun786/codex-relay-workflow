package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/dop251/goja"
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// callPlan is everything about a call that is decided before it opens: its role, pair and inherited identity, or the
// refusal that ends it.
type callPlan struct {
	opts      CallOpts
	prompt    string
	schema    []byte
	schemaRaw map[string]any
	resumeH   string // the handle of a continuation, empty for a fresh call

	role      string
	spec      RoleSpec
	requested Pair
	node      string
	step      string
	resume    *CallInfo
	inherited []string
	refusal   *Refusal
}

func (s *scriptVM) optString(o *goja.Object, key string) string {
	if o == nil {
		return ""
	}
	v := o.Get(key)
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return ""
	}
	if _, ok := v.Export().(string); !ok {
		s.throwType("agent: option %s must be a string", key)
	}
	return v.String()
}

// handleArg reads the value of a resume option: a handle string or a CallResult the engine handed out. Anything else
// (a bare object, null, a number) is a TypeError.
func (s *scriptVM) handleArg(what string, v goja.Value) string {
	if str, ok := v.Export().(string); ok {
		if _, _, ok := ParseHandle(str); !ok {
			s.throwType("%s takes a handle (crwh1:<run>:<call>) or a CallResult, got the string %q", what, str)
		}
		return str
	}
	if obj, ok := v.(*goja.Object); ok {
		if ref, ok := s.results[obj]; ok {
			if ref.res.Handle == "" {
				s.throwType("%s: the CallResult %s has no handle: no thread was started for it", what, ref.res.CallID)
			}
			return ref.res.Handle
		}
	}
	kind := "a bare value"
	switch {
	case goja.IsNull(v):
		kind = "null"
	case goja.IsUndefined(v):
		kind = "undefined"
	default:
		if _, ok := v.(*goja.Object); ok {
			kind = "an object that is not a CallResult"
		}
	}
	s.throwType("%s takes a handle or a CallResult returned by agent.call, got %s", what, kind)
	return ""
}

// agent opens an agent call: agent() resolves with the output (null unless the call completed), agent.call with the
// CallResult.
func (s *scriptVM) agent(call goja.FunctionCall, plain bool) goja.Value {
	s.live()
	name := "agent.call"
	if plain {
		name = "agent"
	}
	pv := call.Argument(0)
	prompt, ok := pv.Export().(string)
	if !ok {
		s.throwType("%s: the prompt must be a string", name)
	}
	var o *goja.Object
	if ov := call.Argument(1); !goja.IsUndefined(ov) && !goja.IsNull(ov) {
		o, ok = ov.(*goja.Object)
		if !ok {
			s.throwType("%s: the options must be an object", name)
		}
	}
	p := &callPlan{prompt: prompt}
	p.opts = CallOpts{
		AgentType: s.optString(o, "agentType"), Model: s.optString(o, "model"), Effort: s.optString(o, "effort"),
		Isolation: s.optString(o, "isolation"), Node: s.optString(o, "node"), StepDecision: s.optString(o, "stepDecision"),
		Label: s.optString(o, "label"), Phase: s.optString(o, "phase"), Head: s.optString(o, "head"),
	}
	p.opts.Cwd = s.optString(o, "worktree")
	if c := s.optString(o, "cwd"); p.opts.Cwd == "" {
		p.opts.Cwd = c
	}
	if o != nil {
		if sv := o.Get("schema"); sv != nil && !goja.IsUndefined(sv) && !goja.IsNull(sv) {
			m, ok := sv.Export().(map[string]any)
			if !ok {
				s.throwType("%s: schema must be a JSON Schema object", name)
			}
			p.schemaRaw = m
			raw, err := json.Marshal(m)
			if err != nil {
				s.throwType("%s: schema is not JSON: %v", name, err)
			}
			p.schema = raw
		}
		if rv := o.Get("resume"); rv != nil && !goja.IsUndefined(rv) {
			p.resumeH = s.handleArg(name+": resume", rv)
		}
	}
	s.plan(p)

	label := p.opts.Label
	if label == "" {
		label = "agent"
		if p.role != "" {
			label += ":" + p.role
		}
	}
	path, position := s.open(label)
	c := &callRun{s: s, e: s.e, plan: p, id: s.e.callID(path.id, position), path: path, position: position, label: label, plain: plain}
	if c.plan.opts.Phase == "" {
		c.plan.opts.Phase = s.e.phase
	}
	promise, resolve, _ := s.vm.NewPromise()
	c.resolve = resolve
	c.complete = s.e.loop.Begin(s.rt)
	c.prepare()
	if p.refusal != nil {
		c.deliverNow(c.terminal(EvRefused, StatusRefused, p.refusal.Reason, map[string]any{"detail": p.refusal.Detail}))
	} else {
		s.e.inflight.Add(1)
		go func() {
			defer s.e.inflight.Done()
			c.deliverNow(c.execute(s.e.ctx))
		}()
	}
	return s.vm.ToValue(promise)
}

// plan decides a call's role, pair and inherited identity from the policy and, for a continuation, from the journal
// record of its handle. A refusal is kept for the call to end with once it has opened.
func (s *scriptVM) plan(p *callPlan) {
	e := s.e
	refuse := func(reason, detail string) {
		if p.refusal == nil {
			p.refusal = &Refusal{Reason: reason, Detail: detail}
		}
	}
	p.node, p.step = p.opts.Node, p.opts.StepDecision
	if p.resumeH == "" {
		r, ref := e.cfg.Policy.ResolveFresh(p.opts)
		if ref != nil {
			refuse(ref.Reason, ref.Detail)
		} else {
			p.role, p.spec, p.requested = r.Role, r.Spec, r.RequestedPair
			if p.role == "executor-t1" && p.step == "" {
				refuse(ReasonMissingStepDecision, "a fresh executor-t1 call carries the stepDecision id crw.tierStep returned")
			}
		}
	} else {
		s.planResume(p, refuse)
	}
	if p.refusal == nil && p.schema != nil {
		if errs := script.LintStrictSchema(p.schemaRaw); len(errs) > 0 {
			refuse(ReasonSchemaNotStrict, strings.Join(errs, "; "))
		}
	}
}

func (s *scriptVM) planResume(p *callPlan, refuse func(reason, detail string)) {
	e := s.e
	runID, callID, _ := ParseHandle(p.resumeH)
	if runID != e.runID {
		refuse(ReasonForeignHandle, fmt.Sprintf("the handle belongs to run %s, not this run", runID))
		return
	}
	info, found := LookupCall(e.j, callID)
	if !found || info.ThreadID == "" {
		refuse(ReasonForeignHandle, "the handle names no call with a thread in this run")
		return
	}
	// The run id in the handle's text is only a claim; the journal row says which run the call belongs to.
	if info.RunID != e.runID {
		refuse(ReasonForeignHandle, fmt.Sprintf("the call the handle names belongs to run %s, not this run", info.RunID))
		return
	}
	spec, ok := e.cfg.Policy.Roles[info.Role]
	if !ok {
		refuse(ReasonPolicyRefused, "the handle's role "+info.Role+" is not in the policy")
		return
	}
	p.resume, p.role, p.spec = &info, info.Role, spec
	p.requested = requested(p.opts, spec)

	// An explicit value that differs from what the handle carries is refused: another role family or another node is a
	// foreign handle, the same family on another pair or step decision is a pair refusal.
	if want := p.opts.AgentType; want != "" {
		role := want
		if !e.cfg.Policy.RoleKey(want) {
			r, ref := e.cfg.Policy.ResolveFresh(CallOpts{AgentType: want, Effort: p.opts.Effort, Isolation: p.opts.Isolation})
			if ref != nil {
				refuse(ref.Reason, ref.Detail)
				return
			}
			role = r.Role
		}
		switch {
		case e.cfg.Policy.Roles[role].Family != spec.Family:
			refuse(ReasonForeignHandle, fmt.Sprintf("the handle is a %s thread; a %s call cannot continue it", spec.Family, e.cfg.Policy.Roles[role].Family))
			return
		case role != info.Role:
			refuse(ReasonPairRefused, fmt.Sprintf("the handle's thread runs as %s; %s would change its pair", info.Role, role))
			return
		}
	} else {
		p.inherited = append(p.inherited, "role")
	}
	if m := p.opts.Model; m != "" && m != spec.Pair.Model && !slices.Contains(spec.ModelAliases, m) {
		refuse(ReasonPairRefused, "model "+m+" is not the thread's model "+spec.Pair.Model)
		return
	}
	// A fresh call records a differing effort and runs on its role's pair; a continuation cannot, because the thread's
	// pair is already fixed: naming another effort is a conflict with what it inherits.
	if ef := p.opts.Effort; ef != "" && ef != spec.Pair.Effort {
		refuse(ReasonPairRefused, "effort "+ef+" is not the thread's effort "+spec.Pair.Effort)
		return
	}
	p.inherited = append(p.inherited, "pair")
	if p.node != "" && p.node != info.Node {
		refuse(ReasonForeignHandle, fmt.Sprintf("the handle belongs to node %q, not %q", info.Node, p.node))
		return
	}
	if p.node == "" {
		p.node = info.Node
		p.inherited = append(p.inherited, "node")
	}
	if p.step != "" && p.step != info.StepDecision {
		refuse(ReasonPairRefused, fmt.Sprintf("stepDecision %q differs from the handle's %q", p.step, info.StepDecision))
		return
	}
	if p.step == "" {
		p.step = info.StepDecision
		p.inherited = append(p.inherited, "stepDecision")
	}
	// A thread whose pair read back wrong when it started was refused then and stays refused: the handle it carries
	// continues nothing.
	if info.Configured != spec.Pair {
		refuse(ReasonPairRefused, fmt.Sprintf("the thread reads back %s/%s, the role's pair is %s/%s", info.Configured.Model, info.Configured.Effort, spec.Pair.Model, spec.Pair.Effort))
	}
}

// callRun is one agent call from its opening to its result.
type callRun struct {
	s        *scriptVM
	e        *Engine
	plan     *callPlan
	id       string
	path     *pathState
	position int
	label    string
	plain    bool

	resolve  func(any) error
	complete func(func())

	startSeq   int
	threadID   string
	configured Pair
	turnIDs    []string
	tokens     Tokens
	repairUsed bool
	delivered  bool
}

func (c *callRun) event(ev string, data map[string]any) Event {
	return c.e.j.Append(Event{RunID: c.e.runID, Kind: KindCall, CallID: c.id, Path: c.path.id, Position: c.position, Op: "agent", Label: c.label, Event: ev, Data: data})
}

// prepare writes the call's intent row before anything is sent.
func (c *callRun) prepare() {
	p := c.plan
	d := map[string]any{
		"role": p.role, "family": p.spec.Family, "model": p.spec.Pair.Model, "effort": p.spec.Pair.Effort,
		"requested_model": p.requested.Model, "requested_effort": p.requested.Effort,
		"node": p.node, "step_decision": p.step, "phase": p.opts.Phase, "head": p.opts.Head, "cwd": p.opts.Cwd,
		"input_digest": digestJSON([]byte(p.prompt)), "plain": c.plain,
	}
	if p.schema != nil {
		d["schema_digest"] = digestJSON(p.schema)
	}
	if p.resume != nil {
		d["resume_of"] = p.resume.CallID
		d["thread_id"] = p.resume.ThreadID
		d["through_turn_id"] = p.resume.ThroughTurnID
		d["configured_model"] = p.resume.Configured.Model
		d["configured_effort"] = p.resume.Configured.Effort
		d["inherited"] = p.inherited
	}
	c.startSeq = c.event(EvPrepared, d).Seq
}

// terminal journals the call's last event and returns its result.
func (c *callRun) terminal(ev, status, reason string, data map[string]any) CallResult {
	if data == nil {
		data = map[string]any{}
	}
	data["reason"] = reason
	row := c.event(ev, data)
	startSeq, endSeq := c.startSeq, row.Seq
	if c.e.cfg.OmitSeq {
		startSeq, endSeq = 0, 0
	}
	res := CallResult{
		CallID: c.id, Path: c.path.id, Position: c.position, Label: c.label, Status: status, Reason: reason,
		Meta: CallMeta{
			Role: c.plan.role, RequestedPair: c.plan.requested, ConfiguredPair: c.configured, ThreadID: c.threadID,
			TurnIDs: append([]string{}, c.turnIDs...), RepairUsed: c.repairUsed, Tokens: c.tokens, StartSeq: startSeq, EndSeq: endSeq,
		},
	}
	if c.threadID != "" {
		res.Handle = Handle(c.e.runID, c.id)
	}
	if out, ok := data["result"].(string); ok && status == StatusCompleted {
		res.Output = json.RawMessage(out)
	}
	return res
}

// deliverNow hands the result to the loop, which settles the path and resolves the script's promise. Until the loop
// runs that, the result is kept as undelivered, so a run aborted in between still reports it.
func (c *callRun) deliverNow(res CallResult) {
	c.e.undeliveredM.Lock()
	c.e.undelivered[c.id] = res
	c.e.undeliveredM.Unlock()
	c.complete(func() {
		c.e.undeliveredM.Lock()
		delete(c.e.undelivered, c.id)
		c.e.undeliveredM.Unlock()
		c.path.settle(c.position)
		c.e.results = append(c.e.results, res)
		if res.Handle != "" {
			c.e.byHandle[res.Handle] = res
		}
		var v goja.Value
		if c.plain {
			v = c.s.fromJSON(res.Output)
		} else {
			v = c.s.callResultObject(res)
		}
		if err := c.resolve(v); err != nil {
			c.e.failResolve(err)
		}
	})
}

// callResultObject builds the frozen CallResult a script holds and remembers it.
func (s *scriptVM) callResultObject(res CallResult) goja.Value {
	vm := s.vm
	meta := s.fromJSON(mustJSON(res.Meta))
	obj := vm.NewObject()
	_ = obj.Set("callId", res.CallID)
	if res.Handle != "" {
		_ = obj.Set("handle", res.Handle)
	} else {
		_ = obj.Set("handle", goja.Null())
	}
	_ = obj.Set("status", res.Status)
	_ = obj.Set("output", s.fromJSON(res.Output))
	if res.Reason != "" {
		_ = obj.Set("reason", res.Reason)
	} else {
		_ = obj.Set("reason", goja.Null())
	}
	_ = obj.Set("meta", meta)
	if mo, ok := meta.(*goja.Object); ok {
		_, _ = s.freeze(goja.Undefined(), mo)
	}
	_, _ = s.freeze(goja.Undefined(), obj)
	s.results[obj] = &resultRef{res: res}
	return obj
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}

// gate stops a call that would send a request while the run is paused or over its token ceiling.
func (c *callRun) gate() (CallResult, bool) {
	reason := c.e.pauseReason()
	if reason == "" {
		return CallResult{}, false
	}
	state := "not_delivered"
	if c.delivered {
		state = "delivered"
	}
	if reason != ReasonBudgetExhausted {
		reason = ReasonEnginePaused
	}
	return c.terminal(EvHeld, StatusHeld, reason, map[string]any{"input_state": state}), true
}

// aborted reports whether the run ended and cancelled the call.
func (c *callRun) aborted(ctx context.Context) bool {
	return ctx.Err() != nil
}

// fromError ends the call with the disposition of a request that failed.
func (c *callRun) fromError(ctx context.Context, err error) CallResult {
	var rpc *RPCError
	switch {
	case c.aborted(ctx):
		return c.terminal(EvSuperseded, StatusSuperseded, ReasonCancelled, nil)
	case errors.As(err, &rpc):
		return c.fromDisposition(ClassifyHostFailure(rpc.CodexErrorInfo), rpc.Message)
	}
	return c.terminal(EvOutcomeUnknown, StatusUnknown, ReasonDispatchUnknown, map[string]any{"error": err.Error()})
}

func (c *callRun) fromDisposition(d Disposition, detail string) CallResult {
	data := map[string]any{"codex_error_info": d.CodexErrorInfo, "detail": detail}
	switch d.Status {
	case StatusHeld:
		state := "not_delivered"
		if c.delivered {
			state = "delivered"
		}
		data["input_state"] = state
		if PausesRun(d.Reason) {
			c.e.pause(ReasonEnginePaused)
		}
		return c.terminal(EvHeld, StatusHeld, d.Reason, data)
	case StatusRefused:
		return c.terminal(EvRefused, StatusRefused, d.Reason, data)
	}
	return c.terminal(EvNullModel, StatusNullModel, d.Reason, data)
}

const repairInstruction = "Your previous answer did not satisfy the output schema: %s\nAnswer again with only a JSON value that satisfies the schema."

// execute runs the call against the backend: thread, turns, schema validation with one repair turn.
func (c *callRun) execute(ctx context.Context) CallResult {
	e, p := c.e, c.plan
	select {
	case e.pool <- struct{}{}:
		defer func() { <-e.pool }()
	case <-ctx.Done():
		return c.terminal(EvSuperseded, StatusSuperseded, ReasonCancelled, nil)
	}
	var resolved *jsonschema.Resolved
	if p.schema != nil {
		var sch jsonschema.Schema
		if err := json.Unmarshal(p.schema, &sch); err == nil {
			resolved, err = sch.Resolve(nil)
			if err != nil {
				resolved = nil
			}
		}
		if resolved == nil {
			return c.terminal(EvRefused, StatusRefused, ReasonSchemaNotStrict, map[string]any{"detail": "the schema cannot be compiled"})
		}
	}
	if res, stop := c.gate(); stop {
		return res
	}
	if p.resume == nil {
		c.event(EvThreadRequested, map[string]any{"cwd": p.opts.Cwd})
		info, err := e.cfg.Backend.StartThread(ctx, ThreadSpec{CallID: c.id, Role: p.role, Pair: p.spec.Pair, Cwd: p.opts.Cwd})
		if err != nil {
			return c.fromError(ctx, err)
		}
		c.threadID, c.configured = info.ID, info.Pair
		c.event(EvThreadStarted, map[string]any{"thread_id": info.ID, "model": info.Pair.Model, "effort": info.Pair.Effort})
		if info.Pair != p.spec.Pair {
			return c.terminal(EvRefused, StatusRefused, ReasonPairRefused, map[string]any{
				"detail": fmt.Sprintf("the thread reads back %s/%s, the role's pair is %s/%s", info.Pair.Model, info.Pair.Effort, p.spec.Pair.Model, p.spec.Pair.Effort)})
		}
	} else {
		c.threadID, c.configured = p.resume.ThreadID, p.resume.Configured
	}

	input := p.prompt
	for k := 1; ; k++ {
		if res, stop := c.gate(); stop {
			return res
		}
		op := fmt.Sprintf("%s/t%d", c.id, k)
		c.event(EvTurnRequested, map[string]any{"k": k, "op": op})
		repair := k > 1
		out, err := e.cfg.Backend.RunTurn(ctx, TurnSpec{
			CallID: c.id, Op: op, K: k, ThreadID: c.threadID, Role: p.role, Pair: p.spec.Pair,
			Input: "crw-flow-op: " + op + "\n\n" + input, Schema: p.schema, Cwd: p.opts.Cwd, Repair: repair,
			Started: func(turnID string) {
				c.turnIDs = append(c.turnIDs, turnID)
				c.delivered = true
				c.event(EvTurnStarted, map[string]any{"k": k, "op": op, "turn_id": turnID})
			},
		})
		if err != nil {
			return c.fromError(ctx, err)
		}
		c.tokens.Input += out.Tokens.Input
		c.tokens.Output += out.Tokens.Output
		c.tokens.Total += out.Tokens.Total
		e.addTokens(out.Tokens.Total)
		if f := out.Failed; f != nil {
			if c.aborted(ctx) {
				return c.terminal(EvSuperseded, StatusSuperseded, ReasonCancelled, nil)
			}
			if f.Interrupted && f.CodexErrorInfo == "" {
				return c.fromDisposition(Disposition{Status: StatusHeld, Reason: ReasonHostInterruptedExternal}, f.Message)
			}
			return c.fromDisposition(ClassifyHostFailure(f.CodexErrorInfo), f.Message)
		}
		result, verr := c.validate(out.Text, resolved)
		if verr == nil {
			return c.terminal(EvCompleted, StatusCompleted, "", map[string]any{
				"result": string(result), "result_digest": digestJSON(result), "tokens": c.tokens, "repair_used": c.repairUsed})
		}
		if repair {
			return c.terminal(EvNullModel, StatusNullModel, ReasonSchemaInvalidAfterRepair, map[string]any{"detail": verr.Error(), "repair_used": true})
		}
		c.repairUsed = true
		input = fmt.Sprintf(repairInstruction, verr.Error())
	}
}

// validate parses a turn's last message and, for a call with a schema, validates it in Go. The returned bytes are the
// canonical JSON of the result (byte-identical on replay).
func (c *callRun) validate(text string, resolved *jsonschema.Resolved) ([]byte, error) {
	if resolved == nil {
		return mustJSON(text), nil
	}
	var plain any
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(text)))
	if err := dec.Decode(&plain); err != nil {
		return nil, fmt.Errorf("the message is not JSON: %v", err)
	}
	// The message is one JSON document: whatever follows it (a closing bracket, a second value, text) is an error.
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("the message holds more than one JSON value")
	}
	if err := resolved.Validate(plain); err != nil {
		return nil, err
	}
	return json.Marshal(plain)
}
