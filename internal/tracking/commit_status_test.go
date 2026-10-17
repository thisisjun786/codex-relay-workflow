package tracking

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

func statusHead() string { return strings.Repeat("a", 40) }

func statusClient(t *testing.T, base string) *GitHub {
	t.Helper()
	g, err := NewGitHub("o/r", StaticToken(trackingfake.Token), WithBaseURL(base))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestRequiredStatusPost(t *testing.T) {
	f := trackingfake.New(t, "o/r")
	g := statusClient(t, f.URL)
	head := statusHead()
	if err := g.SetCommitStatus(context.Background(), head, CommitStatusUpdate{State: "success", Context: "dev-gate", TargetURL: "https://github.com/o/r/actions/runs/6", Description: "Verified integration tree"}); err != nil {
		t.Fatalf("SetCommitStatus: %v", err)
	}
	reqs := f.Requests()
	if len(reqs) != 1 || reqs[0] != "POST /repos/o/r/statuses/"+head {
		t.Fatalf("requests = %v", reqs)
	}
	got := f.Statuses(head)
	if len(got) != 1 || got[0].Context != "dev-gate" || got[0].State != "success" || got[0].TargetURL != "https://github.com/o/r/actions/runs/6" {
		t.Errorf("statuses = %+v", got)
	}
}

func TestRequiredStatusNoTargetURL(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+trackingfake.Token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "success", "context": "dev-gate"})
	}))
	t.Cleanup(srv.Close)
	g := statusClient(t, srv.URL)
	if err := g.SetCommitStatus(context.Background(), statusHead(), CommitStatusUpdate{State: "success", Context: "dev-gate"}); err != nil {
		t.Fatalf("SetCommitStatus: %v", err)
	}
	if _, ok := body["target_url"]; ok {
		t.Errorf("the body carries a target_url: %v", body)
	}
}

func TestRequiredStatusInputs(t *testing.T) {
	f := trackingfake.New(t, "o/r")
	asked := 0
	g, err := NewGitHub("o/r", TokenFunc(func(context.Context) (string, error) { asked++; return trackingfake.Token, nil }), WithBaseURL(f.URL))
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]struct {
		head   string
		status CommitStatusUpdate
	}{
		"short head":      {statusHead()[:20], CommitStatusUpdate{State: "success", Context: "dev-gate"}},
		"upper head":      {strings.ToUpper(statusHead()), CommitStatusUpdate{State: "success", Context: "dev-gate"}},
		"41 head":         {statusHead() + "a", CommitStatusUpdate{State: "success", Context: "dev-gate"}},
		"bad state":       {statusHead(), CommitStatusUpdate{State: "done", Context: "dev-gate"}},
		"empty context":   {statusHead(), CommitStatusUpdate{State: "success"}},
		"space context":   {statusHead(), CommitStatusUpdate{State: "success", Context: " dev-gate"}},
		"control context": {statusHead(), CommitStatusUpdate{State: "success", Context: "dev\u0007gate"}},
		"relative url":    {statusHead(), CommitStatusUpdate{State: "success", Context: "dev-gate", TargetURL: "/runs/6"}},
		"ftp url":         {statusHead(), CommitStatusUpdate{State: "success", Context: "dev-gate", TargetURL: "ftp://x/y"}},
		"userinfo url":    {statusHead(), CommitStatusUpdate{State: "success", Context: "dev-gate", TargetURL: "https://u:p@h/"}},
		"control url":     {statusHead(), CommitStatusUpdate{State: "success", Context: "dev-gate", TargetURL: "https://h/\u0001"}},
		"secret context":  {statusHead(), CommitStatusUpdate{State: "success", Context: "ghp_" + strings.Repeat("a", 36)}},
	} {
		if err := g.SetCommitStatus(context.Background(), s.head, s.status); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if asked != 0 {
		t.Errorf("the token was asked %d times for refused inputs", asked)
	}
	if len(f.Requests()) != 0 {
		t.Errorf("requests were sent for refused inputs: %v", f.Requests())
	}
}

func TestRequiredStatusSHA256Head(t *testing.T) {
	f := trackingfake.New(t, "o/r")
	g := statusClient(t, f.URL)
	head := strings.Repeat("b", 64)
	if err := g.SetCommitStatus(context.Background(), head, CommitStatusUpdate{State: "pending", Context: "dev-gate"}); err != nil {
		t.Fatalf("64-hex head: %v", err)
	}
}

func TestRequiredStatusMismatchedAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "failure", "context": "dev-gate"})
	}))
	t.Cleanup(srv.Close)
	g := statusClient(t, srv.URL)
	if err := g.SetCommitStatus(context.Background(), statusHead(), CommitStatusUpdate{State: "success", Context: "dev-gate"}); err == nil {
		t.Error("a mismatched answer was accepted")
	}
}

func TestRequiredStatusHTTPError(t *testing.T) {
	for _, code := range []int{403, 422} {
		f := trackingfake.New(t, "o/r")
		f.StatusCode = code
		g := statusClient(t, f.URL)
		if err := g.SetCommitStatus(context.Background(), statusHead(), CommitStatusUpdate{State: "success", Context: "dev-gate"}); err == nil {
			t.Errorf("a %d status write was accepted", code)
		}
	}
}

func TestRequiredStatusMalformedAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("not json"))
	}))
	t.Cleanup(srv.Close)
	g := statusClient(t, srv.URL)
	if err := g.SetCommitStatus(context.Background(), statusHead(), CommitStatusUpdate{State: "success", Context: "dev-gate"}); err == nil {
		t.Error("a malformed answer was accepted")
	}
}
