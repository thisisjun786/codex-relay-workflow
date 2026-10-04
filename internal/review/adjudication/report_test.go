package adjudication

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

func scripted() ([]Record, []PR, []Reviewer) {
	prs := []PR{{"example/repo", 1, strings.Repeat("a", 40)}, {"example/repo", 2, strings.Repeat("b", 40)}}
	groups := []Reviewer{{Independent, "A"}, {Independent, "B"}, {Devin, "unknown"}, {Codex, "unknown"}}
	var records []Record
	add := func(id string, pr PR, group Reviewer, status string, count int, calls []Call) {
		r := Run{ID: id, PR: pr, Reviewer: group, Status: status, Calls: calls, Findings: []Finding{}}
		if group.Source == Independent {
			r.ArtifactSHA256 = strings.Repeat(id[:1], 64)
			n := 2
			r.Reviewers = &n
		}
		if status != "complete" {
			r.Reason = status
		}
		for i := range count {
			r.Findings = append(r.Findings, Finding{fmt.Sprint(i), "x.go", i + 1, "reported claim", review.P3, false})
		}
		records = append(records, Record{Schema, "run-" + id, &r, nil})
	}
	call := func(model, class string, ms, in, out, think, cache, total int64) Call {
		c := review.CallRecord{Stage: "review", Model: model, Class: class, ElapsedMillis: ms, Tokens: review.TokenUsage{Input: in, Output: out, Thinking: think, CacheRead: cache, Total: total}}
		if class != "normal" {
			c.Reason = class
		}
		return Call{c, true, true}
	}
	add("a1", prs[0], groups[0], "complete", 5, []Call{call("A", "normal", 11, 10, 1, 2, 3, 16), call("aux", "normal", 13, 20, 2, 4, 5, 31)})
	add("b1", prs[0], groups[1], "complete", 1, nil)
	add("d1", prs[0], groups[2], "complete", 2, nil)
	add("c1", prs[0], groups[3], "complete", 1, nil)
	add("a2", prs[1], groups[0], "invalid", 0, []Call{call("A", "invalid", 7, 4, 0, 0, 0, 4)})
	add("b2", prs[1], groups[1], "unavailable", 0, []Call{call("B", "unavailable", 3, 0, 0, 0, 0, 0)})
	judge := func(run, finding, problem string, v Verdict, grade review.Grade, security bool, changed *bool) {
		j := Judgment{RunID: run, FindingID: finding, Parent: "parent", Problem: problem, Verdict: v, Grade: grade, Security: security, CodeChanged: changed, Note: "scripted parent evidence"}
		if changed != nil && *changed {
			j.ChangeCommit = strings.Repeat("c", 40)
		}
		records = append(records, Record{Schema, fmt.Sprint("j-", len(records)), nil, &j})
	}
	yes, no := true, false
	judge("a1", "0", "S", Correct, review.P1, true, &yes)
	judge("a1", "1", "S", FalsePositive, review.P1, true, &no)
	judge("a1", "2", "M", Minor, review.P3, false, nil)
	judge("a1", "3", "A0", Correct, review.P0, false, &no)
	judge("b1", "0", "S", Correct, review.P1, true, nil)
	judge("d1", "0", "D", Correct, review.P1, false, nil)
	judge("d1", "1", "S", FalsePositive, review.P1, true, nil)
	judge("c1", "0", "A0", Correct, review.P0, false, nil)
	return records, prs, groups
}

func TestFixedCohortCorrectionsAndUnknownAccounting(t *testing.T) {
	records, prs, groups := scripted()
	s, err := Report(records, prs, groups)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(s)
	slices.Reverse(prs)
	slices.Reverse(groups)
	s, err = Report(records, prs, groups)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := json.Marshal(s)
	if string(first) != string(second) {
		t.Fatal("input selection order changed report bytes")
	}
	j := *records[6].Judgment
	j.Verdict = FalsePositive // codeChanged remains true; it must not promote correctness.
	records = append(records, Record{Schema, "parent-correction", nil, &j})
	s, err = Report(records, prs, groups)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range s.Rows {
		if r.Source == Independent && r.Model == "A" && (r.Correct != 1 || r.Security.Numerator != 0 || r.CorrectWithChange != 0) {
			t.Fatalf("another reviewer or code change supplied ground truth: %+v", r)
		}
	}
	r := *records[0].Run
	r.Calls = slices.Clone(r.Calls)
	r.Calls[0].TokensKnown = false
	r.Calls[1].TokensKnown = true
	r.Calls[1].Record.Tokens = review.TokenUsage{}
	s, err = Report([]Record{{Schema, "usage", &r, nil}}, []PR{r.PR}, []Reviewer{r.Reviewer})
	if err != nil {
		t.Fatal(err)
	}
	row := s.Rows[0]
	if row.Calls != 2 || row.KnownElapsedCalls != 2 || row.KnownTokenCalls != 1 || row.Tokens.Total != 0 || row.RecordedTokens.Total != 16 || row.ElapsedMillis != 24 {
		t.Fatalf("unknown vs measured zero: %+v", row)
	}
	for _, selection := range []struct {
		prs    []PR
		groups []Reviewer
	}{{nil, groups}, {prs, nil}, {append(prs, prs[0]), groups}, {prs, append(groups, groups[0])}} {
		if _, err := Report(nil, selection.prs, selection.groups); err == nil {
			t.Fatal("empty/duplicate selection accepted")
		}
	}
}

func TestScriptedMetrics(t *testing.T) {
	records, prs, groups := scripted()
	s, err := Report(records, prs, groups)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Rows) != 4 {
		t.Fatalf("metric rows = %d, want 4", len(s.Rows))
	}
	for _, r := range s.Rows {
		switch r.Reviewer {
		case groups[0]:
			if r.Correct != 2 || r.Pending != 1 || r.Precision.Value == nil || *r.Precision.Value != .5 || r.P0.Numerator != 1 || r.P1.Numerator != 1 || r.P1.Denominator != 2 || r.Security.Numerator != 1 || r.UniqueCorrect != 0 {
				t.Fatalf("A metrics: %+v", r)
			}
			if r.ElapsedMillis != 31 || r.Tokens != (review.TokenUsage{Input: 34, Output: 3, Thinking: 6, CacheRead: 8, Total: 51}) || r.Calls != 3 || r.InvalidRuns != 1 || r.ModelMismatchRuns != 1 || r.CorrectWithChange != 1 || r.CorrectWithoutChange != 1 || r.Reviewers != 4 || r.KnownReviewerRuns != 2 || r.KnownElapsedCalls != 3 || r.KnownTokenCalls != 3 || r.RecordedTokens != r.Tokens {
				t.Fatalf("A accounting: %+v", r)
			}
		case groups[1]:
			if r.Runs != 2 || r.UnavailableRuns != 1 || len(r.MissingPRs) != 0 || r.Security.Numerator != 1 {
				t.Fatalf("B: %+v", r)
			}
		case groups[2]:
			if r.UniqueCorrect != 1 || r.Precision.Value == nil || *r.Precision.Value != .5 || r.Security.Numerator != 0 || len(r.MissingPRs) != 1 {
				t.Fatalf("Devin must receive no credit from another review: %+v", r)
			}
		case groups[3]:
			if r.P0.Numerator != 1 || r.P1.Numerator != 0 || r.UniqueCorrect != 0 || len(r.MissingPRs) != 1 {
				t.Fatalf("Codex: %+v", r)
			}
		}
	}
}
