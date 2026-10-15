package ops

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
)

// PremergeInput is what the run knows when it assembles the premerge record of a head: the verdict of the reviewer and the pair
// and prompt that produced it.
type PremergeInput struct {
	Issue, Node, Head, Dev, CriteriaDigest string
	Grader                                 premerge.Grader
	GradedAt                               time.Time
	Verdict                                ReviewVerdict
	// By is the task that decides the dispositions: the orchestrator the run acts for.
	By string
	// Expect are the ids of the criteria of the node's plan (CriterionIDs); a verdict that does not evaluate each of them once is
	// no record. Nil checks nothing.
	Expect []string
}

// AssemblePremerge assembles the premerge-record/1 of a head from the verdict of its review. It is a code step of the run
// (SP-INL-3), never an agent: the criteria and the defects are the verdict's, and every finding the judgment asks a disposition
// for (a criterion that is not met, a defect of P0 to P2 or whose impact is not minor_separable) is disposed of as blocking. The
// default is blocking and nothing else: a finding is separable or carried only when the orchestrator says so (an escalation the
// orchestrator answers), never by the run.
func AssemblePremerge(in PremergeInput) (premerge.Record, error) {
	if len(in.Verdict.Criteria) == 0 {
		return premerge.Record{}, errors.New("the verdict lists no criterion: an incomplete evaluation is not a record")
	}
	if err := CheckCoverage(in.Verdict.Criteria, in.Expect); err != nil {
		return premerge.Record{}, fmt.Errorf("an incomplete evaluation is not a record: %w", err)
	}
	rec := premerge.Record{
		Schema: premerge.RecordSchema, Issue: in.Issue, Node: in.Node, Head: in.Head, Dev: in.Dev, CriteriaDigest: in.CriteriaDigest, Grader: in.Grader,
		GradedAt: in.GradedAt.UTC().Format(time.RFC3339), Criteria: map[string]premerge.Criterion{}, Defects: []premerge.Defect{}, Summary: in.Verdict.Summary,
		Dispositions: premerge.Dispositions{By: in.By, Items: []premerge.Item{}},
	}
	met := 0
	for _, c := range in.Verdict.Criteria {
		verdict := "FAIL"
		if c.Met {
			verdict = "PASS"
			met++
		}
		rec.Criteria[c.ID] = premerge.Criterion{Verdict: verdict, Evidence: c.Evidence}
	}
	score := float64(met) / float64(len(in.Verdict.Criteria))
	rec.Score = &score
	for _, d := range in.Verdict.Defects {
		rec.Defects = append(rec.Defects, premerge.Defect{ID: d.Ref, Severity: d.Severity, Impact: d.Impact, Introduced: d.Introduced, InPromise: d.InPromise, What: d.Finding})
	}
	for _, need := range PremergeNeeds(rec) {
		rec.Dispositions.Items = append(rec.Dispositions.Items, premerge.Item{Ref: need, Class: "blocking"})
	}
	return rec, nil
}

// UnitCheckVerdict is the verdict a unit check stands for when crw.accept assembles a premerge record from its record (CRW-1456):
// a unit check judges the head and leaves findings, but no model judges the node's criteria one by one. Every criterion of the
// node's plan (expect) passes on the evidence of the passing verification of the head (its digest) and of the check (its call)
// that left no blocking finding; each finding is a defect named by its finding id. A check that gave no judgment, that did not
// verify, or that verified over a finding that blocks is no verified verdict (an error). An empty expect gives a verdict with no
// criterion, which AssemblePremerge refuses: a node whose plan names no criterion is never accepted on a unit check. receipt is the
// sha256 of the executor's evidence receipt the check was handed ("" for none); the evidence and the summary name it.
func UnitCheckVerdict(res reviewadapter.Result, call, verification string, expect []string, receipt string) (ReviewVerdict, error) {
	switch {
	case res.State != reviewadapter.StateOK:
		return ReviewVerdict{}, fmt.Errorf("unit check %s gave no judgment (state %s)", call, res.State)
	case res.Verdict != reviewadapter.VerdictVerified:
		return ReviewVerdict{}, fmt.Errorf("unit check %s ruled %q, not verified", call, res.Verdict)
	}
	for _, f := range res.Findings {
		if premerge.Blocks(f.Severity, f.Impact) {
			return ReviewVerdict{}, fmt.Errorf("unit check %s is verified over finding %s (%s %s), which blocks", call, f.ID, f.Severity, f.Impact)
		}
	}
	v := ReviewVerdict{Verdict: reviewadapter.VerdictVerified, Criteria: []Criterion{}, Defects: []Defect{}, Summary: "unit check " + call + ": verified"}
	family := res.EngineFamily
	if family == "" {
		family = execution.ModelFamily(res.Model)
	}
	evidence := fmt.Sprintf("verification %s of the head passed; unit check %s (%s %s) left no blocking finding", verification, call, family, res.Model)
	if receipt != "" {
		evidence += "; executor receipt " + receipt
		v.Summary += "; executor receipt " + receipt
	}
	for _, id := range expect {
		v.Criteria = append(v.Criteria, Criterion{ID: id, Met: true, Evidence: evidence})
	}
	for _, f := range res.Findings {
		v.Defects = append(v.Defects, Defect{Ref: f.ID, Kind: "other", Severity: f.Severity, Impact: f.Impact, Introduced: true, File: f.File, Line: f.Line, Finding: f.Text})
	}
	return v, nil
}

// PremergeNeeds are the findings of a record the judgment asks a disposition for, in the order it asks.
func PremergeNeeds(r premerge.Record) []string {
	var out []string
	for _, id := range sortedKeys(r.Criteria) {
		if r.Criteria[id].Verdict != "PASS" {
			out = append(out, id)
		}
	}
	for _, d := range r.Defects {
		if premerge.Blocks(d.Severity, d.Impact) {
			out = append(out, d.ID)
		}
	}
	return out
}

func sortedKeys(m map[string]premerge.Criterion) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// PremergeVerdict judges an assembled record the way the relay's acceptance does and answers its document: "" and the document
// when the record passes, else the closed reason of the refusal and its detail.
func PremergeVerdict(rec premerge.Record) (doc []byte, reason, detail string, err error) {
	doc, err = json.Marshal(rec)
	if err != nil {
		return nil, "", "", err
	}
	if _, err := premerge.Decode(doc); err != nil {
		var ref *premerge.Refusal
		if errors.As(err, &ref) {
			return doc, string(ref.Reason), ref.Detail, nil
		}
		return doc, "premerge_missing", err.Error(), nil
	}
	if err := premerge.Judge(rec, premerge.Options{}); err != nil {
		var ref *premerge.Refusal
		if errors.As(err, &ref) {
			return doc, string(ref.Reason), ref.Detail, nil
		}
		return doc, "premerge_blocked", err.Error(), nil
	}
	return doc, "", "", nil
}

// Refusal is the error of an op that did not do what it was asked for a closed reason; the script reads the reason from the
// message (ReasonOf). The row such an op leaves is a refusal, not a completed row, so a resume asks again.
type Refusal struct {
	Reason, Detail string
}

func (r *Refusal) Error() string { return fmt.Sprintf("refused:%s: %s", r.Reason, r.Detail) }

// ReasonOf is the reason of a Refusal message ("" for any other text).
func ReasonOf(msg string) string {
	i := strings.Index(msg, "refused:")
	if i < 0 {
		return ""
	}
	rest := msg[i+len("refused:"):]
	if j := strings.Index(rest, ":"); j >= 0 {
		return rest[:j]
	}
	return rest
}
