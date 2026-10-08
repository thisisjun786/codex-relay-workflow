// Package premerge is the pre-merge gate's record and its judgment (CRW-952). A premerge-record/1 holds one
// evaluation of an issue branch's head and the parent's dispositions of its findings. Judge decides PASS or
// BLOCK from the record alone, branch by branch as premerge_gate.py decides it; the head and criteria checks
// that need the relay's own state belong to the caller. The package is importable by internal/manage, so the
// schema, the validator and the judgment have one owner.
package premerge

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// RecordSchema is the schema tag of a pre-merge record.
const RecordSchema = "premerge-record/1"

// Grader names who graded the head: the model, its effort and the digest of the prompt it was given.
type Grader struct {
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	PromptDigest string `json:"prompt_digest"`
}

// Criterion is one criterion of the issue with its verdict (PASS, PARTIAL or FAIL) and the evidence for it.
type Criterion struct {
	Verdict  string `json:"verdict"`
	Evidence string `json:"evidence,omitempty"`
}

// Defect is one finding of the evaluation. Impact is empty when the record does not give one.
type Defect struct {
	ID         string `json:"id"`
	Severity   string `json:"severity"`
	Impact     string `json:"impact,omitempty"`
	Introduced bool   `json:"introduced,omitempty"`
	InPromise  bool   `json:"in_promise,omitempty"`
	What       string `json:"what,omitempty"`
}

// Item is the parent's disposition of one finding (a criterion id or a defect id).
type Item struct {
	Ref         string `json:"ref"`
	Class       string `json:"class"`
	Note        string `json:"note,omitempty"`
	FollowUp    string `json:"followUp,omitempty"`
	NewCodeOnly string `json:"newCodeOnly,omitempty"`
}

// Dispositions is the parent's decision on the record: who decided, the items, and, for a head that moved after
// the evaluated one, what the later commits change.
type Dispositions struct {
	By              string `json:"by"`
	Items           []Item `json:"items,omitempty"`
	AfterEvaluation string `json:"afterEvaluation,omitempty"`
}

// Record is premerge-record/1: one document with the evaluation and the dispositions. It has no pull request
// member; a record is judged on the head it names.
type Record struct {
	Schema         string               `json:"schema"`
	Issue          string               `json:"issue"`
	Node           string               `json:"node"`
	Head           string               `json:"head"`
	Dev            string               `json:"dev"`
	CriteriaDigest string               `json:"criteria_digest"`
	Grader         Grader               `json:"grader"`
	GradedAt       string               `json:"gradedAt"`
	Criteria       map[string]Criterion `json:"criteria"`
	Defects        []Defect             `json:"defects"`
	Score          *float64             `json:"score"`
	Summary        string               `json:"summary"`
	Dispositions   Dispositions         `json:"dispositions"`
}

// Options are the facts about the head that the record cannot tell by itself. AfterEvaluation is set when the head
// is a descendant of the evaluated head through commits that are not dev-only merges: the later commits then need
// the parent's afterEvaluation statement.
type Options struct {
	AfterEvaluation bool
}

// Refusal is a judgment that does not pass. Reason is the contract name the refusal carries.
type Refusal struct {
	Reason contract.RefusalReason
	Detail string
}

func (e *Refusal) Error() string { return fmt.Sprintf("%s: %s", e.Reason, e.Detail) }

func refuse(reason contract.RefusalReason, format string, args ...any) error {
	return &Refusal{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// the members a record must carry, as the document writes them
var requiredMembers = []string{"schema", "issue", "node", "head", "dev", "criteria_digest", "grader", "gradedAt", "criteria", "defects", "score", "summary", "dispositions"}

var (
	fullSHA        = regexp.MustCompile(`^[0-9a-f]{40}$`)
	criteriaDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
	followUpKey    = regexp.MustCompile(`^CRW-[0-9]+$`)
	// the words that name an edit region rather than a reason (the oracle's REGION_ONLY, case-insensitive)
	regionWords = regexp.MustCompile(`(?i)(edit region|declared region|편집 영역|선언 영역|out of scope|outside (this|the) issue)`)
)

// Canonical is the canonical serialization of a record's bytes: keys sorted, no whitespace, UTF-8, numbers kept as
// written. An input that is not UTF-8 or holds an unpaired surrogate escape has no canonical form.
func Canonical(raw []byte) ([]byte, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("the record is not UTF-8")
	}
	if !surrogatesPaired(raw) {
		return nil, errors.New("the record holds an unpaired surrogate escape")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("the record is more than one JSON value")
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// Digest is sha256:<hex> of the record's canonical serialization.
func Digest(raw []byte) (string, error) {
	canonical, err := Canonical(raw)
	if err != nil {
		return "", err
	}
	return "sha256:" + sha256Hex(canonical), nil
}

// surrogatesPaired reports whether every \uXXXX escape in raw that lies in the surrogate range forms a valid
// high-low pair. Escaped backslashes are skipped, so \\ud800 is text, not an escape.
func surrogatesPaired(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		if i+1 >= len(raw) {
			return false
		}
		if raw[i+1] != 'u' {
			i++
			continue
		}
		if i+6 > len(raw) {
			return false
		}
		cp, ok := hex4(raw[i+2 : i+6])
		if !ok {
			return false
		}
		i += 5
		switch {
		case cp >= 0xDC00 && cp <= 0xDFFF:
			return false
		case cp >= 0xD800 && cp <= 0xDBFF:
			if i+7 > len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, ok := hex4(raw[i+3 : i+7])
			if !ok || low < 0xDC00 || low > 0xDFFF {
				return false
			}
			i += 6
		}
	}
	return true
}

func hex4(b []byte) (int, bool) {
	v := 0
	for _, c := range b {
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'f':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		v = v*16 + int(d)
	}
	return v, true
}

func sha256Hex(b []byte) string {
	sum := sha256Sum(b)
	return hex.EncodeToString(sum[:])
}

// Decode reads a premerge-record/1 document. A document that is not one, or that lacks a member the schema
// requires, is refused as premerge_missing: it is not a record to judge.
func Decode(raw []byte) (Record, error) {
	var record Record
	if _, err := Canonical(raw); err != nil {
		return record, refuse(contract.RefusalPremergeMissing, "the record is not a canonical document: %v", err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return record, refuse(contract.RefusalPremergeMissing, "the record is not a JSON object")
	}
	for _, name := range requiredMembers {
		if _, ok := members[name]; !ok {
			return record, refuse(contract.RefusalPremergeMissing, "the record has no %s member", name)
		}
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return record, refuse(contract.RefusalPremergeMissing, "the record does not match premerge-record/1: %v", err)
	}
	if record.Schema != RecordSchema {
		return record, refuse(contract.RefusalPremergeMissing, "the record names schema %q; this gate reads %s", record.Schema, RecordSchema)
	}
	if !fullSHA.MatchString(record.Head) || !fullSHA.MatchString(record.Dev) {
		return record, refuse(contract.RefusalPremergeMissing, "head and dev are full commit ids")
	}
	if !criteriaDigest.MatchString(record.CriteriaDigest) {
		return record, refuse(contract.RefusalPremergeMissing, "criteria_digest is a sha256 hex digest")
	}
	if len(record.Criteria) == 0 {
		return record, refuse(contract.RefusalPremergeMissing, "the record has no criteria: an incomplete evaluation is not a pass")
	}
	if !isArray(members["defects"]) {
		return record, refuse(contract.RefusalPremergeMissing, "defects is not a list")
	}
	if record.Score == nil {
		return record, refuse(contract.RefusalPremergeMissing, "the record has no score")
	}
	if err := checkDefectMembers(members["defects"]); err != nil {
		return record, err
	}
	for _, d := range record.Defects {
		if d.ID == "" {
			return record, refuse(contract.RefusalPremergeMissing, "a defect has no id")
		}
	}
	return record, nil
}

func isArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '['
}

// checkDefectMembers refuses an impact that is written as an empty string. The judgment reads an absent impact
// and an empty one alike as "none", so an empty word is refused here rather than read as no impact.
func checkDefectMembers(raw json.RawMessage) error {
	var defects []struct {
		Impact *string `json:"impact"`
	}
	if err := json.Unmarshal(raw, &defects); err != nil {
		return refuse(contract.RefusalPremergeMissing, "defects does not match premerge-record/1: %v", err)
	}
	for _, d := range defects {
		if d.Impact != nil && *d.Impact == "" {
			return refuse(contract.RefusalPremergeMissing, "a defect names an empty impact; write the impact or leave it out")
		}
	}
	return nil
}

// dispositionFor is the parent's item for a ref. A later item for the same ref replaces an earlier one, as the
// oracle's dictionary does.
func dispositionFor(items []Item) map[string]Item {
	byRef := map[string]Item{}
	for _, it := range items {
		byRef[it.Ref] = it
	}
	return byRef
}

// need is a finding the parent has to dispose of: a criterion that is not PASS, or a defect of P0 to P2 or with an
// impact other than minor_separable.
type need struct {
	ref       string
	criterion *Criterion
	defect    *Defect
}

func findingsNeedingDisposition(r Record) []need {
	var out []need
	keys := make([]string, 0, len(r.Criteria))
	for k := range r.Criteria {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c := r.Criteria[k]
		if c.Verdict != "PASS" {
			out = append(out, need{ref: k, criterion: &c})
		}
	}
	for i := range r.Defects {
		d := r.Defects[i]
		if d.Severity == "P0" || d.Severity == "P1" || d.Severity == "P2" || (d.Impact != "" && d.Impact != "minor_separable") {
			out = append(out, need{ref: d.ID, defect: &d})
		}
	}
	return out
}

// Judge decides whether a decoded record passes. It returns nil for PASS, and a *Refusal naming the first branch
// of the oracle that blocks. The branches run in the oracle's order.
func Judge(r Record, opts Options) error {
	if opts.AfterEvaluation && utf8.RuneCountInString(r.Dispositions.AfterEvaluation) < 20 {
		return refuse(contract.RefusalPremergeAfterEvaluationMissing, "head %s moved after the evaluation of %s: the afterEvaluation statement of the dispositions names what the later commits change (20 characters or more)", r.Head, r.Dev)
	}
	items := dispositionFor(r.Dispositions.Items)
	needed := findingsNeedingDisposition(r)
	neededRefs := map[string]bool{}
	for _, n := range needed {
		neededRefs[n.ref] = true
	}
	refs := make([]string, 0, len(items))
	for ref := range items {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		if !neededRefs[ref] && items[ref].Class == "blocking" {
			return refuse(contract.RefusalPremergeBlocked, "%s is blocking (parent decision): fix it on this head and evaluate the new head", ref)
		}
	}
	for _, n := range needed {
		it, ok := items[n.ref]
		if !ok {
			return refuse(contract.RefusalPremergeUndisposed, "%s has no parent disposition", n.ref)
		}
		if err := judgeItem(n, it); err != nil {
			return err
		}
	}
	return nil
}

// judgeItem applies the class rules to one needed finding.
func judgeItem(n need, it Item) error {
	note := it.Note
	noteLen := utf8.RuneCountInString(note)
	switch it.Class {
	case "blocking":
		return refuse(contract.RefusalPremergeBlocked, "%s is blocking: fix it on this head and evaluate the new head", n.ref)
	case "separable":
		if n.criterion != nil {
			return refuse(contract.RefusalPremergeSeparableForbidden, "%s: an unmet criterion is never separable", n.ref)
		}
		d := n.defect
		switch {
		case d.Severity == "P0":
			return refuse(contract.RefusalPremergeSeparableForbidden, "%s: a P0 is always fixed before the merge", n.ref)
		case d.Introduced || d.Impact == "regression_introduced":
			return refuse(contract.RefusalPremergeSeparableForbidden, "%s: a defect this change introduced is in scope and cannot be separable", n.ref)
		case d.InPromise:
			return refuse(contract.RefusalPremergeSeparableForbidden, "%s: inside the issue's promise, so it cannot be carried to a follow-up", n.ref)
		case !followUpKey.MatchString(it.FollowUp):
			return refuse(contract.RefusalPremergeSeparableForbidden, "%s: separable needs a followUp issue key", n.ref)
		case note == "" || (regionWords.MatchString(note) && utf8.RuneCountInString(strings.TrimSpace(regionWords.ReplaceAllString(note, ""))) < 40):
			return refuse(contract.RefusalPremergeSeparableForbidden, "%s: separable needs a reason it is independent of what this issue delivers (an edit region is not one)", n.ref)
		}
	case "not_applicable", "already_resolved":
		if noteLen < 20 {
			return refuse(contract.RefusalPremergeDispositionInvalid, "%s: %s needs a note with code evidence", n.ref, it.Class)
		}
	case "carried":
		d := n.defect
		switch {
		case d == nil && n.criterion.Verdict != "PARTIAL":
			return refuse(contract.RefusalPremergeCarriedForbidden, "%s: a %s criterion is fixed, not carried; only PARTIAL may be", n.ref, n.criterion.Verdict)
		case d != nil && d.Severity == "P0":
			return refuse(contract.RefusalPremergeCarriedForbidden, "%s: a P0 is never carried; fix it before the merge", n.ref)
		case d != nil && d.Severity == "P1":
			return refuse(contract.RefusalPremergeCarriedForbidden, "%s: a P1 is fixed in the correction round, not carried", n.ref)
		case d != nil && d.Impact == "regression_introduced" && !((d.Severity == "P2" || d.Severity == "P3") && utf8.RuneCountInString(it.NewCodeOnly) >= 20):
			return refuse(contract.RefusalPremergeCarriedForbidden, "%s: a regression of behaviour that worked before is never carried (a P2 or P3 confined to code this change added may be, with newCodeOnly naming that code)", n.ref)
		case !followUpKey.MatchString(it.FollowUp):
			return refuse(contract.RefusalPremergeCarriedForbidden, "%s: carried needs the followUp issue key it went to", n.ref)
		case noteLen < 20:
			return refuse(contract.RefusalPremergeCarriedForbidden, "%s: carried needs a note saying why the node was accepted without it", n.ref)
		}
	default:
		return refuse(contract.RefusalPremergeDispositionInvalid, "%s: unknown class %q", n.ref, it.Class)
	}
	return nil
}
