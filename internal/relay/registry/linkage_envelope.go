package registry

import (
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// The part of envelope.py the directive paths read: the pointer a directive row carries in its
// reference column, and where the instruction behind it stands (CRW-230).

const (
	envelopeVersion    = "relay-envelope/1"
	supervisorToParent = "supervisor_to_parent"
)

// envelopePurposes is envelope.PURPOSES without the kinds: which purposes each direction carries.
var envelopePurposes = map[string][]string{
	"child_to_parent":      {"completion", "progress", "blocked", "decision_request", "review_ready"},
	"parent_to_child":      {"assignment", "revision_request", "resume", "receipt_confirmation", "acceptance", "integration_result"},
	supervisorToParent:     {"project_assignment", "midpoint_check", "resume", "scope_correction", "relayed_decision", "user_stop"},
	"parent_to_supervisor": {"completion", "blocked", "decision_request", "status_response", "fault_notice", "fault_decision"},
}

// SupervisorPurposes is sorted(envelope.PURPOSES[SUPERVISOR_TO_PARENT]), linkage-directive's --purpose choices.
func SupervisorPurposes() []string {
	out := slices.Clone(envelopePurposes[supervisorToParent])
	slices.Sort(out)
	return out
}

// messageID is envelope.message_id.
func messageID(direction, relation, purpose, subject string) (string, error) {
	kinds, known := envelopePurposes[direction]
	if !known {
		directions := make([]string, 0, len(envelopePurposes))
		for d := range envelopePurposes {
			directions = append(directions, d)
		}
		slices.Sort(directions)
		return "", refuse(contract.RefusalMalformedReceipt, "%s is not a known direction; it is one of %s", pyStr(direction), strings.Join(directions, ", "))
	}
	if !slices.Contains(kinds, purpose) {
		sorted := slices.Clone(kinds)
		slices.Sort(sorted)
		return "", refuse(contract.RefusalMalformedReceipt, "%s is not a purpose %s carries; it has %s", pyStr(purpose), direction, strings.Join(sorted, ", "))
	}
	for _, field := range []struct{ name, value string }{{"relation_id", relation}, {"subject", subject}} {
		if strings.TrimSpace(field.value) == "" {
			return "", refuse(contract.RefusalMalformedReceipt, "%s must be a non-empty string", field.name)
		}
		if strings.Contains(field.value, "|") {
			return "", refuse(contract.RefusalMalformedReceipt, "%s must not contain '|', which is the field separator", field.name)
		}
	}
	return sha256Hex(direction + "|" + relation + "|" + purpose + "|" + subject)[:32], nil
}

// DirectiveReference is envelope.directive_reference; correlation "" with hasCorrelation false is None.
func DirectiveReference(purpose, link, digest, correlation string, hasCorrelation bool) (string, error) {
	if hasCorrelation {
		if strings.TrimSpace(correlation) == "" {
			return "", refuse(contract.RefusalMalformedReceipt, "a correlation id is a non-empty string or is absent")
		}
		if strings.Contains(correlation, "|") {
			return "", refuse(contract.RefusalMalformedReceipt, "a correlation id cannot contain '|', which separates the pointer's fields")
		}
		if correlation == "-" {
			return "", refuse(contract.RefusalMalformedReceipt, "'-' is how the pointer spells no correlation, so it cannot also be one")
		}
	}
	identifier, err := messageID(supervisorToParent, link, purpose, digest)
	if err != nil {
		return "", err
	}
	slot := "-"
	if hasCorrelation {
		slot = correlation
	}
	return envelopeVersion + "|" + strings.Join([]string{supervisorToParent, purpose, identifier, slot}, "|"), nil
}

type pointer struct {
	direction, purpose, messageID, correlation string
	hasCorrelation                             bool
}

// parseReference is envelope.parse_reference; nil is None (a NULL column included).
func parseReference(text any) *pointer {
	s, ok := text.(string)
	if !ok || !strings.HasPrefix(s, envelopeVersion+"|") {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(s, envelopeVersion+"|"), "|")
	if len(parts) != 4 {
		return nil
	}
	purposes, known := envelopePurposes[parts[0]]
	if !known || !slices.Contains(purposes, parts[1]) {
		return nil
	}
	return &pointer{direction: parts[0], purpose: parts[1], messageID: parts[2], correlation: parts[3], hasCorrelation: parts[3] != "-"}
}

func (p *pointer) correlationRepr() string {
	if !p.hasCorrelation {
		return "None"
	}
	return pyStr(p.correlation)
}

// envelopeContradiction is envelope.contradiction; "" is None.
func envelopeContradiction(reference any, link, digest string) (string, error) {
	parsed := parseReference(reference)
	if parsed == nil {
		return "", nil
	}
	if parsed.direction != supervisorToParent {
		return "a directive reference names " + parsed.direction + ", but a directive is always " + supervisorToParent, nil
	}
	expected, err := messageID(supervisorToParent, link, parsed.purpose, digest)
	if err != nil {
		return "", err
	}
	if expected != parsed.messageID {
		return "the reference carries messageId " + parsed.messageID + ", but this link and digest derive " + expected +
			"; the pointer belongs to another instruction", nil
	}
	return "", nil
}

// place is envelope.directive_place: kind is "sole", "answer" or "own"; nil is None.
type place struct{ kind, key string }

func directivePlace(reference any) *place {
	parsed := parseReference(reference)
	if parsed == nil || parsed.direction != supervisorToParent {
		return nil
	}
	if parsed.purpose == "project_assignment" || parsed.purpose == "scope_correction" {
		return &place{"sole", parsed.purpose}
	}
	if parsed.hasCorrelation {
		return &place{"answer", parsed.purpose + "|" + parsed.correlation}
	}
	return &place{"own", parsed.messageID}
}

func samePlace(a, b *place) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// directiveContest is linkage._directive_contest; "" is None.
func directiveContest(firstDigest string, firstReference any, secondDigest string, secondReference any) string {
	if firstDigest == secondDigest {
		return ""
	}
	here, there := directivePlace(firstReference), directivePlace(secondReference)
	if here == nil || there == nil {
		return "purpose_unknown"
	}
	if *here != *there {
		return ""
	}
	return here.kind
}

// pointerDisagreement is linkage._pointer_disagreement; "" is None.
func pointerDisagreement(stored, incoming any) string {
	first, second := parseReference(stored), parseReference(incoming)
	if second == nil {
		return ""
	}
	if first == nil {
		return "this directive id is already recorded with a reference that is not an" +
			" envelope pointer (" + pyRepr(stored) + "), so the purpose " + pyStr(second.purpose) +
			" and the correlation " + second.correlationRepr() + " you are asking for have nowhere to go on" +
			" it. The recorded instruction is preserved; a later one is recorded as its" +
			" own directive with its own digest. Settling the recorded one does not free" +
			" this digest: the id is derived from it, live or settled"
	}
	if first.purpose == second.purpose && first.correlation == second.correlation {
		return ""
	}
	if first.purpose == second.purpose {
		return "this directive id already answers " + first.correlationRepr() + " and the incoming pointer answers " +
			second.correlationRepr() + "; one digest cannot answer two messages, so the later one is recorded as" +
			" its own directive with its own digest"
	}
	return "this directive id already records the purpose " + pyStr(first.purpose) + " and the incoming pointer names " +
		pyStr(second.purpose) + "; one digest cannot be two instructions, so the later one is recorded as its" +
		" own directive with its own digest"
}
