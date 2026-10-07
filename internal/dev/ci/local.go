//go:build dev

package ci

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
	Plan      []localJob
	// Env is extra environment for every step, after the isolated home and TZ (tests use it).
	Env []string
	// output is the file the changed-path decision writes its answer to, set once per run.
	output string
	// script counts the step scripts written under the run's temporary root.
	script int
}

// Local is `crw-dev ci local`: run every ci.yml job and step locally, or install the
// pre-push hook. `crw-dev ci local hook install|status` is the hook subcommand.
func Local(args []string, stdout, stderr io.Writer) int {
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
	plan := opts.Plan
	if plan == nil {
		plan = localPlan()
	}
	current, err := localCurrentKeys(opts)
	if err != nil {
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
	head, err := localRev(opts.Root, opts.Commit)
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
		Tools:        localObservedVersions(localToolVersionsIn(localPathEnv(opts.Env), goMod), pins),
		GoFlags:      localFullRunFlags(os.Getenv("GOFLAGS")),
		GoEnv:        localIsolatedGoEnv,
		Dependencies: dependencies,
		OS:           localHostOS(),
		Arch:         localHostArch(),
	}, nil
}

// localFullRunFlags is GOFLAGS without the flags that select or skip tests, so a full run runs every
// test whatever the caller's GOFLAGS say.
func localFullRunFlags(flags string) string {
	var kept []string
	for _, field := range strings.Fields(flags) {
		if !localSelectsTests(field) {
			kept = append(kept, field)
		}
	}
	return strings.Join(kept, " ")
}

// localSelectsTests reports whether a GOFLAGS field selects, skips or shortens tests.
func localSelectsTests(field string) bool {
	for _, name := range []string{"-run", "-skip", "-short", "-failfast", "-count", "-list", "-test.run", "-test.skip", "-test.short", "-test.failfast", "-test.count", "-test.list"} {
		if field == name || strings.HasPrefix(field, name+"=") {
			return true
		}
	}
	return false
}

// localRepository is the origin remote's URL, the identity a record names. It is read from the
// checkout rather than written down, so the record travels with the repository it describes.
func localRepository(root string) string {
	if out, err := runGit(root, "remote", "get-url", "origin"); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

// localExecute runs the table in a clean worktree of the verified commit and assembles the record.
func localExecute(opts localOptions, plan []localJob, current verificationRecord, stdout io.Writer) (verificationRecord, bool, error) {
	temp, err := os.MkdirTemp(localTempDir(), localTempPrefix)
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
		// Remove the worktree through git, then the whole root. A failed removal falls back to
		// pruning the administrative entry so the repository is not left with a stale worktree.
		runGit(opts.Root, "worktree", "remove", "--force", worktree)
		if _, err := os.Stat(worktree); err == nil {
			runGit(opts.Root, "worktree", "prune")
		}
		os.RemoveAll(temp)
	}
	defer cleanup()
	if out, err := runGit(opts.Root, "worktree", "add", "--detach", worktree, current.HeadCommit); err != nil {
		return verificationRecord{}, false, fmt.Errorf("creating the clean worktree: %w%s", err, localReason(string(out)))
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
	record.Pins = pins
	record.Tools = localObservedVersions(current.Tools, pins)
	record.PinMismatch = localPinMismatch(pins, record.Tools)
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
		return localPassed, step.tool + " " + localObserveTool(step.tool, localPathEnv(env))
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
	argv := localGateArgv(opts.HeavyGate, step.heavy, script)
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
func localGateArgv(gate string, heavy bool, script string) []string {
	argv := []string{"bash", "--noprofile", "--norc", "-eo", "pipefail", script}
	if heavy {
		if words := strings.Fields(gate); len(words) > 0 {
			argv = append(words, argv...)
		}
	}
	return argv
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

// localStepEnv is the isolated environment every step runs in: HOME and the XDG directories point
// into a temporary root, TZ is UTC, and the caches the host already configured are passed through
// (they change speed, never the result).
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
	// GOENV is never inherited: the host's Go environment file can change what the Go steps build.
	// With HOME and XDG_CONFIG_HOME in the run's own home, Go reads its default file there, which is
	// empty (localIsolatedGoEnv).
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"TZ=UTC",
		"RUNNER_TEMP=" + runner,
		"GITHUB_OUTPUT=" + opts.output,
		"GITHUB_EVENT_NAME=pull_request",
	}
	// The host's caches, PATH and the user runtime directory are inherited: they decide how fast a
	// step runs and whether the heavy-check gate can reach the user's systemd, not what it
	// decides. GOFLAGS is left as the host set it, and GOCACHE is never cleared.
	for _, name := range []string{"PATH", "TMPDIR", "GOCACHE", "GOMODCACHE", "GOPATH", "GOPROXY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	// GOFLAGS is inherited without the flags that select or skip tests: a full run runs every test.
	env = append(env, "GOFLAGS="+localFullRunFlags(os.Getenv("GOFLAGS")))
	env = append(env, opts.Env...)
	return env, nil
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
