package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

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
	// qa is whether the call is a QA capture call or a continuation of one: the mark of the option, or of the handle's call.
	qa bool
}

func (s *scriptVM) optBool(o *goja.Object, key string) bool {
	if o == nil {
		return false
	}
	v := o.Get(key)
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return false
	}
	b, ok := v.Export().(bool)
	if !ok {
		s.throwType("agent: option %s must be a boolean", key)
	}
	return b
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
		Label: s.optString(o, "label"), Phase: s.optString(o, "phase"), Head: s.optString(o, "head"), QA: s.optBool(o, "qa"),
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
	if s.e.cfg.Session != nil {
		c.sess = &sessionCall{attempt: 1}
	}
	if c.plan.opts.Phase == "" {
		c.plan.opts.Phase = s.e.phase
	}
	promise, resolve, _ := s.vm.NewPromise()
	c.resolve = resolve
	c.complete = s.e.loop.Begin(s.rt)
	c.prepare()
	if c.prepHold != nil {
		c.deliverNow(c.terminal(EvHeld, StatusHeld, c.prepHold.Reason, map[string]any{"input_state": "not_delivered", "detail": c.prepHold.Detail}))
	} else if p.refusal != nil {
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
			res := c.execute(s.e.ctx)
			c.removeOwnDirectory(res)
			c.sessionEnd(res)
			c.deliverNow(res)
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
			if p.opts.QA {
				if p.role != "executor" {
					refuse(ReasonPolicyRefused, "qa marks the QA capture call, which is an executor call, not "+p.role)
				}
				p.qa = true
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
	// the mark belongs to the thread: a continuation of a QA thread is a QA turn, and no other thread becomes one
	p.qa = info.QA
	if p.opts.QA && !info.QA {
		refuse(ReasonForeignHandle, "the handle is not a QA capture thread: qa marks the call that starts one")
		return
	}

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
	// In a session run a role whose effort is undecided leaves it to the host: the thread's effort is the one the host applied to the
	// call's subagent (its configured pair, effort_unchecked), so an explicit effort is compared with that (CRW-1392). The App Server
	// path binds a thread only on the role's whole pair, so its thread's effort is the role's.
	if ef := p.opts.Effort; ef != "" {
		threadEffort := spec.Pair.Effort
		if threadEffort == "" && e.cfg.Session != nil {
			threadEffort = info.Configured.Effort
		}
		if ef != threadEffort {
			refuse(ReasonPairRefused, "effort "+ef+" is not the thread's effort "+threadEffort)
			return
		}
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
	// continues nothing. A session run compares only the decided parts of the role's pair, at a bind and here: a model the
	// role leaves to the host (pair_unchecked) and an undecided effort (effort_unchecked) are not compared with what the host
	// read back, and the continuation's own bind records the pair its subagent runs.
	held := info.Configured == spec.Pair
	if e.cfg.Session != nil {
		held = sessionPairBinds(spec.Pair, info.Configured)
	}
	if !held {
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

	// The resume walk (F07). digest is the call's input digest (the context boundary and the joins before it included) and
	// snapshot its plan-owned input snapshot, both journaled in the prepared row; suffixCall is set once the call ran live or
	// went on in place, held is a held call that goes on in place, prepHold is the reason a call could not be made ready
	// (its boundary could not be restored), needEstablish is a call whose worktree was never made, firstK is the turn the
	// repair rule counts from.
	digest        string
	snapshot      string
	suffixCall    bool
	held          *heldResume
	prepHold      *Refusal
	needEstablish bool
	firstK        int
	// preface is the checkpoint packet a continuation that could not be forked starts its new thread with.
	preface string
	// sess is the state of a session call (Config.Session), nil for a call of the App Server path.
	sess *sessionCall

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
	if err := journalErr(c.e.j); err != nil {
		c.e.journalFailed(err)
	}
	return row
}

// journalDown ends a call whose intent row could not be stored, before the request that row announces is sent: a request
// whose intent is not durable could not be recovered. The call writes no row (nothing can be stored) and the run ends with
// the journal's error.
func (c *callRun) journalDown() (CallResult, bool) {
	err := journalErr(c.e.j)
	if err == nil {
		return CallResult{}, false
	}
	c.e.journalFailed(err)
	c.mu.Lock()
	seq := c.lastSeq
	c.mu.Unlock()
	return c.result(StatusSuperseded, ReasonCancelled, seq, nil), true
}

// prepare writes the call's intent row before anything is sent.
func (c *callRun) prepare() {
	p := c.plan
	var ctxb map[string]any
	c.digest, ctxb = c.inputDigest()
	c.snapshot = c.readSnapshot()
	if c.e.cfg.Resume != nil && c.resumePrior() {
		return
	}
	if reason, detail := c.restoreBoundary(); reason != "" {
		c.prepHold = &Refusal{Reason: reason, Detail: detail}
	}
	if c.sess != nil {
		// a call a resumed run prepares again: the flow evidence records of its ended attempts go with them, before the new prepared
		// row (an open attempt is superseded, and releases its records, once its subagents run nothing: endOpenCalls)
		if rows := c.e.prior.rowsOf(c.id, "agent"); len(rows) > 0 {
			if st := stateOf(c.id, rows); st.terminal != "" && st.terminal != EvOutcomeUnknown {
				c.e.evidenceRelease(rows)
			}
		}
	}
	d := map[string]any{
		"role": p.role, "family": p.spec.Family, "model": p.spec.Pair.Model, "effort": p.spec.Pair.Effort,
		"requested_model": p.requested.Model, "requested_effort": p.requested.Effort,
		"node": p.node, "step_decision": p.step, "phase": p.opts.Phase, "head": p.opts.Head, "cwd": p.opts.Cwd,
		"input_digest": c.digest, "prompt_digest": digestJSON([]byte(p.prompt)), "context": ctxb, "plain": c.plain, "sandbox": p.spec.Sandbox,
		"explicit_label": p.explicitLabel, "policy_digest": c.e.cfg.Policy.Digest,
	}
	if c.sess != nil {
		d["backend"] = BackendSession
		if c.evidenceGated() {
			// the directories the call's flow evidence records are written under, for a resumed run started elsewhere or one whose
			// owner read fails: EvidenceCwd, and the owner thread's cwd when the host reports one that differs (CRW-1482)
			ev := c.e.cfg.Session.EvidenceCwd
			d["evidence_cwd"] = ev
			cwds := []string{ev}
			if owner := c.e.evidenceOwnerCwd(c.e.ctx); owner != "" && filepath.Clean(owner) != filepath.Clean(ev) {
				cwds = append(cwds, owner)
			}
			d["evidence_cwds"] = cwds
		}
	}
	if c.snapshot != "" {
		d["input_snapshot"] = c.snapshot
	}
	if p.schema != nil {
		d["schema_digest"] = digestJSON(p.schema)
	}
	if p.qa {
		d["qa"] = true
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
	if placer, ok := c.e.cfg.Backend.(Placer); ok && p.refusal == nil && c.prepHold == nil && !c.e.nodeStopped(p.node) {
		c.placeReq = PlaceRequest{RunID: c.e.runID, CallID: c.id, Role: p.role, Family: p.spec.Family, Node: p.node, Head: p.opts.Head, QA: p.qa, Continues: p.resume, Journal: c.e.j,
			Fork: c.forksInSuffix()}
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

// forksInSuffix is whether a continuation runs on a fork: inside a suffix a continuation of a thread the previous process
// started never sends a turn to that thread, whose cwd belongs to a worktree that was retired.
func (c *callRun) forksInSuffix() bool {
	e, p := c.e, c.plan
	return e.cfg.Resume != nil && c.suffixCall && p.resume != nil && e.threadIsPrior(p.resume.ThreadID)
}

// threadIsPrior is whether a thread was started by an earlier process of the run.
func (e *Engine) threadIsPrior(threadID string) bool {
	return e.prior != nil && threadID != "" && e.prior.threads[threadID]
}

// terminal journals the call's last event and returns its result.
func (c *callRun) terminal(ev, status, reason string, data map[string]any) CallResult {
	if data == nil {
		data = map[string]any{}
	}
	data["reason"] = reason
	// what the call used and whether it repaired are part of its result: every terminal row names them, so the result is
	// restored from the call's rows alone (a held call has used tokens too)
	if _, ok := data["tokens"]; !ok && c.tokens != (Tokens{}) {
		data["tokens"] = c.tokens
	}
	if _, ok := data["repair_used"]; !ok && c.repairUsed {
		data["repair_used"] = true
	}
	// the directory of a pinned architect turn is the call's own and goes with its end: the terminal row names it, so its removal is
	// decided by the row that ends the call - whatever happens after the row (a cancellation, a resume that replays the call), the
	// journal says the directory is to go (removeOwnDirectory, sweepOwnDirectories). A call whose outcome is unknown names none: a turn
	// of it may still be at work there.
	if status != StatusUnknown && c.ownsDirectory() {
		data[ownDirectoryKey] = c.placement.Path
	}
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
	if !ok || c.cwd == "" || !c.writesNodeWorktree() {
		return
	}
	snap, err := placer.Snapshot(ctx, c.cwd)
	if err != nil {
		data["p_held_error"] = err.Error()
		return
	}
	data["p_held"] = snap
}

// writesNodeWorktree is whether the call's edits are the state of a node: an executor call that is not the QA capture call, whose
// scratch worktree is discarded when the call ends and holds no pending tree.
func (c *callRun) writesNodeWorktree() bool {
	return c.plan.spec.Family == "executor" && !c.plan.qa
}

// pendingTree adds the pending tree P to the data of a completed executor call: the working snapshot of the call's worktree
// at the moment its turn ended, which crw.commit later commits as it is and which the effect proof of the call compares
// the worktree with. A call that runs in no worktree of the backend's (a reviewer, a backend without a placer) has none.
// It returns the error of a snapshot that could not be made; the call then has no pending tree to record.
func (c *callRun) pendingTree(ctx context.Context, data map[string]any) error {
	placer, ok := c.e.cfg.Backend.(Placer)
	if !ok || c.cwd == "" || !c.writesNodeWorktree() {
		return nil
	}
	snap, err := placer.Snapshot(ctx, c.cwd)
	if err != nil {
		data["pending_tree_error"] = err.Error()
		return err
	}
	data["pending_tree"] = snap
	return nil
}

// qaSource adds the source identity of the directory a QA call ended in to the data of its completed row (qa_source). The call
// edits a scratch worktree that is discarded and holds no pending tree, so this is the only record of what it exercised: the
// receipt of the final gate compares it with the clean head the gate pinned. A source that cannot be read is recorded as the error
// (qa_source_error), which the receipt reads as INCONCLUSIVE; a backend that cannot read one records nothing.
func (c *callRun) qaSource(ctx context.Context, data map[string]any) {
	if !c.plan.qa {
		return
	}
	reader, ok := c.e.cfg.Backend.(SourceReader)
	if !ok {
		return
	}
	id, err := reader.SourceOf(ctx, c.cwd, c.plan.opts.Head)
	if err != nil {
		data["qa_source_error"] = err.Error()
		return
	}
	// stored as the JSON object a journal row holds, so a replayed row and a live one read the same
	var obj map[string]any
	if err := json.Unmarshal(mustJSON(id), &obj); err != nil {
		data["qa_source_error"] = err.Error()
		return
	}
	data["qa_source"] = obj
}

// pendingTreeLost ends an executor call whose turn completed but whose worktree could not be snapshotted: the exact-tree
// commit and the effect proof need the pending tree, so the call is not reported completed. It is held for the worktree
// failure with the error on the row (pending_tree_error); the snapshot is tried once more as P_held, which stands in for the
// pending tree when git recovers. A run that was cancelled ends the call superseded, as every other call.
func (c *callRun) pendingTreeLost(ctx context.Context, err error, data map[string]any) CallResult {
	if c.aborted(ctx) {
		return c.terminal(EvSuperseded, StatusSuperseded, ReasonCancelled, nil)
	}
	held := map[string]any{"input_state": c.inputState(), "detail": "the pending tree could not be captured: " + err.Error(), "pending_tree_error": data["pending_tree_error"]}
	c.heldSnapshot(ctx, held)
	return c.terminal(EvHeld, StatusHeld, ReasonWorktreeFailed, held)
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
	if r, ok := c.e.cfg.Backend.(Reconciled); ok && r.LeavesLostRequestsOpen() {
		// reconcile decides the call and escalates if it cannot; until then the outcome is open, not unknown
		return c.leaveOpen(ReasonDispatchUnknown)
	}
	c.e.escalateUnknown(c, err.Error())
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

// setIntent records the reason of an interrupt the engine requested for the turn before the process stopped.
func (c *callRun) setIntent(reason string) {
	if reason == "" {
		return
	}
	c.mu.Lock()
	c.intent = reason
	c.mu.Unlock()
}

// interrupted is the reason the engine interrupted the call's last turn for, "" when it did not: this process's interrupt of the
// turn in flight (its context, which beginTurn opens with no intent), or for a session call the interrupt the previous process asked
// for when the turn the call is on is the one it was asked for (sessionIntent). It is read on the call's goroutine, which alone moves
// the session call's turn.
func (c *callRun) interrupted() string {
	c.mu.Lock()
	reason, op := c.intent, c.op
	c.mu.Unlock()
	if s := c.sess; reason == "" && s != nil {
		reason = s.intent.of(op, s.attempt, s.agent, s.turn)
	}
	return reason
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
	// a creation sent again runs in the directory the first request was made for: it exists, and nothing is made
	if res, stop := c.creationDirectory(ctx); stop {
		return res, true
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
	if res, down := c.journalDown(); down {
		return res, true
	}
	info, err := e.cfg.Backend.StartThread(ctx, ThreadSpec{CallID: c.id, Role: p.role, Pair: p.spec.Pair, Sandbox: p.spec.Sandbox, Cwd: cwd, Node: p.node, Head: p.opts.Head, Placement: placement, AutoCompactTokenLimit: p.spec.AutoCompactTokenLimit, Send: send, QA: p.qa, Tools: c.e.cfg.Tools})
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
	if res, stop := c.creationDirectory(ctx); stop {
		return res, true
	}
	cwd := c.placement.Path
	send := max(1, c.send)
	prefix := threadPrefix(e.j, p.resume.ThreadID, p.resume.ThroughTurnID)
	throughOp := ""
	if len(prefix) > 0 {
		throughOp = rowStr(prefix[len(prefix)-1], "op")
	}
	c.event(EvForkRequested, c.creationIntent(map[string]any{"thread_id": p.resume.ThreadID, "through_turn_id": p.resume.ThroughTurnID, "prefix": prefix, "cwd": cwd}, send))
	if res, down := c.journalDown(); down {
		return res, true
	}
	info, err := forker.ForkThread(ctx, ForkSpec{CallID: c.id, Role: p.role, Node: p.node, Pair: p.spec.Pair, Sandbox: p.spec.Sandbox, Cwd: cwd,
		SourceThreadID: p.resume.ThreadID, ThroughTurnID: p.resume.ThroughTurnID, ThroughOp: throughOp, AutoCompactTokenLimit: p.spec.AutoCompactTokenLimit, Send: send, QA: p.qa, Tools: c.e.cfg.Tools})
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
// its `<call_id>/t<k>` marker. A fork's prefix is what the forked thread must hold. A thread that was itself forked starts
// with the turns it inherited (the prefix its fork request recorded, which is the whole history through its boundary), and
// only then come the turns its own calls ran; a turn an operator replaced is not part of the history. The rows are read in
// order, and a turn belongs to the thread its call was on when the turn started: a call that ran again on another thread
// (a new attempt, a fork in a suffix) left its earlier turns on the thread it ran them on.
func threadPrefix(j Journal, threadID, throughTurn string) []map[string]any {
	return threadTurns(j, threadID, throughTurn, false, 0)
}

// conversationPrefix is threadPrefix as the conversation a session subagent continues (the checkpoint packet's record), each turn
// with the input of the step or request it took: a session subagent bound to a continuation goes on from the conversation of the
// handle it continues (its packet names that call's packet and answer), as a fork inherits its prefix, and a later attempt of a call
// goes on from the call's conversation before the turn it starts at (its packet holds the task and the answer it repairs). A session
// call's conversation is its turns at the conversation turn each records (callConv), each conversation turn once, a turn that names
// the turn it replaces in that turn's place. The digest chain of a continuation (threadChain) keeps threadPrefix: the replay rule
// replays a position only after every position before it, so the links before the handle's are proven by their own positions
// (CRW-1384).
func conversationPrefix(j Journal, threadID, throughTurn string) []map[string]any {
	return threadTurns(j, threadID, throughTurn, true, 0)
}

// convTurn is a turn of a call's conversation: the conversation turn it is and the thread it ran on.
type convTurn struct {
	index                int
	id, op, input, owner string
}

// callConv is a session call's conversation as its rows record it. The engine records the place of a turn when it publishes the
// turn's step and when it binds the turn: step_published and turn_started name the conversation turn the turn is
// (conversation_turn: the call's task turn 1, its repair 2, conversationTurn) and, for a turn run again - the next attempt of a lost
// turn, the turn a held call goes on with in place on a new subagent - the turn it replaces (replaces_turn). The conversation holds
// each conversation turn once: a turn takes its place over the turn that held it and takes out the turn it names. A turn_started row
// of a journal written before the place was recorded (no conversation_turn) takes it from what its step recorded, the repair member
// of the step_published row at its k (the task turn 1, the repair 2; the steps of one k are one turn, so they agree), and a later
// turn at that place replaces the earlier one by the same rule. Nothing is read back from held rows or from k.
type callConv struct {
	turns []convTurn
	// repairAt is the repair member of the call's step_published rows by k, the place of a turn_started row that records none
	repairAt map[int]bool
}

// step reads a step_published row: the repair member of its k.
func (x *callConv) step(d map[string]any) {
	if x.repairAt == nil {
		x.repairAt = map[int]bool{}
	}
	x.repairAt[rowInt(d, "k")] = d["repair"] == true
}

// index is the conversation turn of a session turn_started row: the one the row records, else the one its step's repair member
// records.
func (x *callConv) index(d map[string]any) int {
	if n := rowInt(d, "conversation_turn"); n > 0 {
		return n
	}
	return conversationTurn(x.repairAt[rowInt(d, "k")])
}

// put puts a turn in its conversation turn's place, over the turn that held it, and takes out the turn it names as the one it
// replaces.
func (x *callConv) put(t convTurn, replaces string) {
	if replaces != "" {
		x.turns = slices.DeleteFunc(x.turns, func(c convTurn) bool { return c.id == replaces })
	}
	for i := range x.turns {
		switch {
		case x.turns[i].index == t.index:
			x.turns[i] = t
			return
		case x.turns[i].index > t.index:
			x.turns = slices.Insert(x.turns, i, t)
			return
		}
	}
	x.turns = append(x.turns, t)
}

// at is the turn the conversation holds at conversation turn n, "" when it holds none there.
func (x *callConv) at(n int) string {
	for _, t := range x.turns {
		if t.index == n {
			return t.id
		}
	}
	return ""
}

// callConversation is the conversation the rows of a session call's newest attempt record.
func callConversation(rows []Event) *callConv {
	x := &callConv{}
	for _, ev := range rows {
		if ev.Kind != KindCall {
			continue
		}
		switch ev.Event {
		case EvPrepared:
			x = &callConv{}
		case EvStepPublished:
			x.step(ev.Data)
		case EvTurnStarted:
			if rowStr(ev.Data, "via") == "session" {
				x.put(convTurn{index: x.index(ev.Data), id: rowStr(ev.Data, "turn_id"), op: rowStr(ev.Data, "op")}, rowStr(ev.Data, "replaces_turn"))
			}
		}
	}
	return x
}

func threadTurns(j Journal, threadID, throughTurn string, session bool, depth int) []map[string]any {
	events := j.Events()
	replaced := replacedTurns(events)
	owner := map[string]string{}
	forkReq := map[string]Event{}
	var out []map[string]any
	inherited := false
	// the session reading: the handle a call continues, each session call's conversation (callConv), the input of each op, and the
	// call whose earlier turns the thread's first turn goes on from
	handle := map[string][2]string{}
	conv := map[string]*callConv{}
	inputOf := map[string]string{}
	goesOn := ""
	convOf := func(call string) *callConv {
		if conv[call] == nil {
			conv[call] = &callConv{}
		}
		return conv[call]
	}
	turn := func(id, op string) map[string]any {
		t := map[string]any{"turn_id": id, "op": op}
		if session {
			t["input"] = inputOf[op]
		}
		return t
	}
	for _, ev := range events {
		if ev.Kind != KindCall {
			continue
		}
		switch ev.Event {
		case EvPrepared:
			// a continuation is on its handle's thread from the start, any other call on none until its thread starts
			owner[ev.CallID], _ = ev.Data["thread_id"].(string)
			handle[ev.CallID] = [2]string{rowStr(ev.Data, "thread_id"), rowStr(ev.Data, "through_turn_id")}
			conv[ev.CallID] = &callConv{}
		case EvTurnRequested:
			inputOf[rowStr(ev.Data, "op")] = rowStr(ev.Data, "input")
		case EvStepPublished:
			inputOf[rowStr(ev.Data, "op")] = rowStr(ev.Data, "input")
			convOf(ev.CallID).step(ev.Data)
		case EvForkRequested:
			forkReq[ev.CallID] = ev
		case EvThreadStarted:
			owner[ev.CallID], _ = ev.Data["thread_id"].(string)
			if session && !inherited && rowStr(ev.Data, "via") == "session" && owner[ev.CallID] == threadID {
				inherited = true
				if h := handle[ev.CallID]; h[0] != "" && h[0] != threadID && depth < 64 {
					out = append(out, threadTurns(j, h[0], h[1], true, depth+1)...)
				}
				goesOn = ev.CallID
			}
		case EvForkStarted:
			t, _ := ev.Data["thread_id"].(string)
			owner[ev.CallID] = t
			if t == threadID && !inherited {
				inherited = true
				for _, p := range prefixOf(forkReq[ev.CallID].Data) {
					out = append(out, turn(p.TurnID, p.Op))
				}
			}
		case EvTurnStarted:
			id, _ := ev.Data["turn_id"].(string)
			op, _ := ev.Data["op"].(string)
			if replaced[id] {
				continue
			}
			x := convOf(ev.CallID)
			at := x.index(ev.Data)
			if session && rowStr(ev.Data, "via") == "session" {
				x.put(convTurn{index: at, id: id, op: op, input: inputOf[op], owner: owner[ev.CallID]}, rowStr(ev.Data, "replaces_turn"))
			}
			if owner[ev.CallID] != threadID {
				continue
			}
			if goesOn == ev.CallID {
				// the thread's first turn: the call's conversation before this turn's conversation turn, on the subagents of its earlier
				// attempts
				goesOn = ""
				for _, t := range x.turns {
					if t.index < at && t.owner != threadID {
						out = append(out, map[string]any{"turn_id": t.id, "op": t.op, "input": t.input})
					}
				}
			}
			out = append(out, turn(id, op))
		}
	}
	for i, p := range out {
		if p["turn_id"] == throughTurn {
			return out[:i+1]
		}
	}
	return out
}

// replacedTurns are the turns an operator's adoption replaced (turn_started rows with `replaces`): the call follows the
// turn that replaced them.
func replacedTurns(events []Event) map[string]bool {
	out := map[string]bool{}
	for _, ev := range events {
		if ev.Kind == KindCall && ev.Event == EvTurnStarted {
			if old := rowStr(ev.Data, "replaces"); old != "" {
				out[old] = true
			}
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
	if c.sess != nil && c.held == nil && c.sess.resume != nil {
		var res CallResult
		var stop bool
		if res, stop, k, input = c.sessionRecover(ctx); stop {
			return res
		}
	} else if c.held != nil {
		var res CallResult
		var stop bool
		if res, stop, k, input, pre = c.resumeHeld(ctx); stop {
			return res
		}
	} else if c.rec != nil {
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
		case c.sess != nil:
			// a session call starts no thread: its subagent is bound when the owner thread reports it (session.go)
			if res, stop := c.sessionPrepare(ctx); stop {
				return res
			}
			input = c.preface + input
		case p.resume == nil:
			if res, stop := c.startThread(ctx); stop {
				return res
			}
		case c.placement != nil && c.placement.Fork:
			if res, stop := c.forkOrCheckpoint(ctx); stop {
				return res
			}
			input = c.preface + input
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
		return res, stop, 1, c.checkpointPreface() + p.prompt, nil
	case RecoverResendCreate:
		c.send = rec.Send
		if rec.Kind == "fork" {
			res, stop = c.forkThread(ctx)
		} else {
			res, stop = c.startThread(ctx)
		}
		return res, stop, 1, c.checkpointPreface() + p.prompt, nil
	}
	info, _ := LookupCall(c.e.j, c.id)
	c.threadID, c.configured, c.cwd = info.ThreadID, info.Configured, info.Cwd
	own := CallEvents(c.e.j, c.id)
	replaced := replacedTurns(own)
	for _, ev := range own {
		if ev.Event == EvPrepared {
			c.turnIDs = nil
		}
		if ev.Event == EvTurnStarted {
			if !replaced[rowStr(ev.Data, "turn_id")] {
				c.turnIDs = append(c.turnIDs, rowStr(ev.Data, "turn_id"))
			}
			c.delivered = true
		}
	}
	if c.firstK == 0 {
		// the first ordinary turn of this pass: a repair turn follows an ordinary one
		c.firstK = max(rec.K, 1)
		if rec.Repair {
			c.firstK = max(rec.K-1, 1)
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
	if c.firstK == 0 {
		c.firstK = 1
	}
	for ; ; k++ {
		var out TurnOutcome
		var err error
		repair := k > c.firstK
		if pre != nil {
			// The turn was sent before the process stopped: no request, no gate, no intent row.
			c.repairUsed = c.repairUsed || repair
			if pre.running {
				host, _ := e.cfg.Backend.(ReconHost)
				turnCtx, endTurn := c.beginTurn(ctx, pre.op)
				c.setIntent(pre.intent)
				out, err = host.AwaitTurn(turnCtx, c.threadID, pre.turnID, pre.op)
				endTurn()
			} else {
				c.setIntent(pre.intent)
				out = *pre.outcome
			}
			pre = nil
		} else {
			if res, stop := c.gate(ctx); stop {
				return res
			}
			if res, stop := c.ownDirectory(ctx); stop {
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
			if c.sess != nil {
				// the turn is a step a subagent of the owner thread takes: step_published is its intent row (session.go)
				c.repairUsed = c.repairUsed || repair
				var res CallResult
				var stop bool
				if out, res, stop, err = c.sessionTurn(ctx, k, op, input, repair); stop {
					return res
				}
			} else {
				intent := map[string]any{"k": k, "op": op, "input": input, "repair": repair}
				if resend {
					intent["resend"] = true
					resend = false
				}
				c.event(EvTurnRequested, intent)
				if res, down := c.journalDown(); down {
					return res
				}
				c.repairUsed = c.repairUsed || repair
				turnCtx, endTurn := c.beginTurn(ctx, op)
				out, err = e.cfg.Backend.RunTurn(turnCtx, TurnSpec{
					CallID: c.id, Op: op, K: k, ThreadID: c.threadID, Role: p.role, Node: p.node, Pair: p.spec.Pair,
					Input: "crw-flow-op: " + op + "\n\n" + input + toolsSuffix(e.cfg.Tools), Schema: p.schema, Cwd: c.cwd, Repair: repair, QA: p.qa,
					Started: func(turnID string) {
						c.turnIDs = append(c.turnIDs, turnID)
						c.delivered = true
						c.event(EvTurnStarted, map[string]any{"k": k, "op": op, "turn_id": turnID})
					},
				})
				endTurn()
			}
		}
		if err != nil {
			return c.fromError(ctx, err)
		}
		c.tokens = c.tokens.Add(out.Tokens)
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
		extracted := false
		if verr != nil && c.sess != nil && resolved != nil {
			// a subagent's answer may carry prose before its JSON: the object that ends it is the answer (§3.3)
			if cand, ok := extractTrailingJSON(out.Text); ok {
				if r, err := c.validate(cand, resolved); err == nil {
					result, verr, extracted = r, nil, true
				}
			}
		}
		if verr == nil {
			data := map[string]any{"result": string(result), "result_digest": digestJSON(result), "tokens": c.tokens, "repair_used": c.repairUsed}
			if extracted {
				data["raw_text"], data["extracted"] = out.Text, true
			}
			if c.sess != nil {
				c.sessionChecks(ctx, data)
			}
			if err := c.pendingTree(ctx, data); err != nil {
				return c.pendingTreeLost(ctx, err, data)
			}
			c.qaSource(ctx, data)
			res := c.terminal(EvCompleted, StatusCompleted, "", data)
			if pt, _ := data["pending_tree"].(string); pt != "" {
				c.e.noteExecutor(p.node, pt)
			}
			return res
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

// ownsDirectory is whether the call is a continuation that runs in a directory of its own: an architect turn that names the head
// it judges (the REFLECTION, a re-plan and the design fixes of its audit), for which the placement made a call-private directory at
// that head, and whose turns name that directory as their cwd. Inside a resume's suffix such a turn runs on a fork of the thread of
// the previous process, in the same kind of directory: it is the call's own as well (CRW-1202).
func (c *callRun) ownsDirectory() bool {
	pl, p := c.placement, c.plan
	return pl != nil && ownsDirectoryAt(pl.Kind, pl.Path, p.role, p.opts.Head, p.resume != nil, p.qa)
}

// ownsDirectoryAt is the one rule of a directory of a call's own: a call-private placement of a continuation of the architect that names
// the head it judges, not a QA turn. Every writer of a terminal row of an agent call decides by it (CRW-1202).
func ownsDirectoryAt(kind, path, role, head string, continues, qa bool) bool {
	return kind == PlaceCall && path != "" && continues && role == "architect" && head != "" && !qa
}

// ownDirectoryOfRows is the directory of its own that the newest attempt of a call made, read from the call's prepared row: the
// terminal rows the engine writes outside the call's own run (a resume superseding an attempt an earlier process left) decide it by
// the same rule as callRun.terminal. "" when the attempt has none.
func ownDirectoryOfRows(rows []Event) string {
	var prep Event
	for _, ev := range rows {
		if ev.Kind == KindCall && ev.Op == "agent" && ev.Event == EvPrepared {
			prep = ev
		}
	}
	m, ok := prep.Data["allocation"].(map[string]any)
	if !ok {
		return ""
	}
	qa, _ := prep.Data["qa"].(bool)
	if path := rowStr(m, "path"); ownsDirectoryAt(rowStr(m, "kind"), path, rowStr(prep.Data, "role"), rowStr(prep.Data, "head"), rowStr(prep.Data, "resume_of") != "", qa) {
		return path
	}
	return ""
}

// supersededRow writes the superseded row the engine itself ends an agent call of an earlier process with, deciding the directory of
// the call's own as callRun.terminal does - a superseded call is not an unknown outcome - and removes it after the row. The flow
// evidence records its attempts still hold open go with it (evidenceRelease).
func (e *Engine) supersededRow(callID string, rows []Event, first Event, data map[string]any) {
	dir := ownDirectoryOfRows(rows)
	if dir != "" {
		data[ownDirectoryKey] = dir
	}
	e.j.Append(Event{RunID: e.runID, Kind: KindCall, CallID: callID, Path: first.Path, Position: first.Position, Op: "agent", Label: first.Label, Event: EvSuperseded, Data: data})
	if dir != "" {
		e.removeOwnDirectory(callID, dir)
	}
	e.evidenceRelease(rows)
}

// creationDirectory makes the directory a call's new thread (a start, a fork, a checkpoint thread) is created in. A directory of the
// call's own is made when it is not there - a held fork that goes on, or whose creation is sent again, finds it removed with the
// held row (removeOwnDirectory) - at the head its prepared row records; any other placement is made only by a first creation.
func (c *callRun) creationDirectory(ctx context.Context) (CallResult, bool) {
	if c.ownsDirectory() {
		return c.ownDirectory(ctx)
	}
	if c.rec == nil || c.needEstablish {
		return c.establish(ctx)
	}
	return CallResult{}, false
}

// ownDirectory makes the directory of a continuation that runs at a head of its own before the turn that needs it is sent, and
// makes it the turn's cwd. A directory that is not there is made again at the head the call's prepared row records: a held call
// that goes on, or a call a resume runs again, finds it removed (removeOwnDirectory), and a path belongs to one call.
func (c *callRun) ownDirectory(ctx context.Context) (CallResult, bool) {
	if !c.ownsDirectory() {
		return CallResult{}, false
	}
	if w, ok := c.e.cfg.Backend.(interface{ WorktreeExists(string) bool }); !ok || !w.WorktreeExists(c.placement.Path) {
		if placer, ok := c.e.cfg.Backend.(Placer); ok {
			if err := placer.Establish(ctx, c.placeReq, *c.placement); err != nil {
				return c.placementProblem(ctx, err), true
			}
		}
	}
	c.cwd = c.placement.Path
	return CallResult{}, false
}

// ownDirectoryKey is the key of a terminal row that names the directory of the call's own it ends (CRW-1202).
const ownDirectoryKey = "own_directory"

// EvOwnDirectoryKept is the flow row of a directory a terminal row names that could not be removed: the next resume removes it again.
const EvOwnDirectoryKept = "own_directory_kept"

// ownDirectoryWait bounds the removal of a directory of a call's own: it does not depend on the run's context, which a cancellation
// right after the terminal row ends.
const ownDirectoryWait = 30 * time.Second

// removeOwnDirectory removes the directory of a continuation that ran at a head of its own once the call has its terminal row, which
// names it. A call that has none (its outcome is not known) may still have a turn at work in it and keeps the directory.
func (c *callRun) removeOwnDirectory(res CallResult) {
	if !c.ownsDirectory() || res.Status == StatusUnknown {
		return
	}
	c.e.removeOwnDirectory(c.id, c.placement.Path)
}

// removeOwnDirectory removes a directory a terminal row names, with a context of its own; a removal that fails is journaled
// (own_directory_kept) and is made again by the next resume. Removing a directory that is not there is nothing.
func (e *Engine) removeOwnDirectory(callID, path string) {
	r, ok := e.cfg.Backend.(DirRemover)
	if !ok || path == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(e.ctx), ownDirectoryWait)
	defer cancel()
	if err := r.RemoveDir(ctx, path); err != nil {
		e.j.Append(Event{RunID: e.runID, Kind: KindFlow, CallID: callID, Op: "agent", Event: EvOwnDirectoryKept, Data: map[string]any{"call_id": callID, "path": path, "error": err.Error()}})
	}
}

// sweepOwnDirectories is the part of a resume that removes the directories the terminal rows of the earlier processes name: a
// cancellation right after the row, or a removal that failed, left it there, and a resume that replays the call, runs it again in a
// directory of its own, or never reaches it again removes it all the same. The directory a call is still at work in - a call whose
// newest state is not a terminal row, or an unknown outcome (a held call that went on in place uses the directory its held row named)
// - is kept.
func (e *Engine) sweepOwnDirectories() {
	if _, ok := e.cfg.Backend.(DirRemover); !ok || e.prior == nil {
		return
	}
	for _, id := range e.prior.order {
		rows := e.prior.rowsOf(id, "agent")
		inUse := ""
		if term := lastTerminal(rows); term.Event == "" || term.Event == EvOutcomeUnknown {
			for _, ev := range rows {
				if ev.Event == EvPrepared {
					if m, ok := ev.Data["allocation"].(map[string]any); ok {
						inUse = rowStr(m, "path")
					}
				}
			}
		}
		done := map[string]bool{}
		for _, ev := range rows {
			path := rowStr(ev.Data, ownDirectoryKey)
			if !isTerminal(ev.Event) || path == "" || path == inUse || done[path] {
				continue
			}
			done[path] = true
			e.removeOwnDirectory(id, path)
		}
	}
}
