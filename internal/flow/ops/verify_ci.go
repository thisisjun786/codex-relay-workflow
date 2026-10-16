package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/execgroup"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// RunCommand runs a command in a directory, with env set over the inherited environment (nil: inherited as it is), and answers
// its exit code, its combined output and the error of starting it (an exit code other than 0 is not an error).
type RunCommand func(ctx context.Context, dir string, env []string, name string, args ...string) (code int, output []byte, err error)

// ExecCommand is the RunCommand that starts the process. The process leads its own group (execgroup.Bind, CRW-1573): a cancel
// kills everything it started, so a cancelled `crw-dev ci local` returns within execgroup.KillGrace instead of waiting for the
// test binaries that hold its output.
func ExecCommand(ctx context.Context, dir string, env []string, name string, args ...string) (int, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		// exec keeps the last entry of a name, so an entry of env replaces the inherited one
		cmd.Env = append(os.Environ(), env...)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	execgroup.Bind(cmd, execgroup.KillGrace)
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), out.Bytes(), nil
	}
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil {
		// the command exited 0 and a child it left holds the output past the grace: the exit is the answer, not a start error
		return cmd.ProcessState.ExitCode(), out.Bytes(), nil
	}
	return 0, out.Bytes(), err
}

// CILocalVerifier verifies a head with `crw-dev ci local`: the command runs every job and step of ci.yml in a clean worktree of the
// commit, with an isolated home, and leaves a verification-record/1 (internal/dev/ci, the digest and the result of the record
// are what the relay's acceptance judges). The record, not the exit code alone, is the verification: a command that exits 0 with
// no record, or a record whose digest is missing, did not verify the head.
type CILocalVerifier struct {
	// Binary is the crw-dev binary; empty is the one Tools names.
	Binary string
	// Tools are the verifier's tools of the configuration (verifier.go_root, verifier.crw_dev; CRW-1319). Nothing configured is
	// crw-dev and go from PATH (fail-open), and the outcome's note says so.
	Tools crwconfig.VerifierTools
	// ExtraArgs follow `ci local` (a heavy gate, a work root).
	ExtraArgs []string
	// RecordDir is where the records are written; empty is a directory inside the checkout being verified (CRW-1489, ciRecordDir):
	// crw-verify below <worktree>/.git when that is a real directory entry (not a link), or .crw/verify when .git is a file (a linked
	// worktree) or a symbolic link, never the caller's TMPDIR, so two lanes never share a record file. A worktree with no git
	// directory, and a .crw/verify that is a symbolic link, are errors before the command runs.
	RecordDir string
	Run       RunCommand
}

// CILocalCommand is the command string the verification record of a head names.
const CILocalCommand = "crw-dev ci local"

type ciRecord struct {
	Schema string `json:"schema"`
	Result string `json:"result"`
	Digest string `json:"digest"`
	// TestPackagesRun is additive (CRW-1328): a record without it does not say how many packages ran tests.
	TestPackagesRun *int `json:"testPackagesRun"`
	Jobs            []struct {
		Name   string `json:"name"`
		Result string `json:"result"`
		Steps  []struct {
			Name   string `json:"name"`
			Result string `json:"result"`
			// Reason is how the step ended: for a failed command, its failing test lines and the last lines of its output (CRW-1469).
			Reason string `json:"reason"`
		} `json:"steps"`
	} `json:"jobs"`
}

// Verify implements Verifier.
func (v CILocalVerifier) Verify(ctx context.Context, req VerifyRequest) (VerifyOutcome, error) {
	if req.Worktree == "" || req.Head == "" {
		return VerifyOutcome{}, errors.New("ops: a verification needs the head and the worktree it is committed in")
	}
	if req.Command != "" && req.Command != CILocalCommand {
		return VerifyOutcome{}, fmt.Errorf("ops: this verifier runs %q only, not %q", CILocalCommand, req.Command)
	}
	run := v.Run
	if run == nil {
		run = ExecCommand
	}
	tools := v.Tools
	if v.Binary != "" {
		tools.CrwDev = v.Binary
	}
	command, err := tools.Command(req.Worktree, os.Getenv("PATH"))
	if err != nil {
		return VerifyOutcome{}, fmt.Errorf("ops: %s cannot run: %w", CILocalCommand, err)
	}
	dir := v.RecordDir
	if dir == "" {
		if dir, err = ciRecordDir(req.Worktree); err != nil {
			return VerifyOutcome{}, fmt.Errorf("ops: ci_record_dir: %w", err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return VerifyOutcome{}, err
	}
	record := filepath.Join(dir, "verification-record-"+req.Head+".json")
	_ = os.Remove(record)
	args := append(append([]string{}, command.Argv[1:]...), "ci", "local", "--commit", req.Head, "--record", record, "--runner", "flow")
	if req.Base != "" {
		args = append(args, "--base", req.Base)
	}
	args = append(args, v.ExtraArgs...)
	code, output, err := run(ctx, req.Worktree, command.Env, command.Argv[0], args...)
	if err != nil {
		return VerifyOutcome{}, fmt.Errorf("ops: %s did not start: %w; %s", CILocalCommand, err, command.Hint)
	}
	raw, rerr := os.ReadFile(record)
	if rerr != nil {
		return VerifyOutcome{}, fmt.Errorf("ops: %s left no record (exit %d): %s", CILocalCommand, code, tail(output))
	}
	var rec ciRecord
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Schema != "verification-record/1" || rec.Digest == "" {
		return VerifyOutcome{}, fmt.Errorf("ops: %s left a record that is no verification-record/1 with a digest (exit %d)", CILocalCommand, code)
	}
	out := VerifyOutcome{Command: CILocalCommand, Digest: rec.Digest, Record: raw, Pass: code == 0 && rec.Result == "pass", Failures: []string{}, Note: command.Note,
		TestPackagesRun: rec.TestPackagesRun}
	for _, job := range rec.Jobs {
		for _, st := range job.Steps {
			if st.Result != "passed" && st.Result != "not_applicable" {
				out.Failures = append(out.Failures, job.Name+"/"+st.Name+": "+st.Result)
				if st.Result != stepSkipped && st.Reason != "" {
					out.FailedTests = append(out.FailedTests, failedSteps(job.Name, st.Name, st.Reason))
				}
			}
		}
	}
	switch {
	case out.Pass:
		out.Summary = "pass"
	case len(out.Failures) > 0:
		out.Summary = strings.Join(out.Failures, "; ")
	default:
		out.Summary = fmt.Sprintf("%s exited %d with result %q", CILocalCommand, code, rec.Result)
	}
	return out, nil
}

// FailedStep is one failed step of a verification record (CRW-1469, CRW-1488 item 16): the job and the step, the tests (`--- FAIL:
// <Test>`) and the packages (`FAIL\t<pkg>`) its reason names, each once in the order of the reason, and its tail (failedTail): the
// last lines of the reason, after the marked block of a test that failed early when the reason leads with one (CRW-1569).
// A reason with no such line (a build error, a missing tool, a reason cut before its test lines) has no test and no package, and the
// tail is what it says.
type FailedStep struct {
	Job      string   `json:"job"`
	Step     string   `json:"step"`
	Tests    []string `json:"tests"`
	Packages []string `json:"packages"`
	Tail     string   `json:"tail"`
}

// stepSkipped is the result of a step that did not run because an earlier step of its job did not pass: it is a failure of the
// verification, but its reason says only that, so it names no failed test.
const stepSkipped = "skipped"

// The tail of a failed step's reason: at most failedTailLines lines and failedTailBytes bytes. Of a reason that leads with a marked
// block (failedHead), the tail keeps the block's first failedHeadLines lines, at most failedHeadBytes bytes of them, then `...`, then
// the last lines of the reason in the room left.
const (
	failedTailLines = 20
	failedTailBytes = 2048
	failedHeadLines = 10
	failedHeadBytes = 1024
)

// failedSteps reads the reason of a failed step: the tests and the packages its lines name, and its tail. A reason of crw-dev ci local
// starts with how the command exited ("exit status 1: ", or "timeout: exit status 1: " for a step stopped at its time limit), which
// is not part of its first line of output.
func failedSteps(job, step, reason string) FailedStep {
	out := FailedStep{Job: job, Step: step, Tests: []string{}, Packages: []string{}}
	lines := strings.Split(strings.TrimRight(reason, "\n"), "\n")
	for i, line := range lines {
		if i == 0 {
			line = failedExited(line)
		}
		if rest, ok := strings.CutPrefix(strings.TrimLeft(line, " "), "--- FAIL: "); ok {
			if name, _, _ := strings.Cut(rest, " "); name != "" && !slices.Contains(out.Tests, name) {
				out.Tests = append(out.Tests, name)
			}
		} else if rest, ok := strings.CutPrefix(line, "FAIL\t"); ok {
			if pkg := strings.FieldsFunc(rest, func(r rune) bool { return r == '\t' || r == ' ' }); len(pkg) > 0 && !slices.Contains(out.Packages, pkg[0]) {
				out.Packages = append(out.Packages, pkg[0])
			}
		}
	}
	out.Tail = failedTail(lines)
	return out
}

// failedExited is the first line of a reason without how the command exited ("exit status N: " or "timeout: exit status N: ").
func failedExited(line string) string {
	if exited := strings.TrimPrefix(line, "timeout: "); strings.HasPrefix(exited, "exit status ") {
		if _, rest, ok := strings.Cut(exited, ": "); ok {
			return rest
		}
	}
	return line
}

// failedTail is the tail of a reason's lines. A reason that fits failedTailLines and failedTailBytes is its own tail. Otherwise a
// reason that leads with a marked block (failedHead) keeps the block's first lines, then `...`, then its last lines; any other reason
// keeps its last lines, cut to failedTailBytes from the end.
func failedTail(lines []string) string {
	if head, rest, ok := failedHead(lines); ok && (len(lines) > failedTailLines || len(strings.Join(lines, "\n")) > failedTailBytes) {
		kept := head[:min(len(head), failedHeadLines)]
		block := strings.Join(kept, "\n")
		for len(kept) > 1 && len(block) > failedHeadBytes {
			kept = kept[:len(kept)-1]
			block = strings.Join(kept, "\n")
		}
		if len(block) > failedHeadBytes {
			block = strings.ToValidUTF8(block[:failedHeadBytes], "")
		}
		block += "\n...\n"
		rest = rest[max(0, len(rest)-(failedTailLines-len(kept)-1)):]
		return block + failedCutEnd(strings.Join(rest, "\n"), failedTailBytes-len(block))
	}
	if len(lines) > failedTailLines {
		lines = lines[len(lines)-failedTailLines:]
	}
	return failedCutEnd(strings.Join(lines, "\n"), failedTailBytes)
}

// failedHead splits a reason at its marked block: crw-dev ci local's reason leads with the failing lines of the output that came before
// its last lines (`--- FAIL`, `FAIL`, `panic:`, each --- FAIL line with its indented message lines), then a `...` line, then the last
// lines (dev/ci/local.go localReason). ok is false for a reason without a `...` line or whose lines before it are not all marked or
// indented: a `...` line of the output itself is not taken for the end of a block.
func failedHead(lines []string) (head, rest []string, ok bool) {
	at := slices.Index(lines, "...")
	if at <= 0 {
		return nil, nil, false
	}
	for i, line := range lines[:at] {
		if i == 0 {
			line = failedExited(line)
		}
		trimmed := strings.TrimLeft(line, " \t")
		marked := strings.HasPrefix(trimmed, "--- FAIL") || strings.HasPrefix(trimmed, "FAIL") || strings.HasPrefix(trimmed, "panic:")
		if strings.TrimSpace(line) == "" || (!marked && trimmed == line) {
			return nil, nil, false
		}
	}
	return lines[:at], lines[at+1:], true
}

// failedCutEnd is the last n bytes of the text, valid UTF-8.
func failedCutEnd(text string, n int) string {
	if len(text) > n {
		return strings.ToValidUTF8(text[len(text)-n:], "")
	}
	return text
}

// ciRecordDirMade is called with .crw/verify right after ciRecordDir has made and checked it, before its ignore file is
// written; a test replaces it to change the directory in that window.
var ciRecordDirMade = func(string) {}

// ciRecordDir is the default record directory of a worktree, inside its checkout: crw-dev refuses a record that leaves the checkout
// for the account's protected directories (homeguard), and a git directory reached through a .git file or a .git link may lie in
// such a place (a linked worktree's directory in the common git directory of a repository below the account's .codex). So it is
// crw-verify below <worktree>/.git only when .git is a real directory entry of the worktree (Lstat, a link is not followed), and
// .crw/verify for a .git file or a .git symbolic link, where nothing is written below the git directory. .crw/verify holds its own
// ignore file ("*", which hides itself too), so git and the working snapshot do not see the records even where .crw already
// existed without crwdir's ignore file and the repository has no .crw rule. The ignore file is created through a descriptor of
// .crw/verify opened without following a link (crwdir.WriteExclusive): a .crw/verify that is a symbolic link, there before the
// call or put there while it runs, is an error and nothing is written through it. The .git file or link must still lead to a git
// directory (ciGitDir): without one it is an error.
func ciRecordDir(worktree string) (string, error) {
	dotGit := filepath.Join(worktree, ".git")
	if info, err := os.Lstat(dotGit); err == nil && info.IsDir() {
		return filepath.Join(dotGit, "crw-verify"), nil
	}
	if _, err := ciGitDir(worktree); err != nil {
		return "", err
	}
	dir, err := crwdir.EnsureSubdir(worktree, "verify")
	if err != nil {
		return "", err
	}
	ciRecordDirMade(dir)
	if err := crwdir.WriteExclusive(filepath.Join(dir, ".gitignore"), "*\n"); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	return dir, nil
}

// ciGitDir answers the worktree's own git directory, read in Go with no subprocess: <worktree>/.git when it is a real directory
// entry (Lstat); the physical path of the directory a .git symbolic link leads to; or the target of the `gitdir:` line of a .git
// file (a linked worktree's per-worktree directory, not the common one). A target is resolved with the kernel's semantics, not
// cleaned lexically first: a relative one is spelled <worktree>/<target> as written, so a ".." after a link in it leaves the
// link's target as the kernel does; that spelling must be a directory (Stat) and the answer is its physical path
// (filepath.EvalSymlinks). A .git link that leads to a file is read as that git file. A .git that is missing, a link that leads
// nowhere, and a file that names no gitdir are the "has no git directory" error.
func ciGitDir(worktree string) (string, error) {
	dotGit := filepath.Join(worktree, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%s has no git directory", worktree)
		}
		return "", err
	}
	if info.IsDir() {
		return dotGit, nil
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		// A link to a directory is that git directory; a link to a file is read as the git file it leads to, as git reads it.
		if to, err := os.Stat(dotGit); err != nil || to.IsDir() {
			return ciPhysicalDir(worktree, dotGit, "the link "+dotGit)
		}
	}
	raw, err := os.ReadFile(dotGit)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	target, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	target = strings.TrimSpace(target)
	if !ok || target == "" {
		return "", fmt.Errorf("%s has no git directory: %s names no gitdir", worktree, dotGit)
	}
	if !filepath.IsAbs(target) {
		// No filepath.Join: it would clean a ".." lexically, before the links in front of it are resolved.
		target = worktree + string(filepath.Separator) + target
	}
	return ciPhysicalDir(worktree, target, "the gitdir "+target+" of "+dotGit)
}

// ciPhysicalDir answers the physical path of spelled, which must be a directory as the kernel resolves it; what names spelled
// in the error of one that is not. A spelled that cannot be stat'ed (a directory on its path the account may not search, a
// missing target) is reported by the Stat error itself, wrapped, never as "is not a directory" (CRW-1508 item 3).
func ciPhysicalDir(worktree, spelled, what string) (string, error) {
	info, err := os.Stat(spelled)
	if err != nil {
		return "", fmt.Errorf("%s has no git directory: %s cannot be stat'ed: %w", worktree, what, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s has no git directory: %s is not a directory", worktree, what)
	}
	resolved, err := filepath.EvalSymlinks(spelled)
	if err != nil {
		return "", fmt.Errorf("%s has no git directory: %s does not resolve: %w", worktree, what, err)
	}
	return resolved, nil
}

func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		s = s[len(s)-400:]
	}
	return s
}
