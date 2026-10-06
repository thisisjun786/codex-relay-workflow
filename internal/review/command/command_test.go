package command

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/agy"
)

type repo struct {
	t   *testing.T
	dir string
}

// clearEnv blanks the settings a developer's environment may carry, which the command reads.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"STATE_DIR", "DAILY_CAP", "MODEL", "AGY", "GH", "LOCK", "LOCK_WAIT", "TIME_LIMIT_FLOOR", "TIME_LIMIT_CEILING"} {
		t.Setenv("CRW_REVIEW_"+name, "")
	}
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	clearEnv(t)
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_AUTHOR_NAME": "Fixture", "GIT_COMMITTER_NAME": "Fixture",
		"GIT_AUTHOR_EMAIL": "fixture@example.invalid", "GIT_COMMITTER_EMAIL": "fixture@example.invalid"} {
		t.Setenv(k, v)
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
		if err := errors.Join(os.MkdirAll(filepath.Dir(path), 0o755), os.WriteFile(path, []byte(text), 0o644)); err != nil {
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
	hold         chan struct{}                  // when set, a call waits until it is closed
	entered      chan struct{}                  // when set, a call announces itself here
	answer       func(prompt []byte) agy.Result // when set, every call is answered from the prompt (stageAnswers) instead of result
	err          error                          // when set, every call fails at the runner, as a runner-side failure does
}

func (s *script) run(ctx context.Context, _ agy.Config, req agy.Request) (agy.Result, error) {
	s.mu.Lock()
	s.calls++
	s.active++
	s.peak = max(s.peak, s.active)
	hold, entered, runErr := s.hold, s.entered, s.err
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.active--; s.mu.Unlock() }()
	if entered != nil {
		entered <- struct{}{}
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return agy.Result{}, ctx.Err()
		}
	}
	if s.answer != nil {
		return s.answer(req.Prompt), nil
	}
	if runErr != nil {
		return agy.Result{}, runErr
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
	forge            forge     // when set, --post-summary talks to it instead of the gh CLI
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
	return append([]string{"--repo", f.repo.dir, "--base", f.base, "--head", head, "--issue", "CRW-506", "--out", f.out, "--state-dir", f.state, "--lock", f.lock, "--agy", "/nonexistent/agy"}, extra...)
}

// run is the command with the scripted runner; it is safe to call from a goroutine.
func (f *fixture) run(head string, extra ...string) (int, Summary, string) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), f.args(head, extra...), &out, &errOut, env{runner: f.s.run, now: func() time.Time { return f.at }, forge: func(Config) forge { return f.forge }})
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
	if err := os.Remove(first.Artifact); err != nil || os.Mkdir(first.Artifact, 0o755) != nil { // gone as a file; a directory in its place is no artifact either
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	code, again, errOut := f.run(h2, "--out", other)
	if _, statErr := os.Stat(other); code != 0 || again.Outcome != OutcomeAlreadyReviewed || again.Artifact != first.Artifact || again.ArtifactPresent == nil || *again.ArtifactPresent || !os.IsNotExist(statErr) || f.s.count() != 2 {
		t.Fatalf("recorded path: %d %+v %s %v", code, again, errOut, statErr)
	}
}

// An unavailable artifact whose reason is neither a problem of the account or the configuration nor a runner-side failure is a result: it closes the patch (TestRetryRule has the rest).
func TestUnavailableArtifactOfAnotherReasonStillCountsAsReviewed(t *testing.T) {
	f := newFixture(t)
	f.s.result = agy.Result{Class: agy.ClassUnavailable, Reason: agy.ReasonContentFilter}
	h := f.repo.change(f.base, 2)
	if code, first, errOut := f.run(h); code != 0 || first.Status != string(review.StatusUnavailable) {
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

// A runner-side failure the pipeline reports as runner_error is retryable exactly like a crash, and the ledger records that the case is a runner error and that the agy call state is unknown.
func TestRunnerErrorIsRetryableAndRecorded(t *testing.T) {
	f := newFixture(t)
	head := f.repo.change(f.base, 2)
	f.on("2026-10-04", okResult)
	f.s.err = errors.New("host failure")
	if _, sum, _ := f.run(head); sum.Outcome != OutcomeReviewed || sum.Status != string(review.StatusUnavailable) || sum.RetryNotBefore != "2026-10-05" {
		t.Fatalf("a runner error must leave the patch open for one more attempt: %+v", sum)
	}
	recs := f.ledger()
	if r := recs[len(recs)-1]; r.Event != "unavailable" || r.Reason != reasonRunnerError || r.AgyCalled != nil {
		t.Fatalf("the runner error record must name the case and leave the agy call unknown: %+v", r)
	}
	f.s.err = nil
	f.on("2026-10-05", okResult)
	if code, sum, errOut := f.run(head); code != 0 || sum.Outcome != OutcomeReviewed || sum.Status != "complete" {
		t.Fatalf("the retry after a runner error: %d %+v %s", code, sum, errOut)
	}
}

// The failure record says which case it was and whether agy was called when that can be told.
func TestTheLedgerRecordsTheCaseAndWhetherAgyWasCalled(t *testing.T) {
	f := newFixture(t)
	head := f.repo.change(f.base, 2)
	f.on("2026-10-04", quotaResult)
	if _, sum, _ := f.run(head); sum.Outcome != OutcomeReviewed {
		t.Fatalf("quota: %+v", sum)
	}
	if recs := f.ledger(); recs[len(recs)-1].Event != "unavailable" || recs[len(recs)-1].Reason != "quota" || recs[len(recs)-1].AgyCalled == nil || !*recs[len(recs)-1].AgyCalled {
		t.Fatalf("a quota run must record the case and that agy was called: %+v", recs[len(recs)-1])
	}
	g := newFixture(t)
	ghead := g.repo.change(g.base, 2)
	g.on("2026-10-04", lockWaitResult)
	if code, sum, _ := g.run(ghead); code != 3 || sum.Outcome != OutcomeLockWaitExpired {
		t.Fatalf("lock wait: %d %+v", code, sum)
	}
	if recs := g.ledger(); recs[len(recs)-1].Event != "lock_wait" || recs[len(recs)-1].Reason != "lock_wait_expired" || recs[len(recs)-1].AgyCalled == nil || *recs[len(recs)-1].AgyCalled {
		t.Fatalf("a lock wait must record the case and that agy was not called: %+v", recs[len(recs)-1])
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
	h2, h3, h4 := f.repo.change(f.base, 2), f.repo.change(f.base, 3), f.repo.change(f.base, 4)
	for _, pair := range [][2]string{{h2, h3}, {h4, h4}} {
		hold, entered := make(chan struct{}), make(chan struct{}, 8)
		var once sync.Once
		release := func() { once.Do(func() { close(hold) }) }
		t.Cleanup(release) // a failed check must not leave a review parked in its runner: it would hold the pipeline's in-process guard for every later test
		f.s.mu.Lock()
		f.s.hold, f.s.entered = hold, entered
		f.s.mu.Unlock()
		prior, outcomes := len(f.ledger()), make(chan string, 2)
		for i, head := range pair {
			go func() { _, sum, _ := f.run(head); outcomes <- sum.Outcome }()
			if i == 0 {
				select { // the first review is inside its runner and holds the run lock
				case <-entered:
				case <-time.After(10 * time.Second):
					t.Fatal("the first review never reached its runner")
				}
			}
		}
		time.Sleep(300 * time.Millisecond) // long enough for the second to build its bundle and meet the lock
		if recs := f.ledger(); len(recs) != prior+1 || recs[prior].Event != "started" || f.s.count()%2 != 1 {
			t.Fatalf("the second review got past the lock: %d records, %d calls", len(recs), f.s.count())
		}
		release()
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

// Neither a busy run lock nor an empty diff reaches agy or writes a ledger record.
func TestBusyAndEmptyDiffRecordNothing(t *testing.T) {
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	unlock, err := (&ledger{dir: f.state}).lock(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for head, c := range map[string]struct {
		code int
		want string
		args []string
	}{h: {3, "busy", []string{"--lock-wait", "-1s"}}, f.base: {1, "nothing to review", nil}} {
		code, sum, errOut := f.run(head, c.args...)
		if _, statErr := os.Stat(filepath.Join(f.state, "ledger.jsonl")); code != c.code || sum.Outcome != "" || !strings.Contains(errOut, c.want) || f.s.count() != 0 || !os.IsNotExist(statErr) {
			t.Fatalf("%s: %d %+v %s (calls %d, ledger %v)", c.want, code, sum, errOut, f.s.count(), statErr)
		}
	}
}

func TestInterruptRecordsAFailedReviewAndExits130(t *testing.T) {
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	ctx, cancel := context.WithCancel(context.Background())
	e := env{now: func() time.Time { return f.at }}
	e.runner = func(ctx context.Context, _ agy.Config, _ agy.Request) (agy.Result, error) {
		cancel()
		return agy.Result{}, ctx.Err()
	}
	var out, errOut bytes.Buffer
	if code := run(ctx, f.args(h), &out, &errOut, e); code != 130 || out.Len() != 0 {
		t.Fatalf("interrupted review: %d %q %q", code, out.String(), errOut.String())
	}
	recs := f.ledger()
	if len(recs) != 2 || recs[0].Event != "started" || recs[1].Event != "failed" || recs[1].Reason != "interrupted" || runsOn(recs, "2026-10-04") != 1 { // a review that began counts toward the cap
		t.Fatalf("ledger after the interrupt: %+v", recs)
	}
	if code, sum, _ := f.run(h); code != 0 || sum.Outcome != OutcomeReviewed { // an interrupted review produced nothing, so the patch is still open
		t.Fatalf("after the interrupt: %d %+v", code, sum)
	}
}

// A publish failure leaves the review recorded: its slot is spent and the patch is never run again. The result is kept with the record, so
// once the fault is gone the same command writes the missing files from it, with no model call and no new ledger record.
func TestPublishFailureIsRepairedFromTheKeptResult(t *testing.T) {
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	artifact, checksum := filepath.Join(f.out, h+".json"), filepath.Join(f.out, h+".json.sha256")
	if err := os.MkdirAll(checksum, 0o755); err != nil { // a directory where a file belongs: Publish refuses it
		t.Fatal(err)
	}
	code, _, errOut := f.run(h)
	if code != 1 || !strings.Contains(errOut, h+".json.sha256") || f.s.count() != 2 {
		t.Fatalf("publish failure: %d %s (calls %d)", code, errOut, f.s.count())
	}
	records := len(f.ledger())
	if code, _, errOut := f.run(h); code != 1 || !strings.Contains(errOut, h+".json.sha256") || f.s.count() != 2 || len(f.ledger()) != records { // the fault stays: the same failure, no model call
		t.Fatalf("run again with the fault in place: %d %s (calls %d, records %d, were %d)", code, errOut, f.s.count(), len(f.ledger()), records)
	}
	if err := errors.Join(os.Remove(checksum), os.Remove(artifact)); err != nil { // the fault is gone and the artifact file is lost with it
		t.Fatal(err)
	}
	code, again, errOut := f.run(h)
	if code != 0 || again.Outcome != OutcomeAlreadyReviewed || again.ArtifactPresent == nil || !*again.ArtifactPresent || !slices.Equal(again.Restored, []string{artifact, checksum}) ||
		f.s.count() != 2 || len(f.ledger()) != records {
		t.Fatalf("repair: %d %+v %s (calls %d, records %d, were %d)", code, again, errOut, f.s.count(), len(f.ledger()), records)
	}
	data, _ := os.ReadFile(artifact)
	sha, _ := os.ReadFile(checksum)
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if recs := f.ledger(); recs[len(recs)-1].Event != "finished" || recs[len(recs)-1].SHA256 != digest || string(sha) != digest+"  "+h+".json\n" {
		t.Fatalf("the restored files do not carry the recorded sha256: %s %q (ledger %+v)", digest, sha, f.ledger()[records-1])
	}
	if _, third, _ := f.run(h); len(third.Restored) != 0 || f.s.count() != 2 {
		t.Fatalf("a repaired result is restored again: %+v", third)
	}
}

// stageAnswers makes the scripted runner answer by the stage the prompt names (its first line, as testdata/fake-agy.sh reads it): every reviewer reports one finding at file:line,
// which the group and verify stages confirm.
func stageAnswers(file string, line int) func([]byte) agy.Result {
	return func(prompt []byte) agy.Result {
		stage, _, _ := bytes.Cut(prompt, []byte("\n"))
		out := map[string]string{
			"review": fmt.Sprintf(`{"findings":[{"file":%q,"line":%d,"endLine":%d,"title":"t","explanation":"e","severity":"P2","needsContext":false}]}`, file, line, line),
			"group":  `{"groups":[[0,1]]}`,
			"verify": `{"verdict":"confirmed","needsContext":false}`,
		}[string(stage)]
		return agy.Result{Class: agy.ClassNormal, StructuredOutput: json.RawMessage(out), Model: "Scripted"}
	}
}

// Finding paths are relative to the repository root (the bundle reads a bare object view), so a run that names a subdirectory with --repo, as the default of the current directory does,
// must check them against the root: a root file is found and a file of the same name in the subdirectory is never read in its place.
func TestRunFromASubdirectoryChecksFindingsAgainstTheRoot(t *testing.T) {
	f := newFixture(t)
	f.repo.git("checkout", "-q", "--detach", f.base)
	withSameName := f.repo.commit(map[string]string{"a.go": "package a\n\nfunc F() int { return 2 }\n", "sub/a.go": "package sub\n"}) // the root a.go has 3 lines, sub/a.go has 1
	f.repo.git("checkout", "-q", "--detach", f.base)
	rootOnly := f.repo.commit(map[string]string{"b.go": "package a\n\nvar B = 1\n", "sub/c.go": "package sub\n"}) // sub has no b.go
	sub := filepath.Join(f.repo.dir, "sub")
	for _, c := range []struct {
		head, file string
		line       int
	}{{withSameName, "a.go", 3}, {rootOnly, "b.go", 3}} {
		f.s.answer = stageAnswers(c.file, c.line)
		code, sum, errOut := f.run(c.head, "--repo", sub)
		if code != 0 || sum.Counts == nil || sum.Findings != 1 || sum.Dropped != 0 {
			t.Errorf("finding at %s:%d from a subdirectory: %d %+v %s", c.file, c.line, code, sum, errOut)
		}
	}
}

// The artifact's file name is the head's, so the same head reviewed against another base must not replace the first review.
func TestAnotherBaseForTheSameHeadDoesNotOverwriteAnArtifact(t *testing.T) {
	f := newFixture(t)
	h2 := f.repo.change(f.base, 2)
	f.repo.git("checkout", "-q", "--detach", h2)
	h3 := f.repo.commit(map[string]string{"a.go": "package a\n\nfunc F() int { return 3 }\n"})
	code, first, errOut := f.run(h3)
	if code != 0 {
		t.Fatalf("first review: %d %s", code, errOut)
	}
	before, _ := os.ReadFile(first.Artifact)
	f.base = h2 // another patch, the same head
	code, _, errOut = f.run(h3)
	after, _ := os.ReadFile(first.Artifact)
	if code != 1 || !strings.Contains(errOut, "use another --out") || f.s.count() != 2 || !bytes.Equal(before, after) {
		t.Fatalf("second base: %d %s (calls %d)", code, errOut, f.s.count())
	}
}

// SIGTERM and SIGHUP cancel a review that waits for the run lock, as SIGINT does, instead of ending the process around agy and its locks.
func TestTerminationSignalsCancelAReview(t *testing.T) {
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	unlock, err := (&ledger{dir: f.state}).lock(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	// The signals go to this process: a Notify that is never stopped keeps any of them, however late, from ending the test binary, and reports delivery, so at most one is in flight.
	sink := make(chan os.Signal, 16)
	signal.Notify(sink, syscall.SIGTERM, syscall.SIGHUP)
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP} {
		done, code := make(chan int, 1), -1
		go func() { done <- Run(context.Background(), f.args(h, "--lock-wait", "1m"), io.Discard, io.Discard) }()
		for deadline := time.Now().Add(10 * time.Second); code < 0 && time.Now().Before(deadline); {
			select {
			case code = <-done:
			case <-time.After(200 * time.Millisecond): // a signal sent before Run has installed its handler changes nothing, so it is repeated
				_ = syscall.Kill(os.Getpid(), sig)
				<-sink
			}
		}
		if code != 130 {
			t.Fatalf("%v: exit %d, want 130 within 10 s", sig, code)
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
		{[]string{"--base", "a", "--head", "b", "--issue", "CRW-1", "--out", "o", "--post-summary"}, "", "--post-summary needs --pr"},
		{[]string{"--base", "a", "--head", "b", "--issue", "CRW-1", "--out", "o", "--post-only"}, "", "--post-only needs --pr"},
		{[]string{"--base", "a", "--head", "b", "--issue", "CRW-1", "--out", "o", "--pr", "7"}, "", "--pr is only used with --post-summary or --post-only"},
		{[]string{"--base", "a", "--head", "b", "--issue", "CRW-1", "--out", "o"}, "soon", "CRW_REVIEW_DAILY_CAP"},
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
	if c := parse(); c.DailyCap != 20 || c.Model != agy.DefaultModel || c.StateDir != "/xdg/crw/review" || c.LockWait != 30*time.Minute || c.Gh != "gh" {
		t.Errorf("defaults: %+v", c)
	}
	t.Setenv("CRW_REVIEW_DAILY_CAP", "7")
	t.Setenv("CRW_REVIEW_MODEL", "from-env")
	t.Setenv("CRW_REVIEW_LOCK_WAIT", "90s")
	t.Setenv("CRW_REVIEW_GH", "/env/gh")
	if c := parse(); c.DailyCap != 7 || c.Model != "from-env" || c.LockWait != 90*time.Second || c.Gh != "/env/gh" {
		t.Errorf("environment: %+v", c)
	}
	if c := parse("--daily-cap", "3", "--model", "from-flag"); c.DailyCap != 3 || c.Model != "from-flag" || c.LockWait != 90*time.Second {
		t.Errorf("flags: %+v", c)
	}
}

// The agy that answers is testdata/fake-agy.sh, run through the real agy.Run: the lock, the arguments, the prompt length check and the log parsing are all in play.
func TestEndToEndWithFakeAgy(t *testing.T) {
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	script, err := os.ReadFile(filepath.Join("testdata", "fake-agy.sh"))
	bin := filepath.Join(t.TempDir(), "agy") // the fake records beside itself
	if err != nil || os.WriteFile(bin, script, 0o755) != nil {
		t.Fatal("cannot install the fake agy", err)
	}
	rec := filepath.Join(filepath.Dir(bin), "rec")
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
	if a.Status != review.StatusComplete || len(a.Findings) != 1 || a.Findings[0].Support != 2 || a.Findings[0].Verdict != review.VerdictConfirmed || a.Model != agy.DefaultModel || len(a.Calls) != 4 ||
		a.Calls[0].Model != "Fake Model" || a.Calls[0].Tokens.Input != 7 || sum.Findings != 1 || sum.Calls != 4 {
		t.Fatalf("artifact %+v summary %+v", a, sum)
	}
	models, _ := os.ReadFile(rec + ".models")
	prompts, _ := os.ReadFile(rec + ".prompts")
	if strings.Count(string(models), agy.DefaultModel+"\n") != 4 || len(prompts) == 0 || strings.Contains(string(prompts), "CRW-506") {
		t.Fatalf("models %q; the issue id must never reach a prompt", models)
	}
}
