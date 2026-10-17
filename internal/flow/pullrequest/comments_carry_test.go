package pullrequest

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

func collectCarryComments(t *testing.T, src *stubCommentSource, in CommentsInput) tracking.PRReviewRecord {
	t.Helper()
	rec, err := Comments(context.Background(), src, in)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestCommentsCarryDispositionWhenInlineLineMoves(t *testing.T) {
	for _, tc := range []struct {
		name, disposition string
		line              int
		legacy            bool
	}{
		{"applied", tracking.DispositionApplied, 25, false},
		{"rejected", tracking.DispositionRejected, 25, false},
		{"outdated line", tracking.DispositionApplied, 0, false},
		{"legacy record", tracking.DispositionRejected, 25, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "review.json")
			src := openCommentSource()
			src.comments = []tracking.PRReviewComment{{ID: "inline:9", Bot: "codex", Body: "keep the check\r\n", Path: "a.go", Line: 10}}
			const legacyDigest = "0d6c20237f080158048736aba725b23154d67befafb4aff76e5e327e155ebff1"
			if tc.legacy {
				prev := tracking.PRReviewRecord{Schema: "crw-pr-review/1", PR: "o/r#7", Head: cmHead, Bots: "connected", Round: 1, State: "complete",
					Items: []tracking.PRReviewItem{{Key: legacyDigest, Comments: append([]tracking.PRReviewComment(nil), src.comments...), Disposition: tc.disposition, Reason: "checked against the contract"}}}
				if err := writeJSONRecord(record, prev); err != nil {
					t.Fatal(err)
				}
			} else {
				rec := collectCarryComments(t, src, commentsIn(record, "connected", 1))
				if len(rec.Items) != 1 || rec.Items[0].Key != legacyDigest {
					t.Fatalf("initial record: %+v", rec)
				}
				collectCarryComments(t, src, commentsIn(record, "connected", 1, CommentDisposition{Key: legacyDigest, Disposition: tc.disposition, Reason: "checked against the contract"}))
			}
			src.pr.HeadSHA = strings.Repeat("e", 40)
			src.comments[0].Line = tc.line
			src.comments[0].Body = "keep the check"
			in := commentsIn(record, "connected", 2)
			in.Head = src.pr.HeadSHA
			rec := collectCarryComments(t, src, in)
			if rec.Schema != "crw-pr-review/1" || rec.State != "complete" || rec.Round != 2 || len(rec.Items) != 1 {
				t.Fatalf("line-only relocation lost the decision: %+v", rec)
			}
			it := rec.Items[0]
			if it.Key != legacyDigest || it.Disposition != tc.disposition || it.Reason != "checked against the contract" || !reflect.DeepEqual(it.Comments, src.comments) {
				t.Fatalf("relocated item: %+v; current comments %+v", it, src.comments)
			}
			src.comments[0].Line = tc.line + 5
			in.Dispositions = []CommentDisposition{{Key: legacyDigest, Disposition: tc.disposition, Reason: "same finding after refresh"}}
			rec = collectCarryComments(t, src, in)
			if len(rec.Items) != 1 || rec.Items[0].Key != legacyDigest || rec.Items[0].Reason != "same finding after refresh" || !reflect.DeepEqual(rec.Items[0].Comments, src.comments) {
				t.Fatalf("same-round refresh lost the original key or metadata: %+v", rec)
			}
			disk, err := tracking.ReadPRReviewRecord(record, src.pr.Ref, src.pr.HeadSHA)
			if err != nil || !reflect.DeepEqual(disk, rec) {
				t.Fatalf("persisted record: %+v, %v; want %+v", disk, err, rec)
			}
			if n, err := disk.Unresolved(); err != nil || n != 0 {
				t.Fatalf("unresolved = %d, %v; want zero", n, err)
			}
		})
	}
}

func TestCommentsCarryDoesNotReuseDecisionForChangedFinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*tracking.PRReviewComment)
	}{
		{"edited body", func(c *tracking.PRReviewComment) { c.Body = "a different finding" }},
		{"different file", func(c *tracking.PRReviewComment) { c.Path = "b.go" }},
		{"new source at another line", func(c *tracking.PRReviewComment) { c.ID = "inline:10" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "review.json")
			src := openCommentSource()
			src.comments = []tracking.PRReviewComment{{ID: "inline:9", Bot: "codex", Body: "keep the check", Path: "a.go", Line: 10}}
			rec := collectCarryComments(t, src, commentsIn(record, "connected", 1))
			collectCarryComments(t, src, commentsIn(record, "connected", 1, CommentDisposition{Key: rec.Items[0].Key, Disposition: "applied", Reason: "fixed"}))
			src.comments[0].Line = 25
			tc.change(&src.comments[0])
			rec = collectCarryComments(t, src, commentsIn(record, "connected", 2))
			if rec.State != "round_limit" {
				t.Fatalf("a changed finding inherited a decision: %+v", rec)
			}
			found := 0
			for _, it := range rec.Items {
				for _, c := range it.Comments {
					if c.ID == src.comments[0].ID {
						found++
						if it.Disposition != "" || it.Reason != "" {
							t.Fatalf("changed item kept a decision: %+v", it)
						}
					}
				}
			}
			if found != 1 {
				t.Fatalf("changed source appeared %d times: %+v", found, rec)
			}
			if n, err := rec.Unresolved(); err != nil || n != 1 {
				t.Fatalf("unresolved = %d, %v; want one", n, err)
			}
		})
	}
}

func TestCommentsCarryDistinctDispositionsAcrossLineSwap(t *testing.T) {
	record := filepath.Join(t.TempDir(), "review.json")
	src := openCommentSource()
	src.comments = []tracking.PRReviewComment{
		{ID: "inline:9", Bot: "codex", Body: "keep the check", Path: "a.go", Line: 10},
		{ID: "inline:10", Bot: "devin", Body: "keep the check", Path: "a.go", Line: 20},
	}
	rec := collectCarryComments(t, src, commentsIn(record, "connected", 1))
	if len(rec.Items) != 2 {
		t.Fatalf("initial items: %+v", rec.Items)
	}
	want := map[string]tracking.PRReviewItem{}
	var dispositions []CommentDisposition
	for i, it := range rec.Items {
		disposition := []string{"applied", "rejected"}[i]
		it.Disposition, it.Reason = disposition, "decision for "+it.Comments[0].ID
		want[it.Comments[0].ID] = it
		dispositions = append(dispositions, CommentDisposition{Key: it.Key, Disposition: it.Disposition, Reason: it.Reason})
	}
	collectCarryComments(t, src, commentsIn(record, "connected", 1, dispositions...))
	src.comments[0], src.comments[1] = src.comments[1], src.comments[0]
	src.comments[0].Line, src.comments[1].Line = 10, 20
	rec = collectCarryComments(t, src, commentsIn(record, "connected", 2))
	if rec.State != "complete" || len(rec.Items) != 2 {
		t.Fatalf("line swap: %+v", rec)
	}
	for _, it := range rec.Items {
		if len(it.Comments) != 1 {
			t.Fatalf("distinct sources were merged: %+v", it)
		}
		expected, ok := want[it.Comments[0].ID]
		if !ok || it.Key != expected.Key || it.Disposition != expected.Disposition || it.Reason != expected.Reason {
			t.Fatalf("source got another decision: %+v; want %+v", it, expected)
		}
	}
}
