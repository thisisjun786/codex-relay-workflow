package ops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
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
	SchemaVerdictFinal = "VERDICT_FINAL"
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

	criterionJSON = `{"type":"object","properties":{"id":{"type":"string"},"met":{"type":"boolean"},"evidence":{"type":"string"}},"required":["id","met","evidence"],"additionalProperties":false}`
	defectJSON    = `{"type":"object","properties":{"ref":{"type":"string"},"kind":{"type":"string","enum":` + defectKinds + `},"severity":{"type":"string","enum":` + severities + `},` +
		`"introduced":{"type":"boolean"},"impact":{"type":"string"},"inPromise":{"type":"boolean"},"file":{"type":"string"},"line":{"type":"integer"},"finding":{"type":"string"},` +
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
	SchemaVerdictFinal: `{"type":"object","properties":{"verdict":{"type":"string","enum":["verified","needs_changes"]},"criteria":{"type":"array","items":` + criterionJSON + `},"defects":{"type":"array","items":` + defectJSON + `},` +
		`"alignment":{"type":"object","properties":{"verdict":{"type":"string","enum":["ALIGNED","MISALIGNED","NOT_ASSESSED"]},"drift":{"type":"array","items":{"type":"object","properties":{"decision_id":{"type":"string"},"observed":{"type":"string"},"file":{"type":"string"},"line":{"type":"integer"},"affected_nodes":` + textsJSON + `},"required":["decision_id","observed","file","line","affected_nodes"],"additionalProperties":false}}},"required":["verdict","drift"],"additionalProperties":false},` +
		`"summary":{"type":"string"}},"required":["verdict","criteria","defects","alignment","summary"],"additionalProperties":false}`,
	SchemaDesign: `{"type":"object","properties":{` +
		`"decisions":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"evidence":` + textsJSON + `,"change":{"type":"string"},"alternatives":` + textsJSON + `,"assumptions":` + textsJSON + `},"required":["id","evidence","change","alternatives","assumptions"],"additionalProperties":false}},` +
		`"issues":{"type":"array","items":{"type":"object","properties":{"issue":{"type":"string"},"node":{"type":"string"},"size_fit":{"type":"string","enum":["executor","child","split"]},"size_reason":{"type":"string"},` +
		`"issue_size_decision":{"type":"string","enum":["ok","over_line","over_line_accepted"]},"edit_regions":` + textsJSON + `,"approach":{"type":"string"},"tests_first":` + textsJSON + `,"done_condition":{"type":"string"},"decision_refs":` + textsJSON + `,"order_after":` + textsJSON + `,"group":` + maybeTextJSON + `},` +
		`"required":["issue","node","size_fit","size_reason","issue_size_decision","edit_regions","approach","tests_first","done_condition","decision_refs","order_after","group"],"additionalProperties":false}},` +
		`"open_questions":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"question":{"type":"string"},"scope":{"type":"string"},"files_needed":` + textsJSON + `},"required":["id","question","scope","files_needed"],"additionalProperties":false}},` +
		`"risks":` + textsJSON + `},"required":["decisions","issues","open_questions","risks"],"additionalProperties":false}`,
	SchemaDesignAudit: `{"type":"object","properties":{"verdict":{"type":"string","enum":["approved","needs_changes"]},"findings":{"type":"array","items":{"type":"object","properties":{"ref":{"type":"string"},"severity":{"type":"string","enum":` + severities + `},"finding":{"type":"string"},"evidence":` + textsJSON + `},"required":["ref","severity","finding","evidence"],"additionalProperties":false}},"summary":{"type":"string"}},"required":["verdict","findings","summary"],"additionalProperties":false}`,
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

// Validate checks an answer against the schema of that name: one JSON document that satisfies it.
func Validate(name string, raw []byte) error {
	r, err := resolvedSchema(name)
	if err != nil {
		return err
	}
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

// DecodeVerdict reads a VERDICT. A verified verdict names no defect of P0 to P2 and no criterion that is not met: those are the
// findings a premerge record needs a disposition for, and the default disposition is blocking (only the orchestrator separates or
// carries one), so a reviewer that says verified over one does not say what its own record says.
func DecodeVerdict(raw []byte) (ReviewVerdict, error) {
	v, err := decode[ReviewVerdict](SchemaVerdict, raw)
	if err != nil {
		return v, err
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
			if d.Severity == "P0" || d.Severity == "P1" || d.Severity == "P2" {
				return fmt.Errorf("%s: verified, and defect %q is %s", what, d.Ref, d.Severity)
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

// Drift is one observed departure of the integrated range from a decision.
type Drift struct {
	DecisionID    string   `json:"decision_id"`
	Observed      string   `json:"observed"`
	File          string   `json:"file"`
	Line          int      `json:"line"`
	AffectedNodes []string `json:"affected_nodes"`
}

// Alignment is the milestone-level judgement of reviewer-final.
type Alignment struct {
	Verdict string  `json:"verdict"`
	Drift   []Drift `json:"drift"`
}

// VerdictFinal is the answer of reviewer-final (F13).
type VerdictFinal struct {
	Verdict   string      `json:"verdict"`
	Criteria  []Criterion `json:"criteria"`
	Defects   []Defect    `json:"defects"`
	Alignment Alignment   `json:"alignment"`
	Summary   string      `json:"summary"`
}

// DecodeVerdictFinal reads a VERDICT_FINAL. A MISALIGNED alignment names the drift it saw.
func DecodeVerdictFinal(raw []byte) (VerdictFinal, error) {
	v, err := decode[VerdictFinal](SchemaVerdictFinal, raw)
	if err != nil {
		return v, err
	}
	if err := checkDefects("VERDICT_FINAL", v.Defects); err != nil {
		return v, err
	}
	if v.Alignment.Verdict == "MISALIGNED" && len(v.Alignment.Drift) == 0 {
		return v, fmt.Errorf("VERDICT_FINAL: MISALIGNED names no drift")
	}
	return v, nil
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
}

// OpenQuestion is a question the architect could not answer from what it read.
type OpenQuestion struct {
	ID          string   `json:"id"`
	Question    string   `json:"question"`
	Scope       string   `json:"scope"`
	FilesNeeded []string `json:"files_needed"`
}

// Design is the answer of the architect (DESIGN).
type Design struct {
	Decisions     []Decision     `json:"decisions"`
	Issues        []DesignIssue  `json:"issues"`
	OpenQuestions []OpenQuestion `json:"open_questions"`
	Risks         []string       `json:"risks"`
}

// DecodeDesign reads a DESIGN. Two issues of one DESIGN do not share a node, and an issue that is not an executor's names why
// (size_reason) in the closed set of the size rules the policy lists.
func DecodeDesign(raw []byte) (Design, error) {
	d, err := decode[Design](SchemaDesign, raw)
	if err != nil {
		return d, err
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
	}
	return d, nil
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
	Verdict  string         `json:"verdict"`
	Findings []AuditFinding `json:"findings"`
	Summary  string         `json:"summary"`
}

// DecodeDesignAudit reads a DESIGN_AUDIT. An approval over a P0 or P1 finding is not an approval, and needs_changes names at
// least one finding the architect can act on.
func DecodeDesignAudit(raw []byte) (DesignAudit, error) {
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
