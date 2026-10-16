package tracking

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/publishpolicy"
)

// CRW-1549: OpenIssue and OpenMilestone post through the scrub boundary; the audit event records
// kinds and counts, never the text.

var pubCtx = publishpolicy.Context{Paths: []string{"/home/me"}, Hostnames: []string{"lina-os"}}

func scrubClient(t *testing.T, f *fakeGitHub, events *[]publishpolicy.Event) *GitHub {
	t.Helper()
	g, err := NewGitHub("o/r", StaticToken(testToken), WithBaseURL(f.srv.URL),
		WithPublication(publishpolicy.Policy{}, pubCtx, func(ev publishpolicy.Event) { *events = append(*events, ev) }))
	if err != nil {
		t.Fatalf("NewGitHub: %v", err)
	}
	return g
}

func TestOpenIssueScrubsTitleBodyAndKeepsTheRest(t *testing.T) {
	f := newFake(t)
	var sent map[string]any
	f.handle("POST /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		reqs := f.requests()
		if err := json.Unmarshal([]byte(reqs[len(reqs)-1].Body), &sent); err != nil {
			t.Errorf("decode: %v", err)
		}
		writeJSON(w, 201, map[string]any{"number": 12, "title": sent["title"], "body": sent["body"],
			"state": "open", "html_url": "https://github.com/o/r/issues/12",
			"milestone": map[string]any{"number": 3}, "labels": []any{}})
	})
	var events []publishpolicy.Event
	is, err := scrubClient(t, f, &events).OpenIssue(bg, NewIssue{Title: "from /home/me/build on lina-os", Body: "log /tmp/x", Milestone: 3, Labels: []string{"bug"}})
	if err != nil {
		t.Fatal(err)
	}
	if is.Ref != (Ref{Repo: "o/r", Number: 12}) || is.Milestone != 3 {
		t.Errorf("issue %+v", is)
	}
	for _, want := range []string{"/home/me", "lina-os", "/tmp/x"} {
		if body, _ := sent["body"].(string); strings.Contains(sent["title"].(string)+body, want) {
			t.Errorf("the sent text holds %q: %v", want, sent)
		}
	}
	if sent["milestone"] != float64(3) || len(sent["labels"].([]any)) != 1 {
		t.Errorf("milestone or labels lost: %v", sent)
	}
	if len(events) == 0 {
		t.Fatal("no publication event recorded")
	}
	for _, ev := range events {
		if ev.Surface != "github" || ev.Repo != "o/r" || ev.Field == "" || len(ev.Changes) == 0 || ev.Blocked {
			t.Errorf("event %+v", ev)
		}
		if data, _ := json.Marshal(ev); strings.Contains(string(data), "/home/me") || strings.Contains(string(data), "lina-os") {
			t.Errorf("the event echoes the private text: %s", data)
		}
	}
	// validation: a blank title and a negative milestone are refused before HTTP
	for _, in := range []NewIssue{{Title: " "}, {Title: "t", Milestone: -1}} {
		if _, err := scrubClient(t, f, &events).OpenIssue(bg, in); !errors.Is(err, ErrInvalidIssue) {
			t.Errorf("%+v: %v", in, err)
		}
	}
}

func TestOpenMilestoneScrubsTheDescription(t *testing.T) {
	f := newFake(t)
	var sent map[string]any
	f.handle("POST /repos/o/r/milestones", func(w http.ResponseWriter, r *http.Request) {
		reqs := f.requests()
		if err := json.Unmarshal([]byte(reqs[len(reqs)-1].Body), &sent); err != nil {
			t.Errorf("decode: %v", err)
		}
		writeJSON(w, 201, map[string]any{"number": 4, "title": sent["title"], "description": sent["description"], "state": "open"})
	})
	var events []publishpolicy.Event
	m, err := scrubClient(t, f, &events).OpenMilestone(bg, NewMilestone{Title: "M1", Description: "work /home/me/m1"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Ref.Number != 4 || m.Title != "M1" {
		t.Errorf("milestone %+v", m)
	}
	if d, _ := sent["description"].(string); strings.Contains(d, "/home/me") || !strings.Contains(d, "[path]") {
		t.Errorf("description %q", d)
	}
	if _, err := scrubClient(t, f, &events).OpenMilestone(bg, NewMilestone{Title: "  "}); !errors.Is(err, ErrInvalidMilestone) {
		t.Errorf("blank milestone title: %v", err)
	}
}

// A secret in any public field is ErrSecret before the request: zero HTTP calls, no echo of it in
// the error or the recorded event.
func TestPublicTextSecretStopsBeforeHTTPEverywhere(t *testing.T) {
	f := newFake(t)
	secret := "ghp_" + strings.Repeat("A", 24)
	var events []publishpolicy.Event
	g := scrubClient(t, f, &events)
	for name, call := range map[string]func() error{
		"issue":     func() error { _, err := g.OpenIssue(bg, NewIssue{Title: "t", Body: "k " + secret}); return err },
		"milestone": func() error { _, err := g.OpenMilestone(bg, NewMilestone{Title: "m", Description: secret}); return err },
		"comment":   func() error { return g.Comment(bg, Ref{Repo: "o/r", Number: 1}, "[a · s]", secret) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, publishpolicy.ErrSecret) {
				t.Fatalf("error %v, want ErrSecret", err)
			}
		})
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests left for the forge; want 0", n)
	}
	for _, ev := range events {
		if !ev.Blocked {
			t.Errorf("a blocked field recorded unblocked: %+v", ev)
		}
		if data, _ := json.Marshal(ev); strings.Contains(string(data), secret) {
			t.Errorf("the event echoes the secret: %s", data)
		}
	}
}

// Comment bodies pass the same boundary; HasComment compares the stored first line with the
// sanitized header, so a session id in the header still finds the comment the same header wrote.
func TestCommentAndHasCommentUseTheScrub(t *testing.T) {
	f := newFake(t)
	header := "[l · session-a1b2c3d4]"
	stored := publishpolicy.Scrub(header, pubCtx, publishpolicy.Policy{}).Text
	f.reply("GET /repos/o/r/issues/1/comments", 200, []map[string]any{{"body": stored + "\n\nmore"}})
	var events []publishpolicy.Event
	g := scrubClient(t, f, &events)
	if has, err := g.HasComment(bg, Ref{Repo: "o/r", Number: 1}, header); err != nil || !has {
		t.Fatalf("HasComment = %v, %v; the sanitized header does not match itself", has, err)
	}
	if _, err := g.HasComment(bg, Ref{Repo: "o/r", Number: 1}, "[l · ghp_"+strings.Repeat("A", 24)+"]"); !errors.Is(err, publishpolicy.ErrSecret) {
		t.Fatalf("a secret header: %v", err)
	}
	var sent map[string]any
	f.handle("POST /repos/o/r/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		reqs := f.requests()
		_ = json.Unmarshal([]byte(reqs[len(reqs)-1].Body), &sent)
		writeJSON(w, 201, map[string]any{"id": 7})
	})
	if err := g.Comment(bg, Ref{Repo: "o/r", Number: 1}, header, "ran in /home/me/out"); err != nil {
		t.Fatal(err)
	}
	body, _ := sent["body"].(string)
	if strings.Contains(body, "/home/me") || strings.Contains(body, "session-a1b2c3d4") || !strings.Contains(body, "[path]") || !strings.Contains(body, "[session_id:") {
		t.Errorf("the comment body was not scrubbed: %q", body)
	}
}

// WithPublication(nil recorder) is an explicit opt-out: the field is still scrubbed, nothing is
// recorded.
func TestPublicationNilRecorderWritesNoEvent(t *testing.T) {
	f := newFake(t)
	var sent map[string]any
	f.handle("POST /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		reqs := f.requests()
		_ = json.Unmarshal([]byte(reqs[len(reqs)-1].Body), &sent)
		writeJSON(w, 201, map[string]any{"number": 9, "title": "t", "body": "", "state": "open", "html_url": "x"})
	})
	g, err := NewGitHub("o/r", StaticToken(testToken), WithBaseURL(f.srv.URL),
		WithPublication(publishpolicy.Policy{}, pubCtx, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.OpenIssue(bg, NewIssue{Title: "from /home/me/x", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sent["title"].(string), "/home/me") {
		t.Errorf("the sent title holds the path: %q", sent["title"])
	}
}

// A null public field stays null (a PATCH clear), a non-string is refused before a token or a
// request, and unrecognized fields pass through unchanged.
func TestPublicJSONNullAndNonString(t *testing.T) {
	f := newFake(t)
	var sent map[string]any
	f.handle("POST /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		reqs := f.requests()
		_ = json.Unmarshal([]byte(reqs[len(reqs)-1].Body), &sent)
		writeJSON(w, 201, map[string]any{"number": 5, "title": "t", "state": "open", "html_url": "x", "head": map[string]any{"sha": "h", "ref": "b"}, "base": map[string]any{"ref": "dev"}})
	})
	g, err := NewGitHub("o/r", StaticToken(testToken), WithBaseURL(f.srv.URL),
		WithPublication(publishpolicy.Policy{}, pubCtx, nil))
	if err != nil {
		t.Fatal(err)
	}
	// null is preserved (a PATCH clear), not turned into a string
	b, err := g.publicJSON(map[string]any{"title": "t", "body": nil, "labels": []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc["body"]) != "null" {
		t.Errorf("null became %s", doc["body"])
	}
	// a non-string public field is refused before a token is fetched or a request is sent
	if _, err := g.publicJSON(map[string]any{"title": 42}); err == nil {
		t.Error("a number title was accepted")
	}
	if len(f.requests()) != 0 {
		t.Errorf("a rejected field still made HTTP")
	}
	// unrecognized fields are not touched
	b, _ = g.publicJSON(map[string]any{"draft": true, "custom_key": "/home/me/x"})
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	if out["custom_key"] != "/home/me/x" {
		t.Errorf("an unrecognized field was scrubbed: %v", out)
	}
}

// A malformed comment header that holds a token stops as ErrSecret — the shape error that would
// have named it never happens.
func TestCommentSecretHeaderIsNotEchoed(t *testing.T) {
	f := newFake(t)
	var events []publishpolicy.Event
	g := scrubClient(t, f, &events)
	header := "[l · s] ghp_" + strings.Repeat("A", 24) + "\nbad"
	err := g.Comment(bg, Ref{Repo: "o/r", Number: 1}, header, "x")
	if !errors.Is(err, publishpolicy.ErrSecret) {
		t.Fatalf("error %v, want ErrSecret", err)
	}
	if strings.Contains(err.Error(), "ghp_") {
		t.Errorf("the error echoes the secret: %v", err)
	}
	if has, err := g.HasComment(bg, Ref{Repo: "o/r", Number: 1}, header); err == nil || has {
		t.Errorf("HasComment on a secret header: %v, %v", has, err)
	}
	if len(f.requests()) != 0 {
		t.Errorf("requests left: %d", len(f.requests()))
	}
}
