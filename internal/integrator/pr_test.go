package integrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

// PR-unit landing (CRW-1355): a project session's pull request from its milestone integration branch goes to dev through the queue.
// The queue merges the pull request's head onto dev in its own worktree, runs the verification command, then the configured review
// command, and only then moves dev fast-forward; the landing is recorded against the pull request (its number, head, dev before and
// after, the verification-record and the review verdict), in the run's files and as a comment on the pull request.

// reviewScript is the fake review command: it records its call (with the verification count, so the order is visible), runs a hook,
// and writes the verdict ctl/review-verdict names (accept by default; "none" writes no verdict and fails).
const reviewScript = `#!/bin/sh
echo "verify-n=$(cat "$CTL_DIR/verify-n" 2>/dev/null) argv=$1|$2|$3|$4 pr=$CRW_REVIEW_PR head=$CRW_REVIEW_HEAD merged=$CRW_REVIEW_MERGED base=$CRW_REVIEW_BASE record=$CRW_VERIFY_RECORD verdict=$CRW_REVIEW_VERDICT dir=$(pwd)" >> "$CTL_DIR/review-calls"
[ -f "$CTL_DIR/during-review.sh" ] && sh "$CTL_DIR/during-review.sh"
v=$(cat "$CTL_DIR/review-verdict" 2>/dev/null || echo accept)
[ "$v" = none ] && exit 3
printf '{"status":"SUCCESS","structured_output":{"verdict":"%s","findings":[]}}\n' "$v" > "$CRW_REVIEW_VERDICT"
`

type prWorld struct {
	*world
	fake *trackingfake.Server
	gh   *tracking.GitHub
}

// newPRWorld is the queue's world with the fake GitHub of repository o/r: the queue lands its pull requests, and the review command is
// the fake review unless review is false.
func newPRWorld(t *testing.T, review bool) *prWorld {
	t.Helper()
	w := newWorld(t)
	fake := trackingfake.New(t, "o/r")
	fake.HeadSHA = func(branch string) string {
		cmd := exec.Command("git", "-C", w.remote, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
		cmd.Env = w.gitEnv()
		out, _ := cmd.Output()
		return strings.TrimSpace(string(out))
	}
	gh, err := tracking.NewGitHub("o/r", tracking.StaticToken(trackingfake.Token), tracking.WithBaseURL(fake.URL))
	if err != nil {
		t.Fatal(err)
	}
	w.write(filepath.Join(w.root, "scripts", "review.sh"), reviewScript)
	cfg := w.cfg
	cfg.PullRequestRepo = "o/r"
	if review {
		cfg.ReviewCommand = []string{filepath.Join(w.root, "scripts", "review.sh"), "{pr}", "{merged}", "{verdict}", "{base}"}
	}
	if cfg, err = cfg.Resolved(); err != nil {
		t.Fatal(err)
	}
	w.cfg, w.r.Cfg = cfg, cfg
	w.r.PullRequests = gh
	return &prWorld{world: w, fake: fake, gh: gh}
}

// milestone publishes the milestone branch crw/m1 with one commit per issue, as crw.integrate lands them, and opens its pull request
// toward dev carrying both issues; it answers the pull request.
func (w *prWorld) milestone(issues ...string) tracking.PullRequest {
	w.t.Helper()
	files := map[string]string{}
	for _, is := range issues {
		files["issue-"+is+".txt"] = is + "\n"
	}
	w.head("crw/m1", files, true)
	var refs []tracking.Ref
	for _, is := range issues {
		ref, err := tracking.ParseRef("o/r#" + is)
		if err != nil {
			w.t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	pr, err := w.gh.OpenPullRequest(context.Background(), tracking.NewPullRequest{Head: "crw/m1", Base: "dev", Title: "milestone M1", Issues: refs})
	if err != nil {
		w.t.Fatal(err)
	}
	return pr
}

func (w *prWorld) enqueue(ref string) Entry {
	w.t.Helper()
	e, err := w.r.EnqueuePR(context.Background(), ref)
	if err != nil {
		w.t.Fatalf("EnqueuePR(%s): %v", ref, err)
	}
	return e
}

func readLanding(t *testing.T, path string) Landing {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the landing record: %v", err)
	}
	var l Landing
	if err := json.Unmarshal(data, &l); err != nil {
		t.Fatalf("the landing record %s: %v", data, err)
	}
	return l
}

func TestPassLandsAPullRequestAfterVerificationAndReview(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10", "11")
	e := w.enqueue("o/r#1")
	if e.Name != "pr-1" || e.Head != pr.HeadSHA || e.PR != 1 {
		t.Fatalf("enqueued %+v for %+v", e, pr)
	}
	w.mustWhere("pr-1", "pending")
	base := w.remoteDev()

	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	w.mustWhere("pr-1", "done")
	dev := w.remoteDev()
	w.git(w.remote, "merge-base", "--is-ancestor", pr.HeadSHA, "dev")
	w.git(w.remote, "merge-base", "--is-ancestor", base, "dev")
	if msg := w.git(w.remote, "log", "-1", "--format=%s", "dev"); msg != "Merge pull request o/r#1 (crw/m1) at "+pr.HeadSHA[:8]+" into dev" {
		t.Errorf("the merge commit says %q", msg)
	}
	// no gate command runs for a pull request: its gate is the verification of the merged tree
	if got := w.ctlRead("gate-calls"); got != "" {
		t.Errorf("the pre-merge gate ran for a pull request: %q", got)
	}
	// the review ran after the verification, on the merged tree, with its placeholders and environment
	calls := strings.Split(strings.TrimSpace(w.ctlRead("review-calls")), "\n")
	verdict := filepath.Join(w.cfg.RunsDir, rep.Summary.Tag+".review.json")
	record := rep.Summary.Record
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "verify-n=1 argv=o/r#1|"+dev+"|"+verdict+"|"+base+" pr=o/r#1 head="+pr.HeadSHA+" merged="+dev+" base="+base+" record="+record+" verdict="+verdict) ||
		!strings.HasSuffix(calls[0], "/run-"+rep.Summary.Tag) {
		t.Fatalf("review calls %q", calls)
	}
	// the record of the landing, against the pull request
	l := readLanding(t, filepath.Join(w.cfg.RunsDir, rep.Summary.Tag+".pr.json"))
	sum := sha256.Sum256([]byte("{\"ok\":true}\n"))
	if l.Schema != LandingSchema || l.PullRequest != "o/r#1" || l.Number != 1 || l.Head != pr.HeadSHA || l.HeadRef != "crw/m1" ||
		strings.Join(l.Issues, ",") != "o/r#10,o/r#11" || l.DevBefore != base || l.DevAfter != dev || l.Merged != dev || l.Result != "pushed" ||
		l.Verification.Record != record || l.Verification.SHA256 != hex.EncodeToString(sum[:]) || l.Verification.Status != 0 ||
		!l.Review.Configured || l.Review.Verdict != "accept" || l.Review.VerdictFile != verdict || l.Comment != "written" {
		t.Errorf("the landing record %+v", l)
	}
	if rep.Landing == nil || rep.Landing.DevAfter != dev {
		t.Errorf("the pass reports the landing %+v", rep.Landing)
	}
	// and the same on the pull request, under the queue's layer header
	comments := w.fake.Comments(1)
	if len(comments) != 1 || !strings.HasPrefix(comments[0], "[dev queue · "+rep.Summary.Tag+"]\n\n") {
		t.Fatalf("comments %q", comments)
	}
	for _, want := range []string{"result: pushed", "head: " + pr.HeadSHA, "dev: " + base + " -> " + dev, "verification-record: " + record + " sha256:" + l.Verification.SHA256,
		"review: accept (" + verdict + ")", "issues: o/r#10, o/r#11"} {
		if !strings.Contains(comments[0], want) {
			t.Errorf("the comment does not say %q:\n%s", want, comments[0])
		}
	}
}

func TestPassRefusesAPullRequestTheReviewSaysNeedsChanges(t *testing.T) {
	w := newPRWorld(t, true)
	w.milestone("10", "11")
	w.enqueue("o/r#1")
	w.ctlWrite("review-verdict", "needs_changes")
	base := w.remoteDev()
	rep := w.pass()
	requireExit(t, rep, ExitReviewRefused, "review-needs-changes")
	if w.remoteDev() != base {
		t.Fatal("dev moved over a review that asked for changes")
	}
	w.mustWhere("pr-1", "blocked")
	verdict := filepath.Join(w.cfg.RunsDir, rep.Summary.Tag+".review.json")
	if moved := strings.Join(rep.Moved, "\n"); !strings.Contains(moved, "review needs_changes: "+verdict) {
		t.Errorf("moved %q does not name the verdict", moved)
	}
	l := readLanding(t, filepath.Join(w.cfg.RunsDir, rep.Summary.Tag+".pr.json"))
	if l.Result != "review-needs-changes" || l.DevAfter != "" || l.DevBefore != base || l.Review.Verdict != "needs_changes" || l.Review.VerdictFile != verdict {
		t.Errorf("the landing record %+v", l)
	}
	if c := w.fake.Comments(1); len(c) != 1 || !strings.Contains(c[0], "review: needs_changes ("+verdict+")") || !strings.Contains(c[0], "dev: "+base+" (not moved)") {
		t.Errorf("comments %q", c)
	}
}

func TestPassWithoutAReviewCommandWarnsRecordsAndLands(t *testing.T) {
	w := newPRWorld(t, false)
	w.milestone("10", "11")
	w.enqueue("o/r#1")
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	l := readLanding(t, filepath.Join(w.cfg.RunsDir, rep.Summary.Tag+".pr.json"))
	if l.Review.Configured || l.Review.Note != "review not configured" || l.DevAfter != w.remoteDev() || len(l.Warnings) != 1 || !strings.Contains(l.Warnings[0], "review not configured") {
		t.Errorf("the landing record %+v", l)
	}
	log, _ := os.ReadFile(filepath.Join(w.cfg.QueueDir, "runner.log"))
	if !strings.Contains(string(log), "review not configured") {
		t.Errorf("the runner log does not warn:\n%s", log)
	}
	if c := w.fake.Comments(1); len(c) != 1 || !strings.Contains(c[0], "review: not configured") {
		t.Errorf("comments %q", c)
	}
}

func TestPassWithAReviewThatGivesNoVerdictWarnsAndLands(t *testing.T) {
	w := newPRWorld(t, true)
	w.milestone("10")
	w.enqueue("o/r#1")
	w.ctlWrite("review-verdict", "none")
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	l := readLanding(t, filepath.Join(w.cfg.RunsDir, rep.Summary.Tag+".pr.json"))
	if !l.Review.Configured || l.Review.Verdict != "" || l.Review.Status != 3 || !strings.Contains(l.Review.Note, "no verdict") || len(l.Warnings) != 1 {
		t.Errorf("the landing record %+v", l)
	}
}

func TestPassReviewsNothingWhenTheVerificationFails(t *testing.T) {
	w := newPRWorld(t, true)
	w.milestone("10")
	w.enqueue("o/r#1")
	w.ctlWrite("verify-rc", "3\n3\n")
	base := w.remoteDev()
	rep := w.pass()
	requireExit(t, rep, ExitVerifyFailed, "verification-failed(rc=3)")
	if w.ctlRead("review-calls") != "" || w.remoteDev() != base {
		t.Fatal("a failed verification was reviewed or landed")
	}
	w.mustWhere("pr-1", "pending")
	l := readLanding(t, filepath.Join(w.cfg.RunsDir, rep.Summary.Tag+".pr.json"))
	if l.Verification.Status != 3 || l.Review.Note != "not reviewed: the verification failed" || l.DevAfter != "" {
		t.Errorf("the landing record %+v", l)
	}
	// alone, a second failure blocks it, as a head that fails alone twice
	requireExit(t, w.pass(), ExitVerifyFailed, "verification-failed(rc=3)")
	w.mustWhere("pr-1", "blocked")
}

func TestPassKeepsAPullRequestPendingWhenDevMovesDuringTheReview(t *testing.T) {
	w := newPRWorld(t, true)
	w.milestone("10")
	w.enqueue("o/r#1")
	// another integration lands on dev while the review runs: the compare-and-swap of dev refuses, nothing is pushed
	w.ctlWrite("during-review.sh", "cd \"$OTHER_DIR\" && git fetch -q origin && git checkout -q -B mover origin/dev && echo m > mover.txt && git add -A && git commit -q -m mover && git push -q origin HEAD:dev\n")
	rep := w.pass()
	if rep.Exit != ExitMoved || !strings.HasPrefix(rep.Result, "dev-moved:") {
		t.Fatalf("pass = %d %q", rep.Exit, rep.Result)
	}
	w.mustWhere("pr-1", "pending")
	if msg := w.git(w.remote, "log", "-1", "--format=%s", "dev"); msg != "mover" {
		t.Errorf("dev is at %q", msg)
	}
	l := readLanding(t, filepath.Join(w.cfg.RunsDir, rep.Summary.Tag+".pr.json"))
	if l.DevAfter != "" || l.Review.Verdict != "accept" {
		t.Errorf("the landing record %+v", l)
	}
}

func TestPassChecksThePullRequestBeforeItMerges(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(w *prWorld)
		dir    string
		reason string
	}{
		{"head moved", func(w *prWorld) {
			// the project session landed one more node on its branch and pushed it after the enqueue
			w.git(w.other, "checkout", "-q", "crw/m1")
			w.write(filepath.Join(w.other, "later.txt"), "x\n")
			w.git(w.other, "add", "-A")
			w.git(w.other, "commit", "-q", "-m", "later")
			w.git(w.other, "push", "-q", "origin", "crw/m1")
		}, "conflict", "the pull request's head is now"},
		{"closed", func(w *prWorld) { w.fake.SetPull(1, func(p *trackingfake.Pull) { p.State = "closed" }) }, "conflict", "the pull request is closed"},
		{"merged", func(w *prWorld) { w.fake.SetPull(1, func(p *trackingfake.Pull) { p.State, p.Merged = "closed", true }) }, "done", "the pull request is merged"},
		{"base", func(w *prWorld) { w.fake.SetPull(1, func(p *trackingfake.Pull) { p.Base = "main" }) }, "blocked", "goes to main, not dev"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newPRWorld(t, true)
			w.milestone("10")
			w.enqueue("o/r#1")
			c.change(w)
			base := w.remoteDev()
			rep := w.pass()
			if rep.Exit != ExitNothingMerged || w.remoteDev() != base || w.ctlRead("verify-n") != "" {
				t.Fatalf("pass = %d %q, dev %s, verified %q", rep.Exit, rep.Result, w.remoteDev(), w.ctlRead("verify-n"))
			}
			w.mustWhere("pr-1", c.dir)
			if !strings.Contains(strings.Join(rep.Moved, "\n"), c.reason) {
				t.Errorf("moved %q, want %q", rep.Moved, c.reason)
			}
		})
	}
}

func TestPassWithAPullRequestThatCannotBeReadKeepsItPending(t *testing.T) {
	w := newPRWorld(t, true)
	w.milestone("10")
	w.enqueue("o/r#1")
	gh, err := tracking.NewGitHub("o/r", tracking.StaticToken("wrong"), tracking.WithBaseURL(w.fake.URL))
	if err != nil {
		t.Fatal(err)
	}
	w.r.PullRequests = gh
	rep := w.pass()
	if rep.Exit != ExitUsage || rep.Result != "error" || !strings.Contains(rep.Error, "unauthorized") && !strings.Contains(rep.Error, "401") {
		t.Fatalf("pass = %d %q %q", rep.Exit, rep.Result, rep.Error)
	}
	w.mustWhere("pr-1", "pending")
	w.r.PullRequests = nil
	if rep := w.pass(); rep.Exit != ExitUsage || !strings.Contains(rep.Error, "pull request") {
		t.Fatalf("pass without the adapter = %d %q", rep.Exit, rep.Error)
	}
	w.mustWhere("pr-1", "pending")
}

func TestPassLandsAPullRequestAloneBeforeTheIssueHeads(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	h3 := w.head("h3", map[string]string{"f3.txt": "3\n"}, true)
	w.pend("3", h3)
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	if first.Summary.Merged != "pr-1="+pr.HeadSHA {
		t.Fatalf("the first pass merged %q", first.Summary.Merged)
	}
	w.mustWhere("3", "pending")
	second := w.pass()
	requireExit(t, second, ExitPushed, "pushed")
	if second.Summary.Merged != "crw-3="+h3 || second.Landing != nil {
		t.Fatalf("the second pass %+v", second)
	}
	if _, err := os.Stat(filepath.Join(w.cfg.RunsDir, second.Summary.Tag+".pr.json")); err == nil {
		t.Error("an issue head's run wrote a pull request landing")
	}
	if strings.Count(w.ctlRead("review-calls"), "\n") != 1 {
		t.Errorf("review calls %q: an issue head is not reviewed by the queue", w.ctlRead("review-calls"))
	}
}

func TestEnqueuePRRefusesAPullRequestTheQueueCannotLand(t *testing.T) {
	w := newPRWorld(t, true)
	w.milestone("10")
	for name, ref := range map[string]string{"other repository": "x/y#1", "not a ref": "o/r", "missing": "o/r#9"} {
		if _, err := w.r.EnqueuePR(context.Background(), ref); err == nil {
			t.Errorf("%s: EnqueuePR(%s) succeeded", name, ref)
		}
	}
	w.fake.SetPull(1, func(p *trackingfake.Pull) { p.Base = "main" })
	if _, err := w.r.EnqueuePR(context.Background(), "o/r#1"); err == nil || !strings.Contains(err.Error(), "not dev") {
		t.Errorf("a pull request to main: %v", err)
	}
	w.fake.SetPull(1, func(p *trackingfake.Pull) { p.Base, p.State = "dev", "closed" })
	if _, err := w.r.EnqueuePR(context.Background(), "o/r#1"); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("a closed pull request: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(w.cfg.QueueDir, "pending")); len(entries) != 0 {
		t.Errorf("refused enqueues left %v", entries)
	}
	w.r.Cfg.PullRequestRepo = ""
	w.fake.SetPull(1, func(p *trackingfake.Pull) { p.State = "open" })
	if _, err := w.r.EnqueuePR(context.Background(), "o/r#1"); err == nil || !strings.Contains(err.Error(), "pull_request_repo") {
		t.Errorf("no pull_request_repo: %v", err)
	}
}

func TestConfigPullRequestRepoAndReviewCommand(t *testing.T) {
	c := validConfig()
	c.PullRequestRepo, c.ReviewCommand = "o/r", []string{"review", "{pr}"}
	if _, err := c.Resolved(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(c *Config){
		"pull_request_repo": func(c *Config) { c.PullRequestRepo = "o/r#1" },
		"review_command":    func(c *Config) { c.ReviewCommand = []string{""} },
	} {
		c := validConfig()
		change(&c)
		if _, err := c.Resolved(); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
