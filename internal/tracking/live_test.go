//go:build live

package tracking

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

// readOnly lets a GET through and fails any other method, so the live test cannot write.
type readOnly struct{ next http.RoundTripper }

func (r readOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return nil, fmt.Errorf("live test refuses %s %s: it only reads", req.Method, req.URL.Path)
	}
	return r.next.RoundTrip(req)
}

func liveNumber(t *testing.T, env string, def int) int {
	t.Helper()
	v := os.Getenv(env)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		t.Fatalf("%s=%q is not a number above 0", env, v)
	}
	return n
}

// TestLiveReadCrwWork reads a milestone with its issues and one issue of thisisjun786/crw-work with
// the production token source. It runs only with -tags live (CI does not) and writes nothing.
// CRW_TRACKING_LIVE_MILESTONE and CRW_TRACKING_LIVE_ISSUE pick other numbers.
func TestLiveReadCrwWork(t *testing.T) {
	const repo = "thisisjun786/crw-work"
	g, err := NewGitHub(repo, DefaultTokenSource(), WithHTTPClient(&http.Client{
		Timeout: 30 * time.Second, Transport: readOnly{http.DefaultTransport},
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	var tr Tracker = g
	ms, issues, err := tr.Milestone(ctx, Ref{repo, liveNumber(t, "CRW_TRACKING_LIVE_MILESTONE", 3)})
	if err != nil {
		t.Fatalf("Milestone: %v", err)
	}
	if ms.Title == "" || len(issues) == 0 {
		t.Fatalf("milestone %+v has %d issues", ms, len(issues))
	}
	if got := len(issues); got != ms.OpenIssues+ms.ClosedIssues {
		t.Errorf("milestone says %d open + %d closed issues, the read found %d", ms.OpenIssues, ms.ClosedIssues, got)
	}
	t.Logf("milestone %s %q (%s): %d issues", ms.Ref, ms.Title, ms.State, len(issues))
	for _, is := range issues {
		if is.Ref.Repo != repo || is.Title == "" || is.Milestone != ms.Ref.Number || (is.State != StateOpen && is.State != StateClosed) {
			t.Errorf("issue %+v does not belong to the milestone read", is)
		}
	}

	ref := Ref{repo, liveNumber(t, "CRW_TRACKING_LIVE_ISSUE", 49)}
	is, err := tr.Issue(ctx, ref)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if is.Ref != ref || is.Title == "" || is.Body == "" || len(is.Labels) == 0 || (is.State != StateOpen && is.State != StateClosed) {
		t.Errorf("issue %+v is missing a field", is)
	}
	t.Logf("issue %s %q (%s): labels %v, blocked by %v, %d sub-issues", is.Ref, is.Title, is.State, is.Labels, is.BlockedBy, len(is.SubIssues))

	if _, err := tr.Issue(ctx, Ref{repo, 999999}); !isNotFound(err) {
		t.Errorf("Issue(999999) error = %v; want not found", err)
	}
	if err := tr.AddLabel(ctx, ref, "live-test-must-not-write"); err == nil {
		t.Error("the read-only transport let a write through")
	}
}

func isNotFound(err error) bool { return err != nil && errors.Is(err, ErrNotFound) }
