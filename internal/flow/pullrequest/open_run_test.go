package pullrequest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/publishpolicy"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

// runWorld is a checkout with the milestone branch crw/m1, a bare remote, the fake GitHub and a stub journal that answers the rows
// of spec: everything `crw flow pr open --run` reaches.
type runWorld struct {
	t       *testing.T
	g       *gitWorld
	fake    *trackingfake.Server
	d       *Deps
	tip     string
	spec    runSpec
	unknown bool
	// journalCalls and sources record what the command asked of the journal.
	journalCalls int
	sources      []Source
}

func newRunWorld(t *testing.T, issues ...string) *runWorld {
	t.Helper()
	w := &runWorld{t: t, g: newGitWorld(t), fake: trackingfake.New(t, "o/r")}
	w.fake.HeadSHA = w.g.remoteSHA
	w.tip = w.g.git(w.g.checkout, "rev-parse", "refs/heads/crw/m1")
	if len(issues) == 0 {
		issues = []string{"o:r:10", "o:r:11"}
	}
	w.spec = newRunSpec(w.tip, issues...)
	w.spec.Crosscheck = []engine.Event{crosscheckRow("M1", w.tip, "verdict", "verified", "f1 [P3/minor_separable] x.go:2 tidy up the helper")}
	w.d = deps(t, w.fake, w.g)
	w.d.Hostname = func() string { return "testhost" }
	w.d.Home = func() string { return "/home/tester" }
	w.d.Journal = func(_ context.Context, src Source, run string) ([]engine.Event, error) {
		w.journalCalls++
		w.sources = append(w.sources, src)
		if w.unknown {
			return nil, ErrNoRun
		}
		return w.spec.events(w.t), nil
	}
	return w
}

// open runs `crw flow pr open --run run-1 --push <checkout>` with the extra arguments.
func (w *runWorld) open(extra ...string) (int, string, string) {
	w.t.Helper()
	args := append([]string{"open", "--repo", "o/r", "--head", "crw/m1", "--title", "milestone M1", "--run", "run-1", "--push", w.g.checkout}, extra...)
	return run(w.t, w.d, args...)
}

// nothingSent is the state after a refusal: no branch on the remote and no request to GitHub.
func (w *runWorld) nothingSent() {
	w.t.Helper()
	if got := w.g.remoteSHA("crw/m1"); got != "" {
		w.t.Errorf("the remote has crw/m1 at %s", got)
	}
	if n := len(w.fake.Pulls()); n != 0 {
		w.t.Errorf("%d pull requests opened", n)
	}
	if reqs := w.fake.Requests(); len(reqs) != 0 {
		w.t.Errorf("requests sent to GitHub: %q", reqs)
	}
}

// TestOpenFromARunRefusesAnUnclosedMilestoneOrAMissingCrosscheck (CRW-1451): `open --run` reads the run's journal and refuses, with
// exit 2, one line and before anything is pushed or sent, a run that did not close its milestone, a crosscheck that is missing, moved
// or not accepted, issues it cannot name, and a body that leaks. The good case opens one pull request whose body has the five parts
// and the Carries line of the decoded issues.
func TestOpenFromARunRefusesAnUnclosedMilestoneOrAMissingCrosscheck(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(w *runWorld)
		extra  []string
		reason string
	}{
		{"an escalated outcome", func(w *runWorld) { w.spec.Outcome = map[string]any{"state": "escalated", "milestone": "M1"} }, nil, "run_not_closed"},
		{"a failed run", func(w *runWorld) { w.spec.Status = "failed" }, nil, "run_not_closed"},
		{"a resumed run", func(w *runWorld) { w.spec.Resumed = true }, nil, "run_not_closed"},
		{"a target that moved after the gate", func(w *runWorld) { w.spec.Moved = true }, nil, "run_not_closed"},
		{"an unknown run", func(w *runWorld) { w.unknown = true }, nil, "run_unknown"},
		{"no crosscheck row", func(w *runWorld) { w.spec.Crosscheck = nil }, nil, "crosscheck_missing"},
		{"a needs_changes verdict", func(w *runWorld) {
			w.spec.Crosscheck = []engine.Event{crosscheckRow("M1", w.tip, "verdict", "needs_changes")}
		}, nil, "crosscheck_not_accepted"},
		{"an evidence row", func(w *runWorld) {
			w.spec.Crosscheck = []engine.Event{crosscheckRow("M1", w.tip, "verdict", "verified"), crosscheckRow("M1", w.tip, "evidence", "needs_changes")}
		}, nil, "crosscheck_not_accepted"},
		{"a verified row of another head", func(w *runWorld) {
			w.spec.Crosscheck = []engine.Event{crosscheckRow("M1", headA, "verdict", "verified")}
		}, nil, "crosscheck_head_moved"},
		{"a Linear key without --issue", func(w *runWorld) { w.spec.Issues = []string{"CRW-1440"} }, nil, "issues_unknown"},
		{"--issue that disagrees with the nodes", nil, []string{"--issue", "o/r#10,o/r#99"}, "issues_not_the_milestones"},
		{"an issue of another repository", func(w *runWorld) { w.spec.Issues = []string{"x:y:10"} }, nil, "issue_other_repo"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newRunWorld(t)
			if c.mutate != nil {
				c.mutate(w)
			}
			code, out, errOut := w.open(c.extra...)
			if code != 2 || out != "" || !strings.Contains(errOut, c.reason) {
				t.Fatalf("exit %d, stdout %q, stderr %q; want exit 2 naming %s", code, out, errOut, c.reason)
			}
			if lines := strings.Split(strings.TrimSpace(errOut), "\n"); len(lines) != 1 {
				t.Errorf("a refusal is one line, got %d: %q", len(lines), errOut)
			}
			w.nothingSent()
		})
	}

	t.Run("the closed milestone opens one pull request", func(t *testing.T) {
		w := newRunWorld(t)
		code, out, errOut := w.open()
		if code != 0 {
			t.Fatalf("exit %d\n%s%s", code, out, errOut)
		}
		var ans Answer
		if err := json.Unmarshal([]byte(out), &ans); err != nil {
			t.Fatalf("answer %q: %v", out, err)
		}
		if ans.Pushed != w.tip || ans.PullRequest.Head != w.tip || strings.Join(ans.PullRequest.Issues, ",") != "o/r#10,o/r#11" || len(ans.Warnings) != 0 {
			t.Errorf("answer %+v", ans)
		}
		pulls := w.fake.Pulls()
		if len(pulls) != 1 {
			t.Fatalf("%d pull requests", len(pulls))
		}
		body := pulls[1].Body
		order := []string{"## Result\nMilestone M1 closed at " + w.tip[:12] + "; gate pass.", "## Closes\n- o/r#10\n- o/r#11", "## Verification\n- test go test ./internal/flow/pullrequest/... fresh 0123456789ab\n- qa fresh",
			"## Crosscheck\nverified at " + w.tip[:12] + ", round 1, gpt/gpt-6-astra, guide crw-crosscheck/1", "## Follow-ups\n- f1 [P3/minor_separable] x.go:2 tidy up the helper", "Carries: o/r#10, o/r#11"}
		at := 0
		for _, part := range order {
			i := strings.Index(body[at:], part)
			if i < 0 {
				t.Fatalf("the body lacks %q after offset %d:\n%s", part, at, body)
			}
			at += i + len(part)
		}
		if strings.Contains(body, "c_aaaaaaaaaaaa") || strings.Contains(body, "stray") || strings.Contains(body, "c_0123456789ab") {
			t.Errorf("the body prints a call id or a why:\n%s", body)
		}
		if w.journalCalls != 1 || w.sources[0] != (Source{}) {
			t.Errorf("journal calls %d sources %+v", w.journalCalls, w.sources)
		}
	})

	t.Run("--issue equal to the nodes' keys is accepted, and a Linear-keyed milestone takes --issue as given", func(t *testing.T) {
		w := newRunWorld(t)
		if code, out, errOut := w.open("--issue", "o/r#11", "--issue", "o/r#10"); code != 0 {
			t.Fatalf("the same set in another order: exit %d\n%s%s", code, out, errOut)
		}
		// GitHub names are case-insensitive: Owner:Repo:10 is an issue of o/r, and --issue o/R#10 names the same one
		w = newRunWorld(t, "O:R:10")
		if code, out, errOut := w.open("--issue", "o/R#10"); code != 0 {
			t.Fatalf("the same issue in another case: exit %d\n%s%s", code, out, errOut)
		}
		w = newRunWorld(t, "CRW-1440")
		if code, out, errOut := w.open("--issue", "o/r#5"); code != 0 {
			t.Fatalf("a Linear key with --issue: exit %d\n%s%s", code, out, errOut)
		}
		if p := w.fake.Pulls(); len(p) != 1 || !strings.HasSuffix(p[1].Body, "Carries: o/r#5") {
			t.Errorf("pull requests %+v", p)
		}
	})
}

// TestOpenFromARunPushesTheCrosscheckedTipOnly (CRW-1451, R1-02): the head that is opened is the commit the crosscheck judged. --run
// needs --push, a local branch that moved off the tip is refused, the push names the tip's sha and is never forced, and a pull
// request whose head is not the tip is reported.
func TestOpenFromARunPushesTheCrosscheckedTipOnly(t *testing.T) {
	t.Run("--run without --push is a usage error before the journal is read", func(t *testing.T) {
		w := newRunWorld(t)
		code, _, errOut := run(t, w.d, "open", "--repo", "o/r", "--head", "crw/m1", "--title", "t", "--run", "run-1")
		if code != 2 || !strings.Contains(errOut, "--push") || !strings.Contains(errOut, "usage:") || w.journalCalls != 0 {
			t.Errorf("exit %d %q, journal calls %d", code, errOut, w.journalCalls)
		}
		w.nothingSent()
	})

	t.Run("--run with a body is a usage error", func(t *testing.T) {
		w := newRunWorld(t)
		for _, extra := range [][]string{{"--body", "x"}, {"--body-file", "/nonexistent"}} {
			if code, _, errOut := w.open(extra...); code != 2 || !strings.Contains(errOut, "usage:") {
				t.Errorf("%v: exit %d %q", extra, code, errOut)
			}
		}
		if w.journalCalls != 0 {
			t.Errorf("the journal was read %d times", w.journalCalls)
		}
		w.nothingSent()
	})

	t.Run("--state and --socket without --run are a usage error", func(t *testing.T) {
		w := newRunWorld(t)
		code, _, errOut := run(t, w.d, "open", "--repo", "o/r", "--head", "crw/m1", "--title", "t", "--issue", "o/r#1", "--state", "/x")
		if code != 2 || !strings.Contains(errOut, "--run") {
			t.Errorf("exit %d %q", code, errOut)
		}
	})

	t.Run("a local branch that moved off the crosschecked tip opens nothing", func(t *testing.T) {
		w := newRunWorld(t)
		w.g.git(w.g.checkout, "branch", "-f", "crw/m1", "dev")
		code, _, errOut := w.open()
		if code != 2 || !strings.Contains(errOut, "crosscheck_head_moved") {
			t.Errorf("exit %d %q", code, errOut)
		}
		w.nothingSent()
	})

	t.Run("a local branch that is missing opens nothing", func(t *testing.T) {
		w := newRunWorld(t)
		w.g.git(w.g.checkout, "branch", "-D", "crw/m1")
		code, _, errOut := w.open()
		if code != 2 || !strings.Contains(errOut, "crosscheck_head_moved") {
			t.Errorf("exit %d %q", code, errOut)
		}
		w.nothingSent()
	})

	t.Run("pushCommit pushes the sha it is given, not what the local ref is at", func(t *testing.T) {
		g := newGitWorld(t)
		devTip := g.git(g.checkout, "rev-parse", "dev")
		if local := g.git(g.checkout, "rev-parse", "crw/m1"); local == devTip {
			t.Fatal("the fixture's branches are the same commit")
		}
		if err := pushCommit(context.Background(), g.env, g.checkout, "origin", "crw/m1", devTip); err != nil {
			t.Fatal(err)
		}
		if got := g.remoteSHA("crw/m1"); got != devTip {
			t.Errorf("the remote's crw/m1 is %s, want the sha given %s", got, devTip)
		}
	})

	t.Run("a remote branch that is ahead of the tip refuses the push and opens nothing", func(t *testing.T) {
		w := newRunWorld(t)
		g := w.g
		g.git(g.checkout, "checkout", "-q", "-b", "ahead", "crw/m1")
		g.git(g.checkout, "commit", "-q", "--allow-empty", "-m", "someone else")
		ahead := g.git(g.checkout, "rev-parse", "HEAD")
		g.git(g.checkout, "push", "-q", "origin", "ahead:refs/heads/crw/m1")
		g.git(g.checkout, "checkout", "-q", "dev")
		code, _, errOut := w.open()
		if code != 1 || !strings.Contains(errOut, "refused") {
			t.Errorf("exit %d %q", code, errOut)
		}
		if got := g.remoteSHA("crw/m1"); got != ahead {
			t.Errorf("the remote's crw/m1 moved to %s, want %s", got, ahead)
		}
		if n := len(w.fake.Pulls()); n != 0 {
			t.Errorf("%d pull requests opened", n)
		}
	})

	t.Run("a pull request whose head is not the tip is reported with exit 1", func(t *testing.T) {
		w := newRunWorld(t)
		w.fake.HeadSHA = func(string) string { return headC }
		code, out, errOut := w.open()
		if code != 1 {
			t.Fatalf("exit %d\n%s%s", code, out, errOut)
		}
		var ans Answer
		if err := json.Unmarshal([]byte(out), &ans); err != nil {
			t.Fatalf("the answer is still printed: %q: %v", out, err)
		}
		warned := strings.Join(ans.Warnings, "\n")
		if ans.Pushed != w.tip || !strings.Contains(warned, "pr_head_moved") || !strings.Contains(warned, headC[:12]) || !strings.Contains(warned, w.tip[:12]) {
			t.Errorf("answer %+v", ans)
		}
		if n := len(w.fake.Pulls()); n != 1 {
			t.Errorf("%d pull requests, want the one that was opened", n)
		}
	})
}

// TestOpenScrubsPrivateTextOnTheManualPath (CRW-1549): a --body or a title that holds a private
// detail is no longer refused — the pull request opens with the sanitized text, the answer records
// the redactions and the forge never sees the original. A token is still stopped before the push.
func TestOpenScrubsPrivateTextOnTheManualPath(t *testing.T) {
	t.Run("privacy text is masked", func(t *testing.T) {
		w := newRunWorld(t)
		code, out, errOut := run(t, w.d, "open", "--repo", "o/r", "--head", "crw/m1", "--issue", "o/r#1", "--push", w.g.checkout,
			"--body", "see /state/crw/notes for the details", "--title", "fix on testhost")
		if code != 0 {
			t.Fatalf("exit %d %q", code, errOut)
		}
		var ans Answer
		if err := json.Unmarshal([]byte(out), &ans); err != nil {
			t.Fatalf("answer: %v", err)
		}
		if len(ans.Redactions) == 0 {
			t.Errorf("no redactions recorded: %+v", ans)
		}
		for _, ev := range ans.Redactions {
			if ev.Blocked || len(ev.Changes) == 0 {
				t.Errorf("event %+v", ev)
			}
			if raw, _ := json.Marshal(ev); strings.Contains(string(raw), "/state/crw") || strings.Contains(string(raw), "testhost") {
				t.Errorf("the event echoes the private text: %s", raw)
			}
		}
		pulls := w.fake.Pulls()
		if len(pulls) != 1 {
			t.Fatalf("%d pull requests opened", len(pulls))
		}
		for _, sent := range pulls {
			if strings.Contains(sent.Title, "testhost") || !strings.Contains(sent.Title, "[hostname]") ||
				strings.Contains(sent.Body, "/state/crw") || !strings.Contains(sent.Body, "[path]") {
				t.Errorf("the forge got %q / %q", sent.Title, sent.Body)
			}
		}
	})

	t.Run("a token is still stopped before the push", func(t *testing.T) {
		w := newRunWorld(t)
		token := "ghp_" + strings.Repeat("A", 24)
		code, out, errOut := run(t, w.d, "open", "--repo", "o/r", "--head", "crw/m1", "--issue", "o/r#1", "--push", w.g.checkout,
			"--body", "key "+token, "--title", "t")
		if code != 2 || out != "" || !strings.Contains(errOut, "body_leak") {
			t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
		}
		if strings.Contains(errOut, token) {
			t.Errorf("the refusal echoes the token: %q", errOut)
		}
		w.nothingSent()
	})

	t.Run("a private repository's own details pass when it says so", func(t *testing.T) {
		w := newRunWorld(t)
		w.d.PublicationPolicy = func(string) (publishpolicy.Policy, error) {
			return publishpolicy.Policy{Disabled: true}, nil
		}
		code, _, errOut := run(t, w.d, "open", "--repo", "o/r", "--head", "crw/m1", "--issue", "o/r#1", "--push", w.g.checkout,
			"--body", "see /state/crw/notes", "--title", "fix on testhost")
		if code != 0 {
			t.Fatalf("exit %d %q", code, errOut)
		}
		if len(w.fake.Pulls()) != 1 {
			t.Fatalf("%d pull requests opened", len(w.fake.Pulls()))
		}
		for _, sent := range w.fake.Pulls() {
			if !strings.Contains(sent.Title, "testhost") {
				t.Errorf("a private repo's own name was masked anyway: %q", sent.Title)
			}
		}
		// a token still stops it
		w2 := newRunWorld(t)
		w2.d.PublicationPolicy = func(string) (publishpolicy.Policy, error) {
			return publishpolicy.Policy{Disabled: true}, nil
		}
		code, _, errOut = run(t, w2.d, "open", "--repo", "o/r", "--head", "crw/m1", "--issue", "o/r#1", "--push", w2.g.checkout,
			"--body", "k "+"ghp_"+strings.Repeat("B", 24), "--title", "t")
		if code != 2 {
			t.Fatalf("a disabled policy published a secret: exit %d %q", code, errOut)
		}
		w2.nothingSent()
	})
}

// TestRunSecretIsCheckedBeforeBodyTruncation (CRW-1549): a token shape inside the run's gate
// evidence is judged before the body renders, where the digest field is cut to 12 characters and a
// 24-character token would no longer match its pattern. The whole open refuses: nothing is pushed,
// nothing is posted, and the refusal never echoes it.
func TestRunSecretIsCheckedBeforeBodyTruncation(t *testing.T) {
	w := newRunWorld(t)
	w.spec.Gate = strings.Replace(w.spec.Gate, `"digest":"0123456789abcdef0123"`, `"digest":"ghp_`+strings.Repeat("A", 20)+`"`, 1)
	code, out, errOut := w.open()
	if code != 2 || out != "" || !strings.Contains(errOut, "body_leak") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if strings.Contains(errOut, "ghp_") || strings.Contains(errOut, strings.Repeat("A", 20)) {
		t.Errorf("the refusal echoes the secret: %q", errOut)
	}
	w.nothingSent()
}

// TestOpenFromRunRecordsOnlyPublishedRedactions (CRW-1549): a private detail in a generated body is
// replaced and recorded once — the answer's redactions and the stderr events name the field and the
// kinds, and the raw fact check counts nothing it never rendered.
func TestOpenFromRunRecordsOnlyPublishedRedactions(t *testing.T) {
	for name, followUp := range map[string]string{
		"home path": "f1 see /home/tester/notes.md",
		"hostname":  "f1 reproduced on testhost only",
		"uuid":      "f1 thread 019a2b3c-4d5e-7f60-8a9b-0c1d2e3f4a5b",
	} {
		t.Run(name, func(t *testing.T) {
			w := newRunWorld(t)
			w.spec.Crosscheck = []engine.Event{crosscheckRow("M1", w.tip, "verdict", "verified", followUp)}
			code, out, errOut := w.open()
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			var ans Answer
			if err := json.Unmarshal([]byte(out), &ans); err != nil {
				t.Fatal(err)
			}
			if len(ans.Redactions) == 0 {
				t.Fatalf("no redactions recorded: %q", out)
			}
			for _, ev := range ans.Redactions {
				if ev.Blocked || ev.Field == "" || len(ev.Changes) == 0 {
					t.Errorf("event %+v", ev)
				}
				if raw, _ := json.Marshal(ev); strings.Contains(string(raw), "/home/tester") || strings.Contains(string(raw), "testhost") || strings.Contains(string(raw), "0c1d2e3f4a5b") {
					t.Errorf("the event echoes the private text: %s", raw)
				}
			}
			for _, sent := range w.fake.Pulls() {
				if strings.Contains(sent.Body, "/home/tester") || strings.Contains(sent.Body, "testhost") || strings.Contains(sent.Body, "019a2b3c") {
					t.Errorf("the forge got the private text: %q", sent.Body)
				}
			}
		})
	}
	// exactly one body event for one private value, not double-counted
	w := newRunWorld(t)
	w.spec.Crosscheck = []engine.Event{crosscheckRow("M1", w.tip, "verdict", "verified", "f1 see /home/tester/notes.md")}
	code, out, errOut := w.open()
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var ans Answer
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		t.Fatal(err)
	}
	pathCount := 0
	for _, ev := range ans.Redactions {
		if ev.Field != "body" {
			continue
		}
		for _, ch := range ev.Changes {
			if ch.Kind == "path" {
				pathCount += ch.Count
			}
		}
	}
	if pathCount != 1 {
		t.Errorf("path redactions %d, want exactly 1 (the rendered body only)", pathCount)
	}
}
