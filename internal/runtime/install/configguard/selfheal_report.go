package configguard

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/guidancerecord"
	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"golang.org/x/sys/unix"
)

// The SessionStart self-heal leg, report only (Jun's J4 decision, 2026-10-06).
//
// CXC v0.2.40 config-guard/src/cli.ts:167-196 runs selfHealDeclaredFeatures (self-heal.ts:151-262)
// at SessionStart: it turns the declared SOFT codex feature flags on, writes the marker and claims
// the flags in the install manifest. CRW's install doctrine says an automatic path never edits the
// user's Codex configuration, so the port keeps the diagnosis and drops every write: it reads the
// declared state, and when a soft flag crw declares is off it says so once per key, naming the
// command the user can run. There is no enable call, no marker write, no manifest write and no
// backup. The rest of the oracle's rule (the opt-out marker, the all-enabled cache, a key the user
// turned off after a heal, an unreadable listing) is kept exactly, including its silence.
//
// Fail-open, as the oracle: every failure path exits 0 without output, because a config-guard
// problem must never stop a session from starting.

// SelfHealReportAction is what one round did for one key.
type SelfHealReportAction string

const (
	// SelfHealReportSkipped is a round with nothing to do: Reason names which guard returned.
	SelfHealReportSkipped SelfHealReportAction = "skipped"
	// SelfHealReportDeclined is a key that is off but listed in healedKeys: the user turned it off
	// after a heal, so the hook stays quiet (self-heal.ts:186-191).
	SelfHealReportDeclined SelfHealReportAction = "declined"
	// SelfHealReportUnavailable is a measurement failure: the listing could not be read.
	SelfHealReportUnavailable SelfHealReportAction = "unavailable"
	// SelfHealReportOff is the J4 outcome: a soft declared key is off and the hook warns.
	SelfHealReportOff SelfHealReportAction = "off"
)

// The three skips, spelled as the oracle's SelfHealOutcome reasons.
const (
	SelfHealReasonOptedOut       = "opted-out"
	SelfHealReasonCached         = "cached"
	SelfHealReasonAlreadyEnabled = "already-enabled"
)

// SelfHealReportOutcome is one key's outcome, or one round-level skip/unavailable.
type SelfHealReportOutcome struct {
	Action  SelfHealReportAction
	Reason  string
	Key     string
	Message string
}

// SelfHealReportDeps are injected, so the rule never resolves a home or starts a binary itself.
type SelfHealReportDeps struct {
	CodexHome string
	// Cwd is the directory the hook's codex runs in; "" means it is not known, and recorded evidence
	// is not reused (CRW-1150).
	Cwd string
	Run CodexRunner
	// Ctx is the round's deadline: a fingerprint read that has not finished when it ends is abandoned
	// and the round measures (CRW-1150). nil means no deadline.
	Ctx context.Context
}

// SelfHealReport is selfHealDeclaredFeatures without its writes. The marker is read only, and the
// only external call is the injected runner (features list).
func SelfHealReport(deps SelfHealReportDeps) []SelfHealReportOutcome {
	marker, err := ReadSelfHealMarkerFile(deps.CodexHome)
	if err != nil {
		// A read failure is not an empty consent record (selfheal_marker.go); the round is silent.
		return []SelfHealReportOutcome{{Action: SelfHealReportUnavailable, Message: err.Error()}}
	}
	if marker != nil && marker.OptedOut != nil && *marker.OptedOut {
		return []SelfHealReportOutcome{{Action: SelfHealReportSkipped, Reason: SelfHealReasonOptedOut}}
	}

	healable := SelfHealReportableFeatures()
	configMtimeMs := selfHealReportMtimeMs(filepath.Join(deps.CodexHome, "config.toml"))
	// A cache that predates a SOFT_FEATURES addition must not vouch for the new key.
	cacheCoversCurrentKeys := marker != nil && marker.CachedKeys != nil && selfHealReportCovers(marker.CachedKeys, healable)
	// A marker that carries verified evidence, even evidence that no longer parses, is judged by the
	// evidence alone: the mtime cache an older CXC wrote must not vouch ahead of a newer explicit
	// finding (CRW-1150).
	if marker != nil && marker.Probe == nil && !marker.probeSeen && marker.AllEnabled != nil && *marker.AllEnabled && cacheCoversCurrentKeys &&
		marker.ConfigMtimeMs != nil && configMtimeMs != nil && *marker.ConfigMtimeMs == *configMtimeMs {
		return []SelfHealReportOutcome{{Action: SelfHealReportSkipped, Reason: SelfHealReasonCached}}
	}

	healedKeys := []string{}
	if marker != nil {
		healedKeys = marker.HealedKeys
	}
	// The record an explicit command made of a verified listing stands in for the listing while the
	// codex version and config.toml are the ones it was measured against (CRW-1150).
	state, fromEvidence := selfHealEvidenceState(deps, marker, healable)
	if !fromEvidence {
		var err error
		state, err = ReadDeclaredState(deps.Run)
		if err != nil {
			// Measurement failure, not a state. Nothing is cached (the oracle leaves the marker as it
			// was) and nothing is said.
			return []SelfHealReportOutcome{{Action: SelfHealReportUnavailable, Message: err.Error()}}
		}
	}

	outcomes := []SelfHealReportOutcome{}
	for _, key := range healable {
		if state[string(key)] {
			continue
		}
		// The user turned this key off after a heal: heal once, never argue (self-heal.ts:183-191).
		if selfHealReportContains(healedKeys, string(key)) {
			outcomes = append(outcomes, SelfHealReportOutcome{Action: SelfHealReportDeclined, Key: string(key)})
			continue
		}
		outcomes = append(outcomes, SelfHealReportOutcome{Action: SelfHealReportOff, Key: string(key)})
	}
	if len(outcomes) == 0 {
		outcomes = append(outcomes, SelfHealReportOutcome{Action: SelfHealReportSkipped, Reason: SelfHealReasonAlreadyEnabled})
	}
	return outcomes
}

// SelfHealReportableFeatures is selfHealableFeatures (self-heal.ts:141-143): the declared flags that
// are soft. A hard flag being off means crw install never ran, which the doctor reports standing.
func SelfHealReportableFeatures() []DeclaredFeature {
	out := []DeclaredFeature{}
	for _, key := range DeclaredFeatures() {
		for _, soft := range SoftFeatures() {
			if key == soft {
				out = append(out, key)
			}
		}
	}
	return out
}

// RenderSelfHealReportContext is the SessionStart additionalContext, or "" when there is nothing to
// say. One line per off key: the oracle's impact sentence, then the command the user runs to fix it.
func RenderSelfHealReportContext(outcomes []SelfHealReportOutcome) string {
	lines := []string{}
	for _, outcome := range outcomes {
		if outcome.Action != SelfHealReportOff {
			continue
		}
		impact := SoftFeatureImpact()[DeclaredFeature(outcome.Key)]
		if impact == "" {
			impact = "이 플래그에 의존하는 기능이 비활성화된다."
		}
		lines = append(lines, "[crw] The codex feature flag "+outcome.Key+" that crw declares is off. "+
			impact+" Turn it on with: crw install features enable")
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// selfHealReportLeg names this hook's record of what a session was told.
const selfHealReportLeg = "self-heal-report"

// RunSelfHealReportHook owns this component's input and answer. Input is used only for the shared
// metadata observation; malformed, oversized or unreadable input still exits 0 silently.
func RunSelfHealReportHook(ctx context.Context, in io.Reader, out io.Writer, env host.LookupEnv) int {
	input := make(chan string, 1)
	go func() {
		raw, _ := harness.ReadStdin(in)
		input <- raw
	}()
	var raw string
	select {
	case raw = <-input:
	case <-ctx.Done():
		return harness.Interrupted
	}
	if ctx.Err() != nil {
		return harness.Interrupted
	}
	harness.RecordInvocation(raw, "config-guard", "session-start", env)
	home, err := selfHealReportHome(env)
	if err != nil {
		return 0
	}
	// Every codex call of the round shares one short deadline (CRW-1150). A probe that overruns it is
	// a measurement that failed, which the round answers with silence, so a slow codex cannot spend
	// the hook's whole time limit and rely on the host to kill it.
	probeCtx, endProbe := context.WithTimeout(ctx, selfHealReportProbeDeadline)
	defer endProbe()
	// codex runs in the hook's working directory, which decides the project layers that apply; when it
	// cannot be read, recorded evidence is not reused.
	cwd, _ := os.Getwd()
	additional := RenderSelfHealReportContext(SelfHealReport(SelfHealReportDeps{CodexHome: home, Cwd: cwd, Run: SelfHealReportRunner(probeCtx, env), Ctx: probeCtx}))
	if ctx.Err() != nil {
		// The probe was cancelled while it ran: nothing is rendered or written after cancellation.
		return harness.Interrupted
	}
	// CRW-1180: the compact start right after a resume that gave this very warning, in the same turn, adds nothing: Codex keeps the
	// resume's output after the compaction record, so saying it again stacks the warning twice. The warning is live state, so a resume
	// always says it. Every start ends a pair a resume left open, also the one that has no warning to say (the flag was on at that
	// clear or startup), so a later compact does not take that resume for its pair.
	if guidancerecord.SilentCompact(env, raw, selfHealReportLeg, additional) || additional == "" {
		return 0
	}
	answer := `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":` +
		pyjson.Dumps(additional, pyjson.Options{Compact: true, Unicode: true}) + "}}\n"
	// Only a warning written whole, by a hook that was not cancelled, counts as said.
	if n, err := io.WriteString(out, answer); err == nil && n == len(answer) && ctx.Err() == nil {
		guidancerecord.Said(env, raw, selfHealReportLeg, additional)
	}
	return 0
}

// selfHealReportHome is the oracle's resolveCodexHome (cli.ts:122-125): a trimmed CODEX_HOME, else
// <home>/.codex. An empty HOME is not an absent one: joining it yields a relative .codex.
func selfHealReportHome(env host.LookupEnv) (string, error) {
	if value, _ := env("CODEX_HOME"); text.Trim(value) != "" {
		return text.Trim(value), nil
	}
	base, set := env("HOME")
	if !set {
		var err error
		if base, err = host.Home(env); err != nil {
			return "", err
		}
	}
	return filepath.Join(base, ".codex"), nil
}

// selfHealReportMtimeMs is statSync(path).mtimeMs; nil when the path does not exist. Node adds the
// seconds and the nanoseconds apart, so the port does too: a single float64(UnixNano()) loses
// precision above 2^53 ns (1970 + about 104 days) and the last bit decides this equality.
func selfHealReportMtimeMs(path string) *float64 {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	ms := selfHealReportMsOf(info.ModTime())
	return &ms
}

// selfHealReportMsOf is msOf (internal/relay/job/store.go:418): mtimeMs with the fractions kept.
func selfHealReportMsOf(t time.Time) float64 {
	return float64(t.Unix())*1e3 + float64(t.Nanosecond())/1e6
}

// selfHealReportCovers is the cache guard's "cachedKeys covers every soft key" test.
func selfHealReportCovers(have []string, want []DeclaredFeature) bool {
	for _, key := range want {
		if !selfHealReportContains(have, string(key)) {
			return false
		}
	}
	return true
}

func selfHealReportContains(list []string, want string) bool {
	for _, value := range list {
		if value == want {
			return true
		}
	}
	return false
}

// SelfHealReportRunner is makeRealRunner (cli.ts:140-149) over the caller's environment: the codex
// on PATH, with spawnSync's combined 1 MiB stdout+stderr budget. A binary that cannot be started, or
// one that overflows the budget, answers exit 1, which ReadDeclaredState turns into an error and the
// hook turns into silence. package install's featureRunner has the same semantics; it cannot be
// reused here because it takes a scope.Env and this package is below install.
func SelfHealReportRunner(ctx context.Context, env host.LookupEnv) CodexRunner {
	return func(args []string) CodexRunResult {
		candidates, err := selfHealReportCandidates(env)
		if err != nil {
			return CodexRunResult{Stderr: err.Error(), ExitCode: 1}
		}
		// execvp semantics: a candidate whose exec fails the way a search goes on from (EACCES, which the access
		// check cannot see for a script whose interpreter may not run, and ENOENT for a missing interpreter) is
		// skipped for the next directory's; the first that starts is the answer, whatever its exit status. When none
		// starts, the last start failure is the answer, and the round stays silent (CRW-977).
		result := CodexRunResult{Stderr: errSelfHealReportENOENT.Error(), ExitCode: 1}
		for _, file := range candidates {
			if ctx.Err() != nil {
				break
			}
			var started bool
			var startErr error
			result, started, startErr = selfHealReportRunOne(ctx, file, args)
			if started || !selfHealReportSearchGoesOn(startErr) {
				break
			}
		}
		return result
	}
}

// selfHealReportSearchGoesOn is whether execvp tries the next PATH directory after an exec failed with err: glibc continues on
// EACCES (remembering it), ENOENT, ESTALE, ENOTDIR, ENODEV and ETIMEDOUT, and stops on anything else.
func selfHealReportSearchGoesOn(err error) bool {
	for _, errno := range []syscall.Errno{syscall.EACCES, syscall.ENOENT, syscall.ESTALE, syscall.ENOTDIR, syscall.ENODEV, syscall.ETIMEDOUT} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// selfHealReportRunOne runs one resolved codex. started is false when the process could not be started (the exec failed, startErr
// is why).
func selfHealReportRunOne(ctx context.Context, file string, args []string) (result CodexRunResult, started bool, startErr error) {
	var out, errOut selfHealReportCapture
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	budget := &selfHealReportBudget{cancel: cancel}
	out.budget, errOut.budget = budget, budget
	cmd := exec.CommandContext(run, file, args...)
	// spawnSync inherits process.env, so the child sees every variable the hook does (the
	// corpus stubs read their own control variables from it). Env stays nil: inherited.
	cmd.Stdout, cmd.Stderr = &out, &errOut
	// A cancelled invocation ends the probe and its descendants, and the answer is not held open
	// by a grandchild that inherited the pipe (skill/merge_build_check_go.go's pattern).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = selfHealReportWaitDelay
	runErr := cmd.Run()
	result = CodexRunResult{Stdout: source.DecodeUTF8(out.buffer.Bytes()), Stderr: source.DecodeUTF8(errOut.buffer.Bytes()), ExitCode: 1}
	// exec.ErrWaitDelay is a probe that exited while a descendant still held its output open: the
	// list that arrived may be cut short, and a cut list would read an enabled flag as off. spawnSync
	// has no timeout and waits for the whole list; the hook has a time limit, so the port ends the wait
	// and takes the list for a failed measurement (CRW-977, known-defects).
	if !budget.overflow && !errors.Is(runErr, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() >= 0 {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if cmd.ProcessState == nil && result.Stderr == "" && runErr != nil {
		result.Stderr = runErr.Error()
	}
	return result, cmd.Process != nil, runErr
}

// selfHealReportCandidates resolves codex against the supplied PATH, as spawnSync does: every directory's codex the caller may
// execute, in order. With none, the error is EACCES when some entry exists and ENOENT when none does.
func selfHealReportCandidates(env host.LookupEnv) ([]string, error) {
	path, set := env("PATH")
	if !set {
		path = "/usr/bin:/bin"
	}
	var denied bool
	var found []string
	for _, dir := range strings.Split(path, string(os.PathListSeparator)) {
		candidate, err := filepath.Abs(filepath.Join(dir, "codex"))
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(candidate)
		// libuv's PATH search (execvp) skips a candidate the caller may not execute (EACCES) and tries
		// the next directory, so the test is the access check with the effective ids, not an execute bit.
		if err == nil && !info.IsDir() && unix.Faccessat(unix.AT_FDCWD, candidate, unix.X_OK, unix.AT_EACCESS) == nil {
			found = append(found, candidate)
			continue
		}
		denied = denied || err == nil || os.IsPermission(err)
	}
	if len(found) > 0 {
		return found, nil
	}
	if denied {
		return nil, errSelfHealReportEACCES
	}
	return nil, errSelfHealReportENOENT
}

var (
	errSelfHealReportENOENT = errSelfHealReport("spawnSync codex ENOENT")
	errSelfHealReportEACCES = errSelfHealReport("spawnSync codex EACCES")
)

type errSelfHealReport string

func (e errSelfHealReport) Error() string { return string(e) }

// selfHealReportWaitDelay bounds how long a probe's output may be held open after it exited or was
// killed, the same bound internal/runtime/doctor's commandWaitDelay gives its codex probe: a
// descendant that inherited the pipe would otherwise hold the hook (and the session start) until it
// exits. A variable only so a test can shorten it.
var selfHealReportWaitDelay = 2 * time.Second

// selfHealReportProbeDeadline is the one deadline the round's codex calls share (CRW-1150). The
// SessionStart declaration allows the whole hook 20 seconds (K1); the deadline plus the pipe
// cleanup above ends the round well inside that, and a normal listing takes a fraction of a second.
// A variable only so a test can shorten it.
var selfHealReportProbeDeadline = 8 * time.Second

// selfHealReportBudget is spawnSync's default 1 MiB, shared across stdout and stderr.
type selfHealReportBudget struct {
	mu       sync.Mutex
	used     int
	overflow bool
	cancel   context.CancelFunc
}

type selfHealReportCapture struct {
	budget *selfHealReportBudget
	buffer bytes.Buffer
}

func (w *selfHealReportCapture) Write(p []byte) (int, error) {
	w.budget.mu.Lock()
	defer w.budget.mu.Unlock()
	const limit = 1 << 20
	n := max(0, min(len(p), limit-w.budget.used))
	_, _ = w.buffer.Write(p[:n])
	w.budget.used += len(p)
	if w.budget.used > limit {
		w.budget.overflow = true
		// A probe that keeps writing is ended here rather than drained forever (featureCapture).
		w.budget.cancel()
	}
	return len(p), nil
}
