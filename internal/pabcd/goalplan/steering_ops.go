package goalplan

// CXC v0.2.40 pabcd-state/src/steering.ts:39-63,72-153,182-239 (commit 3c1459ac). The
// op grammar, its validation and the pure fold of a batch into a plan: a steering batch
// can only ADD (a criterion, a work phase) or annotate, so it can never lower the
// completion bar, and the refusal rule holds by construction rather than by a check.
// Everything the oracle's own doc comments say about applySteeringBatch - the
// idempotency key, the shared goalplan write lock and the ledger - is B15b (CRW-643)
// and is not here. Behaviour is ported as-is, oracle defects included; the c-N minting
// above 2^53 is recorded in docs/port-cxc/known-defects.md.
import (
	"errors"
	"math"
	"strconv"
	"strings"

	jstext "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// The reason-text fragments that hold a double quote. Go string literals would need a
// backslash for each one, so they are raw strings and the quote itself is a rune, which
// keeps every reason byte-identical to the oracle's without an escape in sight.
const (
	steeringOpsQuote            = string(rune(34))
	steeringOpsSurfaceList      = `"logic", "web", "tui", or "desktop"`
	steeringOpsSupportedSet     = `"annotate", "add-criterion", or "add-work-phase"`
	steeringOpsNativeList       = `"native"`
	steeringOpsDesktopList      = `"desktop"`
	steeringOpsExampleWorkPhase = `"wp04-loop-criteria"`
)

// steeringOpsQuoted is the oracle's template-literal insertion of a value: the text
// wrapped in plain double quotes with nothing escaped.
func steeringOpsQuoted(s string) string { return steeringOpsQuote + s + steeringOpsQuote }

// SteerOpKind is the kind of a steering op. Only additive kinds are supported: a
// remove-criterion or supersede-work-phase op would weaken a plan and needs the refusal
// rule designed first, so an unknown kind is a rejection.
type SteerOpKind string

// The kinds of a steering op.
const (
	SteerOpAnnotate     SteerOpKind = "annotate"
	SteerOpAddCriterion SteerOpKind = "add-criterion"
	SteerOpAddWorkPhase SteerOpKind = "add-work-phase"
)

// SteerOp is one op of a batch: the oracle's three-way union flattened into one struct,
// since Go has no sum type and the validation that fills it also fixes which fields each
// kind uses. Note belongs to annotate; Scenario, Surface, Presented and ExpectedEvidence
// to add-criterion; ID, Title and DependsOn to add-work-phase. An absent Surface is the
// zero value and defaults to logic; a nil DependsOn is the oracle's absent-or-empty array.
type SteerOp struct {
	Kind SteerOpKind

	Note string

	Scenario         string
	Surface          CriterionSurface
	Presented        PresentedSurface
	ExpectedEvidence string

	ID        string
	Title     string
	DependsOn []string
}

// SteerBatch is one steering transaction: the key that makes it idempotent, the
// rationale and evidence the loop promises to record with it, and the ops themselves.
type SteerBatch struct {
	IdempotencyKey string
	Rationale      string
	Evidence       string
	Ops            []SteerOp
}

// SteerResultKind is what applying a batch answered.
type SteerResultKind string

// The kinds of a steering answer. locked carries a refusal from the shared goalplan
// write lock and duplicate the entry whose idempotency key was already recorded.
const (
	SteerResultApplied   SteerResultKind = "applied"
	SteerResultDuplicate SteerResultKind = "duplicate"
	SteerResultRejected  SteerResultKind = "rejected"
	SteerResultLocked    SteerResultKind = "locked"
)

// SteerResult is the oracle's SteerResult union: applied carries the new plan and its
// ledger entry (plus a warning when the entry could not be written), duplicate carries
// the entry that was already there, rejected and locked carry only a reason.
type SteerResult struct {
	Kind    SteerResultKind `json:"kind"`
	Plan    *Goalplan       `json:"plan,omitempty"`
	Entry   *SteeringEntry  `json:"entry,omitempty"`
	Reason  string          `json:"reason,omitempty"`
	Warning string          `json:"warning,omitempty"`
}

// steeringOpsSupportedOps is SUPPORTED_OPS (:72): the mutating kinds that land here.
func steeringOpsSupportedOps(kind string) bool {
	switch SteerOpKind(kind) {
	case SteerOpAnnotate, SteerOpAddCriterion, SteerOpAddWorkPhase:
		return true
	}
	return false
}

// steeringOpsSupportedSurfaces is the SURFACES set (:88).
func steeringOpsSupportedSurfaces(surface string) bool {
	switch CriterionSurface(surface) {
	case SurfaceLogic, SurfaceWeb, SurfaceTUI, SurfaceDesktop:
		return true
	}
	return false
}

// steeringOpsField is JavaScript's property read on a value the JSON parser produced:
// an object answers its own key, an array and every primitive answer undefined. The
// oracle's op test is typeof-only, so an array op reaches the kind read and answers the
// kind reason rather than "must be an object"; a map assertion here would say the wrong
// thing for the same input.
func steeringOpsField(raw any, key string) (any, bool) {
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	value, present := object[key]
	return value, present
}

// steeringOpsIsObject is typeof raw === "object" && raw !== null: an ARRAY is an object
// there, and the batch check is the one place the oracle also tests Array.isArray.
func steeringOpsIsObject(raw any) bool {
	if raw == nil {
		return false
	}
	switch raw.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

// steeringOpsString is the oracle's typeof value === "string" test.
func steeringOpsString(value any) (string, bool) {
	s, ok := value.(string)
	return s, ok
}

// steeringOpsTrimmedText is the oracle's typeof value === "string" ? value.trim() : ""
// (:123): a non-string expectedEvidence becomes the empty string, not a refusal.
func steeringOpsTrimmedText(value any) string {
	if s, ok := steeringOpsString(value); ok {
		return jstext.Trim(s)
	}
	return ""
}

// steeringOpsStringList is the oracle's array mapping for dependsOn (:134-144): a
// non-array is refused, a non-string entry becomes "", and a "" entry is refused.
func steeringOpsStringList(value any) ([]string, bool) {
	raw, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		if s, ok := steeringOpsString(entry); ok {
			out = append(out, jstext.Trim(s))
			continue
		}
		out = append(out, "")
	}
	return out, true
}

// steeringOpsValidateBatch ports validateBatch (:74-153): the oracle's order of checks
// and every reason text. The returned reason is "" when the batch is valid.
func steeringOpsValidateBatch(batch any) (SteerBatch, string) {
	if !steeringOpsIsObject(batch) {
		return SteerBatch{}, "batch must be a JSON object"
	}
	if _, isArray := batch.([]any); isArray {
		return SteerBatch{}, "batch must be a JSON object"
	}
	fields := map[string]string{}
	for _, key := range []string{"idempotencyKey", "rationale", "evidence"} {
		value, _ := steeringOpsField(batch, key)
		text, ok := steeringOpsString(value)
		if !ok || jstext.Trim(text) == "" {
			return SteerBatch{}, key + " is required and must be a non-empty string"
		}
		fields[key] = text
	}
	rawOps, present := steeringOpsField(batch, "ops")
	opList, isArray := rawOps.([]any)
	if !present || !isArray || len(opList) == 0 {
		return SteerBatch{}, "ops must be a non-empty array — a batch with nothing to do has nothing to record"
	}
	ops := make([]SteerOp, 0, len(opList))
	for i, raw := range opList {
		at := "ops[" + strconv.Itoa(i) + "]"
		if !steeringOpsIsObject(raw) {
			return SteerBatch{}, at + " must be an object"
		}
		kindValue, _ := steeringOpsField(raw, "kind")
		kind, ok := steeringOpsString(kindValue)
		if !ok {
			return SteerBatch{}, at + ".kind must be a string"
		}
		if !steeringOpsSupportedOps(kind) {
			return SteerBatch{}, at + ".kind " + steeringOpsQuoted(kind) + " is not supported - use " + steeringOpsSupportedSet
		}
		switch SteerOpKind(kind) {
		case SteerOpAnnotate:
			noteValue, _ := steeringOpsField(raw, "note")
			note, ok := steeringOpsString(noteValue)
			if !ok || jstext.Trim(note) == "" {
				return SteerBatch{}, at + " is an annotate without a note"
			}
			// The note is kept untrimmed: only its emptiness is judged.
			ops = append(ops, SteerOp{Kind: SteerOpAnnotate, Note: note})
		case SteerOpAddCriterion:
			scenarioValue, _ := steeringOpsField(raw, "scenario")
			scenario, ok := steeringOpsString(scenarioValue)
			if !ok || jstext.Trim(scenario) == "" {
				return SteerBatch{}, at + " is an add-criterion without a scenario"
			}
			surface := SurfaceLogic
			if surfaceValue, present := steeringOpsField(raw, "surface"); present {
				surfaceText, ok := steeringOpsString(surfaceValue)
				if !ok || !steeringOpsSupportedSurfaces(surfaceText) {
					return SteerBatch{}, at + ".surface must be " + steeringOpsSurfaceList
				}
				surface = CriterionSurface(surfaceText)
			}
			presented := PresentedSurface("")
			if presentedValue, present := steeringOpsField(raw, "presented"); present {
				presentedText, ok := steeringOpsString(presentedValue)
				if !ok || presentedText != string(PresentedNative) {
					return SteerBatch{}, at + ".presented must be " + steeringOpsNativeList
				}
				if surface != SurfaceDesktop {
					return SteerBatch{}, at + ".presented " + steeringOpsNativeList + " requires surface " + steeringOpsDesktopList
				}
				presented = PresentedNative
			}
			expectedValue, _ := steeringOpsField(raw, "expectedEvidence")
			ops = append(ops, SteerOp{
				Kind: SteerOpAddCriterion, Scenario: jstext.Trim(scenario), Surface: surface,
				Presented: presented, ExpectedEvidence: steeringOpsTrimmedText(expectedValue),
			})
		default: // add-work-phase
			idValue, _ := steeringOpsField(raw, "id")
			id, ok := steeringOpsString(idValue)
			if !ok || !isLifecycleID(id) {
				return SteerBatch{}, at + ".id must be a short lowercase work-phase id, e.g. " + steeringOpsExampleWorkPhase
			}
			titleValue, _ := steeringOpsField(raw, "title")
			title, ok := steeringOpsString(titleValue)
			if !ok || jstext.Trim(title) == "" {
				return SteerBatch{}, at + " is an add-work-phase without a title"
			}
			// The oracle's dependsOn ?? [] falls back on null as well as on undefined, so a
			// present null is the empty list rather than a refusal.
			rawDependsOn, present := steeringOpsField(raw, "dependsOn")
			if !present || rawDependsOn == nil {
				rawDependsOn = []any{}
			}
			dependsOn, ok := steeringOpsStringList(rawDependsOn)
			if !ok {
				return SteerBatch{}, at + ".dependsOn must be an array of non-empty work-phase ids"
			}
			for _, dependencyID := range dependsOn {
				if dependencyID == "" {
					return SteerBatch{}, at + ".dependsOn must be an array of non-empty work-phase ids"
				}
			}
			if len(uniqueDependencyIDs(dependsOn)) != len(dependsOn) {
				return SteerBatch{}, at + ".dependsOn must not contain duplicate ids"
			}
			ops = append(ops, SteerOp{Kind: SteerOpAddWorkPhase, ID: id, Title: jstext.Trim(title), DependsOn: dependsOn})
		}
	}
	return SteerBatch{
		IdempotencyKey: fields["idempotencyKey"], Rationale: fields["rationale"],
		Evidence: fields["evidence"], Ops: ops,
	}, ""
}

// steeringOpsIntegrityReasons ports integrityReasons (:182-187): both mutating branches
// run the SAME two checks in the SAME order, so the two surfaces cannot drift apart.
func steeringOpsIntegrityReasons(candidate *Goalplan) []string {
	reasons := GoalplanDefinitionIntegrityReasons(candidate)
	return append(reasons, GoalplanDependencyCompletionReasons(candidate)...)
}

// steeringOpsJSNumberText is JavaScript's Number::toString for radix 10: the shortest
// round-trip decimal, fixed notation for 1e-6 <= |n| < 1e21 and exponential outside.
// The oracle mints a criterion id by a template literal (:202), so this is the spelling
// that lands in the plan; Go's %v would print 1e+06 where JS prints 1000000.
func steeringOpsJSNumberText(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	negative := math.Signbit(f)
	if negative {
		f = -f
	}
	// The shortest round-trip mantissa and the exponent of its leading digit.
	mantissa := strconv.FormatFloat(f, 'e', -1, 64)
	at := strings.IndexByte(mantissa, 'e')
	exponent, _ := strconv.Atoi(mantissa[at+1:])
	digits := strings.Replace(mantissa[:at], ".", "", 1)
	digits = strings.TrimRight(digits, "0")
	if digits == "" {
		digits = "0"
	}
	k := len(digits)
	n := exponent + 1 // the value is 0.digits * 10^n
	var out string
	switch {
	case k <= n && n <= 21:
		out = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		out = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		out = "0." + strings.Repeat("0", -n) + digits
	default:
		out = digits[:1]
		if k > 1 {
			out += "." + digits[1:]
		}
		ex := n - 1
		sign := "+"
		if ex < 0 {
			sign = "-"
			ex = -ex
		}
		out += "e" + sign + strconv.Itoa(ex)
	}
	if negative {
		return "-" + out
	}
	return out
}

// steeringOpsNextCriterionID is the oracle's dense, monotonic id mint (:199-204): the
// highest existing c-N plus one, never criteria.length, so a hand-edited gap cannot
// collide. The digit run is read through JavaScript's Number and a value that is not
// finite contributes 0 (Number.isFinite). Above 2^53 the float64 increment is lost, so
// the minted id can repeat or fall below the highest stored one - a defect of the
// oracle, ported as-is (docs/port-cxc/known-defects.md).
func steeringOpsNextCriterionID(criteria []GoalplanCriterion) string {
	maxID := 0.0
	for i := range criteria {
		n, ok := steeringOpsCriterionNumber(criteria[i].ID)
		if !ok || math.IsInf(n, 0) || math.IsNaN(n) {
			continue
		}
		if n > maxID {
			maxID = n
		}
	}
	return "c-" + steeringOpsJSNumberText(maxID+1)
}

// steeringOpsCriterionNumber is Number(/^c-(d+)$/.exec(id)?.[1] ?? 0): the digit run of
// a c-N id as a JavaScript number, or false when the id does not match. ToNumber of a
// long digit run rounds to the nearest double and becomes Infinity past the range, so
// ParseFloat is the right reader and an exact-integer one is not.
func steeringOpsCriterionNumber(id string) (float64, bool) {
	if !strings.HasPrefix(id, "c-") {
		return 0, false
	}
	digits := id[2:]
	if digits == "" {
		return 0, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	f, err := strconv.ParseFloat(digits, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	return f, true
}

// steeringOpsApplyOps ports applyOps (:189-239): the ops folded into a plan, with the
// caller owning the lock and the write. It is pure - the input plan is never mutated and
// a changed plan is a fresh copy - because no caller exists until B15b.
func steeringOpsApplyOps(plan *Goalplan, ops []SteerOp) (*Goalplan, string) {
	criteria := plan.Criteria
	workPhases := plan.WorkPhases
	for _, op := range ops {
		switch op.Kind {
		case SteerOpAnnotate:
			continue // ledger-only, by design
		case SteerOpAddCriterion:
			scenario := op.Scenario
			for i := range criteria {
				if criteria[i].Scenario == scenario {
					return nil, "a criterion with scenario " + steeringOpsQuoted(scenario) + " is already registered"
				}
			}
			surface := op.Surface
			if surface == "" {
				surface = SurfaceLogic
			}
			presented := PresentedSurface("")
			if op.Presented == PresentedNative {
				presented = PresentedNative
			}
			candidate := append(append([]GoalplanCriterion{}, criteria...), GoalplanCriterion{
				ID:               steeringOpsNextCriterionID(criteria),
				Scenario:         scenario,
				Surface:          surface,
				Presented:        presented,
				ExpectedEvidence: op.ExpectedEvidence,
				CapturedEvidence: nil,
				Status:           CriterionOpen,
			})
			reasons := steeringOpsIntegrityReasons(steeringOpsWithLists(plan, candidate, workPhases))
			if len(reasons) > 0 {
				return nil, strings.Join(reasons, "; ")
			}
			criteria = candidate
		default: // add-work-phase
			duplicate := false
			for i := range workPhases {
				if workPhases[i].ID == op.ID {
					duplicate = true
					break
				}
			}
			if duplicate {
				return nil, "work phase '" + op.ID + "' is already in this plan"
			}
			phase := GoalplanWorkPhase{
				ID: op.ID, Title: op.Title, Status: WorkPhasePending,
				Tasks: []GoalplanTask{}, CriteriaIDs: []string{},
			}
			if len(op.DependsOn) > 0 {
				phase.DependsOn = append([]string{}, op.DependsOn...)
			}
			candidate := append(append([]GoalplanWorkPhase{}, workPhases...), phase)
			reasons := steeringOpsIntegrityReasons(steeringOpsWithLists(plan, criteria, candidate))
			if len(reasons) > 0 {
				return nil, strings.Join(reasons, "; ")
			}
			workPhases = candidate
		}
	}
	next := *plan
	next.Criteria = criteria
	next.WorkPhases = workPhases
	return &next, ""
}

// steeringOpsWithLists is {...plan, criteria, workPhases}: the candidate the integrity
// checks read, with the two lists the caller is folding.
func steeringOpsWithLists(plan *Goalplan, criteria []GoalplanCriterion, workPhases []GoalplanWorkPhase) *Goalplan {
	candidate := *plan
	candidate.Criteria = criteria
	candidate.WorkPhases = workPhases
	return &candidate
}
