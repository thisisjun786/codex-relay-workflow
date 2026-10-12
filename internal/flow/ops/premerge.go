package ops

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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

// PremergeNeeds are the findings of a record the judgment asks a disposition for, in the order it asks.
func PremergeNeeds(r premerge.Record) []string {
	var out []string
	for _, id := range sortedKeys(r.Criteria) {
		if r.Criteria[id].Verdict != "PASS" {
			out = append(out, id)
		}
	}
	for _, d := range r.Defects {
		if d.Severity == "P0" || d.Severity == "P1" || d.Severity == "P2" || (d.Impact != "" && d.Impact != "minor_separable") {
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
