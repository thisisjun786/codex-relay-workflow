package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/roles"
)

// This file is the session call path of the engine (CRW-1316 §3, §3.4, §4, §5, §6). A session call is an agent call whose turns a
// role subagent of the run's owner thread runs: the call keeps everything the App Server path does before and after a turn (the
// prepared row and its allocation, the gate, the schema validation and its one repair, the terminal rows, the pending tree), but a
// turn is a step it publishes and a subagent it binds and harvests, and its recovery is its own. The App Server Reconcile, the live
// resend and the retirement of a worktree are never used for it: the allocation of the prepared row is the one every attempt of the
// call works in.

// sessionCall is what one session call keeps between its steps.
type sessionCall struct {
	// attempt is the ordinal of the call's current subagent (1 for the first); agent, turn and boundK are the subagent bound to
	// it, the turn bound last and its k.
	attempt int
	agent   string
	turn    string
	boundK  int
	rollout string
	pair    Pair
	// trace are the digests taken when the subagent was bound.
	trace *TraceDigests
	// seen is the rollout's token usage already counted: the rollout's token_count is the thread's running total.
	seen Tokens
	// note is what the next spawn's packet says about the attempts before it; task is the input of the call's first ordinary turn
	// (the packet of a spawn that repairs or continues names it); lastText is the answer of the turn harvested last.
	note, task, lastText string
	// prior are the pairs earlier attempts of the call were bound with.
	prior []Pair
	// resume is the stage a call that a resumed run goes on with in place starts from; intent is the interrupt the previous
	// process asked for, with the turn it was asked for.
	resume *sessionStage
	intent sessionIntent
	// conv is the conversation turn of the step the call works on (conversationTurn) and replaces the turn of the call's conversation
	// that step's turn replaces: the step_published and turn_started rows of the step record both (CRW-1384).
	conv     int
	replaces string
	// evidence is the flow evidence assignment of the attempt's spawn, nil when the call's steps get none (session_evidence.go,
	// CRW-1482).
	evidence *sessionEvidence
}

// conversationTurn is the turn of its call's conversation a session step's turn is: the call's task turn is 1 and its one repair 2,
// on whatever subagent, attempt and k it runs. A turn run again - the next attempt of a lost turn, the turn a held call goes on with
// in place on a new subagent - is the turn it replaces.
func conversationTurn(repair bool) int {
	if repair {
		return 2
	}
	return 1
}

// sessionIntent is an interrupt the previous process asked for (its interrupt_requested row) and the turn it was asked for: the op
// the row names, the attempt, and the subagent and turn the rows bound to that step - the turn the engine's interrupt reached, which
// a stop binds before it interrupts it (sessionBindStopped, sessionStop). It decides that turn only: every other turn of the call - a
// new attempt, a step published again, a turn the host started after the interrupt - starts with no intent (CRW-1384).
type sessionIntent struct {
	reason, op  string
	attempt     int
	agent, turn string
}

// of is the reason of the intent for the turn a call is on (its op, attempt, subagent and bound turn), "" for any other turn.
func (i sessionIntent) of(op string, attempt int, agent, turn string) string {
	if i.reason == "" || i.turn == "" || i.op != op || i.attempt != attempt || i.agent != agent || i.turn != turn {
		return ""
	}
	return i.reason
}

// sessionStage is where a session call's rows say it stopped.
type sessionStage struct {
	// kind is prepared, published, bound or lost.
	kind       string
	k, attempt int
	op, input  string
	repair     bool
	action     string
	// agent and turn are the bound subagent and turn (bound); agent is also the subagent a send_input step was for.
	agent, turn string
	rollout     string
	pair        Pair
	note, prev  string
	// intent and intentOp are the reason and the op of the last interrupt_requested row among the step's rows
	intent, intentOp string
	// seen and calls are what a send_input step's row records of the tokens counted before it: the subagent's rollout total the
	// earlier turns counted and the call's tokens (a recovered repair turn counts only what it adds).
	seen, calls Tokens
}

const (
	stagePrepared  = "prepared"
	stagePublished = "published"
	stageBound     = "bound"
	stageLost      = "lost"
)

// sessionRouter hands the dispatch reports the run reads from its inbox to the session calls that wait for them.
type sessionRouter struct {
	mu      sync.Mutex
	waiters map[string]*sessionWaiter
	handed  map[string]bool
}

type sessionWaiter struct {
	k, attempt int
	ch         chan DispatchReport
}

func newSessionRouter() *sessionRouter {
	return &sessionRouter{waiters: map[string]*sessionWaiter{}, handed: map[string]bool{}}
}

func (r *sessionRouter) register(call string, k, attempt int) *sessionWaiter {
	w := &sessionWaiter{k: k, attempt: attempt, ch: make(chan DispatchReport, 8)}
	r.mu.Lock()
	r.waiters[call] = w
	r.mu.Unlock()
	return w
}

// unregister ends a wait; a report handed to it and not taken goes back to the router, which answers it from the journal.
func (r *sessionRouter) unregister(call string, w *sessionWaiter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.waiters[call] == w {
		delete(r.waiters, call)
	}
	for {
		select {
		case rep := <-w.ch:
			delete(r.handed, rep.ID)
		default:
			return
		}
	}
}

func (r *sessionRouter) answered(id string) {
	r.mu.Lock()
	delete(r.handed, id)
	r.mu.Unlock()
}

// routeReports reads the run's inbox until the run ends.
func (e *Engine) routeReports(done chan struct{}) {
	defer close(done)
	s := e.cfg.Session
	for {
		if err := s.wait(e.ctx, s.poll()); err != nil {
			return
		}
		reps, err := s.Bus.Reports()
		if err != nil {
			continue
		}
		for _, rep := range reps {
			e.route(rep)
		}
	}
}

// route hands a report to the call that waits for its step, or answers it from the journal: a report for an attempt that is
// bound already is answered bound (or close, for a second subagent), one for an attempt that ended is answered close, and so is
// one that names no published step (an early report; refused when it names no subagent). A report for a step that is published and that no call waits for now stays in the inbox
// until its call reaches it (a resumed run that has not reached the position yet).
func (e *Engine) route(rep DispatchReport) {
	r, bus := e.sessR, e.cfg.Session.Bus
	if rep.Run != e.runID {
		_ = bus.Reply(rep, DispatchReply{Status: ReplyRefused, Detail: fmt.Sprintf("the report names run %s, not run %s", rep.Run, e.runID)})
		return
	}
	r.mu.Lock()
	if r.handed[rep.ID] {
		r.mu.Unlock()
		return
	}
	if w := r.waiters[rep.Call]; w != nil && w.k == rep.K && w.attempt == rep.Attempt {
		select {
		case w.ch <- rep:
			r.handed[rep.ID] = true
		default:
		}
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()
	if reply, keep := e.judgeReport(rep); !keep {
		_ = bus.Reply(rep, reply)
	}
}

// judgeReport answers a report no call waits for, from the call's rows of its newest prepared attempt.
func (e *Engine) judgeReport(rep DispatchReport) (DispatchReply, bool) {
	var rows []Event
	for _, ev := range CallEvents(e.j, rep.Call) {
		if ev.Op != "agent" {
			continue
		}
		if ev.Event == EvPrepared {
			rows = nil
		}
		rows = append(rows, ev)
	}
	published, ended := false, false
	bound := ""
	for _, ev := range rows {
		d := ev.Data
		switch {
		case ev.Event == EvStepPublished && rowInt(d, "k") == rep.K && rowInt(d, "attempt") == rep.Attempt:
			published = true
		case ev.Event == EvStepPublished && published && rowInt(d, "attempt") > rep.Attempt:
			ended = true
		case ev.Event == EvSessionAttemptLost && published && rowInt(d, "attempt") == rep.Attempt:
			ended = true
		case ev.Event == EvThreadStarted && rowInt(d, "attempt") == rep.Attempt && rowStr(d, "via") == "session":
			bound = rowStr(d, "thread_id")
		case isTerminal(ev.Event) && published && ev.Event != EvHeld:
			ended = true
		}
	}
	switch {
	case !published && rep.Agent != "":
		// an early report (or one of an earlier prepared attempt): the subagent has no step to work on, so it is closed
		return DispatchReply{Status: ReplyClose, Close: rep.Agent, Detail: fmt.Sprintf("close %s: run %s published no step %s/t%d-a%d", rep.Agent, e.runID, rep.Call, rep.K, rep.Attempt)}, false
	case !published:
		return DispatchReply{Status: ReplyRefused, Detail: fmt.Sprintf("run %s published no step %s/t%d-a%d", e.runID, rep.Call, rep.K, rep.Attempt)}, false
	case ended:
		return DispatchReply{Status: ReplyClose, Close: rep.Agent, Detail: fmt.Sprintf("close %s: attempt %d of %s has ended", rep.Agent, rep.Attempt, rep.Call)}, false
	case bound != "" && bound == rep.Agent:
		return DispatchReply{Status: ReplyBound, Detail: "the run bound this agent already"}, false
	case bound != "":
		return DispatchReply{Status: ReplyClose, Close: rep.Agent, Detail: fmt.Sprintf("close %s: the attempt is bound to %s", rep.Agent, bound)}, false
	}
	return DispatchReply{}, true
}

// agentTypeOf is the spawn_agent agent_type of a role.
func agentTypeOf(role string) string {
	switch role {
	case "executor-t1":
		return "executor"
	case "reviewer-final":
		return "reviewer"
	}
	return role
}

// readsOnly is whether the role's subagent changes nothing.
func readsOnly(family string) bool { return family != "executor" }

func hostGone(status string) bool {
	return status == "notLoaded" || status == "systemError" || status == "notFound"
}

// sessionOp is the marker of a turn's attempt.
func sessionOp(op string, attempt int) string { return fmt.Sprintf("%s-a%d", op, attempt) }

// sessionPrepare readies a session call that starts now: its directory P is made, and a continuation inside a suffix takes the
// checkpoint branch (a new subagent never forks a thread).
func (c *callRun) sessionPrepare(ctx context.Context) (CallResult, bool) {
	if res, stop := c.creationDirectory(ctx); stop {
		return res, true
	}
	if c.placement != nil && c.placement.Fork && c.plan.resume != nil {
		c.sessionCheckpoint()
	}
	return c.sessionNeedsDirectory()
}

func (c *callRun) sessionNeedsDirectory() (CallResult, bool) {
	if c.cwd == "" {
		return c.terminal(EvHeld, StatusHeld, ReasonWorktreeFailed, map[string]any{"input_state": "not_delivered",
			"detail": "a session call runs in a directory of its own (P), and the backend placed none"}), true
	}
	return CallResult{}, false
}

// sessionCheckpoint is the checkpoint branch of a continuation inside a suffix: the journal's record of the handle's turns is the
// packet's preface.
func (c *callRun) sessionCheckpoint() {
	p := c.plan
	packet := checkpointPacket(c.e.j, p.resume.ThreadID, p.resume.ThroughTurnID)
	c.preface = packet + "\n\n"
	cwd := c.cwd
	if c.placement != nil && c.placement.Path != "" {
		cwd = c.placement.Path
	}
	c.e.j.Append(Event{RunID: c.e.runID, Kind: KindFlow, CallID: c.id, Path: c.path.id, Position: c.position, Op: "agent", Label: c.label, Event: EvCheckpointStarted,
		Data: map[string]any{"call_id": c.id, "thread_id": p.resume.ThreadID, "through_turn_id": p.resume.ThroughTurnID, "checkpoint_digest": digestJSON([]byte(packet)),
			"packet": packet, "cwd": cwd, "via": "session"}})
}

// sessionEstablish makes the directory of a call that goes on in place when it is not there; a directory an earlier process of the
// run made is the call's as it is.
func (c *callRun) sessionEstablish(ctx context.Context) (CallResult, bool) {
	if c.ownsDirectory() {
		return c.ownDirectory(ctx)
	}
	pl := c.placement
	if pl == nil || pl.Path == "" || pl.Kind == PlaceContinued {
		return CallResult{}, false
	}
	if w, ok := c.e.cfg.Backend.(interface{ WorktreeExists(string) bool }); ok && w.WorktreeExists(pl.Path) {
		c.cwd = pl.Path
		return CallResult{}, false
	}
	return c.establish(ctx)
}

// sessionStageOf reads where a session call's rows of its newest attempt say it stopped.
func sessionStageOf(rows []Event) *sessionStage {
	st := &sessionStage{kind: stagePrepared}
	for _, ev := range rows {
		d := ev.Data
		switch ev.Event {
		case EvPrepared:
			st = &sessionStage{kind: stagePrepared}
		case EvStepPublished:
			st = &sessionStage{kind: stagePublished, k: rowInt(d, "k"), attempt: rowInt(d, "attempt"), op: rowStr(d, "op"), input: rowStr(d, "input"),
				repair: d["repair"] == true, action: rowStr(d, "action"), agent: rowStr(d, "agent_id"), note: rowStr(d, "note"), prev: rowStr(d, "previous_answer"),
				rollout: st.rollout, pair: st.pair, seen: rowTokens(d, "tokens_seen"), calls: rowTokens(d, "call_tokens")}
			if st.action == StepSpawn {
				st.agent = ""
			}
		case EvThreadStarted:
			if rowStr(d, "via") == "session" {
				st.agent, st.rollout = rowStr(d, "thread_id"), rowStr(d, "rollout_path")
				st.pair = Pair{Model: rowStr(d, "model"), Effort: rowStr(d, "effort")}
			}
		case EvTurnStarted:
			if st.kind == stagePublished && rowInt(d, "k") == st.k {
				st.kind, st.turn = stageBound, rowStr(d, "turn_id")
			}
		case EvSessionAttemptLost:
			st.kind = stageLost
		case EvInterruptRequested:
			st.intent, st.intentOp = rowStr(d, "reason"), rowStr(d, "op")
		}
	}
	return st
}

// interrupt is the intent the stage's rows hold, with the turn it was asked for: the turn bound to the step and attempt of the stage
// when its row names that step's op, else none (an interrupt of a turn whose step the rows never published).
func (st *sessionStage) interrupt() sessionIntent {
	in := sessionIntent{reason: st.intent, op: st.intentOp, attempt: st.attempt, agent: st.agent}
	if st.intentOp == st.op {
		in.turn = st.turn
	}
	return in
}

// rowTokens reads the Tokens a row member holds (a struct in memory, a JSON object from the store).
func rowTokens(d map[string]any, key string) Tokens {
	var t Tokens
	if v, ok := d[key]; ok && v != nil {
		if raw, err := json.Marshal(v); err == nil {
			_ = json.Unmarshal(raw, &t)
		}
	}
	return t
}

// sessionResumeOpen is the resume walk of a session call the previous process left open: it goes on in place, in the allocation
// its prepared row records, from the stage its rows show (the reconcile of the App Server path is not used).
func (c *callRun) sessionResumeOpen(rows []Event, prep Event) bool {
	e, p := c.e, c.plan
	if !e.claim(c.id) {
		return c.live()
	}
	c.startSeq = prep.Seq
	c.suffixCall = true
	if m, ok := prep.Data["allocation"].(map[string]any); ok {
		pl := placementFromMap(m)
		c.placement = &pl
		c.placeReq = PlaceRequest{RunID: e.runID, CallID: c.id, Role: p.role, Family: p.spec.Family, Node: p.node, Head: p.opts.Head, QA: p.qa, Continues: p.resume, Journal: e.j}
	}
	c.sess.resume = sessionStageOf(rows)
	e.markLive(c.path, c.position+1)
	return true
}

// sessionRecover readies a call that goes on in place from its stage: its directory, what its rows bound, and the turn it goes on
// with.
func (c *callRun) sessionRecover(ctx context.Context) (res CallResult, stop bool, k int, input string) {
	s, p := c.sess, c.plan
	st := s.resume
	rows := CallEvents(c.e.j, c.id)
	c.sessionRestore(rows)
	if c.placement != nil && c.placement.Path != "" {
		c.cwd = c.placement.Path
	}
	if res, stop := c.sessionEstablish(ctx); stop {
		return res, true, 0, ""
	}
	if res, stop := c.sessionNeedsDirectory(); stop {
		return res, true, 0, ""
	}
	// the interrupt the previous process asked for goes with the turn it was asked for, and decides that turn only (interrupted)
	s.intent = st.interrupt()
	bus := c.e.cfg.Session.Bus
	switch st.kind {
	case stagePrepared:
		s.resume = nil
		c.firstK = 1
		return CallResult{}, false, 1, c.checkpointPreface() + p.prompt
	case stageLost:
		s.resume = nil
		// the step of the lost attempt was taken: a file the dead process left of it is no step to take
		_ = bus.Retract(c.id, st.k, st.attempt)
		// the lost attempt's unclaimed record goes with it (the process that lost it may have died before it removed it)
		c.evidenceDrop()
		s.attempt = st.attempt + 1
		s.note = c.lostNote(st.agent)
		s.lastText = st.prev
		c.sessionClose(st.agent, st.attempt)
		s.agent = ""
	case stageBound:
		s.resume = nil
		// the bound step was taken: the call goes on with its subagent and never binds it again, so the step file the dead process
		// left is retracted here (CRW-1367)
		_ = bus.Retract(c.id, st.k, st.attempt)
		s.attempt, s.agent, s.turn, s.boundK, s.rollout, s.pair = st.attempt, st.agent, st.turn, st.k, st.rollout, st.pair
		s.lastText = st.prev
		s.dropCurrentPair()
		if st.action == StepSendInput {
			c.sessionBaseline(st)
		}
	case stagePublished:
		s.attempt = st.attempt
		if st.action == StepSendInput {
			// a repair turn: the subagent it is for, and what the turns before it counted
			s.agent, s.rollout, s.pair = st.agent, st.rollout, st.pair
			s.dropCurrentPair()
			c.sessionBaseline(st)
		}
		s.note, s.lastText = st.note, st.prev
	}
	c.firstK = max(st.k, 1)
	if st.repair {
		c.firstK = max(st.k-1, 1)
	}
	return CallResult{}, false, max(st.k, 1), st.input
}

// sessionBaseline takes what the turns before a recovered send_input turn counted, from its step_published row: the subagent's
// rollout total already counted (the turn adds only its growth) and the call's tokens, in all their parts. The process that counted
// those tokens died before it ended the call, so they are in no terminal row and the run's figure this process was seeded with
// (Config.SpentBefore, RunSpent over the terminal rows) leaves them out; the call's terminal row will carry them, so the run's
// figure counts them here, once (CRW-1278 with CRW-1367).
func (c *callRun) sessionBaseline(st *sessionStage) {
	c.sess.seen, c.tokens = st.seen, st.calls
	c.e.addTokens(st.calls.Total)
}

// sessionRestore reads what a call's rows of its newest attempt bound: the turns, the thread its handle names, and the task.
func (c *callRun) sessionRestore(rows []Event) {
	s := c.sess
	info, _ := lookupCall(rows, c.id)
	c.threadID, c.configured = info.ThreadID, info.Configured
	c.turnIDs, c.delivered = nil, false
	s.prior = nil
	firstK := 0
	for _, ev := range rows {
		switch ev.Event {
		case EvPrepared:
			c.turnIDs, c.delivered, s.prior, s.task, firstK = nil, false, nil, "", 0
		case EvStepPublished:
			if !(ev.Data["repair"] == true) && (firstK == 0 || rowInt(ev.Data, "k") == firstK) {
				firstK = rowInt(ev.Data, "k")
				if s.task == "" {
					s.task = rowStr(ev.Data, "input")
				}
			}
		case EvThreadStarted:
			if rowStr(ev.Data, "via") == "session" {
				s.prior = append(s.prior, Pair{Model: rowStr(ev.Data, "model"), Effort: rowStr(ev.Data, "effort")})
			}
		case EvTurnStarted:
			c.turnIDs = append(c.turnIDs, rowStr(ev.Data, "turn_id"))
			c.delivered = true
		}
	}
	c.evidenceRestore(rows)
}

// dropCurrentPair takes the pair of the subagent the call goes on with out of the earlier attempts' pairs.
func (s *sessionCall) dropCurrentPair() {
	if n := len(s.prior); n > 0 {
		s.prior = s.prior[:n-1]
	}
}

// sessionResumeHeld goes on with a held session call: a call whose turn was delivered takes the task again on a new subagent (the
// held one is confirmed not running), with the reason it stopped; a call whose step a subagent took that could not be bound with its
// evidence record (evidence_unbound) goes on with the same turn on a new attempt, so a new subagent takes it and the rejected one is
// never bound; a call whose step was published and not bound looks for the subagent that took it first; any other starts its first
// turn. Every new attempt releases the record of the attempt it follows (the process that held the call may have died before its
// end released it).
func (c *callRun) sessionResumeHeld(ctx context.Context) (res CallResult, stop bool, k int, input string, pre *recoveredTurn) {
	s, p, h := c.sess, c.plan, c.held
	rows := c.e.prior.rowsOf(c.id, "agent")
	c.sessionRestore(rows)
	if c.placement != nil && c.placement.Path != "" {
		c.cwd = c.placement.Path
	} else if cwd := heldCwd(rows, Event{CallID: c.id}); cwd != "" {
		c.cwd = cwd
	}
	if res, stop := c.sessionEstablish(ctx); stop {
		return res, true, 0, "", nil
	}
	if res, stop := c.sessionNeedsDirectory(); stop {
		return res, true, 0, "", nil
	}
	st := sessionStageOf(rows)
	lastK, maxAttempt := 0, 0
	var repairStep *Event
	for i, ev := range rows {
		if ev.Event == EvPrepared {
			lastK, maxAttempt, repairStep = 0, 0, nil
		}
		if ev.Event == EvStepPublished {
			lastK, maxAttempt = max(lastK, rowInt(ev.Data, "k")), max(maxAttempt, rowInt(ev.Data, "attempt"))
			if ev.Data["repair"] == true {
				repairStep = &rows[i]
			}
		}
	}
	// the call has used its one repair when its rows hold the repair step: a call held at the gate of the repair turn (its task's
	// answer failed validation, the step was never published) goes on with the task and has its repair still (CRW-1405)
	c.repairUsed = h.repairUsed && repairStep != nil
	switch {
	case h.inputState == "delivered":
		k = lastK + 1
		c.firstK = k
		c.evidenceDrop()
		s.attempt = maxAttempt + 1
		s.note = c.heldNote(h.reason)
		if st.agent != "" {
			c.sessionClose(st.agent, maxAttempt)
		}
		s.agent = ""
		task := s.task
		if task == "" {
			task = c.checkpointPreface() + p.prompt
		}
		if h.repairUsed && repairStep != nil {
			// the call used its one repair (as resumeHeld counts it on the App Server path): the new subagent's turn is that repair
			// again, so its packet names the task once, the answer the repair turn repaired and its instruction (CRW-1384)
			c.firstK = 1
			s.lastText = rowStr(repairStep.Data, "previous_answer")
			return CallResult{}, false, k, rowStr(repairStep.Data, "input"), nil
		}
		return CallResult{}, false, k, task, nil
	case h.unbound != "" && st.kind == stagePublished:
		// the held attempt ended with its held row (evidenceUnbound): its subagent is closed again, in case the process that held the
		// call died before it closed it, its record released, and the step goes to a new attempt
		c.sessionClose(h.unbound, st.attempt)
		c.evidenceDrop()
		s.attempt = maxAttempt + 1
		s.note, s.lastText = c.heldNote(h.reason), st.prev
		c.firstK = max(st.k, 1)
		if st.repair {
			c.firstK = max(st.k-1, 1)
		}
		return CallResult{}, false, max(st.k, 1), st.input, nil
	case st.kind == stagePublished:
		s.resume = st
		s.attempt = st.attempt
		s.note, s.lastText = st.note, st.prev
		c.firstK = max(st.k, 1)
		if st.repair {
			c.firstK = max(st.k-1, 1)
		}
		return CallResult{}, false, max(st.k, 1), st.input, nil
	}
	c.firstK = 1
	c.evidenceDrop()
	s.attempt = maxAttempt + 1
	return CallResult{}, false, 1, c.checkpointPreface() + p.prompt, nil
}

// lostNote is what the packet of the attempt after a lost one says.
func (c *callRun) lostNote(agent string) string {
	note := fmt.Sprintf("The previous attempt at this task (subagent %s) was cut off before it answered.", agent)
	if c.writesNodeWorktree() {
		return note + fmt.Sprintf(" Your workspace already holds what it did: run `git -C %s status` and `git -C %s diff` first, and finish the task from there.", c.cwd, c.cwd)
	}
	return note + " Do the task from the start, from this packet."
}

// heldNote is what the packet of the attempt that goes on with a held call says.
func (c *callRun) heldNote(reason string) string {
	note := fmt.Sprintf("The previous attempt at this task stopped for %s before it answered. Continue the same task; the output contract is unchanged.", reason)
	if c.writesNodeWorktree() {
		return note + fmt.Sprintf(" Your workspace already holds what it did: run `git -C %s status` and `git -C %s diff` first, and finish the task from there.", c.cwd, c.cwd)
	}
	return note
}

// sessionTurn runs turn k of a session call to its outcome: the step, the bound subagent, the harvest, and a new attempt for each
// lost one. It returns the outcome, or the terminal result of a call that stopped (a gate, a journal that cannot be written).
func (c *callRun) sessionTurn(ctx context.Context, k int, op, input string, repair bool) (TurnOutcome, CallResult, bool, error) {
	s := c.sess
	if !repair && s.task == "" {
		s.task = input
	}
	turnCtx, endTurn := c.beginTurn(ctx, op)
	defer endTurn()
	for {
		if s.agent == "" || s.boundK != k {
			out, res, stop, err := c.sessionBind(turnCtx, ctx, k, op, input, repair)
			if stop || err != nil || out != nil {
				if out == nil {
					out = &TurnOutcome{}
				}
				return *out, res, stop, err
			}
		}
		out, loss, err := c.sessionHarvest(turnCtx, ctx)
		if err != nil || loss == nil {
			return out, CallResult{}, false, err
		}
		c.sessionLose(loss)
		if res, stop := c.gate(ctx); stop {
			return TurnOutcome{}, res, true, nil
		}
	}
}

// sessionLoss is a lost attempt as the harvest found it.
type sessionLoss struct {
	reason, hostStatus, turnStatus string
}

// sessionLose records a lost attempt, closes its subagent and readies the next attempt.
func (c *callRun) sessionLose(l *sessionLoss) {
	s := c.sess
	c.event(EvSessionAttemptLost, map[string]any{"attempt": s.attempt, "agent_id": s.agent, "k": s.boundK, "turn_id": s.turn, "reason": l.reason,
		"host_status": l.hostStatus, "turn_status": l.turnStatus})
	c.sessionClose(s.agent, s.attempt)
	c.evidenceDrop()
	s.note = c.lostNote(s.agent)
	s.prior = append(s.prior, s.pair)
	s.attempt++
	s.agent, s.turn, s.boundK = "", "", 0
}

// sessionClose publishes the step that closes a subagent.
func (c *callRun) sessionClose(agent string, attempt int) {
	if agent == "" {
		return
	}
	_ = c.e.cfg.Session.Bus.Publish(Step{Run: c.e.runID, Call: c.id, Attempt: attempt, Role: c.plan.role, Action: StepClose, AgentID: agent})
}

// sessionEnd closes the subagent of a call that has its terminal row, and removes the attempt's unclaimed evidence record: a call
// held with its step published and no subagent bound keeps it, because the owner may still spawn for that step and the held call
// that goes on publishes it again with that record (sessionResumeHeld); a held call whose subagent is closed here goes on with a
// new attempt, which records its own. A call held because the subagent that took its step could not be bound released its record
// already (evidenceUnbound).
func (c *callRun) sessionEnd(res CallResult) {
	if c.sess == nil || res.Status == StatusUnknown {
		return
	}
	if res.Status != StatusHeld || c.sess.agent != "" {
		c.evidenceDrop()
	}
	c.sessionClose(c.sess.agent, c.sess.attempt)
}

// sessionBind publishes the step of turn k (once: a step its rows already publish is shown again) and waits until a subagent is
// bound to it. out is set when the turn ended before anything was bound (an interrupt).
func (c *callRun) sessionBind(turnCtx, ctx context.Context, k int, op, input string, repair bool) (*TurnOutcome, CallResult, bool, error) {
	e, s, p := c.e, c.sess, c.plan
	sc := e.cfg.Session
	marker := sessionOp(op, s.attempt)
	action, agent := StepSpawn, ""
	if repair && s.agent != "" {
		action, agent = StepSendInput, s.agent
	}
	packetName := PacketName(k, s.attempt)
	// the turn's place in the call's conversation, which its step and its turn record: the task turn or its repair, over the turn the
	// conversation holds there (a lost attempt's turn, the turn a held call goes on from in place)
	s.conv = conversationTurn(repair)
	s.replaces = callConversation(CallEvents(e.j, c.id)).at(s.conv)
	recovered := s.resume != nil && s.resume.kind == stagePublished && s.resume.k == k && s.resume.attempt == s.attempt
	if recovered {
		st := s.resume
		s.resume = nil
		action, agent = st.action, st.agent
		if action == StepSendInput && agent == "" {
			action = StepSpawn
		}
		if action == StepSpawn && agent != "" {
			// the attempt's thread_started names its subagent: it is adopted, never spawned again, and goes on with the evidence record
			// its step was published with (open, or claimed by the subagent before the restart)
			if c.evidenceGated() {
				if err := c.ensureEvidence(turnCtx, marker); err != nil {
					return nil, c.evidenceHold(err, ""), true, nil
				}
			}
			return c.sessionAdopt(turnCtx, ctx, k, op, input, repair, st)
		}
	}
	if action == StepSpawn && c.evidenceGated() {
		// the step's evidence record is written before its step_published row, and a step published again reuses it (CRW-1482)
		if err := c.ensureEvidence(turnCtx, marker); err != nil {
			return nil, c.evidenceHold(err, ""), true, nil
		}
	}
	packet := c.sessionPacket(k, marker, input, repair, action)
	step := Step{Run: e.runID, Call: c.id, K: k, Attempt: s.attempt, Op: marker, Role: p.role, Action: action, AgentType: agentTypeOf(p.role),
		Model: p.spec.Pair.Model, ReasoningEffort: p.spec.Pair.Effort, Packet: sc.Bus.FilePath(c.id, packetName), Workspace: c.cwd, AgentID: agent}
	if action == StepSpawn {
		step.Message = c.spawnMessage(marker, step.Packet)
	} else {
		step.Message = "crw-flow-op: " + marker + "\n\n" + input
	}
	if !recovered {
		data := map[string]any{"k": k, "op": op, "marker": marker, "attempt": s.attempt, "action": action, "input": input, "repair": repair,
			"input_digest": digestJSON([]byte(input)), "packet": step.Packet, "packet_digest": digestJSON([]byte(packet)), "role": p.role,
			"agent_type": step.AgentType, "model": step.Model, "effort": step.ReasoningEffort, "workspace": c.cwd, "conversation_turn": s.conv}
		if s.replaces != "" {
			data["replaces_turn"] = s.replaces
		}
		if agent != "" {
			data["agent_id"] = agent
		}
		if action == StepSpawn {
			s.evidenceStepData(data)
		}
		if action == StepSendInput {
			// what the turns before it counted: a process that recovers this turn counts only the tokens it adds
			data["tokens_seen"], data["call_tokens"] = s.seen, c.tokens
		}
		if s.note != "" {
			data["note"] = s.note
		}
		if repair && s.lastText != "" {
			// the answer a repair turn repairs, on a send_input too: a later attempt of the turn (a new subagent after a restart) names it
			// in its packet (CRW-1384)
			data["previous_answer"] = s.lastText
		}
		c.event(EvStepPublished, data)
		if res, down := c.journalDown(); down {
			return nil, res, true, nil
		}
	}
	w := e.sessR.register(c.id, k, s.attempt)
	defer e.sessR.unregister(c.id, w)
	defer func() { _ = sc.Bus.Retract(c.id, k, s.attempt) }()
	// the step waits for the owner from its publication to its bind (or the end of the turn): the waker reads it (§10)
	bound := false
	defer func() { e.sessW.taken(marker, bound, e.cfg.Now()) }()
	if recovered && c.sessionTaken(turnCtx, w, k, op, marker, action) {
		// the owner took the step before this process published it again: it is bound, never spawned or sent again
		bound = true
		return nil, CallResult{}, false, nil
	}
	if res, held := c.evidenceUnbound(); held {
		return nil, res, true, nil
	}
	// a step that cannot be written (a full disk) was sent to nobody: it is recorded as not dispatched and written again, and the
	// call waits as for any step (CRW-1367; never an unknown outcome or a hold of the node)
	// A failure that does not pass (EACCES on the steps directory) is still written again at the poll, and the record shows how long:
	// the warning is written at the 1st, 10th, 100th, ... failure with the count and the first and last time, and a closing one when
	// the step is written or the turn ends (CRW-1392).
	unwritten := stepUnwritten{c: c, k: k, attempt: s.attempt, marker: marker, action: action}
	for {
		_, err := sc.Bus.WriteFile(c.id, packetName, []byte(packet))
		if err == nil {
			err = sc.Bus.Publish(step)
		}
		if err == nil {
			unwritten.close(true)
			break
		}
		unwritten.failed(err, e.cfg.Now())
		if sc.wait(turnCtx, sc.poll()) != nil && turnCtx.Err() != nil {
			unwritten.close(false)
			out, _, err := c.sessionStop(ctx, &stopStep{k: k, op: op, marker: marker})
			return &out, CallResult{}, false, err
		}
	}
	e.sessW.published(marker, e.cfg.Now())
	find := time.NewTicker(6 * sc.poll())
	defer find.Stop()
	for {
		select {
		case rep := <-w.ch:
			reply, ok := c.sessionVerify(turnCtx, rep.Agent, k, op, marker, action)
			_ = sc.Bus.Reply(rep, reply)
			e.sessR.answered(rep.ID)
			if ok {
				bound = true
				return nil, CallResult{}, false, nil
			}
			if res, held := c.evidenceUnbound(); held {
				return nil, res, true, nil
			}
		case <-find.C:
			if c.sessionFind(turnCtx, k, op, marker, action, false) {
				bound = true
				return nil, CallResult{}, false, nil
			}
			if res, held := c.evidenceUnbound(); held {
				return nil, res, true, nil
			}
		case <-turnCtx.Done():
			bound = c.sessionBindStopped(ctx, k, op, marker)
			out, _, err := c.sessionStop(ctx, &stopStep{k: k, op: op, marker: marker})
			bound = bound || s.boundK == k && s.turn != ""
			return &out, CallResult{}, false, err
		}
	}
}

// sessionBindStopped binds, before a stop interrupts it, the turn that the subagent a step was for (a send_input's, an adopted
// one's) started for the step, so the rows name the turn the engine's interrupt reaches: the one turn an interrupt_requested row
// decides for a process that resumes the run (sessionIntent). A spawn step no subagent was bound to has none. It says whether it
// bound one.
func (c *callRun) sessionBindStopped(ctx context.Context, k int, op, marker string) bool {
	s := c.sess
	if s.agent == "" || s.boundK == k && s.turn != "" {
		return false
	}
	vctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	_, ok := c.sessionVerify(vctx, s.agent, k, op, marker, StepSendInput)
	return ok
}

// stepUnwritten is the record of a step the bus could not write: step_unwritten flow rows (input_state not_delivered, nothing was
// sent) at the 1st, 10th, 100th, ... failure, each with the failures so far and the first and last time, and a closing row whenever
// a failure row was written: `written: false` when the turn ends with the step unwritten, `written: true` when the step is written,
// after any number of failures.
type stepUnwritten struct {
	c               *callRun
	k, attempt      int
	marker, action  string
	failures, shown int
	first, last     time.Time
	lastErr         string
}

func (u *stepUnwritten) failed(err error, now time.Time) {
	u.failures++
	if u.failures == 1 {
		u.first = now
	}
	u.last, u.lastErr = now, err.Error()
	if u.failures == 1 || u.failures == u.shown*10 {
		u.row(nil)
	}
}

func (u *stepUnwritten) close(written bool) {
	if u.shown > 0 {
		u.row(&written)
	}
}

func (u *stepUnwritten) row(written *bool) {
	c := u.c
	e := c.e
	u.shown = u.failures
	data := map[string]any{"call_id": c.id, "node": c.plan.node, "k": u.k, "attempt": u.attempt, "marker": u.marker, "action": u.action, "input_state": "not_delivered",
		"error": u.lastErr, "failures": u.failures, "first_at": u.first.UTC().Format(time.RFC3339Nano), "last_at": u.last.UTC().Format(time.RFC3339Nano)}
	if written != nil {
		data["written"] = *written
	}
	e.j.Append(Event{RunID: e.runID, Kind: KindFlow, CallID: c.id, Path: c.path.id, Position: c.position, Op: "agent", Label: c.label, Event: EvStepUnwritten, Data: data})
}

// sessionTaken looks for what took a step a dead process published before this process shows it again: a report the owner wrote
// and the dead process never answered (it waits in the inbox), then the host (sessionFind). It is true when the step is bound.
func (c *callRun) sessionTaken(ctx context.Context, w *sessionWaiter, k int, op, marker, action string) bool {
	e, sc := c.e, c.e.cfg.Session
	if reps, err := sc.Bus.Reports(); err == nil {
		for _, rep := range reps {
			if rep.Run == e.runID && rep.Call == c.id && rep.K == k && rep.Attempt == c.sess.attempt {
				e.route(rep)
			}
		}
	}
	for drained := false; !drained; {
		select {
		case rep := <-w.ch:
			reply, ok := c.sessionVerify(ctx, rep.Agent, k, op, marker, action)
			_ = sc.Bus.Reply(rep, reply)
			e.sessR.answered(rep.ID)
			if ok {
				return true
			}
		default:
			drained = true
		}
	}
	return c.sessionFind(ctx, k, op, marker, action, true)
}

// sessionFind looks a step up on the host: for a spawn, the owner thread's subagent whose turn carries the marker (a pair_mismatch
// one is closed when closeMismatch is set); for a send_input (a repair), the marker turn on the subagent the input was for, so a
// repair the subagent got is bound and never sent again (CRW-1367). It is true when the step is bound.
func (c *callRun) sessionFind(ctx context.Context, k int, op, marker, action string, closeMismatch bool) bool {
	sc := c.e.cfg.Session
	switch action {
	case StepSpawn:
		id, found, err := sc.Host.FindByMarker(ctx, sc.Owner, marker)
		if err != nil || !found {
			return false
		}
		reply, ok := c.sessionVerify(ctx, id, k, op, marker, action)
		if !ok && closeMismatch && reply.Status == ReplyPairMismatch {
			c.sessionClose(id, c.sess.attempt)
		}
		return ok
	case StepSendInput:
		_, ok := c.sessionVerify(ctx, c.sess.agent, k, op, marker, action)
		return ok
	}
	return false
}

// sessionAdopt goes on with an attempt whose subagent a process bound and then died before it wrote turn_started: the subagent its
// thread_started row records is the attempt's, so nothing is spawned or published again and only turn_started is written, as a
// send_input bind writes it (one thread_started per attempt). A recorded subagent the host does not know, or shows notLoaded or
// systemError, for LostAfter loses the attempt, as the harvest judges a bound one; the gate decides the next attempt, which spawns.
func (c *callRun) sessionAdopt(turnCtx, ctx context.Context, k int, op, input string, repair bool, st *sessionStage) (*TurnOutcome, CallResult, bool, error) {
	s, sc, now := c.sess, c.e.cfg.Session, c.e.cfg.Now
	s.agent, s.rollout, s.pair, s.seen = st.agent, st.rollout, st.pair, Tokens{}
	s.dropCurrentPair()
	marker, attempt := sessionOp(op, s.attempt), s.attempt
	defer func() { _ = sc.Bus.Retract(c.id, k, attempt) }()
	var gone time.Time
	for {
		if turnCtx.Err() != nil {
			c.sessionBindStopped(ctx, k, op, marker)
			out, _, err := c.sessionStop(ctx, &stopStep{k: k, op: op, marker: marker})
			return &out, CallResult{}, false, err
		}
		if _, ok := c.sessionVerify(turnCtx, s.agent, k, op, marker, StepSendInput); ok {
			return nil, CallResult{}, false, nil
		}
		// the subagent is gone as the harvest judges a bound one (hostGone): unknown to the host, or a thread it shows notLoaded or
		// systemError (CRW-1392). A read the host cannot answer judges nothing: the time it has been gone stays as it is, as in the
		// harvest (sessionRead)
		ag, err := sc.Host.ReadAgent(turnCtx, s.agent)
		status, judged := "", true
		switch {
		case errors.Is(err, ErrSessionAgentNotFound):
			status = "notFound"
		case err != nil:
			judged = false
		case hostGone(ag.Status):
			status = ag.Status
		}
		switch {
		case !judged:
		case status == "":
			gone = time.Time{}
		case gone.IsZero():
			gone = now()
		case now().Sub(gone) >= sc.lostAfter():
			s.boundK = k
			c.sessionLose(&sessionLoss{reason: fmt.Sprintf("the subagent bound before the restart has been %s on the host for %s", status, sc.lostAfter()), hostStatus: status})
			// the next attempt is a new request: the gate decides it, as after a loss the harvest found (CRW-1392)
			if res, stop := c.gate(ctx); stop {
				return nil, res, true, nil
			}
			return c.sessionBind(turnCtx, ctx, k, op, input, repair)
		}
		_ = sc.wait(turnCtx, sc.poll())
	}
}

// spawnMessage is the spawn_agent message of a step (§3.1): the marker, the role and the directory, and the packet that holds the rest.
// A step with an evidence assignment ends with its [CRW-EVIDENCE-ASSIGNMENT:<id>] block and the receipt file (CRW-1482), after the
// text of before, so every reader of the first lines reads them as before.
func (c *callRun) spawnMessage(marker, packet string) string {
	return "crw-flow-op: " + marker + "\nCRW-ROLE: " + c.plan.role + "\n\n" +
		"Your shell starts in a directory that is NOT your workspace. Your workspace is " + c.cwd + ": run every command with workdir " + c.cwd +
		" and edit only by absolute paths under " + c.cwd + ".\nRead the task packet " + packet + " in full and follow it exactly. You are a leaf subagent: never spawn agents.\n" +
		"Your final message is exactly the JSON object the packet names, and nothing else." + c.sess.evidenceMessage(marker)
}

// sessionVerify checks the subagent a report (or a marker search) names against the step and, when it holds, binds it: the rows
// of the bind, thread_started (a spawn) and turn_started, are written together. The reply says what the command is to do.
func (c *callRun) sessionVerify(ctx context.Context, agent string, k int, op, marker, action string) (DispatchReply, bool) {
	e, s, p := c.e, c.sess, c.plan
	sc := e.cfg.Session
	if action == StepSendInput && agent != s.agent {
		return DispatchReply{Status: ReplyRefused, Detail: fmt.Sprintf("the step sends input to %s, not to %s", s.agent, agent)}, false
	}
	ag, err := sc.Host.ReadAgent(ctx, agent)
	if err != nil {
		return DispatchReply{Status: ReplyRetry, Detail: fmt.Sprintf("the host cannot read %s: %v", agent, err)}, false
	}
	if ag.ParentThreadID != sc.Owner {
		return DispatchReply{Status: ReplyRefused, Detail: fmt.Sprintf("%s is not a subagent of the run's owner thread %s (its parent is %q)", agent, sc.Owner, ag.ParentThreadID)}, false
	}
	turns, err := sc.Host.Turns(ctx, agent, 0, false)
	if err != nil {
		return DispatchReply{Status: ReplyRetry, Detail: fmt.Sprintf("the turns of %s cannot be read: %v", agent, err)}, false
	}
	var turn *SessionTurn
	for i := range turns {
		if first, _, _ := strings.Cut(turns[i].FirstUserText, "\n"); first == "crw-flow-op: "+marker {
			turn = &turns[i]
			break
		}
	}
	if turn == nil {
		return DispatchReply{Status: ReplyRetry, Detail: fmt.Sprintf("no turn of %s carries crw-flow-op: %s yet", agent, marker)}, false
	}
	want := p.spec.Pair
	got := Pair{Model: ag.Model, Effort: ag.Effort}
	if action == StepSpawn && !sessionPairBinds(want, got) {
		return DispatchReply{Status: ReplyPairMismatch, Close: agent,
			Detail: fmt.Sprintf("pair_mismatch: close %s and spawn again with %s/%s (it runs %s/%s)", agent, want.Model, want.Effort, got.Model, got.Effort)}, false
	}
	if t := agentTypeOf(p.role); ag.AgentRole != "" && ag.AgentRole != t {
		e.j.Append(Event{RunID: e.runID, Kind: KindFlow, CallID: c.id, Path: c.path.id, Position: c.position, Op: "agent", Label: c.label, Event: "session_agent_role_mismatch",
			Data: map[string]any{"call_id": c.id, "agent_id": agent, "agent_role": ag.AgentRole, "agent_type": t}})
	}
	if action == StepSpawn {
		data := map[string]any{}
		if reply, ok := c.evidenceBind(agent, marker, ag.Cwd, data); !ok {
			return reply, false
		}
		if sc.Trace != nil {
			if d, err := sc.Trace(ctx, c.cwd); err == nil {
				s.trace = &d
			}
		}
		for key, v := range map[string]any{"thread_id": agent, "model": ag.Model, "effort": ag.Effort, "cwd": c.cwd, "host_cwd": ag.Cwd, "parent_thread_id": ag.ParentThreadID,
			"agent_role": ag.AgentRole, "attempt": s.attempt, "via": "session", "rollout_path": ag.RolloutPath, "marker": marker} {
			data[key] = v
		}
		switch {
		case want.Model == "":
			data["pair_unchecked"] = true
		case want.Effort == "":
			data["effort_unchecked"] = true
		}
		if s.trace != nil {
			data["trace"] = map[string]any{"workspace": s.trace.Workspace, "outside": s.trace.Outside, "shared": s.trace.Shared}
		}
		var prior []map[string]any
		seen := map[Pair]bool{got: true}
		for _, pp := range s.prior {
			if !seen[pp] && pp != (Pair{}) {
				seen[pp] = true
				prior = append(prior, map[string]any{"model": pp.Model, "effort": pp.Effort})
			}
		}
		if len(prior) > 0 {
			data["prior_pairs"] = prior
		}
		c.event(EvThreadStarted, data)
		c.threadID, c.configured = agent, got
		s.agent, s.rollout, s.pair, s.seen = agent, ag.RolloutPath, got, Tokens{}
	}
	c.turnIDs = append(c.turnIDs, turn.ID)
	c.delivered = true
	s.turn, s.boundK = turn.ID, k
	data := map[string]any{"k": k, "op": op, "turn_id": turn.ID, "attempt": s.attempt, "marker": marker, "via": "session", "conversation_turn": s.conv}
	if s.replaces != "" {
		data["replaces_turn"] = s.replaces
	}
	c.event(EvTurnStarted, data)
	return DispatchReply{Status: ReplyBound}, true
}

// sessionPairBinds is the pair rule of a bind and of a continuation's handle: only the decided parts of the role's pair are
// compared. A role whose model the role table leaves to the host binds any subagent (pair_unchecked), a role whose model is set
// and whose effort is undecided binds a subagent of its model at the effort the host applied (effort_unchecked), and any other
// role binds a subagent of its pair only.
func sessionPairBinds(want, got Pair) bool {
	switch {
	case want.Model == "":
		return true
	case want.Effort == "":
		return got.Model == want.Model
	}
	return got == want
}

// sessionHarvest reads the bound subagent until its turn ends (§3.3) and returns the outcome, or the loss of the attempt (§5): the
// marker turn is not completed, the thread is notLoaded, systemError or unknown to the host, and the turn was interrupted or failed
// without an error the host names, or has not changed for LostAfter. A thread the host cannot be asked about is not judged, and a
// subagent that is active is never lost.
func (c *callRun) sessionHarvest(turnCtx, ctx context.Context) (TurnOutcome, *sessionLoss, error) {
	sc, now := c.e.cfg.Session, c.e.cfg.Now
	lastKey, changed := "", now()
	for {
		if turnCtx.Err() != nil {
			return c.sessionStop(ctx, nil)
		}
		status, turn, ok := c.sessionRead(turnCtx)
		if ok {
			key := status
			if turn != nil {
				key += "|" + turn.Status + "|" + turn.UpdatedAt.String()
			}
			if key != lastKey {
				lastKey, changed = key, now()
			}
			turnStatus := ""
			if turn != nil {
				turnStatus = turn.Status
			}
			switch {
			case turn != nil && turn.Status == "completed":
				if out, ok := c.sessionOutcome(turnCtx, turn.ID); ok {
					return out, nil, nil
				}
			case turn != nil && (turn.Status == "failed" || turn.Status == "interrupted") && turn.ErrorInfo == "" && hostGone(status):
				// confirmed once more: a thread the host shows again is judged on that read, not lost
				if again, t2, ok := c.sessionRead(turnCtx); ok && hostGone(again) && (t2 == nil || t2.Status != "completed") {
					return TurnOutcome{}, &sessionLoss{reason: fmt.Sprintf("the turn ended %s on a thread the host shows %s", turn.Status, status), hostStatus: status, turnStatus: turnStatus}, nil
				}
			case turn != nil && turn.Status == "failed":
				return TurnOutcome{TurnID: turn.ID, Failed: &TurnFailure{CodexErrorInfo: turn.ErrorInfo, Message: turn.ErrorMessage}}, nil, nil
			case turn != nil && turn.Status == "interrupted":
				return TurnOutcome{TurnID: turn.ID, Failed: &TurnFailure{Interrupted: true, CodexErrorInfo: turn.ErrorInfo, Message: turn.ErrorMessage}}, nil, nil
			case hostGone(status) && now().Sub(changed) >= sc.lostAfter():
				// confirmed once more: a subagent that shows active again is alive
				if again, _, ok := c.sessionRead(turnCtx); ok && hostGone(again) {
					return TurnOutcome{}, &sessionLoss{reason: fmt.Sprintf("the thread is %s and its turn has not changed for %s", status, sc.lostAfter()), hostStatus: status, turnStatus: turnStatus}, nil
				}
				changed = now()
			}
		}
		if err := sc.wait(turnCtx, sc.poll()); err != nil {
			continue
		}
	}
}

// sessionRead reads the bound subagent's status and its bound turn; ok is false when the host could not be asked.
func (c *callRun) sessionRead(ctx context.Context) (status string, turn *SessionTurn, ok bool) {
	s, host := c.sess, c.e.cfg.Session.Host
	ag, err := host.ReadAgent(ctx, s.agent)
	switch {
	case errors.Is(err, ErrSessionAgentNotFound):
		return "notFound", nil, true
	case err != nil:
		return "", nil, false
	}
	for _, limit := range []int{2, 0} {
		turns, err := host.Turns(ctx, s.agent, limit, false)
		if errors.Is(err, ErrSessionAgentNotFound) {
			return "notFound", nil, true
		}
		if err != nil {
			return "", nil, false
		}
		for i := range turns {
			if turns[i].ID == s.turn {
				return ag.Status, &turns[i], true
			}
		}
	}
	return ag.Status, nil, true
}

// sessionOutcome reads a completed turn in full and makes it the turn's outcome: its answer (§3.3; a commentary message is none)
// and the tokens the rollout's token_count adds since the subagent's last harvest.
func (c *callRun) sessionOutcome(ctx context.Context, turnID string) (TurnOutcome, bool) {
	s, host := c.sess, c.e.cfg.Session.Host
	turns, err := host.Turns(ctx, s.agent, 0, true)
	if err != nil {
		return TurnOutcome{}, false
	}
	for _, t := range turns {
		if t.ID != turnID {
			continue
		}
		out := TurnOutcome{TurnID: t.ID}
		if t.FinalPhase != "commentary" {
			out.Text = t.FinalText
		}
		s.lastText = out.Text
		if s.rollout == "" {
			out.TokensUnmeasured = true
			return out, true
		}
		total, ok := host.Tokens(ctx, s.rollout)
		if !ok {
			out.TokensUnmeasured = true
			return out, true
		}
		out.Tokens = total.Sub(s.seen)
		s.seen = total
		return out, true
	}
	return TurnOutcome{}, false
}

// stopStep is the step a stop came to before a turn of it was bound (sessionBind, sessionAdopt): the turn the subagent runs for it
// carries its marker.
type stopStep struct {
	k          int
	op, marker string
}

// sessionStop ends a turn the engine stopped (a pause, a cancellation, a deadline): the bound subagent's turn is interrupted and
// the thread read until it is no longer active, so a held row is never written beside a subagent that still works. A turn that
// was not bound yet has nothing to interrupt. Every turn the stop interrupts is bound first and the rows are stored before the
// interrupt is sent: the running turns are read once, and a running turn of the step the stop came to (step, nil when the call's
// turn was bound) that no row binds yet - a send_input turn the subagent started after a stop's bind read (sessionBindStopped) or
// while the bus could not write the step - is bound here with the conversation place its step recorded (CRW-1405). The turn_started
// a stop binds, or a bind wrote after the interrupt_requested row, is the turn that row decides for a process that resumes the run,
// and a journal that batches its rows would lose it with a process that dies once the interrupt is sent. A running turn the stop
// cannot bind (no row of the call names it, it carries no marker of the step) is not interrupted: the resumed process would read
// its end as an interrupt nobody asked for.
func (c *callRun) sessionStop(ctx context.Context, step *stopStep) (TurnOutcome, *sessionLoss, error) {
	s, sc := c.sess, c.e.cfg.Session
	if s.agent != "" {
		ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		var interrupt []string
		if turns, err := sc.Host.Turns(ictx, s.agent, 0, false); err == nil {
			for _, t := range turns {
				if t.Status == "inProgress" && c.sessionStopBinds(ictx, step, t) {
					interrupt = append(interrupt, t.ID)
				}
			}
		} else if s.turn != "" {
			interrupt = []string{s.turn}
		}
		if err := journalFlush(c.e.j); err != nil {
			c.e.journalFailed(err)
		}
		for _, id := range interrupt {
			_ = sc.Host.Interrupt(ictx, s.agent, id)
		}
		for i := 0; i < 50; i++ {
			ag, err := sc.Host.ReadAgent(ictx, s.agent)
			if (err == nil && ag.Status != "active") || errors.Is(err, ErrSessionAgentNotFound) {
				break
			}
			if sc.wait(ictx, sc.poll()) != nil {
				break
			}
		}
		cancel()
	}
	if c.aborted(ctx) {
		return TurnOutcome{}, nil, &TransportError{Err: context.Cause(ctx)}
	}
	return TurnOutcome{TurnID: s.turn, Failed: &TurnFailure{Interrupted: true, Message: "the engine interrupted the subagent's turn"}}, nil, nil
}

// sessionStopBinds says whether a running turn of the subagent is bound, binding it when it is the turn of the step the stop came to
// and that step is bound to no turn yet. A step bound to a turn keeps it: another running turn that carries the step's marker (the
// input sent twice) is no turn of the call's rows, so it is neither bound again - a second turn_started that moves the step to it -
// nor interrupted (CRW-1409).
func (c *callRun) sessionStopBinds(ctx context.Context, step *stopStep, t SessionTurn) bool {
	s := c.sess
	if t.ID == s.turn {
		return true
	}
	if step == nil || s.boundK == step.k && s.turn != "" {
		return false
	}
	if first, _, _ := strings.Cut(t.FirstUserText, "\n"); first != "crw-flow-op: "+step.marker {
		return false
	}
	_, ok := c.sessionVerify(ctx, s.agent, step.k, step.op, step.marker, StepSendInput)
	return ok && s.turn == t.ID
}

// sessionQuiesce makes sure no subagent of a call's rows still runs: each bound one (and with unbound, the subagent a published step
// that was never bound may have got, found by its marker) is read, a running turn of it interrupted and the thread read until it is
// no longer active, and a close step is published for it. An error means that could not be shown.
func (e *Engine) sessionQuiesce(ctx context.Context, callID string, rows []Event, unbound bool) error {
	sc := e.cfg.Session
	type sub struct {
		agent   string
		attempt int
	}
	var subs []sub
	seen := map[string]bool{}
	var markers []sub
	for _, ev := range rows {
		switch {
		case ev.Event == EvThreadStarted && rowStr(ev.Data, "via") == "session":
			if a := rowStr(ev.Data, "thread_id"); a != "" && !seen[a] {
				seen[a] = true
				subs = append(subs, sub{a, rowInt(ev.Data, "attempt")})
			}
		case ev.Event == EvStepPublished && rowStr(ev.Data, "action") == StepSpawn:
			markers = append(markers, sub{rowStr(ev.Data, "marker"), rowInt(ev.Data, "attempt")})
		}
	}
	if unbound {
		for _, m := range markers {
			id, found, err := sc.Host.FindByMarker(ctx, sc.Owner, m.agent)
			if err != nil {
				return fmt.Errorf("the subagent of %s cannot be looked up: %w", m.agent, err)
			}
			if found && !seen[id] {
				seen[id] = true
				subs = append(subs, sub{id, m.attempt})
			}
		}
	}
	for _, sb := range subs {
		ag, err := sc.Host.ReadAgent(ctx, sb.agent)
		if errors.Is(err, ErrSessionAgentNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("subagent %s cannot be read: %w", sb.agent, err)
		}
		if ag.Status == "active" {
			turns, err := sc.Host.Turns(ctx, sb.agent, 0, false)
			if err != nil {
				return fmt.Errorf("the turns of subagent %s cannot be read: %w", sb.agent, err)
			}
			for _, t := range turns {
				if t.Status == "inProgress" {
					if err := sc.Host.Interrupt(ctx, sb.agent, t.ID); err != nil {
						return fmt.Errorf("turn %s of subagent %s could not be interrupted: %w", t.ID, sb.agent, err)
					}
				}
			}
			inactive := false
			for i := 0; i < 50 && !inactive; i++ {
				if a, err := sc.Host.ReadAgent(ctx, sb.agent); (err == nil && a.Status != "active") || errors.Is(err, ErrSessionAgentNotFound) {
					inactive = true
					break
				}
				if err := sc.wait(ctx, sc.poll()); err != nil {
					return err
				}
			}
			if !inactive {
				return fmt.Errorf("subagent %s is still active after its turn was interrupted", sb.agent)
			}
		}
		_ = sc.Bus.Publish(Step{Run: e.runID, Call: callID, Attempt: sb.attempt, Action: StepClose, AgentID: sb.agent})
	}
	return nil
}

// sessionChecks are the record-time checks of a completed session turn (§6): the receipt its evidence assignment holds, the trace
// digests compared with the bind's, and an executor's HEAD compared with the commit its attempt started from.
func (c *callRun) sessionChecks(ctx context.Context, data map[string]any) {
	e, s, sc := c.e, c.sess, c.e.cfg.Session
	// the receipt the stop hook accepted, for every call with an evidence assignment (the QA capture call too, which writes no node
	// worktree): what R2's acceptance reads (CRW-1482)
	c.evidenceReceipt(data)
	if sc.Trace != nil && s.trace != nil {
		if now, err := sc.Trace(ctx, c.cwd); err == nil {
			warn := func(event, detail string) {
				e.j.Append(Event{RunID: e.runID, Kind: KindFlow, CallID: c.id, Path: c.path.id, Position: c.position, Op: "agent", Label: c.label, Event: event,
					Data: map[string]any{"call_id": c.id, "node": c.plan.node, "role": c.plan.role, "agent_id": s.agent, "workspace": c.cwd, "detail": detail,
						"before": map[string]any{"workspace": s.trace.Workspace, "outside": s.trace.Outside, "shared": s.trace.Shared},
						"after":  map[string]any{"workspace": now.Workspace, "outside": now.Outside, "shared": now.Shared}}})
			}
			switch {
			case readsOnly(c.plan.spec.Family) && (now.Workspace != s.trace.Workspace || now.Outside != s.trace.Outside):
				warn("read_role_wrote", "a role that changes nothing left its directory or the owner's checkout changed")
			case !readsOnly(c.plan.spec.Family) && now.Outside != s.trace.Outside:
				warn("executor_wrote_outside", "the owner's checkout changed while the executor worked: an edit outside P is not part of the pending tree")
			}
			if now.Shared != s.trace.Shared {
				warn("shared_state_changed", "the run's shared refs or flow-bound directory changed while the subagent worked")
			}
		}
	}
	if !c.writesNodeWorktree() {
		return
	}
	keeper, ok := e.cfg.Backend.(HeadKeeper)
	if !ok {
		return
	}
	start, attempt := c.startCommit()
	if start == "" {
		return
	}
	head, err := keeper.HeadOf(ctx, c.cwd)
	if err != nil {
		data["executor_head_error"] = err.Error()
		return
	}
	if head == start {
		return
	}
	ref := StrayRef(e.runID, c.plan.node, attempt, c.id)
	moved := map[string]any{"start": start, "head": head, "ref": ref}
	if err := keeper.KeepRef(ctx, ref, head); err != nil {
		moved["ref_error"] = err.Error()
	}
	data["executor_head_moved"] = moved
}

// startCommit is the commit an executor call's worktree is to be at when its turn ends: the result of the node's last crw.commit
// after the allocation it works in was recorded, else that allocation's start commit; and that allocation's attempt.
func (c *callRun) startCommit() (string, int) {
	node := c.plan.node
	start, attempt := "", 0
	if pl := c.placement; pl != nil {
		start, attempt = pl.Start, pl.Attempt
	}
	for _, ev := range c.e.j.Events() {
		switch {
		case ev.Kind == KindCall && ev.Op == OpCommit && ev.Event == EvCompleted && rowStr(ev.Data, "node") == node:
			if cm := rowStr(ev.Data, "commit"); cm != "" {
				start = cm
			}
		case ev.Kind == KindCall && ev.Op == "agent" && ev.Event == EvPrepared && rowStr(ev.Data, "node") == node:
			if m, ok := ev.Data["allocation"].(map[string]any); ok && rowStr(m, "path") == c.cwd && (rowStr(m, "kind") == PlaceFresh || rowStr(m, "kind") == PlaceRestore) {
				// the allocation the worktree was made (or restored) for starts at its own start commit: a crw.commit of the node
				// recorded before it (an earlier allocation's, a superseded process's past a suffix's boundary) is not its start,
				// and only a commit after it moves it (CRW-1384)
				if sc := rowStr(m, "start_commit"); sc != "" {
					start = sc
				}
				attempt = rowInt(m, "attempt")
			}
		}
	}
	return start, attempt
}

// sessionPacket is the task packet of a step (§3.1): self-contained, so that any subagent can do the task from it alone.
func (c *callRun) sessionPacket(k int, marker, input string, repair bool, action string) string {
	e, s, p := c.e, c.sess, c.plan
	var b strings.Builder
	fmt.Fprintf(&b, "# CRW flow task packet %s\n\n- Operation: crw-flow-op: %s\n- Role: %s\n- Workspace P: %s\n\n", marker, marker, p.role, c.cwd)
	b.WriteString("## Where you work\n\n")
	fmt.Fprintf(&b, "Your shell starts in a directory that is NOT your workspace. Your workspace is %s: run every command with workdir %s, and work only under the absolute path %s.\n", c.cwd, c.cwd, c.cwd)
	switch {
	case p.qa:
		fmt.Fprintf(&b, "%s is a scratch worktree at the head the gate pinned: whatever you edit there is discarded, and the run reads the tree you end in.\n", c.cwd)
	case !readsOnly(p.spec.Family):
		fmt.Fprintf(&b, "Edit files only under %s. The run snapshots %s when your turn ends; an edit anywhere else is not part of your work and is recorded as a warning.\n", c.cwd, c.cwd)
	default:
		fmt.Fprintf(&b, "Read the files under %s and change nothing, there or anywhere else.\n", c.cwd)
	}
	b.WriteString("Never run `git commit`, `git add`, `git branch`, `git reset` or `git checkout`: the run records and commits the work itself.\n")
	b.WriteString("You are a leaf subagent: never spawn agents.\n\n")
	if text, err := c.roleText(); err == nil && strings.TrimSpace(text) != "" {
		b.WriteString("## Role instructions\n\n")
		b.WriteString(strings.TrimSpace(text))
		b.WriteString("\n\n")
	}
	if s.note != "" && action == StepSpawn {
		b.WriteString("## Earlier attempts\n\n" + s.note + "\n\n")
	}
	if p.resume != nil && action == StepSpawn {
		b.WriteString("## Continuation\n\n")
		fmt.Fprintf(&b, "This call continues call %s, whose subagent is gone.", p.resume.CallID)
		if prev := lastPacketOf(e.j, p.resume.CallID); prev != "" {
			fmt.Fprintf(&b, " Read its task packet %s first: it is the task this call continues.", prev)
		}
		if ans := c.answerFile(p.resume.CallID); ans != "" {
			fmt.Fprintf(&b, " Its answer is in %s.", ans)
		}
		b.WriteString(" The task below is what to do now.\n\n")
	}
	b.WriteString("## Task\n\n")
	if repair && action == StepSpawn {
		b.WriteString(strings.TrimSpace(s.task) + "\n\n")
		if s.lastText != "" {
			b.WriteString("## Your previous answer\n\n" + strings.TrimSpace(s.lastText) + "\n\n")
		}
		b.WriteString("## What to do now\n\n")
	}
	b.WriteString(strings.TrimSpace(input) + "\n\n")
	b.WriteString(s.evidencePacket())
	b.WriteString("## Output contract\n\n")
	if p.schema != nil {
		b.WriteString("Your final message is exactly one JSON value that satisfies this schema, with nothing before or after it:\n\n```json\n")
		b.Write(p.schema)
		b.WriteString("\n```\n")
	} else {
		b.WriteString("Your final message is your answer, and nothing else.\n")
	}
	_ = k
	return b.String()
}

// roleText is the role section of a packet.
func (c *callRun) roleText() (string, error) {
	if f := c.e.cfg.Session.RoleText; f != nil {
		return f(c.plan.role, c.plan.qa)
	}
	text, err := roles.Overlay(c.plan.role, roles.Options{QA: c.plan.qa})
	if err != nil {
		return "", err
	}
	if i := strings.Index(text, "\n## Native spawn"); i >= 0 {
		text = text[:i]
	}
	return text, nil
}

// lastPacketOf is the packet the newest step of a call named.
func lastPacketOf(j Journal, callID string) string {
	packet := ""
	for _, ev := range CallEvents(j, callID) {
		if ev.Event == EvStepPublished {
			packet = rowStr(ev.Data, "packet")
		}
	}
	return packet
}

// answerFile writes the answer of the call a continuation continues next to the continuation's packets and returns its path.
func (c *callRun) answerFile(callID string) string {
	result := ""
	for _, ev := range CallEvents(c.e.j, callID) {
		if ev.Event == EvCompleted {
			result = rowStr(ev.Data, "result")
		}
	}
	if result == "" {
		return ""
	}
	path, err := c.e.cfg.Session.Bus.WriteFile(c.id, "answer-of-"+callID+".json", []byte(result+"\n"))
	if err != nil {
		return ""
	}
	return path
}

// extractTrailingJSON takes the JSON object that ends a message out of prose before it: the longest suffix that is one JSON object.
func extractTrailingJSON(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if !strings.HasSuffix(t, "}") {
		return "", false
	}
	for i := 0; i < len(t); i++ {
		if t[i] != '{' {
			continue
		}
		cand := t[i:]
		var v map[string]any
		dec := json.NewDecoder(strings.NewReader(cand))
		if err := dec.Decode(&v); err != nil {
			continue
		}
		if dec.More() {
			continue
		}
		if _, err := dec.Token(); err == nil {
			continue
		}
		return cand, true
	}
	return "", false
}
