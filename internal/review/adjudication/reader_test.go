package adjudication

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

func TestSavedReadersCombineThreeSourcesWithoutGroundTruth(t *testing.T) {
	p := PR{"example/repo", 1, strings.Repeat("a", 40)}
	a := review.Artifact{Schema: review.SchemaV1, Tool: "crw", ToolVersion: "test", Model: "A", Effort: "test", AgyVersion: "unknown", Base: strings.Repeat("b", 40), Head: p.Head, PatchID: strings.Repeat("c", 40), Reviewers: review.ReviewerCounts{Run: 1}, Status: review.StatusComplete, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:00:01Z", Findings: []review.Finding{{File: "x.go", Line: 1, Title: "$(do not execute)", Explanation: "saved data", Severity: "P1", Grade: review.P1, Perspective: "correctness", Reviewers: []int{0}, Support: 1, Verdict: review.VerdictConfirmed}}, Calls: []review.CallRecord{{Stage: "review", Reviewer: 0, Chunk: 0, Perspective: "correctness", Model: "A", Class: "normal", ElapsedMillis: 5, Tokens: review.TokenUsage{Input: 7, Total: 7}}}}
	data, err := a.Marshal(p.Head)
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	run, err := ReadArtifact(data, p, digest)
	if err != nil {
		t.Fatal(err)
	}
	if run.Model != "A" || run.ArtifactSHA256 != digest || run.Calls[0].TokensKnown || !run.Calls[0].ElapsedKnown {
		t.Fatalf("artifact identity/accounting: %+v", run)
	}
	if _, err := ReadArtifact(append(data, ' '), p, digest); err == nil {
		t.Fatal("hash mismatch accepted")
	}
	wrong := p
	wrong.Head = strings.Repeat("d", 40)
	if _, err := ReadArtifact(data, wrong, digest); err == nil {
		t.Fatal("wrong head accepted")
	}
	records := []Record{{Schema, "independent", &run, nil}}
	groups := []Reviewer{run.Reviewer}
	for _, source := range []Source{Devin, Codex} {
		export := Run{ID: string(source), PR: p, Reviewer: Reviewer{source, "unknown"}, Status: "complete", Findings: []Finding{{"saved-comment", "x.go", 1, "saved finding", review.P1, false}}}
		b, err := json.Marshal(export)
		if err != nil {
			t.Fatal(err)
		}
		r, err := ReadExport(b, p)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, Record{Schema, string(source), &r, nil})
		groups = append(groups, r.Reviewer)
		if _, err := ReadExport(b, wrong); err == nil {
			t.Fatal("export PR mismatch accepted")
		}
	}
	s, err := Report(records, []PR{p}, groups)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range s.Rows {
		if row.Pending != 1 || row.Correct != 0 || row.Precision.Value != nil || row.P1.Value != nil {
			t.Fatalf("reviewer confirmation became ground truth: %+v", row)
		}
	}
	for _, b := range []string{"null", "{}", "{\"source\":\"devin\",\"extra\":1}", "{} {}"} {
		if _, err := ReadExport([]byte(b), p); err == nil {
			t.Fatal("malformed export accepted")
		}
	}
}
