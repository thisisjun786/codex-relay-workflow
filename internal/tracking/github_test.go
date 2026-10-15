package tracking

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

var bg = context.Background()

func issueJSON(n int, title, state string, extra map[string]any) map[string]any {
	m := map[string]any{
		"number": n, "title": title, "state": state, "body": "", "labels": []any{},
		"html_url": "https://github.com/o/r/issues/" + itoa(n), "milestone": nil,
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func itoa(n int) string { return strconv.Itoa(n) }

func labels(names ...string) []any {
	out := []any{}
	for _, n := range names {
		out = append(out, map[string]any{"name": n, "color": "ededed"})
	}
	return out
}

func TestNewGitHubChecksItsInputs(t *testing.T) {
	for _, repo := range []string{"", "o", "o/", "/r", "o/r/x", "o/r#1", "o r/x", "../r", "o/.."} {
		if g, err := NewGitHub(repo, StaticToken("t")); err == nil {
			t.Errorf("NewGitHub(%q) = %v; want an error", repo, g)
		}
	}
	if _, err := NewGitHub("o/r", nil); err == nil {
		t.Error("NewGitHub without a token source succeeded")
	}
	if _, err := NewGitHub("o/r", StaticToken("t"), WithBaseURL("not a url")); err == nil {
		t.Error("NewGitHub with a bad base URL succeeded")
	}
	if g, err := NewGitHub("thisisjun786/crw-work", StaticToken("t")); err != nil || g == nil {
		t.Errorf("NewGitHub(valid) = %v, %v", g, err)
	}
}

func TestMilestoneReadsTheMilestoneAndAllItsIssuesAcrossPages(t *testing.T) {
	f := newFake(t)
	f.reply("GET /repos/o/r/milestones/3", 200, map[string]any{
		"number": 3, "title": "1.0 · GitHub 전환", "description": "d", "state": "open", "open_issues": 2, "closed_issues": 1,
	})
	f.handle("GET /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("milestone") != "3" || q.Get("state") != "all" || q.Get("per_page") != "100" {
			t.Errorf("issues query = %q", r.URL.RawQuery)
		}
		if q.Get("page") == "" {
			next := f.srv.URL + "/repos/o/r/issues?milestone=3&state=all&per_page=100&page=2"
			w.Header().Set("Link", `<`+next+`>; rel="next", <`+next+`>; rel="last"`)
			writeJSON(w, 200, []any{
				issueJSON(10, "first", "open", map[string]any{"body": "Blocked by: #9, other/repo#2", "labels": labels("P1", "feature"), "milestone": map[string]any{"number": 3}}),
				issueJSON(12, "a pull request", "open", map[string]any{"pull_request": map[string]any{"url": "x"}}),
			})
			return
		}
		if q.Get("page") != "2" {
			t.Errorf("page = %q", q.Get("page"))
		}
		writeJSON(w, 200, []any{
			issueJSON(11, "second", "closed", map[string]any{"state_reason": "not_planned", "milestone": map[string]any{"number": 3}}),
		})
	})
	ms, issues, err := newClient(t, f).Milestone(bg, Ref{Repo: "o/r", Number: 3})
	if err != nil {
		t.Fatal(err)
	}
	wantMs := Milestone{Ref: Ref{Repo: "o/r", Number: 3}, Title: "1.0 · GitHub 전환", Description: "d", State: StateOpen, OpenIssues: 2, ClosedIssues: 1}
	if ms != wantMs {
		t.Errorf("milestone = %+v; want %+v", ms, wantMs)
	}
	want := []Issue{
		{Ref: Ref{Repo: "o/r", Number: 10}, URL: "https://github.com/o/r/issues/10", Title: "first", Body: "Blocked by: #9, other/repo#2",
			State: StateOpen, Labels: []string{"P1", "feature"}, Milestone: 3, BlockedBy: []Ref{{Repo: "o/r", Number: 9}, {Repo: "other/repo", Number: 2}}},
		{Ref: Ref{Repo: "o/r", Number: 11}, URL: "https://github.com/o/r/issues/11", Title: "second", State: StateClosed, CloseReason: NotPlanned, Labels: []string{}, Milestone: 3},
	}
	if !reflect.DeepEqual(issues, want) {
		t.Errorf("issues = %+v\nwant %+v", issues, want)
	}
	if got := len(f.requests()); got != 3 {
		t.Errorf("%d requests; want 3 (milestone, two pages)", got)
	}
}

func TestIssueReadsBodyLabelsStateSubIssuesAndBlockedBy(t *testing.T) {
	f := newFake(t)
	f.reply("GET /repos/o/r/issues/10", 200, issueJSON(10, "the issue", "closed", map[string]any{
		"body":         "[층 · 세션] 작성.\n\nBlocked by: #35\n\n## 기준",
		"state_reason": "completed",
		"labels":       labels("P1", "area:relay"),
		"milestone":    map[string]any{"number": 2},
	}))
	f.handle("GET /repos/o/r/issues/10/sub_issues", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("sub_issues query = %q", r.URL.RawQuery)
		}
		if r.URL.Query().Get("page") == "" {
			next := f.srv.URL + "/repos/o/r/issues/10/sub_issues?per_page=100&page=2"
			w.Header().Set("Link", `<`+next+`>; rel="next"`)
			writeJSON(w, 200, []any{issueJSON(20, "sub one", "open", map[string]any{"repository_url": "https://api.github.com/repos/o/r"})})
			return
		}
		writeJSON(w, 200, []any{issueJSON(5, "sub two", "closed", map[string]any{"repository_url": "https://api.github.com/repos/other/repo"})})
	})
	got, err := newClient(t, f).Issue(bg, Ref{Repo: "o/r", Number: 10})
	if err != nil {
		t.Fatal(err)
	}
	want := Issue{
		Ref: Ref{Repo: "o/r", Number: 10}, URL: "https://github.com/o/r/issues/10", Title: "the issue",
		Body: "[층 · 세션] 작성.\n\nBlocked by: #35\n\n## 기준", State: StateClosed, CloseReason: Completed,
		Labels: []string{"P1", "area:relay"}, Milestone: 2,
		SubIssues: []SubIssue{{Ref{Repo: "o/r", Number: 20}, "sub one", StateOpen}, {Ref{Repo: "other/repo", Number: 5}, "sub two", StateClosed}},
		BlockedBy: []Ref{{Repo: "o/r", Number: 35}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("issue = %+v\nwant %+v", got, want)
	}
}

func TestIssueWithoutSubIssuesHasNone(t *testing.T) {
	f := newFake(t)
	f.reply("GET /repos/o/r/issues/7", 200, issueJSON(7, "plain", "open", nil))
	f.reply("GET /repos/o/r/issues/7/sub_issues", 200, []any{})
	got, err := newClient(t, f).Issue(bg, Ref{Repo: "o/r", Number: 7})
	if err != nil || got.State != StateOpen || got.CloseReason != "" || len(got.SubIssues) != 0 || got.Milestone != 0 {
		t.Errorf("Issue = %+v, %v", got, err)
	}
}

func TestIssueRefusesAPullRequest(t *testing.T) {
	f := newFake(t)
	f.reply("GET /repos/o/r/issues/8", 200, issueJSON(8, "pr", "open", map[string]any{"pull_request": map[string]any{}}))
	if _, err := newClient(t, f).Issue(bg, Ref{Repo: "o/r", Number: 8}); !errors.Is(err, ErrNotIssue) {
		t.Errorf("Issue(pull request) error = %v; want ErrNotIssue", err)
	}
}

func TestCommentWritesTheHeaderAsTheFirstLine(t *testing.T) {
	f := newFake(t)
	f.reply("POST /repos/o/r/issues/10/comments", 201, map[string]any{"id": 1})
	g := newClient(t, f)
	if err := g.Comment(bg, Ref{Repo: "o/r", Number: 10}, "[프로젝트 담당 · Claude Code abc12345]", "첫 줄\n둘째 줄"); err != nil {
		t.Fatal(err)
	}
	if err := g.Comment(bg, Ref{Repo: "o/r", Number: 10}, "[관리 · s1]", ""); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests; want 2", len(reqs))
	}
	for i, want := range []string{"[프로젝트 담당 · Claude Code abc12345]\n\n첫 줄\n둘째 줄", "[관리 · s1]"} {
		var body map[string]any
		if err := json.Unmarshal([]byte(reqs[i].Body), &body); err != nil || !reflect.DeepEqual(body, map[string]any{"body": want}) {
			t.Errorf("request %d body = %q; want {body: %q}", i, reqs[i].Body, want)
		}
		if reqs[i].Method != "POST" || reqs[i].Header.Get("Content-Type") != "application/json" {
			t.Errorf("request %d = %s, content type %q", i, reqs[i].Method, reqs[i].Header.Get("Content-Type"))
		}
	}
}

func TestCommentRefusesAHeaderThatIsNotOneBracketedLine(t *testing.T) {
	f := newFake(t)
	g := newClient(t, f)
	for _, h := range []string{"", "no brackets", "[a · b]\nmore", "[a]", "[a · b] trailing", " [a · b]", "[ · ]", "[a · b]\r"} {
		if err := g.Comment(bg, Ref{Repo: "o/r", Number: 1}, h, "text"); !errors.Is(err, ErrInvalidHeader) {
			t.Errorf("Comment(header %q) error = %v; want ErrInvalidHeader", h, err)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests sent for refused headers", n)
	}
}

func TestCloseSendsStateAndReason(t *testing.T) {
	f := newFake(t)
	f.reply("PATCH /repos/o/r/issues/10", 200, issueJSON(10, "x", "closed", nil))
	g := newClient(t, f)
	for _, c := range []struct {
		reason CloseReason
		want   map[string]any
	}{
		{Completed, map[string]any{"state": "closed", "state_reason": "completed"}},
		{NotPlanned, map[string]any{"state": "closed", "state_reason": "not_planned"}},
	} {
		if err := g.Close(bg, Ref{Repo: "o/r", Number: 10}, c.reason); err != nil {
			t.Fatal(err)
		}
		reqs := f.requests()
		last := reqs[len(reqs)-1]
		var body map[string]any
		if err := json.Unmarshal([]byte(last.Body), &body); err != nil || !reflect.DeepEqual(body, c.want) {
			t.Errorf("Close(%s) body = %q; want %v", c.reason, last.Body, c.want)
		}
		if last.Method != "PATCH" {
			t.Errorf("Close method = %s", last.Method)
		}
	}
}

func TestCloseRefusesAnUnknownReason(t *testing.T) {
	f := newFake(t)
	g := newClient(t, f)
	for _, r := range []CloseReason{"", "duplicate", "Completed"} {
		if err := g.Close(bg, Ref{Repo: "o/r", Number: 10}, r); !errors.Is(err, ErrInvalidReason) {
			t.Errorf("Close(%q) error = %v; want ErrInvalidReason", r, err)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests sent", n)
	}
}

func TestAddLabelPostsOneLabelAndKeepsTheOthers(t *testing.T) {
	f := newFake(t)
	f.reply("POST /repos/o/r/issues/10/labels", 200, labels("P1", "blocks-real-use"))
	g := newClient(t, f)
	if err := g.AddLabel(bg, Ref{Repo: "o/r", Number: 10}, "blocks-real-use"); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 1 || reqs[0].Method != "POST" || strings.TrimSpace(reqs[0].Body) != `{"labels":["blocks-real-use"]}` {
		t.Errorf("requests = %+v", reqs)
	}
	for _, l := range []string{"", "  ", "a\nb"} {
		if err := g.AddLabel(bg, Ref{Repo: "o/r", Number: 10}, l); !errors.Is(err, ErrInvalidLabel) {
			t.Errorf("AddLabel(%q) error = %v; want ErrInvalidLabel", l, err)
		}
	}
	if n := len(f.requests()); n != 1 {
		t.Errorf("%d requests; want 1", n)
	}
}

func TestEveryOperationNamesTheRepositoryItWasMadeFor(t *testing.T) {
	f := newFake(t)
	g := newClient(t, f)
	other := Ref{Repo: "x/y", Number: 1}
	calls := map[string]func() error{
		"Milestone": func() error { _, _, err := g.Milestone(bg, other); return err },
		"Issue":     func() error { _, err := g.Issue(bg, other); return err },
		"Comment":   func() error { return g.Comment(bg, other, "[a · b]", "t") },
		"Close":     func() error { return g.Close(bg, other, Completed) },
		"AddLabel":  func() error { return g.AddLabel(bg, other, "l") },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrOtherRepository) {
			t.Errorf("%s(other repo) error = %v; want ErrOtherRepository", name, err)
		}
	}
	for name, call := range map[string]func() error{
		"Issue 0":  func() error { _, err := g.Issue(bg, Ref{Repo: "o/r", Number: 0}); return err },
		"Close -1": func() error { return g.Close(bg, Ref{Repo: "o/r", Number: -1}, Completed) },
	} {
		if err := call(); !errors.Is(err, ErrInvalidRef) {
			t.Errorf("%s error = %v; want ErrInvalidRef", name, err)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests sent", n)
	}
}

func TestRepositoryNameMatchesWithoutRegardToCase(t *testing.T) {
	f := newFake(t)
	f.reply("GET /repos/o/r/issues/7", 200, issueJSON(7, "plain", "open", nil))
	f.reply("GET /repos/o/r/issues/7/sub_issues", 200, []any{})
	got, err := newClient(t, f).Issue(bg, Ref{Repo: "O/R", Number: 7})
	if err != nil || got.Ref != (Ref{Repo: "o/r", Number: 7}) {
		t.Errorf("Issue(O/R#7) = %+v, %v", got, err)
	}
}

func TestRequestsCarryTheTokenAndTheGitHubHeaders(t *testing.T) {
	f := newFake(t)
	f.reply("POST /repos/o/r/issues/1/labels", 200, labels("x"))
	asked := 0
	g, err := NewGitHub("o/r", TokenFunc(func(context.Context) (string, error) { asked++; return testToken + "\n", nil }), WithBaseURL(f.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := g.AddLabel(bg, Ref{Repo: "o/r", Number: 1}, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if asked != 2 {
		t.Errorf("token source asked %d times; want once per request", asked)
	}
	for _, r := range f.requests() {
		h := r.Header
		if h.Get("Authorization") != "Bearer "+testToken || h.Get("Accept") != "application/vnd.github+json" ||
			h.Get("X-GitHub-Api-Version") != "2022-11-28" || h.Get("User-Agent") == "" {
			t.Errorf("headers = %v", h)
		}
	}
}

func TestATokenSourceFailureStopsTheRequest(t *testing.T) {
	f := newFake(t)
	boom := errors.New("no gh")
	for name, src := range map[string]TokenSource{
		"error":        TokenFunc(func(context.Context) (string, error) { return "", boom }),
		"empty":        TokenFunc(func(context.Context) (string, error) { return "", nil }),
		"line break":   TokenFunc(func(context.Context) (string, error) { return "a\nX-Evil: 1", nil }),
		"control byte": TokenFunc(func(context.Context) (string, error) { return "a\x00b", nil }),
	} {
		g, err := NewGitHub("o/r", src, WithBaseURL(f.srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		if err := g.AddLabel(bg, Ref{Repo: "o/r", Number: 1}, "x"); err == nil {
			t.Errorf("%s: AddLabel succeeded", name)
		} else if name == "error" && !errors.Is(err, boom) {
			t.Errorf("%s: error = %v; want it to wrap the source's", name, err)
		}
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests sent", n)
	}
}

func TestACancelledContextSendsNothing(t *testing.T) {
	f := newFake(t)
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if _, err := newClient(t, f).Issue(ctx, Ref{Repo: "o/r", Number: 1}); !errors.Is(err, context.Canceled) {
		t.Errorf("Issue error = %v; want context.Canceled", err)
	}
	if n := len(f.requests()); n != 0 {
		t.Errorf("%d requests sent", n)
	}
}

// operations is every operation of the Tracker against issue 10, for the error tables.
func operations(g *GitHub) map[string]func() error {
	ref := Ref{Repo: "o/r", Number: 10}
	return map[string]func() error{
		"Milestone": func() error { _, _, err := g.Milestone(bg, Ref{Repo: "o/r", Number: 3}); return err },
		"Issue":     func() error { _, err := g.Issue(bg, ref); return err },
		"Comment":   func() error { return g.Comment(bg, ref, "[a · b]", "t") },
		"Close":     func() error { return g.Close(bg, ref, Completed) },
		"AddLabel":  func() error { return g.AddLabel(bg, ref, "l") },
	}
}

// failEverything answers every route of the operations with one failure.
func failEverything(f *fakeGitHub, status int, header map[string]string, body any) {
	h := func(w http.ResponseWriter, r *http.Request) {
		for k, v := range header {
			w.Header().Set(k, v)
		}
		writeJSON(w, status, body)
	}
	for _, route := range []string{
		"GET /repos/o/r/milestones/3", "GET /repos/o/r/issues", "GET /repos/o/r/issues/10", "GET /repos/o/r/issues/10/sub_issues",
		"POST /repos/o/r/issues/10/comments", "PATCH /repos/o/r/issues/10", "POST /repos/o/r/issues/10/labels",
	} {
		f.handle(route, h)
	}
}

func TestHTTPErrorsComeBackTyped(t *testing.T) {
	reset := time.Unix(1893456000, 0)
	cases := []struct {
		name    string
		status  int
		header  map[string]string
		message string
		is      error
		not     []error
	}{
		{"404", 404, nil, "Not Found", ErrNotFound, []error{ErrForbidden, ErrRateLimited}},
		{"410", 410, nil, "Gone", ErrNotFound, nil},
		{"401", 401, nil, "Bad credentials", ErrUnauthorized, []error{ErrNotFound}},
		{"403", 403, map[string]string{"X-RateLimit-Remaining": "4999"}, "Resource not accessible by personal access token", ErrForbidden, []error{ErrRateLimited, ErrNotFound}},
		{"403 primary rate limit", 403, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1893456000"}, "API rate limit exceeded for user", ErrRateLimited, []error{ErrForbidden}},
		{"403 secondary rate limit", 403, map[string]string{"Retry-After": "60"}, "You have exceeded a secondary rate limit.", ErrRateLimited, []error{ErrForbidden}},
		{"403 rate limit by message", 403, nil, "You have exceeded a secondary rate limit. Please wait", ErrRateLimited, nil},
		{"429", 429, map[string]string{"Retry-After": "30"}, "Too Many Requests", ErrRateLimited, nil},
	}
	for _, c := range cases {
		for name, op := range func() map[string]func() error {
			f := newFake(t)
			failEverything(f, c.status, c.header, map[string]any{"message": c.message, "documentation_url": "https://docs.github.com/rest"})
			return operations(newClient(t, f))
		}() {
			err := op()
			if !errors.Is(err, c.is) {
				t.Errorf("%s/%s: error = %v; want Is %v", c.name, name, err, c.is)
				continue
			}
			for _, n := range c.not {
				if errors.Is(err, n) {
					t.Errorf("%s/%s: error also matches %v", c.name, name, n)
				}
			}
			var ae *APIError
			var rl *RateLimitError
			if c.is == ErrRateLimited {
				if !errors.As(err, &rl) || rl.Status != c.status || rl.Message != c.message {
					t.Errorf("%s/%s: error = %#v; want a *RateLimitError of status %d", c.name, name, err, c.status)
					continue
				}
				if c.header["X-RateLimit-Reset"] != "" && !rl.Reset.Equal(reset) {
					t.Errorf("%s/%s: Reset = %v; want %v", c.name, name, rl.Reset, reset)
				}
				if c.header["Retry-After"] != "" && rl.RetryAfter != map[string]time.Duration{"60": time.Minute, "30": 30 * time.Second}[c.header["Retry-After"]] {
					t.Errorf("%s/%s: RetryAfter = %v", c.name, name, rl.RetryAfter)
				}
			} else if !errors.As(err, &ae) || ae.Status != c.status || ae.Message != c.message || ae.Op == "" {
				t.Errorf("%s/%s: error = %#v; want an *APIError of status %d", c.name, name, err, c.status)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Errorf("%s/%s: error text holds the token: %v", c.name, name, err)
			}
		}
	}
}

func TestOtherStatusesAreAPIErrorsOfNoKind(t *testing.T) {
	for _, status := range []int{422, 500, 502} {
		f := newFake(t)
		failEverything(f, status, nil, map[string]any{"message": "Validation Failed"})
		err := newClient(t, f).AddLabel(bg, Ref{Repo: "o/r", Number: 10}, "l")
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != status || errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrRateLimited) || errors.Is(err, ErrUnauthorized) {
			t.Errorf("status %d: error = %#v", status, err)
		}
	}
}

func TestAnErrorBodyThatIsNotJSONStillGivesAStatusError(t *testing.T) {
	f := newFake(t)
	f.handle("PATCH /repos/o/r/issues/10", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "<html>bad gateway</html>", http.StatusBadGateway)
	})
	err := newClient(t, f).Close(bg, Ref{Repo: "o/r", Number: 10}, Completed)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 502 {
		t.Errorf("error = %#v; want an *APIError of status 502", err)
	}
}

func TestASecondPageThatFailsFailsTheRead(t *testing.T) {
	f := newFake(t)
	f.reply("GET /repos/o/r/milestones/3", 200, map[string]any{"number": 3, "title": "m", "state": "open"})
	f.handle("GET /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `<`+f.srv.URL+`/repos/o/r/issues?milestone=3&state=all&per_page=100&page=2>; rel="next"`)
			writeJSON(w, 200, []any{issueJSON(10, "first", "open", nil)})
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "0")
		writeJSON(w, 403, map[string]any{"message": "API rate limit exceeded"})
	})
	_, issues, err := newClient(t, f).Milestone(bg, Ref{Repo: "o/r", Number: 3})
	if !errors.Is(err, ErrRateLimited) || len(issues) != 0 {
		t.Errorf("Milestone = %v, %v; want no issues and ErrRateLimited", issues, err)
	}
}

func TestTheTokenNeverReachesAnErrorMessage(t *testing.T) {
	f := newFake(t)
	failEverything(f, 401, nil, map[string]any{"message": "Bad credentials: " + testToken})
	for name, op := range operations(newClient(t, f)) {
		err := op()
		var ae *APIError
		if !errors.As(err, &ae) || strings.Contains(err.Error(), testToken) || strings.Contains(ae.Message, testToken) {
			t.Errorf("%s: error = %#v", name, err)
		}
	}
}

func TestAWriteIsNeverFollowedThroughARedirect(t *testing.T) {
	f := newFake(t)
	f.handle("PATCH /repos/o/r/issues/10", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", f.srv.URL+"/repos/o/moved/issues/10")
		w.WriteHeader(http.StatusMovedPermanently)
	})
	err := newClient(t, f).Close(bg, Ref{Repo: "o/r", Number: 10}, Completed)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 301 {
		t.Errorf("error = %#v; want an *APIError of status 301", err)
	}
	if n := len(f.requests()); n != 1 {
		t.Errorf("%d requests; want 1", n)
	}
}

func TestANextLinkToAnotherHostIsNotFollowed(t *testing.T) {
	var foreign []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreign = append(foreign, r.URL.String()+" "+r.Header.Get("Authorization"))
		writeJSON(w, 200, []any{})
	}))
	t.Cleanup(other.Close)
	f := newFake(t)
	f.reply("GET /repos/o/r/milestones/3", 200, map[string]any{"number": 3, "title": "m", "state": "open"})
	f.handle("GET /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<`+other.URL+`/steal>; rel="next"`)
		writeJSON(w, 200, []any{issueJSON(10, "first", "open", nil)})
	})
	if _, _, err := newClient(t, f).Milestone(bg, Ref{Repo: "o/r", Number: 3}); err == nil {
		t.Error("Milestone followed a next link to another host")
	}
	if len(foreign) != 0 {
		t.Errorf("another host received %v", foreign)
	}
}

func TestAPageLoopThatNeverEndsIsStopped(t *testing.T) {
	f := newFake(t)
	f.reply("GET /repos/o/r/milestones/3", 200, map[string]any{"number": 3, "title": "m", "state": "open"})
	f.handle("GET /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		next := url.URL{Scheme: "http", Host: r.Host, Path: r.URL.Path, RawQuery: "page=" + r.URL.Query().Get("page") + "x"}
		w.Header().Set("Link", `<`+next.String()+`>; rel="next"`)
		writeJSON(w, 200, []any{})
	})
	if _, _, err := newClient(t, f).Milestone(bg, Ref{Repo: "o/r", Number: 3}); err == nil {
		t.Error("Milestone read pages forever")
	}
	if n := len(f.requests()); n > 300 {
		t.Errorf("%d requests before stopping", n)
	}
}
