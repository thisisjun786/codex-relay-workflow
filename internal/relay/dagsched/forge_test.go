package dagsched

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

const forgeHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const forgeBase = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// ghScript answers the gh calls evidence.Collect makes about one pull request, with the shapes the GitHub API gives them. The projection test runs the real collector over it, so what is
// projected is what the collector produces, not what this package assumes it does.
type ghScript struct {
	pulls   []map[string]any // successive answers for the pull request (the last repeats)
	checks  []any
	rules   []any
	rulesOK bool // false: the rules endpoint fails
	pullsOK bool // false: the pull request endpoint fails
	// checkTotal, when set, is the total_count the check-run endpoint reports whatever it returns
	checkTotal int
	// statuses are the commit statuses (the combined status endpoint lists the newest of each context)
	statuses []any
}

func newGHScript() *ghScript {
	return &ghScript{rulesOK: true, pullsOK: true, rules: []any{map[string]any{"type": "required_status_checks", "parameters": map[string]any{"strict_required_status_checks_policy": false,
		"required_status_checks": []any{map[string]any{"context": "dev-gate"}}}}},
		checks: []any{map[string]any{"id": 11, "name": "dev-gate", "head_sha": forgeHead, "status": "completed", "conclusion": "success", "app": map[string]any{"id": 42}}}}
}

func (g *ghScript) pull(head string) map[string]any {
	return map[string]any{"number": 7, "html_url": "u", "state": "open", "merged": false, "draft": false, "head": map[string]any{"sha": head}, "base": map[string]any{"sha": forgeBase, "ref": "dev"},
		"mergeable": true, "mergeable_state": "clean"}
}

func (g *ghScript) runner(argv []string, _ time.Duration) (int, string, string, error) {
	encode := func(v any) (int, string, string, error) {
		b, _ := json.Marshal(v)
		return 0, string(b), "", nil
	}
	last := argv[len(argv)-1]
	page := func(items []any, key string, total int) map[string]any {
		values, _ := url.ParseQuery(strings.SplitN(last, "?", 2)[len(strings.SplitN(last, "?", 2))-1])
		number, _ := strconv.Atoi(values.Get("page"))
		size, _ := strconv.Atoi(values.Get("per_page"))
		if number < 1 {
			number = 1
		}
		if size < 1 {
			size = 100
		}
		start := min((number-1)*size, len(items))
		if total == 0 {
			total = len(items)
		}
		return map[string]any{"total_count": total, key: items[start:min(start+size, len(items))]}
	}
	if argv[1] == "api" && argv[2] == "graphql" {
		field := "reviewThreads"
		for _, a := range argv {
			if strings.HasPrefix(a, "query=") && strings.Contains(a, "query reviews") {
				field = "reviews"
			} else if strings.HasPrefix(a, "query=") && strings.Contains(a, "query comments") {
				field = "comments"
			}
		}
		return encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{field: map[string]any{"totalCount": 0,
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}, "nodes": []any{}}}}}})
	}
	switch {
	case strings.Contains(last, "/pulls/"):
		if !g.pullsOK {
			return 1, "", "boom", nil
		}
		p := g.pulls[0]
		if len(g.pulls) > 1 {
			g.pulls = g.pulls[1:]
		}
		return encode(p)
	case strings.Contains(last, "/git/ref/"):
		return encode(map[string]any{"ref": "refs/heads/dev"})
	case strings.Contains(last, "/rules/branches/"):
		if !g.rulesOK {
			return 1, "", "boom", nil
		}
		return encode(g.rules)
	case strings.Contains(last, "/actions/runs/") && strings.Contains(last, "/jobs"):
		return encode(map[string]any{"total_count": 0, "jobs": []any{}})
	case strings.Contains(last, "/actions/runs"):
		return encode(map[string]any{"total_count": 0, "workflow_runs": []any{}})
	case strings.Contains(last, "/check-runs"):
		return encode(page(g.checks, "check_runs", g.checkTotal))
	case strings.Contains(last, "/status"):
		return encode(page(g.statuses, "statuses", 0))
	}
	return 1, "", "unexpected " + last, nil
}

func (g *ghScript) read(t *testing.T) PullRequest {
	t.Helper()
	if len(g.pulls) == 0 {
		g.pulls = []map[string]any{g.pull(forgeHead)}
	}
	reader := ForgePullRequestReader(func(context.Context) evidence.Runner { return g.runner })
	pr, err := reader(contextBackground(), "owner/name", 7)
	if err != nil {
		t.Fatal(err)
	}
	return pr
}

// The collector reports unreadable, truncated or moved evidence as problems with a nil error. Each snapshot below is produced by the real collector over a scripted forge, projected,
// and then classified by the fail-closed rule: an unknown verdict is never an accepted PullRequest, a stale one is a refusal, and a failing check is a judgement that passes through
// with its checks.
func TestForgeProjectionFromRealSnapshot(t *testing.T) {
	t.Run("a clean pull request", func(t *testing.T) {
		pr := newGHScript().read(t)
		if pr.Verdict != "ready" || pr.State != "open" || pr.IsDraft || pr.HeadSHA != forgeHead || pr.BaseSHA != forgeBase || pr.BaseRef != "dev" || pr.Number != 7 || pr.Repository != "owner/name" {
			t.Fatalf("pull request = %+v", pr)
		}
		if len(pr.Checks) != 1 || pr.Checks[0].Name != "dev-gate" || pr.Checks[0].Conclusion != "success" || pr.Checks[0].HeadSHA != forgeHead || pr.Checks[0].Attempt != 1 || pr.Checks[0].RunID == "" {
			t.Fatalf("checks = %+v", pr.Checks)
		}
		if !pr.RequiredReadable || len(pr.RequiredDeclared) != 1 || pr.RequiredDeclared[0] != "dev-gate" || len(pr.CheckProblems) != 0 {
			t.Fatalf("required = %+v readable %v problems %v snapshot problems %+v", pr.RequiredDeclared, pr.RequiredReadable, pr.CheckProblems, pr.Problems)
		}
		if err := ClassifyPullRequest(pr); err != nil {
			t.Fatal(err)
		}
		if body := EvidenceBodyOf(pr); len(body.Checks) != 1 || EvidenceDigest(body) == "" {
			t.Fatalf("evidence = %+v", body)
		}
	})
	t.Run("a failing required check is a judgement, not a read failure", func(t *testing.T) {
		g := newGHScript()
		g.checks = []any{map[string]any{"id": 11, "name": "dev-gate", "head_sha": forgeHead, "status": "completed", "conclusion": "failure", "app": map[string]any{"id": 42}}}
		pr := g.read(t)
		if pr.Verdict != "not_ready" || len(pr.Checks) != 1 || pr.Checks[0].Conclusion != "failure" {
			t.Fatalf("pull request = %+v", pr)
		}
		if err := ClassifyPullRequest(pr); err != nil {
			t.Fatalf("a not_ready snapshot was refused as unreadable: %v", err)
		}
	})
	t.Run("the candidate moved while it was read", func(t *testing.T) {
		g := newGHScript()
		g.pulls = []map[string]any{g.pull(forgeHead), g.pull("cccccccccccccccccccccccccccccccccccccccc")}
		pr := g.read(t)
		err := ClassifyPullRequest(pr)
		if pr.Verdict != "stale" || refusalReason(err) != "merge_candidate_moved" {
			t.Fatalf("verdict %s, classification %v", pr.Verdict, err)
		}
	})
	t.Run("the check list is shorter than the forge says", func(t *testing.T) {
		g := newGHScript()
		g.checkTotal = 5
		pr := g.read(t)
		err := ClassifyPullRequest(pr)
		if pr.Verdict != "unknown" || err == nil || !strings.Contains(err.Error(), "enumeration") || errors.Is(err, nil) && refusalReason(err) == "" {
			t.Fatalf("verdict %s, classification %v", pr.Verdict, err)
		}
		if refusalReason(err) != "not a refusal: "+err.Error() {
			t.Fatalf("an unknown snapshot was classified as a refusal: %v", err)
		}
		if len(pr.Checks) != 1 || pr.Checks[0].Conclusion != "success" {
			t.Fatalf("the checks it did read: %+v (they hold successes, which is exactly why the verdict must decide)", pr.Checks)
		}
	})
	t.Run("the pull request cannot be read", func(t *testing.T) {
		g := newGHScript()
		g.pullsOK = false
		pr := g.read(t)
		if err := ClassifyPullRequest(pr); pr.Verdict != "unknown" || err == nil {
			t.Fatalf("verdict %s, classification %v", pr.Verdict, err)
		}
	})
	t.Run("the required list cannot be read", func(t *testing.T) {
		g := newGHScript()
		g.rulesOK = false
		pr := g.read(t)
		if pr.RequiredReadable {
			t.Fatalf("a rules endpoint that failed reads as a declared list: %+v", pr)
		}
	})
	t.Run("checks of another head are not evidence of this one", func(t *testing.T) {
		g := newGHScript()
		g.checks = append(g.checks, map[string]any{"id": 12, "name": "old", "head_sha": "dddddddddddddddddddddddddddddddddddddddd", "status": "completed", "conclusion": "success", "app": map[string]any{"id": 42}})
		pr := g.read(t)
		for _, c := range EvidenceBodyOf(pr).Checks {
			if c.HeadSHA != pr.HeadSHA {
				t.Fatalf("evidence carries a check of %s for head %s", c.HeadSHA, pr.HeadSHA)
			}
		}
	})
}
