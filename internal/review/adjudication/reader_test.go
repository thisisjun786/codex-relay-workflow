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

func TestRejectMissingNullAndOverflow(t *testing.T) {
	records, prs, groups := scripted()
	r := *records[0].Run
	r.Source, r.ArtifactSHA256 = Codex, ""
	data, _ := json.Marshal(r)
	for _, key := range []string{"elapsedMillis", "tokens"} {
		for _, absent := range []bool{false, true} {
			var raw map[string]any
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			call := raw["calls"].([]any)[0].(map[string]any)["record"].(map[string]any)
			if absent {
				delete(call, key)
			} else {
				call[key] = nil
			}
			bad, _ := json.Marshal(raw)
			if _, err := ReadExport(bad, r.PR); err == nil {
				t.Fatalf("%s absent=%v became known zero", key, absent)
			}
		}
	}
	parent, _ := json.Marshal(records[6])
	observation, _ := json.Marshal(records[0])
	snapshot := string(observation) + "\n" + strings.Replace(string(parent), `"security":true`, `"security":null`, 1) + "\n"
	if _, err := Read(strings.NewReader(snapshot)); err == nil {
		t.Fatal("null parent security became false")
	}
	r.Calls[0].Record.ElapsedMillis = 1<<63 - 1
	r.Calls[0].Record.Tokens = review.TokenUsage{Input: 1<<63 - 1, Total: 1<<63 - 1}
	r.Calls[1] = r.Calls[0]
	if _, err := Report([]Record{{Schema, "overflow", &r, nil}}, prs, append(groups, r.Reviewer)); err == nil {
		t.Fatal("aggregate overflow accepted")
	}
	r.Calls = nil
	n := int(^uint(0) >> 1)
	r.Reviewers = &n
	first := r
	r.ID = "second-run"
	if _, err := Report([]Record{{Schema, "one", &first, nil}, {Schema, "two", &r, nil}}, prs, []Reviewer{r.Reviewer}); err == nil {
		t.Fatal("reviewer total overflow accepted")
	}
}
