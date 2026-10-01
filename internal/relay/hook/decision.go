package hook

import (
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

const Observe = "observe"
const Hold = "hold"

var omissions = []string{"managed_unregistered", "receipt_missing", "undeclared_turn_end"}
var generationEvidence = []string{"registration_generation_mismatch", "generation_dispatch_mismatch", "generation_absent"}

type Observation struct {
	Stop, Marker Object
	Assignment   any
	Disposition  any
	Receipt      Object
	Unreadable   []string
	Malformed    string
	Now          string
	Reached      delivery.ReturnReacher
}

func ReceiptMatches(receipt, stop, marker Object) bool {
	return pyvalue.Truthy(get(receipt, "atCurrentHead")) && delivery.SameIdentity(get(receipt, "sessionId"), get(stop, "session_id")) && delivery.SameIdentity(get(receipt, "turnId"), get(stop, "turn_id")) && delivery.SameIdentity(get(receipt, "relationshipId"), get(object(get(marker, "relationship")), "relationshipId"))
}
func ClassifyDeclaration(o Observation) string {
	d, ok := evidence.Object(o.Disposition)
	var outcome any
	if ok && delivery.SameIdentity(get(d, "turnId"), get(o.Stop, "turn_id")) && delivery.SameIdentity(get(d, "sessionId"), get(o.Stop, "session_id")) {
		outcome = get(d, "outcome")
	}
	switch outcome {
	case "in_progress", "blocked_needs_input", "interrupted", "failed":
		delivery.MarkReturn(o.Reached, "classify_declaration", 1)
		return "declared_" + text(outcome)
	case "ready_for_review":
		if ReceiptMatches(o.Receipt, o.Stop, o.Marker) {
			delivery.MarkReturn(o.Reached, "classify_declaration", 2)
			return "declared_ready_receipted"
		}
		delivery.MarkReturn(o.Reached, "classify_declaration", 3)
		return "receipt_missing"
	}
	delivery.MarkReturn(o.Reached, "classify_declaration", 4)
	return "undeclared_turn_end"
}
func ObserveState(o Observation) (string, string) {
	if len(o.Unreadable) > 0 {
		u := slices.Clone(o.Unreadable)
		slices.Sort(u)
		u = slices.Compact(u)
		delivery.MarkReturn(o.Reached, "observe_state", 1)
		return "state_unreadable", "Cannot read " + strings.Join(u, ", ") + "."
	}
	if o.Marker != nil || o.Malformed != "" {
		bad := o.Malformed
		if bad == "" {
			bad = delivery.Malformed(o.Marker)
		}
		if bad != "" {
			delivery.MarkReturn(o.Reached, "observe_state", 2)
			return "marker_malformed", "The published " + bad + " is not the shape a fact must be. Repair the marker; a record that cannot be read is reported, never guessed at."
		}
	}
	if len(o.Marker) == 0 {
		delivery.MarkReturn(o.Reached, "observe_state", 3)
		return "unmanaged", "No assignment directory for this workspace."
	}
	session := get(o.Stop, "session_id")
	bound := object(get(o.Marker, "bound"))
	if len(bound) == 0 {
		if !delivery.CorrelatedTrace(o.Marker, session, o.Assignment, o.Reached) {
			delivery.MarkReturn(o.Reached, "observe_state", 4)
			return "dispatch_uncorrelated", "This session presented no matching dispatch request id."
		}
		delivery.MarkReturn(o.Reached, "observe_state", 5)
		return "correlated_unbound", "Correlated to the intent but not yet bound by the coordinator. Released; the turn's observation is recorded for the coordinator to fold once the bind lands."
	}
	if !delivery.Named(get(bound, "sessionId")) {
		delivery.MarkReturn(o.Reached, "observe_state", 6)
		return "bound_identity_unnamed", "The bind record names no session, so nothing can be shown to be the bound child. Repair the marker; a turn is never held against an identity nobody published."
	}
	if !delivery.SameIdentity(get(bound, "sessionId"), session) {
		delivery.MarkReturn(o.Reached, "observe_state", 7)
		return "marker_claimed_by_other_session", "This session is not the bound child."
	}
	declaration := ClassifyDeclaration(o)
	if strings.HasPrefix(declaration, "declared_") {
		delivery.MarkReturn(o.Reached, "observe_state", 8)
		return declaration, "The child declared this turn."
	}
	problem := delivery.CorrelationProblemTrace(o.Marker, session, o.Assignment, o.Reached)
	if problem == delivery.ClaimAbsent {
		delivery.MarkReturn(o.Reached, "observe_state", 9)
		return "marker_unclaimed", "The coordinator bound this session, but it has not claimed this assignment. Released and recorded; a hold needs the child's own claim, not only the coordinator's bind."
	}
	if problem != "" {
		delivery.MarkReturn(o.Reached, "observe_state", 10)
		return "claim_uncorrelated", "This session is bound but its claim does not correlate with this assignment (" + problem + "). Released and recorded; every fact this reads is create-once, so it does not clear itself and no resolution consumed here will: correlation reads the claim and the intent, never the adjudications. Recovery is a new assignment, declared for a fresh dispatch request id."
	}
	if !delivery.Named(get(object(get(o.Marker, "relationship")), "relationshipId")) {
		delivery.MarkReturn(o.Reached, "observe_state", 11)
		return "managed_unregistered", "This workspace is managed but its relationship is not registered. Register it, or record a disposition explaining why it cannot be."
	}
	ev := text(get(o.Receipt, "evidence"))
	if declaration == "receipt_missing" && slices.Contains(generationEvidence, ev) {
		detail := pyvalue.Str(get(o.Receipt, "detail"))
		if ev == "generation_absent" {
			delivery.MarkReturn(o.Reached, "observe_state", 12)
			return "receipt_missing", "The relay's store holds no record of the generation it reports as current for this relationship: " + detail + ". Nothing can be attributed to this assignment while the store cannot say which dispatch opened the generation it is on, and no receipt this session emits changes that. The relay's store is what needs repair."
		}
		delivery.MarkReturn(o.Reached, "observe_state", 13)
		return "receipt_missing", "This assignment's registration does not name the generation the relay is on: " + detail + ". No receipt this session emits can satisfy it, because the registration fact is create-once and cannot be republished onto the live generation, and a receipt earned there belongs to work this assignment never registered. Recovery is a new assignment, declared for a fresh dispatch request id."
	}
	if declaration == "receipt_missing" {
		delivery.MarkReturn(o.Reached, "observe_state", 14)
		return "receipt_missing", "Readiness is declared but no receipt stands at the current head revision for this session and turn. Emit the receipt over the actual artifacts."
	}
	delivery.MarkReturn(o.Reached, "observe_state", 15)
	return "undeclared_turn_end", "No usable turn disposition was recorded for this turn. Record in_progress, blocked_needs_input, interrupted, failed, or ready_for_review with a receipt."
}

// Decide classifies first and applies terminal exhaustion before transient hold guards.
func Decide(o Observation, counters Object, mode string) Object {
	state, reason := ObserveState(o)
	if !slices.Contains(omissions, state) {
		delivery.MarkReturn(o.Reached, "decide", 1)
	}
	if slices.Contains(omissions, state) {
		if bad := delivery.MalformedCounters(counters); bad != "" {
			state = "marker_malformed"
			reason = "Invalid persisted " + bad + "; repair the hold budget record."
		}
	}
	decision, finalState, finalReason := "release", state, reason
	count := func(k string) int64 { n, _ := evidence.IntOf(get(counters, k)); return n }
	if slices.Contains(omissions, state) {
		switch {
		case count("holdsThisGeneration") >= 2 || count("holdsThisSessionWindow") >= 3:
			delivery.MarkReturn(o.Reached, "decide", 2)
			finalState = "unresolved_handoff"
			finalReason = "Hold bound reached; recording an unresolved handoff instead of holding again."
		case count("holdsThisTurn") >= 1:
			delivery.MarkReturn(o.Reached, "decide", 3)
			finalState = "hold_in_flight"
			finalReason = "This turn already took its one hold."
		case pyvalue.Truthy(get(o.Stop, "stop_hook_active")):
			delivery.MarkReturn(o.Reached, "decide", 4)
			finalState = "hold_in_flight"
			finalReason = "A continuation is already running for this turn; the omission is recorded."
		default:
			delivery.MarkReturn(o.Reached, "decide", 5)
			decision = "block"
		}
	}
	if decision == "block" && mode != Hold {
		decision = "release"
		finalReason += " Observe-only: recorded without holding, because per-session write isolation was not asserted for this run."
	}
	record := Object{{Key: "observation", Value: state}, {Key: "turnId", Value: get(o.Stop, "turn_id")}, {Key: "sessionId", Value: get(o.Stop, "session_id")}, {Key: "decisionState", Value: finalState}, {Key: "held", Value: decision == "block"}, {Key: "mode", Value: mode}, {Key: "at", Value: o.Now}}
	result := Object{{Key: "decision", Value: decision}, {Key: "state", Value: finalState}, {Key: "observation", Value: state}, {Key: "reason", Value: finalReason}, {Key: "record", Value: record}}
	marker := o.Marker
	if len(o.Unreadable) > 0 || o.Malformed != "" || (len(marker) > 0 && delivery.Malformed(marker) != "") {
		marker = nil
	}
	if state == "correlated_unbound" {
		record = set(record, "pendingObservation", ClassifyDeclaration(o))
	}
	if state == "claim_uncorrelated" && len(marker) > 0 {
		ev := delivery.CorrelationProblem(marker, get(o.Stop, "session_id"), o.Assignment)
		record = set(record, "claimEvidence", ev)
		result = set(result, "claimEvidence", ev)
		record = set(record, "pendingObservation", ClassifyDeclaration(o))
	}
	if len(marker) > 0 {
		record = set(record, "assignmentState", delivery.DeriveAssignmentStateTrace(marker, o.Now, o.Reached))
		record = set(record, "identityContested", delivery.IdentityContestedTrace(marker, o.Reached))
	}
	if pyvalue.Truthy(get(o.Receipt, "evidence")) {
		record = set(record, "receiptEvidence", get(o.Receipt, "evidence"))
		result = set(result, "receiptEvidence", get(o.Receipt, "evidence"))
		if pyvalue.Truthy(get(o.Receipt, "detail")) {
			record = set(record, "receiptDetail", get(o.Receipt, "detail"))
			result = set(result, "receiptDetail", get(o.Receipt, "detail"))
		}
	}
	answer := Object{}
	if decision == "block" {
		answer = Object{{Key: "decision", Value: "block"}, {Key: "reason", Value: finalReason}, {Key: "continue", Value: true}}
	}
	result = set(result, "record", record)
	return set(result, "hook_output", answer)
}
