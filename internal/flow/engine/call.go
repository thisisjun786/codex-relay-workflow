package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

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
	// explicitLabel is whether the script gave the call a label of its own (lint judges label uniqueness on those).
	explicitLabel bool
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

	p.explicitLabel = p.opts.Label != ""
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
		data := map[string]any{"detail": p.refusal.Detail}
		if p.refusal.Cause != "" {
			data["cause"] = p.refusal.Cause
		}
		c.deliverNow(c.terminal(EvRefused, StatusRefused, p.refusal.Reason, data))
	} else if c.replay != nil {
		c.deliverNow(*c.replay)
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
			p.refusal.Cause = ref.Cause
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
	// A thread keeps the pair it was created with: a continuation runs on the handle's pair even when the policy the
	// run now reads (policy_repinned) gives the role another one. Only a new thread takes the new pair.
	if info.Pair != (Pair{}) {
		spec.Pair = info.Pair
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
				p.refusal.Cause = ref.Cause
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

	// placement is where the call runs when the backend places calls (Placer); placeReq is what it was asked.
	placement *Placement
	placeReq  PlaceRequest
	// cwd is the working directory of the call's thread once it is known.
	cwd string
	// rec is what reconcile decided for the call of a resumed run; replay is the result of a call the journal already
	// ended with the same input; send is the send ordinal of the creation request (0 is the first).
	rec    *Recovery
	replay *CallResult
	send   int

	// mu guards what an interrupt from another goroutine reads and writes: the last row, the turn in flight and the
	// engine's intent to interrupt it.
	mu         sync.Mutex
	lastSeq    int
	lastEvent  string
	op         string
	cancelTurn context.CancelCauseFunc
	intent     string
}

func (c *callRun) event(ev string, data map[string]any) Event {
	row := c.e.j.Append(Event{RunID: c.e.runID, Kind: KindCall, CallID: c.id, Path: c.path.id, Position: c.position, Op: "agent", Label: c.label, Event: ev, Data: data})
	c.mu.Lock()
	c.lastSeq, c.lastEvent = row.Seq, ev
	c.mu.Unlock()
	return row
}

// prepare writes the call's intent row before anything is sent.
func (c *callRun) prepare() {
	p := c.plan
	if c.e.cfg.Resume != nil && p.refusal == nil && c.resumePrior() {
		return
	}
	d := map[string]any{
		"role": p.role, "family": p.spec.Family, "model": p.spec.Pair.Model, "effort": p.spec.Pair.Effort,
		"requested_model": p.requested.Model, "requested_effort": p.requested.Effort,
		"node": p.node, "step_decision": p.step, "phase": p.opts.Phase, "head": p.opts.Head, "cwd": p.opts.Cwd,
		"input_digest": digestJSON([]byte(p.prompt)), "plain": c.plain, "sandbox": p.spec.Sandbox,
		"explicit_label": p.explicitLabel, "policy_digest": c.e.cfg.Policy.Digest,
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
		if p.resume.Cwd != "" {
			d["thread_cwd"] = p.resume.Cwd
			c.cwd = p.resume.Cwd
		}
	}
	if placer, ok := c.e.cfg.Backend.(Placer); ok && p.refusal == nil && !c.e.nodeStopped(p.node) {
		c.placeReq = PlaceRequest{RunID: c.e.runID, CallID: c.id, Role: p.role, Family: p.spec.Family, Node: p.node, Head: p.opts.Head, Continues: p.resume, Journal: c.e.j}
		pl, err := placer.Place(c.placeReq)
		if err != nil {
			p.refusal = &Refusal{Reason: ReasonWorktreeFailed, Detail: err.Error()}
		} else {
			c.placement = &pl
			d["allocation"] = pl.Map()
		}
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
	return c.result(status, reason, row.Seq, data)
}

// leaveOpen ends the call for the script without a terminal row: its last row stays the intent that was sent, and the
// reconcile pass (or a resume) decides the call.
func (c *callRun) leaveOpen(reason string) CallResult {
	c.mu.Lock()
	seq := c.lastSeq
	c.mu.Unlock()
	return c.result(StatusUnknown, reason, seq, nil)
}

// result is the CallResult of a call whose last row is endSeq.
func (c *callRun) result(status, reason string, endSeq int, data map[string]any) CallResult {
	startSeq := c.startSeq
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
func (c *callRun) gate(ctx context.Context) (CallResult, bool) {
	if reason := c.e.nodeStopReason(c.plan.node); reason != "" {
		detail := "the node was stopped: a restored worktree changed after it was verified"
		if reason == ReasonRecoveryHold {
			detail = "the node is in recovery_hold: a call of it has an outcome that is not known, so nothing more is sent for it"
		}
		data := map[string]any{"input_state": c.inputState(), "detail": detail}
		c.heldSnapshot(ctx, data)
		return c.terminal(EvHeld, StatusHeld, reason, data), true
	}
	reason := c.e.pauseReason()
	if reason == "" {
		return CallResult{}, false
	}
	data := map[string]any{"input_state": c.inputState()}
	if reason != ReasonBudgetExhausted {
		reason = ReasonEnginePaused
	}
	c.heldSnapshot(ctx, data)
	return c.terminal(EvHeld, StatusHeld, reason, data), true
}

func (c *callRun) inputState() string {
	if c.delivered {
		return "delivered"
	}
	return "not_delivered"
}

// aborted reports whether the run ended and cancelled the call.
func (c *callRun) aborted(ctx context.Context) bool {
	return ctx.Err() != nil
}

// heldSnapshot adds P_held to the data of a held row: the working snapshot of an executor call's worktree at the moment
// the run classified the failure (for a call that sent nothing, the snapshot it started from).
func (c *callRun) heldSnapshot(ctx context.Context, data map[string]any) {
	placer, ok := c.e.cfg.Backend.(Placer)
	if !ok || c.cwd == "" || c.plan.spec.Family != "executor" {
		return
	}
	snap, err := placer.Snapshot(ctx, c.cwd)
	if err != nil {
		data["p_held_error"] = err.Error()
		return
	}
	data["p_held"] = snap
}

// fromError ends the call with the disposition of a request that failed.
func (c *callRun) fromError(ctx context.Context, err error) CallResult {
	var rpc *RPCError
	switch {
	case c.aborted(ctx):
		return c.terminal(EvSuperseded, StatusSuperseded, ReasonCancelled, nil)
	case c.interrupted() != "":
		return c.fromIntent(ctx, err.Error())
	case errors.As(err, &rpc):
		return c.fromDisposition(ctx, ClassifyHostFailure(rpc.CodexErrorInfo), rpc.Message)
	}
	// A request nobody can account for holds the node: nothing is sent again, no new attempt starts and no tier step is
	// taken for it until reconcile or an operator establishes the outcome.
	c.e.stopNode(c.plan.node, ReasonRecoveryHold)
	c.e.escalateUnknown(c, err.Error())
	if r, ok := c.e.cfg.Backend.(Reconciled); ok && r.LeavesLostRequestsOpen() {
		return c.leaveOpen(ReasonDispatchUnknown)
	}
	return c.terminal(EvOutcomeUnknown, StatusUnknown, ReasonDispatchUnknown, map[string]any{"error": err.Error()})
}

// fromIntent ends a call whose turn the engine itself interrupted: it is held for the reason of the intent, whatever the
// turn came back as.
func (c *callRun) fromIntent(ctx context.Context, detail string) CallResult {
	return c.fromDisposition(ctx, Disposition{Status: StatusHeld, Reason: c.interrupted()}, detail)
}

// quotaEscalation is written once per run, by the call whose failure paused it.
func (e *Engine) quotaEscalation(c *callRun, info string) {
	if !e.quotaEscalated.CompareAndSwap(false, true) {
		return
	}
	e.j.Append(Event{RunID: e.runID, Kind: KindFlow, CallID: c.id, Path: c.path.id, Position: c.position, Op: "agent", Label: c.label, Event: "escalation",
		Data: map[string]any{"reason": "quota_or_rate", "node": c.plan.node, "call_id": c.id, "codex_error_info": info}})
}

func (c *callRun) fromDisposition(ctx context.Context, d Disposition, detail string) CallResult {
	data := map[string]any{"codex_error_info": d.CodexErrorInfo, "detail": detail}
	switch d.Status {
	case StatusHeld:
		data["input_state"] = c.inputState()
		c.heldSnapshot(ctx, data)
		if PausesRun(d.Reason) {
			c.e.quotaEscalation(c, d.CodexErrorInfo)
			c.e.pause(ReasonEnginePaused)
			c.e.interruptTurns(ReasonEnginePaused)
		}
		return c.terminal(EvHeld, StatusHeld, d.Reason, data)
	case StatusRefused:
		return c.terminal(EvRefused, StatusRefused, d.Reason, data)
	}
	return c.terminal(EvNullModel, StatusNullModel, d.Reason, data)
}

// beginTurn opens the context one turn request runs under; the engine's interrupt cancels it.
func (c *callRun) beginTurn(ctx context.Context, op string) (context.Context, func()) {
	turnCtx, cancel := context.WithCancelCause(ctx)
	c.mu.Lock()
	c.cancelTurn, c.op, c.intent = cancel, op, ""
	c.mu.Unlock()
	c.e.turnsM.Lock()
	c.e.turns[c] = struct{}{}
	c.e.turnsM.Unlock()
	return turnCtx, func() {
		c.e.turnsM.Lock()
		delete(c.e.turns, c)
		c.e.turnsM.Unlock()
		c.mu.Lock()
		c.cancelTurn = nil
		c.mu.Unlock()
		cancel(nil)
	}
}

// interrupted is the reason the engine interrupted the call's last turn for, "" when it did not.
func (c *callRun) interrupted() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.intent
}

// interrupt journals the engine's intent to interrupt the turn in flight and then cancels it: the backend sends the
// host's interrupt, and the turn that comes back interrupted is classified by this intent, never as a null_model.
func (c *callRun) interrupt(reason string) {
	c.mu.Lock()
	if c.cancelTurn == nil || c.intent != "" {
		c.mu.Unlock()
		return
	}
	c.intent = reason
	cancel, op := c.cancelTurn, c.op
	c.mu.Unlock()
	c.event(EvInterruptRequested, map[string]any{"reason": reason, "op": op})
	cancel(errInterrupt)
}

var errInterrupt = errors.New("interrupted by the engine")

// interruptTurns interrupts the turn every call in flight is running.
func (e *Engine) interruptTurns(reason string) {
	e.turnsM.Lock()
	calls := make([]*callRun, 0, len(e.turns))
	for c := range e.turns {
		calls = append(calls, c)
	}
	e.turnsM.Unlock()
	for _, c := range calls {
		c.interrupt(reason)
	}
}

// placementProblem ends a call whose worktree could not be made. A restored worktree that changed since it was verified
// stops the node: nothing more is sent, the call keeps the row it had, and an escalation names it.
func (c *callRun) placementProblem(ctx context.Context, err error) CallResult {
	var mismatch *SnapshotMismatch
	switch {
	case c.aborted(ctx):
		return c.terminal(EvSuperseded, StatusSuperseded, ReasonCancelled, nil)
	case errors.As(err, &mismatch):
		c.e.stopNode(c.plan.node, ReasonRestoreSnapshotMismatch)
		c.mu.Lock()
		last := c.lastEvent
		c.mu.Unlock()
		c.e.j.Append(Event{RunID: c.e.runID, Kind: KindFlow, CallID: c.id, Path: c.path.id, Position: c.position, Op: "agent", Label: c.label, Event: "escalation",
			Data: map[string]any{"reason": ReasonRestoreSnapshotMismatch, "node": c.plan.node, "call_id": c.id, "last_event": last, "verified": mismatch.Want, "live": mismatch.Got}})
		return c.leaveOpen(ReasonRestoreSnapshotMismatch)
	}
	return c.terminal(EvHeld, StatusHeld, ReasonWorktreeFailed, map[string]any{"input_state": "not_delivered", "detail": err.Error()})
}

// establish makes the call's placement real, after its prepared row.
func (c *callRun) establish(ctx context.Context) (CallResult, bool) {
	placer, ok := c.e.cfg.Backend.(Placer)
	if !ok || c.placement == nil || c.placement.Path == "" || c.placement.Kind == PlaceContinued {
		return CallResult{}, false
	}
	if err := placer.Establish(ctx, c.placeReq, *c.placement); err != nil {
		return c.placementProblem(ctx, err), true
	}
	c.cwd = c.placement.Path
	return CallResult{}, false
}

// pairCheck ends a call whose new thread read back another pair than its role's.
func (c *callRun) pairCheck(info ThreadInfo) (CallResult, bool) {
	p := c.plan
	if info.Pair == p.spec.Pair {
		return CallResult{}, false
	}
	return c.terminal(EvRefused, StatusRefused, ReasonPairRefused, map[string]any{
		"detail": fmt.Sprintf("the thread reads back %s/%s, the role's pair is %s/%s", info.Pair.Model, info.Pair.Effort, p.spec.Pair.Model, p.spec.Pair.Effort)}), true
}

// startThread creates the call's thread.
func (c *callRun) startThread(ctx context.Context) (CallResult, bool) {
	e, p := c.e, c.plan
	if c.rec == nil {
		// a creation sent again runs in the directory the first request was made for: it exists, and nothing is made
		if res, stop := c.establish(ctx); stop {
			return res, true
		}
	}
	cwd := p.opts.Cwd
	var placement Placement
	if c.placement != nil {
		placement = *c.placement
		if placement.Path != "" {
			cwd = placement.Path
		}
	}
	send := max(1, c.send)
	c.event(EvThreadRequested, c.creationIntent(map[string]any{"cwd": cwd}, send))
	info, err := e.cfg.Backend.StartThread(ctx, ThreadSpec{CallID: c.id, Role: p.role, Pair: p.spec.Pair, Sandbox: p.spec.Sandbox, Cwd: cwd, Node: p.node, Head: p.opts.Head, Placement: placement, AutoCompactTokenLimit: p.spec.AutoCompactTokenLimit, Send: send})
	if err != nil {
		return c.fromError(ctx, err), true
	}
	c.threadID, c.configured = info.ID, info.Pair
	if info.Cwd != "" {
		c.cwd = info.Cwd
	} else if c.cwd == "" {
		c.cwd = cwd
	}
	c.event(EvThreadStarted, map[string]any{"thread_id": info.ID, "model": info.Pair.Model, "effort": info.Pair.Effort, "cwd": info.Cwd})
	return c.pairCheck(info)
}

// forkThread forks the handle's thread at its last turn into the restored worktree the call adopted.
func (c *callRun) forkThread(ctx context.Context) (CallResult, bool) {
	e, p := c.e, c.plan
	forker, ok := e.cfg.Backend.(Forker)
	if !ok {
		return c.terminal(EvRefused, StatusRefused, ReasonPolicyRefused, map[string]any{"detail": "the backend cannot fork a thread"}), true
	}
	if c.rec == nil {
		if res, stop := c.establish(ctx); stop {
			return res, true
		}
	}
	cwd := c.placement.Path
	send := max(1, c.send)
	c.event(EvForkRequested, c.creationIntent(map[string]any{"thread_id": p.resume.ThreadID, "through_turn_id": p.resume.ThroughTurnID, "prefix": threadPrefix(e.j, p.resume.ThreadID, p.resume.ThroughTurnID), "cwd": cwd}, send))
	info, err := forker.ForkThread(ctx, ForkSpec{CallID: c.id, Role: p.role, Node: p.node, Pair: p.spec.Pair, Sandbox: p.spec.Sandbox, Cwd: cwd,
		SourceThreadID: p.resume.ThreadID, ThroughTurnID: p.resume.ThroughTurnID, AutoCompactTokenLimit: p.spec.AutoCompactTokenLimit, Send: send})
	if err != nil {
		return c.fromError(ctx, err), true
	}
	c.threadID, c.configured = info.ID, info.Pair
	if info.Cwd != "" {
		c.cwd = info.Cwd
	}
	c.event(EvForkStarted, map[string]any{"thread_id": info.ID, "model": info.Pair.Model, "effort": info.Pair.Effort, "cwd": info.Cwd})
	return c.pairCheck(info)
}

// threadPrefix is the turns of a thread through throughTurn, in order, as the journal recorded them: each turn's id and
// its `<call_id>/t<k>` marker. A fork's prefix is what the forked thread must hold.
func threadPrefix(j Journal, threadID, throughTurn string) []map[string]any {
	owner := map[string]string{}
	events := j.Events()
	for _, ev := range events {
		if ev.Kind != KindCall {
			continue
		}
		switch ev.Event {
		case EvPrepared:
			if t, _ := ev.Data["thread_id"].(string); t != "" {
				owner[ev.CallID] = t
			}
		case EvThreadStarted, EvForkStarted:
			owner[ev.CallID], _ = ev.Data["thread_id"].(string)
		}
	}
	var out []map[string]any
	for _, ev := range events {
		if ev.Kind != KindCall || ev.Event != EvTurnStarted || owner[ev.CallID] != threadID {
			continue
		}
		id, _ := ev.Data["turn_id"].(string)
		op, _ := ev.Data["op"].(string)
		out = append(out, map[string]any{"turn_id": id, "op": op})
		if id == throughTurn {
			break
		}
	}
	return out
}

const repairInstruction = "Your previous answer did not satisfy the output schema: %s\nAnswer again with only a JSON value that satisfies the schema."

// creationIntent adds to a thread_requested or fork_requested row what reconcile needs: the send ordinal, the intended
// wall time (the identity of a thread is bounded by it) and the process that holds the connection the request goes on.
func (c *callRun) creationIntent(data map[string]any, send int) map[string]any {
	data["send"] = send
	data["at"] = c.e.cfg.Now().UnixMilli()
	if o, ok := c.e.cfg.Backend.(Owned); ok {
		data["owner"] = o.Owner()
	}
	return data
}

// recoveredTurn is a turn of a recovered call that was already sent: the loop takes its outcome instead of sending.
type recoveredTurn struct {
	k       int
	op      string
	turnID  string
	running bool
	outcome *TurnOutcome
	intent  string
}

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
	if c.rec != nil && c.rec.Action == RecoverPending {
		return c.leaveOpen(ReasonDispatchUnknown)
	}
	if res, stop := c.gate(ctx); stop {
		return res
	}
	k, input := 1, p.prompt
	var pre *recoveredTurn
	if c.rec != nil {
		var res CallResult
		var stop bool
		if res, stop, k, input, pre = c.recover(ctx); stop {
			return res
		}
	} else {
		if res, stop := c.supersedePrior(ctx); stop {
			return res
		}
		switch {
		case p.resume == nil:
			if res, stop := c.startThread(ctx); stop {
				return res
			}
		case c.placement != nil && c.placement.Fork:
			if res, stop := c.forkThread(ctx); stop {
				return res
			}
		default:
			c.threadID, c.configured = p.resume.ThreadID, p.resume.Configured
		}
	}
	return c.turnLoop(ctx, resolved, k, input, pre)
}

// recover goes on with a call whose reconcile decided it: the thread it adopted, the creation it sends again, the turn
// that is its own, or the turn that was proven absent. It returns the turn the loop begins with.
func (c *callRun) recover(ctx context.Context) (res CallResult, stop bool, k int, input string, pre *recoveredTurn) {
	rec, p := c.rec, c.plan
	switch rec.Action {
	case RecoverThreadReady:
		c.threadID, c.configured, c.cwd = rec.ThreadID, rec.Pair, rec.Cwd
		res, stop = c.pairCheck(ThreadInfo{ID: rec.ThreadID, Pair: rec.Pair, Cwd: rec.Cwd})
		return res, stop, 1, p.prompt, nil
	case RecoverResendCreate:
		c.send = rec.Send
		if rec.Kind == "fork" {
			res, stop = c.forkThread(ctx)
		} else {
			res, stop = c.startThread(ctx)
		}
		return res, stop, 1, p.prompt, nil
	}
	info, _ := LookupCall(c.e.j, c.id)
	c.threadID, c.configured, c.cwd = info.ThreadID, info.Configured, info.Cwd
	for _, ev := range CallEvents(c.e.j, c.id) {
		if ev.Event == EvPrepared {
			c.turnIDs = nil
		}
		if ev.Event == EvTurnStarted {
			c.turnIDs = append(c.turnIDs, rowStr(ev.Data, "turn_id"))
			c.delivered = true
		}
	}
	if rec.Action == RecoverResendTurn {
		return CallResult{}, false, rec.K, rec.Input, nil
	}
	return CallResult{}, false, rec.K, rec.Input, &recoveredTurn{k: rec.K, op: rec.Op, turnID: rec.TurnID, running: rec.Running, outcome: rec.Outcome, intent: rec.InterruptReason}
}

// turnLoop runs the turns of a call from turn k: each is sent after its intent row, and a turn that was already sent (pre)
// is taken from the host instead.
func (c *callRun) turnLoop(ctx context.Context, resolved *jsonschema.Resolved, k int, input string, pre *recoveredTurn) CallResult {
	e, p := c.e, c.plan
	resend := c.rec != nil && c.rec.Action == RecoverResendTurn
	for ; ; k++ {
		var out TurnOutcome
		var err error
		repair := k > 1
		if pre != nil {
			// The turn was sent before the process stopped: no request, no gate, no intent row.
			repair = pre.k > 1
			c.repairUsed = c.repairUsed || repair
			if pre.intent != "" {
				c.mu.Lock()
				c.intent = pre.intent
				c.mu.Unlock()
			}
			if pre.running {
				host, _ := e.cfg.Backend.(ReconHost)
				turnCtx, endTurn := c.beginTurn(ctx, pre.op)
				out, err = host.AwaitTurn(turnCtx, c.threadID, pre.turnID, pre.op)
				endTurn()
			} else {
				out = *pre.outcome
			}
			pre = nil
		} else {
			if res, stop := c.gate(ctx); stop {
				return res
			}
			if k == 1 && c.placement != nil && c.placement.Kind == PlaceRestore {
				if placer, ok := e.cfg.Backend.(Placer); ok {
					if err := placer.Recheck(ctx, *c.placement); err != nil {
						return c.placementProblem(ctx, err)
					}
				}
			}
			op := fmt.Sprintf("%s/t%d", c.id, k)
			intent := map[string]any{"k": k, "op": op, "input": input}
			if resend {
				intent["resend"] = true
				resend = false
			}
			c.event(EvTurnRequested, intent)
			c.repairUsed = c.repairUsed || repair
			turnCtx, endTurn := c.beginTurn(ctx, op)
			out, err = e.cfg.Backend.RunTurn(turnCtx, TurnSpec{
				CallID: c.id, Op: op, K: k, ThreadID: c.threadID, Role: p.role, Node: p.node, Pair: p.spec.Pair,
				Input: "crw-flow-op: " + op + "\n\n" + input, Schema: p.schema, Cwd: c.cwd, Repair: repair,
				Started: func(turnID string) {
					c.turnIDs = append(c.turnIDs, turnID)
					c.delivered = true
					c.event(EvTurnStarted, map[string]any{"k": k, "op": op, "turn_id": turnID})
				},
			})
			endTurn()
		}
		if err != nil {
			return c.fromError(ctx, err)
		}
		c.tokens.Input += out.Tokens.Input
		c.tokens.Output += out.Tokens.Output
		c.tokens.Total += out.Tokens.Total
		e.addTokens(out.Tokens.Total)
		if out.TokensUnmeasured {
			e.tokensUnmeasured(c, out.TurnID)
		}
		if f := out.Failed; f != nil {
			if c.aborted(ctx) {
				return c.terminal(EvSuperseded, StatusSuperseded, ReasonCancelled, nil)
			}
			if c.interrupted() != "" {
				return c.fromIntent(ctx, f.Message)
			}
			if f.Interrupted && f.CodexErrorInfo == "" {
				return c.fromDisposition(ctx, Disposition{Status: StatusHeld, Reason: ReasonHostInterruptedExternal}, f.Message)
			}
			return c.fromDisposition(ctx, ClassifyHostFailure(f.CodexErrorInfo), f.Message)
		}
		if c.interrupted() != "" {
			// The engine's own interrupt decides the call whatever the host made of the turn: a turn that completed before
			// the interrupt took effect is still held for the reason of the intent.
			if c.aborted(ctx) {
				return c.terminal(EvSuperseded, StatusSuperseded, ReasonCancelled, nil)
			}
			return c.fromIntent(ctx, "the turn ended completed after the engine interrupted it")
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
