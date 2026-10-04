package command

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/agy"
)

// scriptedForge is a pull request that keeps its general comments in memory. It has no way to make a review thread, and it counts what it is asked.
type scriptedForge struct {
	mu                      sync.Mutex
	comments                []prComment
	lists, creates, updates int
	fail                    error         // returned by the next Create, once
	entered, hold           chan struct{} // when set, the first List announces itself on entered and waits for hold
	held                    bool
	onList                  func() // when set, called by every List
}

func (s *scriptedForge) List(ctx context.Context, _ int) ([]prComment, error) {
	s.mu.Lock()
	s.lists++
	block := s.hold != nil && !s.held
	s.held = s.held || block
	snapshot := slices.Clone(s.comments)
	s.mu.Unlock()
	if block {
		s.entered <- struct{}{}
		<-s.hold
	}
	if s.onList != nil {
		s.onList()
	}
	return snapshot, ctx.Err()
}

func (s *scriptedForge) Create(_ context.Context, pr int, body string) (prComment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.fail; err != nil {
		s.fail = nil
		return prComment{}, err
	}
	s.creates++
	c := prComment{ID: int64(len(s.comments) + 1), Body: body, URL: fmt.Sprintf("https://forge.invalid/pull/%d#comment-%d", pr, len(s.comments)+1)}
	s.comments = append(s.comments, c)
	return c, nil
}

func (s *scriptedForge) Update(_ context.Context, id int64, body string) (prComment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.comments {
		if s.comments[i].ID == id {
			s.updates++
			s.comments[i].Body = body
			return s.comments[i], nil
		}
	}
	return prComment{}, errors.New("no such comment")
}

func (f *fixture) post(head string, extra ...string) (int, Summary, string) {
	return f.run(head, append([]string{"--post-summary", "--pr", "7"}, extra...)...)
}

// The pull request keeps exactly one summary comment: the first run creates it, a run for another change updates that comment, and a repeat changes nothing.
func TestPostSummaryKeepsOneCommentAndUpdatesIt(t *testing.T) {
	f := newFixture(t)
	sf := &scriptedForge{}
	f.forge = sf
	h2, h3 := f.repo.change(f.base, 2), f.repo.change(f.base, 3)

	code, sum, errOut := f.post(h2)
	if code != 0 || sum.Comment == nil || sum.Comment.Action != "created" || len(sf.comments) != 1 || sum.Comment.URL != sf.comments[0].URL {
		t.Fatalf("first run: %d %+v %s", code, sum, errOut)
	}
	body := sf.comments[0].Body
	var marker struct{ Head, PatchID, Status, SHA256 string }
	prefix, rest, _ := strings.Cut(body, " -->\n")
	if !strings.HasPrefix(prefix, "<!-- crw-independent-review v1 {") || json.Unmarshal([]byte(strings.TrimPrefix(prefix, "<!-- crw-independent-review v1 ")), &marker) != nil ||
		marker.Head != h2 || marker.PatchID != sum.PatchID || marker.Status != "complete" || marker.SHA256 != sum.SHA256 {
		t.Fatalf("marker line %q (%+v)", prefix, marker)
	}
	for _, want := range []string{"reference opinion", "not a merge gate", agy.DefaultModel, "reviewer 0", "ok"} {
		if !strings.Contains(rest, want) {
			t.Errorf("the comment lacks %q:\n%s", want, body)
		}
	}

	f.on("2026-10-04", deniedResult)
	id := sf.comments[0].ID
	code, sum, errOut = f.post(h3) // another change, and one whose reviewers gave an invalid answer
	if code != 0 || sum.Comment == nil || sum.Comment.Action != "updated" || len(sf.comments) != 1 || sf.comments[0].ID != id {
		t.Fatalf("second change: %d %+v %s", code, sum, errOut)
	}
	for _, want := range []string{"invalid", "denied_actions", "not \"no findings\"", "unavailable"} {
		if !strings.Contains(sf.comments[0].Body, want) {
			t.Errorf("the updated comment lacks %q:\n%s", want, sf.comments[0].Body)
		}
	}

	updates := sf.updates
	calls := f.s.count()
	code, sum, errOut = f.post(h3) // already reviewed: the same text, so nothing is written
	if code != 0 || sum.Outcome != OutcomeAlreadyReviewed || sum.Comment == nil || sum.Comment.Action != "unchanged" || sf.creates != 1 || sf.updates != updates || f.s.count() != calls {
		t.Fatalf("repeat: %d %+v %s (creates %d, updates %d, was %d)", code, sum, errOut, sf.creates, sf.updates, updates)
	}
}

// A review that could not run is posted too, and a failed post is repaired by running the command again, also while the retry is deferred.
func TestPostSummaryRecoversAfterAFailedPost(t *testing.T) {
	f := newFixture(t)
	sf := &scriptedForge{fail: errors.New("forge is down")}
	f.forge = sf
	head := f.repo.change(f.base, 2)
	f.on("2026-10-04", quotaResult)
	code, sum, errOut := f.post(head)
	if code != 1 || sum.Outcome != OutcomeReviewed || sum.Comment != nil || !strings.Contains(errOut, "forge is down") || len(sf.comments) != 0 {
		t.Fatalf("failed post: %d %+v %s", code, sum, errOut)
	}
	calls := f.s.count()
	code, sum, errOut = f.post(head)
	if code != 3 || sum.Outcome != OutcomeRetryDeferred || sum.Comment == nil || sum.Comment.Action != "created" || len(sf.comments) != 1 || f.s.count() != calls ||
		!strings.Contains(sf.comments[0].Body, "unavailable") || !strings.Contains(sf.comments[0].Body, "quota") {
		t.Fatalf("repair: %d %+v %s %v", code, sum, errOut, sf.comments)
	}
}

// Where there is nothing recorded to summarize, nothing is posted, and a request that cannot be met says so.
func TestPostSummaryNeedsAnArtifactItCanTrust(t *testing.T) {
	f := newFixture(t)
	sf := &scriptedForge{}
	f.forge = sf
	a := f.repo.change(f.base, 2)
	if code, _, errOut := f.run(a); code != 0 {
		t.Fatalf("review: %d %s", code, errOut)
	}
	artifact := filepath.Join(f.out, a+".json")
	data, _ := os.ReadFile(artifact)
	if err := os.WriteFile(artifact, append([]byte(" "), data...), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, sum, errOut := f.post(a); code != 1 || sum.Outcome != OutcomeAlreadyReviewed || !strings.Contains(errOut, "sha256") {
		t.Fatalf("an artifact that is not the recorded one: %d %+v %s", code, sum, errOut)
	}
	if err := os.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	if code, sum, errOut := f.post(a); code != 1 || sum.Outcome != OutcomeAlreadyReviewed || !strings.Contains(errOut, "artifact") {
		t.Fatalf("a missing artifact: %d %+v %s", code, sum, errOut)
	}
	if sf.lists+sf.creates+sf.updates != 0 {
		t.Fatalf("the forge was used: %d %d %d", sf.lists, sf.creates, sf.updates)
	}
}

// Comments that carry the marker but were made elsewhere: the first is updated, none is created or deleted, and stderr says there are more.
func TestPostSummaryUpdatesTheFirstOfSeveralMarkerComments(t *testing.T) {
	f := newFixture(t)
	mark := "<!-- crw-independent-review v1 {} -->\nold "
	quoted := "quoting <!-- crw-independent-review v1 {} --> mid-line"
	sf := &scriptedForge{comments: []prComment{{ID: 1, Body: "hello"}, {ID: 2, Body: mark + "A"}, {ID: 3, Body: mark + "B"}, {ID: 4, Body: quoted}}}
	f.forge = sf
	code, sum, errOut := f.post(f.repo.change(f.base, 2))
	if code != 0 || sum.Comment == nil || sum.Comment.Action != "updated" || sf.creates != 0 || sf.updates != 1 || !strings.HasPrefix(sf.comments[1].Body, "<!-- crw-independent-review v1 {") ||
		sf.comments[2].Body != mark+"B" || sf.comments[3].Body != quoted || !strings.Contains(errOut, "2 comments") {
		t.Fatalf("%d %+v %s\n%+v", code, sum, errOut, sf.comments)
	}
}

// Two runs that post for one pull request take turns: while one is inside the forge, the other waits at the post lock, so the list-then-create steps cannot interleave and make a second comment.
// A contender that tries once (--lock-wait=-1s) must be turned away with the forge entered once, so the test fails when the lock is gone.
func TestPostSummaryRunsTakeTurns(t *testing.T) {
	f := newFixture(t)
	sf := &scriptedForge{hold: make(chan struct{}), entered: make(chan struct{}, 4)}
	f.forge = sf
	head := f.repo.change(f.base, 2)
	if code, _, errOut := f.run(head); code != 0 { // reviewed without a post: the posts below find an already reviewed patch and take no run lock
		t.Fatalf("review: %d %s", code, errOut)
	}
	var once sync.Once
	release := func() { once.Do(func() { close(sf.hold) }) }
	t.Cleanup(release)
	done := make(chan int, 1)
	go func() { code, _, _ := f.post(head); done <- code }()
	select {
	case <-sf.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first post never reached the forge")
	}
	code, _, errOut := f.post(head, "--lock-wait", "-1s")
	if code != 3 || !strings.Contains(errOut, "post lock") || sf.lists != 1 || sf.creates != 0 {
		t.Fatalf("contender: exit %d %q (forge lists %d, creates %d)", code, errOut, sf.lists, sf.creates)
	}
	release()
	if code := <-done; code != 0 || len(sf.comments) != 1 {
		t.Fatalf("after the release: exit %d, %d comments", code, len(sf.comments))
	}
}

var codeSpan = regexp.MustCompile("`[^`\n]*`")

// Everything a reviewer or a setting wrote is data: it reaches the comment inside a code span on one line, cut to a length, and cannot make a mention, a link, an image or HTML.
func TestSummaryTextIsData(t *testing.T) {
	hostile := "<img src=x onerror=1> [click](http://evil.invalid) ![i](http://evil.invalid/i.png) @octocat | a\nsecond `line`"
	a := &review.Artifact{Model: hostile, Effort: hostile, Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), PatchID: strings.Repeat("c", 40), Status: review.StatusPartial, Reason: hostile,
		Reviewers: review.ReviewerCounts{Run: 1, Failed: 1}, Calls: []review.CallRecord{{Stage: "review", Perspective: hostile, Class: "invalid", Reason: hostile}}}
	for i := range 12 {
		a.Findings = append(a.Findings, review.Finding{File: hostile, Line: i + 1, Title: strings.Repeat(hostile, 400), Grade: review.P1, Security: i == 0, Reviewers: []int{0}, Support: 1, Verdict: review.VerdictConfirmed})
	}
	body := renderSummary(a, strings.Repeat("d", 64))
	plain := codeSpan.ReplaceAllString(body, "")
	for _, bad := range []string{"@octocat", "<img", "](", "![", "evil", "onerror", "|"} {
		if strings.Contains(plain, bad) {
			t.Errorf("%q is outside a code span:\n%s", bad, plain)
		}
	}
	if !strings.Contains(body, "…") || len(body) > 30000 || strings.Count(body, "\n- **P1**") != 10 || !strings.Contains(body, "2 more") {
		t.Errorf("the findings list is not bounded: %d bytes, %d findings listed", len(body), strings.Count(body, "\n- **P1**"))
	}
}

// The gh forge speaks only to the issue comment endpoints of the checkout's repository, with the body on stdin.
func TestGhForgeCallsOnlyTheCommentEndpoints(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	script := "#!/bin/sh\n" +
		"d=\"$(dirname \"$0\")\"\n" +
		"printf '%s\\n' \"$*\" >> \"$d/argv\"\n" +
		"pwd >> \"$d/cwd\"\n" +
		"case \"$*\" in\n" +
		"*\"--method GET\"*) printf '[{\"id\":1,\"body\":\"hello\",\"html_url\":\"u1\"},{\"id\":2,\"body\":\"<!-- crw-independent-review v1 {} -->\\\\nold\",\"html_url\":\"u2\"}]\\n[{\"id\":3,\"body\":\"later\",\"html_url\":\"u3\"}]\\n';;\n" +
		"*) cat > \"$d/stdin\"; printf '{\"id\":9,\"body\":\"b\",\"html_url\":\"https://forge.invalid/c9\"}\\n';;\n" +
		"esac\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string { data, _ := os.ReadFile(filepath.Join(dir, name)); return string(data) }
	g := ghForge{binary: gh, dir: f.repo.dir}
	ctx := context.Background()
	comments, err := g.List(ctx, 7)
	if err != nil || len(comments) != 3 || comments[2].ID != 3 || comments[1].URL != "u2" {
		t.Fatalf("list across pages: %v %v", comments, err)
	}
	if c, err := g.Create(ctx, 7, "body one\nline two"); err != nil || c.ID != 9 || read("stdin") != "body one\nline two" {
		t.Fatalf("create: %v %v %q", c, err, read("stdin"))
	}
	if c, err := g.Update(ctx, 2, "body two"); err != nil || c.URL != "https://forge.invalid/c9" || read("stdin") != "body two" {
		t.Fatalf("update: %v %v %q", c, err, read("stdin"))
	}
	want := "api --method GET --paginate repos/{owner}/{repo}/issues/7/comments?per_page=100\n" +
		"api --method POST repos/{owner}/{repo}/issues/7/comments -F body=@-\n" +
		"api --method PATCH repos/{owner}/{repo}/issues/comments/2 -F body=@-\n"
	if got := read("argv"); got != want {
		t.Fatalf("arguments:\n%s\nwant\n%s", got, want)
	}
	checkout, _ := filepath.EvalSymlinks(f.repo.dir)
	if cwd := strings.TrimSpace(strings.SplitN(read("cwd"), "\n", 2)[0]); cwd != checkout {
		t.Fatalf("gh ran in %s, want the checkout %s", cwd, checkout)
	}

	// Through the command, with --gh: the comment the fake serves with the marker is updated, and no other endpoint is touched.
	if err := os.Remove(filepath.Join(dir, "argv")); err != nil {
		t.Fatal(err)
	}
	f.forge = nil
	code, sum, errOut := f.post(f.repo.change(f.base, 2), "--gh", gh)
	argv := strings.Split(strings.TrimSpace(read("argv")), "\n")
	if code != 0 || sum.Comment == nil || sum.Comment.Action != "updated" || len(argv) != 2 || !strings.HasSuffix(argv[1], "issues/comments/2 -F body=@-") || !strings.HasPrefix(read("stdin"), "<!-- crw-independent-review v1 {") {
		t.Fatalf("through the command: %d %+v %s %q", code, sum, errOut, argv)
	}
	for _, line := range argv {
		if !strings.Contains(line, "/issues/") || strings.Contains(line, "/pulls/") || strings.Contains(line, "/reviews") {
			t.Errorf("a call outside the issue comments: %s", line)
		}
	}
}

// Cancelling the command while it posts ends it as interrupted, as cancelling the review does.
func TestInterruptWhilePostingExits130(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	sf := &scriptedForge{onList: cancel}
	e := env{runner: f.s.run, now: func() time.Time { return f.at }, forge: func(Config) forge { return sf }}
	var out, errOut bytes.Buffer
	if code := run(ctx, f.args(f.repo.change(f.base, 2), "--post-summary", "--pr", "7"), &out, &errOut, e); code != 130 || !strings.Contains(errOut.String(), "interrupted") || len(sf.comments) != 0 {
		t.Fatalf("interrupted while posting: %d %q", code, errOut.String())
	}
}
