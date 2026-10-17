package pullrequest

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

	"github.com/thisisjun786/codex-relay-workflow/internal/integrator"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

// CRW-1444 (P2-1): `crw flow pr decide` is the parent's merge decision for a pull request of the dev queue. It looks at head,
// base, checks, the queue's order and the release impact — never at the diff's content — and enqueues the pull request only
// when all of them pass.

// decideWorld is the queue runner over the git world's checkout (its origin is the bare remote) with the fake GitHub as the
// pull request source.
type decideWorld struct {
	g      *gitWorld
	fake   *trackingfake.Server
	gh     *tracking.GitHub
	runner *integrator.Runner
	cfg    integrator.Config
}

func newDecideWorld(t *testing.T) *decideWorld {
	t.Helper()
	g := newGitWorld(t)
	g.git(g.checkout, "push", "-q", "origin", "crw/m1")
	fake := trackingfake.New(t, "o/r")
	fake.HeadSHA = g.remoteSHA
	gh, err := tracking.NewGitHub("o/r", tracking.StaticToken(trackingfake.Token), tracking.WithBaseURL(fake.URL))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfg, err := integrator.Config{
		Checkout: g.checkout, QueueDir: filepath.Join(root, "queue"), LockFile: filepath.Join(root, "lock"),
		RunsDir: filepath.Join(root, "runs"), WorkRoot: filepath.Join(root, "work"),
		GateCommand: []string{"true"}, VerifyCommand: []string{"true"}, RecordVersionCommand: []string{"true"},
		PullRequestRepo: "o/r",
	}.Resolved()
	if err != nil {
		t.Fatal(err)
	}
	runner := integrator.NewRunner(cfg)
	runner.Env = g.env
	runner.PullRequests = gh
	return &decideWorld{g: g, fake: fake, gh: gh, runner: runner, cfg: cfg}
}

// openPR opens a pull request of branch toward dev through the adapter.
func (w *decideWorld) openPR(t *testing.T, branch string, issue int) tracking.PullRequest {
	t.Helper()
	pr, err := w.gh.OpenPullRequest(context.Background(), tracking.NewPullRequest{Head: branch, Base: "dev", Title: "milestone " + branch,
		Issues: []tracking.Ref{{Repo: "o/r", Number: issue}}})
	if err != nil {
		t.Fatalf("open %s: %v", branch, err)
	}
	return pr
}

// pushBranchOf commits content to file on the branch (made off the remote's dev when it does not exist yet), pushes it and
// returns its sha; the checkout is left on dev.
func (w *decideWorld) pushBranchOf(t *testing.T, name, file, content string) string {
	t.Helper()
	g := w.g
	g.git(g.checkout, "checkout", "-q", "dev")
	if code, _, _ := gitRC(t, g.checkout, "show-ref", "--verify", "--quiet", "refs/heads/"+name); code != 0 {
		g.git(g.checkout, "checkout", "-q", "-b", name)
	} else {
		g.git(g.checkout, "checkout", "-q", name)
	}
	if err := os.WriteFile(filepath.Join(g.checkout, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	g.git(g.checkout, "add", "-A")
	g.git(g.checkout, "commit", "-q", "-m", name)
	g.git(g.checkout, "push", "-q", "origin", name)
	g.git(g.checkout, "checkout", "-q", "dev")
	return g.remoteSHA(name)
}

// gitRC runs git in dir and returns its status.
func gitRC(t *testing.T, dir string, args ...string) (int, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), strings.TrimSpace(out.String()), err
		}
		return -1, "", err
	}
	return 0, strings.TrimSpace(out.String()), nil
}

// green answers successful checks for the head.
func (w *decideWorld) green(head string) {
	w.fake.SetChecks(head, []trackingfake.FakeCheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}}, nil)
}

// pending writes pending/pr-<n> holding head into the queue.
func (w *decideWorld) pending(t *testing.T, n int, head string) {
	t.Helper()
	dir := filepath.Join(w.cfg.QueueDir, "pending")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("pr-%d", n)), []byte(head+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (w *decideWorld) pendingHead(t *testing.T, n int) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(w.cfg.QueueDir, "pending", fmt.Sprintf("pr-%d", n)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (w *decideWorld) decide(t *testing.T, pr string, head string, updateBase bool) (Decision, error) {
	t.Helper()
	return Decide(context.Background(), w.runner, DecideInput{PR: pr, Head: head, ReleaseImpact: "low", UpdateBase: updateBase})
}

// checkoutPristine asserts the queue checkout's own state is untouched: branch, index and worktree as before.
func (w *decideWorld) checkoutPristine(t *testing.T, dev, m1 string) {
	t.Helper()
	g := w.g
	if got := g.git(g.checkout, "rev-parse", "dev"); got != dev {
		t.Errorf("local dev moved to %s", got)
	}
	if got := g.git(g.checkout, "rev-parse", "crw/m1"); got != m1 {
		t.Errorf("local crw/m1 moved to %s", got)
	}
	if got := g.git(g.checkout, "status", "--porcelain"); got != "" {
		t.Errorf("the checkout is dirty: %q", got)
	}
	if got := g.git(g.checkout, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Errorf("a worktree leaked:\n%s", got)
	}
}

// gitArgvLog puts a git first on PATH that logs its argv and then runs the real git, with extra lines run before the real
// call (for the push-CAS test).
func gitArgvLog(t *testing.T, beforePush string) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "argv.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n" + beforePush + "\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func argvLines(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func TestDecideEnqueuesOnAllGreen(t *testing.T) {
	w := newDecideWorld(t)
	pr := w.openPR(t, "crw/m1", 10)
	head := w.g.remoteSHA("crw/m1")
	base := w.g.remoteSHA("dev")
	w.green(head)
	devBefore, m1Before := w.g.git(w.g.checkout, "rev-parse", "dev"), w.g.git(w.g.checkout, "rev-parse", "crw/m1")

	dec, err := w.decide(t, "o/r#1", head, false)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.Schema != DecisionSchema || dec.State != "enqueued" || dec.PR != pr.Ref.String() || dec.Head != head || dec.Base != "dev" ||
		dec.BaseHead != base || dec.Enqueued == nil || dec.Enqueued.Name != "pr-1" || dec.Enqueued.Head != head {
		t.Fatalf("decision %+v", dec)
	}
	if got := w.pendingHead(t, 1); got != head {
		t.Fatalf("pending/pr-1 = %q, want %s", got, head)
	}
	// the record on disk is the same decision
	data, err := os.ReadFile(dec.Record)
	if err != nil {
		t.Fatal(err)
	}
	var rec Decision
	if err := json.Unmarshal(data, &rec); err != nil || rec.State != "enqueued" || rec.Head != head {
		t.Fatalf("record %s: %v %s", dec.Record, err, data)
	}
	if want := filepath.Join(w.cfg.RunsDir, "decisions", "pr-1-"+head+".json"); dec.Record != want {
		t.Errorf("record path %q, want %q", dec.Record, want)
	}
	if fi, err := os.Stat(dec.Record); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("record mode: %v %v", fi, err)
	}
	// the pull request carries the decision comment, once
	header := fmt.Sprintf("[crw-pr-decide · 1 · %s · %s]", head, base)
	comments := w.fake.Comments(1)
	if len(comments) != 1 || !strings.HasPrefix(comments[0], header) {
		t.Fatalf("comments %q", comments)
	}
	for _, want := range []string{head, "dev", "low"} {
		if !strings.Contains(comments[0], want) {
			t.Errorf("the comment lacks %q:\n%s", want, comments[0])
		}
	}
	if strings.Contains(comments[0], w.cfg.RunsDir) || strings.Contains(comments[0], w.g.checkout) {
		t.Errorf("the comment leaks a local path:\n%s", comments[0])
	}
	// a retry does not write the comment twice
	if _, err := w.decide(t, "o/r#1", head, false); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := len(w.fake.Comments(1)); got != 1 {
		t.Errorf("%d comments after the retry, want 1", got)
	}
	w.checkoutPristine(t, devBefore, m1Before)
	if got := w.g.remoteSHA("crw/m1"); got != head {
		t.Errorf("the remote head moved to %s without a base update", got)
	}
}

func TestDecideRefusesAHeadThatMoved(t *testing.T) {
	w := newDecideWorld(t)
	w.openPR(t, "crw/m1", 10)
	head := w.g.remoteSHA("crw/m1")
	moved := strings.Repeat("e", 40)
	w.green(head)
	w.green(moved)
	// the pull request's head moved before the first read
	w.fake.HeadSHA = func(string) string { return moved }
	dec, err := w.decide(t, "o/r#1", head, false)
	if err != nil || dec.State != "refused" || dec.Reason != "head_moved" {
		t.Fatalf("decision %+v err %v", dec, err)
	}
	if got := w.pendingHead(t, 1); got != "" {
		t.Errorf("a moved head is pending at %q", got)
	}
	if data, err := os.ReadFile(dec.Record); err != nil || !strings.Contains(string(data), `"state": "refused"`) {
		t.Errorf("the refusal is not recorded: %s %v", data, err)
	}
	// the head moves after the first read: the checks answer another head, and the pending file is never written
	w2 := newDecideWorld(t)
	w2.openPR(t, "crw/m1", 10)
	head2 := w2.g.remoteSHA("crw/m1")
	w2.green(head2)
	w2.green(moved)
	reads := 0
	w2.fake.HeadSHA = func(b string) string {
		reads++
		if reads > 1 {
			return moved
		}
		return w2.g.remoteSHA(b)
	}
	dec, err = w2.decide(t, "o/r#1", head2, false)
	if err != nil || dec.State != "refused" || dec.Reason != "head_moved" {
		t.Fatalf("stale during checks: %+v err %v", dec, err)
	}
	if got := w2.pendingHead(t, 1); got != "" {
		t.Errorf("pending written for a moved head: %q", got)
	}
	// the head moves only at the enqueue read: nothing is enqueued either
	w3 := newDecideWorld(t)
	w3.openPR(t, "crw/m1", 10)
	head3 := w3.g.remoteSHA("crw/m1")
	w3.green(head3)
	w3.green(moved)
	reads = 0
	w3.fake.HeadSHA = func(b string) string {
		reads++
		if reads > 4 {
			return moved
		}
		return w3.g.remoteSHA(b)
	}
	dec, err = w3.decide(t, "o/r#1", head3, false)
	if err == nil && dec.State == "enqueued" {
		t.Fatalf("a moved head was enqueued: %+v", dec)
	}
	if got := w3.pendingHead(t, 1); got != "" {
		t.Errorf("pending written for a moved head: %q", got)
	}
}

func TestDecideHoldsAPRThatOverlapsAnEarlierRequest(t *testing.T) {
	w := newDecideWorld(t)
	// an earlier pending pull request touches shared.txt; the current one touches it too
	shaA := w.pushBranchOf(t, "crw/m1a", "shared.txt", "from a\n")
	shaB := w.pushBranchOf(t, "crw/m2", "shared.txt", "from b\n")
	w.openPR(t, "crw/m1a", 10)
	w.openPR(t, "crw/m2", 11)
	w.pending(t, 1, shaA)
	w.green(shaB)
	dec, err := w.decide(t, "o/r#2", shaB, false)
	if err != nil || dec.State != "hold_order" || len(dec.Earlier) != 1 || dec.Earlier[0] != "o/r#1" {
		t.Fatalf("decision %+v err %v", dec, err)
	}
	if got := w.pendingHead(t, 2); got != "" {
		t.Errorf("a held pull request is pending at %q", got)
	}
	// an earlier pull request of another file does not hold the queue
	w2 := newDecideWorld(t)
	shaC := w2.pushBranchOf(t, "crw/m1a", "alone.txt", "a\n")
	shaD := w2.pushBranchOf(t, "crw/m2", "other.txt", "b\n")
	w2.openPR(t, "crw/m1a", 10)
	w2.openPR(t, "crw/m2", 11)
	w2.pending(t, 1, shaC)
	w2.green(shaD)
	dec, err = w2.decide(t, "o/r#2", shaD, false)
	if err != nil || dec.State != "enqueued" {
		t.Fatalf("unrelated earlier pull request: %+v err %v", dec, err)
	}
	if got := w2.pendingHead(t, 2); got != shaD {
		t.Errorf("pending/pr-2 = %q, want %s", got, shaD)
	}
	// an earlier entry whose queued head is not the pull request's is an error, never silently passed
	shaE := w2.pushBranchOf(t, "crw/m1a", "alone.txt", "newer\n")
	_ = shaE
	dec, err = w2.decide(t, "o/r#2", shaD, false)
	if err == nil {
		t.Errorf("a queued head that is not the pull request's head was decided %+v", dec)
	}
	if got := w2.pendingHead(t, 2); got != shaD {
		t.Errorf("pending/pr-2 = %q after the closed failure", got)
	}
}

func TestDecideUpdatesTheBaseNoFFWithoutForceAndAsksForCI(t *testing.T) {
	w := newDecideWorld(t)
	w.openPR(t, "crw/m1", 10)
	head := w.g.remoteSHA("crw/m1")
	devBefore, m1Before := w.g.git(w.g.checkout, "rev-parse", "dev"), w.g.git(w.g.checkout, "rev-parse", "crw/m1")
	w.green(head)
	// dev moves past the point crw/m1 left it
	other := filepath.Join(t.TempDir(), "other")
	w.g.git(t.TempDir(), "clone", "-q", w.g.bare, other)
	w.g.git(other, "checkout", "-q", "dev")
	if err := os.WriteFile(filepath.Join(other, "dev.txt"), []byte("dev moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.g.git(other, "add", "-A")
	w.g.git(other, "commit", "-q", "-m", "dev moved")
	w.g.git(other, "push", "-q", "origin", "dev")
	devTip := w.g.remoteSHA("dev")
	if devTip == devBefore {
		t.Fatal("dev did not move")
	}
	// without --update-base the decision is needs_base
	dec, err := w.decide(t, "o/r#1", head, false)
	if err != nil || dec.State != "needs_base" || dec.BaseHead != devTip {
		t.Fatalf("decision %+v err %v", dec, err)
	}
	if got := w.pendingHead(t, 1); got != "" {
		t.Errorf("needs_base enqueued %q", got)
	}
	log := gitArgvLog(t, "")
	// with it the base is merged into the head without fast-forward and pushed, never forced
	dec, err = w.decide(t, "o/r#1", head, true)
	if err != nil || dec.State != "needs_ci" || dec.UpdatedHead == "" {
		t.Fatalf("decision %+v err %v", dec, err)
	}
	merged := dec.UpdatedHead
	if got := w.g.remoteSHA("crw/m1"); got != merged {
		t.Fatalf("the remote head is %q, want the merge %s", got, merged)
	}
	if parents := w.g.git(w.g.bare, "rev-list", "--parents", "-n", "1", merged); parents != merged+" "+head+" "+devTip {
		t.Errorf("the pushed merge's parents are %q, want head %s then base %s", parents, head, devTip)
	}
	var pushes int
	for _, line := range argvLines(t, log) {
		if strings.Contains(line, " push ") {
			pushes++
			if !strings.Contains(line, "--porcelain") || !strings.Contains(line, "core.hooksPath") ||
				!strings.Contains(line, merged+":refs/heads/crw/m1") || strings.Contains(line, "--force") ||
				strings.Contains(line, " +") || strings.Contains(line, ":+") {
				t.Errorf("push argv %q", line)
			}
		}
		if strings.Contains(line, " merge ") && !strings.Contains(line, "--no-ff") {
			t.Errorf("a fast-forwardable merge ran: %q", line)
		}
	}
	if pushes != 1 {
		t.Errorf("%d pushes, want 1", pushes)
	}
	if got := w.pendingHead(t, 1); got != "" {
		t.Errorf("needs_ci enqueued %q", got)
	}
	if len(w.fake.Comments(1)) != 0 {
		t.Error("a base update commented on the pull request")
	}
	w.checkoutPristine(t, devBefore, m1Before)
	if data, err := os.ReadFile(dec.Record); err != nil || !strings.Contains(string(data), `"state": "needs_ci"`) || !strings.Contains(string(data), merged) {
		t.Errorf("the needs_ci record: %s %v", data, err)
	}
}

// TestDecideBaseUpdateCASStopsARemoteThatMovedDuringPush: between the last advertised read and the push the remote head moves
// — the private pre-push hook refuses the update, nothing is pushed and nothing is enqueued.
func TestDecideBaseUpdateCASStopsARemoteThatMovedDuringPush(t *testing.T) {
	w := newDecideWorld(t)
	w.openPR(t, "crw/m1", 10)
	head := w.g.remoteSHA("crw/m1")
	w.green(head)
	other := filepath.Join(t.TempDir(), "other")
	w.g.git(t.TempDir(), "clone", "-q", w.g.bare, other)
	w.g.git(other, "checkout", "-q", "dev")
	if err := os.WriteFile(filepath.Join(other, "dev.txt"), []byte("dev moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.g.git(other, "add", "-A")
	w.g.git(other, "commit", "-q", "-m", "dev moved")
	w.g.git(other, "push", "-q", "origin", "dev")
	// ADV is the dev tip the wrapper moves crw/m1 to while the push runs: it is a parent of the merge the decision pushes, so
	// the update is fast-forwardable — only the compare-and-swap hook can refuse it
	adv := w.g.remoteSHA("dev")
	move := "case \" $* \" in *\" push \"*) git -C " + w.g.bare + " update-ref refs/heads/crw/m1 " + adv + ";; esac"
	gitArgvLog(t, move)
	dec, err := w.decide(t, "o/r#1", head, true)
	if err == nil || dec.State == "enqueued" || dec.State == "needs_ci" {
		t.Fatalf("a moved remote was pushed: %+v err %v", dec, err)
	}
	if got := w.g.remoteSHA("crw/m1"); got != adv {
		t.Fatalf("the remote head is %q: the push moved it past %s", got, adv)
	}
	if got := w.pendingHead(t, 1); got != "" {
		t.Errorf("pending written: %q", got)
	}
	if len(w.fake.Comments(1)) != 0 {
		t.Error("a failed CAS commented")
	}
}

func TestDecideReadsNoDiffContent(t *testing.T) {
	w := newDecideWorld(t)
	shaA := w.pushBranchOf(t, "crw/m1a", "shared.txt", "a\n")
	shaB := w.pushBranchOf(t, "crw/m2", "shared.txt", "b\n")
	w.openPR(t, "crw/m1a", 10)
	w.openPR(t, "crw/m2", 11)
	w.pending(t, 1, shaA)
	w.green(shaB)
	log := gitArgvLog(t, "")
	dec, err := w.decide(t, "o/r#2", shaB, false)
	if err != nil || dec.State != "hold_order" {
		t.Fatalf("decision %+v err %v", dec, err)
	}
	for _, line := range argvLines(t, log) {
		if strings.Contains(line, " diff ") {
			for _, want := range []string{"--name-only", "--no-renames", "-z"} {
				if !strings.Contains(line, want) {
					t.Errorf("diff argv lacks %s: %q", want, line)
				}
			}
		}
		if strings.Contains(line, " show ") || strings.Contains(line, " log ") || strings.Contains(line, "format-patch") {
			t.Errorf("content-reading git call: %q", line)
		}
	}
	for _, req := range w.fake.Requests() {
		if strings.Contains(req, "/files") || strings.HasSuffix(req, ".diff") || strings.HasSuffix(req, ".patch") {
			t.Errorf("a diff-content request ran: %s", req)
		}
	}
}

func TestDecideRecordsTheReleaseImpact(t *testing.T) {
	w := newDecideWorld(t)
	w.openPR(t, "crw/m1", 10)
	head := w.g.remoteSHA("crw/m1")
	w.green(head)
	dec, err := Decide(context.Background(), w.runner, DecideInput{PR: "o/r#1", Head: head, ReleaseImpact: "adds the pr decide command"})
	if err != nil || dec.State != "enqueued" {
		t.Fatalf("%+v %v", dec, err)
	}
	data, err := os.ReadFile(dec.Record)
	if err != nil || !strings.Contains(string(data), `"release_impact": "adds the pr decide command"`) {
		t.Fatalf("the record lacks the impact: %s %v", data, err)
	}
	if c := w.fake.Comments(1); len(c) != 1 || !strings.Contains(c[0], "adds the pr decide command") {
		t.Errorf("the comment lacks the impact: %q", c)
	}
	// a refusal records it too
	w2 := newDecideWorld(t)
	w2.openPR(t, "crw/m1", 10)
	head = w2.g.remoteSHA("crw/m1")
	dec, err = Decide(context.Background(), w2.runner, DecideInput{PR: "o/r#1", Head: head, ReleaseImpact: "none"})
	if err != nil || dec.State != "refused" {
		t.Fatalf("%+v %v", dec, err)
	}
	if data, err := os.ReadFile(dec.Record); err != nil || !strings.Contains(string(data), `"release_impact": "none"`) {
		t.Errorf("the refusal record lacks the impact: %s %v", data, err)
	}
}

func TestDecideRefusesTheOtherSemanticStates(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(w *decideWorld, head string)
		want    string
	}{
		"not open": {func(w *decideWorld, head string) {
			w.fake.SetPull(1, func(p *trackingfake.Pull) { p.State = "closed" })
		}, "pr_not_open"},
		"wrong base": {func(w *decideWorld, head string) {
			w.fake.SetPull(1, func(p *trackingfake.Pull) { p.Base = "main" })
		}, "wrong_base"},
		"another repository": {func(w *decideWorld, head string) {
			w.fake.SetPull(1, func(p *trackingfake.Pull) { p.HeadRepo = "x/y" })
		}, "wrong_repository"},
		"pending checks": {func(w *decideWorld, head string) {
			w.fake.SetChecks(head, []trackingfake.FakeCheckRun{{Name: "ci", Status: "in_progress"}}, nil)
		}, "checks_pending"},
		"failed checks": {func(w *decideWorld, head string) {
			w.fake.SetChecks(head, []trackingfake.FakeCheckRun{{Name: "ci", Status: "completed", Conclusion: "failure"}}, nil)
		}, "checks_failed"},
		"no checks": {func(w *decideWorld, head string) {}, "checks_none"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newDecideWorld(t)
			w.openPR(t, "crw/m1", 10)
			head := w.g.remoteSHA("crw/m1")
			tc.prepare(w, head)
			dec, err := w.decide(t, "o/r#1", head, false)
			if err != nil || dec.State != "refused" || dec.Reason != tc.want {
				t.Fatalf("%s: %+v err %v", name, dec, err)
			}
			if got := w.pendingHead(t, 1); got != "" {
				t.Errorf("%s enqueued %q", name, got)
			}
		})
	}
}

// TestDecideCLI wires `crw flow pr decide` through the dependencies seam.
func TestDecideCLI(t *testing.T) {
	w := newDecideWorld(t)
	w.openPR(t, "crw/m1", 10)
	head := w.g.remoteSHA("crw/m1")
	w.green(head)
	d := deps(t, w.fake, w.g)
	d.IntegrateConfig = func() (integrator.Config, error) { return w.cfg, nil }
	code, out, errOut := run(t, d, "decide", "--pr", "o/r#1", "--head", head, "--release-impact", "none")
	if code != 0 {
		t.Fatalf("decide: %d %s %s", code, out, errOut)
	}
	var dec Decision
	if err := json.Unmarshal([]byte(out), &dec); err != nil || dec.State != "enqueued" {
		t.Fatalf("answer %s: %v", out, err)
	}
	for name, args := range map[string][]string{
		"no pr":        {"decide", "--head", head, "--release-impact", "x"},
		"no head":      {"decide", "--pr", "o/r#1", "--release-impact", "x"},
		"no impact":    {"decide", "--pr", "o/r#1", "--head", head},
		"a short head": {"decide", "--pr", "o/r#1", "--head", "abc", "--release-impact", "x"},
		"other repo":   {"decide", "--pr", "x/y#1", "--head", head, "--release-impact", "x"},
	} {
		if code, _, _ := run(t, d, args...); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
	// a refusal is exit 2 with the decision on stdout
	fresh := newDecideWorld(t)
	fresh.openPR(t, "crw/m1", 10)
	head = fresh.g.remoteSHA("crw/m1")
	d2 := deps(t, fresh.fake, fresh.g)
	d2.IntegrateConfig = func() (integrator.Config, error) { return fresh.cfg, nil }
	code, out, _ = run(t, d2, "decide", "--pr", "o/r#1", "--head", head, "--release-impact", "x")
	if code != 2 {
		t.Fatalf("refusal exit %d %s", code, out)
	}
	if err := json.Unmarshal([]byte(out), &dec); err != nil || dec.State != "refused" || dec.Reason != "checks_none" {
		t.Errorf("answer %s", out)
	}
}

// TestDecideBaseUpdateRunsWhileChecksAreNotGreen: a base update is not blocked by the old head's checks — pending, red or
// absent — because those are exactly what the update is for. Whatever the check state, the merge is pushed and needs_ci is
// answered; nothing is enqueued.
func TestDecideBaseUpdateRunsWhileChecksAreNotGreen(t *testing.T) {
	for name, prepare := range map[string]func(w *decideWorld, head string){
		"pending": func(w *decideWorld, head string) {
			w.fake.SetChecks(head, []trackingfake.FakeCheckRun{{Name: "ci", Status: "in_progress"}}, nil)
		},
		"failure": func(w *decideWorld, head string) {
			w.fake.SetChecks(head, []trackingfake.FakeCheckRun{{Name: "ci", Status: "completed", Conclusion: "failure"}}, nil)
		},
		"none": func(w *decideWorld, head string) {},
	} {
		t.Run(name, func(t *testing.T) {
			w := newDecideWorld(t)
			w.openPR(t, "crw/m1", 10)
			head := w.g.remoteSHA("crw/m1")
			prepare(w, head)
			// dev moves past the head
			other := filepath.Join(t.TempDir(), "other")
			w.g.git(t.TempDir(), "clone", "-q", w.g.bare, other)
			w.g.git(other, "checkout", "-q", "dev")
			if err := os.WriteFile(filepath.Join(other, "dev.txt"), []byte("dev moved\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			w.g.git(other, "add", "-A")
			w.g.git(other, "commit", "-q", "-m", "dev moved")
			w.g.git(other, "push", "-q", "origin", "dev")
			dec, err := w.decide(t, "o/r#1", head, true)
			if err != nil || dec.State != "needs_ci" || dec.UpdatedHead == "" {
				t.Fatalf("%s: %+v err %v", name, dec, err)
			}
			if got := w.pendingHead(t, 1); got != "" {
				t.Errorf("%s: %s enqueued %q", name, dec.State, got)
			}
		})
	}
}

// TestDecideReadyCommentNeverClaimsEnqueued: the pre-enqueue comment describes a ready decision, not an enqueued one; when the
// pull request moves at the final read the queue stays empty and the comment never claimed enqueued.
func TestDecideReadyCommentNeverClaimsEnqueued(t *testing.T) {
	w := newDecideWorld(t)
	w.openPR(t, "crw/m1", 10)
	head := w.g.remoteSHA("crw/m1")
	moved := strings.Repeat("e", 40)
	w.green(head)
	w.green(moved)
	reads := 0
	w.fake.HeadSHA = func(b string) string {
		reads++
		if reads > 4 {
			return moved
		}
		return w.g.remoteSHA(b)
	}
	dec, err := w.decide(t, "o/r#1", head, false)
	if err == nil && dec.State == "enqueued" {
		t.Fatalf("a moved head was enqueued: %+v", dec)
	}
	if got := w.pendingHead(t, 1); got != "" {
		t.Fatalf("pending written for a moved head: %q", got)
	}
	for _, c := range w.fake.Comments(1) {
		if strings.Contains(c, "state: enqueued") {
			t.Errorf("a comment claims enqueued while the queue is empty:\n%s", c)
		}
		if !strings.Contains(c, "state: ready") {
			t.Errorf("the pre-enqueue comment does not say ready:\n%s", c)
		}
	}
}

// shq single-quotes a path for an sh script.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }

// captureHook is a pre-push hook that writes argv, cwd, stdin and a call counter into dir, then exits with code.
func captureHook(dir string, code int) string {
	return "#!/bin/sh\n" +
		"cat > " + shq(filepath.Join(dir, "stdin")) + "\n" +
		"pwd > " + shq(filepath.Join(dir, "cwd")) + "\n" +
		"printf '%s\\n' \"$@\" > " + shq(filepath.Join(dir, "argv")) + "\n" +
		"printf 'x\\n' >> " + shq(filepath.Join(dir, "calls")) + "\n" +
		"exit " + fmt.Sprint(code) + "\n"
}

// TestDecideBaseUpdatePreservesPrePushHook: the compare-and-swap push of a base update runs the repository's configured
// pre-push hook with the same arguments, working directory and update lines; a refusing hook stops the push (and nothing is
// pushed or enqueued), a passing one lets it through exactly once.
func TestDecideBaseUpdatePreservesPrePushHook(t *testing.T) {
	for name, install := range map[string]func(t *testing.T, w *decideWorld, script string){
		"the default hooks dir": func(t *testing.T, w *decideWorld, script string) {
			hook := filepath.Join(w.g.checkout, ".git", "hooks", "pre-push")
			if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"core.hooksPath relative with a space and quote": func(t *testing.T, w *decideWorld, script string) {
			dir := filepath.Join(w.g.checkout, "my hooks' dir")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "pre-push"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			w.g.git(w.g.checkout, "config", "core.hooksPath", "my hooks' dir")
		},
		"core.hooksPath absolute": func(t *testing.T, w *decideWorld, script string) {
			dir := filepath.Join(t.TempDir(), "hooks here")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "pre-push"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			w.g.git(w.g.checkout, "config", "core.hooksPath", dir)
		},
	} {
		for _, refusing := range []bool{false, true} {
			t.Run(name+map[bool]string{true: " refuses", false: " passes"}[refusing], func(t *testing.T) {
				w := newDecideWorld(t)
				w.openPR(t, "crw/m1", 10)
				head := w.g.remoteSHA("crw/m1")
				w.green(head)
				// dev moves past the head
				other := filepath.Join(t.TempDir(), "other")
				w.g.git(t.TempDir(), "clone", "-q", w.g.bare, other)
				w.g.git(other, "checkout", "-q", "dev")
				if err := os.WriteFile(filepath.Join(other, "dev.txt"), []byte("dev moved\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				w.g.git(other, "add", "-A")
				w.g.git(other, "commit", "-q", "-m", "dev moved")
				w.g.git(other, "push", "-q", "origin", "dev")
				// the hook is installed only after the fixture pushes, so the only run it can see is the decision's
				capture := t.TempDir()
				code := 0
				if refusing {
					code = 37
				}
				install(t, w, captureHook(capture, code))
				dec, err := w.decide(t, "o/r#1", head, true)
				read := func(name string) string {
					data, _ := os.ReadFile(filepath.Join(capture, name))
					return string(data)
				}
				if refusing {
					if err == nil || dec.State == "needs_ci" || dec.State == "enqueued" {
						t.Fatalf("a refusing pre-push hook did not stop the push: %+v err %v", dec, err)
					}
					if got := w.g.remoteSHA("crw/m1"); got != head {
						t.Errorf("the remote head moved to %s past a refusing hook", got)
					}
					if got := w.pendingHead(t, 1); got != "" {
						t.Errorf("pending written past a refusing hook: %q", got)
					}
					if got := read("calls"); got != "x\n" {
						t.Errorf("the configured hook ran %q times, want once", got)
					}
					return
				}
				if err != nil || dec.State != "needs_ci" || dec.UpdatedHead == "" {
					t.Fatalf("decision %+v err %v", dec, err)
				}
				if got := read("calls"); got != "x\n" {
					t.Fatalf("the configured hook ran %q, want once", got)
				}
				// the hook saw the real push: remote name and URL as argv, the repo root as cwd, the update on stdin
				if got := read("argv"); !strings.Contains(got, "origin") || !strings.Contains(got, w.g.bare) {
					t.Errorf("hook argv %q, want the remote name and the bare url", got)
				}
				if got := strings.TrimSpace(read("cwd")); got != w.g.checkout {
					t.Errorf("hook cwd %q, want the checkout %s", got, w.g.checkout)
				}
				stdin := read("stdin")
				for _, want := range []string{dec.UpdatedHead, "refs/heads/crw/m1", head} {
					if !strings.Contains(stdin, want) {
						t.Errorf("hook stdin lacks %q:\n%s", want, stdin)
					}
				}
				if got := w.g.remoteSHA("crw/m1"); got != dec.UpdatedHead {
					t.Errorf("the remote head is %q, want the merge %s", got, dec.UpdatedHead)
				}
			})
		}
	}
}
