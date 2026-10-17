package pullrequest

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

func identity1605Item(t *testing.T, rec tracking.PRReviewRecord, id string) tracking.PRReviewItem {
	t.Helper()
	found := 0
	var item tracking.PRReviewItem
	for _, it := range rec.Items {
		for _, c := range it.Comments {
			if c.ID == id {
				found++
				item = it
			}
		}
	}
	if found != 1 {
		t.Fatalf("source %s appears %d times: %+v", id, found, rec)
	}
	return item
}

const identity1605CollisionDigest = "d3a10e83c39c577299a6b44921e1b3a38c9ae1bbb7421e0835ccb964b7523169"

func TestCommentsIdentity1605OriginalLineReuseStaysUnresolved(t *testing.T) {
	for _, disposition := range []string{"applied", "rejected"} {
		for _, newFirst := range []bool{false, true} {
			for _, priorRefresh := range []bool{false, true} {
				name := disposition
				if newFirst {
					name += "/new-first"
				} else {
					name += "/old-first"
				}
				if priorRefresh {
					name += "/already-relocated"
				} else {
					name += "/same-collection"
				}
				t.Run(name, func(t *testing.T) {
					record := filepath.Join(t.TempDir(), "review.json")
					src := openCommentSource()
					original := tracking.PRReviewComment{ID: "inline:9", Bot: "codex", Body: "keep the check", Path: "a.go", Line: 10}
					src.comments = []tracking.PRReviewComment{original}
					rec := collectCarryComments(t, src, commentsIn(record, "connected", 1))
					originalKey := identity1605Item(t, rec, original.ID).Key
					collectCarryComments(t, src, commentsIn(record, "connected", 1, CommentDisposition{Key: originalKey, Disposition: disposition, Reason: "original decision"}))
					original.Line = 25
					if priorRefresh {
						src.comments = []tracking.PRReviewComment{original}
						collectCarryComments(t, src, commentsIn(record, "connected", 1))
					}
					fresh := tracking.PRReviewComment{ID: "inline:10", Bot: "devin", Body: "keep the check", Path: "a.go", Line: 10}
					duplicate := fresh
					duplicate.ID, duplicate.Bot, duplicate.Body = "inline:11", "codex", "keep the check\r\n"
					src.comments = []tracking.PRReviewComment{original, fresh, duplicate}
					if newFirst {
						src.comments = []tracking.PRReviewComment{fresh, duplicate, original}
					}
					in := commentsIn(record, "connected", 2, CommentDisposition{Key: originalKey, Disposition: disposition, Reason: "reconfirmed original"})
					rec = collectCarryComments(t, src, in)
					if rec.State != "round_limit" || len(rec.Items) != 2 {
						t.Fatalf("new finding was absorbed: %+v", rec)
					}
					oldItem, newItem := identity1605Item(t, rec, original.ID), identity1605Item(t, rec, fresh.ID)
					if oldItem.Disposition != disposition || oldItem.Reason != "reconfirmed original" || !reflect.DeepEqual(oldItem.Comments, []tracking.PRReviewComment{original}) {
						t.Fatalf("original decision or location lost: %+v", oldItem)
					}
					if oldItem.Key != originalKey || newItem.Key != identity1605CollisionDigest || newItem.Disposition != "" || newItem.Reason != "" || len(newItem.Comments) != 2 || identity1605Item(t, rec, duplicate.ID).Key != newItem.Key {
						t.Fatalf("new finding inherited a decision or failed dedup: old=%+v new=%+v", oldItem, newItem)
					}
					if n, err := rec.Unresolved(); err != nil || n != 1 {
						t.Fatalf("unresolved = %d, %v; want one", n, err)
					}
					disk, err := tracking.ReadPRReviewRecord(record, src.pr.Ref, src.pr.HeadSHA)
					if err != nil || !reflect.DeepEqual(disk, rec) {
						t.Fatalf("invalid persisted record: %+v %v", disk, err)
					}
					refreshed := collectCarryComments(t, src, commentsIn(record, "connected", 2))
					if !reflect.DeepEqual(refreshed, rec) {
						t.Fatalf("refresh changed identities: %+v; want %+v", refreshed, rec)
					}
					rec = collectCarryComments(t, src, commentsIn(record, "connected", 2, CommentDisposition{Key: newItem.Key, Disposition: "applied", Reason: "separately fixed"}))
					if rec.State != "complete" || identity1605Item(t, rec, fresh.ID).Reason != "separately fixed" || identity1605Item(t, rec, original.ID).Reason != "reconfirmed original" {
						t.Fatalf("separate decision reached wrong item: %+v", rec)
					}
					rec = collectCarryComments(t, src, in)
					if rec.State != "complete" || identity1605Item(t, rec, fresh.ID).Reason != "separately fixed" || identity1605Item(t, rec, original.ID).Reason != "reconfirmed original" {
						t.Fatalf("replaying the old decision replaced the new finding's separate decision: %+v", rec)
					}
				})
			}
		}
	}
}

func TestCommentsIdentity1605DeduplicatesAtCurrentLocation(t *testing.T) {
	for _, newFirst := range []bool{false, true} {
		name := "old-first"
		if newFirst {
			name = "new-first"
		}
		t.Run(name, func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "review.json")
			src := openCommentSource()
			original := tracking.PRReviewComment{ID: "inline:9", Bot: "codex", Body: "keep the check", Path: "a.go", Line: 10}
			src.comments = []tracking.PRReviewComment{original}
			rec := collectCarryComments(t, src, commentsIn(record, "connected", 1))
			originalKey := rec.Items[0].Key
			collectCarryComments(t, src, commentsIn(record, "connected", 1, CommentDisposition{Key: originalKey, Disposition: "applied", Reason: "fixed"}))
			original.Line = 25
			duplicate := original
			duplicate.ID, duplicate.Bot = "inline:10", "devin"
			src.comments = []tracking.PRReviewComment{original, duplicate}
			if newFirst {
				src.comments = []tracking.PRReviewComment{duplicate, original}
			}
			rec = collectCarryComments(t, src, commentsIn(record, "connected", 2))
			if rec.State != "complete" || len(rec.Items) != 1 || rec.Items[0].Key != originalKey || rec.Items[0].Disposition != "applied" || len(rec.Items[0].Comments) != 2 {
				t.Fatalf("current-location duplicate became a new finding: %+v", rec)
			}
			if n, err := rec.Unresolved(); err != nil || n != 0 {
				t.Fatalf("unresolved = %d, %v", n, err)
			}
		})
	}
}

func TestCommentsIdentity1605OccupiedKeysDoNotMove(t *testing.T) {
	record := filepath.Join(t.TempDir(), "review.json")
	src := openCommentSource()
	src.comments = []tracking.PRReviewComment{
		{ID: "inline:9", Bot: "codex", Body: "keep the check", Path: "a.go", Line: 10},
		{ID: "inline:10", Bot: "devin", Body: "keep the check", Path: "a.go", Line: 20},
	}
	rec := collectCarryComments(t, src, commentsIn(record, "connected", 1))
	first := identity1605Item(t, rec, "inline:9")
	second := identity1605Item(t, rec, "inline:10")
	collectCarryComments(t, src, commentsIn(record, "connected", 1,
		CommentDisposition{Key: first.Key, Disposition: "applied", Reason: "first decision"},
		CommentDisposition{Key: second.Key, Disposition: "rejected", Reason: "second decision"}))
	src.comments[0].Line, src.comments[1].Line = 20, 30
	src.comments = append(src.comments, tracking.PRReviewComment{ID: "inline:11", Bot: "codex", Body: "keep the check", Path: "a.go", Line: 10})
	rec = collectCarryComments(t, src, commentsIn(record, "connected", 2))
	if len(rec.Items) != 3 || rec.State != "round_limit" {
		t.Fatalf("key chain: %+v", rec)
	}
	a, b, fresh := identity1605Item(t, rec, "inline:9"), identity1605Item(t, rec, "inline:10"), identity1605Item(t, rec, "inline:11")
	if a.Disposition != "applied" || a.Reason != "first decision" || b.Disposition != "rejected" || b.Reason != "second decision" || fresh.Disposition != "" || fresh.Reason != "" || fresh.Key == first.Key || fresh.Key == second.Key || a.Key != first.Key || b.Key != second.Key {
		t.Fatalf("occupied keys changed decisions: %+v", rec)
	}
	if n, err := rec.Unresolved(); err != nil || n != 1 {
		t.Fatalf("unresolved = %d, %v", n, err)
	}
}

func TestCommentsIdentity1605ConvergedLocationsKeepDistinctIdentities(t *testing.T) {
	record := filepath.Join(t.TempDir(), "review.json")
	src := openCommentSource()
	src.comments = []tracking.PRReviewComment{
		{ID: "inline:9", Bot: "codex", Body: "keep the check", Path: "a.go", Line: 10},
		{ID: "inline:10", Bot: "devin", Body: "keep the check", Path: "a.go", Line: 20},
	}
	rec := collectCarryComments(t, src, commentsIn(record, "connected", 1))
	first, second := identity1605Item(t, rec, "inline:9"), identity1605Item(t, rec, "inline:10")
	collectCarryComments(t, src, commentsIn(record, "connected", 1,
		CommentDisposition{Key: first.Key, Disposition: "applied", Reason: "first decision"},
		CommentDisposition{Key: second.Key, Disposition: "rejected", Reason: "second decision"}))
	src.comments[0].Line = 20
	src.comments = append(src.comments, tracking.PRReviewComment{ID: "inline:11", Bot: "codex", Body: "keep the check", Path: "a.go", Line: 10})
	rec = collectCarryComments(t, src, commentsIn(record, "connected", 2))
	a, b, fresh := identity1605Item(t, rec, "inline:9"), identity1605Item(t, rec, "inline:10"), identity1605Item(t, rec, "inline:11")
	if len(rec.Items) != 3 || rec.State != "round_limit" || a.Key != first.Key || a.Disposition != "applied" || b.Key != second.Key || b.Disposition != "rejected" || fresh.Key == a.Key || fresh.Key == b.Key || fresh.Disposition != "" {
		t.Fatalf("converged locations lost identities or decided a new finding: %+v", rec)
	}
	if n, err := rec.Unresolved(); err != nil || n != 1 {
		t.Fatalf("unresolved = %d, %v", n, err)
	}
}

func TestCommentsIdentity1605OccupiedFallbackDoesNotOverwriteRecord(t *testing.T) {
	record := filepath.Join(t.TempDir(), "review.json")
	src := openCommentSource()
	original := tracking.PRReviewComment{ID: "inline:9", Bot: "codex", Body: "keep the check", Path: "a.go", Line: 10}
	src.comments = []tracking.PRReviewComment{original}
	rec := collectCarryComments(t, src, commentsIn(record, "connected", 1))
	rec = collectCarryComments(t, src, commentsIn(record, "connected", 1, CommentDisposition{Key: rec.Items[0].Key, Disposition: "applied", Reason: "fixed original"}))
	rec.Items = append(rec.Items, tracking.PRReviewItem{Key: identity1605CollisionDigest, Comments: []tracking.PRReviewComment{{ID: "review:2", Bot: "devin", Body: "a separate retained finding"}}, Disposition: "rejected", Reason: "separate decision"})
	if err := writeJSONRecord(record, rec); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	original.Line = 25
	src.comments = []tracking.PRReviewComment{original, {ID: "inline:10", Bot: "devin", Body: "keep the check", Path: "a.go", Line: 10}}
	_, err = Comments(context.Background(), src, commentsIn(record, "connected", 2))
	if err == nil || !strings.Contains(err.Error(), "collides with an existing item") {
		t.Fatalf("occupied fallback = %v; want a safe refusal", err)
	}
	after, err := os.ReadFile(record)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("refused collection overwrote prior record: %v", err)
	}
}

func TestCommentsIdentity1605DroppedKeyIsNotReusedInCollection(t *testing.T) {
	record := filepath.Join(t.TempDir(), "review.json")
	src := openCommentSource()
	original := tracking.PRReviewComment{ID: "inline:9", Bot: "codex", Body: "keep the check", Path: "a.go", Line: 10}
	src.comments = []tracking.PRReviewComment{original}
	rec := collectCarryComments(t, src, commentsIn(record, "connected", 1))
	originalKey := rec.Items[0].Key
	collectCarryComments(t, src, commentsIn(record, "connected", 1, CommentDisposition{Key: originalKey, Disposition: "applied", Reason: "old decision"}))
	original.Body = "a changed finding"
	fresh := tracking.PRReviewComment{ID: "inline:10", Bot: "devin", Body: "keep the check", Path: "a.go", Line: 10}
	src.comments = []tracking.PRReviewComment{fresh, original}
	rec = collectCarryComments(t, src, commentsIn(record, "connected", 2))
	if len(rec.Items) != 2 || rec.State != "round_limit" {
		t.Fatalf("changed and fresh findings: %+v", rec)
	}
	for _, it := range rec.Items {
		if it.Key == originalKey || it.Disposition != "" {
			t.Fatalf("dropped key reused or decision inherited: %+v", it)
		}
	}
	in := commentsIn(record, "connected", 2, CommentDisposition{Key: originalKey, Disposition: "applied", Reason: "old decision"})
	if _, err := Comments(context.Background(), src, in); err == nil {
		t.Fatal("a stale disposition for the dropped item was accepted")
	}
}

func TestCommentsIdentity1605ReplayingDispositionsDoesNotDecideANewFinding(t *testing.T) {
	record := filepath.Join(t.TempDir(), "review.json")
	src := openCommentSource()
	original := tracking.PRReviewComment{ID: "inline:9", Bot: "codex", Body: "keep the check", Path: "a.go", Line: 10}
	src.comments = []tracking.PRReviewComment{original}
	rec := collectCarryComments(t, src, commentsIn(record, "connected", 1))
	originalKey := identity1605Item(t, rec, original.ID).Key
	collectCarryComments(t, src, commentsIn(record, "connected", 1, CommentDisposition{Key: originalKey, Disposition: "applied", Reason: "fixed original finding"}))
	original.Line = 25
	fresh := tracking.PRReviewComment{ID: "inline:10", Bot: "devin", Body: "keep the check", Path: "a.go", Line: 10}
	src.comments = []tracking.PRReviewComment{original, fresh}
	in := commentsIn(record, "connected", 2, CommentDisposition{Key: originalKey, Disposition: "applied", Reason: "fixed original finding"})
	rec = collectCarryComments(t, src, in)
	if rec.State != "round_limit" || identity1605Item(t, rec, fresh.ID).Disposition != "" {
		t.Fatalf("first collection decided new finding: %+v", rec)
	}
	rec = collectCarryComments(t, src, in)
	if rec.State != "round_limit" || identity1605Item(t, rec, fresh.ID).Disposition != "" {
		t.Fatalf("identical disposition replay decided new finding: %+v", rec)
	}
	if n, err := rec.Unresolved(); err != nil || n != 1 {
		t.Fatalf("unresolved after replay = %d, %v; want one", n, err)
	}
}
