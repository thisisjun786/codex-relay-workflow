package pullrequest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// CRW-1460 (P2-3): `crw flow pr comments` collects the review bots' comments of a pull request, deduplicates them into items
// and persists each item's disposition over at most two rounds.

const cmHead = "dddddddddddddddddddddddddddddddddddddddd"

// stubCommentSource answers the pull request and its bot comments.
type stubCommentSource struct {
	pr       tracking.PullRequest
	comments []tracking.PRReviewComment
	prErr    error
	revErr   error
	prReads  int
}

func (s *stubCommentSource) PullRequest(context.Context, tracking.Ref) (tracking.PullRequest, error) {
	s.prReads++
	return s.pr, s.prErr
}

func (s *stubCommentSource) ReviewComments(context.Context, tracking.Ref) ([]tracking.PRReviewComment, error) {
	return s.comments, s.revErr
}

func openCommentSource() *stubCommentSource {
	return &stubCommentSource{pr: tracking.PullRequest{Ref: tracking.Ref{Repo: "o/r", Number: 7}, State: tracking.StateOpen, HeadSHA: cmHead, HeadRef: "crw/m1", BaseRef: "dev", HeadRepo: "o/r"}}
}

func commentsIn(record string, bots string, round int, dispositions ...CommentDisposition) CommentsInput {
	return CommentsInput{PR: "o/r#7", Head: cmHead, Record: record, Bots: bots, Round: round, Dispositions: dispositions}
}

func TestCommentsAreDeduplicatedAndEachNeedsADisposition(t *testing.T) {
	record := filepath.Join(t.TempDir(), "review.json")
	src := openCommentSource()
	// one finding both bots saw (a trailing newline and CRLF spellings are the same comment), and a second one
	src.comments = []tracking.PRReviewComment{
		{ID: "inline:9", Bot: "codex", Body: "rename x to y", Path: "a.go", Line: 3},
		{ID: "issue:1", Bot: "devin", Body: "rename x to y\r\n", Path: "a.go", Line: 3},
		{ID: "review:5", Bot: "devin", Body: "missing a test"},
	}
	rec, err := Comments(context.Background(), src, commentsIn(record, "connected", 1))
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if rec.Schema != tracking.PRReviewSchema || rec.State != "pending" || rec.Round != 1 || len(rec.Items) != 2 {
		t.Fatalf("record %+v", rec)
	}
	var merged, other tracking.PRReviewItem
	for _, it := range rec.Items {
		if len(it.Comments) == 2 {
			merged = it
		} else {
			other = it
		}
	}
	if len(merged.Comments) != 2 || len(other.Comments) != 1 {
		t.Fatalf("items %+v", rec.Items)
	}
	n, err := rec.Unresolved()
	if err != nil || n != 2 {
		t.Fatalf("unresolved %d %v", n, err)
	}
	// a disposition for a key the record does not know is an input error
	if _, err := Comments(context.Background(), src, commentsIn(record, "connected", 1,
		CommentDisposition{Key: "ffffffff", Disposition: "applied", Reason: "done"})); err == nil {
		t.Error("a disposition of an unknown key was accepted")
	}
	// a disposition without a reason, or a duplicate key, is an input error
	if _, err := Comments(context.Background(), src, commentsIn(record, "connected", 1,
		CommentDisposition{Key: merged.Key, Disposition: "applied"})); err == nil {
		t.Error("a disposition without a reason was accepted")
	}
	if _, err := Comments(context.Background(), src, commentsIn(record, "connected", 1,
		CommentDisposition{Key: merged.Key, Disposition: "applied", Reason: "r"}, CommentDisposition{Key: merged.Key, Disposition: "rejected", Reason: "r"})); err == nil {
		t.Error("a duplicate disposition key was accepted")
	}
	if _, err := Comments(context.Background(), src, commentsIn(record, "connected", 1,
		CommentDisposition{Key: merged.Key, Disposition: "maybe", Reason: "r"})); err == nil {
		t.Error("a disposition other than applied/rejected was accepted")
	}
	// the same round again, with both decided: idempotent refresh, still round 1, complete
	rec, err = Comments(context.Background(), src, commentsIn(record, "connected", 1,
		CommentDisposition{Key: merged.Key, Disposition: "rejected", Reason: "as intended"},
		CommentDisposition{Key: other.Key, Disposition: "applied", Reason: "fixed"}))
	if err != nil || rec.State != "complete" || rec.Round != 1 {
		t.Fatalf("decided round: %+v %v", rec, err)
	}
	// a source whose body changed is a new item; its old source leaves the old item
	src.comments = []tracking.PRReviewComment{
		{ID: "inline:9", Bot: "codex", Body: "rename x to y and the caller too", Path: "a.go", Line: 3},
		{ID: "issue:1", Bot: "devin", Body: "rename x to y\r\n", Path: "a.go", Line: 3},
		{ID: "review:5", Bot: "devin", Body: "missing a test"},
	}
	rec, err = Comments(context.Background(), src, commentsIn(record, "connected", 1))
	if err != nil {
		t.Fatalf("re-collect: %v", err)
	}
	var moved tracking.PRReviewItem
	found := false
	for _, it := range rec.Items {
		for _, c := range it.Comments {
			if c.ID == "inline:9" {
				moved, found = it, true
			}
		}
	}
	if !found || moved.Disposition == "rejected" {
		t.Fatalf("the moved comment kept the old disposition: %+v", rec.Items)
	}
	// the old item keeps the remaining source and its disposition
	old := false
	for _, it := range rec.Items {
		if len(it.Comments) == 1 && it.Comments[0].ID == "issue:1" {
			old = true
			if it.Disposition != "rejected" {
				t.Errorf("the item the source left lost its disposition: %+v", it)
			}
		}
	}
	if !old {
		t.Errorf("the item the moved source left is gone: %+v", rec.Items)
	}
	// a comment no longer posted stays in the record
	src.comments = nil
	rec, err = Comments(context.Background(), src, commentsIn(record, "connected", 1))
	if err != nil || len(rec.Items) == 0 {
		t.Fatalf("dropped comments were dropped from the record: %+v %v", rec, err)
	}
}

func TestNoBotsConnectedIsRecordedAndPasses(t *testing.T) {
	record := filepath.Join(t.TempDir(), "review.json")
	src := openCommentSource()
	rec, err := Comments(context.Background(), src, commentsIn(record, "not_connected", 1))
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if rec.State != "not_connected" || rec.Bots != "not_connected" || rec.Note != "리뷰 봇 미연결" || len(rec.Items) != 0 {
		t.Fatalf("record %+v", rec)
	}
	data, err := os.ReadFile(record)
	if err != nil || !strings.Contains(string(data), "리뷰 봇 미연결") {
		t.Fatalf("the record on disk: %s %v", data, err)
	}
	// connected with nothing to collect is complete, not disconnected
	rec, err = Comments(context.Background(), src, commentsIn(filepath.Join(t.TempDir(), "r2.json"), "connected", 1))
	if err != nil || rec.State != "complete" || rec.Bots != "connected" {
		t.Fatalf("an empty collection is complete: %+v %v", rec, err)
	}
	// not_connected cannot be claimed while bot comments exist
	src.comments = []tracking.PRReviewComment{{ID: "issue:1", Bot: "devin", Body: "nit"}}
	if _, err := Comments(context.Background(), src, commentsIn(filepath.Join(t.TempDir(), "r3.json"), "not_connected", 1)); err == nil {
		t.Error("not_connected with bot comments present was accepted")
	}
	// nor when a previous round recorded them
	src.comments = nil
	recFile := filepath.Join(t.TempDir(), "r4.json")
	prev := tracking.PRReviewRecord{Schema: tracking.PRReviewSchema, PR: "o/r#7", Head: cmHead, Bots: "connected", Round: 1, State: "pending",
		Items: []tracking.PRReviewItem{{Key: "k", Comments: []tracking.PRReviewComment{{ID: "issue:1", Bot: "devin", Body: "nit"}}}}}
	if err := writeJSONRecord(recFile, prev); err == nil {
		if _, err := Comments(context.Background(), src, commentsIn(recFile, "not_connected", 2)); err == nil {
			t.Error("not_connected over a record that holds comments was accepted")
		}
	}
}

func TestPingPongStopsAfterTwoRoundTrips(t *testing.T) {
	record := filepath.Join(t.TempDir(), "review.json")
	src := openCommentSource()
	src.comments = []tracking.PRReviewComment{{ID: "issue:1", Bot: "devin", Body: "nit"}}
	rec, err := Comments(context.Background(), src, commentsIn(record, "connected", 1))
	if err != nil || rec.State != "pending" || rec.Round != 1 {
		t.Fatalf("round 1: %+v %v", rec, err)
	}
	// still unresolved after round 2: the limit is reached, not "complete"
	rec, err = Comments(context.Background(), src, commentsIn(record, "connected", 2))
	if err != nil || rec.State != "round_limit" || rec.Round != 2 {
		t.Fatalf("round 2: %+v %v", rec, err)
	}
	// a third collection never happens: round 3 is refused before the pull request is even read
	if _, err := Comments(context.Background(), src, commentsIn(record, "connected", 3)); err == nil {
		t.Error("round 3 ran")
	}
	if src.prReads != 4 {
		t.Errorf("the refused round read the pull request %d times (want the 4 reads of rounds 1 and 2)", src.prReads)
	}
	// a round cannot go back and cannot be skipped
	if _, err := Comments(context.Background(), src, commentsIn(record, "connected", 1)); err == nil {
		t.Error("round 1 after round 2 ran")
	}
	src2 := openCommentSource()
	if _, err := Comments(context.Background(), src2, commentsIn(filepath.Join(t.TempDir(), "fresh.json"), "connected", 2)); err == nil {
		t.Error("round 2 over no round 1 ran")
	}
	if src2.prReads != 0 {
		t.Error("the skipped round read the pull request")
	}
	// a head that moved between the reads is an error, not a record
	src3 := openCommentSource()
	moving := &movingCommentSource{stubCommentSource: src3, head: cmHead}
	if _, err := Comments(context.Background(), moving, commentsIn(filepath.Join(t.TempDir(), "m.json"), "connected", 1)); err == nil {
		t.Error("a head that moved during collection was recorded")
	}
}

// movingCommentSource answers a different head on the second read (the pull request moved during collection).
type movingCommentSource struct {
	*stubCommentSource
	head string
}

func (m *movingCommentSource) PullRequest(ctx context.Context, ref tracking.Ref) (tracking.PullRequest, error) {
	pr, err := m.stubCommentSource.PullRequest(ctx, ref)
	if m.prReads > 1 {
		pr.HeadSHA = strings.Repeat("9", 40)
	}
	return pr, err
}

// writeJSONRecord writes v to path for tests that seed a previous record (the production write is the same shape).
func writeJSONRecord(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// TestCommentsCLIRefusesBadInput: the command line refuses malformed flags and missing required values with exit 2.
func TestCommentsCLIRefusesBadInput(t *testing.T) {
	d := deps(t, nil, nil)
	d.NewPullRequests = func(repo string) (tracking.PullRequests, error) { return openCommentSource(), nil }
	for name, args := range map[string][]string{
		"no record":     {"comments", "--pr", "o/r#7", "--head", cmHead, "--bots", "connected", "--round", "1"},
		"no bots":       {"comments", "--pr", "o/r#7", "--head", cmHead, "--record", "/tmp/x"},
		"bad bots":      {"comments", "--pr", "o/r#7", "--head", cmHead, "--record", "/tmp/x", "--bots", "maybe", "--round", "1"},
		"bad round":     {"comments", "--pr", "o/r#7", "--head", cmHead, "--record", "/tmp/x", "--bots", "connected", "--round", "9"},
		"bad head":      {"comments", "--pr", "o/r#7", "--head", "abc", "--record", "/tmp/x", "--bots", "connected", "--round", "1"},
		"bad pr":        {"comments", "--pr", "o/r", "--head", cmHead, "--record", "/tmp/x", "--bots", "connected", "--round", "1"},
		"unknown flag":  {"comments", "--pr", "o/r#7", "--head", cmHead, "--record", "/tmp/x", "--bots", "connected", "--round", "1", "--what"},
		"no subcommand": {"merge"},
	} {
		if code, _, _ := run(t, d, args...); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
}

var errUnused = errors.New("unused in this test")

// OpenPullRequest and Checks make the stub a tracking.PullRequests; Comment and HasComment make it an integrator.PullRequestSource.
func (s *stubCommentSource) OpenPullRequest(context.Context, tracking.NewPullRequest) (tracking.PullRequest, error) {
	return tracking.PullRequest{}, errUnused
}
func (s *stubCommentSource) Checks(context.Context, tracking.Ref) (tracking.Checks, error) {
	return tracking.Checks{}, errUnused
}
func (s *stubCommentSource) Comment(context.Context, tracking.Ref, string, string) error {
	return errUnused
}
func (s *stubCommentSource) HasComment(context.Context, tracking.Ref, string) (bool, error) {
	return false, errUnused
}

// TestDispositionReachesTheItemItNamesAfterADrop: when a source id moved out of its old item and the old item dropped, a
// disposition still reaches its own key — never the item that took the old index.
func TestDispositionReachesTheItemItNamesAfterADrop(t *testing.T) {
	record := filepath.Join(t.TempDir(), "review.json")
	src := openCommentSource()
	src.comments = []tracking.PRReviewComment{
		{ID: "issue:1", Bot: "devin", Body: "finding A"},
		{ID: "inline:2", Bot: "codex", Body: "finding B", Path: "b.go", Line: 1},
		{ID: "review:3", Bot: "devin", Body: "finding C"},
	}
	rec, err := Comments(context.Background(), src, commentsIn(record, "connected", 1))
	if err != nil || len(rec.Items) != 3 {
		t.Fatalf("round 1: %+v %v", rec, err)
	}
	keyOf := func(body string) string {
		for _, it := range rec.Items {
			for _, c := range it.Comments {
				if strings.Contains(c.Body, body) {
					return it.Key
				}
			}
		}
		return ""
	}
	keyB := keyOf("finding B")
	// source A's comment changed its body: it is a new item and its old item drops
	src.comments = []tracking.PRReviewComment{
		{ID: "issue:1", Bot: "devin", Body: "finding A, reworded"},
		{ID: "inline:2", Bot: "codex", Body: "finding B", Path: "b.go", Line: 1},
		{ID: "review:3", Bot: "devin", Body: "finding C"},
	}
	rec, err = Comments(context.Background(), src, commentsIn(record, "connected", 1,
		CommentDisposition{Key: keyB, Disposition: "rejected", Reason: "won't fix"}))
	if err != nil {
		t.Fatalf("round 1 refresh: %v", err)
	}
	for _, it := range rec.Items {
		for _, c := range it.Comments {
			switch {
			case strings.Contains(c.Body, "finding B"):
				if it.Disposition != "rejected" || it.Key != keyB {
					t.Fatalf("B did not take its disposition: %+v", it)
				}
			case strings.Contains(c.Body, "reworded") || strings.Contains(c.Body, "finding C"):
				if it.Disposition != "" {
					t.Fatalf("the disposition of B reached another item: %+v", it)
				}
			}
		}
	}
}

// TestCommentsAChangedHeadNeedsTheNextRound: after round 2 the record's head cannot be replaced by repeating round 2 at a new
// head — a changed head must start the next round (which the limit refuses), and a same-head refresh is not affected.
func TestCommentsAChangedHeadNeedsTheNextRound(t *testing.T) {
	record := filepath.Join(t.TempDir(), "review.json")
	src := openCommentSource()
	src.comments = []tracking.PRReviewComment{{ID: "issue:1", Bot: "devin", Body: "nit"}}
	if _, err := Comments(context.Background(), src, commentsIn(record, "connected", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := Comments(context.Background(), src, commentsIn(record, "connected", 2)); err != nil {
		t.Fatal(err)
	}
	// the head moved: repeating round 2 is an input error before any read
	moved := strings.Repeat("9", 40)
	in := commentsIn(record, "connected", 2)
	in.Head = moved
	if _, err := Comments(context.Background(), src, in); err == nil {
		t.Error("a changed head at the same round was accepted")
	}
	if src.prReads != 4 {
		t.Errorf("the changed-head round read the pull request %d times", src.prReads)
	}
	// the round after the limit is refused as a third round, head or not
	in.Round = 3
	if _, err := Comments(context.Background(), src, in); err == nil {
		t.Error("round 3 ran")
	}
	// the same head at the same round still refreshes
	rec, err := Comments(context.Background(), src, commentsIn(record, "connected", 2))
	if err != nil || rec.State != "round_limit" {
		t.Fatalf("same-head repeat of round 2: %+v %v", rec, err)
	}
}
