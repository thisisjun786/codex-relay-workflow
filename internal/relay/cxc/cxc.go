package cxc

import (
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// Error retains Python's exception class and str(exception), including KeyError quoting.
type Error struct{ Kind, Message string }

func (e *Error) Error() string        { return e.Message }
func valueError(message string) error { return &Error{"ValueError", message} }
func refusal(message string) error {
	return &Error{"ReceiptRefused", "outcome_inconsistent: " + message}
}
func unhashable(value any) error {
	switch value.(type) {
	case []any:
		return &Error{"TypeError", "unhashable type: 'list'"}
	case map[string]any:
		return &Error{"TypeError", "unhashable type: 'dict'"}
	}
	return nil
}
func Provenance() map[string]any {
	return map[string]any{"package": Package, "version": Version, "sources": Sources}
}
func SourcesFor(rule any) []Source {
	out := []Source{}
	for _, s := range Sources {
		if rule == s.Rule {
			out = append(out, s)
		}
	}
	return out
}
func AffectedBy(paths []string) []string {
	out := []string{}
	for _, s := range Sources {
		if slices.Contains(paths, s.Path) && !slices.Contains(out, s.Rule) {
			out = append(out, s.Rule)
		}
	}
	slices.Sort(out)
	return out
}
func CheckKnown(status any) error {
	s, ok := status.(string)
	if !ok || !slices.Contains(ReportStatuses, s) {
		return refusal(fmt.Sprintf("%s is not a CXC report status this build maps. Accepted: %s. Read against %s %s; a newer contract needs the mapping extended rather than the value guessed at", evidence.Repr(status), strings.Join(ReportStatuses, ", "), Package, Version))
	}
	return nil
}
func CheckStatus(status, outcome any) error {
	if err := CheckKnown(status); err != nil {
		return err
	}
	s := status.(string)
	o, ok := outcome.(string)
	if !ok || !slices.Contains(CompatibleOutcomes[s], o) {
		return refusal(fmt.Sprintf("a %s report cannot accompany outcome %s: %s means %s, which this contract pairs with %s", s, evidence.Repr(outcome), s, Meaning[s], strings.Join(CompatibleOutcomes[s], ", ")))
	}
	return nil
}
func RefusePromotion(fact any) (string, error) {
	if err := unhashable(fact); err != nil {
		return "", err
	}
	if s, ok := fact.(string); ok {
		if reason, found := NotVerification[s]; found {
			return reason, nil
		}
	}
	return "", &Error{"KeyError", evidence.StrRepr(evidence.Repr(fact) + " is not a recorded non-verification fact; add it with its reason rather than letting an unlisted fact through by omission")}
}
func VerdictLine(kind, blockers any) (string, error) {
	k, ok := kind.(string)
	if !ok || !slices.Contains(VerdictKinds, k) {
		return "", valueError(fmt.Sprintf("%s is not a review verdict; REVIEW-OUTPUT-01 fixes %s", evidence.Repr(kind), strings.Join(VerdictKinds, ", ")))
	}
	if k == GoWithFixes {
		var digits string
		switch n := blockers.(type) {
		case int:
			digits = strconv.Itoa(n)
		case int64:
			digits = strconv.FormatInt(n, 10)
		case json.Number:
			if !strings.ContainsAny(string(n), ".eE") {
				digits = string(n)
			}
		}
		n, valid := new(big.Int).SetString(digits, 10)
		if !valid || n.Sign() < 1 {
			return "", valueError("GO-WITH-FIXES states how many blockers it is going ahead with; a count below one is a PASS and should say so")
		}
		if n.Cmp(big.NewInt(BlockersMax)) > 0 {
			return "", valueError(fmt.Sprintf("a blocker count of %s is past the point of being a review, and it sits on a line the message cannot shorten; the limit is %d", n, BlockersMax))
		}
		return fmt.Sprintf("%s%s (blockers=%s)", Prefix, k, n), nil
	}
	if blockers != nil {
		return "", valueError(fmt.Sprintf("a %s verdict carries no blocker count", k))
	}
	return Prefix + k, nil
}
func ParseVerdictLine(line any) (map[string]any, error) {
	if !evidence.Truthy(line) {
		line = ""
	}
	s, ok := line.(string)
	if !ok {
		name := "int"
		switch value := line.(type) {
		case bool:
			name = "bool"
		case []any:
			name = "list"
		case map[string]any:
			name = "dict"
		case float64:
			name = "float"
		case json.Number:
			if strings.ContainsAny(string(value), ".eE") {
				name = "float"
			}
		}
		return nil, &Error{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'strip'", name)}
	}
	text := strip(s)
	if !strings.HasPrefix(text, Prefix) {
		return nil, nil
	}
	body := strip(strings.TrimPrefix(text, Prefix))
	if body == Pass || body == Fail {
		return map[string]any{"kind": body, "blockers": nil}, nil
	}
	if strings.HasPrefix(body, GoWithFixes) {
		rest := strip(strings.TrimPrefix(body, GoWithFixes))
		if strings.HasPrefix(rest, "(blockers=") && strings.HasSuffix(rest, ")") {
			digits := strings.TrimSuffix(strings.TrimPrefix(rest, "(blockers="), ")")
			normalized := ""
			nonDecimal := false
			for _, r := range digits {
				if !unicode.IsDigit(r) {
					if !otherDigit(r) {
						return nil, nil
					}
					nonDecimal = true
				}
				// Unicode Nd digits are in consecutive runs of ten.
				for _, table := range unicode.Digit.R16 {
					if uint32(r) >= uint32(table.Lo) && uint32(r) <= uint32(table.Hi) && (uint32(r)-uint32(table.Lo))%uint32(table.Stride) == 0 {
						normalized += strconv.Itoa(int((uint32(r) - uint32(table.Lo)) / uint32(table.Stride) % 10))
						break
					}
				}
				for _, table := range unicode.Digit.R32 {
					if uint32(r) >= table.Lo && uint32(r) <= table.Hi && (uint32(r)-table.Lo)%table.Stride == 0 {
						normalized += strconv.Itoa(int((uint32(r) - table.Lo) / table.Stride % 10))
						break
					}
				}
			}
			if nonDecimal {
				return nil, valueError("invalid literal for int() with base 10: " + evidence.StrRepr(digits))
			}
			n, err := strconv.Atoi(normalized)
			if err == nil && n >= 1 && n <= BlockersMax {
				return map[string]any{"kind": GoWithFixes, "blockers": n}, nil
			}
		}
	}
	return nil, nil
}

// Python str.isspace also includes the four ASCII information separators.
func strip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f })
}

// Unicode Digit but not Decimal values pass str.isdigit, then int() raises.
func otherDigit(r rune) bool {
	for _, span := range [][2]rune{{0xb2, 0xb3}, {0xb9, 0xb9}, {0x1369, 0x1371}, {0x19da, 0x19da}, {0x2070, 0x2070}, {0x2074, 0x2079}, {0x2080, 0x2089}, {0x2460, 0x2468}, {0x2474, 0x247c}, {0x2488, 0x2490}, {0x24ea, 0x24ea}, {0x24f5, 0x24fd}, {0x24ff, 0x24ff}, {0x2776, 0x277e}, {0x2780, 0x2788}, {0x278a, 0x2792}, {0x10a40, 0x10a43}, {0x10e60, 0x10e68}, {0x11052, 0x1105a}, {0x1f100, 0x1f10a}} {
		if r >= span[0] && r <= span[1] {
			return true
		}
	}
	return false
}

func AssertReviewed(hasReview any) error {
	if !evidence.Truthy(hasReview) {
		return valueError("a verdict line states a review judgment; a progress or completion notice that renders one is dressing an update as a review")
	}
	return nil
}

// ClassifyWait accepts keyword-shaped input; omitted observable defaults to true.
func ClassifyWait(options map[string]any) map[string]any {
	state, reason := SuspectedStagnation, "comparable observations show no advancement yet"
	observable, present := options["observable"]
	switch {
	case evidence.Truthy(options["terminal_error"]):
		state, reason = ConfirmedFailure, evidence.Text(options["terminal_error"])
	case evidence.Truthy(options["input_requested"]):
		state, reason = InputNeeded, "the work is waiting on an answer, not failing"
	case evidence.Truthy(options["advancing_evidence"]):
		state, reason = Progress, "observations advanced since the last look"
	case present && !evidence.Truthy(observable):
		state, reason = Unobservable, "available observations establish neither progress nor failure; the gap is the finding"
	case evidence.Truthy(options["stagnation_confirmed"]):
		state, reason = ConfirmedFailure, "no advancement at the stated review point, compared against the prior observation"
	case evidence.Truthy(options["timed_out"]):
		state, reason = TimedOut, "the wait ended on its own bound. That is a normal outcome: it is neither a failure nor permission to start the work again"
	}
	return map[string]any{"state": state, "reason": reason, "authorisesRerun": false, "isFailure": state == ConfirmedFailure}
}
func DispatchProblems(body any) []string   { return SectionProblems(body, DispatchSections) }
func CorrectionProblems(body any) []string { return SectionProblems(body, CorrectionSections) }
func SectionProblems(body any, sections []string) []string {
	text, ok := body.(string)
	seen := map[string]bool{}
	if ok {
		current := ""
		// str.splitlines includes vertical tabs, form feeds and Unicode separators.
		for _, line := range strings.FieldsFunc(text, func(r rune) bool { return strings.ContainsRune("\n\r\v\f\x1c\x1d\x1e\x85\u2028\u2029", r) }) {
			stripped := strip(strings.TrimLeft(strip(line), "-*#>"))
			heading := strings.ToUpper(strings.Trim(stripped, "*`_"))
			opened := ""
			for _, name := range sections {
				if heading == name || strings.HasPrefix(heading, name+":") {
					opened = name
					break
				}
			}
			if opened != "" {
				rest := strip(strings.TrimLeft(strings.TrimPrefix(heading, opened), ":"))
				if rest != "" {
					seen[opened] = true
					current = ""
				} else {
					current = opened
				}
			} else if current != "" && stripped != "" {
				seen[current] = true
				current = ""
			}
		}
	}
	out := []string{}
	for _, name := range sections {
		if !seen[name] {
			out = append(out, name)
		}
	}
	return out
}
func SkillPointer(activity any) (string, error) {
	if err := unhashable(activity); err != nil {
		return "", err
	}
	if s, ok := activity.(string); ok {
		if owner, found := SkillPointers[s]; found {
			return owner, nil
		}
	}
	return "", &Error{"KeyError", evidence.StrRepr(evidence.Repr(activity) + " has no recorded owner; name the owning skill rather than sending a recipient to reload everything")}
}
