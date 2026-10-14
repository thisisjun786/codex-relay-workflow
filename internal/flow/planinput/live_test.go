//go:build live

package planinput

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// readOnly lets a GET through and fails any other method, so the live test cannot write.
type readOnly struct{ next http.RoundTripper }

func (r readOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return nil, fmt.Errorf("live test refuses %s %s: it only reads", req.Method, req.URL.Path)
	}
	return r.next.RoundTrip(req)
}

// TestLiveCompileDryRunOfACrwWorkMilestone (CRW-1343) reads a small milestone of thisisjun786/crw-work with the production token source,
// builds the compile input from it and compiles it with a dry run. It runs only with -tags live (CI does not) and writes nothing; the
// milestone number is CRW_TRACKING_LIVE_MILESTONE (default 3). The manual form of the same check is
//
//	go run ./cmd/crw flow plan-input --milestone thisisjun786/crw-work#3 --target-repository /srv/checkout --base-ref dev --envelope
//	go run ./cmd/crw flow plan-input --milestone thisisjun786/crw-work#3 --target-repository /srv/checkout --base-ref dev | go run ./cmd/crw flow compile --dry-run -
func TestLiveCompileDryRunOfACrwWorkMilestone(t *testing.T) {
	const slug = "thisisjun786/crw-work"
	number := 3
	if v := os.Getenv("CRW_TRACKING_LIVE_MILESTONE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("CRW_TRACKING_LIVE_MILESTONE=%q is not a number above 0", v)
		}
		number = n
	}
	g, err := tracking.NewGitHub(slug, tracking.DefaultTokenSource(), tracking.WithHTTPClient(&http.Client{
		Timeout: 30 * time.Second, Transport: readOnly{http.DefaultTransport},
	}))
	if err != nil {
		t.Fatal(err)
	}
	ref := tracking.Ref{Repo: slug, Number: number}
	res, err := Build(bg, g, ref, Options{Target: &compile.Target{Repository: "/srv/checkout", BaseRef: "dev"}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, w := range res.Warnings {
		t.Logf("warning [%s] %s: %s", w.Rule, w.Issue, w.Detail)
	}
	p := res.Input.Projects[0]
	if want, _ := EncodeKey(ref); p.Key != want || res.Refs[p.Key] != ref.String() || len(p.Issues) == 0 {
		t.Fatalf("project %q (want %q) with %d issues", p.Key, want, len(p.Issues))
	}
	plan, err := compile.Compile(res.Input, compile.Options{DryRun: true})
	if err != nil {
		t.Fatalf("compile --dry-run: %v", err)
	}
	t.Logf("milestone %s: %d open issues compile; width %d, critical path %v", ref, len(p.Issues), plan.Plans[0].Preview.ParallelWidth, plan.Plans[0].Preview.CriticalPath)
}
