// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/store/economics.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package adjudication

import (
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

type Ratio struct {
	Numerator   int      `json:"numerator"`
	Denominator int      `json:"denominator"`
	Value       *float64 `json:"value"`
}

type Row struct {
	Reviewer
	Runs                 int               `json:"runs"`
	Reviewers            int               `json:"reviewers"`
	KnownReviewerRuns    int               `json:"knownReviewerRuns"`
	KnownCallRuns        int               `json:"knownCallRuns"`
	Calls                int               `json:"calls"`
	KnownElapsedCalls    int               `json:"knownElapsedCalls"`
	KnownTokenCalls      int               `json:"knownTokenCalls"`
	ElapsedMillis        int64             `json:"elapsedMillis"`
	Tokens               review.TokenUsage `json:"tokens"`
	RecordedTokens       review.TokenUsage `json:"recordedTokens"`
	Findings             int               `json:"findings"`
	Pending              int               `json:"pending"`
	Correct              int               `json:"correct"`
	FalsePositive        int               `json:"falsePositive"`
	Minor                int               `json:"minor"`
	CorrectWithChange    int               `json:"correctWithChange"`
	CorrectWithoutChange int               `json:"correctWithoutChange"`
	CorrectChangeUnknown int               `json:"correctChangeUnknown"`
	UniqueCorrect        int               `json:"uniqueCorrect"`
	Precision            Ratio             `json:"precision"`
	P0                   Ratio             `json:"p0"`
	P1                   Ratio             `json:"p1"`
	Security             Ratio             `json:"security"`
	InvalidRuns          int               `json:"invalidRuns"`
	UnavailableRuns      int               `json:"unavailableRuns"`
	PartialRuns          int               `json:"partialRuns"`
	ModelMismatchRuns    int               `json:"modelMismatchRuns"`
	MissingPRs           []PR              `json:"missingPRs"`
	ServedModels         []string          `json:"servedModels"`
}

type Summary struct {
	PRs  []PR  `json:"prs"`
	Rows []Row `json:"rows"`
}

func ratio(n, d int) Ratio {
	r := Ratio{Numerator: n, Denominator: d}
	if d > 0 {
		v := float64(n) / float64(d)
		r.Value = &v
	}
	return r
}

func addTokens(sum *review.TokenUsage, t review.TokenUsage) {
	sum.Input += t.Input
	sum.Output += t.Output
	sum.Thinking += t.Thinking
	sum.CacheRead += t.CacheRead
	sum.Total += t.Total
}

type rowState struct {
	Row
	seen   map[PR]bool
	caught map[problemRef]Judgment
	models map[string]bool
}

// Report compares every requested group over exactly prs, including absent and failed runs.
// Its ground truth is exclusively the latest parent judgments and recorded later changes.
func Report(records []Record, prs []PR, groups []Reviewer) (Summary, error) {
	l, err := replay(records)
	if err != nil {
		return Summary{}, err
	}
	if len(prs) == 0 || len(groups) == 0 {
		return Summary{}, fmt.Errorf("explicit nonempty PR cohort and reviewer groups required")
	}
	prs, groups = slices.Clone(prs), slices.Clone(groups)
	slices.SortFunc(prs, func(a, b PR) int {
		if c := strings.Compare(a.Repository, b.Repository); c != 0 {
			return c
		}
		if a.Number < b.Number {
			return -1
		}
		if a.Number > b.Number {
			return 1
		}
		return strings.Compare(a.Head, b.Head)
	})
	slices.SortFunc(groups, func(a, b Reviewer) int {
		if c := strings.Compare(string(a.Source), string(b.Source)); c != 0 {
			return c
		}
		return strings.Compare(a.Model, b.Model)
	})
	selected := map[PR]bool{}
	for _, p := range prs {
		if err := p.validate(); err != nil {
			return Summary{}, err
		}
		if selected[p] {
			return Summary{}, fmt.Errorf("duplicate PR in cohort")
		}
		selected[p] = true
	}
	rows := make([]rowState, len(groups))
	index := map[Reviewer]int{}
	for i, g := range groups {
		if err := g.validate(); err != nil {
			return Summary{}, err
		}
		if _, exists := index[g]; exists {
			return Summary{}, fmt.Errorf("duplicate reviewer group")
		}
		index[g] = i
		rows[i] = rowState{Row: Row{Reviewer: g, MissingPRs: []PR{}, ServedModels: []string{}}, seen: map[PR]bool{}, caught: map[problemRef]Judgment{}, models: map[string]bool{}}
	}
	owners := map[problemRef]map[Reviewer]bool{}
	truth := map[problemRef]Judgment{}
	for _, record := range records {
		if record.Run == nil {
			continue
		}
		run := *record.Run
		i, included := index[run.Reviewer]
		if !included || !selected[run.PR] {
			continue
		}
		r := &rows[i]
		r.Runs++
		r.seen[run.PR] = true
		if run.Reviewers != nil {
			r.KnownReviewerRuns++
			r.Reviewers += *run.Reviewers
		}
		account(r, run)
		for _, f := range run.Findings {
			r.Findings++
			j, judged := l.judgments[findingRef{run.ID, f.ID}]
			if !judged {
				r.Pending++
				continue
			}
			switch j.Verdict {
			case FalsePositive:
				r.FalsePositive++
			case Minor:
				r.Minor++
			case Correct:
				r.Correct++
				if j.CodeChanged == nil {
					r.CorrectChangeUnknown++
				} else if *j.CodeChanged {
					r.CorrectWithChange++
				} else {
					r.CorrectWithoutChange++
				}
				key := problemRef{run.PR, j.Problem}
				r.caught[key] = j
				truth[key] = j
				if owners[key] == nil {
					owners[key] = map[Reviewer]bool{}
				}
				owners[key][run.Reviewer] = true
			}
		}
	}
	denom := severeCounts(truth)
	s := Summary{PRs: prs, Rows: []Row{}}
	for _, r := range rows {
		for _, p := range prs {
			if !r.seen[p] {
				r.MissingPRs = append(r.MissingPRs, p)
			}
		}
		for key := range r.caught {
			if len(owners[key]) == 1 {
				r.UniqueCorrect++
			}
		}
		for model := range r.models {
			r.ServedModels = append(r.ServedModels, model)
		}
		slices.Sort(r.ServedModels)
		caught := severeCounts(r.caught)
		r.Precision = ratio(r.Correct, r.Correct+r.FalsePositive+r.Minor)
		r.P0 = ratio(caught[0], denom[0])
		r.P1 = ratio(caught[1], denom[1])
		r.Security = ratio(caught[2], denom[2])
		s.Rows = append(s.Rows, r.Row)
	}
	return s, nil
}

func severeCounts(problems map[problemRef]Judgment) [3]int {
	var counts [3]int
	for _, j := range problems {
		if j.Grade == review.P0 {
			counts[0]++
		}
		if j.Grade == review.P1 {
			counts[1]++
		}
		if j.Security {
			counts[2]++
		}
	}
	return counts
}

func account(r *rowState, run Run) {
	invalid, unavailable, mismatch := run.Status == "invalid", run.Status == "unavailable", false
	if run.Status == "partial" {
		r.PartialRuns++
	}
	if run.Calls != nil {
		r.KnownCallRuns++
	}
	for _, call := range run.Calls {
		c := call.Record
		r.Calls++
		r.models[c.Model] = true
		invalid = invalid || c.Class == "invalid"
		unavailable = unavailable || c.Class == "unavailable"
		mismatch = mismatch || c.Model != run.Model
		addTokens(&r.RecordedTokens, c.Tokens)
		if call.ElapsedKnown {
			r.KnownElapsedCalls++
			r.ElapsedMillis += c.ElapsedMillis
		}
		if call.TokensKnown {
			r.KnownTokenCalls++
			addTokens(&r.Tokens, c.Tokens)
		}
	}
	if invalid {
		r.InvalidRuns++
	}
	if unavailable {
		r.UnavailableRuns++
	}
	if mismatch {
		r.ModelMismatchRuns++
	}
}
