package tracking

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
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
