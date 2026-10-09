package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/configguard"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// CXC v0.2.40 config-guard/src/cli.ts:38-47, with the dropped uninstall alias omitted.
const featureUsage = "Usage:\n" +
	"  crw install features enable       activate declared Codex feature flags\n" +
	"  crw install features disable      revert flags crw enabled when safe\n" +
	"  crw install features status       show declared feature-flag state\n\n" +
	"  --help / -h / help in any argument position prints this text and writes nothing.\n" +
	"  enable writes $CODEX_HOME/config.toml and a timestamped .bak; disable reverts.\n"

// This surface keeps the oracle's text and exit codes, outside the installer's JSON verbs.
func runFeatures(ctx context.Context, args []string, env scope.Env, stdout, stderr io.Writer) int {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" || arg == "help" {
			fmt.Fprint(stdout, featureUsage)
			return 0
		}
	}
	if len(args) == 0 || args[0] != "enable" && args[0] != "disable" && args[0] != "status" {
		fmt.Fprintln(stderr, "usage: crw install features <enable|disable|status>")
		return 2
	}
	home, err := resolveFeatureHome(env)
	if err != nil {
		fmt.Fprintln(stderr, "crw: "+err.Error())
		return 1
	}
	run := featureRunner(ctx, env)
	switch args[0] {
	case "enable":
		var m *configguard.InstallManifest
		m, err = configguard.Activate(configguard.ActivateDeps{Run: run, CodexHome: home})
		if err == nil {
			// Explicit enable resumes healing; the optional marker never gates activation.
			_ = configguard.ClearSelfHealOptOut(home)
			renderFeatureEnable(stdout, stderr, m)
		}
	case "disable":
		var result *configguard.DeactivateResult
		result, err = configguard.Deactivate(configguard.DeactivateDeps{Run: run, CodexHome: home})
		if result != nil {
			renderFeatureDisable(stdout, result)
		}
		if err == nil && result != nil && len(result.Failed) > 0 {
			// A flag crw could not disable fails the command (CRW-1145); the ownership stays recorded for a retry.
			for _, f := range result.Failed {
				fmt.Fprintf(stderr, "crw: could not disable '%s' (exit %d): %s\n", f.Key, f.ExitCode, f.Message)
			}
			fmt.Fprintln(stderr, "crw: the flags above are still recorded as crw's; run 'crw install features disable' again once codex can disable them")
			return 1
		}
	case "status":
		var state map[string]bool
		state, err = configguard.ReadDeclaredState(run)
		if err == nil {
			for _, key := range configguard.DeclaredFeatures() {
				value := "disabled"
				if state[string(key)] {
					value = "enabled"
				}
				fmt.Fprintf(stdout, "%s: %s\n", key, value)
			}
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "crw: "+err.Error())
		return 1
	}
	return 0
}

// The oracle's resolveCodexHome: trim CODEX_HOME, then os.homedir(). HOME="" is
// different from absent HOME: joining the empty value yields a relative .codex.
func resolveFeatureHome(env scope.Env) (string, error) {
	if home := text.Trim(env.Get("CODEX_HOME")); home != "" {
		return home, nil
	}
	base, set := environOf(env)("HOME")
	if !set {
		var err error
		base, err = record.Home(environOf(env))
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(base, ".codex"), nil
}

func featureList(keys []string) string {
	if len(keys) == 0 {
		return "none"
	}
	return strings.Join(keys, ", ")
}

func renderFeatureEnable(stdout, stderr io.Writer, m *configguard.InstallManifest) {
	var enabled, failed, keys []string
	for _, key := range configguard.DeclaredFeatures() {
		r := m.Flags[string(key)]
		if r.EnabledByCodexclaw {
			enabled = append(enabled, string(key))
		}
		if r.EnableFailed {
			failed = append(failed, string(key))
		}
	}
	for _, entry := range configguard.AutoEnabledManagedKeys() {
		id := configguard.ManagedKeyID(entry)
		if m.TableKeys[id].SetByCodexclaw {
			keys = append(keys, id)
		}
	}
	if m.Unchanged {
		// Nothing was changed and nothing published (CRW-1145).
		fmt.Fprintf(stdout, "crw: already enabled [%s]; nothing changed\n", featureList(enabled))
		return
	}
	fmt.Fprintf(stdout, "crw: enabled [%s]", featureList(enabled))
	if len(keys) > 0 {
		fmt.Fprintf(stdout, "\nconfig keys: %s", strings.Join(keys, ", "))
	}
	for _, id := range keys {
		if entry := configguard.FindManagedKey(id); entry != nil && entry.Caution != "" {
			fmt.Fprintf(stdout, "\n  %s", entry.Caution)
		}
	}
	if m.RunBackupPath != nil && *m.RunBackupPath != "" {
		fmt.Fprintf(stdout, "\nbackup: %s", *m.RunBackupPath)
	}
	fmt.Fprintln(stdout)
	for _, key := range failed {
		rec := m.Flags[key]
		fmt.Fprint(stderr, featureWarning(key, &rec))
	}
}

func featureWarning(key string, rec *configguard.FlagRecord) string {
	impact := configguard.SoftFeatureImpact()[configguard.DeclaredFeature(key)]
	if impact == "" {
		impact = "이 플래그에 의존하는 기능이 비활성화된다."
	}
	message := fmt.Sprintf("crw: 경고 — '%s' 를 켤 수 없었다", key)
	if rec != nil && rec.Failure != nil {
		message += " (exit " + strconv.FormatFloat(rec.Failure.ExitCode, 'f', -1, 64) + ")"
	}
	message += "\n  영향: " + impact
	if rec != nil && rec.Failure != nil && rec.Failure.Message != "" {
		message += "\n  codex: " + rec.Failure.Message
	}
	return message + "\n  확인: codex features list | grep " + key + "\n  수동: codex features enable " + key + "\n"
}

func renderFeatureDisable(stdout io.Writer, r *configguard.DeactivateResult) {
	if r.NoManifest {
		fmt.Fprintln(stdout, "crw: no install manifest; nothing to revert")
		return
	}
	if r.Released {
		fmt.Fprintln(stdout, "crw: already disabled; nothing to revert")
		return
	}
	fmt.Fprintf(stdout, "crw: disabled [%s]; kept pre-existing [%s]\n", featureList(r.Disabled), featureList(r.SkippedPreExisting))
	if len(r.RestoredKeys) > 0 {
		fmt.Fprintf(stdout, "restored keys: %s\n", strings.Join(r.RestoredKeys, ", "))
	}
	if len(r.SkippedExternal) > 0 {
		details := make([]string, 0, len(r.SkippedExternal))
		for _, skipped := range r.SkippedExternal {
			details = append(details, skipped.Target+" ("+string(skipped.Reason)+")")
		}
		fmt.Fprintf(stdout, "left to their current owner: %s\n", strings.Join(details, ", "))
	}
	if r.FileDrifted {
		fmt.Fprintln(stdout, "note: config.toml changed since activation; reverted per key")
	}
	if r.FeaturesStateUnavailable {
		fmt.Fprintln(stdout, "note: could not read 'codex features list'; reverted flags from the manifest alone")
	}
}

// Resolve against the supplied PATH: exec.Command's own lookup uses this process's environment.
func featureBinary(env scope.Env) (string, error) {
	path := env.Get("PATH")
	if _, set := environOf(env)("PATH"); !set {
		path = "/usr/bin:/bin"
	}
	var denied bool
	for _, dir := range strings.Split(path, string(os.PathListSeparator)) {
		candidate, err := filepath.Abs(filepath.Join(dir, "codex"))
		if err != nil {
			return "", err
		}
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode().Perm()&0111 != 0 {
			return candidate, nil
		}
		denied = denied || err == nil || errors.Is(err, os.ErrPermission)
	}
	if denied {
		return "", errors.New("spawnSync codex EACCES")
	}
	return "", errors.New("spawnSync codex ENOENT")
}

// Node spawnSync's default 1 MiB is shared across stdout and stderr (skill/search's pattern).
type featureBudget struct {
	mu       sync.Mutex
	used     int
	overflow bool
	cancel   context.CancelFunc
}
type featureCapture struct {
	budget *featureBudget
	buffer bytes.Buffer
}

func (w *featureCapture) Write(p []byte) (int, error) {
	w.budget.mu.Lock()
	defer w.budget.mu.Unlock()
	const limit = 1 << 20
	n := max(0, min(len(p), limit-w.budget.used))
	_, _ = w.buffer.Write(p[:n])
	w.budget.used += len(p)
	if w.budget.used > limit {
		w.budget.overflow = true
		w.budget.cancel()
	}
	return len(p), nil
}

func featureRunner(ctx context.Context, env scope.Env) configguard.CodexRunner {
	return func(args []string) configguard.CodexRunResult {
		file, err := featureBinary(env)
		if err != nil {
			return configguard.CodexRunResult{Stderr: err.Error(), ExitCode: 1}
		}
		run, cancel := context.WithCancel(ctx)
		defer cancel()
		budget := featureBudget{cancel: cancel}
		out, errOut := featureCapture{budget: &budget}, featureCapture{budget: &budget}
		cmd := exec.CommandContext(run, file, args...)
		cmd.Env, cmd.Stdout, cmd.Stderr = env, &out, &errOut
		err = cmd.Run()
		result := configguard.CodexRunResult{Stdout: source.DecodeUTF8(out.buffer.Bytes()), Stderr: source.DecodeUTF8(errOut.buffer.Bytes()), ExitCode: 1}
		if !budget.overflow && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() >= 0 {
			result.ExitCode = cmd.ProcessState.ExitCode()
		}
		if cmd.ProcessState == nil && result.Stderr == "" && err != nil {
			result.Stderr = err.Error()
		}
		return result
	}
}
