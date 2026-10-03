package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/agy"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/bundle"
)

func TestFailuresRemainEvidence(t *testing.T) {
	one := findings(sample(2, "real bug", "P2"))
	two := findings(sample(2, "first bug", "P2"), sample(3, "second bug", "P2"))
	invalid := agy.Result{Class: agy.ClassInvalid, Reason: agy.ReasonDeniedActions, StructuredOutput: one.StructuredOutput}
	quota := agy.Result{Class: agy.ClassUnavailable, Reason: agy.ReasonQuota}
	for _, c := range []struct {
		name                  string
		steps                 []agy.Result
		head                  fakeHead
		status                review.Status
		failed, kept, dropped int
	}{
		{"some invalid", []agy.Result{invalid, findings(), findings()}, fakeHead{}, review.StatusPartial, 1, 0, 0},
		{"all invalid/unavailable", []agy.Result{invalid, invalid, quota}, fakeHead{}, review.StatusUnavailable, 3, 0, 0},
		{"all malformed", []agy.Result{normal(map[string]any{}), normal(map[string]any{}), normal(map[string]any{})}, fakeHead{}, review.StatusUnavailable, 3, 0, 0},
		{"group invalid", []agy.Result{two, findings(), findings(), invalid}, fakeHead{}, review.StatusPartial, 0, 2, 0},
		{"group unavailable", []agy.Result{two, findings(), findings(), quota}, fakeHead{}, review.StatusPartial, 0, 2, 0},
		{"duplicate partition", []agy.Result{two, findings(), findings(), normal(map[string]any{"groups": [][]int{{0, 0}, {1}}})}, fakeHead{}, review.StatusPartial, 0, 2, 0},
		{"missing partition", []agy.Result{two, findings(), findings(), normal(map[string]any{"groups": [][]int{{0}}})}, fakeHead{}, review.StatusPartial, 0, 2, 0},
		{"verification unavailable", []agy.Result{one, findings(), findings(), quota}, fakeHead{}, review.StatusPartial, 0, 1, 0},
		{"verification malformed", []agy.Result{one, findings(), findings(), normal(map[string]any{"verdict": "confirmed"})}, fakeHead{}, review.StatusPartial, 0, 1, 0},
		{"read failure", []agy.Result{one, findings(), findings()}, fakeHead{readErr: errors.New("unreadable head code")}, review.StatusPartial, 0, 1, 0},
		{"unsafe representative", []agy.Result{findings(sample(11, "invalid location", "P2"), sample(5, "valid location", "P2")), findings(), findings(), normal(map[string]any{"groups": [][]int{{0, 1}}})}, fakeHead{}, review.StatusPartial, 0, 1, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			run, reqs := scripted(t, c.steps)
			b := testBundle()
			a, err := Run(context.Background(), b, run, Config{Agy: agy.Config{Model: "requested-test-model"}, Head: c.head})
			if err != nil {
				t.Fatal(err)
			}
			if a.Status != c.status || a.Reason == "" || a.Reviewers.Failed != c.failed || len(a.Findings) != c.kept || len(a.Dropped) != c.dropped || len(*reqs) != len(c.steps) {
				t.Fatalf("outcome: %+v calls%d", a, len(*reqs))
			}
			for _, f := range a.Findings {
				if f.Verdict != review.VerdictUnverified {
					t.Fatalf("failure became confirmation: %+v", f)
				}
			}
			if err := a.Validate(b.Metadata.Head); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestChunksAccountingAndNeedsContext(t *testing.T) {
	ctxFinding := sample(3, "outside context", "P3")
	ctxFinding["file"], ctxFinding["needsContext"] = "b.go", true
	confirmed := sample(4, "other bug", "P2")
	confirmed["file"] = "b.go"
	run, reqs := scripted(t, []agy.Result{findings(sample(2, "shared", "P2"), sample(2, "shared", "P2")), findings(ctxFinding), findings(sample(2, "shared", "P2")), findings(ctxFinding), findings(), findings(confirmed), normal(map[string]any{"groups": [][]int{{0, 1, 3}, {2, 4}, {5}}}), normal(map[string]any{"verdict": "confirmed", "needsContext": false}), normal(map[string]any{"verdict": "confirmed", "needsContext": false})})
	b := testBundle()
	b.Metadata.FileCount = 2
	b.Metadata.Files = append(b.Metadata.Files, bundle.FileMetadata{Path: "b.go", HeadLines: 10})
	b.Chunks = append(b.Chunks, bundle.Chunk{Paths: []string{"b.go"}, Text: "second chunk"})
	a, err := Run(context.Background(), b, run, Config{Agy: agy.Config{Model: "requested-test-model"}, Head: fakeHead{}})
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != review.StatusComplete || len(a.Findings) != 3 || len(a.Calls) != 9 || len(*reqs) != 9 {
		t.Fatalf("%+v", a)
	}
	if a.Findings[0].Support != 2 || a.Findings[1].Support != 2 || !a.Findings[1].NeedsContext || a.Findings[1].Verdict != review.VerdictUnverified || a.Findings[2].Verdict != review.VerdictConfirmed {
		t.Fatal(a.Findings)
	}
	for i, c := range a.Calls {
		if c.Class != "normal" || c.Model != "served-test-model" || c.ElapsedMillis != 1 || c.Tokens.Input != 10 || c.Tokens.Output != 2 || c.Tokens.CacheRead != 3 || c.Tokens.Total != 15 {
			t.Fatalf("call %d: %+v", i, c)
		}
		if i < 6 && (c.Reviewer != i/2 || c.Chunk != i%2) {
			t.Fatalf("serial chunk order %d: %+v", i, c)
		}
	}
	if _, err := a.Marshal(b.Metadata.Head); err != nil {
		t.Fatal(err)
	}
}

func TestStrictModelOutput(t *testing.T) {
	for _, data := range []string{`{}`, `{"FINDINGS":[]}`, `{"findings":[],"extra":true}`, `{"findings":null}`, `{"findings":[null]}`, `{"findings":[{}]}`, `{"findings":"none"}`, `{"findings":[]} {}`, `null`, `[]`} {
		run, _ := scripted(t, []agy.Result{{Class: agy.ClassNormal, StructuredOutput: []byte(data)}, findings(), findings()})
		a, err := Run(context.Background(), testBundle(), run, Config{Agy: agy.Config{Model: "requested-test-model"}, Head: fakeHead{}})
		if err != nil || a.Status != review.StatusPartial || a.Reviewers.Failed != 1 || a.Calls[0].Class != "invalid" || !strings.HasPrefix(a.Calls[0].Reason, "invalid_output:") {
			t.Fatalf("%s: %+v %v", data, a, err)
		}
	}
}

func TestFailureCountsAndContextPartition(t *testing.T) {
	b := testBundle()
	cfg := Config{Agy: agy.Config{Model: "requested-test-model"}, Head: fakeHead{}}
	run, _ := scripted(t, []agy.Result{{Class: agy.ClassInvalid, Reason: agy.ReasonTimeLimit}, {Class: agy.ClassUnavailable, Reason: agy.ReasonAuthentication}, {Class: agy.ClassUnavailable, Reason: agy.ReasonQuota}})
	a, err := Run(context.Background(), b, run, cfg)
	if err != nil || a.Reviewers != (review.ReviewerCounts{Run: 3, Failed: 3, TimedOut: 1, AuthFailed: 1}) || a.Status != review.StatusUnavailable {
		t.Fatalf("%+v %v", a, err)
	}
	ctxFinding := sample(2, "needs outside context", "P3")
	ctxFinding["needsContext"] = true
	run, _ = scripted(t, []agy.Result{findings(ctxFinding, sample(2, "confirmed candidate", "P2")), findings(), findings(), normal(map[string]any{"groups": [][]int{{0, 1}}})})
	a, err = Run(context.Background(), b, run, cfg)
	if err != nil || a.Status != review.StatusPartial || len(a.Findings) != 2 || !a.Findings[0].NeedsContext || a.Findings[1].NeedsContext || a.Calls[3].Reason != "invalid_partition" {
		t.Fatalf("mixed context merged: %+v %v", a, err)
	}
	for _, change := range []func(map[string]any){func(f map[string]any) { f["needsContext"] = nil }, func(f map[string]any) { f["line"] = "2" }, func(f map[string]any) { f["severity"] = nil }} {
		f := sample(2, "bug", "P2")
		change(f)
		run, _ = scripted(t, []agy.Result{findings(f), findings(), findings()})
		a, err = Run(context.Background(), b, run, cfg)
		if err != nil || a.Calls[0].Class != "invalid" || a.Reviewers.Failed != 1 {
			t.Fatalf("malformed scalar: %+v %v", a, err)
		}
	}
}

func TestBlankTextDoesNotEraseValidEvidence(t *testing.T) {
	for _, field := range []string{"title", "explanation"} {
		bad := sample(2, "bad text", "P2")
		bad[field] = " "
		run, _ := scripted(t, []agy.Result{findings(bad), findings(sample(3, "independent defect", "P1")), findings(), {Class: agy.ClassUnavailable, Reason: agy.ReasonQuota}})
		a, err := Run(context.Background(), testBundle(), run, Config{Agy: agy.Config{Model: "requested-test-model"}, Head: fakeHead{}})
		if err != nil || a == nil {
			t.Fatalf("%s erased other evidence: %v", field, err)
		}
		if a.Status != review.StatusPartial || a.Reviewers.Failed != 1 || a.Calls[0].Class != "invalid" || len(a.Findings) != 1 || a.Findings[0].Title != "independent defect" {
			t.Fatalf("%s: %+v", field, a)
		}
	}
}
