package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/pullrequest"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
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

// ---- the bundle's native pull request flow (CRW-1423, step 3) ----

// bundleRequester is the parent's merge-request delivery of a bundle run: it feeds the request to the production decision
// flow of the dev queue, never to a direct EnqueuePR.
type bundleRequester func(context.Context, ops.MergeRequest) (string, error)

func (f bundleRequester) MergeRequest(ctx context.Context, m ops.MergeRequest) (string, error) {
	return f(ctx, m)
}

// bundleGH is the tracking adapter of a bundle's end to end: the real GitHub adapter over the fake, whose checks answer
// success for whatever head the pull request currently carries (the lane's head moves as the milestone lands) and whose
// OpenPullRequest lets a test crash the process right after the POST, before the op's row is written.
type bundleGH struct {
	*tracking.GitHub
	fake      *trackingfake.Server
	afterOpen func()
	posts     int
}

func (g *bundleGH) Checks(ctx context.Context, ref tracking.Ref) (tracking.Checks, error) {
	pr, err := g.GitHub.PullRequest(ctx, ref)
	if err != nil {
		return tracking.Checks{}, err
	}
	g.fake.SetChecks(pr.HeadSHA, []trackingfake.FakeCheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}}, nil)
	return g.GitHub.Checks(ctx, ref)
}

func (g *bundleGH) OpenPullRequest(ctx context.Context, in tracking.NewPullRequest) (tracking.PullRequest, error) {
	g.posts++
	pr, err := g.GitHub.OpenPullRequest(ctx, in)
	if err == nil && g.afterOpen != nil {
		g.afterOpen()
		if ctx.Err() != nil {
			return pr, ctx.Err()
		}
	}
	return pr, err
}

// bundleLab is a two-issue PR-unit lab whose handoff's target names pr_base dev: the milestone's own project lane opens the
// pull request (crw.prOpen after the close is reported) and asks for its merge (crw.mergeRequest), which the requester
// closure lands through the queue's decide flow. answers the lab, the fake, the opened-count, the queue's pass, the count of
// merge requests and the landing the requester saw.
func bundleLab(t *testing.T, onOpen func()) (*laneLab, *trackingfake.Server, *bundleGH, *integrator.Runner, *int, **integrator.Report, string) {
	t.Helper()
	l := newLaneLabFor(t, labOwner, "crw/m1", labIssue{Key: "o:r:10", Milestone: "M1"}, labIssue{Key: "o:r:11", Milestone: "M1", After: []string{"o:r:10"}})
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	l.git(root, "init", "-q", "--bare", "-b", "dev", bare)
	l.git(l.repo, "remote", "add", "origin", bare)
	l.git(l.repo, "push", "-q", "origin", "main:refs/heads/dev")
	// the bundle's handoff: the milestone integrates on crw/m1 and its pull request goes onto dev (written before start)
	l.input.Projects[0].Target.PRBase = "dev"
	raw, err := json.MarshalIndent(l.input, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	fake := trackingfake.New(t, "o/r")
	fake.HeadSHA = func(branch string) string {
		out, _ := exec.Command("git", "-C", bare, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output()
		return strings.TrimSpace(string(out))
	}
	gh, err := tracking.NewGitHub("o/r", tracking.StaticToken(trackingfake.Token), tracking.WithBaseURL(fake.URL))
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &bundleGH{GitHub: gh, fake: fake, afterOpen: onOpen}
	l.prFactory = func(repo string) (tracking.PullRequests, error) { return wrapped, nil }
	l.prRepo = "o/r"
	scripts := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	verify := filepath.Join(scripts, "verify.sh")
	if err := os.WriteFile(verify, []byte(e2eVerify), 0o755); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(scripts, "gate.sh")
	if err := os.WriteFile(gate, []byte("#!/bin/sh\necho gate >> \"$0.calls\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(root, "queue-checkout")
	l.git(root, "clone", "-q", bare, checkout)
	cfg, err := integrator.Config{
		Checkout: checkout, QueueDir: filepath.Join(root, "queue"), LockFile: filepath.Join(root, "lock"), RunsDir: filepath.Join(root, "runs"),
		WorkRoot: filepath.Join(root, "work"), Remote: "origin", Branch: "dev", GateCommand: []string{gate}, VerifyCommand: []string{verify},
		RecordVersionCommand: []string{"true"}, PullRequestRepo: "o/r",
	}.Resolved()
	if err != nil {
		t.Fatal(err)
	}
	queue := integrator.NewRunner(cfg)
	queue.PullRequests = wrapped
	queue.Now = func() time.Time { return time.Date(2026, 10, 15, 4, 0, 0, 0, time.UTC) }
	requests := 0
	var landing *integrator.Report
	l.mergeRequester = bundleRequester(func(ctx context.Context, m ops.MergeRequest) (string, error) {
		requests++
		dec, err := pullrequest.Decide(ctx, queue, pullrequest.DecideInput{PR: m.PR, Head: m.Head, ReleaseImpact: "none"})
		if err != nil || dec.State != "enqueued" {
			return "", fmt.Errorf("the parent's decision: %v %v", dec, err)
		}
		rep := queue.Pass(ctx)
		if rep.Exit != integrator.ExitPushed {
			return "", fmt.Errorf("the queue's pass: %+v", rep)
		}
		landing = &rep
		return "req_" + m.Head[:8], nil
	})
	return l, fake, wrapped, queue, &requests, &landing, bare
}

// bundleRows are the completed rows of op in the run's journal.
func bundleRows(l *laneLab, op string) []engine.Event {
	var out []engine.Event
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Op == op && e.Event == engine.EvCompleted {
			out = append(out, e)
		}
	}
	return out
}

// TestBundlePRFlowEndToEnd (CRW-1423): a bundle's own lane opens the pull request after its milestone closes and reports
// (crw.prOpen), asks the parent for the merge with the persisted review record, and the parent's decision enqueues it —
// one pull request, both issues' heads on dev, no manual open or enqueue step.
func TestBundlePRFlowEndToEnd(t *testing.T) {
	l, fake, gh, _, requests, landing, bare := bundleLab(t, nil)
	spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	v := l.closed(l.runToEnd(spec.RunID))
	l.effectsOnce("o:r:10", "o:r:11")
	// one pull request, opened by the lane itself, carrying both issues
	if pulls := fake.Pulls(); len(pulls) != 1 {
		t.Fatalf("%d pull requests", len(pulls))
	}
	if *requests != 1 || landing == nil || *landing == nil {
		t.Fatalf("%d merge requests, landing %v", *requests, landing)
	}
	for _, key := range []string{"o:r:10", "o:r:11"} {
		var head string
		if err := l.w.st.DB.QueryRow("SELECT head FROM dag_agent_acceptances WHERE node_id = ?", l.nodeOf[key]).Scan(&head); err != nil {
			t.Fatal(err)
		}
		if err := exec.Command("git", "-C", bare, "merge-base", "--is-ancestor", head, "dev").Run(); err != nil {
			t.Errorf("the head of %s is not on dev", key)
		}
	}
	// the journal's order: gate and report rows precede the open, the open precedes the merge request
	gate := bundleRows(l, "crw."+ops.NameFinalGate)
	report := bundleRows(l, ops.OpReportUp)
	open := bundleRows(l, ops.OpPROpen)
	merges := bundleRows(l, ops.OpMergeRequest)
	if len(open) != 1 || len(merges) != 1 || len(gate) == 0 || len(report) == 0 {
		t.Fatalf("rows: %d gate %d report %d open %d merge", len(gate), len(report), len(open), len(merges))
	}
	if !(gate[len(gate)-1].Seq < open[0].Seq && report[len(report)-1].Seq < open[0].Seq && open[0].Seq < merges[0].Seq) {
		t.Errorf("row order gate/report %d,%d open %d merge %d", gate[len(gate)-1].Seq, report[len(report)-1].Seq, open[0].Seq, merges[0].Seq)
	}
	if str(open[0].Data, "state") != "opened" || str(merges[0].Data, "state") != "requested" {
		t.Errorf("open %v merge %v", open[0].Data, merges[0].Data)
	}
	// the persisted review record: no review bots are connected in the lab
	records, err := filepath.Glob(filepath.Join(RunDir(l.w.state, spec.RunID), "pr-review-*.json"))
	if err != nil || len(records) != 1 {
		t.Fatalf("review records %v %v", records, err)
	}
	rec, err := tracking.ReadPRReviewHistory(records[0], tracking.Ref{Repo: "o/r", Number: 1})
	if err != nil || rec.Bots != tracking.ReviewBotsNotConnected || rec.Note != "리뷰 봇 미연결" {
		t.Errorf("the review record: %+v %v", rec, err)
	}
	// the pull request holds the parent's ready comment and the queue's landing comment
	comments := fake.Comments(1)
	var ready, landed bool
	for _, c := range comments {
		ready = ready || strings.Contains(c, "state: ready")
		landed = landed || strings.Contains(c, "result: pushed")
	}
	if !ready || !landed {
		t.Errorf("comments ready %v landed %v: %q", ready, landed, comments)
	}
	if land := (*landing).Landing; land == nil || land.PullRequest != "o/r#1" || land.Verification.Status != 0 {
		t.Errorf("the landing %+v", (*landing).Landing)
	}
	// the run's value carries the opened pull request
	if v.State != "closed" {
		t.Fatalf("the run ended %s", v.State)
	}
	if posts := gh.posts; posts != 1 {
		t.Errorf("%d open POST(s), want 1", posts)
	}
}

// TestResumeAfterPROpenDoesNotOpenASecondPR: a crash after the lane's completed prOpen row, and a crash between the open's
// POST and its row, both resume to one pull request and one merge request — the first by the op's record, the second by the
// head/base lookup (FindPullRequest) that finds the pull request the killed process made.
func TestResumeAfterPROpenDoesNotOpenASecondPR(t *testing.T) {
	t.Run("after the completed row", func(t *testing.T) {
		l, fake, gh, _, requests, landing, _ := bundleLab(t, nil)
		l.afterRow = func(e engine.Event) {
			if e.Kind == engine.KindCall && e.Op == ops.OpPROpen && e.Event == engine.EvCompleted {
				l.mu.Lock()
				cur := l.cur
				l.mu.Unlock()
				if f, ok := cur.Journal.(interface{ Flush() error }); ok {
					if err := f.Flush(); err != nil {
						t.Fatal(err)
					}
				}
				l.kill()
			}
		}
		spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
		l.closed(l.runToEnd(spec.RunID))
		if l.processes != 2 {
			t.Fatalf("%d processes, want 2", l.processes)
		}
		if got := len(fake.Pulls()); got != 1 || gh.posts != 1 {
			t.Fatalf("%d pull requests, %d POST(s)", got, gh.posts)
		}
		if *requests != 1 || *landing == nil {
			t.Fatalf("%d merge requests", *requests)
		}
	})
	t.Run("after the POST before the row", func(t *testing.T) {
		var l *laneLab
		var fake *trackingfake.Server
		var gh *bundleGH
		var requests *int
		var landing **integrator.Report
		l, fake, gh, _, requests, landing, _ = bundleLab(t, func() {
			// the process dies between the successful POST and the op's row; the resumed lookup must find this pull request
			l.mu.Lock()
			cur := l.cur
			l.mu.Unlock()
			if cur != nil {
				l.kill()
			}
		})
		spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
		l.closed(l.runToEnd(spec.RunID))
		if got := len(fake.Pulls()); got != 1 || gh.posts != 1 {
			t.Fatalf("%d pull requests, %d POST(s): the killed open made one and the retry reused it", got, gh.posts)
		}
		if *requests != 1 || *landing == nil {
			t.Fatalf("%d merge requests", *requests)
		}
		open := bundleRows(l, ops.OpPROpen)
		if len(open) != 1 || str(open[0].Data, "pr") != "o/r#1" {
			t.Errorf("the opened row: %v", open)
		}
	})
}

// TestNoPRIsOpenedForAnIncompleteOrCrosscheckPendingMilestone: a run that does not close — a lane that escalates (all lanes
// not landed: incomplete) or a crosscheck that stays needs_changes — opens no pull request and asks for no merge, whatever
// state the value reports.
func TestNoPRIsOpenedForAnIncompleteOrCrosscheckPendingMilestone(t *testing.T) {
	for name, script := range map[string]func(l *laneLab){
		"a lane that escalates": func(l *laneLab) {
			bad := func(text string) labCheck {
				return labCheck{Verdict: reviewadapter.VerdictNeedsChanges, Findings: []reviewadapter.Finding{{Severity: "P1", Text: text, File: "README.md", Line: 1}}}
			}
			l.UnitChecks("o:r:10", ops.UnitCheckUncommitted, bad("wrong once"), bad("wrong twice"), bad("a third thing is wrong"))
		},
		"a crosscheck that stays needs_changes": func(l *laneLab) {
			f := reviewadapter.Finding{Severity: "P1", Text: "the close does not hold", File: "README.md", Line: 1}
			needs := reviewadapter.Result{State: reviewadapter.StateOK, Verdict: reviewadapter.VerdictNeedsChanges, Findings: []reviewadapter.Finding{f}}
			l.Crosschecks("M1", 1, 0, "A", 1, needs)
			l.Crosschecks("M1", 2, 0, "A", 1, needs)
		},
	} {
		t.Run(name, func(t *testing.T) {
			l, fake, gh, _, requests, _, _ := bundleLab(t, nil)
			script(l)
			spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
			res := l.runToEnd(spec.RunID)
			var v map[string]any
			_ = json.Unmarshal(res.Value, &v)
			if v["state"] == "closed" {
				t.Fatalf("%s closed: %s", name, res.Value)
			}
			if got := len(fake.Pulls()); got != 0 || gh.posts != 0 {
				t.Errorf("%d pull requests, %d POST(s): none may exist for a milestone that did not close", got, gh.posts)
			}
			if *requests != 0 {
				t.Errorf("%d merge requests", *requests)
			}
			if open := bundleRows(l, ops.OpPROpen); len(open) != 0 {
				t.Errorf("crw.prOpen ran on a milestone that did not close: %v", open)
			}
		})
	}
}
