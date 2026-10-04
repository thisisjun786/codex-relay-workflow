package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

// The reading of the child's independentReview item is advice for the parent. It is graded beside
// the restatement and never in it: whatever it finds, the restatement's own problems, current, the
// verdict and the exit code are what they would be without it.

func reviewArtifact(t *testing.T, head string) []byte {
	t.Helper()
	f := review.Finding{File: "a.go", Line: 1, Title: "t", Explanation: "e", Severity: "high", Grade: review.P1, Perspective: "correctness", Reviewers: []int{0}, Support: 1, Verdict: review.VerdictConfirmed}
	a := review.Artifact{Schema: review.SchemaV1, Tool: "crw review", ToolVersion: "1", Model: "m", Effort: "high", AgyVersion: "unknown", Base: strings.Repeat("e", 40), Head: head, PatchID: strings.Repeat("c", 40),
		Reviewers: review.ReviewerCounts{Run: 1}, Status: review.StatusComplete, StartedAt: "2026-10-04T00:00:00Z", FinishedAt: "2026-10-04T00:01:00Z", Findings: []review.Finding{f}}
	data, err := a.Marshal(head)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// reviewRecord is a receipt of the pull request the scripted forge shows, with the item (when given)
// among its members.
func reviewRecord(t *testing.T, item map[string]any) string {
	t.Helper()
	_, first := runMerge(t, scriptedForge{})
	if item != nil {
		first["handoff"].(map[string]any)[reviewMember] = item
	}
	return lateWrite(t, "record.json", first)
}

const reviewMember = "independentReview"

func reviewItem(t *testing.T, head string, sha func(string) string) map[string]any {
	t.Helper()
	data := reviewArtifact(t, head)
	path := filepath.Join(t.TempDir(), "artifact.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return map[string]any{"artifact": map[string]any{"path": path, "sha256": sha(hex.EncodeToString(sum[:]))}, "status": "complete", "invalidReviewerCalls": 0,
		"dispositions": []any{map[string]any{"finding": 0, "disposition": "fixed", "evidence": "commit abc123"}}}
}

func reviewWarnings(t *testing.T, p map[string]any) []string {
	t.Helper()
	section, ok := p[reviewMember].(map[string]any)
	if !ok {
		t.Fatalf("no %s in the payload: %v", reviewMember, p)
	}
	var codes []string
	for _, raw := range section["warnings"].([]any) {
		codes = append(codes, strings.TrimPrefix(raw.(map[string]any)["code"].(string), "independent_review_"))
	}
	return codes
}

func TestMergeEvidenceIndependentReview(t *testing.T) {
	same := func(s string) string { return s }
	t.Run("a record that states no item and no flag leaves the payload as it was", func(t *testing.T) {
		code, p := runMerge(t, scriptedForge{}, "--restate", reviewRecord(t, nil))
		if _, given := p[reviewMember]; given || code != 0 || p["restatement"].(map[string]any)["current"] != true {
			t.Fatal(code, p)
		}
	})
	t.Run("the flag asks for a warning where the record states no item", func(t *testing.T) {
		code, p := runMerge(t, scriptedForge{}, "--restate", reviewRecord(t, nil), "--expect-independent-review")
		if code != 0 || p["restatement"].(map[string]any)["current"] != true || strings.Join(reviewWarnings(t, p), ",") != "absent" || p[reviewMember].(map[string]any)["stated"] != false {
			t.Fatal(code, p)
		}
	})
	t.Run("an item that matches its artifact says nothing", func(t *testing.T) {
		code, p := runMerge(t, scriptedForge{}, "--restate", reviewRecord(t, reviewItem(t, lateHead, same)))
		if code != 0 || len(reviewWarnings(t, p)) != 0 || p[reviewMember].(map[string]any)["stated"] != true {
			t.Fatal(code, p)
		}
	})
	t.Run("warnings change neither current, the problems, the verdict nor the exit code", func(t *testing.T) {
		wrongSHA := func(string) string { return strings.Repeat("0", 64) }
		item := reviewItem(t, lateOtherHead, wrongSHA)
		item["dispositions"] = []any{}
		code, p := runMerge(t, scriptedForge{}, "--restate", reviewRecord(t, item))
		rest := p["restatement"].(map[string]any)
		if code != 0 || rest["current"] != true || len(rest["problems"].([]any)) != 0 || p["verdict"] != "ready" || strings.Join(reviewWarnings(t, p), ",") != "sha256_mismatch" {
			t.Fatal(code, p)
		}
		withoutItem, q := runMerge(t, scriptedForge{}, "--restate", reviewRecord(t, nil))
		if withoutItem != code || q["verdict"] != p["verdict"] {
			t.Fatal("the same record without the item answers differently", withoutItem, q["verdict"])
		}
	})
	t.Run("the item is graded against the head the record is about", func(t *testing.T) {
		code, p := runMerge(t, scriptedForge{}, "--restate", reviewRecord(t, reviewItem(t, lateOtherHead, same)))
		if code != 0 || strings.Join(reviewWarnings(t, p), ",") != "head_differs" {
			t.Fatal(code, p)
		}
	})
	t.Run("the flag grades a restated record and needs one", func(t *testing.T) {
		var out, stderr bytes.Buffer
		code := Execute(context.Background(), []string{"merge-evidence", "--repository", "owner/repo", "--pull-request", "7", "--expect-independent-review"}, &out, &stderr)
		if code != 4 || !strings.Contains(stderr.String(), "--restate") {
			t.Fatal(code, out.String(), stderr.String())
		}
	})
}
