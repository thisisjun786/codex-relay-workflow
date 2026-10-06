package doctor

// This file is the red-first proof of the CRW-748 change. HarnessOptions.CodexHome and
// HarnessOptions.PluginKey carry the oracle three states -- nil absent, a pointer the explicit
// value, a pointer to the empty string the explicit empty value -- as SessionID and AgentID
// already do, so the port reads them with the oracle `??` (doctor.ts:402, :469, :476) instead of
// treating "" as absent. Before the change the field was a plain string: the install check PASSed
// against another installation named by CODEX_HOME where the oracle answers the relative
// ./plugins/cache WARN (the probe output saved with this issue red evidence), and the trust check
// adopted the single candidate for an explicitly empty key where the oracle warns.
//
// Every case builds its own temporary root and never touches a real home. The relative cache and
// config paths resolve against the working directory, so each case pins the working directory to an
// empty temporary one (t.Chdir) and the assertions name the relative path the oracle joins.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// harnessOptionsPtr is the pointer an option case passes: a value becomes a pointer to itself, an
// empty value included.
func harnessOptionsPtr(value string) *string { return &value }

// harnessOptionsEnv is a host.LookupEnv over a literal map.
func harnessOptionsEnv(values map[string]string) host.LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

// harnessOptionsWrite writes one fixture file, creating its directory.
func harnessOptionsWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// harnessOptionsPlugin builds a plugin root whose manifest names crw, and a Codex home whose
// config.toml enables one install key of that plugin.
func harnessOptionsPlugin(t *testing.T) (plugin, home string) {
	t.Helper()
	dir := t.TempDir()
	plugin, home = filepath.Join(dir, "plugin"), filepath.Join(dir, "home")
	harnessOptionsWrite(t, filepath.Join(plugin, ".codex-plugin", "plugin.json"), `{"name":"crw","version":"1.2.3"}`)
	harnessOptionsWrite(t, filepath.Join(home, "config.toml"), "[plugins.\"crw@local\"]\nenabled = true\n")
	return plugin, home
}

// TestHarnessOptionsExplicitEmptyCodexHomeIsNotAbsent is the install-root half of the issue red-first
// case: an explicit empty option is kept verbatim, so the check looks at the relative
// ./plugins/cache while CODEX_HOME names another installation whose cache would PASS.
func TestHarnessOptionsExplicitEmptyCodexHomeIsNotAbsent(t *testing.T) {
	root := t.TempDir()
	payload := harnessInstallPayloadAt(t, root, "option-empty", harnessInstallValue(map[string]any{"name": "crw", "version": "0.4.0"}))
	env := harnessInstallHomeAt(t, root, "option-empty-env", [][]string{{"mkt", "crw", "0.4.0"}})
	t.Setenv("CODEX_HOME", env)
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Chdir(t.TempDir())

	check := HarnessInstalledRootCheck(payload, HarnessOptions{CodexHome: harnessOptionsPtr("")})
	want := "no plugin cache at " + filepath.Join("", "plugins", "cache") + " (running uninstalled?)"
	if check.Name != "install-root" || check.Severity != HarnessWarn || check.Evidence != want {
		t.Fatalf("explicit empty codexHome = %+v, want the install-root WARN %q", check, want)
	}

	// The control: nil is absent, so today resolution stands and CODEX_HOME cache is found. The
	// install-root PASS names no path, so the contrast is the severity: the same installation and the
	// same payload PASS through CODEX_HOME and WARN through an explicitly empty option.
	nilCheck := HarnessInstalledRootCheck(payload, HarnessOptions{})
	if nilCheck.Severity != HarnessPass {
		t.Fatalf("nil codexHome = %+v, want the CODEX_HOME PASS", nilCheck)
	}
}

// TestHarnessOptionsExplicitEmptyCodexHomeReachesTheHookTrustCheck is the hook half of the same
// case: the trust check reads config.toml under the option verbatim, so an explicit empty value
// reads the relative path and finds no install key, where nil reads CODEX_HOME and finds one.
func TestHarnessOptionsExplicitEmptyCodexHomeReachesTheHookTrustCheck(t *testing.T) {
	plugin, home := harnessOptionsPlugin(t)
	defer t.Chdir(t.TempDir())
	env := map[string]string{"CODEX_HOME": home}

	nilCheck := HarnessHookTrustCheck(plugin, HarnessOptions{}, harnessOptionsEnv(env))
	if !strings.Contains(nilCheck.Evidence, "crw@local") {
		t.Fatalf("nil codexHome = %+v, want the CODEX_HOME install key", nilCheck)
	}

	emptyCheck := HarnessHookTrustCheck(plugin, HarnessOptions{CodexHome: harnessOptionsPtr("")}, harnessOptionsEnv(env))
	if emptyCheck.Severity != HarnessWarn || emptyCheck.Evidence != "enabled install key is ambiguous (0): (none)" {
		t.Fatalf("explicit empty codexHome = %+v, want the relative-config WARN", emptyCheck)
	}
	if strings.Contains(emptyCheck.Evidence, home) {
		t.Fatalf("explicit empty codexHome reached CODEX_HOME: %+v", emptyCheck)
	}
}

// TestHarnessOptionsExplicitEmptyPluginKeyIsNotReplacedByTheSingleCandidate is the issue second
// red-first case: the oracle reads options.pluginKey ?? (candidates.length === 1 ? candidates[0] :
// null) (doctor.ts:476), so an explicit empty key is kept and the check WARNs instead of adopting
// the one candidate.
func TestHarnessOptionsExplicitEmptyPluginKeyIsNotReplacedByTheSingleCandidate(t *testing.T) {
	plugin, home := harnessOptionsPlugin(t)
	empty := HarnessHookTrustCheck(plugin, HarnessOptions{CodexHome: harnessOptionsPtr(home), PluginKey: harnessOptionsPtr("")}, harnessOptionsEnv(nil))
	if empty.Severity != HarnessWarn || empty.Evidence != "enabled install key is ambiguous (1): crw@local" {
		t.Fatalf("explicit empty pluginKey = %+v, want the ambiguous-key WARN naming the one candidate", empty)
	}

	// The control: nil is absent, so the single candidate is adopted and the check proceeds.
	nilCheck := HarnessHookTrustCheck(plugin, HarnessOptions{CodexHome: harnessOptionsPtr(home)}, harnessOptionsEnv(nil))
	if nilCheck.Evidence == empty.Evidence || !strings.Contains(nilCheck.Evidence, "crw@local") {
		t.Fatalf("nil pluginKey = %+v, want the single-candidate path", nilCheck)
	}
}

// TestHarnessOptionsExplicitEmptyCodexHomeReachesTheHookExecutionCheck covers the other reader of
// the same option (harness_hooks.go:62-66, doctor.ts:452): the observation query reads the store
// under the option verbatim, so an explicitly empty value reads the relative layout and finds no
// records where nil reads CODEX_HOME.
func TestHarnessOptionsExplicitEmptyCodexHomeReachesTheHookExecutionCheck(t *testing.T) {
	plugin, home := harnessOptionsPlugin(t)
	t.Chdir(t.TempDir())
	session := "rec-s1"
	env := map[string]string{"CODEX_HOME": home, "PLUGIN_ROOT": plugin}
	// The Go writer lays one invocation record under CODEX_HOME; only the nil option finds it.
	if !harness.RecordInvocation(`{"session_id":"rec-s1"}`, "cxc-ops", "session-start", harnessOptionsEnv(env)) {
		t.Fatal("the invocation record was not written")
	}

	nilCheck := HarnessHookExecutionCheck(plugin, HarnessOptions{SessionID: &session}, harnessOptionsEnv(env), time.Now())
	emptyCheck := HarnessHookExecutionCheck(plugin, HarnessOptions{CodexHome: harnessOptionsPtr(""), SessionID: &session}, harnessOptionsEnv(env), time.Now())
	if nilCheck.Evidence == emptyCheck.Evidence {
		t.Fatalf("an explicitly empty codexHome read the same store as nil: %q", nilCheck.Evidence)
	}
	if nilCheck.Severity != HarnessPass {
		t.Fatalf("nil codexHome = %q, want the recorded invocation", nilCheck.Evidence)
	}
	if !strings.Contains(emptyCheck.Evidence, "no invocation records") {
		t.Fatalf("explicit empty codexHome = %q, want the relative store to hold no records", emptyCheck.Evidence)
	}
	if strings.Contains(emptyCheck.Evidence, home) {
		t.Fatalf("explicit empty codexHome reached CODEX_HOME: %q", emptyCheck.Evidence)
	}
}

// TestHarnessRunParseOptionsFillsTheOptionOnlyForAGivenFlag is the CLI half: the oracle refuses an
// empty flag value (cli.ts:49) and otherwise stores what it was given, so the parser answers nil
// CodexHome when no flag was given (the checks resolve CODEX_HOME and the home themselves) and a
// pointer only for a flag it accepted.
func TestHarnessRunParseOptionsFillsTheOptionOnlyForAGivenFlag(t *testing.T) {
	env := harnessRunEnv(map[string]string{"CODEX_HOME": "/env-codex-home", "HOME": "/env-home"})
	options, err := harnessRunParseOptions(nil, env)
	if err != nil {
		t.Fatal(err)
	}
	if options.CodexHome != nil || options.PluginKey != nil {
		t.Fatalf("no flag = %+v, want nil options", options)
	}

	options, err = harnessRunParseOptions([]string{"--codex-home", "/flag-home", "--key", "crw@local"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if options.CodexHome == nil || *options.CodexHome != "/flag-home" {
		t.Fatalf("--codex-home = %+v, want the flag value", options.CodexHome)
	}
	if options.PluginKey == nil || *options.PluginKey != "crw@local" {
		t.Fatalf("--key = %+v, want the flag value", options.PluginKey)
	}

	// The oracle refuses an empty value for both flags (cli.ts:49), so an explicit empty string is
	// expressible only through the Go API, which is where the two cases above sit.
	for _, args := range [][]string{{"--codex-home", ""}, {"--key", ""}} {
		if _, err := harnessRunParseOptions(args, env); err == nil || err.Error() != args[0]+" requires a value" {
			t.Fatalf("%v error = %v, want %q", args, err, args[0]+" requires a value")
		}
	}
}
