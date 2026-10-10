//go:build dev

package ci

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/homeguard"
)

// CRW-964: `crw-dev ci local`. It runs the local step table in a clean worktree of the
// commit being verified, with an isolated home and UTC, and writes a verification-record/1. The
// clean tree is what makes the result the commit's rather than the caller's working state, and the
// record is what makes it reusable when nothing that decides it has changed.

const (
	// localRecordDefault is where the record is written when --record is not given.
	localRecordDefault = ".codexclaw/ci-local/verification-record.json"
	// localHeavyGateEnv names the host's heavy-check gate. When it is set, a heavy step runs as
	// <gate> <command>; when it is unset the command runs directly. No host path is ever written
	// into the repository.
	localHeavyGateEnv = "CRW_CI_HEAVY_GATE"
	// localTempPrefix names the temporary root a run makes for its worktree and its home.
	localTempPrefix = "crw-ci-local-"
)

// localOptions is one local run's inputs. Plan is the table (the default is localPlan); tests pass
// a smaller one.
type localOptions struct {
	Root      string
	Commit    string
	Base      string
	Record    string
	Runner    string
	HeavyGate string
	Keep      bool
	// WorkRoot is the directory the clean worktree is made in; it may not be inside TMPDIR, /tmp or /var/tmp.
	WorkRoot string
	// Parallel is the Go test parallelism every step runs with (GOFLAGS=-p=N); 0 means 4.
	Parallel int
	// allowTempRoot lets a test work root sit under TMPDIR. Only tests set it; the command never does.
	allowTempRoot bool
	Plan          []localJob
	// Env is extra environment for every step, after the isolated home and TZ (tests use it).
	Env []string
	// output is the file the changed-path decision writes its answer to, set once per run.
	output string
	// script counts the step scripts written under the run's temporary root.
	script int
	// hostOS and hostArch name the platform the run judges itself on; empty is this host. Only tests set them.
	hostOS, hostArch string
	// buildInfo reports the build record of the running binary; nil means runtime/debug.ReadBuildInfo. Only tests set it.
	buildInfo func() (*debug.BuildInfo, bool)
}

// platform is the operating system and architecture the run records and judges itself on.
func (o localOptions) platform() (string, string) {
	goos, goarch := o.hostOS, o.hostArch
	if goos == "" {
		goos = localHostOS()
	}
	if goarch == "" {
		goarch = localHostArch()
	}
	return goos, goarch
}

// Local is `crw-dev ci local`: run every ci.yml job and step locally, or install the
// pre-push hook. `crw-dev ci local hook install|status` is the hook subcommand.
func Local(args []string, stdout, stderr io.Writer) int {
	localScrubGitEnv()
	if len(args) > 0 && args[0] == "hook" {
		return localHook(args[1:], stdout, stderr)
	}
	flags := newFlags("local")
	commit := flags.String("commit", "HEAD", "the commit to verify")
	base := flags.String("base", "", "the base commit the record names (default: merge-base with origin/dev)")
	record := flags.String("record", localRecordDefault, "where to write the verification record")
	reuse := flags.String("reuse", "", "answer this record instead of running, when every key matches")
	heavyGate := flags.String("heavy-gate", "", "the heavy-check gate to run heavy steps through (default $"+localHeavyGateEnv+")")
	runner := flags.String("runner", "local", "where this run happened; the record names it")
	keep := flags.Bool("keep", false, "keep the clean worktree for debugging")
	workRoot := flags.String("work-root", "", "the directory the clean worktree is made in (default: the XDG state directory); never inside TMPDIR, /tmp or /var/tmp")
	parallel := flags.Int("parallel", 4, "the Go test parallelism every step runs with (GOFLAGS=-p=N)")
	description := "Run every job and step of .github/workflows/ci.yml locally, in a clean worktree of the\n" +
		"commit being verified, and write a verification-record/1. The record is answered without\n" +
		"running again only when the tree, the ci.yml digest, the tool versions, the dependency\n" +
		"digests and the OS and architecture all match."
	if code := parseFlags(flags, description, args, stdout, stderr); code >= 0 {
		return code
	}
	root, err := repositoryRoot()
	if err != nil {
		return failf(stderr, "local: %s", err)
	}
	opts := localOptions{
		Root:      root,
		Commit:    *commit,
		Base:      *base,
		Record:    *record,
		Runner:    *runner,
		HeavyGate: *heavyGate,
		Keep:      *keep,
		WorkRoot:  *workRoot,
		Parallel:  *parallel,
	}
	if opts.Record == "" {
		opts.Record = filepath.Join(root, localRecordDefault)
	} else if !filepath.IsAbs(opts.Record) {
		opts.Record = filepath.Join(root, opts.Record)
	}
	if opts.HeavyGate == "" {
		opts.HeavyGate = os.Getenv(localHeavyGateEnv)
	}
	reusePath := ""
	if *reuse != "" {
		reusePath = localAbs(root, *reuse)
	}
	made, reused, err := localVerify(opts, reusePath, stdout)
	if err != nil {
		return failf(stderr, "local: %s", err)
	}
	if reused {
		fmt.Fprintf(stdout, "local: reused %s (%s)\n", *reuse, made.Digest)
		return 0
	}
	sealed, err := writeRecord(opts.Record, made)
	if err != nil {
		return failf(stderr, "local: %s", err)
	}
	if sealed.Result == localPass {
		fmt.Fprintf(stdout, "local: pass at %s (%s)\n", sealed.HeadCommit, sealed.Digest)
		return 0
	}
	fmt.Fprintf(stderr, "local: fail at %s (%s)\n", sealed.HeadCommit, sealed.Digest)
	for _, job := range made.Jobs {
		for _, step := range job.Steps {
			if step.Result != localPassed && step.Result != localNotApplicableResult {
				fmt.Fprintf(stderr, "  %s / %s: %s (%s)\n", job.Name, step.Name, step.Result, step.Reason)
			}
		}
	}
	return 1
}

// localVerify answers a reused record when every key matches, or runs the table and returns the
// record it made.
func localVerify(opts localOptions, reusePath string, stdout io.Writer) (verificationRecord, bool, error) {
	// CRW-1186: what the run writes outside the clean worktree is the record (and its siblings) and the
	// probes' directories in TMPDIR; neither lands in the account's real home, and nothing is made
	// before both are known. A work root is judged again where it is made (localWorkRoot).
	if opts.Record != "" {
		if err := localCheckRecordDestination(opts.Root, opts.Record); err != nil {
			return verificationRecord{}, false, err
		}
	}
	if err := homeguard.Refuse(localTempDir()); err != nil {
		return verificationRecord{}, false, fmt.Errorf("TMPDIR: %w", err)
	}
	localScrubGitEnv()
	opts.HeavyGate = localAbsGate(opts.HeavyGate)
	// Replacement refs (refs/replace) would let another object stand in for a commit; the run reads the
	// objects as they are.
	os.Setenv("GIT_NO_REPLACE_OBJECTS", "1")
	plan := opts.Plan
	if plan == nil {
		plan = localPlan()
	}
	current, err := localCurrentKeys(opts)
	if err != nil {
		return verificationRecord{}, false, err
	}
	// CRW-1025: the plan is compiled from the caller's engine source, so a run from an engine that is not the
	// verified commit's would record that commit's pass for steps it does not define. Refused before any step
	// runs and before any record is reused.
	if differs, err := localEngineDiffers(opts.Root, current.HeadCommit); err != nil {
		return verificationRecord{}, false, err
	} else if differs != "" {
		return verificationRecord{}, false, fmt.Errorf("engine_differs_from_commit: the engine source (%s) differs from %s; run from a checkout of that commit", differs, shortHash(current.HeadCommit))
	}
	// CRW-1025: a prebuilt binary carries the plan and the executor it was compiled from, which a clean
	// checkout does not describe; the build record it was stamped with must name the verified engine.
	if differs, err := localBinaryDiffers(opts, current.HeadCommit); err != nil {
		return verificationRecord{}, false, err
	} else if differs != "" {
		return verificationRecord{}, false, fmt.Errorf("engine_differs_from_commit: the running crw-dev binary %s; build it from a clean checkout of %s (make crw-dev) or run go run -tags dev ./cmd/crw-dev ci local", differs, shortHash(current.HeadCommit))
	}
	// CRW-1027 (merged into CRW-1025): the secrets step is linux x64 only, so a plan that carries it is refused
	// on any other host before a step runs or a record is reused.
	if err := localPlatformRefusal(plan, opts); err != nil {
		return verificationRecord{}, false, err
	}
	if reusePath != "" {
		// A record path that does not exist is a usage error, not a silent full run: the caller
		// asked for that record.
		reused, err := readRecord(reusePath)
		if err != nil {
			return verificationRecord{}, false, fmt.Errorf("--reuse: %w", err)
		}
		ok, why := localReuse(reused, current)
		if ok {
			ok, why = localValidateReuse(reused, plan)
		}
		if ok {
			ok, why = localRecomputedReuse(opts, plan, current)
		}
		if ok {
			// The record answers: write it to --record so the caller finds it where it asked.
			if _, err := writeRecord(opts.Record, reused); err != nil {
				return verificationRecord{}, false, err
			}
			fmt.Fprintf(stdout, "local: every key matches (%s); %s answers this tree\n", why, reusePath)
			return reused, true, nil
		}
		fmt.Fprintf(stdout, "local: %s does not answer this tree: %s\n", reusePath, why)
	}
	// The workflow-coverage refusal guards the real table: a step ci.yml grows cannot go
	// unverified. An injected table (tests) owns its own coverage. The workflow is read from the
	// verified commit, not the caller's working tree, so a dirty edit to ci.yml cannot change what
	// this run is held to.
	if opts.Plan == nil {
		if err := checkWorkflowAt(opts.Root, current.HeadCommit, plan); err != nil {
			return verificationRecord{}, false, err
		}
	}
	return localExecute(opts, plan, current, stdout)
}

// localCurrentKeys is the state a record is judged against: the tree, the ci.yml digest, the tool
// versions, the dependency digests and the platform. It is computed from the verified commit, not
// from the caller's working tree, so a dirty checkout does not change it (comment-c6).
func localCurrentKeys(opts localOptions) (verificationRecord, error) {
	hostOS, hostArch := opts.platform()
	head, err := localRev(opts.Root, opts.Commit+"^{commit}")
	if err != nil {
		return verificationRecord{}, err
	}
	tree, err := localRev(opts.Root, head+"^{tree}")
	if err != nil {
		return verificationRecord{}, err
	}
	ciDigest, err := localBlobDigest(opts.Root, head, ".github/workflows/ci.yml")
	if err != nil {
		return verificationRecord{}, err
	}
	dependencies := map[string]string{}
	for _, name := range []string{"go.sum", "web/package-lock.json"} {
		digest, err := localBlobDigest(opts.Root, head, name)
		if err != nil {
			return verificationRecord{}, err
		}
		dependencies[name] = digest
	}
	// The range the blob and secret steps judge is base..head, so the base is part of what the
	// record covers: a different base is a different verification even at the same tree.
	base := opts.Base
	if base == "" {
		base, err = localDefaultBase(opts.Root, head)
		if err != nil {
			return verificationRecord{}, err
		}
	}
	// The base is recorded as the full commit it names, however it was given.
	if base != "" {
		base, err = localRev(opts.Root, base+"^{commit}")
		if err != nil {
			return verificationRecord{}, err
		}
	}
	// The commits between base and head are the input the blob and secret steps judge, so the record
	// names them: a different commit list is a different verification even at the same tree.
	commits, err := runGit(opts.Root, "rev-list", base+".."+head)
	if err != nil {
		return verificationRecord{}, err
	}
	rangeSum := sha256.Sum256(commits)
	// The Go probe runs in a module holding the commit's go.mod, so it reports the toolchain the steps select.
	goMod, _ := runGit(opts.Root, "show", head+":go.mod")
	pins, err := localToolPinsFrom(func(path string) ([]byte, error) { return runGit(opts.Root, "show", head+":"+path) })
	if err != nil {
		return verificationRecord{}, err
	}
	return verificationRecord{
		Schema:       recordSchema,
		Runner:       opts.Runner,
		Range:        fmt.Sprintf("sha256:%x", rangeSum),
		Repository:   localRepository(opts.Root),
		BaseCommit:   base,
		HeadCommit:   head,
		TreeHash:     tree,
		CiDigest:     ciDigest,
		Tools:        localObservedVersions(localToolVersionsIn(localPathEnv(opts.Env), goMod, opts.HeavyGate), pins),
		GoFlags:      localEngineFlags(opts.Parallel),
		HeavyGate:    localGateDigest(opts.HeavyGate),
		GoEnv:        localIsolatedGoEnv,
		Dependencies: dependencies,
		OS:           hostOS,
		Arch:         hostArch,
	}, nil
}

// localEngineFlags is the GOFLAGS every step runs with: the engine's own parallelism. The caller's
// GOFLAGS never reach a step, so the record names the engine's value.
func localEngineFlags(parallel int) string {
	if parallel <= 0 {
		parallel = 4
	}
	return fmt.Sprintf("-p=%d", parallel)
}

// localRepository is the origin remote's URL, the identity a record names. It is read from the
// checkout rather than written down, so the record travels with the repository it describes.
func localRepository(root string) string {
	out, err := runGit(root, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return localWithoutUserinfo(strings.TrimSpace(string(out)))
}

// localWithoutUserinfo removes the credentials from a remote address before the record names it: the user,
// password and query of a URL, or the user@ of an scp-style address (pre-merge finding d1). A URL that
// url.Parse rejects, such as one whose password holds a % or a space that git accepts verbatim, is cut
// textually at its last @; when the text before that @ holds a slash, the boundary is unknown and the
// name is empty rather than the raw remote.
func localWithoutUserinfo(remote string) string {
	if u, err := url.Parse(remote); err == nil && u.Scheme != "" && u.Host != "" {
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		return u.String()
	}
	if i := strings.Index(remote, "://"); i >= 0 {
		rest := remote[i+len("://"):]
		at := strings.LastIndex(rest, "@")
		tail := rest
		if at >= 0 {
			if strings.Contains(rest[:at], "/") {
				return ""
			}
			tail = rest[at+1:]
		}
		if cut := strings.IndexAny(tail, "?#"); cut >= 0 {
			tail = tail[:cut]
		}
		return remote[:i+len("://")] + tail
	}
	// An scp-style address is [user@]host:path; without the colon the text is a local path and keeps its @.
	if colon := strings.Index(remote, ":"); colon >= 0 && !strings.Contains(remote[:colon], "/") {
		if at := strings.LastIndex(remote[:colon], "@"); at >= 0 {
			return remote[at+1:]
		}
	}
	return remote
}

// localScrubGitEnv removes the caller's git repository and configuration variables from this process, so no
// git command the run makes reads another repository, index or configuration (review finding, P2).
func localScrubGitEnv() {
	for _, entry := range os.Environ() {
		if name, _, _ := strings.Cut(entry, "="); strings.HasPrefix(name, "GIT_") {
			os.Unsetenv(name)
		}
	}
}

// localExecute runs the table in a clean worktree of the verified commit and assembles the record.
func localExecute(opts localOptions, plan []localJob, current verificationRecord, stdout io.Writer) (verificationRecord, bool, error) {
	root, err := localWorkRoot(opts)
	if err != nil {
		return verificationRecord{}, false, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return verificationRecord{}, false, err
	}
	temp, err := os.MkdirTemp(root, localTempPrefix)
	if err != nil {
		return verificationRecord{}, false, err
	}
	worktree := filepath.Join(temp, "tree")
	home := filepath.Join(temp, "home")
	cleanup := func() {
		if opts.Keep {
			fmt.Fprintf(stdout, "local: kept %s\n", temp)
			return
		}
		// Remove this run's own worktree through git, then the whole root. No repository-wide prune runs:
		// it would also drop the administrative state of other tasks' worktrees.
		runGit(opts.Root, "worktree", "remove", "--force", worktree)
		os.RemoveAll(temp)
	}
	defer cleanup()
	checkoutErr := ""
	if out, err := runGit(opts.Root, "-c", "core.hooksPath=/dev/null", "worktree", "add", "--detach", worktree, current.HeadCommit); err != nil {
		checkoutErr = fmt.Sprintf("%v%s", err, localReason(string(out)))
	} else if status, err := runGit(worktree, "status", "--porcelain"); err != nil {
		checkoutErr = fmt.Sprintf("%v%s", err, localReason(string(status)))
	} else if dirty := strings.TrimSpace(string(status)); dirty != "" {
		// Attributes or filters changed the checked-out content, so the tests would not be the commit's.
		checkoutErr = "the clean worktree differs from the commit" + localReason(dirty)
	} else if diff := localCheckoutDiff(worktree, current.HeadCommit); diff != "" {
		checkoutErr = "the clean worktree differs from the commit: " + diff
	}
	if checkoutErr != "" {
		// A checkout that fails is a failed record, named with its reason, not a missing one.
		failed := current
		failed.Sealed = true
		failed.PlanDigest = planDigest(plan)
		failed.IgnoredEnv = localIgnoredEnv(os.Environ())
		failed.Result = localFail
		failed.Jobs = []recordJob{{Name: "clean worktree", Result: localFail, Steps: []recordStep{{Job: "clean worktree", Name: "git worktree add", Scope: "full", Result: localFailed, Reason: checkoutErr}}}}
		return failed, false, nil
	}
	opts.output = filepath.Join(temp, "runner", "output")
	env, err := localStepEnv(home, temp, opts)
	if err != nil {
		return verificationRecord{}, false, err
	}
	pins, err := localToolPinsFrom(func(path string) ([]byte, error) { return os.ReadFile(filepath.Join(worktree, path)) })
	if err != nil {
		return verificationRecord{}, false, err
	}
	record := current
	record.Sealed = true
	record.PlanDigest = planDigest(plan)
	record.IgnoredEnv = localIgnoredEnv(os.Environ())
	record.Pins = pins
	record.Tools = localObservedVersions(current.Tools, pins)
	workflowData, err := os.ReadFile(filepath.Join(worktree, ".github", "workflows", "ci.yml"))
	if err != nil {
		return verificationRecord{}, false, err
	}
	workflowJobs, err := parseWorkflow(string(workflowData))
	if err != nil {
		return verificationRecord{}, false, err
	}
	// The record's node pin is the distinct setup-node pins of the workflow; the mismatch is per job.
	var nodePins []string
	for _, job := range workflowJobs {
		for _, step := range job.steps {
			if step.nodeVersion != "" {
				nodePins = append(nodePins, step.nodeVersion)
			}
		}
	}
	if nodes := localSortedUnique(nodePins); len(nodes) > 0 {
		pins["node"] = strings.Join(nodes, ",")
	}
	record.PinMismatch = localSortedUnique(append(localPinMismatch(pins, record.Tools), localNodeMismatch(plan, workflowJobs, record.Tools["node"])...))
	// The steps read the same base the record names (the blob and secret range) and the commit
	// itself (the changed-path decisions), so the environment carries the resolved values rather
	// than the caller's flags.
	opts.Base = record.BaseCommit
	opts.Commit = current.HeadCommit
	if len(record.PinMismatch) > 0 {
		fmt.Fprintf(stdout, "local: pin mismatch (%s); this record will not be reused\n", strings.Join(record.PinMismatch, ", "))
	}

	for _, job := range plan {
		legs := job.legs
		if legs == nil {
			legs = []string{""}
		}
		for _, leg := range legs {
			name := job.name
			if leg != "" {
				name = job.name + " (" + leg + ")"
			}
			recorded := recordJob{Name: name}
			failed := false
			for _, step := range job.steps {
				if step.legs != nil && !localContains(step.legs, leg) {
					continue
				}
				entry := recordStep{Job: name, Name: step.name, Command: step.command, Scope: step.scope}
				if entry.Name == "" {
					entry.Name = localStepLabel(step)
				}
				start := time.Now()
				offset := localOutputSize(opts.output)
				switch {
				case step.kind == localNotApplicable:
					entry.Result, entry.Reason = localNotApplicableResult, step.note
				case failed:
					// GitHub stops a job's later steps once one fails; the local run does the same
					// and records the rest as skipped, which is itself a failure (answer 5).
					entry.Result, entry.Reason = localSkipped, "an earlier step of this job did not pass"
				default:
					entry.Result, entry.Reason = localRunStep(opts, step, worktree, leg, env, record.Jobs)
					if entry.Result != localPassed {
						failed = true
					}
				}
				entry.Decision = localDecisionSince(opts.output, offset)
				entry.Seconds = time.Since(start).Seconds()
				recorded.Steps = append(recorded.Steps, entry)
				fmt.Fprintf(stdout, "local: %-26s %-44s %s\n", name, entry.Name, entry.Result)
			}
			recorded.Result = localPassed
			for _, step := range recorded.Steps {
				if step.Result != localPassed && step.Result != localNotApplicableResult {
					recorded.Result = localFail
					break
				}
			}
			record.Jobs = append(record.Jobs, recorded)
		}
	}
	record.Result = localPass
	for _, job := range record.Jobs {
		if job.Result != localPassed {
			record.Result = localFail
			break
		}
	}
	return record, false, nil
}

// localRunStep performs one step and returns its result and reason. done is the record's jobs so
// far, which the aggregate step judges.
func localRunStep(opts localOptions, step localStep, worktree, leg string, env []string, done []recordJob) (string, string) {
	if step.tool != "" && !localToolPresent(step.tool, localPathEnv(env)) {
		return localMissingTool, step.tool + " is not on PATH"
	}
	switch step.action {
	case localCheckout:
		return localPassed, "the clean worktree of " + shortHash(opts.Commit)
	case localGo, localNode:
		return localPassed, step.tool + " " + localObserveTool(step.tool, localPathEnv(env), opts.HeavyGate)
	case localAggregate:
		// dev-gate: every prerequisite job must have succeeded, judged from the jobs already
		// recorded rather than from a hosted needs object.
		for _, job := range done {
			if job.Result != localPassed {
				return localFailed, "prerequisite job " + job.Name + " did not pass"
			}
		}
		return localPassed, "every prerequisite job succeeded"
	}
	command := step.command
	dir := worktree
	if step.workdir != "" {
		dir = filepath.Join(worktree, step.workdir)
	}
	stepEnv := append(append([]string{}, env...), localStepEnvValues(step.env, opts, leg)...)
	// The command runs as a script file through bash, so a ci.yml run text that is a block scalar
	// (a loop, a heredoc) runs as the runner runs it, and a gate between this process and bash
	// never sees the command's own characters. systemd-run, for one, expands the arguments it is
	// handed, which would otherwise mangle a run text that uses a shell variable.
	script, err := localWriteScript(opts, command)
	if err != nil {
		return localFailed, err.Error()
	}
	argv := localGateArgv(opts.HeavyGate, step.heavy, script, dir, stepEnv)
	shell := exec.Command(argv[0], argv[1:]...)
	shell.Dir = dir
	shell.Env = stepEnv
	var output bytes.Buffer
	shell.Stdout, shell.Stderr = &output, &output
	if err := shell.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			reason := fmt.Sprintf("exit status %d%s", exit.ExitCode(), localReason(output.String()))
			if strings.Contains(output.String(), localTimeoutMark) {
				reason = "timeout: " + reason
			}
			return localFailed, reason
		}
		return localFailed, err.Error()
	}
	return localPassed, ""
}

// localIsolatedGoEnv is the record's goEnv: GOENV is unset in a step, so Go reads its default file
// under the run's own XDG_CONFIG_HOME, which is empty.
const localIsolatedGoEnv = "unset: Go reads its default file under the run's own XDG_CONFIG_HOME"

// localGateArgv is the argv a step runs: bash <script>, with the heavy-check gate's own words
// prepended for a heavy step. The step's command lives in the script file, so a gate between this
// process and bash never sees the command's own characters.
// localChdirScript starts the step in its directory and replaces the shell with the step's own bash.
const localChdirScript = `cd "$1" && shift && exec "$@"`

// localGateArgv is the argv a step runs. A heavy step runs through the heavy-check gate, and the step's
// directory and sealed environment are part of that argv (env -i, then the directory, then bash), so no
// launcher of the gate can start the step elsewhere or with variables of its own (pre-merge finding d2).
func localGateArgv(gate string, heavy bool, script, dir string, env []string) []string {
	shell := []string{"bash", "--noprofile", "--norc", "-eo", "pipefail", script}
	words := strings.Fields(gate)
	if !heavy || len(words) == 0 {
		return shell
	}
	argv := append(append([]string{}, words...), "env", "-i")
	argv = append(argv, env...)
	argv = append(argv, "bash", "-c", localChdirScript, "bash", dir)
	return append(argv, shell...)
}

// localWriteScript writes a step's command to a script in the run's temporary root and returns its
// path. The file lives under the run's own root, so it is removed with the rest.
func localWriteScript(opts localOptions, command string) (string, error) {
	dir := filepath.Join(filepath.Dir(opts.output), "steps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	opts.script++
	path := filepath.Join(dir, fmt.Sprintf("step-%03d.sh", opts.script))
	if err := os.WriteFile(path, []byte(command+"\n"), 0o700); err != nil {
		return "", err
	}
	return path, nil
}

// localInheritedEnv is the caller's variables a step may inherit. Everything else the caller sets
// stays out of a step, and localIgnoredEnv names the Go, Node and npm variables that were left out.
var localInheritedEnv = []string{"PATH", "LANG", "TMPDIR", "GOCACHE", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "NPM_CONFIG_CACHE"}

// localStepEnv is the sealed environment every step runs in: HOME and the XDG directories point into
// the run's own home, TZ is UTC, GOTOOLCHAIN is local, and GOFLAGS is set by the engine alone (its
// parallelism). The caller's other variables are not inherited.
func localStepEnv(home, temp string, opts localOptions) ([]string, error) {
	runner := filepath.Join(temp, "runner")
	for _, dir := range []string{home, filepath.Join(home, "config"), filepath.Join(home, "cache"),
		filepath.Join(home, "data"), filepath.Join(home, "state"), filepath.Join(temp, "tmp"), runner} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	// The changed-path decision appends to GITHUB_OUTPUT, so the file has to exist first.
	if err := os.WriteFile(opts.output, nil, 0o644); err != nil {
		return nil, err
	}
	parallel := opts.Parallel
	if parallel <= 0 {
		parallel = 4
	}
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"TZ=UTC",
		"GOTOOLCHAIN=local",
		fmt.Sprintf("GOFLAGS=-p=%d", parallel),
		"RUNNER_TEMP=" + runner,
		"GITHUB_OUTPUT=" + opts.output,
		"GITHUB_EVENT_NAME=pull_request",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GOWORK=off",
	}
	for _, name := range localInheritedEnv {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	env = append(env, opts.Env...)
	return env, nil
}

// localIgnoredEnv names the caller's variables a sealed step does not inherit and that could change
// what a step does: the Go, cgo, Node and npm settings. The record names them; it never carries their
// values.
func localIgnoredEnv(environ []string) []string {
	inherited := map[string]bool{}
	for _, name := range localInheritedEnv {
		inherited[name] = true
	}
	var ignored []string
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if inherited[name] {
			continue
		}
		if strings.HasPrefix(name, "GO") || strings.HasPrefix(name, "CGO_") || strings.HasPrefix(name, "NODE") || strings.HasPrefix(name, "NPM_") || strings.HasPrefix(name, "npm_") {
			ignored = append(ignored, name)
		}
	}
	return localSortedUnique(ignored)
}

// localNodeMismatch names every job whose ci.yml setup-node pin differs from the Node the host runs
// the steps with. The pin is the job's own, read from its setup-node step, never one shared pin.
func localNodeMismatch(plan []localJob, jobs []workflowJob, observed string) []string {
	var out []string
	for _, job := range plan {
		wj := workflowJobNamed(jobs, job.name)
		if wj == nil {
			continue
		}
		pin := ""
		for _, step := range wj.steps {
			if step.nodeVersion != "" {
				pin = step.nodeVersion
			}
		}
		if pin != "" && observed != pin {
			out = append(out, job.name+":node")
		}
	}
	return out
}

// localWorkRoot is the directory a clean worktree is made in: the caller's choice, or the XDG state
// directory of the real user. A root inside TMPDIR, /tmp or /var/tmp is refused, because the
// repository's own checkout tests assert the checkout lies outside them.
func localWorkRoot(opts localOptions) (string, error) {
	root := opts.WorkRoot
	if root == "" {
		state := os.Getenv("XDG_STATE_HOME")
		if state == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			state = filepath.Join(home, ".local", "state")
		}
		root = filepath.Join(state, "crw-ci-local")
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("the work root %q is not absolute", root)
	}
	root = filepath.Clean(root)
	if err := homeguard.Refuse(root); err != nil {
		return "", fmt.Errorf("the work root: %w", err)
	}
	for _, banned := range localBannedRoots(opts) {
		if banned == "" {
			continue
		}
		if localWithin(root, filepath.Clean(banned)) {
			return "", fmt.Errorf("work_root_in_tmp: %s is inside %s; choose a work root outside the temporary directories with --work-root", root, banned)
		}
	}
	return root, nil
}

// localBannedRoots are the directories a work root may not be inside: TMPDIR, the system temporary
// directories, and nothing when a test has allowed its own root.
func localBannedRoots(opts localOptions) []string {
	if opts.allowTempRoot {
		return nil
	}
	return []string{localTempDir(), os.TempDir(), "/tmp", "/var/tmp"}
}

// localWithin reports whether path is dir or lies below it.
func localWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// localStepEnvValues is a step's own environment with the run's placeholders filled in.
func localStepEnvValues(values []string, opts localOptions, leg string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ReplaceAll(value, localBaseEnv, opts.Base)
		value = strings.ReplaceAll(value, localHeadEnv, opts.Commit)
		value = strings.ReplaceAll(value, localGuiOutputEnv, opts.output)
		value = strings.ReplaceAll(value, localPartEnv, leg)
		value = strings.ReplaceAll(value, "${{ matrix.part }}", leg)
		out = append(out, value)
	}
	return out
}

// localDefaultBase is the commit a record names as its base: the merge base with origin/dev, or
// the commit's parent when that is not known. The head itself is never the base: a merge base that
// equals the head means the commit is already reachable from origin/dev, and base..head would then
// be empty, which would let the range-scoped blob and secret steps judge nothing.
func localDefaultBase(root, head string) (string, error) {
	if out, err := runGit(root, "merge-base", "origin/dev", head); err == nil {
		if base := strings.TrimSpace(string(out)); base != "" && base != head {
			return base, nil
		}
	}
	if out, err := runGit(root, "rev-parse", "--verify", "--quiet", head+"^"); err == nil {
		if base := strings.TrimSpace(string(out)); base != "" && base != head {
			return base, nil
		}
	}
	return "", nil
}

// localRev resolves a revision to its full commit or tree hash.
func localRev(root, rev string) (string, error) {
	out, err := runGit(root, "rev-parse", "--verify", "--quiet", rev)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", rev, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// localBlobDigest is the sha256 of a file as the named commit has it. A file the commit does not
// carry is reported with an empty digest rather than failing the run.
func localBlobDigest(root, commit, name string) (string, error) {
	out, err := runGit(root, "show", commit+":"+name)
	if err != nil {
		return "", nil
	}
	sum := sha256.Sum256(out)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// localToolPresent reports whether a tool resolves on path.
func localToolPresent(name, pathEnv string) bool {
	if strings.ContainsRune(name, filepath.Separator) {
		info, err := os.Stat(name)
		return err == nil && info.Mode().IsRegular()
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// localPathEnv is the PATH a step runs with.
func localPathEnv(env []string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], "PATH="); ok {
			return value
		}
	}
	return os.Getenv("PATH")
}

// localTempDir is where a run makes its temporary root: TMPDIR, so the caller decides, and never a
// path inside the repository.
func localTempDir() string {
	if dir := os.Getenv("TMPDIR"); dir != "" {
		return dir
	}
	return os.TempDir()
}

// localAbs resolves a path against root unless it is already absolute.
func localAbs(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

// localStepLabel names an unnamed step by its command's first words.
func localStepLabel(step localStep) string {
	command := strings.TrimSpace(step.command)
	if command == "" {
		return step.kind
	}
	if line, _, found := strings.Cut(command, "\n"); found && line != "" {
		command = line
	}
	if len(command) > 60 {
		command = command[:60]
	}
	return command
}

// localTimeoutMark is the line a Go test binary prints when a test run outlives its -timeout.
const localTimeoutMark = "panic: test timed out"

// localReason is the reason a failed command's output gives: the failing test lines that come
// before the last lines (so a test that failed early still reaches the record), then the last
// localReasonLines lines, cut to localReasonBytes. The reason stays bounded however long the output.
func localReason(output string) string {
	text := strings.TrimRight(output, "\n")
	if text == "" {
		return ""
	}
	all := strings.Split(text, "\n")
	cut := len(all) - localReasonLines
	if cut < 0 {
		cut = 0
	}
	var marked []string
	for _, line := range all[:cut] {
		if localMarkedLine(line) {
			marked = append(marked, localCut(line, localReasonLine))
			if len(marked) == localReasonMarked {
				break
			}
		}
	}
	tail := localCutEnd(strings.Join(all[cut:], "\n"), localReasonBytes)
	if len(marked) > 0 {
		tail = strings.Join(marked, "\n") + "\n...\n" + tail
	}
	return ": " + tail
}

// localMarkedLine reports whether a line of a test run names a failing test or a panic.
func localMarkedLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "--- FAIL") || strings.HasPrefix(trimmed, "FAIL") || strings.HasPrefix(trimmed, "panic:")
}

// localCut is the first n bytes of a line, valid UTF-8.
func localCut(line string, n int) string {
	if len(line) > n {
		return strings.ToValidUTF8(line[:n], "")
	}
	return line
}

// localCutEnd is the last n bytes of the text, valid UTF-8.
func localCutEnd(text string, n int) string {
	if len(text) > n {
		return strings.ToValidUTF8(text[len(text)-n:], "")
	}
	return text
}

// The bounds of a failure's reason: the last lines of the output and at most this many bytes of
// them, plus up to localReasonMarked failing lines from earlier in the output.
const (
	localReasonLines  = 40
	localReasonBytes  = 4096
	localReasonMarked = 20
	localReasonLine   = 300
)

// localEngineSources are the paths the engine's plan is compiled from: the engine package, the command
// that runs it and the Makefile target that starts it (CRW-1025).
var localEngineSources = []string{"internal/dev/ci", "cmd/crw-dev", "Makefile"}

// localCompiledExt are the extensions of files the Go toolchain compiles or links into a package, so an
// ignored file with one of them under the engine paths is engine source whatever git ignores.
var localCompiledExt = []string{".go", ".s", ".S", ".c", ".h", ".cc", ".cpp", ".cxx", ".hh", ".hpp", ".hxx", ".m", ".f", ".F", ".for", ".f90", ".syso", ".swig", ".swigcxx", ".mod", ".sum", ".work"}

// localEngineDiffers names the engine source the caller's tree holds that the commit does not. The working
// tree is compared with the commit's blobs byte for byte, never through the index (assume-unchanged and
// skip-worktree marks hide an edit from git diff, and the exclude files hide a new file from git status) and
// never through Git's line-ending normalisation or clean filters (the raw bytes are hashed in place: Go compiles
// the raw bytes, which a filter could map back to the committed blob), so every engine file the commit holds must
// exist with its bytes as a regular file (a FIFO or device is a difference and is never opened), every other file under the engine paths is a difference, and an ignored file is let
// through only when Go does not compile it (a log, a coverage file).
// An empty result means the caller runs the commit's engine.
func localEngineDiffers(root, commit string) (string, error) {
	out, err := runGit(root, append([]string{"ls-tree", "-r", "-z", commit, "--"}, localEngineSources...)...)
	if err != nil {
		return "", err
	}
	type entry struct{ mode, oid string }
	committed := map[string]entry{}
	for _, record := range strings.Split(string(out), "\x00") {
		if record == "" {
			continue
		}
		meta, path, found := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !found || len(fields) != 3 {
			return "", fmt.Errorf("git ls-tree returned an unreadable entry: %q", record)
		}
		if fields[1] == "blob" {
			committed[path] = entry{mode: fields[0], oid: fields[2]}
		}
	}
	// Files git reports as ignored and untracked: the ones that may stay when Go does not compile them.
	ignoredOut, err := runGit(root, append([]string{"ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--"}, localEngineSources...)...)
	if err != nil {
		return "", err
	}
	ignored := map[string]bool{}
	for _, path := range strings.Split(string(ignoredOut), "\x00") {
		if path != "" {
			ignored[path] = true
		}
	}
	disk := map[string]bool{}
	for _, source := range localEngineSources {
		err := filepath.WalkDir(filepath.Join(root, source), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			disk[filepath.ToSlash(rel)] = true
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	var names []string
	for path := range disk {
		want, inCommit := committed[path]
		switch {
		case strings.ContainsAny(path, "\n\r"):
			names = append(names, strconv.Quote(path))
		case !inCommit:
			if !ignored[path] || localCompiledExtension(path) {
				names = append(names, path)
			}
		case want.mode == "120000":
			target, err := os.Readlink(filepath.Join(root, filepath.FromSlash(path)))
			blob, gitErr := runGit(root, "cat-file", "blob", want.oid)
			if err != nil || gitErr != nil || string(blob) != target {
				names = append(names, path)
			}
		default:
			// Only a regular file is read, and it is opened without blocking: a FIFO or device that replaced
			// a tracked file is a difference, and opening a FIFO with no writer would wait for ever.
			oid, same, err := localRawBlobID(filepath.Join(root, filepath.FromSlash(path)), len(want.oid) == 64)
			if err != nil {
				return "", err
			}
			if !same || oid != want.oid {
				names = append(names, path)
			}
		}
	}
	for path := range committed {
		if !disk[path] {
			names = append(names, path)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", "), nil
}

// localRawBlobID is the Git blob id of the file's raw bytes (what Go compiles, with no line-ending
// normalisation or filter). same is false when the path is not a regular file, or stopped being one between
// the checks: nothing is read from it then. sha256 selects the repository's object format.
func localRawBlobID(path string, sha256Format bool) (oid string, same bool, err error) {
	before, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	if !before.Mode().IsRegular() {
		return "", false, nil
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", false, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return "", false, nil
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return "", false, err
	}
	header := fmt.Sprintf("blob %d\x00", len(data))
	if sha256Format {
		sum := sha256.New()
		sum.Write([]byte(header))
		sum.Write(data)
		return hex.EncodeToString(sum.Sum(nil)), true, nil
	}
	sum := sha1.New()
	sum.Write([]byte(header))
	sum.Write(data)
	return hex.EncodeToString(sum.Sum(nil)), true, nil
}

// localBinaryDiffers names why the running binary is not the verified commit's engine, or returns "". go
// build stamps the revision it built from and whether the tree was modified; a binary built from a modified
// tree, or from a revision whose engine paths differ from the commit's (or that this repository does not
// hold), executes a plan and an executor the commit does not define. A binary without a revision (go run, go
// test, -buildvcs=false) is compiled from the working tree that localEngineDiffers has compared with the
// commit.
func localBinaryDiffers(opts localOptions, commit string) (string, error) {
	read := opts.buildInfo
	if read == nil {
		read = debug.ReadBuildInfo
	}
	info, ok := read()
	if !ok || info == nil {
		return "", nil
	}
	var revision, modified string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}
	if revision == "" {
		return "", nil
	}
	if modified == "true" {
		return fmt.Sprintf("was built from a modified tree (revision %s)", shortHash(revision)), nil
	}
	if revision == commit {
		return "", nil
	}
	args := append([]string{"ls-tree", "-z"}, "REV", "--")
	args = append(args, localEngineSources...)
	listing := func(rev string) (string, error) {
		args[2] = rev
		out, err := runGit(opts.Root, args...)
		return string(out), err
	}
	built, err := listing(revision + "^{commit}")
	if err != nil {
		return fmt.Sprintf("was built from revision %s, which this repository does not hold", shortHash(revision)), nil
	}
	verified, err := listing(commit)
	if err != nil {
		return "", err
	}
	if built != verified {
		return fmt.Sprintf("was built from revision %s, whose engine source differs from %s", shortHash(revision), shortHash(commit)), nil
	}
	return "", nil
}

// localCompiledExtension reports whether Go compiles a file with this name's extension.
func localCompiledExtension(path string) bool {
	return localContains(localCompiledExt, filepath.Ext(path))
}

// localPlatformRefusal is the unsupported_platform refusal for a plan whose secrets step cannot run on the
// host: scripts/ci/secrets.sh downloads the linux x64 Gitleaks archive and calls sha256sum. A plan without
// that step is platform independent.
func localPlatformRefusal(plan []localJob, opts localOptions) error {
	goos, goarch := opts.platform()
	if goos == "linux" && goarch == "amd64" {
		return nil
	}
	for _, job := range plan {
		for _, step := range job.steps {
			if step.kind == localRun && step.command == localSecretsCommand {
				return fmt.Errorf("unsupported_platform: the %s job runs %s, which fetches the linux x64 Gitleaks archive and calls sha256sum; this host is %s/%s", job.name, localSecretsCommand, goos, goarch)
			}
		}
	}
	return nil
}

// shortHash is a commit hash's first twelve characters.
func shortHash(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// localContains reports whether items holds item.
func localContains(items []string, item string) bool {
	for _, candidate := range items {
		if candidate == item {
			return true
		}
	}
	return false
}

// localOutputSize is the size of the changed-path output file before a step runs.
func localOutputSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// localDecisionSince is what a step appended to GITHUB_OUTPUT since offset: the decision the step
// recorded, copied into its record entry (parent ruling 2, d7).
func localDecisionSince(path string, offset int64) string {
	data, err := os.ReadFile(path)
	if err != nil || int64(len(data)) <= offset {
		return ""
	}
	return strings.TrimSpace(string(data[offset:]))
}

// localAbsGate resolves a relative path to the heavy-check gate's executable against the directory the run was
// started in, once, before any probe or step changes directory (pre-merge finding d3).
func localAbsGate(gate string) string {
	words := strings.Fields(gate)
	if len(words) == 0 || !strings.Contains(words[0], "/") || filepath.IsAbs(words[0]) {
		return gate
	}
	abs, err := filepath.Abs(words[0])
	if err != nil {
		return gate
	}
	words[0] = abs
	return strings.Join(words, " ")
}
