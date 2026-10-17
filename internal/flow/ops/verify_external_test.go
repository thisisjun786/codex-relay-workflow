package ops

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestExternalVerificationRefsSeparateHeads(t *testing.T) {
	repo, base, head := ghRepo(t)
	v := ghVerifier(&fakeGH{}, t.TempDir())
	v.Coordinator = &store.Store{}
	g := &ghVerify{v: v, run: v.Run}
	req := VerifyRequest{Node: "external", Worktree: repo, Head: head, Base: base}
	first, err := g.branchFor(context.Background(), repo, req, head)
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "checkout", "devin/test-work")
	gitIn(t, repo, "commit", "--allow-empty", "-m", "next candidate")
	next := gitIn(t, repo, "rev-parse", "HEAD")
	second, err := g.branchFor(context.Background(), repo, req, next)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("different heads share cancellation ref %q", first)
	}
}

// This fake is shared by distinct controller clients/checkouts. No GitHub
// command, network push or worker process is used by these scenarios.
type coordinatedGH struct {
	mu                     sync.Mutex
	f                      *fakeGH
	loseDispatch, hideRuns bool
}

func (c *coordinatedGH) run(ctx context.Context, dir string, env []string, name string, args ...string) (int, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if name == "git" && len(args) > 0 && args[0] == "ls-remote" {
		ref := args[len(args)-1]
		head := c.f.pushed[strings.TrimPrefix(ref, "refs/heads/")]
		if head == "" {
			return 0, nil, nil
		}
		return 0, []byte(head + "\t" + ref + "\n"), nil
	}
	if name == "gh" && len(args) > 1 && args[0] == "run" && args[1] == "list" && c.hideRuns {
		return 0, []byte("[]"), nil
	}
	code, out, err := c.f.run()(ctx, dir, env, name, args...)
	if name == "gh" && len(args) > 1 && args[0] == "api" && strings.Contains(args[len(args)-1], "/attempts/") {
		route := strings.Split(args[len(args)-1], "/")
		runID, _ := strconv.ParseInt(route[5], 10, 64)
		attempt, _ := strconv.Atoi(route[7])
		var page map[string]any
		if json.Unmarshal(out, &page) == nil {
			if jobs, ok := page["jobs"].([]any); ok {
				for _, value := range jobs {
					job := value.(map[string]any)
					job["run_id"] = runID
					job["run_attempt"] = attempt
				}
			}
			out, _ = json.Marshal(page)
		}
	}
	if name == "gh" && len(args) > 1 && args[0] == "workflow" && args[1] == "run" && c.loseDispatch {
		return 1, nil, errors.New("reply lost after server accepted dispatch")
	}
	return code, out, err
}
func controllerVerifier(t *testing.T, c *coordinatedGH, s *store.Store) GitHubActionsVerifier {
	t.Helper()
	v := ghVerifier(c.f, t.TempDir())
	v.Coordinator = s
	v.Run = c.run
	return v
}
func controllerStore(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func controllerGH() *coordinatedGH {
	return &coordinatedGH{f: &fakeGH{workflow: fakeGHWorkflow{ID: 77, Name: "CRW CI", Path: ".github/workflows/ci.yml", State: "active"}, newRun: func(r *fakeGHRun) {
		r.views[1] = []fakeGHView{{Status: "completed", Conclusion: "success"}}
		r.jobs[1] = successJobs()
	}}}
}
func TestExternalVerificationTwoHostsShareRunAndResult(t *testing.T) {
	repo, base, head := ghRepo(t)
	other := filepath.Join(t.TempDir(), "other-host")
	gitIn(t, repo, "clone", "--no-hardlinks", repo, other)
	gitIn(t, other, "remote", "set-url", "origin", "https://github.com/o/r.git")
	db := filepath.Join(t.TempDir(), "relay.sqlite3")
	c := controllerGH()
	v1, v2 := controllerVerifier(t, c, controllerStore(t, db)), controllerVerifier(t, c, controllerStore(t, db))
	first, err := v1.Verify(context.Background(), VerifyRequest{Worktree: repo, Head: head, Base: base})
	if err != nil || !first.Pass {
		t.Fatalf("first: %+v %v", first, err)
	}
	second, err := v2.Verify(context.Background(), VerifyRequest{Worktree: other, Head: head, Base: base})
	if err != nil || !second.Pass || first.Digest != second.Digest {
		t.Fatalf("attachment: %+v %v", second, err)
	}
	if len(c.f.runs) != 1 || len(c.f.scanArgs) != 2 {
		t.Fatalf("runs=%d scans=%d", len(c.f.runs), len(c.f.scanArgs))
	}
}
func TestExternalVerificationLostDispatchSurvivesRestart(t *testing.T) {
	repo, base, head := ghRepo(t)
	db := filepath.Join(t.TempDir(), "relay.sqlite3")
	c := controllerGH()
	c.loseDispatch = true
	s := controllerStore(t, db)
	v := controllerVerifier(t, c, s)
	req := VerifyRequest{Worktree: repo, Head: head, Base: base}
	if out, err := v.Verify(context.Background(), req); err == nil || out.Pass {
		t.Fatalf("lost reply passed: %+v %v", out, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	v = controllerVerifier(t, c, controllerStore(t, db))
	c.loseDispatch = false
	c.hideRuns = true
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		waiting := v
		waiting.Run = func(ctx context.Context, dir string, env []string, name string, args ...string) (int, []byte, error) {
			code, out, err := v.Run(ctx, dir, env, name, args...)
			if name == "gh" && len(args) > 1 && args[0] == "run" && args[1] == "list" {
				cancel()
			}
			return code, out, err
		}
		out, err := waiting.Verify(ctx, req)
		cancel()
		if err == nil || out.Pass {
			t.Fatalf("empty listing passed: %+v %v", out, err)
		}
	}
	if len(c.f.runs) != 1 {
		t.Fatal("unknown dispatch repeated")
	}
	c.hideRuns = false
	if out, err := v.Verify(context.Background(), req); err != nil || !out.Pass {
		t.Fatalf("reconcile: %+v %v", out, err)
	}
	if len(c.f.runs) != 1 {
		t.Fatal("reconcile dispatched again")
	}
}
func TestExternalVerificationTerminalFailuresAreShared(t *testing.T) {
	for _, conclusion := range []string{"failure", "cancelled", "skipped"} {
		t.Run(conclusion, func(t *testing.T) {
			repo, base, head := ghRepo(t)
			c := controllerGH()
			c.f.newRun = func(r *fakeGHRun) {
				r.views[1] = []fakeGHView{{Status: "completed", Conclusion: "success"}}
				r.jobs[1] = successJobs()
				r.jobs[1][0].Conclusion = conclusion
			}
			v := controllerVerifier(t, c, controllerStore(t, filepath.Join(t.TempDir(), "relay.sqlite3")))
			req := VerifyRequest{Worktree: repo, Head: head, Base: base}
			for i := 0; i < 2; i++ {
				out, err := v.Verify(context.Background(), req)
				if err != nil || out.Pass {
					t.Fatalf("required job %s: %+v %v", conclusion, out, err)
				}
			}
			if len(c.f.runs) != 1 {
				t.Fatal("failure dispatched again")
			}
		})
	}
}
func TestExternalVerificationRefDriftAndBlobRangeRefused(t *testing.T) {
	repo, base, head := ghRepo(t)
	c := controllerGH()
	v := controllerVerifier(t, c, controllerStore(t, filepath.Join(t.TempDir(), "relay.sqlite3")))
	req := VerifyRequest{Worktree: repo, Head: head, Base: base}
	if out, err := v.Verify(context.Background(), req); err != nil || !out.Pass {
		t.Fatalf("first: %+v %v", out, err)
	}
	for branch := range c.f.pushed {
		c.f.pushed[branch] = base
	}
	if out, err := v.Verify(context.Background(), req); err == nil || out.Pass {
		t.Fatal("moved immutable ref passed")
	}
	write(t, repo, "oversized", strings.Repeat("x", (2<<20)+1), 0600)
	gitIn(t, repo, "add", "oversized")
	gitIn(t, repo, "commit", "-m", "add oversized")
	gitIn(t, repo, "rm", "oversized")
	gitIn(t, repo, "commit", "-m", "remove oversized")
	req.Head = gitIn(t, repo, "rev-parse", "HEAD")
	if out, err := v.Verify(context.Background(), req); err == nil || out.Pass || !strings.Contains(err.Error(), "over 2 MiB") {
		t.Fatalf("intermediate blob: %+v %v", out, err)
	}
	if len(c.f.runs) != 1 {
		t.Fatal("range failure dispatched")
	}
}

func TestExternalVerificationConcurrentHostsAndDifferentHeads(t *testing.T) {
	repo, base, head := ghRepo(t)
	db := filepath.Join(t.TempDir(), "relay.sqlite3")
	c := controllerGH()
	first := controllerVerifier(t, c, controllerStore(t, db))
	second := controllerVerifier(t, c, controllerStore(t, db))
	req := VerifyRequest{Worktree: repo, Head: head, Base: base}
	type answer struct {
		out VerifyOutcome
		err error
	}
	done := make(chan answer, 2)
	start := make(chan struct{})
	for _, v := range []GitHubActionsVerifier{first, second} {
		go func(v GitHubActionsVerifier) {
			<-start
			out, err := v.Verify(context.Background(), req)
			done <- answer{out, err}
		}(v)
	}
	close(start)
	a, b := <-done, <-done
	// An attachment that arrives before publication reports uncertainty; it may
	// read the completed flight on its next poll, but cannot repeat its dispatch.
	for _, result := range []*answer{&a, &b} {
		if result.err != nil && strings.Contains(result.err.Error(), "dispatch_unknown") {
			result.out, result.err = second.Verify(context.Background(), req)
		}
		if result.err != nil || !result.out.Pass {
			t.Fatalf("attach: %+v", result)
		}
	}
	if a.out.Digest != b.out.Digest || len(c.f.runs) != 1 {
		t.Fatal("concurrent hosts did not share one result")
	}
	write(t, repo, "different.txt", "another tree", 0600)
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-m", "different head")
	req.Head = gitIn(t, repo, "rev-parse", "HEAD")
	if out, err := second.Verify(context.Background(), req); err != nil || !out.Pass {
		t.Fatalf("second head: %+v %v", out, err)
	}
	if len(c.f.runs) != 2 || c.f.runs[0].branch == c.f.runs[1].branch || c.f.runs[0].head == c.f.runs[1].head {
		t.Fatal("different heads share a concurrency ref")
	}
}
func TestExternalVerificationLostRetryReconcilesSameAttempt(t *testing.T) {
	repo, base, head := ghRepo(t)
	c := controllerGH()
	db := filepath.Join(t.TempDir(), "relay.sqlite3")
	c.f.newRun = func(r *fakeGHRun) {
		r.views[1] = []fakeGHView{{Status: "completed", Conclusion: "failure"}}
		r.jobs[1] = successJobs()
		r.jobs[1][0].Conclusion = "failure"
		r.views[2] = []fakeGHView{{Status: "completed", Conclusion: "success"}}
		r.jobs[2] = successJobs()
	}
	v := controllerVerifier(t, c, controllerStore(t, db))
	run := v.Run
	v.Run = func(ctx context.Context, dir string, env []string, name string, args ...string) (int, []byte, error) {
		code, out, err := run(ctx, dir, env, name, args...)
		if name == "gh" && len(args) > 1 && args[0] == "run" && args[1] == "rerun" {
			return 1, nil, errors.New("lost retry reply")
		}
		return code, out, err
	}
	req := VerifyRequest{Worktree: repo, Head: head, Base: base}
	if out, err := v.Verify(context.Background(), req); err == nil || out.Pass {
		t.Fatal("uncertain retry passed")
	}
	v = controllerVerifier(t, c, controllerStore(t, db))
	out, err := v.Verify(context.Background(), req)
	if err != nil || !out.Pass {
		t.Fatalf("reconcile attempt two: %+v %v", out, err)
	}
	if len(c.f.runs) != 1 || len(c.f.reruns) != 1 {
		t.Fatal("uncertain retry was repeated")
	}
	record, err := dagsched.DecodeVerificationRecord(out.Record)
	if err != nil || len(record.GitHub.Runs) != 2 {
		t.Fatal("original failure evidence lost")
	}
}

func TestExternalVerificationReusesTreeWithFreshRangeScans(t *testing.T) {
	repo, base, head := ghRepo(t)
	c := controllerGH()
	v := controllerVerifier(t, c, controllerStore(t, filepath.Join(t.TempDir(), "relay.sqlite3")))
	first, err := v.Verify(context.Background(), VerifyRequest{Worktree: repo, Head: head, Base: base})
	if err != nil || !first.Pass {
		t.Fatalf("first: %+v %v", first, err)
	}
	gitIn(t, repo, "commit", "--allow-empty", "-m", "same tree, new range")
	next := gitIn(t, repo, "rev-parse", "HEAD")
	out, err := v.Verify(context.Background(), VerifyRequest{Worktree: repo, Head: next, Base: head})
	if err != nil || !out.Pass {
		t.Fatalf("reuse: %+v %v", out, err)
	}
	record, err := dagsched.DecodeVerificationRecord(out.Record)
	if err != nil || record.GitHub.ReusedFrom == nil || record.GitHub.ReusedFrom.Digest != first.Digest || record.BaseCommit != head || record.HeadCommit != next || record.GitHub.Scan.Head != next || len(c.f.runs) != 1 || len(c.f.scanArgs) != 2 {
		t.Fatalf("reuse lost provenance/range: %+v %v", record, err)
	}
}

func TestExternalVerificationRejectsForeignJobIdentity(t *testing.T) {
	repo, base, head := ghRepo(t)
	c := controllerGH()
	v := controllerVerifier(t, c, controllerStore(t, filepath.Join(t.TempDir(), "relay.sqlite3")))
	run := v.Run
	v.Run = func(ctx context.Context, dir string, env []string, name string, args ...string) (int, []byte, error) {
		code, out, err := run(ctx, dir, env, name, args...)
		if name == "gh" && len(args) > 1 && args[0] == "api" && strings.Contains(args[len(args)-1], "/jobs") {
			var page map[string]any
			if json.Unmarshal(out, &page) == nil {
				page["jobs"].([]any)[0].(map[string]any)["run_id"] = 999
				out, _ = json.Marshal(page)
			}
		}
		return code, out, err
	}
	if out, err := v.Verify(context.Background(), VerifyRequest{Worktree: repo, Head: head, Base: base}); err == nil || out.Pass || !strings.Contains(err.Error(), "another run or attempt") {
		t.Fatalf("foreign job passed: %+v %v", out, err)
	}
}

func TestExternalVerificationRefCreationCannotOverwriteRacingWriter(t *testing.T) {
	repo, base, head := ghRepo(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitIn(t, repo, "clone", "--bare", repo, remote)
	c := controllerGH()
	v := controllerVerifier(t, c, controllerStore(t, filepath.Join(t.TempDir(), "relay.sqlite3")))
	run := v.Run
	var racedRef string
	v.Run = func(ctx context.Context, dir string, env []string, name string, args ...string) (int, []byte, error) {
		if name == "git" && len(args) > 0 && args[0] == "push" {
			// A real bare Git remote receives an intervening writer after the fake
			// ls-remote reported absent. Git itself must refuse the attempted creation.
			_, racedRef, _ = strings.Cut(args[len(args)-1], ":")
			gitIn(t, remote, "update-ref", racedRef, base)
			local := append([]string(nil), args...)
			for i, arg := range local {
				if arg == "origin" {
					local[i] = remote
				}
			}
			code, out, err := execGit(ctx, dir, env, local...)
			if code == 0 && err == nil {
				c.mu.Lock()
				if c.f.pushed == nil {
					c.f.pushed = map[string]string{}
				}
				c.f.pushed[strings.TrimPrefix(racedRef, "refs/heads/")] = head
				c.mu.Unlock()
			}
			return code, out, err
		}
		return run(ctx, dir, env, name, args...)
	}
	out, err := v.Verify(context.Background(), VerifyRequest{Worktree: repo, Head: head, Base: base})
	if err == nil || out.Pass || len(c.f.runs) != 0 || racedRef == "" {
		t.Fatalf("racing ref overwritten: pass=%t error=%v", out.Pass, err)
	}
	if got := gitIn(t, remote, "rev-parse", racedRef); got != base {
		t.Fatalf("racing writer lost its ref: %s", got)
	}
}

func TestExternalVerificationWaitsForDelayedRunVisibility(t *testing.T) {
	repo, base, head := ghRepo(t)
	c := controllerGH()
	v := controllerVerifier(t, c, controllerStore(t, filepath.Join(t.TempDir(), "relay.sqlite3")))
	run := v.Run
	delayed := false
	v.Run = func(ctx context.Context, dir string, env []string, name string, args ...string) (int, []byte, error) {
		code, out, err := run(ctx, dir, env, name, args...)
		if name == "gh" && len(args) > 1 && args[0] == "run" && args[1] == "list" && len(c.f.runs) > 0 && !delayed {
			delayed = true
			return 0, []byte("[]"), nil
		}
		return code, out, err
	}
	out, err := v.Verify(context.Background(), VerifyRequest{Worktree: repo, Head: head, Base: base})
	if err != nil || !out.Pass || !delayed || len(c.f.runs) != 1 {
		t.Fatalf("listing lag: pass=%t dispatches=%d error=%v", out.Pass, len(c.f.runs), err)
	}
}
