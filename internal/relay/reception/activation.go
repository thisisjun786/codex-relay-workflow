package reception

import (
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

var activationFacts = []string{"instructed", "activated", "nativeGoal"}
var activationStates = []string{"observed", "absent", "refused", "unverified", "not_applicable"}
var modes = []string{"loop", "non_loop", "coordination"}

func checkMode(mode any) error {
	if slices.Contains(modes, pyjson.Text(mode)) {
		return nil
	}
	return malformed("%s is not an execution mode; it is one of coordination, loop, non_loop", quote.Value(mode))
}
func ActivationFact(state, source any, detail string) (Obj, error) {
	if !slices.Contains(activationStates, pyjson.Text(state)) {
		return nil, malformed("%s is not an activation state; it is one of absent, not_applicable, observed, refused, unverified", quote.Value(state))
	}
	if slices.Contains([]any{"observed", "absent", "refused"}, state) && !truth(source) {
		return nil, malformed("an %s activation fact names the record that says so; without one it is unverified", state)
	}
	return O("state", state, "source", source, "detail", detail), nil
}
func ActivationClass(triple any, mode string, earlier any) (Obj, error) {
	if e := checkMode(mode); e != nil {
		return nil, e
	}
	for _, k := range activationFacts {
		if !Has(triple, k) {
			return nil, malformed("an activation reading answers all three facts; %s is missing. Start from unexamined(mode) rather than from a partial dictionary", k)
		}
	}
	instructed, activated, goal := Get(Get(triple, "instructed"), "state"), Get(Get(triple, "activated"), "state"), Get(Get(triple, "nativeGoal"), "state")
	if activated == "not_applicable" && mode == "loop" {
		return nil, malformed("a loop reading cannot answer not_applicable for activation: this mode arms a goalplan and persists phases, so an absent one is absent rather than inapplicable. That answer belongs to a mode that arms nothing")
	}
	class, reason := "L6", "nothing readable yet distinguishes the classes above; a single negative reading of activation state is not a verdict"
	switch {
	case instructed == "absent":
		class, reason = "L0", "the assignment read back from this dispatch carries no invocation and names no agreed alternative"
	case activated == "refused":
		class, reason = "L2", "binding or initialisation was attempted and refused, and the refusal itself is the evidence"
	case activated == "not_applicable":
		class, reason = "L5", "this mode arms no implementation FSM, so there is nothing missing; "+pyjson.Text(Get(Get(triple, "activated"), "detail"))
	case activated == "observed":
		class, reason = "L5", "the task's own bound goalplan and persisted phases answer for this assignment"
	case Get(Get(earlier, "activated"), "state") == "observed" && activated == "absent":
		class, reason = "L4", "activation was observed earlier on this assignment and the later reading runs without it, so it was lost rather than never made"
	case activated == "absent" && goal == "observed":
		class, reason = "L3", "a host goal is active and no goalplan or persisted phase answers, which is an unarmed loop wearing an active status"
	case activated == "absent" && instructed == "observed":
		class, reason = "L1", "the invocation was sent and nothing records it being loaded, bound or invoked"
	}
	return O("class", class, "reason", reason), nil
}

var Progression = []string{"read", "transport_accepted", "relay_ack", "criteria_verdict", "parent_acceptance", "merge_landing", "linear_done"}
var progressionSources = []string{"", "attempts", "acks", "verdicts", "verdicts", "merge_turns", "linear_issue_status"}

const noReading = "no row in this store records a recipient reading anything; an acknowledgement is the nearest fact and it is the next state, not this one"

func Unobserved() Obj {
	o := Obj{}
	for i, n := range Progression {
		state, detail := "unmeasured", "nothing readable answered yet"
		if progressionSources[i] == "" {
			state, detail = "not_applicable", noReading
		}
		Set(&o, n, Stage(state, nil, detail))
	}
	return o
}
func CheckProgression(ladder any) error {
	if _, ok := evidence.Object(ladder); !ok {
		return malformed("a handover ladder is an object of named states, not %s", quote.Kind(ladder))
	}
	for i, n := range Progression {
		if !Has(ladder, n) {
			return malformed("a handover ladder answers every state; %s is missing. Start from unobserved() rather than from a partial dictionary", n)
		}
		entry := Get(ladder, n)
		if _, ok := evidence.Object(entry); !ok {
			return malformed("each handover state is an object with a state and the record that answered it; %s is %s", n, quote.Kind(entry))
		}
		state := Get(entry, "state")
		if !slices.Contains(states, pyjson.Text(state)) {
			return malformed("%s is not a state; it is one of conditional, no, not_applicable, unmeasured, yes", quote.Value(state))
		}
		source := progressionSources[i]
		if source == "" {
			if state != "not_applicable" {
				return malformed("nothing answers %s, so it cannot say %s: %s", n, quote.Value(state), noReading)
			}
		} else if slices.Contains([]any{"yes", "no", "conditional"}, state) && Get(entry, "source") != source {
			return malformed("%s is answered by %s, not by %s", n, source, quote.Value(Get(entry, "source")))
		}
	}
	return nil
}
func UnsupportedPromotions(ladder any) ([]string, error) {
	if e := CheckProgression(ladder); e != nil {
		return nil, e
	}
	out := []string{}
	held := true
	for _, n := range Progression {
		state := Get(Get(ladder, n), "state")
		if state == "not_applicable" {
			continue
		}
		if state == "yes" && !held {
			out = append(out, n)
		}
		held = state == "yes"
	}
	return out, nil
}
func Claims(claimed, held any) (Obj, error) {
	unbacked, unmeasurable := []string{}, []string{}
	if claimed != nil {
		if e := CheckProgression(claimed); e != nil {
			return nil, e
		}
		for _, n := range Progression {
			if Get(Get(claimed, n), "state") != "yes" {
				continue
			}
			state := Get(Get(held, n), "state")
			if state == "yes" {
				continue
			}
			if state == "no" || state == "conditional" {
				unbacked = append(unbacked, n)
			} else {
				unmeasurable = append(unmeasurable, n)
			}
		}
	}
	return O("unbacked", unbacked, "unmeasurable", unmeasurable), nil
}
func ContentDigest(one any) string {
	payload := Obj{}
	o, _ := evidence.Object(one)
	region, _ := evidence.Object(Get(one, "envelope"))
	kept := Obj{}
	for _, f := range region {
		if !slices.Contains([]string{"observedAt", "scope", "basis", "reach", "messageId"}, f.Key) {
			kept = append(kept, f)
		}
	}
	for _, f := range o {
		if f.Key != "envelope" {
			payload = append(payload, f)
		}
	}
	Set(&payload, "envelope", kept)
	return digest(pyjson.Dumps(payload, pyjson.Options{Compact: true, SortKeys: true, Unicode: true}))
}
func Repeat(one, answered any) Obj {
	id := Get(Get(one, "envelope"), "messageId")
	prior := Get(answered, pyjson.Text(id))
	mine := ContentDigest(one)
	if !truth(prior) {
		return O("state", "first", "messageId", id, "contentDigest", mine)
	}
	if Get(prior, "contentDigest") == mine {
		applied := Get(prior, "applied") == true
		reason := "this id was answered already and asks for the same thing; nothing records it applied yet, so today's answer decides act until --applied records it"
		if applied {
			reason = "this id was answered already and asks for the same thing, and it is recorded applied, so it is not acted on again"
		}
		return O("state", "replay", "messageId", id, "contentDigest", mine, "disposition", Get(prior, "disposition"), "applied", applied, "answeredDigest", mine, "reason", reason)
	}
	return O("state", "collision", "messageId", id, "contentDigest", mine, "disposition", Get(prior, "disposition"), "answeredDigest", Get(prior, "contentDigest"), "reason", "this id was answered already and asks for something else. It is raised rather than given the earlier answer, which is also what stops a predictable id being spent in advance to suppress the real request")
}
func SettleRepeat(answer Obj, repeated any) Obj {
	settled := slices.Clone(answer)
	accepted := Get(answer, "disposition") == "accepted"
	if repeated == nil {
		Set(&settled, "repeat", O("state", "unchecked", "reason", "no reception ledger was named, so a repeat cannot be told from a first arrival"))
		Set(&settled, "act", false)
		return settled
	}
	state := Get(repeated, "state")
	switch state {
	case "first":
		Set(&settled, "repeat", O("state", "first", "contentDigest", Get(repeated, "contentDigest"), "applied", false))
		Set(&settled, "act", accepted)
	case "replay":
		applied := Get(repeated, "applied") == true
		Set(&settled, "repeat", O("state", "replay", "contentDigest", Get(repeated, "contentDigest"), "previousDisposition", Get(repeated, "disposition"), "applied", applied, "reason", Get(repeated, "reason")))
		Set(&settled, "act", accepted && !applied)
	default:
		problems, _ := evidence.List(Get(settled, "mismatches"))
		problems = append(slices.Clone(problems), Mismatch("message_collision", "messageId", Get(repeated, "answeredDigest"), Get(repeated, "contentDigest"), pyjson.Text(Get(repeated, "reason"))))
		Set(&settled, "mismatches", problems)
		Set(&settled, "disposition", "refused")
		Set(&settled, "repeat", O("state", "collision", "contentDigest", Get(repeated, "contentDigest"), "previousDisposition", Get(repeated, "disposition"), "reason", Get(repeated, "reason")))
		Set(&settled, "act", false)
	}
	if Get(settled, "act") == true && Get(Get(settled, "record"), "relationStatus") == "paused" {
		Set(&settled, "act", false)
		Set(&settled, "actHeld", "the relationship is paused: this packet is current and accepted, and it is acted on only after relationship-resume; check it again then")
	}
	return settled
}
