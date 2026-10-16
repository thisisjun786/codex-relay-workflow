// Package trackingfake is a fake GitHub for the tests of the packages that use the tracking adapter (internal/tracking): an
// httptest server over one repository that keeps pull requests and comments in memory and answers the REST routes the adapter's
// pull request operations and its comment use (write and list). It never reaches a real GitHub; a test points tracking.WithBaseURL at URL.
package trackingfake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Token is the token the fake accepts; any other is a 401.
const Token = "ghp_trackingfake"

// Pull is a pull request the fake keeps.
type Pull struct {
	Number                  int
	Title, Body, Head, Base string
	State                   string
	Merged, Draft           bool
	// HeadRepo is the repository the head branch is in: a pull request the fake opens is in the fake's own repository, and an
	// empty HeadRepo answers head.repo null, as GitHub answers for a fork that was deleted.
	HeadRepo string
}

// Comment is a comment the fake keeps.
type Comment struct {
	ID     int
	Number int
	Body   string
}

// FakeCheckRun is a check run of a head commit, in the fields the adapter reads: Status is "queued", "in_progress" or "completed";
// Conclusion is the result of a completed one.
type FakeCheckRun struct {
	Name, Status, Conclusion, HTMLURL string
}

// FakeStatus is a commit status of a head commit; State is "success", "pending", "failure" or "error".
type FakeStatus struct {
	Context, State, TargetURL string
}

type fakeChecks struct {
	runs     []FakeCheckRun
	statuses []FakeStatus
}

// Server is the fake.
type Server struct {
	Repo string
	URL  string
	// HeadSHA answers the commit a branch of the repository is at, read at every read of a pull request as GitHub reads it; a
	// test points it at its remote. Nil or "" answers 40 zeros.
	HeadSHA func(branch string) string

	srv      *httptest.Server
	mu       sync.Mutex
	next     int
	pulls    map[int]*Pull
	comments []Comment
	lastID   int
	requests []string
	checks   map[string]fakeChecks
}

// New starts the fake for the repository "owner/name"; it stops with the test.
func New(t *testing.T, repo string) *Server {
	t.Helper()
	s := &Server{Repo: repo, next: 1, pulls: map[int]*Pull{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL
	t.Cleanup(s.srv.Close)
	return s
}

var (
	pullsRoute    = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/pulls$`)
	pullRoute     = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/pulls/([0-9]+)$`)
	commentsRoute = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/issues/([0-9]+)/comments$`)
	checksRoute   = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/commits/([0-9a-f]{40})/(check-runs|status)$`)
)

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+Token {
		reply(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})
		return
	}
	path := r.URL.Path
	repoOK := func(m []string) bool { return m != nil && strings.EqualFold(m[1], s.Repo) }
	switch m := pullsRoute.FindStringSubmatch(path); {
	case r.Method == http.MethodPost && repoOK(m):
		var in struct {
			Head, Base, Title, Body string
			Draft                   bool
		}
		if json.Unmarshal(body, &in) != nil || in.Head == "" || in.Base == "" || in.Title == "" {
			reply(w, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed"})
			return
		}
		s.mu.Lock()
		for _, p := range s.pulls {
			if p.State == "open" && p.Head == in.Head && p.Base == in.Base {
				s.mu.Unlock()
				reply(w, http.StatusUnprocessableEntity, map[string]any{"message": "A pull request already exists for " + in.Head})
				return
			}
		}
		p := &Pull{Number: s.next, Title: in.Title, Body: in.Body, Head: in.Head, Base: in.Base, State: "open", Draft: in.Draft, HeadRepo: s.Repo}
		s.next++
		s.pulls[p.Number] = p
		out := s.pullJSON(p)
		s.mu.Unlock()
		reply(w, http.StatusCreated, out)
		return
	}
	if m := pullRoute.FindStringSubmatch(path); r.Method == http.MethodGet && repoOK(m) {
		n, _ := strconv.Atoi(m[2])
		s.mu.Lock()
		p := s.pulls[n]
		var out map[string]any
		if p != nil {
			out = s.pullJSON(p)
		}
		s.mu.Unlock()
		if p == nil {
			reply(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		reply(w, http.StatusOK, out)
		return
	}
	if m := commentsRoute.FindStringSubmatch(path); r.Method == http.MethodGet && repoOK(m) {
		n, _ := strconv.Atoi(m[2])
		s.mu.Lock()
		_, ok := s.pulls[n]
		out := []map[string]any{}
		for _, c := range s.comments {
			if c.Number == n {
				out = append(out, map[string]any{"id": c.ID, "body": c.Body})
			}
		}
		s.mu.Unlock()
		if !ok {
			reply(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		reply(w, http.StatusOK, out)
		return
	}
	if m := commentsRoute.FindStringSubmatch(path); r.Method == http.MethodPost && repoOK(m) {
		n, _ := strconv.Atoi(m[2])
		var in struct{ Body string }
		if json.Unmarshal(body, &in) != nil || in.Body == "" {
			reply(w, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed"})
			return
		}
		s.mu.Lock()
		_, ok := s.pulls[n]
		id := 0
		if ok {
			s.lastID++
			id = s.lastID
			s.comments = append(s.comments, Comment{ID: id, Number: n, Body: in.Body})
		}
		s.mu.Unlock()
		if !ok {
			reply(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
			return
		}
		reply(w, http.StatusCreated, map[string]any{"id": id, "body": in.Body})
		return
	}
	if m := checksRoute.FindStringSubmatch(path); r.Method == http.MethodGet && repoOK(m) {
		s.mu.Lock()
		set, ok := s.checks[m[2]]
		s.mu.Unlock()
		if m[3] == "check-runs" {
			runs := []any{}
			for _, r := range set.runs {
				run := map[string]any{"name": r.Name, "status": r.Status, "html_url": r.HTMLURL}
				if r.Conclusion != "" {
					run["conclusion"] = r.Conclusion
				} else {
					run["conclusion"] = nil
				}
				runs = append(runs, run)
			}
			reply(w, http.StatusOK, map[string]any{"total_count": len(runs), "check_runs": runs})
		} else {
			statuses := []any{}
			for _, st := range set.statuses {
				statuses = append(statuses, map[string]any{"context": st.Context, "state": st.State, "target_url": st.TargetURL})
			}
			reply(w, http.StatusOK, map[string]any{"state": combinedState(ok, set.statuses), "statuses": statuses})
		}
		return
	}
	reply(w, http.StatusNotFound, map[string]any{"message": "Not Found: " + r.Method + " " + path})
}

// combinedState is the "state" GitHub gives for the statuses of a commit: pending when there is none (and for a head nothing was
// set for), failure when one failed or errored, pending when one is pending, else success.
func combinedState(set bool, statuses []FakeStatus) string {
	if !set || len(statuses) == 0 {
		return "pending"
	}
	out := "success"
	for _, st := range statuses {
		switch st.State {
		case "success":
		case "pending":
			if out == "success" {
				out = "pending"
			}
		default:
			return "failure"
		}
	}
	return out
}

// pullJSON is the GitHub shape of p; the caller holds the lock.
func (s *Server) pullJSON(p *Pull) map[string]any {
	sha := strings.Repeat("0", 40)
	if s.HeadSHA != nil {
		if got := s.HeadSHA(p.Head); got != "" {
			sha = got
		}
	}
	repo := map[string]any{"full_name": s.Repo}
	var headRepo any // nil: head.repo null
	if p.HeadRepo != "" {
		headRepo = map[string]any{"full_name": p.HeadRepo}
	}
	return map[string]any{
		"number": p.Number, "title": p.Title, "body": p.Body, "state": p.State, "merged": p.Merged, "draft": p.Draft,
		"html_url": fmt.Sprintf("https://github.com/%s/pull/%d", s.Repo, p.Number),
		"head":     map[string]any{"ref": p.Head, "sha": sha, "repo": headRepo},
		"base":     map[string]any{"ref": p.Base, "repo": repo},
	}
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Pulls is a copy of every pull request, by number.
func (s *Server) Pulls() map[int]Pull {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int]Pull{}
	for n, p := range s.pulls {
		out[n] = *p
	}
	return out
}

// SetPull changes a kept pull request (a test closes or merges it).
func (s *Server) SetPull(n int, change func(p *Pull)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.pulls[n]; p != nil {
		change(p)
	}
}

// Comments is every comment written on the pull request n, in order.
func (s *Server) Comments(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.comments {
		if c.Number == n {
			out = append(out, c.Body)
		}
	}
	return out
}

// DropComments removes every comment written on the pull request n and no other: a test models "the pass died before the
// comment call" with it. The comments of other pull requests stay.
func (s *Server) DropComments(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.comments[:0:0]
	for _, c := range s.comments {
		if c.Number != n {
			kept = append(kept, c)
		}
	}
	s.comments = kept
}

// Requests is "METHOD /path" of every request, in order.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// SetChecks sets the check runs and commit statuses the fake answers for the commit sha (40 lowercase hex characters, as the
// adapter asks for the head it read). It replaces what was set for sha; no runs and no statuses clear it. A sha nothing was set
// for answers no check run and a pending status, as the fake always did.
func (s *Server) SetChecks(sha string, runs []FakeCheckRun, statuses []FakeStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(runs) == 0 && len(statuses) == 0 {
		delete(s.checks, sha)
		return
	}
	if s.checks == nil {
		s.checks = map[string]fakeChecks{}
	}
	s.checks[sha] = fakeChecks{runs: append([]FakeCheckRun(nil), runs...), statuses: append([]FakeStatus(nil), statuses...)}
}
