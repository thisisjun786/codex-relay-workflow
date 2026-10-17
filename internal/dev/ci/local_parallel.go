//go:build dev

package ci

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

// localJobEnv seals the private cache and lets Go build/test temporaries follow the job's TMPDIR.
// Go 1.27 testing.T.TempDir prefers GOTMPDIR even when a test changes TMPDIR, so a persistent
// override would bypass the suite's temporary-root isolation and path normalization.
func localJobEnv(env []string, cache string) []string {
	out := crwconfig.SetEnv(env, "GOCACHE", cache) // SetEnv returns a copy.
	return slices.DeleteFunc(out, func(kv string) bool { return strings.HasPrefix(kv, "GOTMPDIR=") })
}

// localJobLimit is the number of job/legs running at once: an explicit jobs, else the host's CPUs
// divided by the Go test parallelism each step already gets (CRW-1584).
func localJobLimit(jobs, parallel, cpus int) int {
	if jobs > 0 {
		return jobs
	}
	if parallel <= 0 {
		parallel = 4
	}
	return max(1, cpus/parallel)
}

// localCPUOptions resolves a budgeted run's job/parallel split and the recordCPU the record
// carries (CRW-1591). Zero CPUs is the legacy unbudgeted behavior and changes nothing. The jobs
// the budget feeds is min(cpus, the usual job limit), each job gets cpus/jobs of the budget, and a
// package parallelism above that is clamped per job. The method is the resolved backend; the
// fallback additionally names the per-package GOMAXPROCS the sealed environment sets.
func localCPUOptions(opts localOptions) (localOptions, error) {
	if opts.CPUs < 0 {
		return opts, fmt.Errorf("cpus must be positive")
	}
	if opts.CPUs == 0 {
		return opts, nil
	}
	if opts.Jobs < 0 {
		return opts, fmt.Errorf("jobs must be zero (automatic) or positive")
	}
	parallel := opts.Parallel
	if parallel <= 0 {
		parallel = 4
	}
	jobs := min(opts.CPUs, localJobLimit(opts.Jobs, parallel, opts.CPUs))
	perJob := max(1, opts.CPUs/jobs)
	opts.Jobs, opts.Parallel = jobs, min(parallel, perJob)
	method := opts.cpuMethod
	if method != localCPUCgroup {
		method = localCPUFallback
	}
	opts.cpu = &recordCPU{Limit: opts.CPUs, Method: method, Jobs: jobs, Parallel: opts.Parallel}
	if method == localCPUFallback {
		// Each concurrently running Go package gets its own GOMAXPROCS, so the per-package
		// share keeps jobs*parallel*GOMAXPROCS inside the budget.
		opts.cpu.GOMAXPROCS = max(1, perJob/opts.Parallel)
	}
	return opts, nil
}

// localTask is one leg of one job flattened in plan order; a job with no legs is one empty leg.
type localTask struct {
	job   localJob
	leg   string
	name  string
	index int
}

// localTaskResult is a worker's complete answer: the job record and its console group. A worker
// never touches the shared record or stdout.
type localTaskResult struct {
	job    recordJob
	tests  localTests
	output bytes.Buffer
}

// localExecuteTask is the old serial loop of one job's steps, run for one task in its own
// workspace (CRW-1584): the steps stay serial inside the task, an N/A answers before a failure's
// skip like the serial loop did, and a setup error fails the first applicable step and skips the
// later ones rather than losing or passing the record.
func localExecuteTask(opts localOptions, task localTask, worktree string, env []string, done []recordJob, setupErr error) localTaskResult {
	var res localTaskResult
	res.job.Name = task.name
	failed := false
	lastFresh := localLastFreshStep(task)
	for _, step := range task.job.steps {
		if step.legs != nil && !localContains(step.legs, task.leg) {
			continue
		}
		index := len(res.job.Steps)
		entry := recordStep{Job: task.name, Name: step.name, Command: step.command, Scope: step.scope}
		if entry.Name == "" {
			entry.Name = localStepLabel(step)
		}
		start := time.Now()
		offset := localOutputSize(opts.output)
		switch {
		case step.kind == localNotApplicable:
			entry.Result, entry.Reason = localNotApplicableResult, step.note
		case setupErr != nil && !failed:
			failed = true
			entry.Result, entry.Reason = localFailed, "prepare job: "+setupErr.Error()
		case failed:
			// GitHub stops a job's later steps once one fails; the local run does the same
			// and records the rest as skipped, which is itself a failure (answer 5).
			entry.Result, entry.Reason = localSkipped, "an earlier step of this job did not pass"
		case opts.reuse != nil && index > lastFresh && step.scope == "full" && step.action != localAggregate:
			// CRW-1590: a full step past the fresh prefix answers from the validated source record.
			entry = localMarkReused(opts.reuse.record.Jobs[task.index].Steps[index], opts.reuse, task.name, index)
		default:
			var tests localTests
			entry.Result, entry.Reason, tests = localRunStep(opts, step, worktree, task.leg, env, done)
			if tests.saw {
				res.tests.saw = true
				res.tests.ran += tests.ran
			}
			if entry.Result != localPassed {
				failed = true
			}
		}
		if entry.ReusedFrom == nil {
			entry.Decision = localDecisionSince(opts.output, offset)
			entry.Seconds = time.Since(start).Seconds()
		}
		res.job.Steps = append(res.job.Steps, entry)
		fmt.Fprintf(&res.output, "local: %-26s %-44s %s\n", task.name, entry.Name, entry.Result)
	}
	res.job.Result = localPassed
	for _, step := range res.job.Steps {
		if step.Result != localPassed && step.Result != localNotApplicableResult {
			res.job.Result = localFail
			break
		}
	}
	return res
}

// localTaskWorkspace is one task's private roots: its own worktree (cloned and verified like the
// coordinator's), home, step TMPDIR, runner output and build caches (CRW-1584).
type localTaskWorkspace struct {
	worktree string
	home     string
	tmp      string
	opts     localOptions
	env      []string
}

// localMakeTaskWorkspace gives a multi-job task its own worktree, home, step TMPDIR, output and
// Go caches, all under the run's roots; the git administration runs under addMu.
func localMakeTaskWorkspace(opts localOptions, task localTask, temp, coordHome string, addMu *sync.Mutex) (*localTaskWorkspace, error) {
	taskDir := filepath.Join(temp, fmt.Sprintf("job-%03d", task.index))
	ws := &localTaskWorkspace{
		worktree: filepath.Join(taskDir, "tree"),
		home:     filepath.Join(coordHome, fmt.Sprintf("job-%03d", task.index)),
		tmp:      filepath.Join(opts.stepTmp, fmt.Sprintf("job-%03d", task.index)),
	}
	for _, dir := range []string{taskDir, ws.home, ws.tmp, filepath.Join(taskDir, "runner"), filepath.Join(taskDir, "gocache")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	addMu.Lock()
	_, addErr := runGit(opts.Root, "-c", "core.hooksPath=/dev/null", "worktree", "add", "--detach", ws.worktree, opts.Commit)
	addMu.Unlock()
	if addErr != nil {
		return nil, addErr
	}
	// The same clean-checkout judgment as the coordinator's tree: attributes and filters must not
	// change the checked-out bytes.
	if status, err := runGit(ws.worktree, "status", "--porcelain"); err != nil {
		return nil, err
	} else if dirty := strings.TrimSpace(string(status)); dirty != "" {
		return nil, fmt.Errorf("the clean worktree differs from the commit:%s", dirty)
	} else if diff := localCheckoutDiff(ws.worktree, opts.Commit); diff != "" {
		return nil, fmt.Errorf("the clean worktree differs from the commit: %s", diff)
	}
	ws.opts = opts
	ws.opts.output = filepath.Join(taskDir, "runner", "output")
	ws.opts.stepTmp = ws.tmp
	ws.opts.Tools.BuildCache = filepath.Join(taskDir, "gocache")
	ws.opts.Tools.BuildWork = ""
	ws.opts.Tools.ModuleCache = ""
	env, err := localStepEnv(ws.home, taskDir, ws.opts)
	if err != nil {
		return nil, err
	}
	// The task's cache wins over an inherited or configured GOCACHE; GOTMPDIR is dropped so build
	// and test temporaries stay under the task's own TMPDIR (CRW-1584).
	env = localJobEnv(env, ws.opts.Tools.BuildCache)
	env = crwconfig.SetEnv(env, "TMPDIR", ws.tmp)
	ws.env = env
	return ws, nil
}

// localRunTasks runs a batch of tasks on a bounded worker pool and hands each ordered result to
// consume as it becomes available in plan order (CRW-1584): the coordinator alone appends the
// record and writes stdout, so groups print per job, ordered, and never interleave.
func localRunTasks(batch []localTask, limit int, run func(localTask) localTaskResult, consume func(localTaskResult)) {
	queue := make(chan int, len(batch))
	answers := make([]chan localTaskResult, len(batch))
	for i := range batch {
		answers[i] = make(chan localTaskResult, 1)
		queue <- i
	}
	close(queue)
	var wg sync.WaitGroup
	for w := 0; w < min(limit, len(batch)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				answers[i] <- run(batch[i])
			}
		}()
	}
	for i := range batch {
		consume(<-answers[i])
	}
	wg.Wait()
}
