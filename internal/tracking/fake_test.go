package tracking

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

const testToken = "ghp_testtoken123"

type recorded struct {
	Method, Path, RawQuery string
	Header                 http.Header
	Body                   string
}

// fakeGitHub is an httptest GitHub: a route is "METHOD /path", every request is recorded.
type fakeGitHub struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	reqs   []recorded
	routes map[string]http.HandlerFunc
}

func newFake(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t, routes: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(body)})
		h := f.routes[r.Method+" "+r.URL.Path]
		f.mu.Unlock()
		if h == nil {
			t.Errorf("unrouted request %s %s", r.Method, r.URL.RequestURI())
			http.Error(w, "unrouted", http.StatusInternalServerError)
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) handle(route string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[route] = h
}

// reply routes to a fixed status and JSON value.
func (f *fakeGitHub) reply(route string, status int, v any) {
	f.handle(route, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, status, v) })
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGitHub) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

// newClient is a tracker for "o/r" against the fake.
func newClient(t *testing.T, f *fakeGitHub) *GitHub {
	t.Helper()
	g, err := NewGitHub("o/r", StaticToken(testToken), WithBaseURL(f.srv.URL))
	if err != nil {
		t.Fatalf("NewGitHub: %v", err)
	}
	return g
}

// The fake of GitHub serves the checks that a test set for a head commit, and only for that head; a head with none set answers
// what it answered before SetChecks existed (no check run, a pending status), which reads as CheckPending.
func TestFakeServesTheChecksSetForAHead(t *testing.T) {
	const (
		headA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		headB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	fake := trackingfake.New(t, "o/r")
	heads := map[string]string{"head-a": headA, "head-b": headB, "head-c": strings.Repeat("c", 40)}
	fake.HeadSHA = func(branch string) string { return heads[branch] }
	g, err := NewGitHub("o/r", StaticToken(trackingfake.Token), WithBaseURL(fake.URL))
	if err != nil {
		t.Fatalf("NewGitHub: %v", err)
	}
	ctx := context.Background()
	open := func(head string) Ref {
		t.Helper()
		pr, err := g.OpenPullRequest(ctx, NewPullRequest{Head: head, Base: "dev", Title: "t " + head, Issues: []Ref{{Repo: "o/r", Number: 1}}})
		if err != nil {
			t.Fatalf("open %s: %v", head, err)
		}
		return pr.Ref
	}
	a, b, c := open("head-a"), open("head-b"), open("head-c")
	fake.SetChecks(headA,
		[]trackingfake.FakeCheckRun{{Name: "build", Status: "completed", Conclusion: "failure", HTMLURL: "https://example.test/build"}},
		[]trackingfake.FakeStatus{{Context: "ci/lint", State: "success"}})
	fake.SetChecks(headB,
		[]trackingfake.FakeCheckRun{{Name: "build", Status: "completed", Conclusion: "success"}, {Name: "docs", Status: "completed", Conclusion: "skipped"}},
		[]trackingfake.FakeStatus{{Context: "ci/lint", State: "success"}})

	got, err := g.Checks(ctx, a)
	if err != nil {
		t.Fatalf("Checks a: %v", err)
	}
	if got.Head != headA || got.State != CheckFailure || len(got.Checks) != 2 {
		t.Fatalf("head a: %+v, want head %s, failure, 2 checks", got, headA)
	}
	if run := got.Checks[0]; run.Kind != CheckRun || run.Name != "build" || run.Conclusion != "failure" || run.State != CheckFailure || run.URL != "https://example.test/build" {
		t.Errorf("head a run: %+v", run)
	}
	if st := got.Checks[1]; st.Kind != CommitStatus || st.Name != "ci/lint" || st.State != CheckSuccess {
		t.Errorf("head a status: %+v", st)
	}

	got, err = g.Checks(ctx, b)
	if err != nil {
		t.Fatalf("Checks b: %v", err)
	}
	if got.Head != headB || got.State != CheckSuccess || len(got.Checks) != 3 {
		t.Errorf("head b: %+v, want head %s, success, 3 checks", got, headB)
	}

	got, err = g.Checks(ctx, c)
	if err != nil {
		t.Fatalf("Checks c: %v", err)
	}
	if got.State != CheckNone && got.State != CheckPending || len(got.Checks) != 0 {
		t.Errorf("an unset head: %+v, want no check", got)
	}

	// Setting again replaces what was set; setting none clears the head back to the unset answer.
	fake.SetChecks(headA, []trackingfake.FakeCheckRun{{Name: "build", Status: "in_progress"}}, nil)
	if got, err = g.Checks(ctx, a); err != nil || got.State != CheckPending || len(got.Checks) != 1 {
		t.Errorf("head a after a reset: %+v, %v, want pending with 1 check", got, err)
	}
	fake.SetChecks(headA, nil, nil)
	if got, err = g.Checks(ctx, a); err != nil || len(got.Checks) != 0 {
		t.Errorf("head a cleared: %+v, %v, want no check", got, err)
	}
}
