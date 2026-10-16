package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/pullrequest"
	"github.com/thisisjun786/codex-relay-workflow/internal/integrator"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

// e2eVerify is the repository's verification command in the queue's worktree: it writes a verification-record of the merged tree.
const e2eVerify = `#!/bin/sh
printf '{"schema":"verification-record/1","head":"%s","base":"%s","result":"PASS"}\n' "$CRW_VERIFY_HEAD" "$CRW_VERIFY_BASE" > "$CRW_VERIFY_RECORD"
`

// e2eReview is the repository's review command (the coordinator runs Argon here): it reads the diff it is given and writes accept.
const e2eReview = `#!/bin/sh
git diff --stat "$2" "$1" >/dev/null || exit 9
printf '{"verdict":"accept","findings":[]}\n' > "$3"
`

// TestPRUnitLandingEndToEnd (CRW-1355 c4): two issues of one milestone land through PR-unit landing with no human step. The run lands
// both nodes on the milestone integration branch (the plan's target base_ref), the project session opens one pull request from that
// branch toward dev carrying both issues (crw flow pr open, which pushes the branch to the local bare remote) and enqueues it; one pass
// of the dev queue merges the pull request's head onto dev in its own worktree, verifies it, reviews it with the configured (fake)
// review command and fast-forwards dev on the bare remote. The landing record and the pull request's comment name the pull request,
// its head, dev before and after, the verification-record and the verdict. GitHub is the adapter's fake.
func TestPRUnitLandingEndToEnd(t *testing.T) {
	ctx := context.Background()
	l := newLaneLabFor(t, labOwner, "crw/m1", labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1", After: []string{"A-1"}})
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	l.git(root, "init", "-q", "--bare", "-b", "dev", bare)
	l.git(l.repo, "remote", "add", "origin", bare)
	l.git(l.repo, "push", "-q", "origin", "main:refs/heads/dev")
	devBefore := l.git(bare, "rev-parse", "dev")

	// 1. the run lands the milestone's two nodes on crw/m1
	spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	l.closed(l.runToEnd(spec.RunID))
	l.effectsOnce("A-1", "A-2")
	heads := map[string]string{}
	for _, key := range []string{"A-1", "A-2"} {
		var head string
		if err := l.w.st.DB.QueryRow("SELECT head FROM dag_agent_acceptances WHERE node_id = ?", l.nodeOf[key]).Scan(&head); err != nil {
			t.Fatal(err)
		}
		heads[key] = head
		l.git(l.repo, "merge-base", "--is-ancestor", head, "crw/m1")
	}
	milestoneTip := l.git(l.repo, "rev-parse", "crw/m1")
	if got := l.git(bare, "rev-parse", "dev"); got != devBefore {
		t.Fatalf("the run moved dev to %s", got)
	}

	// 2. the project session opens one pull request carrying both issues and enqueues it
	fake := trackingfake.New(t, "o/r")
	fake.HeadSHA = func(branch string) string {
		out, _ := exec.Command("git", "-C", bare, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output()
		return strings.TrimSpace(string(out))
	}
	adapter := func(repo string) (*tracking.GitHub, error) {
		return tracking.NewGitHub(repo, tracking.StaticToken(trackingfake.Token), tracking.WithBaseURL(fake.URL))
	}
	var out, errOut bytes.Buffer
	d := &pullrequest.Deps{NewPullRequests: func(repo string) (tracking.PullRequests, error) { return adapter(repo) }}
	if code := pullrequest.RunWith(ctx, d, []string{"open", "--repo", "o/r", "--head", "crw/m1", "--base", "dev", "--title", "milestone M1",
		"--issue", "o/r#10,o/r#11", "--push", l.repo}, &out, &errOut); code != 0 {
		t.Fatalf("crw flow pr open: %d\n%s%s", code, out.String(), errOut.String())
	}
	var opened pullrequest.Answer
	if err := json.Unmarshal(out.Bytes(), &opened); err != nil || opened.PullRequest.Ref != "o/r#1" || opened.PullRequest.Head != milestoneTip || opened.Pushed != milestoneTip {
		t.Fatalf("the opened pull request %s: %v", out.String(), err)
	}

	scripts := filepath.Join(root, "scripts")
	for name, body := range map[string]string{"verify.sh": e2eVerify, "review.sh": e2eReview, "gate.sh": "#!/bin/sh\necho gate >> \"$0.calls\"\nexit 1\n"} {
		if err := os.MkdirAll(scripts, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scripts, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	checkout := filepath.Join(root, "queue-checkout")
	l.git(root, "clone", "-q", bare, checkout)
	cfg, err := integrator.Config{
		Checkout: checkout, QueueDir: filepath.Join(root, "queue"), LockFile: filepath.Join(root, "lock"), RunsDir: filepath.Join(root, "runs"),
		WorkRoot: filepath.Join(root, "work"), GateCommand: []string{filepath.Join(scripts, "gate.sh")}, VerifyCommand: []string{filepath.Join(scripts, "verify.sh")},
		RecordVersionCommand: []string{"true"}, PullRequestRepo: "o/r",
		ReviewCommand: []string{filepath.Join(scripts, "review.sh"), "{merged}", "{base}", "{verdict}"},
	}.Resolved()
	if err != nil {
		t.Fatal(err)
	}
	queue := integrator.NewRunner(cfg)
	gh, err := adapter("o/r")
	if err != nil {
		t.Fatal(err)
	}
	queue.PullRequests = gh
	queue.Now = func() time.Time { return time.Date(2026, 10, 15, 4, 0, 0, 0, time.UTC) }
	if _, err := queue.EnqueuePR(ctx, "o/r#1"); err != nil {
		t.Fatal(err)
	}

	// 3. one pass of the dev queue: merge, verify, review, fast-forward dev
	rep := queue.Pass(ctx)
	if rep.Exit != integrator.ExitPushed || rep.Result != "pushed" || rep.Landing == nil {
		raw, _ := json.Marshal(rep)
		t.Fatalf("the queue's pass: %s", raw)
	}
	devAfter := l.git(bare, "rev-parse", "dev")
	for key, head := range heads {
		if err := exec.Command("git", "-C", bare, "merge-base", "--is-ancestor", head, "dev").Run(); err != nil {
			t.Errorf("the head of %s is not on dev", key)
		}
	}
	if parents := l.git(bare, "rev-list", "--parents", "-n", "1", "dev"); parents != devAfter+" "+devBefore+" "+milestoneTip {
		t.Errorf("dev is %q, want one merge of the pull request's head %s onto the old dev %s", parents, milestoneTip, devBefore)
	}
	if _, err := os.Stat(filepath.Join(scripts, "gate.sh.calls")); err == nil {
		t.Error("the issue gate ran for the pull request")
	}

	// 4. the record of the landing, against the pull request
	land := rep.Landing
	if land.PullRequest != "o/r#1" || land.Head != milestoneTip || land.HeadRef != "crw/m1" || strings.Join(land.Issues, ",") != "o/r#10,o/r#11" ||
		land.DevBefore != devBefore || land.DevAfter != devAfter || land.Review.Verdict != "accept" || land.Verification.Status != 0 ||
		land.Verification.SHA256 == "" || land.Comment != "written" {
		t.Errorf("the landing %+v", land)
	}
	record, err := os.ReadFile(land.Verification.Record)
	if err != nil || !strings.Contains(string(record), `"head":"`+devAfter+`"`) || !strings.Contains(string(record), `"result":"PASS"`) {
		t.Errorf("the verification-record %s: %s %v", land.Verification.Record, record, err)
	}
	onDisk, err := os.ReadFile(filepath.Join(cfg.RunsDir, rep.Summary.Tag+".pr.json"))
	if err != nil || !strings.Contains(string(onDisk), `"verdict": "accept"`) {
		t.Errorf("the landing file: %s %v", onDisk, err)
	}
	comments := fake.Comments(1)
	if len(comments) != 1 {
		t.Fatalf("comments on the pull request: %q", comments)
	}
	// the comment is public text (CRW-1549): the record and verdict file paths are masked
	for _, want := range []string{"result: pushed", "dev: " + devBefore + " -> " + devAfter, "verification-record: [path]", "review: accept ([path]"} {
		if !strings.Contains(comments[0], want) {
			t.Errorf("the comment does not say %q:\n%s", want, comments[0])
		}
	}
	if got := fake.Pulls(); len(got) != 1 {
		t.Errorf("%d pull requests: one pull request carries the milestone", len(got))
	}
}
