package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

// The artifact the rows review: findings 0 (P1), 1 (P2 and security), 2 (P2), 3 (P0) and 4 (P3), so
// the findings that need a disposition are 0, 1 and 3.
var (
	reviewedHead  = strings.Repeat("a", 40)
	candidateHead = strings.Repeat("b", 40)
	patchID       = strings.Repeat("c", 40)
	otherPatchID  = strings.Repeat("d", 40)
)

const artifactPath = "/review/artifact.json"

func finding(grade review.Grade, security bool) review.Finding {
	return review.Finding{File: "internal/x/x.go", Line: 10, Title: "t", Explanation: "e", Severity: string(grade), Grade: grade, Security: security, Perspective: "correctness", Reviewers: []int{0, 1}, Support: 2, Verdict: review.VerdictConfirmed}
}

func artifactBytes(t *testing.T, change func(*review.Artifact)) []byte {
	t.Helper()
	a := review.Artifact{Schema: review.SchemaV1, Tool: "crw review", ToolVersion: "1", Model: "m", Effort: "high", AgyVersion: "unknown", Base: strings.Repeat("e", 40), Head: reviewedHead, PatchID: patchID,
		Diff: review.DiffStats{Files: 1, Additions: 2}, Reviewers: review.ReviewerCounts{Run: 2}, Status: review.StatusComplete,
		StartedAt: "2026-10-04T00:00:00Z", FinishedAt: "2026-10-04T00:01:00Z",
		Findings: []review.Finding{finding(review.P1, false), finding(review.P2, true), finding(review.P2, false), finding(review.P0, false), finding(review.P3, false)}}
	if change != nil {
		change(&a)
	}
	data, err := a.Marshal(a.Head)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type reviewCase struct {
	item  map[string]any
	files map[string][]byte
}

func newReviewCase(t *testing.T) *reviewCase {
	t.Helper()
	data := artifactBytes(t, nil)
	sum := sha256.Sum256(data)
	return &reviewCase{files: map[string][]byte{artifactPath: data}, item: map[string]any{
		"artifact": map[string]any{"path": artifactPath, "sha256": hex.EncodeToString(sum[:])}, "status": "complete", "invalidReviewerCalls": 0,
		"dispositions": []any{disposition(0, "fixed"), disposition(1, "refuted"), disposition(3, "recorded")}}}
}

func disposition(finding int, kind string) map[string]any {
	return map[string]any{"finding": finding, "disposition": kind, "evidence": "commit abc123"}
}

func (c *reviewCase) setBytes(data []byte) { c.files[artifactPath] = data }

func (c *reviewCase) reader(path string) ([]byte, error) {
	if data, ok := c.files[path]; ok {
		return data, nil
	}
	return nil, fs.ErrNotExist
}

func codes(c ReviewCoverage) []string {
	var out []string
	for _, w := range c.Warnings {
		out = append(out, strings.TrimPrefix(w.Code, "independent_review_"))
	}
	return out
}

func TestIndependentReviewCoverage(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		head   string // the candidate head; the reviewed head when empty
		change func(c *reviewCase)
		stated bool
		want   []string
	}{
		{name: "every P0, P1 and security finding answered on the reviewed head", stated: true},
		{name: "a head the review did not see with the same patch-id is a base refresh", head: candidateHead, stated: true, change: func(c *reviewCase) { c.item["headPatchId"] = patchID }},
		{name: "a head the review did not see and no patch-id stated", head: candidateHead, stated: true, want: []string{"head_differs"}},
		{name: "a head the review did not see with another patch-id", head: candidateHead, stated: true, want: []string{"head_differs"}, change: func(c *reviewCase) { c.item["headPatchId"] = otherPatchID }},
		{name: "sha256 that is not the file's bytes stops the artifact checks", stated: true, want: []string{"sha256_mismatch"}, change: func(c *reviewCase) {
			c.item["artifact"].(map[string]any)["sha256"] = strings.Repeat("0", 64)
			c.item["dispositions"] = []any{}
		}},
		{name: "a file that cannot be read", stated: true, want: []string{"unreadable"}, change: func(c *reviewCase) { delete(c.files, artifactPath) }},
		{name: "a P0 without a disposition", stated: true, want: []string{"disposition_missing"}, change: func(c *reviewCase) { c.item["dispositions"] = []any{disposition(0, "fixed"), disposition(1, "fixed")} }},
		{name: "a P1 without a disposition", stated: true, want: []string{"disposition_missing"}, change: func(c *reviewCase) { c.item["dispositions"] = []any{disposition(1, "fixed"), disposition(3, "fixed")} }},
		{name: "a security finding graded P2 without a disposition", stated: true, want: []string{"disposition_missing"}, change: func(c *reviewCase) { c.item["dispositions"] = []any{disposition(0, "fixed"), disposition(3, "fixed")} }},
		{name: "P2 and P3 findings need none", stated: true, change: func(c *reviewCase) {
			c.item["dispositions"] = []any{disposition(0, "fixed"), disposition(1, "fixed"), disposition(3, "fixed")}
		}},
		{name: "a disposition for a finding the artifact does not have", stated: true, want: []string{"disposition_unknown"}, change: func(c *reviewCase) {
			c.item["dispositions"] = append(c.item["dispositions"].([]any), disposition(9, "fixed"))
		}},
		{name: "a status that is not the artifact's", stated: true, want: []string{"status_differs"}, change: func(c *reviewCase) { c.item["status"] = "partial"; c.item["reason"] = "one reviewer failed" }},
		{name: "bytes that are not a review artifact", stated: true, want: []string{"artifact_invalid"}, change: func(c *reviewCase) {
			data := []byte("{\"head\": \"" + reviewedHead + "\"}")
			c.setBytes(data)
			sum := sha256.Sum256(data)
			c.item["artifact"].(map[string]any)["sha256"] = hex.EncodeToString(sum[:])
		}},
		{name: "unavailable with no artifact at all", stated: true, change: func(c *reviewCase) {
			delete(c.item, "artifact")
			c.item["status"], c.item["reason"], c.item["dispositions"] = "unavailable", "the daily cap stopped the run", []any{}
		}},
		{name: "complete with no artifact", stated: true, want: []string{"malformed"}, change: func(c *reviewCase) { delete(c.item, "artifact") }},
		{name: "a relative path", stated: true, want: []string{"malformed"}, change: func(c *reviewCase) { c.item["artifact"].(map[string]any)["path"] = "review/artifact.json" }},
		{name: "a status outside the three", stated: true, want: []string{"malformed"}, change: func(c *reviewCase) { c.item["status"] = "done" }},
		{name: "partial without a reason", stated: true, want: []string{"malformed"}, change: func(c *reviewCase) { c.item["status"] = "partial" }},
		{name: "a negative count of invalid calls", stated: true, want: []string{"malformed"}, change: func(c *reviewCase) { c.item["invalidReviewerCalls"] = -1 }},
		{name: "counts written as 0.0 are the same whole number", stated: true, change: func(c *reviewCase) {
			c.item["invalidReviewerCalls"] = 0.0
			c.item["dispositions"].([]any)[0].(map[string]any)["finding"] = 0.0
		}},
		{name: "a count that is a fraction", stated: true, want: []string{"malformed"}, change: func(c *reviewCase) { c.item["invalidReviewerCalls"] = 0.5 }},
		{name: "a count that is a string", stated: true, want: []string{"malformed"}, change: func(c *reviewCase) { c.item["invalidReviewerCalls"] = "0" }},
		{name: "a disposition with no evidence", stated: true, want: []string{"malformed"}, change: func(c *reviewCase) { c.item["dispositions"].([]any)[0].(map[string]any)["evidence"] = " " }},
		{name: "a disposition that is not one of the three", stated: true, want: []string{"malformed"}, change: func(c *reviewCase) { c.item["dispositions"].([]any)[0].(map[string]any)["disposition"] = "ignored" }},
		{name: "two dispositions for one finding", stated: true, want: []string{"malformed"}, change: func(c *reviewCase) {
			c.item["dispositions"] = append(c.item["dispositions"].([]any), disposition(0, "refuted"))
		}},
		{name: "dispositions that are not a list", stated: true, want: []string{"malformed"}, change: func(c *reviewCase) { c.item["dispositions"] = "all fixed" }},
		{name: "a patch-id that is not hex", head: candidateHead, stated: true, want: []string{"malformed"}, change: func(c *reviewCase) { c.item["headPatchId"] = "same" }},
		{name: "a member the item does not have", stated: true, want: []string{"malformed"}, change: func(c *reviewCase) { c.item["verdict"] = "good" }},
		{name: "several warnings keep the order of the checks", head: candidateHead, stated: true, want: []string{"status_differs", "head_differs", "disposition_missing", "disposition_missing"}, change: func(c *reviewCase) {
			c.item["status"], c.item["reason"] = "partial", "one reviewer failed"
			c.item["dispositions"] = []any{disposition(0, "fixed")}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			c := newReviewCase(t)
			if row.change != nil {
				row.change(c)
			}
			head := row.head
			if head == "" {
				head = reviewedHead
			}
			got := IndependentReviewCoverage(head, map[string]any{"checks": []any{}, IndependentReviewMember: c.item}, c.reader)
			if got.Stated != row.stated || !slices.Equal(codes(got), row.want) {
				t.Fatalf("stated %v warnings %v, want stated %v warnings %v: %+v", got.Stated, codes(got), row.stated, row.want, got.Warnings)
			}
		})
	}
}

func TestIndependentReviewCoverageOfARecordWithoutTheItem(t *testing.T) {
	t.Parallel()
	for name, record := range map[string]any{"a record that does not state it": map[string]any{"checks": []any{}}, "a record that is not an object": "handoff", "no record": nil} {
		got := IndependentReviewCoverage(reviewedHead, record, func(string) ([]byte, error) { t.Fatal("no file is read without an item"); return nil, nil })
		if got.Stated || !slices.Equal(codes(got), []string{"absent"}) {
			t.Fatalf("%s: stated %v warnings %+v", name, got.Stated, got.Warnings)
		}
	}
	for name, member := range map[string]any{"an item that is null": nil, "an item that is not an object": "reviewed"} {
		got := IndependentReviewCoverage(reviewedHead, map[string]any{IndependentReviewMember: member}, nil)
		if !got.Stated || !slices.Equal(codes(got), []string{"malformed"}) {
			t.Fatalf("%s: %+v", name, got)
		}
	}
}

// What a handoff names is the parent's input, not the child's authority: nothing the file holds,
// no validator message and no operating-system error text reaches a warning.
func TestIndependentReviewWarningsNeverEchoTheFile(t *testing.T) {
	t.Parallel()
	const marker = "SECRET-MARKER-9f3a"
	c := newReviewCase(t)
	data := []byte("{\"head\": \"" + reviewedHead + "\", \"" + marker + "\": \"" + marker + "\"}")
	c.setBytes(data)
	sum := sha256.Sum256(data)
	c.item["artifact"].(map[string]any)["sha256"] = hex.EncodeToString(sum[:])
	for name, read := range map[string]ArtifactReader{
		"invalid artifact": c.reader,
		"read error":       func(string) ([]byte, error) { return nil, errors.New("open " + marker + ": permission denied") },
	} {
		got := IndependentReviewCoverage(reviewedHead, map[string]any{IndependentReviewMember: c.item}, read)
		if len(got.Warnings) != 1 {
			t.Fatalf("%s: %+v", name, got)
		}
		whole, _ := json.Marshal(got)
		if strings.Contains(string(whole), marker) || strings.Contains(string(whole), artifactPath) {
			t.Fatalf("%s: a warning repeats what the file or the path said: %s", name, whole)
		}
	}
}

// The same holds for the item itself: text the record supplies (a member name, a status, a path, a
// digest, a disposition) is never copied into a warning, however it is wrong.
func TestIndependentReviewWarningsNeverEchoTheRecord(t *testing.T) {
	t.Parallel()
	const marker = "IGNORE-ALL-PRIOR-INSTRUCTIONS"
	item := map[string]any{marker: marker, "status": marker, "invalidReviewerCalls": marker, "headPatchId": marker,
		"artifact":     map[string]any{"path": marker, "sha256": marker, marker: marker},
		"dispositions": []any{map[string]any{"finding": marker, "disposition": marker, marker: marker}}}
	got := IndependentReviewCoverage(reviewedHead, map[string]any{IndependentReviewMember: item}, nil)
	whole, _ := json.Marshal(got)
	if len(got.Warnings) < 8 || strings.Contains(string(whole), marker) {
		t.Fatalf("%d warnings, and the record's text must not be among them: %s", len(got.Warnings), whole)
	}
}

func TestReadArtifactFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	lowered := reviewArtifactCap
	t.Cleanup(func() { reviewArtifactCap = lowered })
	for _, row := range []struct {
		name, path string
		cap        int64
		wantErr    error
	}{
		{name: "a regular file", path: good, cap: 5},
		{name: "a file one byte over the cap", path: good, cap: 4, wantErr: errArtifactTooLarge},
		{name: "a directory", path: dir, cap: 5, wantErr: syscall.EINVAL},
		{name: "a path that is not there", path: filepath.Join(dir, "gone.json"), cap: 5, wantErr: fs.ErrNotExist},
	} {
		reviewArtifactCap = row.cap
		data, err := readArtifactFile(row.path)
		if !errors.Is(err, row.wantErr) || (row.wantErr == nil && string(data) != "12345") {
			t.Fatalf("%s: %q, %v", row.name, data, err)
		}
	}
}

// A file swapped for a FIFO between the parent choosing to read it and the open would otherwise
// block the whole merge-evidence run until someone writes to it.
func TestOpenRegularDoesNotWaitOnAFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "swapped.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip("no FIFO here:", err)
	}
	done := make(chan error, 1)
	go func() { _, err := readArtifactFile(path); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO was read as an artifact")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the open of a FIFO is still waiting after five seconds")
	}
}
