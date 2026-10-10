package review

import (
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// MaxBlockers is the largest blocker count a verdict line carries, the relay renderer's limit (REVIEW-OUTPUT-01).
const MaxBlockers = goalplan.MaxBlockers

// The one grammar of a reviewer's verdict (CRW-1116, port: fixed). The words are the oracle's: PASS, FAIL, NEAR-PASS and
// GO-WITH-FIXES, case-insensitive. A GO-WITH-FIXES or NEAR-PASS may carry, in one parenthesis, the number of blockers the
// reviewer went ahead with and the references of the findings behind them:
//
//	VERDICT: GO-WITH-FIXES (blockers=2)
//	VERDICT: GO-WITH-FIXES (blockers=2; findings=c1,r2)
//
// The sign-off parser reads it, and the relay's report renderer writes it through BlockerSuffix, so the line the skill asks for,
// the line the relay renders and the line the observer records cannot drift apart. The bare words stay valid (legacy), and a
// count that is not a whole number from 1 to MaxBlockers, a parenthesis the grammar does not know, or any text after it is not
// a verdict at all: it is never read as the bare word with the suffix cut off.

// BlockerSuffix is the parenthesis that follows GO-WITH-FIXES on a verdict line, with its leading space. The findings are the
// references of ValidFindingRef; none leaves the count alone.
func BlockerSuffix(blockers int, findings []string) string {
	suffix := " (blockers=" + strconv.Itoa(blockers)
	if len(findings) > 0 {
		suffix += "; findings=" + strings.Join(findings, ",")
	}
	return suffix + ")"
}

type verdictLine struct {
	verdict  ReviewVerdict
	blockers int
	findings []string
}

// parseVerdictLine reads the value of a VERDICT line (the text after the colon, trimmed).
func parseVerdictLine(raw string) (verdictLine, bool) {
	head, rest, decorated := strings.Cut(raw, "(")
	var out verdictLine
	switch reviewVerdictUpper(text.Trim(head)) {
	case "PASS":
		out.verdict = goalplan.VerdictPass
	case "FAIL":
		out.verdict = goalplan.VerdictFail
	case "NEAR-PASS", "GO-WITH-FIXES":
		out.verdict = goalplan.VerdictNearPass
	default:
		return verdictLine{}, false
	}
	if !decorated {
		return out, true
	}
	if out.verdict != goalplan.VerdictNearPass {
		return verdictLine{}, false
	}
	inner, ok := strings.CutSuffix(rest, ")")
	if !ok || strings.ContainsAny(inner, "()") {
		return verdictLine{}, false
	}
	count, refs, hasRefs := strings.Cut(inner, ";")
	n, ok := verdictLineField(count, "blockers")
	if !ok || len(n) > 4 {
		return verdictLine{}, false
	}
	for i := 0; i < len(n); i++ {
		if n[i] < '0' || n[i] > '9' {
			return verdictLine{}, false
		}
	}
	blockers, err := strconv.Atoi(n)
	if err != nil || blockers < 1 || blockers > MaxBlockers {
		return verdictLine{}, false
	}
	out.blockers = blockers
	if hasRefs {
		list, ok := verdictLineField(refs, "findings")
		if !ok {
			return verdictLine{}, false
		}
		parts := strings.Split(list, ",")
		if len(parts) > goalplan.MaxFindingRefs {
			return verdictLine{}, false
		}
		for _, p := range parts {
			p = text.Trim(p)
			if !goalplan.ValidFindingRef(p) {
				return verdictLine{}, false
			}
			out.findings = append(out.findings, p)
		}
	}
	return out, true
}

// verdictLineField reads `label = value` with the label in any ASCII case.
func verdictLineField(s, label string) (string, bool) {
	s = text.Trim(s)
	if len(s) < len(label) {
		return "", false
	}
	for i := range label {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		if c != label[i] {
			return "", false
		}
	}
	rest := text.Trim(s[len(label):])
	value, ok := strings.CutPrefix(rest, "=")
	if !ok {
		return "", false
	}
	value = text.Trim(value)
	return value, value != ""
}
