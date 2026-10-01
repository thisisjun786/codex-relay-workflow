package skill

import (
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
)

// The offline probe contract differs from the runtime guard in nullable facts,
// receipt wording, and the fields it records. Identity predicates remain shared.
func probeState(o, marker hook.Object, malformed string, reached *hookReplayReach) (string, string, error) {
	answer := func(n int, state, reason string) (string, string, error) {
		markHookReplay(reached, "observe_state", n)
		return state, reason, nil
	}
	if unreadable := objGet(o, "store_unreadable"); pyvalue.Truthy(unreadable) {
		var names []string
		switch value := unreadable.(type) {
		case string:
			for _, r := range value {
				names = append(names, string(r))
			}
		case hook.Object:
			for _, f := range value {
				names = append(names, f.Key)
			}
		case []any:
			for _, v := range value {
				names = append(names, pyvalue.Str(v))
			}
		default:
			return "", "", pythonNotIterable(unreadable, false)
		}
		slices.Sort(names)
		return answer(1, "state_unreadable", "Cannot read "+strings.Join(names, ", ")+".")
	}
	if malformed != "" {
		return answer(2, "marker_malformed", "The published "+malformed+" is not the shape a fact must be. Repair the marker; a record that cannot be read is reported, never guessed at.")
	}
	if objGet(o, "marker") == nil {
		return answer(3, "unmanaged", "No assignment directory for this workspace.")
	}
	stop := asObject(objGet(o, "stop_input"))
	session := objGet(stop, "session_id")
	bound := asObject(objGet(marker, "bound"))
	assignment := objGet(o, "assignment")
	if len(bound) == 0 {
		if !delivery.CorrelatedTrace(marker, session, assignment, reached) {
			return answer(4, "dispatch_uncorrelated", "This session presented no matching dispatch request id.")
		}
		return answer(5, "correlated_unbound", "Correlated to the intent but not yet bound by the coordinator. Released; the turn's observation is recorded for the coordinator to fold once the bind lands.")
	}
	if !named(objGet(bound, "sessionId")) {
		return answer(6, "bound_identity_unnamed", "The bind record names no session, so nothing can be shown to be the bound child. Repair the marker; a turn is never held against an identity nobody published.")
	}
	if !delivery.SameIdentity(objGet(bound, "sessionId"), session) {
		return answer(7, "marker_claimed_by_other_session", "This session is not the bound child.")
	}
	declaration := probeDeclaration(o, marker, reached)
	if strings.HasPrefix(declaration, "declared_") {
		return answer(8, declaration, "The child declared this turn.")
	}
	problem := delivery.CorrelationProblemTrace(marker, session, assignment, reached)
	if problem == delivery.ClaimAbsent {
		return answer(9, "marker_unclaimed", "The coordinator bound this session, but it has not claimed this assignment. Released and recorded; a hold needs the child's own claim, not only the coordinator's bind.")
	}
	if problem != "" {
		return answer(10, "claim_uncorrelated", "This session is bound but its claim does not correlate with this assignment ("+problem+"). Released and recorded; every fact this reads is create-once, so it does not clear itself and no resolution consumed here will: correlation reads the claim and the intent, never the adjudications. Recovery is a new assignment, declared for a fresh dispatch request id.")
	}
	if !pyvalue.Truthy(objGet(marker, "relationship")) {
		return answer(11, "managed_unregistered", "This workspace is managed but its relationship is not registered. Register it, or record a disposition explaining why it cannot be.")
	}
	receipt := asObject(objGet(o, "receipt"))
	ev := objGet(receipt, "evidence")
	if declaration == "receipt_missing" {
		detail := pyvalue.Str(objGet(receipt, "detail"))
		if ev == "generation_absent" {
			return answer(12, declaration, "The relay's store holds no record of the generation it reports as current for this relationship: "+detail+". Nothing can be attributed to this assignment while the store cannot say which dispatch opened the generation it is on, and no receipt this session emits changes that. The relay's store is what needs repair.")
		}
		if ev == "registration_generation_mismatch" || ev == "generation_dispatch_mismatch" {
			return answer(13, declaration, "This assignment's registration does not name the generation the relay is on: "+detail+". No receipt this session emits can satisfy it, because the registration fact is create-once and cannot be republished onto the live generation, and a receipt earned there belongs to work this assignment never registered. Recovery is a new assignment, declared for a fresh dispatch request id.")
		}
		return answer(14, declaration, "Readiness is declared but no receipt exists at the current head revision. Emit the receipt over the actual artifacts.")
	}
	return answer(15, "undeclared_turn_end", "No usable turn disposition was recorded for this turn. Record in_progress, blocked_needs_input, interrupted, failed, or ready_for_review with a receipt.")
}

func probeDeclaration(o, marker hook.Object, reached *hookReplayReach) string {
	return hook.ClassifyDeclaration(hook.Observation{Stop: asObject(objGet(o, "stop_input")), Marker: marker, Disposition: objGet(o, "disposition"), Receipt: asObject(objGet(o, "receipt")), Reached: reached})
}

func probeDecision(o, marker hook.Object, malformed string, reached *hookReplayReach) (map[string]any, error) {
	state, reason, err := probeState(o, marker, malformed, reached)
	if err != nil {
		return nil, err
	}
	omission := func(s string) bool {
		return s == "managed_unregistered" || s == "receipt_missing" || s == "undeclared_turn_end"
	}
	counters := asObject(objGet(o, "counters"))
	if omission(state) && objHas(o, "counters") {
		bad := ""
		if counters == nil {
			bad = "counters"
		} else {
			bad = delivery.MalformedCounters(counters)
		}
		if bad != "" {
			state, reason = "marker_malformed", "Invalid persisted "+bad+"; repair the hold budget record."
		}
	}
	decision, finalState, finalReason := "release", state, reason
	stop := asObject(objGet(o, "stop_input"))
	count := func(key string) int64 { value, _ := objGet(counters, key).(int64); return value }
	if !omission(state) {
		markHookReplay(reached, "decide", 1)
	} else {
		switch {
		case count("holdsThisGeneration") >= 2 || count("holdsThisSessionWindow") >= 3:
			markHookReplay(reached, "decide", 2)
			finalState, finalReason = "unresolved_handoff", "Hold bound reached; recording an unresolved handoff instead of holding again."
		case count("holdsThisTurn") >= 1:
			markHookReplay(reached, "decide", 3)
			finalState, finalReason = "hold_in_flight", "This turn already took its one hold."
		case pyvalue.Truthy(objGet(stop, "stop_hook_active")):
			markHookReplay(reached, "decide", 4)
			finalState, finalReason = "hold_in_flight", "A continuation is already running for this turn; the omission is recorded."
		default:
			markHookReplay(reached, "decide", 5)
			decision = "block"
		}
	}
	record := map[string]any{"observation": state, "turnId": orderedPlain(objGet(stop, "turn_id")), "sessionId": orderedPlain(objGet(stop, "session_id")), "decisionState": finalState, "held": decision == "block", "at": orderedPlain(objGet(o, "now"))}
	result := map[string]any{"decision": decision, "state": finalState, "observation": state, "reason": finalReason, "record": record}
	if malformed != "" {
		marker = nil
	}
	if state == "correlated_unbound" {
		record["pendingObservation"] = probeDeclaration(o, marker, reached)
	}
	if state == "claim_uncorrelated" && len(marker) > 0 {
		record["claimEvidence"] = delivery.CorrelationProblem(marker, objGet(stop, "session_id"), objGet(o, "assignment"))
		record["pendingObservation"] = probeDeclaration(o, marker, reached)
	}
	if len(marker) > 0 {
		record["assignmentState"] = delivery.DeriveAssignmentStateTrace(marker, objGet(o, "now"), reached)
		if len(asObject(objGet(marker, "bound"))) > 0 && pyvalue.Truthy(objGet(marker, "relationship")) {
			record["assignmentState"] = "relationship_registered"
		}
		record["identityContested"] = delivery.IdentityContestedTrace(marker, reached)
	}
	receipt := asObject(objGet(o, "receipt"))
	if pyvalue.Truthy(objGet(receipt, "evidence")) {
		result["receiptEvidence"], record["receiptEvidence"] = orderedPlain(objGet(receipt, "evidence")), orderedPlain(objGet(receipt, "evidence"))
		if pyvalue.Truthy(objGet(receipt, "detail")) {
			result["receiptDetail"], record["receiptDetail"] = orderedPlain(objGet(receipt, "detail")), orderedPlain(objGet(receipt, "detail"))
		}
	}
	output := map[string]any{}
	if decision == "block" {
		output = map[string]any{"decision": "block", "reason": finalReason, "continue": true}
	}
	result["hook_output"] = output
	return result, nil
}
