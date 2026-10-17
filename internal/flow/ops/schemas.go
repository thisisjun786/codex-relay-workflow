package ops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
)

// This file is the Go side of crw:lib/schemas (internal/flow/workflows/lib/schemas.js): a validation copy of every schema a flow
// role answers with, and the decoder of each. A flow script hands a schema to the model as a strict outputSchema, the engine
// validates the answer against the same text, and the ops that consume an answer (the verdict of a review, the DESIGN of an
// architect, the audit of a DESIGN) decode it here. The JSON Schema text cannot say everything a verdict means (a defect of kind
// evidence_missing carries its evidence_question), so a decoder checks those rules too, and an answer that breaks one is not a
// verdict. TestSchemasProducerConsumerPaired pins the copies to the library, the producers to the workflows and the consumers to
// the decoders.

// The schema names, as the library exports them.
const (
	SchemaReceipt      = "RECEIPT"
	SchemaVerdict      = "VERDICT"
	SchemaVerdictBatch = "VERDICT_BATCH"
	SchemaDesign       = "DESIGN"
	SchemaDesignAudit  = "DESIGN_AUDIT"
	SchemaReflection   = "REFLECTION"
	SchemaFindings     = "FINDINGS"
	SchemaFindingsEnd  = "FINDINGS_END"
	SchemaQACapture    = "QA_CAPTURE"
	SchemaQAPass       = "QA_PASS"
	SchemaQAReceipt    = "QA_RECEIPT"
)

const (
	defectKinds = `["correctness", "criteria_unmet", "test_gap", "evidence_missing", "scope", "maintainability", "other"]`
	severities  = `["P0", "P1", "P2", "P3"]`
	// impacts is premerge.Impacts as a JSON enum (TestImpactIsAClosedSet pins the two equal).
	impacts = `["core_function", "safety_or_data", "permission_boundary", "criterion_unmet", "regression_introduced", "minor_separable"]`

	criterionJSON = `{"type":"object","properties":{"id":{"type":"string"},"met":{"type":"boolean"},"evidence":{"type":"string"}},"required":["id","met","evidence"],"additionalProperties":false}`
	defectJSON    = `{"type":"object","properties":{"ref":{"type":"string"},"kind":{"type":"string","enum":` + defectKinds + `},"severity":{"type":"string","enum":` + severities + `},` +
		`"introduced":{"type":"boolean"},"impact":{"type":"string","enum":` + impacts + `},"inPromise":{"type":"boolean"},"file":{"type":"string"},"line":{"type":"integer"},"finding":{"type":"string"},` +
		`"evidence_question":{"anyOf":[{"type":"object","properties":{"question":{"type":"string"},"files_needed":{"type":"array","items":{"type":"string"}}},"required":["question","files_needed"],"additionalProperties":false},{"type":"null"}]}},` +
		`"required":["ref","kind","severity","introduced","impact","inPromise","file","line","finding","evidence_question"],"additionalProperties":false}`
	verdictFieldsJSON = `"verdict":{"type":"string","enum":["verified","needs_changes"]},"criteria":{"type":"array","items":` + criterionJSON + `},"defects":{"type":"array","items":` + defectJSON + `},"summary":{"type":"string"}`
	textsJSON         = `{"type":"array","items":{"type":"string"}}`
	maybeTextJSON     = `{"type":["string","null"]}`

	qaVerdicts  = `["PASS", "REVISE", "FAIL"]`
	qaFindingJS = `{"type":"object","properties":{"ref":{"type":"string"},"severity":{"type":"string","enum":` + severities + `},"finding":{"type":"string"},"evidence":` + textsJSON + `},"required":["ref","severity","finding","evidence"],"additionalProperties":false}`
)

// schemaTexts are the validation copies, by the name the library exports.
var schemaTexts = map[string]string{
	SchemaReceipt: `{"type":"object","properties":{"outcome":{"type":"string","enum":["ready_for_review","needs_research","blocked_scope","failed","budget_exhausted"]},` +
		`"files":` + textsJSON + `,"verification":{"type":"array","items":{"type":"object","properties":{"cmd":{"type":"string"},"exit":{"type":"integer"},"summary":{"type":"string"}},"required":["cmd","exit","summary"],"additionalProperties":false}},` +
		`"summary":{"type":"string"},"gap":` + maybeTextJSON + `,"research_question":` + maybeTextJSON + `,"plan_mismatch":` + maybeTextJSON + `},` +
		`"required":["outcome","files","verification","summary","gap","research_question","plan_mismatch"],"additionalProperties":false}`,
	SchemaVerdict: `{"type":"object","properties":{` + verdictFieldsJSON + `},"required":["verdict","criteria","defects","summary"],"additionalProperties":false}`,
	SchemaVerdictBatch: `{"type":"object","properties":{"nodes":{"type":"array","items":{"type":"object","properties":{"node":{"type":"string"},"head":{"type":"string"},"criteria_digest":{"type":"string"},` +
		verdictFieldsJSON + `},"required":["node","head","criteria_digest","verdict","criteria","defects","summary"],"additionalProperties":false}}},"required":["nodes"],"additionalProperties":false}`,
	SchemaDesign: `{"type":"object","properties":{` +
		`"decisions":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"evidence":` + textsJSON + `,"change":{"type":"string"},"alternatives":` + textsJSON + `,"assumptions":` + textsJSON + `},"required":["id","evidence","change","alternatives","assumptions"],"additionalProperties":false}},` +
		`"issues":{"type":"array","items":{"type":"object","properties":{"issue":{"type":"string"},"node":{"type":"string"},"size_fit":{"type":"string","enum":["executor","split","bundle_split"]},"size_reason":{"type":"string"},` +
		`"issue_size_decision":{"type":"string","enum":["ok","over_line","over_line_accepted"]},"edit_regions":` + textsJSON + `,"approach":{"type":"string"},"tests_first":` + textsJSON + `,"done_condition":{"type":"string"},"decision_refs":` + textsJSON + `,"order_after":` + textsJSON + `,"group":` + maybeTextJSON + `,` +
		`"sub_tasks":{"type":["array","null"],"maxItems":4,"items":{"type":"object","properties":{"id":{"type":"string","pattern":"` + subTaskIDForm + `"},"summary":{"type":"string"},"edit_regions":{"type":"array","items":{"type":"string"},"minItems":1},"done_condition":{"type":"string"},` +
		`"after":{"type":["array","null"],"items":{"type":"string"}}},"required":["id","summary","edit_regions","done_condition","after"],"additionalProperties":false}}},` +
		`"required":["issue","node","size_fit","size_reason","issue_size_decision","edit_regions","approach","tests_first","done_condition","decision_refs","order_after","group","sub_tasks"],"additionalProperties":false}},` +
		`"open_questions":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"question":{"type":"string"},"scope":{"type":"string"},"files_needed":` + textsJSON + `},"required":["id","question","scope","files_needed"],"additionalProperties":false}},` +
		`"risks":` + textsJSON + `,` +
		`"excluded_scope":{"type":["array","null"],"items":{"type":"object","properties":{"scope":{"type":"string"},"reason":{"type":"string"},"alternative":{"type":["string","null"]}},"required":["scope","reason","alternative"],"additionalProperties":false}},` +
		`"dispositions":{"type":["array","null"],"items":{"type":"object","properties":{"finding":{"type":"string"},"disposition":{"type":"string","enum":["folded","rebutted"]},"reason":{"type":"string"}},` +
		`"required":["finding","disposition","reason"],"additionalProperties":false}}},"required":["decisions","issues","open_questions","risks","excluded_scope","dispositions"],"additionalProperties":false}`,
	SchemaDesignAudit: `{"type":"object","properties":{"root":{"type":["string","null"]},"first_check":{"type":["string","null"]},"verdict":{"type":"string","enum":["approved","needs_changes"]},"findings":{"type":"array","items":{"type":"object","properties":{"ref":{"type":"string"},"severity":{"type":"string","enum":` + severities + `},"finding":{"type":"string"},"evidence":` + textsJSON + `},"required":["ref","severity","finding","evidence"],"additionalProperties":false}},"summary":{"type":"string"}},"required":["root","first_check","verdict","findings","summary"],"additionalProperties":false}`,
	SchemaReflection:  `{"type":"object","properties":{"verdict":{"type":"string","enum":["ALIGNED","MISALIGNED"]},"misaligned":{"type":"array","items":{"type":"object","properties":{"decision_id":{"type":"string"},"observed":{"type":"string"},"affected_nodes":` + textsJSON + `,"evidence":` + textsJSON + `},"required":["decision_id","observed","affected_nodes","evidence"],"additionalProperties":false}}},"required":["verdict","misaligned"],"additionalProperties":false}`,
	SchemaQACapture: `{"type":"object","properties":{"captures":{"type":"array","items":{"type":"object","properties":{"path":{"type":"string"},"kind":{"type":"string","enum":["screenshot","terminal_capture","response","transcript"]},"shows":{"type":"string"}},"required":["path","kind","shows"],"additionalProperties":false}},` +
		`"matrix":` + textsJSON + `,"teardown":` + textsJSON + `,"summary":{"type":"string"}},"required":["captures","matrix","teardown","summary"],"additionalProperties":false}`,
	SchemaQAPass:    `{"type":"object","properties":{"pass":{"type":"string","enum":["A","B"]},"verdict":{"type":"string","enum":` + qaVerdicts + `},"findings":{"type":"array","items":` + qaFindingJS + `},"summary":{"type":"string"}},"required":["pass","verdict","findings","summary"],"additionalProperties":false}`,
	SchemaQAReceipt: `{"type":"object","properties":{"verdict":{"type":"string","enum":` + qaVerdicts + `},"findings":{"type":"array","items":` + qaFindingJS + `},"summary":{"type":"string"}},"required":["verdict","findings","summary"],"additionalProperties":false}`,
	SchemaFindings:  `{"type":"object","properties":{"answer":{"type":"string"},"evidence":` + textsJSON + `,"verified":` + textsJSON + `,"inferred":` + textsJSON + `,"unknown":` + textsJSON + `},"required":["answer","evidence","verified","inferred","unknown"],"additionalProperties":false}`,
	SchemaFindingsEnd: `{"type":"object","properties":{"answer":{"type":"string"},"evidence":` + textsJSON + `,"verified":` + textsJSON + `,"inferred":` + textsJSON + `,"unknown":` + textsJSON + `,` +
		`"end_conditions":{"type":"array","items":{"type":"object","properties":{"command":{"type":"string"},"exit_code":{"type":"integer"},"tests":{"type":"integer"},"summary":{"type":"string"}},"required":["command","exit_code","tests","summary"],"additionalProperties":false}}},` +
		`"required":["answer","evidence","verified","inferred","unknown","end_conditions"],"additionalProperties":false}`,
}

// SchemaNames lists the schemas this file holds a copy of.
func SchemaNames() []string {
	out := make([]string, 0, len(schemaTexts))
	for name := range schemaTexts {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// SchemaText is the validation copy of a schema, as JSON text.
func SchemaText(name string) (string, bool) {
	text, ok := schemaTexts[name]
	return text, ok
}

var (
	resolvedM sync.Mutex
	resolved  = map[string]*jsonschema.Resolved{}
)

func resolvedSchema(name string) (*jsonschema.Resolved, error) {
	resolvedM.Lock()
	defer resolvedM.Unlock()
	if r, ok := resolved[name]; ok {
		return r, nil
	}
	text, ok := schemaTexts[name]
	if !ok {
		return nil, fmt.Errorf("ops: no schema %q", name)
	}
	var sch jsonschema.Schema
	if err := json.Unmarshal([]byte(text), &sch); err != nil {
		return nil, fmt.Errorf("ops: schema %s: %w", name, err)
	}
	r, err := sch.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("ops: schema %s: %w", name, err)
	}
	resolved[name] = r
	return r, nil
}

// storedVerdictSchema reads the empty impact older reviews recorded without
// widening the output schema given to a new reviewer.
var storedVerdictSchema = sync.OnceValues(func() (*jsonschema.Resolved, error) {
	var sch jsonschema.Schema
	if err := json.Unmarshal([]byte(schemaTexts[SchemaVerdict]), &sch); err != nil {
		return nil, err
	}
	impact := sch.Properties["defects"].Items.Properties["impact"]
	impact.Enum = append(impact.Enum, "")
	return sch.Resolve(nil)
})

// Validate checks an answer against the schema of that name: one JSON document that satisfies it.
func Validate(name string, raw []byte) error {
	r, err := resolvedSchema(name)
	if err != nil {
		return err
	}
	return validateSchema(name, raw, r)
}

// validateSchema is Validate against an already resolved schema.
func validateSchema(name string, raw []byte, r *jsonschema.Resolved) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("%s: not JSON: %w", name, err)
	}
	if dec.More() {
		return fmt.Errorf("%s: more than one JSON value", name)
	}
	if err := r.Validate(v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func decode[T any](name string, raw []byte) (T, error) {
	var v T
	if err := Validate(name, raw); err != nil {
		return v, err
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("%s: %w", name, err)
	}
	return v, nil
}

// Criterion is one criterion of a verdict.
type Criterion struct {
	ID       string `json:"id"`
	Met      bool   `json:"met"`
	Evidence string `json:"evidence"`
}

// EvidenceQuestion is what a reviewer could not tell from the head: the question and the files it needs read.
type EvidenceQuestion struct {
	Question    string   `json:"question"`
	FilesNeeded []string `json:"files_needed"`
}

// Defect is one finding of a verdict.
type Defect struct {
	Ref              string            `json:"ref"`
	Kind             string            `json:"kind"`
	Severity         string            `json:"severity"`
	Introduced       bool              `json:"introduced"`
	Impact           string            `json:"impact"`
	InPromise        bool              `json:"inPromise"`
	File             string            `json:"file"`
	Line             int               `json:"line"`
	Finding          string            `json:"finding"`
	EvidenceQuestion *EvidenceQuestion `json:"evidence_question"`
}

// ReviewVerdict is the answer of a reviewer (VERDICT).
type ReviewVerdict struct {
	Verdict  string      `json:"verdict"`
	Criteria []Criterion `json:"criteria"`
	Defects  []Defect    `json:"defects"`
	Summary  string      `json:"summary"`
}

// checkDefects holds the rules of a defect the JSON Schema cannot say: a defect of kind evidence_missing carries its
// evidence_question (the question the explorer answers) and no other kind pretends to.
func checkDefects(what string, defects []Defect) error {
	for _, d := range defects {
		switch {
		case d.Kind == "evidence_missing" && (d.EvidenceQuestion == nil || d.EvidenceQuestion.Question == ""):
			return fmt.Errorf("%s: defect %q is evidence_missing and has no evidence_question", what, d.Ref)
		case d.Kind != "evidence_missing" && d.EvidenceQuestion != nil:
			return fmt.Errorf("%s: defect %q has an evidence_question and is of kind %s", what, d.Ref, d.Kind)
		}
	}
	return nil
}

// DecodeVerdict reads a VERDICT. Stored empty impacts remain empty and follow premerge.Blocks; new answers still use the strict
// producer schema. A verified verdict names no defect that blocks (premerge.Blocks: P0 to P2, or an impact other than
// minor_separable) and no criterion that is not met: those are the findings a premerge record needs a disposition for, and the
// default disposition is blocking (only the orchestrator separates or carries one), so a reviewer that says verified over one does
// not say what its own record says.
func DecodeVerdict(raw []byte) (ReviewVerdict, error) {
	var v ReviewVerdict
	sch, err := storedVerdictSchema()
	if err != nil {
		return v, fmt.Errorf("%s: %w", SchemaVerdict, err)
	}
	if err := validateSchema(SchemaVerdict, raw, sch); err != nil {
		return v, err
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("%s: %w", SchemaVerdict, err)
	}
	return v, v.check("VERDICT")
}

func (v ReviewVerdict) check(what string) error {
	if err := checkDefects(what, v.Defects); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, c := range v.Criteria {
		if seen[c.ID] {
			return fmt.Errorf("%s: criterion %q is named twice", what, c.ID)
		}
		seen[c.ID] = true
	}
	if v.Verdict == "verified" {
		for _, c := range v.Criteria {
			if !c.Met {
				return fmt.Errorf("%s: verified, and criterion %q is not met", what, c.ID)
			}
		}
		for _, d := range v.Defects {
			if premerge.Blocks(d.Severity, d.Impact) {
				return fmt.Errorf("%s: verified, and defect %q is %s with impact %q, which blocks", what, d.Ref, d.Severity, d.Impact)
			}
		}
	}
	return nil
}

// CriterionIDs are the ids of the n criteria of a node's plan, in the order the plan lists them: c1 to cn. The packets of the lane
// label the criteria with them and a verified verdict answers one entry for each.
func CriterionIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("c%d", i+1)
	}
	return ids
}

// CheckCoverage says whether a verdict evaluated exactly the criteria with the ids, each once: a criterion the reviewer left out
// was never judged, one it names twice has two answers, and one the plan does not have is not the plan's. An empty want checks
// nothing (a plan with no criteria has nothing to cover).
func CheckCoverage(have []Criterion, want []string) error {
	if len(want) == 0 {
		return nil
	}
	left := map[string]bool{}
	for _, id := range want {
		left[id] = true
	}
	for _, c := range have {
		if !left[c.ID] {
			if slices.Contains(want, c.ID) {
				return fmt.Errorf("criterion %q is named twice", c.ID)
			}
			return fmt.Errorf("criterion %q is not a criterion of the plan (%s)", c.ID, strings.Join(want, ", "))
		}
		delete(left, c.ID)
	}
	if len(left) > 0 {
		var missing []string
		for _, id := range want {
			if left[id] {
				missing = append(missing, id)
			}
		}
		return fmt.Errorf("the verdict leaves criteria unevaluated: %s", strings.Join(missing, ", "))
	}
	return nil
}

// BatchBlock is one member's block of a VERDICT_BATCH.
type BatchBlock struct {
	Node           string `json:"node"`
	Head           string `json:"head"`
	CriteriaDigest string `json:"criteria_digest"`
	ReviewVerdict
}

// VerdictBatch is the answer of one reviewer call over a candidate batch (SP-REV-2, CRW-1225).
type VerdictBatch struct {
	Nodes []BatchBlock `json:"nodes"`
}

// DecodeVerdictBatch reads a VERDICT_BATCH; every block follows the rules of a verdict.
func DecodeVerdictBatch(raw []byte) (VerdictBatch, error) {
	b, err := decode[VerdictBatch](SchemaVerdictBatch, raw)
	if err != nil {
		return b, err
	}
	for _, n := range b.Nodes {
		if err := n.ReviewVerdict.check("VERDICT_BATCH " + n.Node); err != nil {
			return b, err
		}
	}
	return b, nil
}

// Decision is one decision of a DESIGN.
type Decision struct {
	ID           string   `json:"id"`
	Evidence     []string `json:"evidence"`
	Change       string   `json:"change"`
	Alternatives []string `json:"alternatives"`
	Assumptions  []string `json:"assumptions"`
}

// DesignIssue is the plan of one issue in a DESIGN.
type DesignIssue struct {
	Issue             string   `json:"issue"`
	Node              string   `json:"node"`
	SizeFit           string   `json:"size_fit"`
	SizeReason        string   `json:"size_reason"`
	IssueSizeDecision string   `json:"issue_size_decision"`
	EditRegions       []string `json:"edit_regions"`
	Approach          string   `json:"approach"`
	TestsFirst        []string `json:"tests_first"`
	DoneCondition     string   `json:"done_condition"`
	DecisionRefs      []string `json:"decision_refs"`
	OrderAfter        []string `json:"order_after"`
	Group             *string  `json:"group"`
	// SubTasks are the parts of a split node (2 to 4), run in their order; null for an executor or a bundle_split node, and for every
	// node of a DESIGN recorded before S-1 (CRW-1435).
	SubTasks []SubTask `json:"sub_tasks"`
}

// SubTask is one part of a split node: its id (a sub-task key and a worktree path carry it), what it does, the regions it edits (each
// within the node's own), when it is done, and the ids of the earlier sub-tasks it starts after (null for none).
type SubTask struct {
	ID            string   `json:"id"`
	Summary       string   `json:"summary"`
	EditRegions   []string `json:"edit_regions"`
	DoneCondition string   `json:"done_condition"`
	After         []string `json:"after"`
}

// OpenQuestion is a question the architect could not answer from what it read.
type OpenQuestion struct {
	ID          string   `json:"id"`
	Question    string   `json:"question"`
	Scope       string   `json:"scope"`
	FilesNeeded []string `json:"files_needed"`
}

// Disposition is the architect's answer to one design-audit finding in a design-fix: the finding id (r<round>.<index>), folded
// into the DESIGN or rebutted, and why.
type Disposition struct {
	Finding     string `json:"finding"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
}

// ScopeExclusion records work outside the design's authorized scope and an allowed alternative, when one exists.
type ScopeExclusion struct {
	Scope       string  `json:"scope"`
	Reason      string  `json:"reason"`
	Alternative *string `json:"alternative"`
}

// Design is the answer of the architect (DESIGN). Dispositions is nil (null) in the first DESIGN and in a re-plan answer.
type Design struct {
	Decisions     []Decision     `json:"decisions"`
	Issues        []DesignIssue  `json:"issues"`
	OpenQuestions []OpenQuestion `json:"open_questions"`
	Risks         []string       `json:"risks"`
	// ExcludedScope is nil when no assessment was recorded; an empty list records no exclusions.
	ExcludedScope []ScopeExclusion `json:"excluded_scope"`
	Dispositions  []Disposition    `json:"dispositions"`
}

// DecodeDesign reads a DESIGN. Two issues of one DESIGN do not share a node, and an issue that is not an executor's names why
// (size_reason) in the closed set of the size rules the policy lists.
//
// It reads the DESIGNs the run recorded: the candidate of crw.designAudit, the DESIGN crw.designLedger records, and the ledger's DESIGN
// the release of a node (its plan and the admission regions of the other nodes), crw.issuePlan and the final gate's decisions read.
// A DESIGN an earlier binary recorded may lack dispositions (CRW-1452) or excluded_scope (CRW-1538): missing members are read as
// null, and the original stored bytes and their digests stay unchanged. A missing exclusion assessment is not an empty assessment.
// A fresh architect answer is held to the schema, which requires both members, by the engine before any operation reads it.
//
// The shape of the DESIGN, not the reader, says which rules hold (S-1, CRW-1435). A DESIGN in which an issue carries the sub_tasks
// member is of the S-1 shape: the schema refuses a node sized for a child (the `child` session path is retired), a split node names
// 2 to 4 sub-tasks (ids unique and of the sub-task id form, edit regions within the node's, `after` naming only earlier sub-tasks) and
// an executor or bundle_split node names none. A DESIGN in which no issue carries it was recorded before S-1: it is read with every
// sub_tasks null against legacyDesignSchema, which also takes child, and the sub-task rules do not apply (an old split has none).
// The schema holds what of the sub-task rules a strict output schema can express (at most 4, the id form, a region for each); the
// rest are design rules, answered as a *DesignRuleError.
func DecodeDesign(raw []byte) (Design, error) {
	raw = withLegacyDesignFields(raw)
	var d Design
	legacyRaw, legacy := withNullSubTasks(raw)
	if legacy {
		r, err := legacyDesignSchema()
		if err != nil {
			return d, err
		}
		var v any
		if err := json.Unmarshal(legacyRaw, &v); err != nil {
			return d, fmt.Errorf("%s: not JSON: %w", SchemaDesign, err)
		}
		if err := r.Validate(v); err != nil {
			return d, fmt.Errorf("%s: %w", SchemaDesign, err)
		}
		if err := json.Unmarshal(legacyRaw, &d); err != nil {
			return d, fmt.Errorf("%s: %w", SchemaDesign, err)
		}
	} else {
		var err error
		if d, err = decode[Design](SchemaDesign, raw); err != nil {
			return d, err
		}
	}
	seen := map[string]bool{}
	for _, is := range d.Issues {
		if seen[is.Node] {
			return d, fmt.Errorf("DESIGN: node %q has two issues", is.Node)
		}
		seen[is.Node] = true
		if is.SizeFit != "executor" && is.SizeReason == "" {
			return d, fmt.Errorf("DESIGN: node %q is size_fit %s and gives no size_reason", is.Node, is.SizeFit)
		}
		if legacy {
			continue
		}
		if is.SizeFit != "split" {
			if len(is.SubTasks) > 0 {
				return d, designRule(is.Node, "DESIGN: node %q is size_fit %s and names sub-tasks (only a split node does)", is.Node, is.SizeFit)
			}
			continue
		}
		if n := len(is.SubTasks); n < 2 || n > 4 {
			return d, designRule(is.Node, "DESIGN: split node %q names %d sub-tasks, want 2 to 4", is.Node, n)
		}
		ids := map[string]bool{}
		for _, sub := range is.SubTasks {
			if !subTaskID.MatchString(sub.ID) {
				return d, designRule(is.Node, "DESIGN: split node %q has the sub-task id %q, want %s", is.Node, sub.ID, subTaskIDForm)
			}
			if ids[sub.ID] {
				return d, designRule(is.Node, "DESIGN: split node %q has two sub-tasks %q", is.Node, sub.ID)
			}
			if len(sub.EditRegions) == 0 {
				return d, designRule(is.Node, "DESIGN: sub-task %q of node %q names no edit region", sub.ID, is.Node)
			}
			for _, region := range sub.EditRegions {
				if !slices.ContainsFunc(is.EditRegions, func(own string) bool { return regionWithin(region, own) }) {
					return d, designRule(is.Node, "DESIGN: the region %q of sub-task %q is not within the edit regions of node %q", region, sub.ID, is.Node)
				}
			}
			for _, before := range sub.After {
				if !ids[before] {
					return d, designRule(is.Node, "DESIGN: sub-task %q of node %q starts after %q, which is not an earlier sub-task", sub.ID, is.Node, before)
				}
			}
			ids[sub.ID] = true
		}
	}
	return d, nil
}

// subTaskIDForm is the form of a sub-task id, in the DESIGN schema (pattern) and in DecodeDesign: it becomes part of a sub-task key and
// of a worktree path.
const subTaskIDForm = `^[a-z0-9][a-z0-9-]{0,31}$`

var subTaskID = regexp.MustCompile(subTaskIDForm)

// DesignRuleError is a rule of the DESIGN that its schema cannot express and the DESIGN breaks (CRW-1435): the count of a node's
// sub-tasks for its size_fit, unique sub-task ids, sub-task regions within the node's and an `after` that names only earlier sub-tasks
// (the schema, which a strict output schema keeps free of conditions, holds the most count, the id form and a region for each). Node
// names the issue's node. crw.designAudit takes it as a finding of the audit, which sends the DESIGN back to the architect.
type DesignRuleError struct {
	Node string
	Msg  string
}

func (e *DesignRuleError) Error() string { return e.Msg }

func designRule(node, format string, a ...any) error {
	return &DesignRuleError{Node: node, Msg: fmt.Sprintf(format, a...)}
}

// regionWithin says whether the region sub of a sub-task lies within the region node of its node: the same region (in the normal form
// RegionOf gives), a path in the node's tree or the tree itself, or a symbol of the node's file. The root tree holds the relative paths
// that stay inside it, not an absolute path or one that leaves it.
func regionWithin(sub, node string) bool {
	s, n := RegionOf(sub), RegionOf(node)
	switch {
	case s == n:
		return true
	case n.Kind == "tree" && n.Path == ".":
		return !strings.HasPrefix(s.Path, "/") && s.Path != ".." && !strings.HasPrefix(s.Path, "../")
	case n.Kind == "tree":
		return s.Path == n.Path || strings.HasPrefix(s.Path, n.Path+"/")
	case n.Kind == "file":
		return s.Kind == "symbol" && s.Path == n.Path
	}
	return false
}

// withNullSubTasks says whether a DESIGN was recorded before S-1 (legacy: it holds issues and none of them carries the sub_tasks
// member, or it holds none), and gives it with every issue's sub_tasks null. A DESIGN that is not a JSON object with an issues list,
// or in which an issue carries the member, is not legacy and is returned as it is, so the schema judges it.
func withNullSubTasks(raw []byte) ([]byte, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return raw, false
	}
	var issues []map[string]json.RawMessage
	if err := json.Unmarshal(m["issues"], &issues); err != nil {
		return raw, false
	}
	for _, is := range issues {
		if _, ok := is["sub_tasks"]; ok {
			return raw, false
		}
	}
	for _, is := range issues {
		if is != nil {
			is["sub_tasks"] = json.RawMessage("null")
		}
	}
	b, err := json.Marshal(issues)
	if err != nil {
		return raw, false
	}
	m["issues"] = b
	out, err := json.Marshal(m)
	if err != nil {
		return raw, false
	}
	return out, true
}

var (
	legacyDesignOnce sync.Once
	legacyDesign     *jsonschema.Resolved
	legacyDesignErr  error
)

// legacyDesignSchema is the schema a DESIGN recorded before S-1 is read against: the DESIGN schema with child back in the size_fit
// enum. It is derived once from schemaTexts[SchemaDesign] and is no schema of the library: no role answers with it.
func legacyDesignSchema() (*jsonschema.Resolved, error) {
	legacyDesignOnce.Do(func() {
		var sch jsonschema.Schema
		if err := json.Unmarshal([]byte(schemaTexts[SchemaDesign]), &sch); err != nil {
			legacyDesignErr = fmt.Errorf("ops: the legacy DESIGN schema: %w", err)
			return
		}
		fit := sch.Properties["issues"].Items.Properties["size_fit"]
		fit.Enum = append(slices.Clone(fit.Enum), "child")
		legacyDesign, legacyDesignErr = sch.Resolve(nil)
		if legacyDesignErr != nil {
			legacyDesignErr = fmt.Errorf("ops: the legacy DESIGN schema: %w", legacyDesignErr)
		}
	})
	return legacyDesign, legacyDesignErr
}

// withLegacyDesignFields reads absent members added after the original DESIGN as null.
// Present values, including malformed ones, are left for the schema to judge.
func withLegacyDesignFields(raw []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return raw
	}
	changed := false
	for _, key := range []string{"dispositions", "excluded_scope"} {
		if _, ok := m[key]; !ok {
			m[key] = json.RawMessage("null")
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// AuditFinding is one finding of a design audit.
type AuditFinding struct {
	Ref      string   `json:"ref"`
	Severity string   `json:"severity"`
	Finding  string   `json:"finding"`
	Evidence []string `json:"evidence"`
}

// DesignAudit is the answer of the auditor of a DESIGN (DESIGN_AUDIT).
type DesignAudit struct {
	Root       *string        `json:"root"`
	FirstCheck *string        `json:"first_check"`
	Verdict    string         `json:"verdict"`
	Findings   []AuditFinding `json:"findings"`
	Summary    string         `json:"summary"`
}

// DecodeDesignAudit reads a DESIGN_AUDIT. An approval over a P0 or P1 finding is not an approval, and needs_changes names at
// least one finding the architect can act on. Missing optional fields in an older stored answer are read as null without
// changing its bytes.
func DecodeDesignAudit(raw []byte) (DesignAudit, error) {
	raw = withLegacyAuditFields(raw)
	a, err := decode[DesignAudit](SchemaDesignAudit, raw)
	if err != nil {
		return a, err
	}
	switch a.Verdict {
	case "approved":
		for _, f := range a.Findings {
			if f.Severity == "P0" || f.Severity == "P1" {
				return a, fmt.Errorf("DESIGN_AUDIT: approved, and finding %q is %s", f.Ref, f.Severity)
			}
		}
	case "needs_changes":
		if len(a.Findings) == 0 {
			return a, fmt.Errorf("DESIGN_AUDIT: needs_changes names no finding")
		}
	}
	return a, nil
}

// withLegacyAuditFields supplies optional fields absent in older stored audits.
// Present values are left for the schema to judge.
func withLegacyAuditFields(raw []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return raw
	}
	changed := false
	for _, key := range []string{"root", "first_check"} {
		if _, ok := m[key]; !ok {
			m[key] = json.RawMessage("null")
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// Misaligned is one decision a reflection found the milestone to depart from.
type Misaligned struct {
	DecisionID    string   `json:"decision_id"`
	Observed      string   `json:"observed"`
	AffectedNodes []string `json:"affected_nodes"`
	Evidence      []string `json:"evidence"`
}

// Reflection is the answer of the architect's milestone-close check (REFLECTION, F09).
type Reflection struct {
	Verdict    string       `json:"verdict"`
	Misaligned []Misaligned `json:"misaligned"`
}

// DecodeReflection reads a REFLECTION. MISALIGNED names the decisions it found departed from.
func DecodeReflection(raw []byte) (Reflection, error) {
	r, err := decode[Reflection](SchemaReflection, raw)
	if err != nil {
		return r, err
	}
	if r.Verdict == "MISALIGNED" && len(r.Misaligned) == 0 {
		return r, fmt.Errorf("REFLECTION: MISALIGNED names no decision")
	}
	return r, nil
}

// Findings is the answer of an explorer (FINDINGS).
type Findings struct {
	Answer   string   `json:"answer"`
	Evidence []string `json:"evidence"`
	Verified []string `json:"verified"`
	Inferred []string `json:"inferred"`
	Unknown  []string `json:"unknown"`
	// EndConditions are the end condition commands the explorer was given and ran once, each with its exit code and the tests it ran
	// (FINDINGS_END, CRW-1382); nil for the FINDINGS of an explorer given none.
	EndConditions []EndConditionRun `json:"end_conditions"`
}

// EndConditionRun is one end condition command the design-stage explorer ran: its exit code and how many tests it ran.
type EndConditionRun struct {
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
	Tests    int    `json:"tests"`
	Summary  string `json:"summary"`
}

// DecodeFindings reads FINDINGS, or FINDINGS_END when the answer carries end_conditions (the explorer given the end condition commands,
// CRW-1382). An answer of an explorer given none is the FINDINGS of the earlier binary, which a run recorded before CRW-1382 holds.
func DecodeFindings(raw []byte) (Findings, error) {
	var members map[string]json.RawMessage
	if json.Unmarshal(raw, &members) == nil {
		if _, ok := members["end_conditions"]; ok {
			return decode[Findings](SchemaFindingsEnd, raw)
		}
	}
	return decode[Findings](SchemaFindings, raw)
}

// Receipt is the report of an executor (RECEIPT).
type Receipt struct {
	Outcome      string   `json:"outcome"`
	Files        []string `json:"files"`
	Verification []struct {
		Cmd     string `json:"cmd"`
		Exit    int    `json:"exit"`
		Summary string `json:"summary"`
	} `json:"verification"`
	Summary          string  `json:"summary"`
	Gap              *string `json:"gap"`
	ResearchQuestion *string `json:"research_question"`
	PlanMismatch     *string `json:"plan_mismatch"`
}

// DecodeReceipt reads a RECEIPT. needs_research carries its research_question.
func DecodeReceipt(raw []byte) (Receipt, error) {
	r, err := decode[Receipt](SchemaReceipt, raw)
	if err != nil {
		return r, err
	}
	if r.Outcome == "needs_research" && (r.ResearchQuestion == nil || *r.ResearchQuestion == "") {
		return r, fmt.Errorf("RECEIPT: needs_research has no research_question")
	}
	return r, nil
}
