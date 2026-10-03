package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/agy"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/bundle"
)

type fakeHead struct{ readErr error }

func (fakeHead) Lines(string) (int, error) { return 10, nil }
func (h fakeHead) ReadLines(_ string, start, end int) ([]string, error) {
	if h.readErr != nil {
		return nil, h.readErr
	}
	var lines []string
	for i := start; i <= end; i++ {
		lines = append(lines, fmt.Sprintf("head code %d", i))
	}
	return lines, nil
}
func testBundle() *bundle.Bundle {
	return &bundle.Bundle{Metadata: bundle.Metadata{Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), PatchID: strings.Repeat("c", 40), FileCount: 1, Additions: 200, Files: []bundle.FileMetadata{{Path: "a.go", HeadLines: 10}}}, Chunks: []bundle.Chunk{{Paths: []string{"a.go"}, Text: "synthetic diff and head"}}}
}
func sample(line int, title, severity string) map[string]any {
	return map[string]any{"file": "a.go", "line": line, "endLine": 0, "title": title, "explanation": "demonstrable wrong behavior", "severity": severity, "needsContext": false}
}
func normal(v any) agy.Result {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return agy.Result{Class: agy.ClassNormal, StructuredOutput: data, Model: "served-test-model", Elapsed: time.Millisecond, Usage: agy.Usage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 3, TotalTokens: 15}}
}
func findings(fs ...map[string]any) agy.Result {
	if fs == nil {
		fs = []map[string]any{}
	}
	return normal(map[string]any{"findings": fs})
}
func scripted(t *testing.T, steps []agy.Result) (Runner, *[]agy.Request) {
	t.Helper()
	requests := []agy.Request{}
	return func(_ context.Context, cfg agy.Config, req agy.Request) (agy.Result, error) {
		if cfg.Model != "requested-test-model" {
			t.Errorf("model configuration lost: %q", cfg.Model)
		}
		requests = append(requests, req)
		i := len(requests) - 1
		if i >= len(steps) {
			t.Fatalf("unexpected call %d", i)
		}
		return steps[i], nil
	}, &requests
}
func TestRunEndToEnd(t *testing.T) {
	run, calls := scripted(t, []agy.Result{
		findings(sample(2, "shared bug", "P2"), sample(4, "false alarm", "P2")),
		findings(sample(2, "shared bug", "P2"), sample(5, "weak evidence", "P3")),
		findings(sample(6, "security risk", "P1 security")),
		normal(map[string]any{"groups": [][]int{{0, 2}, {1}, {3}, {4}}}),
		normal(map[string]any{"verdict": "confirmed", "needsContext": false}),
		normal(map[string]any{"verdict": "rejected", "needsContext": false}),
		normal(map[string]any{"verdict": "uncertain", "needsContext": false}),
		normal(map[string]any{"verdict": "uncertain", "needsContext": false}),
	})
	b := testBundle()
	a, err := Run(context.Background(), b, run, Config{Agy: agy.Config{Model: "requested-test-model"}, Head: fakeHead{}})
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != review.StatusComplete || a.Reviewers.Run != 3 || len(*calls) != 8 {
		t.Fatalf("want complete with three reviewers and eight serial calls, got %+v, calls=%d", a, len(*calls))
	}
	if len(a.Findings) != 2 || a.Findings[0].Support != 2 || !slices.Equal(a.Findings[0].Reviewers, []int{0, 1}) || a.Findings[0].Verdict != review.VerdictConfirmed {
		t.Fatalf("grouped evidence: %+v", a.Findings)
	}
	if !a.Findings[1].Security || a.Findings[1].Verdict != review.VerdictUncertain || len(a.Dropped) != 2 || a.Dropped[0].Reason != review.ReasonRejected || a.Dropped[1].Reason != review.ReasonBelowThreshold {
		t.Fatalf("verification/rules: %+v", a)
	}
	data, err := a.Marshal(b.Metadata.Head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = review.ParseArtifact(data, b.Metadata.Head); err != nil {
		t.Fatal(err)
	}
}
