package spawn

import (
	"errors"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// F14 (CRW-1207): the native spawn of a thread that a flow run has bound. The binding is the run's file for the thread
// (internal/flow/bound); the hook only reads it and makes claim files. Nothing here opens the relay store or reads a policy file:
// the helper pairs are in the binding line.
//
// The standalone path is not touched: a session id that is not a thread id, no flow-bound directory, a thread no run registered
// and a closed run are all answered by the code before this file, byte for byte. What this file decides:
//
//   - a directory or file that cannot be trusted refuses every spawn of a thread-shaped session (flow_bound_unavailable);
//   - an engine thread (a thread of an engine call) may spawn one native helper per call, agent_type explorer, with the explorer
//     pair of the run's policy whatever the caller sent or inherited (native_type_refused, native_limit_refused); the limit is a
//     claim made by link(2), a delivery of the same event again gets the stored answer, and CRW-SUBSPAWN-ALLOWED mints no grant;
//   - the thread that orchestrates an open run may spawn explorer helpers only, with the pair its message header names
//     (CRW-ROLE: reviewer, else explorer), no count limit but the host's;
//   - a full-history fork keeps the parent's pair, which cannot be set, so it is refused with the way out (a new explorer
//     with a question packet);
//   - every decision goes to the thread's ledger (bound.AppendDecision): the pair the caller sent, the pair applied, the reason.
//
// The helper gets the pair and nothing else: no guard block, skills, evidence assignment or prompt override (the managed
// dispatch and the settings store are the ordinary path's, not read here).

const (
	flowBoundTypeGuidanceEngine = "the helper of an engine call is a native explorer: agent_type \"explorer\" (got %s). Do other work yourself, or report the need in your final answer"
	flowBoundTypeGuidanceOrch   = "only an explorer helper can be spawned natively from this thread (got %s); send other work to run(crw flow)"
	flowBoundArchitectGuidance  = "CRW-ROLE: architect is not a native helper role of a thread bound to a flow run; send it to run(crw flow)"
	flowBoundForkGuidance       = "a full-history fork keeps the parent's model and effort, so its role pair cannot be set; spawn a new explorer with a question packet (the task and what it needs to know, fork_context false or fork_turns \"none\") instead"
	flowBoundLimitGuidance      = "this engine call has used its native helper (one per call); do the rest yourself or report it in your final answer"
	flowBoundCiphertextGuidance = "the task of a native V2 spawn is ciphertext, so the hook cannot read its CRW-ROLE header; name the role in task_name: reviewer or explorer, or the role and an underscore and more (reviewer_diff, explorer_parser)"
	flowBoundNoneGuidance       = "this engine call may spawn no native helper; do the work yourself or report the need in your final answer"
)

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
	// The hook's own recovery (RunSpawnAttachHook) prints nothing, which allows the spawn; a failure here must not: a session whose
	// id has the form of a thread id may be a bound one, and it is refused.
	defer func() {
		if recover() != nil {
			if bound.ValidThreadID(session) {
				out, handled = DenyEnvelope(flowBoundReason(bound.ReasonFlowBoundUnavailable, "the hook failed while deciding this spawn")), true
			} else {
				out, handled = "", false
			}
		}
	}()
	res := bound.Lookup(session)
	switch res.State {
	case bound.Standalone:
		return "", false
	case bound.Unavailable:
		return DenyEnvelope(flowBoundReason(bound.ReasonFlowBoundUnavailable, "the flow binding of this thread cannot be read ("+res.Err.Error()+
			"); no native spawn is allowed until the flow-bound directory is repaired")), true
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
	agentType, _ := e.input.Get("agent_type").(string)
	orchestrator := e.line().Kind == bound.KindOrchestrator
	switch {
	case agentType != "explorer":
		got := "none"
		if v := e.input.Get("agent_type"); v != nil {
			got = spawnHookRouteStringify(v)
		}
		guidance := flowBoundTypeGuidanceEngine
		if orchestrator {
			guidance = flowBoundTypeGuidanceOrch
		}
		return e.refuse(bound.ReasonNativeTypeRefused, strings.Replace(guidance, "%s", got, 1), "")
	case IsFullHistoryFork(spawnHookView(e.input)):
		return e.refuse(bound.ReasonFullHistoryFork, flowBoundForkGuidance, "")
	case deep:
		return e.refuse(bound.ReasonFlowBoundUnavailable, "tool_input nests too deeply to be recorded", "")
	}
	if orchestrator {
		return e.orchestrator()
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

// orchestrator answers a spawn of the thread that orchestrates an open run: the pair is the one the producer named, and keywords
// never choose. A plaintext message names it in its header; the task of a native V2 spawn is ciphertext, whose header the hook
// cannot read, so it names it in the plaintext task_name, and a call whose role cannot be established is refused: it never gets
// the explorer pair by default.
func (e *flowBoundEvent) orchestrator() string {
	role, readable := e.headerRole(), true
	if e.ciphertext() {
		role, readable = taskNameRole(e.input.Get("task_name")), false
	}
	helper := bound.HelperExplorer
	switch role {
	case "reviewer":
		helper = bound.HelperReviewer
	case "architect":
		return e.refuse(bound.ReasonNativeTypeRefused, flowBoundArchitectGuidance, "")
	case "":
		if !readable {
			return e.refuse(bound.ReasonNativeTypeRefused, flowBoundCiphertextGuidance, "")
		}
	}
	pair := e.line().HelperPairs[helper]
	answer, orig, err := answerFrom(e.inText, pair)
	if err != nil {
		return e.unavailable(err)
	}
	return e.allow(helper, pair, answer, orig)
}

// taskNameRole is the role a V2 task_name names: the role itself, or the role and an underscore and more ("reviewer_diff"); ""
// for any other name. Only the three roles of the header are known, and the case is exact.
func taskNameRole(v any) string {
	name, _ := v.(string)
	for _, role := range []string{"reviewer", "explorer", "architect"} {
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

func (e *flowBoundEvent) ciphertext() bool {
	_, c := e.packet()
	return c
}

// headerRole is the CRW-ROLE value of the producer header of a plaintext message (the lines before the first line-start TASK:),
// "" for none.
func (e *flowBoundEvent) headerRole() string {
	packet, _ := e.packet()
	message := StripControlMarkers(packet, false)
	header := message
	if at := spawnClassifyTaskStart(message); at >= 0 {
		header = message[:at]
	}
	return spawnClassifyRoleMarker(header)
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
