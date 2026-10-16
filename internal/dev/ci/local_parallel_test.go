//go:build dev

package ci

// The concurrency tests of `ci local -jobs` (CRW-1584): real steps in real worktrees, with a Go
// child process of the test binary itself as the step's work, so overlap, isolation, ordering and
// the record are all measured rather than slept on.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLocalParallelHelper is the child process the parallel tests run as a step. It returns at
// once unless it is running as a helper (the marker), in which case it writes file.txt in its own
// checkout, posts its environment to the test's server, waits for the release gate, prints a
// passing `ok` package line and exits.
func TestLocalParallelHelper(t *testing.T) {
	if os.Getenv("CRW_TEST_PARALLEL_HELPER") == "" {
		return
	}
	name := os.Getenv("CRW_TEST_PARALLEL_NAME")
	receipt := map[string]string{
		"name":     name,
		"cwd":      mustGetwd(t),
		"home":     os.Getenv("HOME"),
		"tmpdir":   os.Getenv("TMPDIR"),
		"gocache":  os.Getenv("GOCACHE"),
		"gotmpdir": os.Getenv("GOTMPDIR"),
		"runner":   os.Getenv("RUNNER_TEMP"),
		"output":   os.Getenv("GITHUB_OUTPUT"),
	}
	entries, err := os.ReadDir(os.Getenv("TMPDIR"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("initial TMPDIR not empty: %v (%v)", entries, err)
	}
	receipt["test_tmp"] = t.TempDir()
	if err := os.WriteFile("file.txt", []byte(name+"\n"), 0o644); err != nil {
		t.Fatalf("helper file.txt: %v", err)
	}
	body, _ := json.Marshal(receipt)
	resp, err := http.Post(os.Getenv("CRW_TEST_PARALLEL_URL")+"/arrive", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("helper post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("helper post: %s", resp.Status)
	}
	resp, err = http.Get(os.Getenv("CRW_TEST_PARALLEL_URL") + "/release")
	if err != nil {
		t.Fatalf("helper release: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("helper release: %s", resp.Status)
	}
	// one package this leg reports it ran
	println("ok example/" + name + " 0.01s")
}

func mustGetwd(t *testing.T) string {
	d, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// parallelServer is the arrivals gate: every helper POSTs its receipt to /arrive and blocks on
// /release until the test closes the gate.
type parallelServer struct {
	*httptest.Server
	arrivals    chan map[string]string
	release     chan struct{}
	releaseOnce sync.Once
	peak        atomic.Int32
	inflight    atomic.Int32
}

func (s *parallelServer) closeRelease() {
	s.releaseOnce.Do(func() { close(s.release) })
}

func newParallelServer(t *testing.T) *parallelServer {
	s := &parallelServer{arrivals: make(chan map[string]string, 16), release: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/arrive", func(w http.ResponseWriter, r *http.Request) {
		var receipt map[string]string
		if err := json.NewDecoder(r.Body).Decode(&receipt); err != nil || receipt["name"] == "" {
			w.WriteHeader(400)
			return
		}
		n := s.inflight.Add(1)
		for old := s.peak.Load(); n > old; old = s.peak.Load() {
			if s.peak.CompareAndSwap(old, n) {
				break
			}
		}
		s.arrivals <- receipt
		w.WriteHeader(200)
	})
	mux.HandleFunc("/release", func(w http.ResponseWriter, r *http.Request) {
		<-s.release
		s.inflight.Add(-1)
		w.WriteHeader(200)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// awaitVerify runs localVerify on a goroutine and registers a cleanup that releases the gate and
// joins it before the fixture's roots are removed, so a failing assertion never orphans the run.
func awaitVerify(t *testing.T, opts localOptions, srv *parallelServer) (finished chan struct{}, made *verificationRecord, verr *error) {
	t.Helper()
	finished = make(chan struct{})
	made, verr = &verificationRecord{}, new(error)
	go func() {
		defer close(finished)
		var reok bool
		*made, reok, *verr = localVerify(opts, "", io.Discard)
		_ = reok
	}()
	t.Cleanup(func() {
		if srv != nil {
			srv.closeRelease()
		}
		select {
		case <-finished:
		case <-time.After(20 * time.Second):
			t.Logf("localVerify did not finish before cleanup")
		}
	})
	return finished, made, verr
}

func (s *parallelServer) arrive(t *testing.T) map[string]string {
	t.Helper()
	select {
	case r := <-s.arrivals:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("a helper did not arrive before the deadline")
		return nil
	}
}

// helperStep is a step that runs the helper child over the fixture.
func (s *parallelServer) helperStep(name string) localStep {
	binary, _ := os.Executable()
	return localStep{
		name: "run " + name, kind: localRun, scope: "full",
		command: `CRW_TEST_PARALLEL_HELPER=1 CRW_TEST_PARALLEL_NAME=` + name + ` CRW_TEST_PARALLEL_URL=` + s.URL + ` "$TEST_BINARY" -test.run='^TestLocalParallelHelper$' -test.v`,
		env:     []string{"TEST_BINARY=" + binary},
	}
}

func parallelPlan(commands func(name string) localStep, jobs ...string) []localJob {
	var plan []localJob
	for _, name := range jobs {
		plan = append(plan, localJob{name: name, steps: []localStep{
			{kind: localAction, action: localCheckout, scope: "full"},
			commands(name),
		}})
	}
	return plan
}

// Two jobs run at once under -jobs 2: both helpers arrive before either is released, each in its
// own worktree/home/TMPDIR/caches, the caller's file.txt is untouched and the record and console
// groups stay in plan order.
func TestLocalParallelJobsRunConcurrentlyIsolated(t *testing.T) {
	repo := newLocalFixture(t)
	srv := newParallelServer(t)
	record := filepath.Join(t.TempDir(), "record.json")
	opts := localRunOptions(repo, parallelPlan(srv.helperStep, "a", "b"), record)
	opts.Jobs = 2
	unused := filepath.Join(t.TempDir(), "must-not-be-used")
	opts.Env = []string{"GOTMPDIR=" + unused}

	var buf strings.Builder
	finished := make(chan struct{})
	var made verificationRecord
	var verr error
	go func() {
		defer close(finished)
		var reok bool
		made, reok, verr = localVerify(opts, "", &buf)
		_ = reok
	}()
	t.Cleanup(func() {
		srv.closeRelease()
		select {
		case <-finished:
		case <-time.After(20 * time.Second):
			t.Logf("localVerify did not finish before cleanup")
		}
	})

	// the serial loop's one arrival would deadlock here; two must arrive before release
	first := srv.arrive(t)
	second := srv.arrive(t)
	if first["name"] == second["name"] {
		t.Fatalf("one helper arrived twice: %v vs %v", first, second)
	}
	// read each worker's file.txt while the helpers still hold their worktrees
	for _, r := range []map[string]string{first, second} {
		if body, err := os.ReadFile(filepath.Join(r["cwd"], "file.txt")); err != nil || strings.TrimSpace(string(body)) != r["name"] {
			t.Errorf("helper %s file.txt is %q (%v), want its own name", r["name"], body, err)
		}
		rel, err := filepath.Rel(r["tmpdir"], r["test_tmp"])
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			t.Errorf("test temp %q escaped job TMPDIR %q", r["test_tmp"], r["tmpdir"])
		}
		if r["gotmpdir"] != "" {
			t.Errorf("job overrides GOTMPDIR: %s", r["gotmpdir"])
		}
	}
	srv.closeRelease()
	<-finished
	if verr != nil {
		t.Fatal(verr)
	}
	got := struct{ rec verificationRecord }{made}
	if srv.peak.Load() != 2 {
		t.Fatalf("peak %d, want 2", srv.peak.Load())
	}
	// isolation: every worker-owned root differs, caches included
	for _, k := range []string{"cwd", "home", "tmpdir", "gocache", "runner", "output"} {
		if first[k] == second[k] {
			t.Errorf("workers share %s: %v", k, first[k])
		}
	}
	if _, err := os.Lstat(unused); err == nil {
		t.Errorf("the injected GOTMPDIR directory was used")
	}
	// the run's cleanup removed every worker root (this run did not keep)
	for _, r := range []map[string]string{first, second} {
		if _, err := os.Lstat(filepath.Dir(r["cwd"])); err == nil {
			t.Errorf("the worker's task root %s was left", r["cwd"])
		}
	}
	if body, err := os.ReadFile(filepath.Join(repo.root, "file.txt")); err != nil || strings.TrimSpace(string(body)) != "one" {
		t.Errorf("the caller's file.txt is %q, want one", body)
	}
	rec := got.rec
	if len(rec.Jobs) != 2 || rec.Jobs[0].Name != "a" || rec.Jobs[1].Name != "b" {
		t.Fatalf("record jobs %+v, want a then b", rec.Jobs)
	}
	if rec.TestPackagesRun == nil || *rec.TestPackagesRun != 2 {
		t.Errorf("TestPackagesRun %v, want 2", rec.TestPackagesRun)
	}
	// console output groups per job, in plan order
	out := buf.String()
	ia, ib := strings.Index(out, "local: a"), strings.Index(out, "local: b")
	if ia < 0 || ib < ia {
		t.Errorf("console groups out of order:\n%s", out)
	}
	if rec.Result != localPass {
		t.Errorf("result %q: %+v", rec.Result, rec.Jobs)
	}
}

// Two legs of one job run concurrently too, not only distinct jobs.
func TestLocalParallelLegsOfOneJobOverlap(t *testing.T) {
	repo := newLocalFixture(t)
	srv := newParallelServer(t)
	plan := []localJob{{
		name: "go-product",
		legs: []string{"l1", "l2"},
		steps: []localStep{
			{kind: localAction, action: localCheckout, scope: "full"},
			srv.helperStep("leg"),
		},
	}}
	opts := localRunOptions(repo, plan, filepath.Join(t.TempDir(), "record.json"))
	opts.Jobs = 2

	finished := make(chan struct{})
	var legsMade verificationRecord
	var verr error
	go func() {
		defer close(finished)
		var reok bool
		legsMade, reok, verr = localVerify(opts, "", io.Discard)
		_ = reok
	}()
	t.Cleanup(func() {
		srv.closeRelease()
		select {
		case <-finished:
		case <-time.After(20 * time.Second):
			t.Logf("localVerify did not finish")
		}
	})
	first, second := srv.arrive(t), srv.arrive(t)
	srv.closeRelease()
	<-finished
	if verr != nil {
		t.Fatal(verr)
	}
	if len(legsMade.Jobs) != 2 || legsMade.Result != localPass || legsMade.Jobs[0].Result != localPassed || legsMade.Jobs[1].Result != localPassed {
		t.Errorf("the legs' record %+v, want both passed", legsMade.Jobs)
	}
	if first["cwd"] == second["cwd"] {
		t.Errorf("the legs shared a worktree: %v", first["cwd"])
	}
}

// A job cap of 2 bounds three queued tasks: the third starts only after a running one completes.
func TestLocalParallelJobsLimitIsHonored(t *testing.T) {
	repo := newLocalFixture(t)
	srv := newParallelServer(t)
	opts := localRunOptions(repo, parallelPlan(srv.helperStep, "a", "b", "c"), filepath.Join(t.TempDir(), "record.json"))
	opts.Jobs = 2

	finished := make(chan struct{})
	var made verificationRecord
	var verr error
	go func() {
		defer close(finished)
		var reok bool
		made, reok, verr = localVerify(opts, "", io.Discard)
		_ = reok
	}()
	t.Cleanup(func() {
		srv.closeRelease()
		select {
		case <-finished:
		case <-time.After(20 * time.Second):
			t.Logf("localVerify did not finish")
		}
	})
	srv.arrive(t)
	srv.arrive(t)
	// under the cap only two arrive; the third is let in once the release frees a worker
	select {
	case r := <-srv.arrivals:
		t.Fatalf("a third helper started while two were running: %v", r)
	case <-time.After(200 * time.Millisecond):
	}
	srv.closeRelease()
	srv.arrive(t)
	<-finished
	if verr != nil {
		t.Fatal(verr)
	}
	if srv.peak.Load() > 2 {
		t.Fatalf("peak %d over the cap 2", srv.peak.Load())
	}
	if len(made.Jobs) != 3 || made.Result != localPass {
		t.Fatalf("record %+v, want the three jobs all passed", made.Jobs)
	}
}

// The Jobs 1 vs Jobs 2 records agree apart from timing and workspace paths: same steps, results,
// reasons and decisions, one skipped step after the failure, the run still failing.
func TestLocalParallelJobsOneAndTwoAgree(t *testing.T) {
	repo := newLocalFixture(t)
	plan := []localJob{
		{name: "a", steps: []localStep{
			{kind: localAction, action: localCheckout, scope: "full"},
			{name: "fail", kind: localRun, scope: "full", command: "printf '%s\\n' '--- FAIL: TestMarker' '    x_test.go:1: exact failure reason'; exit 3"},
			{name: "must skip", kind: localRun, scope: "full", command: "echo marker > reached.txt"},
		}},
		{name: "b", steps: []localStep{
			{kind: localAction, action: localCheckout, scope: "full"},
			{name: "pass", kind: localRun, scope: "full", command: "printf 'ok example/b 0.01s\\n'"},
		}},
		{name: "gate", steps: []localStep{
			{name: "aggregate", kind: localAction, action: localAggregate, scope: "full"},
		}},
	}
	run := func(jobs int) (verificationRecord, string) {
		opts := localRunOptions(repo, plan, filepath.Join(t.TempDir(), "record.json"))
		opts.Jobs = jobs
		var out strings.Builder
		made, _, err := localVerify(opts, "", &out)
		if err != nil {
			t.Fatal(err)
		}
		return made, out.String()
	}
	one, outOne := run(1)
	two, outTwo := run(2)
	if one.TestPackagesRun == nil || two.TestPackagesRun == nil || *one.TestPackagesRun != 1 || *two.TestPackagesRun != 1 {
		t.Fatalf("package counts: %v vs %v, want 1 each", one.TestPackagesRun, two.TestPackagesRun)
	}
	if one.HeadCommit != two.HeadCommit || one.TreeHash != two.TreeHash {
		t.Fatalf("the runs verified different commits: %s/%s vs %s/%s", one.HeadCommit, one.TreeHash, two.HeadCommit, two.TreeHash)
	}
	if len(one.Jobs) != len(two.Jobs) {
		t.Fatalf("jobs %d vs %d", len(one.Jobs), len(two.Jobs))
	}
	for i := range one.Jobs {
		a, b := one.Jobs[i], two.Jobs[i]
		if a.Name != b.Name || a.Result != b.Result {
			t.Fatalf("job %d differs: %+v vs %+v", i, a, b)
		}
		if len(a.Steps) != len(b.Steps) {
			t.Fatalf("job %d steps %d vs %d", i, len(a.Steps), len(b.Steps))
		}
		for j := range a.Steps {
			sa, sb := a.Steps[j], b.Steps[j]
			sa.Seconds, sb.Seconds = 0, 0
			if sa != sb {
				t.Errorf("job %s step %d differs: %+v vs %+v", a.Name, j, sa, sb)
			}
		}
	}
	if one.Result != localFail || two.Result != localFail {
		t.Fatalf("results %q vs %q, want fail both ways", one.Result, two.Result)
	}
	// the gate runs after b finished, and the failing job's reason keeps the whole FAIL line
	agg := two.Jobs[2].Steps[0]
	if agg.Result != localFailed || !strings.Contains(agg.Reason, "a") {
		t.Errorf("aggregate %+v", agg)
	}
	failed := two.Jobs[0].Steps[1]
	if !strings.Contains(failed.Reason, "--- FAIL: TestMarker") || !strings.Contains(failed.Reason, "x_test.go:1: exact failure reason") {
		t.Errorf("reason %q dropped the failure line or its diagnostic", failed.Reason)
	}
	if skipped := two.Jobs[0].Steps[2]; skipped.Result != localSkipped {
		t.Errorf("the step after the failure is %q, want skipped", skipped.Result)
	}
	// console groups stay ordered even though a failed while b still ran
	ia, ib, ig := strings.LastIndex(outTwo, "local: a"), strings.Index(outTwo, "local: b"), strings.Index(outTwo, "local: gate")
	if ia < 0 || ib < 0 || ig < 0 || ia > ib || strings.LastIndex(outTwo, "local: b") > ig {
		t.Errorf("console groups out of plan order:\n%s", outTwo)
	}
	_ = outOne
}

// localJobLimit: explicit jobs win; zero is CPUs over Go parallelism, at least one.
func TestLocalJobLimit(t *testing.T) {
	for _, row := range []struct{ jobs, parallel, cpus, want int }{
		{0, 4, 20, 5}, {0, 4, 4, 1}, {0, 8, 20, 2}, {3, 4, 20, 3},
	} {
		if got := localJobLimit(row.jobs, row.parallel, row.cpus); got != row.want {
			t.Errorf("localJobLimit(%d,%d,%d) = %d, want %d", row.jobs, row.parallel, row.cpus, got, row.want)
		}
	}
}

// A negative jobs count refuses before any filesystem work.
func TestLocalNegativeJobsRefused(t *testing.T) {
	repo := newLocalFixture(t)
	marker := filepath.Join(t.TempDir(), "marker-ran")
	opts := localRunOptions(repo, localFixturePlan(`printf ran > "$MARKER"`), filepath.Join(t.TempDir(), "record.json"))
	opts.Jobs = -1
	opts.Env = []string{"MARKER=" + marker}
	if _, _, err := localVerify(opts, "", io.Discard); err == nil || !strings.Contains(err.Error(), "jobs must be zero") {
		t.Fatalf("verify: %v, want the negative-jobs refusal", err)
	}
	if _, err := os.Lstat(marker); err == nil {
		t.Errorf("the refused run still ran a step")
	}
	if ent, err := os.ReadDir(opts.WorkRoot); err == nil && len(ent) > 0 {
		t.Errorf("the refused run left %d entries under the work root", len(ent))
	}
}

// --keep preserves the workers' roots: the job worktrees and their private caches are left under
// the kept run root, and the test removes them itself through git worktree remove (no prune).
func TestLocalParallelKeepLeavesTheWorkspaces(t *testing.T) {
	repo := newLocalFixture(t)
	srv := newParallelServer(t)
	opts := localRunOptions(repo, parallelPlan(srv.helperStep, "a", "b"), filepath.Join(t.TempDir(), "record.json"))
	opts.Jobs = 2
	opts.Keep = true
	finished, made, verr := awaitVerify(t, opts, srv)
	srv.arrive(t)
	srv.arrive(t)
	srv.closeRelease()
	<-finished
	if *verr != nil {
		t.Fatal(*verr)
	}
	home := made.RunEnv.Home
	temp := filepath.Dir(home)
	// kept run root holds every worker's worktree and private cache
	for i := 0; i < 2; i++ {
		tree := filepath.Join(temp, fmt.Sprintf("job-%03d", i), "tree")
		if _, err := os.Lstat(tree); err != nil {
			t.Errorf("the kept run lost %s: %v", tree, err)
		}
		if _, err := os.Lstat(filepath.Join(temp, fmt.Sprintf("job-%03d", i), "gocache")); err != nil {
			t.Errorf("the kept run lost the job's gocache: %v", err)
		}
	}
	// this test owns the cleanup: the kept run leaves the job worktrees AND the coordinator's
	// registered, so remove each through git, then the run root
	for i := 0; i < 2; i++ {
		runGit(repo.root, "worktree", "remove", "--force", filepath.Join(temp, fmt.Sprintf("job-%03d", i), "tree"))
	}
	runGit(repo.root, "worktree", "remove", "--force", filepath.Join(temp, "tree"))
	os.RemoveAll(temp)
	os.RemoveAll(made.RunEnv.TmpDir)
	out, werr := runGit(repo.root, "worktree", "list", "--porcelain")
	if werr != nil || strings.Count(string(out), "worktree ") != 1 {
		t.Errorf("worktrees still registered (err %v):\n%s", werr, out)
	}
}

// A worker's checkout must pass the same clean-tree check the coordinator's does: a smudge filter
// that fires only under a job worktree dirties it, the job records the failure, and the run still
// removes the clone's git registration (CRW-1584).
func TestLocalParallelDirtyWorkerCheckoutFailsAndIsRemoved(t *testing.T) {
	// the caller's TMPDIR sits below a job-011 ancestor: the filter must judge the worktree's own
	// directory, not an ancestor of the caller's paths
	callerTmp := filepath.Join(t.TempDir(), "job-011")
	if err := os.MkdirAll(callerTmp, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", callerTmp)
	repo := newLocalFixture(t)
	// the filter smudges file.txt only when the checkout itself is a job-NNN/tree
	runGitOrFail(t, repo.root, "config", "filter.worker.clean", "tr A-Z a-z")
	runGitOrFail(t, repo.root, "config", "filter.worker.smudge", `case "${PWD%/*}" in */job-[0-9][0-9][0-9]) tr a-z A-Z ;; *) cat ;; esac`)
	repo.write(".gitattributes", "file.txt filter=worker\n")
	repo.commit()
	srv := newParallelServer(t)
	opts := localRunOptions(repo, parallelPlan(srv.helperStep, "a", "b"), filepath.Join(t.TempDir(), "record.json"))
	opts.Jobs = 2
	finished, made, verr := awaitVerify(t, opts, srv)
	srv.closeRelease()
	<-finished
	if *verr != nil {
		t.Fatal(*verr)
	}
	if len(made.Jobs) != 2 || made.Result != localFail {
		t.Fatalf("result %q jobs %+v, want a failed run of the two jobs", made.Result, made.Jobs)
	}
	for _, job := range made.Jobs {
		if job.Result != localFail || len(job.Steps) == 0 || job.Steps[0].Result != localFailed ||
			!strings.Contains(job.Steps[0].Reason, "clean worktree differs") {
			t.Errorf("job %q steps %+v, want the checkout step failed with the dirty reason", job.Name, job.Steps)
		}
	}
	select {
	case r := <-srv.arrivals:
		t.Errorf("a helper ran despite the failed checkout: %v", r)
	default:
	}
	out, werr := runGit(repo.root, "worktree", "list", "--porcelain")
	if werr != nil || strings.Count(string(out), "worktree ") != 1 {
		t.Errorf("the failed worker worktree stayed registered (err %v):\n%s", werr, out)
	}
}

// One job still gets a private Go cache that exists before the step runs, and GOTMPDIR stays
// unset so its build and test temporaries use the job TMPDIR (CRW-1584).
func TestLocalSingleJobHasPrivateCaches(t *testing.T) {
	repo := newLocalFixture(t)
	opts := localRunOptions(repo, localFixturePlan(`test -d "$GOCACHE" && test -z "${GOTMPDIR+x}"`), filepath.Join(t.TempDir(), "record.json"))
	opts.Jobs = 1
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localPass || len(made.Jobs) != 1 || made.Jobs[0].Result != localPassed {
		t.Fatalf("single job did not pass: %+v", made.Jobs)
	}
}
