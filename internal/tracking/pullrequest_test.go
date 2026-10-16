package tracking

import (
	"encoding/json"
	"errors"
	"net/http"
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

func TestParseCarries(t *testing.T) {
	got := ParseCarries("o/r", "Carries: #1, o/r#2\n**Carries:** other/x#3 #1\nnot Carries: #4\n")
	if want := []Ref{{Repo: "o/r", Number: 1}, {Repo: "o/r", Number: 2}, {Repo: "other/x", Number: 3}}; !reflect.DeepEqual(got, want) {
		t.Errorf("ParseCarries = %v; want %v", got, want)
	}
}
