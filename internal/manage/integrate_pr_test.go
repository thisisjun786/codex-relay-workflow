package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/integrator"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

// CRW-1355: `crw manage integrate --enqueue-pr OWNER/NAME#N` puts a project session's pull request into the dev queue through the
// tracking adapter of the configured pull_request_repo, and a pass of the queue reads its pull requests through the same adapter.
func TestIntegrateEnqueuesAPullRequestOfTheConfiguredRepository(t *testing.T) {
	head := strings.Repeat("c", 40)
	fake := trackingfake.New(t, "o/r")
	fake.HeadSHA = func(string) string { return head }
	gh, err := tracking.NewGitHub("o/r", tracking.StaticToken(trackingfake.Token), tracking.WithBaseURL(fake.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gh.OpenPullRequest(context.Background(), tracking.NewPullRequest{Head: "crw/m1", Base: "dev", Title: "M1", Issues: []tracking.Ref{{Repo: "o/r", Number: 10}}}); err != nil {
		t.Fatal(err)
	}
	asked := []string{}
	previous := integratePullRequests
	integratePullRequests = func(repo string) (integrator.PullRequestSource, error) {
		asked = append(asked, repo)
		return tracking.NewGitHub(repo, tracking.StaticToken(trackingfake.Token), tracking.WithBaseURL(fake.URL))
	}
	t.Cleanup(func() { integratePullRequests = previous })

	dir := t.TempDir()
	var section map[string]any
	if err := json.Unmarshal([]byte(integrateTestSection(dir)), &section); err != nil {
		t.Fatal(err)
	}
	section["pull_request_repo"] = "o/r"
	data, _ := json.Marshal(section)
	code, out, errOut := integrateTestRun(t, string(data), "--enqueue-pr", "o/r#1")
	if code != 0 || errOut != "" {
		t.Fatalf("--enqueue-pr = %d %q %q", code, out, errOut)
	}
	var ans struct {
		Enqueued integrator.Entry `json:"enqueued"`
	}
	if err := json.Unmarshal([]byte(out), &ans); err != nil || ans.Enqueued.Name != "pr-1" || ans.Enqueued.Head != head || ans.Enqueued.PR != 1 {
		t.Fatalf("answer %q: %v", out, err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "queue", "pending", "pr-1"))
	if err != nil || string(got) != head+"\n" {
		t.Fatalf("pending/pr-1 = %q %v", got, err)
	}
	if strings.Join(asked, ",") != "o/r" {
		t.Errorf("the adapter was made for %v", asked)
	}
	// the dry run plans the pull request alone and reads no GitHub
	before := len(fake.Requests())
	code, out, _ = integrateTestRun(t, string(data), "--dry-run")
	if code != 0 || !strings.Contains(out, `"plan":["pr-1=`+head+`"]`) || len(fake.Requests()) != before {
		t.Errorf("--dry-run = %d %s (%d requests)", code, out, len(fake.Requests())-before)
	}

	for name, tc := range map[string]struct {
		section string
		args    []string
		want    string
	}{
		"no value":                {string(data), []string{"--enqueue-pr"}, "expected one argument"},
		"with watch":              {string(data), []string{"--enqueue-pr", "o/r#1", "--watch"}, "cannot be combined"},
		"with dry run":            {string(data), []string{"--enqueue-pr", "o/r#1", "--dry-run"}, "cannot be combined"},
		"no pull_request_repo":    {integrateTestSection(dir), []string{"--enqueue-pr", "o/r#1"}, "pull_request_repo"},
		"another repository":      {string(data), []string{"--enqueue-pr", "x/y#1"}, "not a pull request of o/r"},
		"a bad pull_request_repo": {strings.Replace(string(data), `"o/r"`, `"o/r#1"`, 1), []string{"--enqueue-pr", "o/r#1"}, "pull_request_repo must be owner/name"},
	} {
		code, out, errOut := integrateTestRun(t, tc.section, tc.args...)
		if code != usageExit || out != "" || !strings.Contains(errOut, tc.want) {
			t.Errorf("%s: code %d stdout %q stderr %q", name, code, out, errOut)
		}
	}
}
