package command

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/agy"
)

var (
	okResult        = agy.Result{Class: agy.ClassNormal, StructuredOutput: json.RawMessage(`{"findings":[]}`), Model: "Scripted"}
	quotaResult     = agy.Result{Class: agy.ClassUnavailable, Reason: agy.ReasonQuota}
	authResult      = agy.Result{Class: agy.ClassUnavailable, Reason: agy.ReasonAuthentication}
	modelResult     = agy.Result{Class: agy.ClassUnavailable, Reason: agy.ReasonUnknownModel}
	noStartResult   = agy.Result{Class: agy.ClassUnavailable, Reason: agy.ReasonNotStarted}
	filterResult    = agy.Result{Class: agy.ClassUnavailable, Reason: agy.ReasonContentFilter}
	deniedResult    = agy.Result{Class: agy.ClassInvalid, Reason: agy.ReasonDeniedActions}
	limitResult     = agy.Result{Class: agy.ClassInvalid, Reason: agy.ReasonTimeLimit}
	crashResult     = agy.Result{Class: agy.ClassUnavailable, Reason: agy.ReasonCrash}
	runnerErrResult = agy.Result{Class: agy.ClassUnavailable, Reason: agy.Reason(reasonRunnerError)}
	lockWaitResult  = agy.Result{Class: agy.ClassUnavailable, Reason: agy.ReasonLockWaitExpired}
)

// on sets the fake clock to noon UTC of the day and the answer of every agy call.
func (f *fixture) on(day string, result agy.Result) {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		f.t.Fatal(err)
	}
	f.at = t.Add(12 * time.Hour)
	f.s.mu.Lock()
	defer f.s.mu.Unlock()
	f.s.result = result
}

// A run that could not review at all because of the account or the configuration (or because the runner failed: a crash or a runner error) does not spend the patch: the same patch may be
// tried once more on a later UTC day, never on the same one, and a second attempt of that kind closes it. A lock wait (agy was never started) counts toward neither the patch nor the daily
// cap and may be repeated the same day. Results that repeat for the same input close the patch at once. Every attempt that starts counts toward the daily cap, except a lock wait.
func TestRetryRule(t *testing.T) {
	const d1, d2, d3 = "2026-10-04", "2026-10-05", "2026-10-06"
	type step struct {
		day, outcome string
		result       agy.Result
		calls        int    // agy calls this step makes: two reviewers, so 2 when the review runs
		event        string // the last ledger event after the step
		status       string // of the artifact the answer names
		notBefore    string // the retryNotBefore of the answer
	}
	account := func(r agy.Result) []step {
		return []step{
			{d1, OutcomeReviewed, r, 2, "unavailable", "unavailable", d2},
			{d1, OutcomeRetryDeferred, okResult, 0, "refused", "unavailable", d2}, // the same window: never
			{d2, OutcomeReviewed, r, 2, "finished", "unavailable", ""},            // a second unavailable closes the patch
			{d3, OutcomeAlreadyReviewed, okResult, 0, "finished", "unavailable", ""},
		}
	}
	closes := func(r agy.Result) []step {
		return []step{
			{d1, OutcomeReviewed, r, 2, "finished", "unavailable", ""},
			{d2, OutcomeAlreadyReviewed, okResult, 0, "finished", "unavailable", ""},
		}
	}
	for _, c := range []struct {
		name  string
		steps []step
	}{
		{"quota", account(quotaResult)},
		{"authentication", account(authResult)},
		{"unknown model", account(modelResult)},
		{"agy not started", account(noStartResult)},
		{"a retry that reviews", []step{
			{d1, OutcomeReviewed, authResult, 2, "unavailable", "unavailable", d2},
			{d2, OutcomeReviewed, okResult, 2, "finished", "complete", ""},
			{d3, OutcomeAlreadyReviewed, quotaResult, 0, "finished", "complete", ""},
		}},
		{"crash", account(crashResult)},
		{"runner error", account(runnerErrResult)},
		{"lock wait expired", []step{
			{d1, OutcomeLockWaitExpired, lockWaitResult, 2, "lock_wait", "", ""},
			{d1, OutcomeLockWaitExpired, lockWaitResult, 2, "lock_wait", "", ""}, // the same day again: it counts toward nothing
			{d1, OutcomeReviewed, okResult, 2, "finished", "complete", ""},       // and the patch is still there to review
			{d2, OutcomeAlreadyReviewed, okResult, 0, "finished", "complete", ""},
		}},
		{"content filter", closes(filterResult)},
		{"denied actions", closes(deniedResult)},
		{"time limit", closes(limitResult)},
		{"the clock goes back", []step{
			{d2, OutcomeReviewed, quotaResult, 2, "unavailable", "unavailable", d3},
			{d1, OutcomeRetryDeferred, okResult, 0, "refused", "unavailable", d3},
			{d3, OutcomeReviewed, okResult, 2, "finished", "complete", ""},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			head := f.repo.change(f.base, 2)
			var first Summary
			started := map[string]int{}
			for i, s := range c.steps {
				f.on(s.day, s.result)
				before := f.s.count()
				code, sum, errOut := f.run(head)
				wantCode := 0
				if s.outcome == OutcomeRetryDeferred || s.outcome == OutcomeLockWaitExpired {
					wantCode = 3
				}
				recs := f.ledger()
				if code != wantCode || sum.Outcome != s.outcome || f.s.count()-before != s.calls || recs[len(recs)-1].Event != s.event || sum.Status != s.status || sum.RetryNotBefore != s.notBefore {
					t.Fatalf("step %d (%s): exit %d outcome %q calls %d event %q status %q notBefore %q; want %d %q %d %q %q %q: %s",
						i, s.day, code, sum.Outcome, f.s.count()-before, recs[len(recs)-1].Event, sum.Status, sum.RetryNotBefore, wantCode, s.outcome, s.calls, s.event, s.status, s.notBefore, errOut)
				}
				if s.calls > 0 && s.outcome != OutcomeLockWaitExpired { // a lock wait is not an attempt
					started[s.day]++
				}
				if i == 0 {
					first = sum
				} else if s.outcome == OutcomeRetryDeferred && (sum.Artifact != first.Artifact || sum.SHA256 != first.SHA256 || !strings.Contains(sum.Reason, s.notBefore)) {
					t.Fatalf("step %d: the deferral names %q (%s), want the first attempt's artifact and the day it may retry: %q", i, sum.Artifact, sum.Reason, first.Artifact)
				}
			}
			for day, n := range started { // every attempt counts toward the cap, whatever its result
				if got := runsOn(f.ledger(), day); got != n {
					t.Errorf("%s: %d attempts started, the ledger counts %d toward the cap", day, n, got)
				}
			}
		})
	}
}

// The deferral comes before the cap; a retry day whose cap is spent refuses the retry without using it up.
func TestRetryDeferralAndTheDailyCap(t *testing.T) {
	f := newFixture(t)
	a, b := f.repo.change(f.base, 2), f.repo.change(f.base, 3)
	f.on("2026-10-04", quotaResult)
	if _, sum, _ := f.run(a, "--daily-cap", "1"); sum.Outcome != OutcomeReviewed || sum.RetryNotBefore != "2026-10-05" {
		t.Fatalf("first attempt: %+v", sum)
	}
	if code, sum, _ := f.run(a, "--daily-cap", "1"); code != 3 || sum.Outcome != OutcomeRetryDeferred { // the cap is spent too: the window is the reason
		t.Fatalf("same day, cap spent: %d %+v", code, sum)
	}
	f.on("2026-10-05", okResult)
	if _, sum, _ := f.run(b, "--daily-cap", "1"); sum.Outcome != OutcomeReviewed { // another patch uses the cap of the retry day
		t.Fatalf("another patch: %+v", sum)
	}
	calls := f.s.count()
	if code, sum, _ := f.run(a, "--daily-cap", "1"); code != 3 || sum.Outcome != OutcomeDailyCap || f.s.count() != calls {
		t.Fatalf("retry on a day whose cap is spent: %d %+v", code, sum)
	}
	f.on("2026-10-06", okResult)
	if code, sum, _ := f.run(a, "--daily-cap", "1"); code != 0 || sum.Outcome != OutcomeReviewed || sum.Status != "complete" { // the refusal did not use the retry up
		t.Fatalf("the retry once the cap allows it: %d %+v", code, sum)
	}
}

// The retry may use another head of the same patch: it writes its own artifact and leaves the first one alone; the answer afterwards is the retry's.
func TestRetryOnAnotherHeadOfTheSamePatch(t *testing.T) {
	f := newFixture(t)
	h1 := f.repo.change(f.base, 2)
	f.on("2026-10-04", quotaResult)
	_, first, _ := f.run(h1)
	f.repo.git("checkout", "-q", "--detach", h1)
	h2 := f.repo.commit(nil) // the same patch
	f.on("2026-10-05", okResult)
	code, retry, errOut := f.run(h2)
	if code != 0 || retry.Outcome != OutcomeReviewed || retry.Artifact != filepath.Join(f.out, h2+".json") || first.Artifact != filepath.Join(f.out, h1+".json") {
		t.Fatalf("retry on another head: %d %+v %s", code, retry, errOut)
	}
	data, _ := os.ReadFile(first.Artifact) // the first head's artifact is untouched
	if _, again, _ := f.run(h1); again.Outcome != OutcomeAlreadyReviewed || again.ReviewedHead != h2 || again.Status != "complete" || !bytes.Contains(data, []byte("unavailable")) {
		t.Fatalf("after the retry: %+v", again)
	}
}

// The retry of the same head replaces the unavailable artifact the ledger recorded, and only that file.
func TestRetryReplacesOnlyTheRecordedUnavailableArtifact(t *testing.T) {
	f := newFixture(t)
	head := f.repo.change(f.base, 2)
	f.on("2026-10-04", quotaResult)
	_, first, _ := f.run(head)
	f.on("2026-10-05", okResult)
	if code, retry, errOut := f.run(head); code != 0 || retry.Outcome != OutcomeReviewed || retry.SHA256 == first.SHA256 || retry.Artifact != first.Artifact {
		t.Fatalf("retry of the same head: %d %+v %s", code, retry, errOut)
	}
	if data, err := os.ReadFile(first.Artifact); err != nil || !bytes.Contains(data, []byte(`"complete"`)) {
		t.Fatalf("the artifact was not replaced: %v", err)
	}
	// Another file at the recorded path (its bytes no longer the recorded ones) is foreign: the retry refuses before any agy call.
	g := newFixture(t)
	head = g.repo.change(g.base, 2)
	g.on("2026-10-04", quotaResult)
	_, first, _ = g.run(head)
	if err := os.WriteFile(first.Artifact, []byte("someone else's file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.on("2026-10-05", okResult)
	calls := g.s.count()
	if code, _, errOut := g.run(head); code != 1 || !strings.Contains(errOut, "use another --out") || g.s.count() != calls {
		t.Fatalf("a replaced artifact file: %d %s", code, errOut)
	}
}

// The one more attempt is spent when the retry starts: a retry that ends without a result leaves the patch closed on the earlier unavailable artifact.
func TestRetryThatEndsWithoutAResultClosesThePatch(t *testing.T) {
	f := newFixture(t)
	head := f.repo.change(f.base, 2)
	f.on("2026-10-04", quotaResult)
	_, first, _ := f.run(head)
	f.on("2026-10-05", okResult)
	ctx, cancel := context.WithCancel(context.Background())
	e := env{now: func() time.Time { return f.at }, runner: func(ctx context.Context, _ agy.Config, _ agy.Request) (agy.Result, error) {
		cancel()
		return agy.Result{}, ctx.Err()
	}}
	var out, errOut bytes.Buffer
	if code := run(ctx, f.args(head), &out, &errOut, e); code != 130 {
		t.Fatalf("interrupted retry: %d %s", code, errOut.String())
	}
	calls := f.s.count()
	for _, day := range []string{"2026-10-05", "2026-10-06"} {
		f.on(day, okResult)
		code, sum, _ := f.run(head)
		if code != 0 || sum.Outcome != OutcomeAlreadyReviewed || sum.Artifact != first.Artifact || sum.Status != "unavailable" || f.s.count() != calls {
			t.Fatalf("%s after the interrupted retry: %d %+v (calls %d, were %d)", day, code, sum, f.s.count(), calls)
		}
	}
}

// An attempt that ends after UTC midnight is dated by its end: the answer and the ledger agree on the day the retry opens.
func TestRetryDayIsTheDayTheAttemptEnded(t *testing.T) {
	f := newFixture(t)
	head := f.repo.change(f.base, 2)
	f.on("2026-10-04", quotaResult)
	f.at = f.at.Add(11*time.Hour + 59*time.Minute) // it starts at 23:59 and the answer comes after midnight
	e := env{now: func() time.Time { return f.at }, runner: func(ctx context.Context, c agy.Config, r agy.Request) (agy.Result, error) {
		f.at = time.Date(2026, 10, 5, 0, 1, 0, 0, time.UTC)
		return f.s.run(ctx, c, r)
	}}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), f.args(head), &out, &errOut, e); code != 0 || !strings.Contains(out.String(), `"retryNotBefore":"2026-10-06"`) {
		t.Fatalf("attempt across midnight: %d %s %s", code, out.String(), errOut.String())
	}
	f.on("2026-10-05", okResult)
	if code, sum, _ := f.run(head); code != 3 || sum.Outcome != OutcomeRetryDeferred || sum.RetryNotBefore != "2026-10-06" {
		t.Fatalf("the day after the start: %d %+v", code, sum)
	}
}

// A ledger written before the retry rule has finished lines with status unavailable: they close the patch as they always did.
func TestLedgerFromBeforeTheRetryRuleKeepsClosingThePatch(t *testing.T) {
	f := newFixture(t)
	head := f.repo.change(f.base, 2)
	_, first, _ := f.run(head) // learns the patch-id
	line := func(event, extra string) string {
		return fmt.Sprintf(`{"time":"2026-10-03T09:00:00Z","event":%q,"patchId":%q,"head":%q%s}`+"\n", event, first.PatchID, head, extra)
	}
	old := line("started", "") + line("finished", `,"artifact":"/old/a.json","sha256":"00","status":"unavailable"`)
	if err := os.WriteFile(filepath.Join(f.state, "ledger.jsonl"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := f.s.count()
	if code, sum, _ := f.run(head); code != 0 || sum.Outcome != OutcomeAlreadyReviewed || sum.Status != "unavailable" || sum.Artifact != "/old/a.json" || f.s.count() != calls {
		t.Fatalf("an old unavailable finished line: %d %+v", code, sum)
	}
}

// Only a review that could not run at all for these reasons may be retried: every review call must be one of them.
func TestAccountUnavailable(t *testing.T) {
	const u = review.StatusUnavailable
	for _, c := range []struct {
		name   string
		status review.Status
		calls  []string // stage/class/reason
		want   string
	}{
		{"quota", u, []string{"review/unavailable/quota", "review/unavailable/quota"}, "quota"},
		{"authentication and quota", u, []string{"review/unavailable/quota", "review/unavailable/authentication"}, "authentication,quota"},
		{"unknown model", u, []string{"review/unavailable/unknown_model"}, "unknown_model"},
		{"not started", u, []string{"review/unavailable/not_started"}, "not_started"},
		{"an auxiliary call does not count", u, []string{"review/unavailable/quota", "group/unavailable/crash"}, "quota"},
		{"quota and a crash", u, []string{"review/unavailable/quota", "review/unavailable/crash"}, "crash,quota"},
		{"crash", u, []string{"review/unavailable/crash"}, "crash"},
		{"runner error", u, []string{"review/unavailable/runner_error"}, "runner_error"},
		{"quota and a time limit", u, []string{"review/unavailable/quota", "review/invalid/time_limit_exceeded"}, ""},
		{"content filter", u, []string{"review/unavailable/content_filter"}, ""},
		{"lock wait expired", u, []string{"review/unavailable/lock_wait_expired"}, ""},
		{"no review call", u, nil, ""},
		{"partial", review.StatusPartial, []string{"review/unavailable/quota"}, ""},
	} {
		a := &review.Artifact{Status: c.status}
		for _, s := range c.calls {
			p := strings.Split(s, "/")
			a.Calls = append(a.Calls, review.CallRecord{Stage: p[0], Class: p[1], Reason: p[2]})
		}
		if got, ok := retryableUnavailable(a); ok != (c.want != "") || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, got, ok, c.want)
		}
	}
}

// A lock wait is the one unavailable run where agy never ran: lockWaitExpired names it and agyCalledOf says which runs called agy and which did not, when that can be told.
func TestLockWaitExpiredAndAgyCalled(t *testing.T) {
	const u = review.StatusUnavailable
	art := func(status review.Status, calls ...string) *review.Artifact {
		a := &review.Artifact{Status: status}
		for _, s := range calls {
			p := strings.Split(s, "/")
			a.Calls = append(a.Calls, review.CallRecord{Stage: p[0], Class: p[1], Reason: p[2]})
		}
		return a
	}
	format := func(b *bool) string {
		switch {
		case b == nil:
			return "unknown"
		case *b:
			return "true"
		}
		return "false"
	}
	for _, c := range []struct {
		name   string
		a      *review.Artifact
		lock   bool
		called string
	}{
		{"every review call waited for the lock", art(u, "review/unavailable/lock_wait_expired", "review/unavailable/lock_wait_expired"), true, "false"},
		{"an auxiliary call does not count", art(u, "review/unavailable/lock_wait_expired", "group/unavailable/crash"), true, "false"},
		{"a lock wait beside a quota call", art(u, "review/unavailable/lock_wait_expired", "review/unavailable/quota"), false, "true"},
		{"a quota run", art(u, "review/unavailable/quota"), false, "true"},
		{"agy was not started", art(u, "review/unavailable/not_started"), false, "false"},
		{"a crash", art(u, "review/unavailable/crash"), false, "unknown"},
		{"a runner error", art(u, "review/unavailable/runner_error"), false, "unknown"},
		{"a completed review", art(review.StatusComplete, "review/normal/"), false, "unknown"},
		{"a partial review", art(review.StatusPartial, "review/unavailable/quota"), false, "unknown"},
	} {
		if got := lockWaitExpired(c.a); got != c.lock {
			t.Errorf("%s: lockWaitExpired = %v, want %v", c.name, got, c.lock)
		}
		if got := format(agyCalledOf(c.a)); got != c.called {
			t.Errorf("%s: agyCalledOf = %s, want %s", c.name, got, c.called)
		}
	}
}

// A lock wait does not count toward the daily cap, so a patch may be tried again the same day until one attempt really starts.
func TestLockWaitExpiredDoesNotCountTowardTheCap(t *testing.T) {
	f := newFixture(t)
	head := f.repo.change(f.base, 2)
	f.on("2026-10-04", lockWaitResult)
	for i := 0; i < 2; i++ {
		if code, sum, errOut := f.run(head, "--daily-cap", "1"); code != 3 || sum.Outcome != OutcomeLockWaitExpired {
			t.Fatalf("lock wait %d: %d %+v %s", i, code, sum, errOut)
		}
	}
	if n := runsOn(f.ledger(), "2026-10-04"); n != 0 {
		t.Fatalf("a lock wait counted %d toward the cap", n)
	}
	f.on("2026-10-04", okResult)
	if code, sum, errOut := f.run(head, "--daily-cap", "1"); code != 0 || sum.Outcome != OutcomeReviewed || sum.Status != "complete" {
		t.Fatalf("the cap of 1 still allows the review after two lock waits: %d %+v %s", code, sum, errOut)
	}
}

// A lock wait does not spend the one more attempt an unavailable run left open: on the retry day the lock may fail twice and the retry is still there the day after.
func TestLockWaitExpiredDoesNotSpendTheRetry(t *testing.T) {
	f := newFixture(t)
	head := f.repo.change(f.base, 2)
	f.on("2026-10-04", quotaResult)
	if _, sum, _ := f.run(head); sum.Outcome != OutcomeReviewed || sum.RetryNotBefore != "2026-10-05" {
		t.Fatalf("first attempt: %+v", sum)
	}
	f.on("2026-10-05", lockWaitResult)
	for i := 0; i < 2; i++ {
		if code, sum, _ := f.run(head); code != 3 || sum.Outcome != OutcomeLockWaitExpired {
			t.Fatalf("the lock wait %d on the retry day: %d %+v", i, code, sum)
		}
	}
	f.on("2026-10-06", okResult)
	if code, sum, errOut := f.run(head); code != 0 || sum.Outcome != OutcomeReviewed || sum.Status != "complete" {
		t.Fatalf("the one more attempt survived the lock waits: %d %+v %s", code, sum, errOut)
	}
}
