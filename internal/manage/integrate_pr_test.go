package manage

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/integrator"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

// CRW-1355/CRW-1444: `crw manage integrate --enqueue-pr OWNER/NAME#N --head SHA --release-impact TEXT` decides the pull
// request (head match, base tip, checks, queue order, release impact) through crw flow pr decide's decision and enqueues it
// only on all green; the queue's pass then lands it.

// prGit runs git under an isolated identity and fails the test on error.
func prGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestIntegrateEnqueuesAPullRequestOfTheConfiguredRepository(t *testing.T) {
	dir := t.TempDir()
	bare, work, checkout := filepath.Join(dir, "remote.git"), filepath.Join(dir, "src"), filepath.Join(dir, "checkout")
	prGit(t, dir, "init", "-q", "--bare", "-b", "dev", bare)
	prGit(t, dir, "init", "-q", "-b", "dev", work)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prGit(t, work, "add", "-A")
	prGit(t, work, "commit", "-q", "-m", "base")
	prGit(t, work, "remote", "add", "origin", bare)
	prGit(t, work, "push", "-q", "origin", "dev")
	prGit(t, work, "checkout", "-q", "-b", "crw/m1")
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prGit(t, work, "add", "-A")
	prGit(t, work, "commit", "-q", "-m", "m1")
	prGit(t, work, "push", "-q", "origin", "crw/m1")
	prGit(t, dir, "clone", "-q", bare, checkout)
	head := prGit(t, bare, "rev-parse", "crw/m1")

	fake := trackingfake.New(t, "o/r")
	fake.HeadSHA = func(branch string) string {
		out, _ := exec.Command("git", "-C", bare, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output()
		return strings.TrimSpace(string(out))
	}
	fake.SetChecks(head, []trackingfake.FakeCheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}}, nil)
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

	var section map[string]any
	if err := json.Unmarshal([]byte(integrateTestSection(dir)), &section); err != nil {
		t.Fatal(err)
	}
	section["pull_request_repo"] = "o/r"
	section["checkout"] = checkout
	data, _ := json.Marshal(section)
	code, out, errOut := integrateTestRun(t, string(data), "--enqueue-pr", "o/r#1", "--head", head, "--release-impact", "none")
	if code != 0 || errOut != "" {
		t.Fatalf("--enqueue-pr = %d %q %q", code, out, errOut)
	}
	var ans struct {
		State    string           `json:"state"`
		Enqueued integrator.Entry `json:"enqueued"`
	}
	if err := json.Unmarshal([]byte(out), &ans); err != nil || ans.State != "enqueued" || ans.Enqueued.Name != "pr-1" || ans.Enqueued.Head != head || ans.Enqueued.PR != 1 {
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

	// a head the pull request does not carry is refused, exit 2, the pending file keeps the decided head
	code, out, _ = integrateTestRun(t, string(data), "--enqueue-pr", "o/r#1", "--head", strings.Repeat("e", 40), "--release-impact", "none")
	if code != usageExit || !strings.Contains(out, `"state":"refused"`) || !strings.Contains(out, `"reason":"head_moved"`) {
		t.Errorf("moved head: code %d stdout %q", code, out)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "queue", "pending", "pr-1")); string(got) != head+"\n" {
		t.Errorf("pending/pr-1 = %q after the refusal", got)
	}

	for name, tc := range map[string]struct {
		section string
		args    []string
		want    string
	}{
		"no value":                {string(data), []string{"--enqueue-pr"}, "expected one argument"},
		"with watch":              {string(data), []string{"--enqueue-pr", "o/r#1", "--watch"}, "cannot be combined"},
		"with dry run":            {string(data), []string{"--enqueue-pr", "o/r#1", "--dry-run"}, "cannot be combined"},
		"no head":                 {string(data), []string{"--enqueue-pr", "o/r#1", "--release-impact", "x"}, "--head"},
		"no impact":               {string(data), []string{"--enqueue-pr", "o/r#1", "--head", head}, "--release-impact"},
		"decision flags alone":    {string(data), []string{"--head", head, "--release-impact", "x"}, "belong to --enqueue-pr"},
		"update-base alone":       {string(data), []string{"--update-base"}, "belong to --enqueue-pr"},
		"no pull_request_repo":    {integrateTestSection(dir), []string{"--enqueue-pr", "o/r#1", "--head", head, "--release-impact", "x"}, "pull_request_repo"},
		"another repository":      {string(data), []string{"--enqueue-pr", "x/y#1", "--head", head, "--release-impact", "x"}, "not a pull request of o/r"},
		"a bad pull_request_repo": {strings.Replace(string(data), `"o/r"`, `"o/r#1"`, 1), []string{"--enqueue-pr", "o/r#1", "--head", head, "--release-impact", "x"}, "pull_request_repo must be owner/name"},
	} {
		code, out, errOut := integrateTestRun(t, tc.section, tc.args...)
		if code != usageExit || out != "" || !strings.Contains(errOut, tc.want) {
			t.Errorf("%s: code %d stdout %q stderr %q", name, code, out, errOut)
		}
	}
}
