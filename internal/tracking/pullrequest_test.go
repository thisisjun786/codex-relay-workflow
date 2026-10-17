package tracking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The pull request operations of the adapter (CRW-1355): open a pull request from a milestone branch naming the issues it carries,
// read its state, and read the checks of its head. They go through the same transport as the issue operations, so the token, the
// headers, the typed errors and the repository check are the same.

func pullJSON(n int, state string, merged bool, extra map[string]any) map[string]any {
	m := map[string]any{
		"number": n, "title": "milestone M1", "state": state, "merged": merged, "draft": false,
		"body":     "Two issues.\n\nCarries: o/r#10, o/r#11",
		"html_url": "https://github.com/o/r/pull/" + itoa(n),
		"head":     map[string]any{"ref": "crw/m1", "sha": strings.Repeat("a", 40), "repo": map[string]any{"full_name": "o/r"}},
		"base":     map[string]any{"ref": "dev", "sha": strings.Repeat("b", 40), "repo": map[string]any{"full_name": "o/r"}},
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestOpenPullRequestPostsHeadBaseTitleAndTheCarriedIssues(t *testing.T) {
	f := newFake(t)
	var sent map[string]any
	f.handle("POST /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		// the fake has read the body into its record before the route runs
		reqs := f.requests()
		if err := json.Unmarshal([]byte(reqs[len(reqs)-1].Body), &sent); err != nil {
			t.Errorf("decode: %v", err)
		}
		writeJSON(w, 201, pullJSON(21, "open", false, map[string]any{"body": sent["body"]}))
	})
	pr, err := newClient(t, f).OpenPullRequest(bg, NewPullRequest{Head: "crw/m1", Base: "dev", Title: "milestone M1", Body: "Two issues.\n",
		Issues: []Ref{{Repo: "o/r", Number: 10}, {Repo: "o/r", Number: 11}, {Repo: "o/r", Number: 10}}})
	if err != nil {
		t.Fatal(err)
	}
	if sent["head"] != "crw/m1" || sent["base"] != "dev" || sent["title"] != "milestone M1" || sent["draft"] != false {
		t.Errorf("sent %v", sent)
	}
	if body, _ := sent["body"].(string); body != "Two issues.\n\nCarries: o/r#10, o/r#11" {
		t.Errorf("body %q: the issues it carries are named once each on a Carries: line after the text", body)
	}
	want := PullRequest{Ref: Ref{Repo: "o/r", Number: 21}, URL: "https://github.com/o/r/pull/21", Title: "milestone M1", Body: "Two issues.\n\nCarries: o/r#10, o/r#11",
		State: StateOpen, HeadRef: "crw/m1", HeadSHA: strings.Repeat("a", 40), HeadRepo: "o/r", BaseRef: "dev", Issues: []Ref{{Repo: "o/r", Number: 10}, {Repo: "o/r", Number: 11}}}
	if !reflect.DeepEqual(pr, want) {
		t.Errorf("OpenPullRequest = %+v\nwant %+v", pr, want)
	}
	if n := len(f.requests()); n != 1 {
		t.Errorf("%d requests; want 1", n)
	}
}

func TestOpenPullRequestRefusesWhatItCannotSend(t *testing.T) {
	f := newFake(t)
	g := newClient(t, f)
	ok := NewPullRequest{Head: "crw/m1", Base: "dev", Title: "t", Issues: []Ref{{Repo: "o/r", Number: 1}}}
	for name, in := range map[string]NewPullRequest{
		"no head":          {Base: "dev", Title: "t", Issues: ok.Issues},
		"no base":          {Head: "crw/m1", Title: "t", Issues: ok.Issues},
		"head is base":     {Head: "dev", Base: "dev", Title: "t", Issues: ok.Issues},
		"fork head":        {Head: "someone:crw/m1", Base: "dev", Title: "t", Issues: ok.Issues},
		"head with space":  {Head: "crw m1", Base: "dev", Title: "t", Issues: ok.Issues},
		"head with dots":   {Head: "crw/../m1", Base: "dev", Title: "t", Issues: ok.Issues},
		"head as option":   {Head: "-m1", Base: "dev", Title: "t", Issues: ok.Issues},
		"full ref head":    {Head: "refs/heads/crw/m1", Base: "dev", Title: "t", Issues: ok.Issues},
		"no title":         {Head: "crw/m1", Base: "dev", Title: " ", Issues: ok.Issues},
		"two-line title":   {Head: "crw/m1", Base: "dev", Title: "a\nb", Issues: ok.Issues},
		"no issue":         {Head: "crw/m1", Base: "dev", Title: "t"},
		"an invalid issue": {Head: "crw/m1", Base: "dev", Title: "t", Issues: []Ref{{Repo: "o/r", Number: 0}}},
		// GitHub refuses a body over 65536 characters with a raw API error at the POST: refused before anything is sent (CRW-1451)
		"a body over the limit":           {Head: "crw/m1", Base: "dev", Title: "t", Issues: ok.Issues, Body: strings.Repeat("a", 65536)},
		"a multibyte body over the limit": {Head: "crw/m1", Base: "dev", Title: "t", Issues: ok.Issues, Body: strings.Repeat("é", 65530)},
	} {
		if _, err := g.OpenPullRequest(bg, in); !errors.Is(err, ErrInvalidPullRequest) {
			t.Errorf("%s: error = %v; want ErrInvalidPullRequest", name, err)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests sent", n)
	}
	// the limit counts characters of the whole body, Carries line included: a body that fills it exactly is sent
	carries := "\n\nCarries: o/r#1"
	for name, text := range map[string]string{"ascii": strings.Repeat("a", 65536-len(carries)), "multibyte": strings.Repeat("é", 65536-len(carries))} {
		in := ok
		in.Body = text
		if err := in.Check(); err != nil {
			t.Errorf("a %s body of exactly 65536 characters: %v", name, err)
		}
		in.Body += "a"
		if err := in.Check(); !errors.Is(err, ErrInvalidPullRequest) {
			t.Errorf("a %s body of 65537 characters: error = %v; want ErrInvalidPullRequest", name, err)
		}
	}
}

func TestPullRequestReadsStateHeadBaseAndCarriedIssues(t *testing.T) {
	f := newFake(t)
	f.reply("GET /repos/o/r/pulls/21", 200, pullJSON(21, "closed", true, map[string]any{"body": "x\n- Carries: #10, other/repo#3\n```\nCarries: #99\n```"}))
	pr, err := newClient(t, f).PullRequest(bg, Ref{Repo: "o/r", Number: 21})
	if err != nil {
		t.Fatal(err)
	}
	if pr.State != StateClosed || !pr.Merged || pr.HeadSHA != strings.Repeat("a", 40) || pr.BaseRef != "dev" || pr.HeadRef != "crw/m1" {
		t.Errorf("PullRequest = %+v", pr)
	}
	if want := []Ref{{Repo: "o/r", Number: 10}, {Repo: "other/repo", Number: 3}}; !reflect.DeepEqual(pr.Issues, want) {
		t.Errorf("Issues = %v; want %v (a fenced line is not read)", pr.Issues, want)
	}
}

func TestChecksJudgeTheCheckRunsAndStatusesOfTheHead(t *testing.T) {
	head := strings.Repeat("a", 40)
	run := func(name, status, conclusion string) map[string]any {
		return map[string]any{"name": name, "status": status, "conclusion": conclusion, "html_url": "https://x/" + name}
	}
	status := func(context, state string) map[string]any {
		return map[string]any{"context": context, "state": state, "target_url": "https://s/" + context}
	}
	for _, c := range []struct {
		name     string
		runs     []any
		statuses []any
		want     CheckState
	}{
		{"none", nil, nil, CheckNone},
		{"all green", []any{run("ci", "completed", "success"), run("lint", "completed", "skipped")}, []any{status("devin", "success")}, CheckSuccess},
		{"one running", []any{run("ci", "in_progress", ""), run("lint", "completed", "success")}, nil, CheckPending},
		{"a pending status", []any{run("ci", "completed", "success")}, []any{status("devin", "pending")}, CheckPending},
		{"one failed", []any{run("ci", "completed", "failure"), run("lint", "in_progress", "")}, nil, CheckFailure},
		{"a timed out run", []any{run("ci", "completed", "timed_out")}, nil, CheckFailure},
		{"an error status", nil, []any{status("devin", "error")}, CheckFailure},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t)
			f.reply("GET /repos/o/r/pulls/21", 200, pullJSON(21, "open", false, nil))
			f.handle("GET /repos/o/r/commits/"+head+"/check-runs", func(w http.ResponseWriter, r *http.Request) {
				runs := c.runs
				if r.URL.Query().Get("page") == "" && len(runs) > 1 {
					w.Header().Set("Link", `<`+f.srv.URL+`/repos/o/r/commits/`+head+`/check-runs?per_page=100&page=2>; rel="next"`)
					writeJSON(w, 200, map[string]any{"total_count": len(runs), "check_runs": runs[:1]})
					return
				}
				if r.URL.Query().Get("page") == "2" {
					runs = runs[1:]
				}
				if runs == nil {
					runs = []any{}
				}
				writeJSON(w, 200, map[string]any{"total_count": len(c.runs), "check_runs": runs})
			})
			f.handle("GET /repos/o/r/commits/"+head+"/status", func(w http.ResponseWriter, r *http.Request) {
				st := c.statuses
				if st == nil {
					st = []any{}
				}
				writeJSON(w, 200, map[string]any{"state": "pending", "statuses": st})
			})
			got, err := newClient(t, f).Checks(bg, Ref{Repo: "o/r", Number: 21})
			if err != nil {
				t.Fatal(err)
			}
			if got.State != c.want || got.Head != head || got.PullRequest != (Ref{Repo: "o/r", Number: 21}) || len(got.Checks) != len(c.runs)+len(c.statuses) {
				t.Errorf("Checks = %+v; want state %s over %d checks", got, c.want, len(c.runs)+len(c.statuses))
			}
			for _, ch := range got.Checks {
				if ch.Name == "" || (ch.Kind != CheckRun && ch.Kind != CommitStatus) || ch.URL == "" {
					t.Errorf("check %+v", ch)
				}
			}
		})
	}
}

func TestPullRequestOperationsNameTheRepositoryAndComeBackTyped(t *testing.T) {
	f := newFake(t)
	g := newClient(t, f)
	for name, call := range map[string]func() error{
		"PullRequest": func() error { _, err := g.PullRequest(bg, Ref{Repo: "x/y", Number: 1}); return err },
		"Checks":      func() error { _, err := g.Checks(bg, Ref{Repo: "x/y", Number: 1}); return err },
	} {
		if err := call(); !errors.Is(err, ErrOtherRepository) {
			t.Errorf("%s(other repo) error = %v; want ErrOtherRepository", name, err)
		}
	}
	if _, err := g.PullRequest(bg, Ref{Repo: "o/r", Number: 0}); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("PullRequest(#0) error = %v; want ErrInvalidRef", err)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests sent", n)
	}
	for _, c := range []struct {
		status int
		is     error
	}{{404, ErrNotFound}, {401, ErrUnauthorized}, {403, ErrForbidden}, {429, ErrRateLimited}} {
		f := newFake(t)
		h := func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, c.status, map[string]any{"message": "no: " + testToken})
		}
		for _, route := range []string{"POST /repos/o/r/pulls", "GET /repos/o/r/pulls/21"} {
			f.handle(route, h)
		}
		g := newClient(t, f)
		for name, call := range map[string]func() error{
			"OpenPullRequest": func() error {
				_, err := g.OpenPullRequest(bg, NewPullRequest{Head: "crw/m1", Base: "dev", Title: "t", Issues: []Ref{{Repo: "o/r", Number: 1}}})
				return err
			},
			"PullRequest": func() error { _, err := g.PullRequest(bg, Ref{Repo: "o/r", Number: 21}); return err },
			"Checks":      func() error { _, err := g.Checks(bg, Ref{Repo: "o/r", Number: 21}); return err },
		} {
			err := call()
			if !errors.Is(err, c.is) || strings.Contains(err.Error(), testToken) {
				t.Errorf("%s on %d: error = %v; want Is %v and no token", name, c.status, err, c.is)
			}
		}
	}
}

func TestReadingAnIssueStillRefusesAPullRequest(t *testing.T) {
	// the pull request operations do not widen the issue operations: Issue on a pull request is ErrNotIssue as before
	f := newFake(t)
	f.reply("GET /repos/o/r/issues/21", 200, issueJSON(21, "pr", "open", map[string]any{"pull_request": map[string]any{}}))
	if _, err := newClient(t, f).Issue(bg, Ref{Repo: "o/r", Number: 21}); !errors.Is(err, ErrNotIssue) {
		t.Errorf("Issue(pull request) error = %v; want ErrNotIssue", err)
	}
}

// CRW-1549: text bound for the public forge passes the publication scrub before the request: host
// paths, the host's name, private addresses and e-mail are replaced, never sent as they are.
func TestPublicPullRequestScrubsInternalText(t *testing.T) {
	f := newFake(t)
	host, _ := os.Hostname()
	var sent map[string]any
	f.handle("POST /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		reqs := f.requests()
		if err := json.Unmarshal([]byte(reqs[len(reqs)-1].Body), &sent); err != nil {
			t.Errorf("decode: %v", err)
		}
		writeJSON(w, 201, pullJSON(21, "open", false, map[string]any{"body": sent["body"], "title": sent["title"]}))
	})
	body := "Work ran in /home/private/build on " + host + " at 10.2.3.4; dev@private.example knows."
	_, err := newClient(t, f).OpenPullRequest(bg, NewPullRequest{Head: "crw/m1", Base: "dev", Title: "log /home/private/build", Body: body,
		Issues: []Ref{{Repo: "o/r", Number: 10}}})
	if err != nil {
		t.Fatal(err)
	}
	sentBody, _ := sent["body"].(string)
	sentTitle, _ := sent["title"].(string)
	for _, want := range []string{"/home/private/build", host, "10.2.3.4", "dev@private.example"} {
		if strings.Contains(sentBody+sentTitle, want) {
			t.Errorf("the sent text holds %q: title %q body %q", want, sentTitle, sentBody)
		}
	}
	for _, want := range []string{"[path]", "[hostname]", "[ip]", "[email]"} {
		if !strings.Contains(sentBody+sentTitle, want) {
			t.Errorf("the sent text lacks %q: title %q body %q", want, sentTitle, sentBody)
		}
	}
}

// CRW-1549: a token-shaped secret in public text stops the request before any HTTP: nothing is
// sent, and the error carries no echo of the secret.
func TestPublicPullRequestStopsSecretBeforeHTTP(t *testing.T) {
	f := newFake(t)
	f.reply("POST /repos/o/r/pulls", 201, pullJSON(21, "open", false, nil))
	secret := "ghp_" + strings.Repeat("A", 24)
	_, err := newClient(t, f).OpenPullRequest(bg, NewPullRequest{Head: "crw/m1", Base: "dev", Title: "milestone M1",
		Body: "the key is " + secret, Issues: []Ref{{Repo: "o/r", Number: 10}}})
	if err == nil {
		t.Fatal("a token-shaped secret reached the forge")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error echoes the secret: %v", err)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests left for the forge; want 0", n)
	}
}

func TestParseCarries(t *testing.T) {
	got := ParseCarries("o/r", "Carries: #1, o/r#2\n**Carries:** other/x#3 #1\nnot Carries: #4\n")
	if want := []Ref{{Repo: "o/r", Number: 1}, {Repo: "o/r", Number: 2}, {Repo: "other/x", Number: 3}}; !reflect.DeepEqual(got, want) {
		t.Errorf("ParseCarries = %v; want %v", got, want)
	}
}

// TestPublicPullRequestPreservesCarriedReferences (CRW-1549 v2): the publication scrub must leave
// relative source file names and `owner/repo#N` Carries references intact in a real POST body — the
// issues it names stay machine-readable.
func TestPublicPullRequestPreservesCarriedReferences(t *testing.T) {
	for _, repo := range []string{
		"acme/session-management",
		"session-maintainers/thread-status-tests",
		"acme/req_0123456789abcdef01234567",
		"acme/123e4567-e89b-42d3-a456-426614174000",
	} {
		t.Run(repo, func(t *testing.T) {
			f := newFake(t)
			var sent map[string]any
			f.handle("POST /repos/"+repo+"/pulls", func(w http.ResponseWriter, r *http.Request) {
				reqs := f.requests()
				if err := json.Unmarshal([]byte(reqs[len(reqs)-1].Body), &sent); err != nil {
					t.Errorf("decode: %v", err)
				}
				writeJSON(w, 201, pullJSON(21, "open", false, map[string]any{
					"body": sent["body"], "head": map[string]any{"ref": "feature", "sha": strings.Repeat("a", 40), "repo": map[string]any{"full_name": repo}},
				}))
			})
			g, err := NewGitHub(repo, StaticToken(testToken), WithBaseURL(f.srv.URL))
			if err != nil {
				t.Fatal(err)
			}
			body := "See internal/flow/engine/session_evidence.go and thread_status_test.go."
			wantBody := body + "\n\nCarries: " + repo + "#7"
			pr, err := g.OpenPullRequest(bg, NewPullRequest{Head: "feature", Base: "dev", Title: "Fix source references", Body: body,
				Issues: []Ref{{Repo: repo, Number: 7}}})
			if err != nil {
				t.Fatal(err)
			}
			if sent["body"] != wantBody {
				t.Errorf("sent body %q, want %q", sent["body"], wantBody)
			}
			if !reflect.DeepEqual(pr.Issues, []Ref{{Repo: repo, Number: 7}}) {
				t.Errorf("issues %v", pr.Issues)
			}
			if refs := ParseCarries(repo, wantBody); !reflect.DeepEqual(refs, []Ref{{Repo: repo, Number: 7}}) {
				t.Errorf("ParseCarries %v", refs)
			}
			if n := len(f.requests()); n != 1 {
				t.Errorf("%d requests; want 1", n)
			}
		})
	}
}

// ---- review comment records (CRW-1460) ----

// commentJSON is a comment of a review surface as GitHub answers it.
func commentJSON(id int, login, body string, extra map[string]any) map[string]any {
	m := map[string]any{"id": id, "body": body, "user": map[string]any{"login": login}, "html_url": "https://c/" + itoa(id)}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestReviewCommentsCollectsTheBotCommentsOfAllThreeSurfaces(t *testing.T) {
	f := newFake(t)
	f.handle("GET /repos/o/r/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []any{
			commentJSON(1, "devin[bot]", "issue note", nil),
			commentJSON(2, "Devin-AI-Integration[bot]", "second note", nil),
			commentJSON(3, "jun", "a human note", nil),
			commentJSON(4, "devin[bot]", "", nil),
		})
	})
	f.handle("GET /repos/o/r/pulls/7/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `<`+f.srv.URL+`/repos/o/r/pulls/7/comments?per_page=100&page=2>; rel="next"`)
			writeJSON(w, 200, []any{commentJSON(9, "chatgpt-codex-connector[bot]", "rename this", map[string]any{"path": "a.go", "line": nil, "original_line": 3})})
			return
		}
		writeJSON(w, 200, []any{commentJSON(10, "codex[bot]", "and this", map[string]any{"path": "b.go", "line": 12})})
	})
	f.reply("GET /repos/o/r/pulls/7/reviews", 200, []any{
		commentJSON(5, "devin-ai-integration[bot]", "overall note", nil),
		commentJSON(6, "codex[bot]", "   ", nil),
		commentJSON(7, "jun", "reviewed", nil),
	})
	got, err := newClient(t, f).ReviewComments(bg, Ref{Repo: "o/r", Number: 7})
	if err != nil {
		t.Fatal(err)
	}
	want := []PRReviewComment{
		{ID: "issue:1", Bot: "devin", Body: "issue note", URL: "https://c/1"},
		{ID: "issue:2", Bot: "devin", Body: "second note", URL: "https://c/2"},
		{ID: "inline:9", Bot: "codex", Body: "rename this", Path: "a.go", Line: 3, URL: "https://c/9"},
		{ID: "inline:10", Bot: "codex", Body: "and this", Path: "b.go", Line: 12, URL: "https://c/10"},
		{ID: "review:5", Bot: "devin", Body: "overall note", URL: "https://c/5"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReviewComments = %+v\nwant %+v", got, want)
	}
	// a surface that errors is an error of the whole read, never a partial list
	f2 := newFake(t)
	f2.reply("GET /repos/o/r/issues/7/comments", 200, []any{})
	f2.reply("GET /repos/o/r/pulls/7/comments", 500, map[string]any{"message": "down"})
	f2.reply("GET /repos/o/r/pulls/7/reviews", 200, []any{})
	if _, err := newClient(t, f2).ReviewComments(bg, Ref{Repo: "o/r", Number: 7}); err == nil {
		t.Error("a surface error was not an error")
	}
}

func TestReadPRReviewRecordValidatesWhatItReads(t *testing.T) {
	head := strings.Repeat("a", 40)
	ref := Ref{Repo: "o/r", Number: 7}
	write := func(t *testing.T, v any) string {
		t.Helper()
		var data []byte
		var err error
		switch doc := v.(type) {
		case string:
			data = []byte(doc)
		default:
			data, err = json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, '\n')
		}
		path := filepath.Join(t.TempDir(), "r.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	item := func(key, dis, reason string) PRReviewItem {
		return PRReviewItem{Key: key, Disposition: dis, Reason: reason,
			Comments: []PRReviewComment{{ID: "issue:1-" + key, Bot: ReviewBotDevin, Body: "nit"}}}
	}
	valid := PRReviewRecord{Schema: PRReviewSchema, PR: "o/r#7", Head: head, Bots: ReviewBotsConnected, Round: 1, State: ReviewStatePending,
		Items: []PRReviewItem{item("k", "", "")}}
	got, err := ReadPRReviewRecord(write(t, valid), ref, head)
	if err != nil || got.Round != 1 || len(got.Items) != 1 {
		t.Fatalf("valid record: %+v %v", got, err)
	}
	if n, err := got.Unresolved(); err != nil || n != 1 {
		t.Fatalf("unresolved %d %v", n, err)
	}
	// the pull request binds without regard to case, the head exactly
	cased := valid
	cased.PR = "O/R#7"
	if _, err := ReadPRReviewRecord(write(t, cased), ref, head); err != nil {
		t.Errorf("a same pull request in another case: %v", err)
	}
	// applied items with a reason count as resolved; a decided record's state is complete
	applied := valid
	applied.State = ReviewStateComplete
	applied.Items = []PRReviewItem{item("k", DispositionApplied, "fixed")}
	if rec, err := ReadPRReviewRecord(write(t, applied), ref, head); err != nil {
		t.Fatal(err)
	} else if n, err := rec.Unresolved(); err != nil || n != 0 {
		t.Errorf("applied: unresolved %d %v", n, err)
	}
	// one invalid field per case
	for name, mutate := range map[string]func(*PRReviewRecord){
		"wrong schema":            func(r *PRReviewRecord) { r.Schema = "other/1" },
		"other pr":                func(r *PRReviewRecord) { r.PR = "o/r#8" },
		"not a ref":               func(r *PRReviewRecord) { r.PR = "x" },
		"stale head":              func(r *PRReviewRecord) { r.Head = strings.Repeat("b", 40) },
		"short head":              func(r *PRReviewRecord) { r.Head = "abc" },
		"round 0":                 func(r *PRReviewRecord) { r.Round = 0 },
		"round 3":                 func(r *PRReviewRecord) { r.Round = 3 },
		"unknown bots":            func(r *PRReviewRecord) { r.Bots = "sometimes" },
		"unknown state":           func(r *PRReviewRecord) { r.State = "sideways" },
		"complete but unresolved": func(r *PRReviewRecord) { r.State = ReviewStateComplete },
		"pending at round 2":      func(r *PRReviewRecord) { r.Round = 2 },
		"duplicate keys":          func(r *PRReviewRecord) { r.Items = append(r.Items, item("k", "", "")) },
		"empty comments":          func(r *PRReviewRecord) { r.Items[0].Comments = nil },
		"empty comment id":        func(r *PRReviewRecord) { r.Items[0].Comments[0].ID = "" },
		"duplicate comment id": func(r *PRReviewRecord) {
			r.Items = append(r.Items, item("k2", "", ""))
			r.Items[1].Comments[0].ID = r.Items[0].Comments[0].ID
		},
		"unknown bot":       func(r *PRReviewRecord) { r.Items[0].Comments[0].Bot = "jun" },
		"empty body":        func(r *PRReviewRecord) { r.Items[0].Comments[0].Body = "  " },
		"bad disposition":   func(r *PRReviewRecord) { r.Items[0].Disposition = "maybe"; r.Items[0].Reason = "r" },
		"no reason":         func(r *PRReviewRecord) { r.Items[0].Disposition = DispositionApplied },
		"reason no verdict": func(r *PRReviewRecord) { r.Items[0].Reason = "r" },
	} {
		t.Run(name, func(t *testing.T) {
			rec := valid
			rec.Items = []PRReviewItem{item("k", "", "")}
			mutate(&rec)
			if _, err := ReadPRReviewRecord(write(t, rec), ref, head); err == nil {
				t.Errorf("%s was accepted", name)
			}
		})
	}
	// raw file problems
	for name, doc := range map[string]string{
		"malformed": "{",
		"trailing":  `{"schema":"crw-pr-review/1","pr":"o/r#7","head":"` + head + `","bots":"connected","round":1,"state":"complete","items":[]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadPRReviewRecord(write(t, doc), ref, head); err == nil {
				t.Errorf("%s was accepted", name)
			}
		})
	}
	if _, err := ReadPRReviewRecord(filepath.Join(t.TempDir(), "none.json"), ref, head); err == nil {
		t.Error("a missing record was accepted")
	}
	// oversized data is refused
	if _, err := ReadPRReviewRecord(write(t, `{"x":`+strings.Repeat(" ", 16<<20)+`}`), ref, head); err == nil {
		t.Error("an oversized record was accepted")
	}
	// not_connected records carry the exact note and no items
	nc := PRReviewRecord{Schema: PRReviewSchema, PR: "o/r#7", Head: head, Bots: ReviewBotsNotConnected, Round: 1,
		State: ReviewStateNotConnected, Note: "리뷰 봇 미연결", Items: []PRReviewItem{}}
	if rec, err := ReadPRReviewRecord(write(t, nc), ref, head); err != nil {
		t.Fatalf("not_connected: %v", err)
	} else if n, _ := rec.Unresolved(); n != 0 {
		t.Errorf("not_connected unresolved %d", n)
	}
	badNC := nc
	badNC.Items = []PRReviewItem{item("k", "", "")}
	if _, err := ReadPRReviewRecord(write(t, badNC), ref, head); err == nil {
		t.Error("a not_connected record with items was accepted")
	}
	badNC2 := nc
	badNC2.Note = "something else"
	if _, err := ReadPRReviewRecord(write(t, badNC2), ref, head); err == nil {
		t.Error("a not_connected record without the note was accepted")
	}
}

// TestFindPullRequestPagesFiltersAndPrefersOpen: the lookup paginates the list call, keeps only rows of this repository with
// the exact head and base refs, prefers one open over a historical one, refuses two opens and answers none/unique history.
func TestFindPullRequestPagesFiltersAndPrefersOpen(t *testing.T) {
	head := "crw/m1"
	newPull := func(n int, state string, merged bool, headRef, baseRef, headRepo string) map[string]any {
		repo := any(nil)
		if headRepo != "" {
			repo = map[string]any{"full_name": headRepo}
		}
		return map[string]any{"number": n, "state": state, "merged": merged, "draft": false,
			"head": map[string]any{"ref": headRef, "sha": fmt.Sprintf("%040x", 100+n), "repo": repo},
			"base": map[string]any{"ref": baseRef}, "html_url": "https://h/" + itoa(n)}
	}
	var queries []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/pulls" {
			t.Errorf("the lookup hit %s, only the list is read", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "", "1":
			// page 1: noise — a fork's head repo, another base, another head, an unrelated state
			w.Header().Set("Link", `<`+srv.URL+`/repos/o/r/pulls?page=2>; rel="next"`)
			json.NewEncoder(w).Encode([]any{
				newPull(1, "open", false, head, "dev", "o/fork"),
				newPull(2, "open", false, head, "release", "o/r"),
				newPull(3, "open", false, "crw/m2", "dev", "o/r"),
			})
		case "2":
			json.NewEncoder(w).Encode([]any{newPull(7, "open", false, head, "dev", "o/r")})
		default:
			json.NewEncoder(w).Encode([]any{})
		}
	}))
	defer srv.Close()
	g := mustGitHub(t, srv.URL, "o/r")
	pr, found, err := g.FindPullRequest(context.Background(), head, "dev")
	if err != nil || !found || pr.Ref.Number != 7 {
		t.Fatalf("the paginated filtered lookup: %v %v %v", pr.Ref, found, err)
	}
	if q, err := url.ParseQuery(queries[0]); err != nil || q.Get("head") != "o:"+head || q.Get("base") != "dev" || q.Get("state") != "all" {
		t.Errorf("the list query: %v %v", q, err)
	}
	// a historical match answers it (a closed unmerged pull request refuses the caller to duplicate)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]any{newPull(4, "closed", false, head, "dev", "o/r")})
	}))
	defer srv2.Close()
	pr, found, err = mustGitHub(t, srv2.URL, "o/r").FindPullRequest(context.Background(), head, "dev")
	if err != nil || !found || pr.Ref.Number != 4 || pr.State != "closed" {
		t.Fatalf("the unique historical match: %v %v %v", pr.Ref, found, err)
	}
	// two opens are an error, never a choice
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]any{newPull(8, "open", false, head, "dev", "o/r"), newPull(9, "open", false, head, "dev", "o/r")})
	}))
	defer srv3.Close()
	if _, _, err = mustGitHub(t, srv3.URL, "o/r").FindPullRequest(context.Background(), head, "dev"); err == nil {
		t.Fatal("two open pull requests are an error, not a pick")
	}
}

// mustGitHub is the adapter under test against the httptest base.
func mustGitHub(t *testing.T, base, repo string) *GitHub {
	t.Helper()
	g, err := NewGitHub(repo, StaticToken("t"), WithBaseURL(base))
	if err != nil {
		t.Fatal(err)
	}
	return g
}
