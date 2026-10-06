package configguard

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
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
	Run       CodexRunner
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
	if marker != nil && marker.AllEnabled != nil && *marker.AllEnabled && cacheCoversCurrentKeys &&
		marker.ConfigMtimeMs != nil && configMtimeMs != nil && *marker.ConfigMtimeMs == *configMtimeMs {
		return []SelfHealReportOutcome{{Action: SelfHealReportSkipped, Reason: SelfHealReasonCached}}
	}

	state, err := ReadDeclaredState(deps.Run)
	if err != nil {
		// Measurement failure, not a state. Nothing is cached (the oracle leaves the marker as it
		// was) and nothing is said.
		return []SelfHealReportOutcome{{Action: SelfHealReportUnavailable, Message: err.Error()}}
	}

	healedKeys := []string{}
	if marker != nil {
		healedKeys = marker.HealedKeys
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
	context := RenderSelfHealReportContext(SelfHealReport(SelfHealReportDeps{CodexHome: home, Run: SelfHealReportRunner(env)}))
	if context == "" {
		return 0
	}
	answer := `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":` +
		pyjson.Dumps(context, pyjson.Options{Compact: true, Unicode: true}) + "}}\n"
	if _, err := io.WriteString(out, answer); err != nil {
		return 0
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

// selfHealReportMtimeMs is statSync(path).mtimeMs; nil when the path does not exist.
func selfHealReportMtimeMs(path string) *float64 {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	ms := float64(info.ModTime().UnixNano()) / 1e6
	return &ms
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
func SelfHealReportRunner(env host.LookupEnv) CodexRunner {
	return func(args []string) CodexRunResult {
		file, err := selfHealReportBinary(env)
		if err != nil {
			return CodexRunResult{Stderr: err.Error(), ExitCode: 1}
		}
		var out, errOut selfHealReportCapture
		budget := &selfHealReportBudget{}
		out.budget, errOut.budget = budget, budget
		cmd := exec.Command(file, args...)
		// spawnSync inherits process.env, so the child sees every variable the hook does (the
		// corpus stubs read their own control variables from it). Env stays nil: inherited.
		cmd.Stdout, cmd.Stderr = &out, &errOut
		runErr := cmd.Run()
		result := CodexRunResult{Stdout: source.DecodeUTF8(out.buffer.Bytes()), Stderr: source.DecodeUTF8(errOut.buffer.Bytes()), ExitCode: 1}
		if !budget.overflow && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() >= 0 {
			result.ExitCode = cmd.ProcessState.ExitCode()
		}
		if cmd.ProcessState == nil && result.Stderr == "" && runErr != nil {
			result.Stderr = runErr.Error()
		}
		return result
	}
}

// selfHealReportBinary resolves codex against the supplied PATH, as spawnSync does.
func selfHealReportBinary(env host.LookupEnv) (string, error) {
	path, set := env("PATH")
	if !set {
		path = "/usr/bin:/bin"
	}
	var denied bool
	for _, dir := range strings.Split(path, string(os.PathListSeparator)) {
		candidate, err := filepath.Abs(filepath.Join(dir, "codex"))
		if err != nil {
			return "", err
		}
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
		denied = denied || err == nil || os.IsPermission(err)
	}
	if denied {
		return "", errSelfHealReportEACCES
	}
	return "", errSelfHealReportENOENT
}

var (
	errSelfHealReportENOENT = errSelfHealReport("spawnSync codex ENOENT")
	errSelfHealReportEACCES = errSelfHealReport("spawnSync codex EACCES")
)

type errSelfHealReport string

func (e errSelfHealReport) Error() string { return string(e) }

// selfHealReportBudget is spawnSync's default 1 MiB, shared across stdout and stderr.
type selfHealReportBudget struct {
	mu       sync.Mutex
	used     int
	overflow bool
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
	}
	return len(p), nil
}
