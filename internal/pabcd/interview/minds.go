package interview

// CXC v0.2.40 minds.ts (3c1459ac). These are read-only lenses; this library
// never dispatches them. The hook copy of the directive remains until its owning
// integration replaces it; both are pinned to the same renamed oracle bytes.

import (
	"encoding/json"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Mind is one of the five frozen contradiction lenses.
type Mind string

const (
	MindContrarian Mind = "contrarian"
	MindSocratic   Mind = "socratic"
	MindOntologist Mind = "ontologist"
	MindEvaluator  Mind = "evaluator"
	MindSimplifier Mind = "simplifier"
	// MindConcurrencyCap is the recommended concurrent dispatch cap.
	MindConcurrencyCap = 3
)

// Minds returns fresh canonical ids without a package initializer.
func Minds() [5]Mind {
	return [5]Mind{MindContrarian, MindSocratic, MindOntologist, MindEvaluator, MindSimplifier}
}

const mindsPromptContrarian = `[Mind: Contrarian] Challenge the stated goals and constraints. Be skeptical, not rude:
where is an assumption unjustified, a goal in tension with a constraint, or a claim unproven?
Return ONLY a JSON array of contradictions. Each item:
{ "dimension": "goal|constraint|success|ontology", "contradiction": "<short gap/conflict>",
  "severity": "low|medium|high", "evidence": "<file:line, section ref, or exact short quote>" }.
Empty array [] means you found no contradiction. Do NOT ask questions, edit files, call the user,
choose options, write state, or edit the plan. Evidence must be real (no unsupported guesses).`

const mindsPromptSocratic = `[Mind: Socratic] Probe vague terms and missing operational definitions. Which words lack a
testable meaning? Which success terms are undefined? Surface the undefined, not opinions.
Return ONLY a JSON array of contradictions. Each item:
{ "dimension": "goal|constraint|success|ontology", "contradiction": "<short gap/conflict>",
  "severity": "low|medium|high", "evidence": "<file:line, section ref, or exact short quote>" }.
Empty array [] means you found no contradiction. Do NOT ask questions, edit files, call the user,
choose options, write state, or edit the plan. Evidence must be real (no unsupported guesses).`

const mindsPromptOntologist = `[Mind: Ontologist] Check entity/relationship/field/data-model completeness. What entity,
relation, field, or state is referenced but never defined, or modeled inconsistently?
Return ONLY a JSON array of contradictions. Each item:
{ "dimension": "goal|constraint|success|ontology", "contradiction": "<short gap/conflict>",
  "severity": "low|medium|high", "evidence": "<file:line, section ref, or exact short quote>" }.
Empty array [] means you found no contradiction. Do NOT ask questions, edit files, call the user,
choose options, write state, or edit the plan. Evidence must be real (no unsupported guesses).`

const mindsPromptEvaluator = `[Mind: Evaluator] Check success criteria for measurability and outcome-level parsimony. Which
acceptance criterion is unmeasurable, untestable, or redundant with another?
Return ONLY a JSON array of contradictions. Each item:
{ "dimension": "goal|constraint|success|ontology", "contradiction": "<short gap/conflict>",
  "severity": "low|medium|high", "evidence": "<file:line, section ref, or exact short quote>" }.
Empty array [] means you found no contradiction. Do NOT ask questions, edit files, call the user,
choose options, write state, or edit the plan. Evidence must be real (no unsupported guesses).`

const mindsPromptSimplifier = `[Mind: Simplifier] Find over-engineering, redundant constraints, and scope creep. What can be
removed without losing a real requirement? Where does scope exceed the stated goal?
Return ONLY a JSON array of contradictions. Each item:
{ "dimension": "goal|constraint|success|ontology", "contradiction": "<short gap/conflict>",
  "severity": "low|medium|high", "evidence": "<file:line, section ref, or exact short quote>" }.
Empty array [] means you found no contradiction. Do NOT ask questions, edit files, call the user,
choose options, write state, or edit the plan. Evidence must be real (no unsupported guesses).`

// MindDispatchDirective is the main-session dispatch pointer, copied under the name table.
const MindDispatchDirective = `[crw: INTERVIEW — Mind dispatch]
You (the main session) OWN this interview loop: select Minds, dispatch contradiction workers,
triage contradictions, ask the user if needed, edit the plan, update state, and re-question.
The hook only injects directives — it does not coordinate worker returns or plan edits.
Dispatch Minds ONLY from the top-level main session; if you are yourself a subagent, Mind
dispatch is unavailable (no nested orchestration) — fall back to inline reasoning, do not nest.
Each Mind is a read-only lens: it returns contradictions ONLY (never asks/edits/calls/writes).
Choose Minds by lowest-scoring dimensions; concurrent cap 3.
MIND-SPAWN-SHAPE-01: only when Mind dispatch is authorized, fully read crw-interview's
references/mind-dispatch.md before dispatch. Use the live tool schema; never invent unsupported arguments.
Keep read-only explorer intent, mind_<mindname> labels, NON-full-history tasks and explicit user settings.
Minds are stateless: pack the lens prompt PLUS a compact interview snapshot (dimension scores,
knowns, open assumptions, draft plan path) into each task message.
State + plan artifacts live under .crw/ (session tracker + .crw/plan/).`

// MindRolePrompt returns the fixed lens prompt; an unknown id has none.
func MindRolePrompt(mind Mind) string {
	switch mind {
	case MindContrarian:
		return mindsPromptContrarian
	case MindSocratic:
		return mindsPromptSocratic
	case MindOntologist:
		return mindsPromptOntologist
	case MindEvaluator:
		return mindsPromptEvaluator
	case MindSimplifier:
		return mindsPromptSimplifier
	}
	return ""
}

// MindContradiction is the six-field allowlist; worker action keys never survive.
type MindContradiction struct {
	Mind          Mind                  `json:"mind"`
	CorrelationID string                `json:"correlationId"`
	Dimension     Dimension             `json:"dimension"`
	Contradiction string                `json:"contradiction"`
	Severity      ContradictionSeverity `json:"severity"`
	Evidence      string                `json:"evidence"`
}

// NormalizeMindOutput validates parsed JSON and attaches the frozen round-mind
// correlation id. An omitted or malformed round is 0; rejected input is [] named
// as such rather than nil/null. Evidence is an oracle heuristic, not verification.
func NormalizeMindOutput(mind Mind, raw any, roundID ...any) []MindContradiction {
	out := []MindContradiction{}
	ids := Minds()
	if !slices.Contains(ids[:], mind) {
		return out
	}
	rows, ok := raw.([]any)
	if !ok {
		return out
	}
	var round any
	if len(roundID) > 0 {
		round = roundID[0]
	}
	b, _ := json.Marshal(rescanRound(round))
	id := string(b) + "-" + string(mind)
	dimensions := DimensionOrder()
	for _, v := range rows {
		row, ok := v.(map[string]any)
		if !ok {
			continue
		}
		d, ok := row["dimension"].(string)
		if !ok || !slices.Contains(dimensions[:], Dimension(d)) {
			continue
		}
		severity, ok := row["severity"].(string)
		if !ok {
			continue
		}
		sev, valid := severityOf(severity)
		if !valid {
			continue
		}
		gap, ok := row["contradiction"].(string)
		if !ok || text.Trim(gap) == "" {
			continue
		}
		evidence, ok := row["evidence"].(string)
		if !ok || !mindsGrounded(evidence) {
			continue
		}
		out = append(out, MindContradiction{mind, id, Dimension(d), text.Trim(gap), sev, text.Trim(evidence)})
	}
	return out
}

func mindsGrounded(s string) bool {
	v := text.Trim(s)
	if v == "" {
		return false
	}
	// JavaScript \s, ASCII ignore-case/word boundaries, and all four dot-excluded
	// line terminators. RE2's \s, (?i) and dot alone are not equivalent.
	const space = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`
	if regexp.MustCompile(`[^` + space + `:]+:[0-9]+`).MatchString(v) {
		return true
	}
	if regexp.MustCompile(`(^|\b)([sS][eE][cC][tT][iI][oO][nN]\b|##|[lL][0-9]+(\.[0-9]+)?)`).MatchString(v) {
		return true
	}
	return regexp.MustCompile(`["'“”‘’][^\n\r\x{2028}\x{2029}]+["'“”‘’]`).MatchString(v)
}

// SelectMinds sorts weakest dimensions first, stable in canonical order. An
// omitted count is 2; NaN produces [], as the oracle's slice(0, NaN) does.
func SelectMinds(tracker any, count ...float64) []Mind {
	n := 2.0
	if len(count) > 0 {
		n = math.Max(1, math.Min(MindConcurrencyCap, math.Floor(count[0])))
	}
	ids := Minds()
	rank := [5]float64{}
	for i, dim := range [5]Dimension{DimensionConstraint, DimensionGoal, DimensionOntology, DimensionSuccess, DimensionConstraint} {
		rank[i] = mindsRank(mindsLevel(tracker, dim))
	}
	if math.IsNaN(n) {
		return []Mind{}
	}
	// The inherited-property comparator is not transitive. Reproduce V8's sort
	// of five elements: detect/reverse its initial run, then binary insertion.
	indices := []int{0, 1, 2, 3, 4}
	less := func(a, b int) bool {
		d := rank[a] - rank[b]
		if d != 0 && !math.IsNaN(d) {
			return d < 0
		}
		return a < b
	}
	run := 2
	descending := less(indices[1], indices[0])
	for run < len(indices) && less(indices[run], indices[run-1]) == descending {
		run++
	}
	if descending {
		slices.Reverse(indices[:run])
	}
	for i := run; i < len(indices); i++ {
		pivot := indices[i]
		lo, hi := 0, i
		for lo < hi {
			mid := (lo + hi) / 2
			if less(pivot, indices[mid]) {
				hi = mid
			} else {
				lo = mid + 1
			}
		}
		copy(indices[lo+1:i+1], indices[lo:i])
		indices[lo] = pivot
	}
	out := []Mind{}
	for _, i := range indices[:int(n)] {
		out = append(out, ids[i])
	}
	return out
}

func mindsLevel(tracker any, dim Dimension) any {
	switch t := tracker.(type) {
	case *Tracker:
		if t != nil {
			return string(t.Dimensions.Score(dim).Level)
		}
	case map[string]any:
		ds, _ := t["dimensions"].(map[string]any)
		score, _ := ds[string(dim)].(map[string]any)
		return score["level"]
	}
	return nil
}

func mindsRank(level any) float64 {
	switch mindsPropertyKey(level) {
	case "mid":
		return 1
	case "high":
		return 2
	case "max":
		return 3
	case "constructor", "__proto__", "__defineGetter__", "__defineSetter__", "hasOwnProperty", "__lookupGetter__", "__lookupSetter__", "isPrototypeOf", "propertyIsEnumerable", "toString", "valueOf", "toLocaleString":
		return math.NaN()
	}
	return 0
}

// LEVEL_RANK's bracket lookup converts JSON levels to property keys, arrays
// included. Prototype property values subtract as NaN and fall back to id order.
func mindsPropertyKey(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = mindsPropertyKey(e)
		}
		return strings.Join(parts, ",")
	case map[string]any:
		// With string hint JS tries toString before valueOf. JSON can shadow
		// that method with a noncallable value; valueOf then returns the object.
		if _, shadowed := x["toString"]; shadowed {
			panic("Cannot convert object to primitive value")
		}
		return "[object Object]"
	}
	b, _ := json.Marshal(v)
	return string(b)
}
