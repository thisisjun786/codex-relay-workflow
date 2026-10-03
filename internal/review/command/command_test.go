package command

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/agy"
)

// repo is a temporary Git repository with no configuration of its own beyond what the test sets.
type repo struct {
	t   *testing.T
	dir string
}

// clearEnv blanks the settings a developer's environment may carry, which the command reads.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"STATE_DIR", "DAILY_CAP", "MODEL", "AGY", "LOCK", "LOCK_WAIT", "TIME_LIMIT_FLOOR", "TIME_LIMIT_CEILING"} {
		t.Setenv("CRW_REVIEW_"+name, "")
	}
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	clearEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(name, "Fixture")
	}
	for _, name := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(name, "fixture@example.invalid")
	}
	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "--initial-branch=main")
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", r.dir}, args...)...).CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes files, commits them (an empty commit when there are none) and returns the commit id.
func (r *repo) commit(files map[string]string) string {
	r.t.Helper()
	for name, text := range files {
		path := filepath.Join(r.dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", "fixture")
	return r.git("rev-parse", "HEAD")
}

// change commits version n of a.go on top of base; each n is its own patch.
func (r *repo) change(base string, n int) string {
	r.git("checkout", "-q", "--detach", base)
	return r.commit(map[string]string{"a.go": fmt.Sprintf("package a\n\nfunc F() int { return %d }\n", n)})
}

// script is a pipeline.Runner that answers every call alike and records how it was called.
type script struct {
	mu           sync.Mutex
	calls        int
	active, peak int
	result       agy.Result
	hold         chan struct{} // when set, a call waits until it is closed
	entered      chan struct{} // when set, a call announces itself here
}

func (s *script) run(ctx context.Context, _ agy.Config, _ agy.Request) (agy.Result, error) {
	s.mu.Lock()
	s.calls++
	s.active++
	s.peak = max(s.peak, s.active)
	hold := s.hold
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.active--; s.mu.Unlock() }()
	if s.entered != nil {
		s.entered <- struct{}{}
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return agy.Result{}, ctx.Err()
		}
	}
	return s.result, nil
}

func (s *script) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

type fixture struct {
	t                *testing.T
	repo             *repo
	base             string
	state, out, lock string
	s                *script
	at               time.Time // the fake clock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	r := newRepo(t)
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xdg")) // no default path may reach the real state
	return &fixture{t: t, repo: r, base: r.commit(map[string]string{"a.go": "package a\n\nfunc F() int { return 1 }\n"}),
		state: filepath.Join(tmp, "state"), out: filepath.Join(tmp, "out"), lock: filepath.Join(tmp, "agy.lock"),
		s:  &script{result: agy.Result{Class: agy.ClassNormal, StructuredOutput: json.RawMessage(`{"findings":[]}`), Model: "Scripted"}},
		at: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
}

func (f *fixture) args(head string, extra ...string) []string {
	return append([]string{"--repo", f.repo.dir, "--base", f.base, "--head", head, "--issue", "CRW-506", "--out", f.out, "--state-dir", f.state, "--lock", f.lock}, extra...)
}

func (f *fixture) env() env { return env{runner: f.s.run, now: func() time.Time { return f.at }} }

// run is the command with the scripted runner; it is safe to call from a goroutine.
func (f *fixture) run(head string, extra ...string) (int, Summary, string) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), f.args(head, extra...), &out, &errOut, f.env())
	var sum Summary
	if out.Len() > 0 {
		if err := json.Unmarshal(out.Bytes(), &sum); err != nil {
			f.t.Errorf("stdout is not a summary: %q", out.String())
		}
	}
	return code, sum, errOut.String()
}

func (f *fixture) ledger() []record {
	f.t.Helper()
	recs, err := (&ledger{dir: f.state}).read()
	if err != nil {
		f.t.Fatal(err)
	}
	return recs
}

func TestSamePatchIDIsReviewedOnce(t *testing.T) {
	f := newFixture(t)
	h1 := f.repo.change(f.base, 2)
	code, first, errOut := f.run(h1)
	if code != 0 || first.Outcome != OutcomeReviewed || first.Counts == nil || f.s.count() != 2 || first.Artifact != filepath.Join(f.out, h1+".json") {
		t.Fatalf("first review: %d %+v %s (calls %d)", code, first, errOut, f.s.count())
	}
	f.repo.git("checkout", "-q", "--detach", h1)
	h2 := f.repo.commit(nil) // a new head with the same patch
	for _, head := range []string{h1, h2} {
		code, again, errOut := f.run(head)
		if code != 0 || again.Outcome != OutcomeAlreadyReviewed || again.Artifact != first.Artifact || again.ReviewedHead != h1 || again.SHA256 != first.SHA256 ||
			again.ArtifactPresent == nil || !*again.ArtifactPresent || again.Counts != nil || f.s.count() != 2 {
			t.Fatalf("repeat for %s: %d %+v %s (calls %d)", head[:7], code, again, errOut, f.s.count())
		}
	}
	// The ledger decides, not the file: with the artifact gone and another --out the answer is the same and nothing is created.
	if err := os.Remove(first.Artifact); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	code, again, errOut := f.run(h2, "--out", other)
	if _, statErr := os.Stat(other); code != 0 || again.Outcome != OutcomeAlreadyReviewed || again.Artifact != first.Artifact || again.ArtifactPresent == nil || *again.ArtifactPresent || !os.IsNotExist(statErr) || f.s.count() != 2 {
		t.Fatalf("recorded path: %d %+v %s %v", code, again, errOut, statErr)
	}
}

func TestUnavailableArtifactStillCountsAsReviewed(t *testing.T) {
	f := newFixture(t)
	f.s.result = agy.Result{Class: agy.ClassUnavailable, Reason: agy.ReasonQuota}
	h := f.repo.change(f.base, 2)
	code, first, errOut := f.run(h)
	if code != 0 || first.Status != string(review.StatusUnavailable) || first.Counts == nil || first.Reviewers.Failed != 2 {
		t.Fatalf("unavailable review: %d %+v %s", code, first, errOut)
	}
	code, again, _ := f.run(h)
	if code != 0 || again.Outcome != OutcomeAlreadyReviewed || again.Status != string(review.StatusUnavailable) || f.s.count() != 2 {
		t.Fatalf("a patch with an unavailable artifact was run again: %d %+v (calls %d)", code, again, f.s.count())
	}
	if n := runsOn(f.ledger(), "2026-10-04"); n != 1 {
		t.Fatalf("the unavailable run counts once toward the cap, got %d", n)
	}
}

func TestDailyCapStopsBeforeAnyAgyCall(t *testing.T) {
	f := newFixture(t)
	f.at = time.Date(2026, 10, 4, 23, 0, 0, 0, time.UTC)
	var heads []string
	for n := 2; n <= 4; n++ {
		heads = append(heads, f.repo.change(f.base, n))
	}
	for _, h := range heads[:2] {
		if code, sum, errOut := f.run(h, "--daily-cap", "2"); code != 0 || sum.Outcome != OutcomeReviewed {
			t.Fatalf("review under the cap: %d %+v %s", code, sum, errOut)
		}
	}
	calls := f.s.count()
	code, sum, _ := f.run(heads[2], "--daily-cap", "2")
	if code != 3 || sum.Outcome != OutcomeDailyCap || sum.DailyCap != 2 || sum.RunsToday != 2 || !strings.Contains(sum.Reason, "2026-10-04") || sum.Artifact != "" || f.s.count() != calls {
		t.Fatalf("beyond the cap: %d %+v (calls %d, were %d)", code, sum, f.s.count(), calls)
	}
	if recs := f.ledger(); recs[len(recs)-1].Event != "refused" || recs[len(recs)-1].Reason != sum.Reason || recs[len(recs)-1].Issue != "CRW-506" {
		t.Fatalf("the refusal is not recorded with its reason: %+v", recs[len(recs)-1])
	}
	f.at = f.at.Add(2 * time.Hour) // the next UTC day; the refusal was not a run
	if code, sum, errOut := f.run(heads[2], "--daily-cap", "2"); code != 0 || sum.Outcome != OutcomeReviewed {
		t.Fatalf("the next day: %d %+v %s", code, sum, errOut)
	}
}

// While one review is held inside its runner, a second invocation (another patch, then the same patch) records nothing and calls nothing.
func TestConcurrentReviewsAreSerialAndTheLedgerStaysConsistent(t *testing.T) {
	f := newFixture(t)
	f.s.entered = make(chan struct{}, 32)
	h2, h3, h4 := f.repo.change(f.base, 2), f.repo.change(f.base, 3), f.repo.change(f.base, 4)
	for _, pair := range [][2]string{{h2, h3}, {h4, h4}} {
		f.s.mu.Lock()
		f.s.hold = make(chan struct{})
		f.s.mu.Unlock()
		prior, outcomes := len(f.ledger()), make(chan string, 2)
		for i, head := range pair {
			go func() { _, sum, _ := f.run(head); outcomes <- sum.Outcome }()
			if i == 0 {
				select { // the first review is inside its runner and holds the run lock
				case <-f.s.entered:
				case <-time.After(10 * time.Second):
					t.Fatal("the first review never reached its runner")
				}
			}
		}
		time.Sleep(300 * time.Millisecond) // long enough for the second to build its bundle and meet the lock
		if recs := f.ledger(); len(recs) != prior+1 || recs[prior].Event != "started" || f.s.count()%2 != 1 {
			t.Fatalf("the second review got past the lock: %d records, %d calls", len(recs), f.s.count())
		}
		close(f.s.hold)
		got, want := []string{<-outcomes, <-outcomes}, []string{OutcomeReviewed, OutcomeReviewed}
		if pair[0] == pair[1] {
			want = []string{OutcomeAlreadyReviewed, OutcomeReviewed}
		}
		if slices.Sort(got); !slices.Equal(got, want) {
			t.Fatalf("outcomes for %v: %v, want %v", pair, got, want)
		}
	}
	var events []string
	for _, r := range f.ledger() {
		events = append(events, r.Event)
	}
	if strings.Join(events, ",") != "started,finished,started,finished,started,finished" || f.s.peak != 1 || f.s.count() != 6 {
		t.Fatalf("ledger %v, at most %d calls at once, %d calls", events, f.s.peak, f.s.count())
	}
}

func TestBusyWhenTheRunLockIsHeld(t *testing.T) {
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	unlock, err := (&ledger{dir: f.state}).lock(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	code, sum, errOut := f.run(h, "--lock-wait", "-1s")
	if _, statErr := os.Stat(filepath.Join(f.state, "ledger.jsonl")); code != 3 || sum.Outcome != "" || !strings.Contains(errOut, "busy") || f.s.count() != 0 || !os.IsNotExist(statErr) {
		t.Fatalf("busy: %d %+v %s (calls %d, ledger %v)", code, sum, errOut, f.s.count(), statErr)
	}
}

func TestEmptyDiffIsRefusedBeforeAnyRecord(t *testing.T) {
	f := newFixture(t)
	code, _, errOut := f.run(f.base)
	if _, statErr := os.Stat(f.state); code != 1 || !strings.Contains(errOut, "nothing to review") || f.s.count() != 0 || !os.IsNotExist(statErr) {
		t.Fatalf("empty diff: %d %s (calls %d, state %v)", code, errOut, f.s.count(), statErr)
	}
}

func TestInterruptRecordsAFailedReviewAndExits130(t *testing.T) {
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	ctx, cancel := context.WithCancel(context.Background())
	e := f.env()
	e.runner = func(ctx context.Context, _ agy.Config, _ agy.Request) (agy.Result, error) {
		cancel()
		return agy.Result{}, ctx.Err()
	}
	var out, errOut bytes.Buffer
	if code := run(ctx, f.args(h), &out, &errOut, e); code != 130 || out.Len() != 0 {
		t.Fatalf("interrupted review: %d %q %q", code, out.String(), errOut.String())
	}
	recs := f.ledger()
	if len(recs) != 2 || recs[0].Event != "started" || recs[1].Event != "failed" || recs[1].Reason != "interrupted" {
		t.Fatalf("ledger after the interrupt: %+v", recs)
	}
	if code, sum, _ := f.run(h); code != 0 || sum.Outcome != OutcomeReviewed { // an interrupted review produced nothing, so the patch is still open
		t.Fatalf("after the interrupt: %d %+v", code, sum)
	}
}

// A publish failure leaves the review recorded: its slot is spent and the patch is never run again, whichever file could not be written.
func TestPublishFailureNeverFreesThePatchID(t *testing.T) {
	for _, suffix := range []string{".json", ".json.sha256"} {
		f := newFixture(t)
		h := f.repo.change(f.base, 2)
		if err := os.MkdirAll(filepath.Join(f.out, h+suffix), 0o755); err != nil { // a directory where a file belongs: Publish refuses it
			t.Fatal(err)
		}
		code, _, errOut := f.run(h)
		if code != 1 || !strings.Contains(errOut, h+suffix) || f.s.count() != 2 {
			t.Fatalf("%s: %d %s (calls %d)", suffix, code, errOut, f.s.count())
		}
		if code, again, _ := f.run(h); code != 0 || again.Outcome != OutcomeAlreadyReviewed || f.s.count() != 2 {
			t.Fatalf("%s: run again: %d %+v (calls %d)", suffix, code, again, f.s.count())
		}
	}
}

func TestUsageErrorsAndHelp(t *testing.T) {
	for _, c := range []struct {
		args []string
		env  string
		want string
	}{
		{nil, "", "the following arguments are required: --base, --head, --issue, --out"},
		{[]string{"--base", "a", "--head", "b", "--issue", "crw 1", "--out", "o"}, "", "is not an issue id"},
		{[]string{"--base", "a", "--head", "b", "--issue", "CRW-1", "--out", "o", "--daily-cap", "0"}, "", "the daily cap must be at least 1"},
		{[]string{"--base", "a", "--head", "b", "--issue", "CRW-1", "--out", "o", "extra"}, "", `unrecognized argument "extra"`},
		{[]string{"--base", "a", "--head", "b", "--issue", "CRW-1", "--out", "o"}, "soon", "CRW_REVIEW_DAILY_CAP"},
		{[]string{"--nope"}, "", "flag provided but not defined: -nope"},
	} {
		clearEnv(t)
		t.Setenv("CRW_REVIEW_DAILY_CAP", c.env)
		var out, errOut bytes.Buffer
		if _, code := parseConfig(c.args, &out, &errOut); code != 2 || out.Len() != 0 || !strings.HasPrefix(errOut.String(), "usage: crw review --base") || !strings.Contains(errOut.String(), "crw review: error: ") || !strings.Contains(errOut.String(), c.want) {
			t.Errorf("%v: %d %q %q, want %q", c.args, code, out.String(), errOut.String(), c.want)
		}
	}
	clearEnv(t)
	var out, errOut bytes.Buffer
	if _, code := parseConfig([]string{"-h"}, &out, &errOut); code != 0 || errOut.Len() != 0 || !strings.HasPrefix(out.String(), "usage: crw review") || !strings.Contains(out.String(), "--daily-cap") {
		t.Errorf("-h: %d %q %q", code, out.String(), errOut.String())
	}
}

func TestConfigFlagBeatsEnvironmentBeatsDefault(t *testing.T) {
	args := []string{"--base", "a", "--head", "b", "--issue", "CRW-1", "--out", "o"}
	parse := func(extra ...string) Config {
		var out, errOut bytes.Buffer
		c, code := parseConfig(append(args, extra...), &out, &errOut)
		if code != -1 {
			t.Fatalf("%v: %d %s", extra, code, errOut.String())
		}
		return c
	}
	clearEnv(t)
	t.Setenv("XDG_STATE_HOME", "/xdg")
	if c := parse(); c.DailyCap != 20 || c.Model != agy.DefaultModel || c.StateDir != "/xdg/crw/review" || c.LockPath != "/xdg/crw/review/agy.lock" || c.LockWait != 30*time.Minute || c.Binary != "agy" || !filepath.IsAbs(c.Out) {
		t.Errorf("defaults: %+v", c)
	}
	t.Setenv("CRW_REVIEW_DAILY_CAP", "7")
	t.Setenv("CRW_REVIEW_MODEL", "from-env")
	t.Setenv("CRW_REVIEW_LOCK_WAIT", "90s")
	if c := parse(); c.DailyCap != 7 || c.Model != "from-env" || c.LockWait != 90*time.Second {
		t.Errorf("environment: %+v", c)
	}
	if c := parse("--daily-cap", "3", "--model", "from-flag"); c.DailyCap != 3 || c.Model != "from-flag" || c.LockWait != 90*time.Second {
		t.Errorf("flags: %+v", c)
	}
}

// The agy that answers is a shell script run through the real agy.Run, so the lock, the arguments, the prompt length check and the log parsing are all in play.
const fakeAgy = `#!/bin/sh
rec=%s
while [ $# -gt 0 ]; do case $1 in --log-file) log=$2;; --model) model=$2;; esac; shift; done
cat > "$rec.in"
cat "$rec.in" >> "$rec.prompts"
echo "$model" >> "$rec.models"
n=$(wc -c < "$rec.in" | tr -d ' ')
printf 'Print mode: starting (promptLength=%%s, model="%%s", conversationID="")\nPropagating selected model override to backend: label="Fake Model"\n' "$n" "$model" > "$log"
case $(head -n 1 "$rec.in") in
review) out='{"findings":[{"file":"a.go","line":3,"endLine":3,"title":"t","explanation":"e","severity":"P2","needsContext":false}]}';;
group) out='{"groups":[[0,1]]}';;
verify) out='{"verdict":"confirmed","needsContext":false}';;
esac
printf '{"status":"SUCCESS","response":"ok","usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10},"structured_output":%%s}\n' "$out"
`

func TestEndToEndWithFakeAgy(t *testing.T) {
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	dir := t.TempDir()
	rec, bin := filepath.Join(dir, "rec"), filepath.Join(dir, "agy")
	if err := os.WriteFile(bin, []byte(fmt.Sprintf(fakeAgy, rec)), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run(context.Background(), f.args(h, "--agy", bin, "--lock-wait", "30s"), &out, &errOut)
	var sum Summary
	if err := json.Unmarshal(out.Bytes(), &sum); err != nil || code != 0 {
		t.Fatalf("crw review: %d %q %q", code, out.String(), errOut.String())
	}
	data, err := os.ReadFile(filepath.Join(f.out, h+".json"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := review.ParseArtifact(data, h)
	if err != nil {
		t.Fatalf("the artifact does not validate: %v", err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	sha, _ := os.ReadFile(filepath.Join(f.out, h+".json.sha256"))
	if string(sha) != digest+"  "+h+".json\n" || sum.SHA256 != digest || sum.Artifact != filepath.Join(f.out, h+".json") || sum.Head != h || sum.PatchID != a.PatchID || sum.Issue != "CRW-506" {
		t.Fatalf("sha256 file %q, summary %+v", sha, sum)
	}
	if a.Status != review.StatusComplete || sum.Status != "complete" || len(a.Findings) != 1 || a.Findings[0].Support != 2 || a.Findings[0].Verdict != review.VerdictConfirmed || a.Model != agy.DefaultModel || len(a.Calls) != 4 ||
		a.Calls[0].Model != "Fake Model" || a.Calls[0].Tokens.Input != 7 || sum.Findings != 1 || sum.Calls != 4 || sum.Reviewers.Run != 2 {
		t.Fatalf("artifact %+v summary %+v", a, sum)
	}
	models, _ := os.ReadFile(rec + ".models")
	prompts, _ := os.ReadFile(rec + ".prompts")
	if strings.Count(string(models), agy.DefaultModel+"\n") != 4 || len(prompts) == 0 || strings.Contains(string(prompts), "CRW-506") {
		t.Fatalf("models %q; the issue id must never reach a prompt", models)
	}
	if entries, _ := os.ReadDir(f.out); len(entries) != 2 {
		t.Fatalf("the output directory holds %d entries, want the artifact and its sha256 only", len(entries))
	}
	recs := f.ledger() // the real clock here
	if len(recs) != 2 || recs[1].Event != "finished" || recs[1].SHA256 != digest || recs[1].Status != "complete" || recs[1].Artifact != sum.Artifact {
		t.Fatalf("ledger: %+v", recs)
	}
}
