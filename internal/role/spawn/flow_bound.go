package spawn

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// F14 (CRW-1207): the native spawn of a thread that a flow run has bound. The binding is the run's file for the thread
// (internal/flow/bound); the hook only reads it and makes claim files. Nothing here opens the relay store or reads a policy file:
// the pairs are in the binding line.
//
// The standalone path is not touched: a session id that is not a thread id, no flow-bound directory, a thread no run registered
// and a closed run are all answered by the code before this file, byte for byte. What this file decides:
//
//   - a directory or file that cannot be trusted is read as no binding: the spawn is decided as an ordinary thread's, with a
//     warning on stderr (CRW-1348: no flow_bound_unavailable refusal of every spawn; the run's import still finds an engine
//     thread's spawn that no ledger line covers, unbound_spawn);
//   - an engine thread (a thread of an engine call of the App Server backend) may spawn one native helper per call, agent_type
//     explorer, with the explorer pair of the run's policy whatever the caller sent or inherited (native_type_refused,
//     native_limit_refused); the limit is a claim made by link(2), a delivery of the same event again gets the stored answer, and
//     CRW-SUBSPAWN-ALLOWED mints no grant;
//   - the thread that orchestrates an open session run (CRW-1316 §9) spawns the role subagents of the run's steps. Two spawns are
//     refused: a spawn by a subagent (a leaf, as anywhere) and a full-history fork (it keeps the parent's pair, which cannot be
//     set). Every other spawn is allowed with the pair of its role overwritten from the line's six pairs: the role the step's
//     CRW-ROLE: header names (the flow-bound parser, which takes exactly the six values), else the role of its agent_type. A role
//     that does not pair with the agent_type, and a spawn that names no step (no crw-flow-op: marker), are allowed with a warning
//     in the ledger (role_agent_type_mismatch, unrecorded_spawn, unrecorded_author for an author's type). A ledger that cannot be
//     written allows with a warning on stderr;
//   - every decision goes to the thread's ledger (bound.AppendDecision): the pair the caller sent, the pair applied, the reason or
//     the warning, and for an orchestrator the role and the op, which a session run joins with the owner turn's items to find the
//     subagent of a published step (internal/flow/sessionhost FindByMarker).
//
// The helper gets the pair and nothing else: no guard block, skills, evidence assignment or prompt override (the managed
// dispatch and the settings store are the ordinary path's, not read here).

const (
	flowBoundTypeGuidanceEngine = "the helper of an engine call is a native explorer: agent_type \"explorer\" (got %s). Do other work yourself, or report the need in your final answer"
	flowBoundForkGuidance       = "a full-history fork keeps the parent's model and effort, so its role pair cannot be set; spawn a new explorer with a question packet (the task and what it needs to know, fork_context false or fork_turns \"none\") instead"
	flowBoundStepForkGuidance   = "a full-history fork keeps the parent's model and effort, so its role pair cannot be set; spawn the step with fork_context false (crw flow next names the spawn_agent arguments)"
	flowBoundLimitGuidance      = "this engine call has used its native helper (one per call); do the rest yourself or report it in your final answer"
	flowBoundNoneGuidance       = "this engine call may spawn no native helper; do the work yourself or report the need in your final answer"
)

// The warnings of an orchestrator's allowed spawn (bound.Decision.Warning starts with one).
const (
	flowBoundWarnRoleMismatch     = "role_agent_type_mismatch"
	flowBoundWarnUnrecordedSpawn  = "unrecorded_spawn"
	flowBoundWarnUnrecordedAuthor = "unrecorded_author"
	flowBoundWarnUnknownType      = "unknown_agent_type"
	flowBoundWarnInputUnrecorded  = "input_unrecorded"
	flowBoundWarnRoleUndecided    = "role_undecided"
)

// flowBoundWarn writes a warning of the hook to stderr, where the host logs it; the hook's answer is its stdout. A test replaces it.
var flowBoundWarn = func(msg string) { fmt.Fprintln(os.Stderr, msg) }

// flowBoundReason is the deny text of a refusal: it starts with the code.
func flowBoundReason(code, detail string) string {
	return "crw flow-bound " + code + ": " + detail
}

// spawnFlowBound answers the native spawn of a thread a flow run bound, and handled is false for every other thread: the
// payload is then the ordinary path's. It is total: a failure while deciding refuses, it never lets the spawn through.
func spawnFlowBound(payload pyjson.Object) (out string, handled bool) {
	if payload.Get("hook_event_name") != "PreToolUse" || !IsSpawnToolName(payload.Get("tool_name")) {
		return "", false
	}
	session, _ := payload.Get("session_id").(string)
	// The hook's own recovery (RunSpawnAttachHook) prints nothing, which allows the spawn; a failure here must not for an engine
	// thread, whose helper count and type the binding limits: a session whose id has the form of a thread id may be a bound one, and
	// it is refused. An orchestrator's spawn goes through untouched with a warning (CRW-1348: fail open).
	state := bound.Unavailable
	defer func() {
		if recover() != nil {
			switch {
			case state == bound.Orchestrator:
				flowBoundWarn("crw flow-bound: warning: the hook failed while deciding a spawn of the orchestrator thread " + session + "; the spawn goes through as it was sent")
				out, handled = "", true
			case bound.ValidThreadID(session):
				out, handled = DenyEnvelope(flowBoundReason(bound.ReasonFlowBoundUnavailable, "the hook failed while deciding this spawn")), true
			default:
				out, handled = "", false
			}
		}
	}()
	res := bound.Lookup(session)
	state = res.State
	switch res.State {
	case bound.Standalone:
		return "", false
	case bound.Unavailable:
		flowBoundWarn("crw flow-bound: warning: the flow binding of thread " + session + " cannot be read (" + res.Err.Error() + "); the spawn is decided as an ordinary thread's. `crw doctor` checks the flow-bound directory")
		return "", false
	}
	e := &flowBoundEvent{res: res, payload: payload, thread: session}
	return e.decide(), true
}

// flowBoundEvent is one spawn event of a bound thread.
type flowBoundEvent struct {
	res     bound.Result
	payload pyjson.Object
	thread  string

	input   pyjson.Object // tool_input, nil when it is no object
	inText  string        // tool_input as the hook writes it (JSON.stringify), "" when too deep
	toolUse string
}

func (e *flowBoundEvent) line() bound.Line { return e.res.Line }

// spawnFlowBoundBeforeDecide runs before a bound thread's spawn is decided; a test makes it fail.
var spawnFlowBoundBeforeDecide = func() {}

func (e *flowBoundEvent) decide() string {
	spawnFlowBoundBeforeDecide()
	if IsSubagentSpawner(spawnHookView(e.payload)) {
		return DenyEnvelope(RecurseDenyReason) // a child of a bound thread is a leaf, as anywhere
	}
	if id, ok := e.payload.Get("tool_use_id").(string); ok {
		e.toolUse = id
	}
	e.input, _ = spawnHookRecord(e.payload.Get("tool_input"))
	deep := e.input != nil && spawnHookRouteDeep(e.input)
	if e.input != nil && !deep {
		e.inText = spawnHookRouteStringify(e.input)
	}
	if e.line().Kind == bound.KindOrchestrator {
		return e.orchestrator()
	}
	agentType, _ := e.input.Get("agent_type").(string)
	switch {
	case agentType != "explorer":
		got := "none"
		if v := e.input.Get("agent_type"); v != nil {
			got = spawnHookRouteStringify(v)
		}
		return e.refuse(bound.ReasonNativeTypeRefused, strings.Replace(flowBoundTypeGuidanceEngine, "%s", got, 1), "")
	case IsFullHistoryFork(spawnHookView(e.input)):
		return e.refuse(bound.ReasonFullHistoryFork, flowBoundForkGuidance, "")
	case deep:
		return e.refuse(bound.ReasonFlowBoundUnavailable, "tool_input nests too deeply to be recorded", "")
	}
	return e.engine()
}

// refuse is the deny of a refusal, recorded in the thread's ledger (a record that cannot be written does not turn the refusal
// into anything else).
func (e *flowBoundEvent) refuse(code, detail, helper string) string {
	d := e.decision(bound.DecisionRefused, helper, e.input, bound.Pair{})
	d.Reason, d.Detail = code, detail
	_ = bound.AppendDecision(e.res.Dir, d)
	return DenyEnvelope(flowBoundReason(code, detail))
}

// decision is the ledger entry of this event for the original input orig: what the caller sent and the pair applied.
func (e *flowBoundEvent) decision(kind, helper string, orig pyjson.Object, applied bound.Pair) bound.Decision {
	line := e.line()
	sent := func(key string) string { s, _ := orig.Get(key).(string); return s }
	return bound.Decision{
		RunID: line.RunID, ThreadID: e.thread, ThreadKind: line.Kind, CallID: line.CallID, ToolUseID: e.toolUse, Decision: kind, HelperRole: helper,
		RequestedModel: sent("model"), RequestedEffort: sent("reasoning_effort"), AppliedModel: applied.Model, AppliedEffort: applied.Effort,
	}
}

// unavailable is the refusal for a state the hook cannot trust or write.
func (e *flowBoundEvent) unavailable(err error) string {
	reason := "the hook cannot keep the record this spawn needs"
	var u *bound.UnavailableError
	if errors.As(err, &u) {
		reason += " (" + u.Error() + ")"
	}
	return e.refuse(bound.ReasonFlowBoundUnavailable, reason, "")
}

// answerFrom is the allowing answer for the original input given as text, with the pair: the input with the pair put in, as the
// ordinary route writes it. It depends on the input and the pair only, so every delivery of the event writes the same bytes.
func answerFrom(inputText string, pair bound.Pair) (string, pyjson.Object, error) {
	v, err := pyjson.Loads(inputText, pyjson.LoadOptions{Surrogates: true, Numbers: pyjson.SpelledNumbers})
	orig, ok := v.(pyjson.Object)
	if err != nil || !ok {
		return "", nil, errors.New("the recorded input cannot be read")
	}
	updated := slices.Clone(orig).Set("model", pair.Model).Set("reasoning_effort", pair.Effort)
	output := pyjson.Object{{Key: "hookEventName", Value: "PreToolUse"}, {Key: "permissionDecision", Value: "allow"}, {Key: "updatedInput", Value: updated}}
	return spawnHookRouteStringify(pyjson.Object{{Key: "hookSpecificOutput", Value: output}}) + "\n", orig, nil
}

// allow records the decision and returns the answer; an allow whose record cannot be written is refused, because the run checks
// every native spawn of a bound thread against the ledger.
func (e *flowBoundEvent) allow(helper string, pair bound.Pair, answer string, orig pyjson.Object) string {
	if err := bound.AppendDecision(e.res.Dir, e.decision(bound.DecisionAllowed, helper, orig, pair)); err != nil {
		return e.unavailable(err)
	}
	return answer
}

// orchestrator answers a spawn of the thread that orchestrates an open session run (CRW-1316 §9): a full-history fork is refused,
// every other spawn is allowed with the pair of its role. The role is the one the step's CRW-ROLE: header names (exactly one of the
// six), else the one its agent_type names, else explorer; the op is the step's crw-flow-op: marker. A native V2 task is ciphertext,
// whose header the hook cannot read: its role is the one its plaintext task_name names. What does not match the step form is
// allowed with a warning in the ledger, and a ledger that cannot be written allows with a warning on stderr.
func (e *flowBoundEvent) orchestrator() string {
	if IsFullHistoryFork(spawnHookView(e.input)) {
		return e.refuse(bound.ReasonFullHistoryFork, flowBoundStepForkGuidance, "")
	}
	agentType, _ := e.input.Get("agent_type").(string)
	shown := "none"
	if v := e.input.Get("agent_type"); v != nil {
		shown = spawnHookRouteStringify(v)
	}
	role, op := "", ""
	if packet, ciphertext := e.packet(); ciphertext {
		role = flowBoundTaskNameRole(e.input.Get("task_name"))
	} else {
		message := StripControlMarkers(packet, false)
		role, op = flowBoundRoleHeader(message), bound.MarkerOp(message)
	}
	typeRole := bound.RoleOfAgentType(agentType)
	var warnings []string
	if op == "" {
		code := flowBoundWarnUnrecordedSpawn
		if role == "" {
			role = typeRole
		}
		if t := bound.AgentTypeOf(role); t == "executor" || t == "architect" {
			code = flowBoundWarnUnrecordedAuthor
		}
		warnings = append(warnings, code+": the spawn names no step of the run (no crw-flow-op: line), so the run records no call for it")
	}
	switch {
	case role != "" && typeRole != bound.AgentTypeOf(role):
		warnings = append(warnings, fmt.Sprintf("%s: CRW-ROLE: %s is spawned as agent_type %s, not %s; the %s pair is applied", flowBoundWarnRoleMismatch, role, shown, bound.AgentTypeOf(role), role))
	case role == "" && typeRole != "":
		role = typeRole
	case role == "":
		role = bound.HelperExplorer
		warnings = append(warnings, fmt.Sprintf("%s: agent_type %s names no role and the spawn no CRW-ROLE:; the explorer pair is applied", flowBoundWarnUnknownType, shown))
	}
	pair := e.line().HelperPairs[role]
	answer, orig, err := "", pyjson.Object(nil), error(nil)
	switch {
	case pair == (bound.Pair{}):
		// an undecided role (CRW-1330): the spawn keeps what it was sent, the host's default when it names no pair
		warnings = append(warnings, fmt.Sprintf("%s: the run's role table leaves %s undecided, so the spawn keeps the host's default", flowBoundWarnRoleUndecided, role))
		_, orig, _ = answerFrom(e.inText, pair)
	case e.inText == "":
		err = errors.New("tool_input is no object that can be recorded")
	default:
		answer, orig, err = answerFrom(e.inText, pair)
	}
	if err != nil {
		// the input cannot be given the pair: it goes through as it was sent, inheriting the orchestrator's pair
		warnings = append(warnings, flowBoundWarnInputUnrecorded+": "+err.Error()+"; the spawn goes through as it was sent")
		answer, pair = "", bound.Pair{}
	}
	d := e.decision(bound.DecisionAllowed, role, orig, pair)
	d.Role, d.Op, d.Warning = role, op, strings.Join(warnings, "; ")
	if err := bound.AppendDecision(e.res.Dir, d); err != nil {
		flowBoundWarn(fmt.Sprintf("crw flow-bound: warning: the spawn ledger of thread %s cannot be written (%v); the spawn is allowed with the %s pair and is not recorded", e.thread, err, role))
	}
	return answer
}

// flowBoundRoleHeader is the role a step's header names: the first line that starts with "CRW-ROLE:" among the lines before the
// first blank line or TASK: line, when its value is exactly one of the six roles of an orchestrator line; "" otherwise. It is the
// flow-bound parser: the ordinary path's (spawnClassifyRoleMarker, the oracle's three values) is not used here and is unchanged.
func flowBoundRoleHeader(message string) string {
	header := message
	if at := spawnClassifyTaskStart(header); at >= 0 {
		header = header[:at]
	}
	for _, line := range strings.Split(header, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			return ""
		}
		value, ok := strings.CutPrefix(line, "CRW-ROLE:")
		if !ok {
			continue
		}
		if value = strings.TrimSpace(value); bound.IsOrchestratorRole(value) {
			return value
		}
		return ""
	}
	return ""
}

// flowBoundTaskNameRole is the role a V2 task_name names: one of the six roles itself, or a role and an underscore and more
// ("reviewer_diff"); "" for any other name. The case is exact.
func flowBoundTaskNameRole(v any) string {
	name, _ := v.(string)
	for _, role := range []string{"reviewer-final", "executor-t1", "architect", "executor", "reviewer", "explorer"} {
		if name == role || strings.HasPrefix(name, role+"_") {
			return role
		}
	}
	return ""
}

// packet is the message of the spawn as text, and whether it is a native V2 task of ciphertext shape.
func (e *flowBoundEvent) packet() (text string, ciphertext bool) {
	v2 := IsCollaborationToolName(e.payload.Get("tool_name")) || IsV2SpawnInput(spawnHookView(e.input))
	text = spawnHookPacketText(e.input, v2)
	return text, v2 && IsFernetTokenShape(text)
}

// engine answers a spawn of an engine thread: one helper per call, the explorer pair.
func (e *flowBoundEvent) engine() string {
	line := e.line()
	if e.toolUse == "" {
		return e.refuse(bound.ReasonFlowBoundUnavailable, "the spawn event carries no tool_use_id, so a delivery of it again cannot be told from another spawn", "")
	}
	claim, present, err := bound.ReadClaim(e.res.Dir, e.thread, line.CallID)
	if err != nil {
		return e.unavailable(err)
	}
	if !present {
		if line.Allowance() == 0 {
			return e.refuse(bound.ReasonNativeLimitRefused, flowBoundNoneGuidance, "")
		}
		created, err := bound.CreateClaim(e.res.Dir, e.thread, line.CallID, bound.Claim{ToolUseID: e.toolUse, Input: e.inText})
		if err != nil {
			return e.unavailable(err)
		}
		if created {
			return e.answer(bound.Claim{ToolUseID: e.toolUse, Input: e.inText}, false)
		}
		if claim, present, err = bound.ReadClaim(e.res.Dir, e.thread, line.CallID); err != nil || !present {
			if err == nil {
				err = errors.New("the claim of the call is not there")
			}
			return e.unavailable(err)
		}
	}
	return e.answer(claim, true)
}

// answer is the answer of the claim's spawn for the event in hand: this delivery's own (first), or the same event delivered
// again, which gets the stored answer; any other spawn is over the limit.
func (e *flowBoundEvent) answer(claim bound.Claim, existing bool) string {
	line, helper := e.line(), bound.HelperExplorer
	pair := line.HelperPairs[helper]
	stored, haveStored := "", false
	if existing {
		if claim.ToolUseID != e.toolUse {
			return e.refuse(bound.ReasonNativeLimitRefused, flowBoundLimitGuidance, "")
		}
		var err error
		if stored, haveStored, err = bound.ReadAnswer(e.res.Dir, e.thread, line.CallID); err != nil {
			return e.unavailable(err)
		}
	}
	answer := stored
	if !haveStored {
		var err error
		if answer, _, err = answerFrom(claim.Input, pair); err != nil {
			return e.unavailable(err)
		}
	}
	if existing {
		// the same event: its original input, or the updatedInput of its answer; another input of the same tool use is another spawn
		if updated := spawnHookReplayUpdated(answer); updated == "" || (e.inText != claim.Input && e.inText != updated) {
			if updated == "" {
				return e.unavailable(errors.New("the stored answer cannot be read"))
			}
			return e.refuse(bound.ReasonNativeLimitRefused, flowBoundLimitGuidance, "")
		}
	}
	if !haveStored {
		if err := bound.WriteAnswer(e.res.Dir, e.thread, line.CallID, answer); err != nil {
			return e.unavailable(err)
		}
	}
	_, orig, err := answerFrom(claim.Input, pair)
	if err != nil {
		return e.unavailable(err)
	}
	// the allowance is spent for good only when the claim, its answer and the directories that hold them have reached the disk
	if err := bound.SyncClaims(e.res.Dir, e.thread); err != nil {
		return e.unavailable(err)
	}
	return e.allow(helper, pair, answer, orig)
}
