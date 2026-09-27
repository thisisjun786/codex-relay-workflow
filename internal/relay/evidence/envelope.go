package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

const (
	EnvelopeVersion    = "relay-envelope/1"
	Request            = "request"
	Notification       = "notification"
	Decision           = "decision"
	StatusResponse     = "status_response"
	Recipient          = "recipient"
	User               = "user"
	ChildToParent      = "child_to_parent"
	ParentToChild      = "parent_to_child"
	SupervisorToParent = "supervisor_to_parent"
	ParentToSupervisor = "parent_to_supervisor"
	Inherited          = "inherited"
	Unknown            = "unknown"
	NotApplicable      = "not_applicable"
	TransportAccepted  = "transport_accepted"
	Received           = "received"
	Agreed             = "agreed"
	Applied            = "applied"
	Verified           = "verified"
	Yes                = "yes"
	No                 = "no"
	Conditional        = "conditional"
	Unmeasured         = "unmeasured"
	Impossible         = "not_applicable"
)

var AnswerOwedBy = map[string]any{Request: Recipient, Decision: User, Notification: nil, StatusResponse: nil}
var endpointRoles = map[string][2]string{ChildToParent: {"child", "parent"}, ParentToChild: {"parent", "child"}, SupervisorToParent: {"supervisor", "parent"}, ParentToSupervisor: {"parent", "supervisor"}}
var purposes = map[string]map[string]string{
	ChildToParent:      {"completion": Request, "progress": Notification, "blocked": Request, "decision_request": Decision, "review_ready": Request},
	ParentToChild:      {"assignment": Request, "revision_request": Request, "resume": Request, "receipt_confirmation": Notification, "acceptance": Notification, "integration_result": Notification},
	SupervisorToParent: {"project_assignment": Request, "midpoint_check": Request, "resume": Request, "scope_correction": Request, "relayed_decision": Request, "user_stop": Request},
	ParentToSupervisor: {"completion": Notification, "blocked": Notification, "decision_request": Decision, "status_response": StatusResponse, "fault_notice": Notification, "fault_decision": Decision},
}
var stages = []string{TransportAccepted, Received, Agreed, Applied, Verified}
var reachSources = map[string]map[string]string{
	ChildToParent:      {TransportAccepted: "attempts", Received: "acks", Agreed: "acks", Applied: "verdicts", Verified: "verdicts"},
	ParentToChild:      {TransportAccepted: "attempts", Received: "", Agreed: "", Applied: "events", Verified: "verdicts"},
	SupervisorToParent: {TransportAccepted: "", Received: "scope_directives", Agreed: "scope_directives", Applied: "", Verified: ""},
	ParentToSupervisor: {TransportAccepted: "supervisor_attempts", Received: "supervisor_readbacks", Agreed: "", Applied: "", Verified: ""},
}
var noMechanism = map[string]string{
	ChildToParent:      "",
	ParentToChild:      "a revision request carries no acknowledgement; the completion receipt of the generation it opened is what shows it was applied",
	SupervisorToParent: "a directive is recorded rather than sent, so nothing transports, applies or verifies this one",
	ParentToSupervisor: "a report upward is transported and read back, and nothing here records that a supervisor agreed, applied or verified anything; what became of it is read from the Linear record, confirmed",
}

type EnvelopeError struct{ Detail string }

func (e *EnvelopeError) Error() string { return e.Detail }
func envelopeError(format string, args ...any) error {
	return &EnvelopeError{fmt.Sprintf(format, args...)}
}

func KindOf(direction, purpose string) (string, error) {
	kinds, ok := purposes[direction]
	if !ok {
		return "", envelopeError("%s is not a known direction; it is one of child_to_parent, parent_to_child, parent_to_supervisor, supervisor_to_parent", StrRepr(direction))
	}
	kind, ok := kinds[purpose]
	if !ok {
		keys := make([]string, 0, len(kinds))
		for key := range kinds {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		return "", envelopeError("%s is not a purpose %s carries; it has %s", StrRepr(purpose), direction, strings.Join(keys, ", "))
	}
	return kind, nil
}

func Absent(reason, detail string) (map[string]any, error) {
	if reason != Inherited && reason != Unknown && reason != NotApplicable {
		return nil, envelopeError("%s is not a known absence; it is one of inherited, not_applicable, unknown", StrRepr(reason))
	}
	return map[string]any{"absent": reason, "detail": detail}, nil
}
func absent(reason, detail string) map[string]any { v, _ := Absent(reason, detail); return v }
func IsAbsent(value any) bool {
	o, ok := Object(value)
	if !ok {
		return false
	}
	r := Get(o, "absent")
	return Equal(r, Inherited) || Equal(r, Unknown) || Equal(r, NotApplicable)
}
func Shown(value any) string {
	if IsAbsent(value) {
		o := Dict(value, false)
		if Truthy(o["detail"]) {
			return "<" + Text(o["absent"]) + ": " + Text(o["detail"]) + ">"
		}
		return "<" + Text(o["absent"]) + ">"
	}
	if value == nil {
		return ""
	}
	return Text(value)
}

func Stage(state, source, detail string) (map[string]any, error) {
	if !slices.Contains([]string{Yes, No, Conditional, Unmeasured, Impossible}, state) {
		return nil, envelopeError("%s is not a known reach state", StrRepr(state))
	}
	if (state == Yes || state == No || state == Conditional) && source == "" {
		return nil, envelopeError("a %s at a reach stage needs the record that says so", state)
	}
	return map[string]any{"state": state, "source": nilIfEmpty(source), "detail": detail}, nil
}
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func Unreached(direction string) (map[string]any, error) {
	sources, ok := reachSources[direction]
	if !ok {
		return nil, envelopeError("%s is not a known direction", StrRepr(direction))
	}
	out := map[string]any{}
	for _, name := range stages {
		if sources[name] == "" {
			out[name], _ = Stage(Impossible, "", noMechanism[direction])
		} else {
			out[name], _ = Stage(Unmeasured, "", "nothing readable answered yet")
		}
	}
	return out, nil
}
func StageHolds(ladder map[string]any, name string) bool {
	one := Dict(ladder[name], true)
	return Equal(one["state"], Yes)
}
func PromotionRefused(ladder map[string]any) []string {
	var out []string
	held := true
	for _, name := range stages {
		one := Dict(ladder[name], true)
		state := one["state"]
		if state == Impossible {
			continue
		}
		if state == Yes && !held {
			out = append(out, name)
		}
		held = state == Yes
	}
	return out
}
func CheckReach(direction string, ladder map[string]any) error {
	sources, ok := reachSources[direction]
	if !ok {
		return envelopeError("%s is not a known direction", StrRepr(direction))
	}
	for _, name := range stages {
		raw, present := ladder[name]
		if !present {
			return envelopeError("a reach ladder answers every stage; %s is missing. Start from unreached(direction) rather than from a partial dictionary", name)
		}
		one := Dict(raw, true)
		state, source := one["state"], one["source"]
		if !Equal(state, Yes) && !Equal(state, No) && !Equal(state, Conditional) && !Equal(state, Unmeasured) && !Equal(state, Impossible) {
			return envelopeError("%s is not a known reach state; it is one of conditional, no, not_applicable, unmeasured, yes", Repr(state))
		}
		declared := sources[name]
		if declared == "" && state != Impossible {
			return envelopeError("%s has no mechanism for %s, so it cannot answer %s: %s", direction, name, Repr(state), noMechanism[direction])
		}
		if declared != "" && (state == Yes || state == No || state == Conditional) && source != declared {
			return envelopeError("%s on %s is answered by %s, not by %s", name, direction, declared, Repr(one["source"]))
		}
	}
	return nil
}
func MessageID(direction, relation, purpose, subject string) (string, error) {
	if _, err := KindOf(direction, purpose); err != nil {
		return "", err
	}
	for name, value := range map[string]string{"relation_id": relation, "subject": subject} {
		if strings.TrimSpace(value) == "" {
			return "", envelopeError("%s must be a non-empty string", name)
		}
		if strings.Contains(value, "|") {
			return "", envelopeError("%s must not contain '|', which is the field separator", name)
		}
	}
	sum := sha256.Sum256([]byte(direction + "|" + relation + "|" + purpose + "|" + subject))
	return hex.EncodeToString(sum[:])[:32], nil
}
func Region(direction, purpose, relation, sender, recipient, subject, observed string, reach map[string]any) (map[string]any, error) {
	kind, err := KindOf(direction, purpose)
	if err != nil {
		return nil, err
	}
	id, err := MessageID(direction, relation, purpose, subject)
	if err != nil {
		return nil, err
	}
	if reach == nil {
		reach, _ = Unreached(direction)
	}
	roles := endpointRoles[direction]
	owed := AnswerOwedBy[kind]
	if owed == nil {
		owed = absent(NotApplicable, "this kind owes no answer")
	}
	out := map[string]any{"version": EnvelopeVersion, "direction": direction, "kind": kind, "purpose": purpose, "messageId": id, "relationId": relation, "relationRevision": absent(Unknown, "the link revision was not read"), "sender": map[string]any{"role": roles[0], "taskId": sender}, "recipient": map[string]any{"role": roles[1], "taskId": recipient}, "subject": subject, "scope": absent(Unknown, "no Linear scope was read"), "basis": absent(Unknown, "no generation or revision was read"), "observedAt": observed, "evidence": []any{}, "correlationId": absent(NotApplicable, "this message answers nothing earlier"), "replyTo": absent(NotApplicable, "no reply is directed at one message"), "answerOwedBy": owed, "decision": absent(NotApplicable, "no user decision is being asked for"), "reach": reach}
	if kind == Decision {
		return nil, envelopeError("a decision envelope cannot omit decision: %s", Shown(out["decision"]))
	}
	if kind == StatusResponse {
		return nil, envelopeError("a status_response envelope cannot omit correlationId: %s", Shown(out["correlationId"]))
	}
	if err := CheckReach(direction, reach); err != nil {
		return nil, err
	}
	return out, nil
}
