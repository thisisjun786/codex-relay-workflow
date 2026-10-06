// Harness doctor assembly, ported from CXC v0.2.40 cxc-ops/src/doctor.ts (commit
// 3c1459acadeb1906d97c00a598e1457327ae372d): runDoctor (:261-360), the three checks it builds
// inline -- the plugin manifest (:263-279), skills (:291-311) and agents (:313-323) -- and its
// metadata (:362-378). The command that runs it is the doctor path of cxc-ops/src/cli.ts
// (:80-93): text unless --json, exit 1 on an overall FAIL, and the option parser (:46-66) whose
// catch the entry point prints as "cxc-ops error: <message>" (:155-162).
//
// Two name rules apply (decision 1, contract/schema/cxc/name-substitution.json): R32 makes the
// component word crw, the cli table maps cxc doctor to crw doctor harness, and R29 renames the
// component word in the catch prefix to crw-ops. Every other check name, severity, evidence
// string and repair hint is the oracle text.
//
// Windows and WSL are out of the port scope (inventory.md: win-exec.ts and wsl.ts are OUT): the
// WSL check answers the branch the oracle takes off WSL. Every function that would run codex or
// python3 takes the runner as an argument, so no caller or test needs a real binary.
//
// The oracle derives its plugin root from its own module path (cli.ts:27-31); a Go binary
// installed through the runtime pointer has no such relation to the plugin package, so the
// harness branch takes the host-provided PLUGIN_ROOT when it is set (the port's convention,
// internal/harness/observation.go and internal/role/spawn/hook.go) and otherwise the one crw
// plugin root the Codex home's cache holds (harnessRunRoot). The difference from the oracle's
// module-derived root is the defect line this issue fixes in docs/port-cxc/known-defects.md.
package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// The oracle's paths and probe timeouts (doctor.ts:263, :291, :313, :91, :350).
const (
	harnessRunManifestRelative = ".codex-plugin/plugin.json"
	harnessRunSkillsDir        = "skills"
	harnessRunAgentsDir        = "agents"
	harnessRunVersionTimeout   = 5 * time.Second
	harnessRunFeaturesTimeout  = 8 * time.Second
	// harnessRunRootPluginFolder is the plugin folder the installed root sits under in the Codex
	// home's plugin cache: <codexHome>/plugins/cache/<marketplace>/crw/<version>.
	harnessRunRootPluginFolder = "crw"
)

// RunHarnessDoctor is runDoctor (doctor.ts:261-360): assemble every check of the CXC plugin slice
// in the oracle's push order and roll the severities up. pluginRoot is the plugin package the
// checks read; runner is the injected subprocess seam; options carries the Codex home, plugin key
// and session; projectRoot is the process working directory the pabcd check reads; env and now are
// the ambient reads the oracle makes (doctor.ts:369, :452).
func RunHarnessDoctor(pluginRoot string, runner HarnessRunner, options HarnessOptions, projectRoot string, env host.LookupEnv, now time.Time) HarnessReport {
	checks := make([]HarnessCheck, 0, 15)
	manifestPath := filepath.Join(pluginRoot, harnessRunManifestRelative)
	checks = append(checks, harnessRunManifestCheck(manifestPath))
	if harnessRunExists(manifestPath) {
		checks = append(checks, HarnessManifestTargetChecks(pluginRoot)...)
	}
	checks = append(checks, harnessRunSkillsCheck(pluginRoot))
	checks = append(checks, harnessRunAgentsCheck(pluginRoot))
	checks = append(checks, HarnessDriftChecks(pluginRoot)...)
	checks = append(checks, HarnessHookTrustCheck(pluginRoot, options, env))
	checks = append(checks, HarnessHookExecutionCheck(pluginRoot, options, env, now))
	checks = append(checks, HarnessAstGrepCheck(pluginRoot, runner))
	checks = append(checks, HarnessInstalledRootCheck(pluginRoot, options))
	checks = append(checks, HarnessPabcdCheck(projectRoot))
	checks = append(checks, HarnessFeaturesCheck(runner("codex", []string{"features", "list"}, harnessRunFeaturesTimeout)))
	checks = append(checks, HarnessWslCheck())

	return HarnessReport{
		SchemaVersion: HarnessSchemaVersion,
		Overall:       HarnessRollup(checks),
		Checks:        checks,
		PluginVersion: harnessRunPluginVersion(manifestPath),
		CodexVersion:  harnessReportDetectCodexVersion(runner),
		ActiveSurface: harnessRunActiveSurface(env),
	}
}

// harnessRunExists is existsSync (doctor.ts:265): a stat that answers false instead of throwing.
func harnessRunExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// harnessRunManifestCheck is the inline manifest check (doctor.ts:263-279): the document must
// parse and reference hooks. A missing file FAILs by name; an unreadable or unparseable one FAILs
// with the engine's message, and a JSON null FAILs as the oracle's property read throws.
func harnessRunManifestCheck(manifestPath string) HarnessCheck {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return HarnessCheck{Name: "manifest", Severity: HarnessFail, Evidence: "missing " + manifestPath}
		}
		return HarnessCheck{Name: "manifest", Severity: HarnessFail, Evidence: "unparseable plugin.json: " + harnessInstallString(err)}
	}
	manifest, err := harnessInstallParseJSON(raw)
	if err != nil {
		return HarnessCheck{Name: "manifest", Severity: HarnessFail, Evidence: "unparseable plugin.json: " + harnessInstallString(err)}
	}
	if manifest == nil {
		return HarnessCheck{Name: "manifest", Severity: HarnessFail, Evidence: "unparseable plugin.json: TypeError: Cannot read properties of null (reading 'hooks')"}
	}
	hookCount := 0
	if object, ok := manifest.(pyjson.Object); ok {
		if hooks, ok := object.Get("hooks").([]any); ok {
			hookCount = len(hooks)
		}
	}
	severity := HarnessWarn
	if hookCount > 0 {
		severity = HarnessPass
	}
	return HarnessCheck{Name: "manifest", Severity: severity, Evidence: fmt.Sprintf("plugin.json parsed, %d hook(s) referenced", hookCount)}
}

// harnessRunSkillsCheck is the inline skills check (doctor.ts:291-311): every directory under
// skills/ needs SKILL.md and agents/openai.yaml. An unreadable skills/ throws outside every catch
// in the oracle, so this panics with the same message for the CLI boundary to recover.
func harnessRunSkillsCheck(pluginRoot string) HarnessCheck {
	skillsDir := filepath.Join(pluginRoot, harnessRunSkillsDir)
	if !harnessReportIsDir(skillsDir) {
		return HarnessCheck{Name: "skills", Severity: HarnessWarn, Evidence: "no skills/ directory"}
	}
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		panic(harnessInstallScandirError(err))
	}
	dirs := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := harnessInstallNodeName(entry.Name())
		if harnessReportIsDir(filepath.Join(skillsDir, name)) {
			dirs = append(dirs, name)
		}
	}
	broken := make([]string, 0)
	for _, name := range dirs {
		if !harnessRunExists(filepath.Join(skillsDir, name, "SKILL.md")) || !harnessRunExists(filepath.Join(skillsDir, name, "agents", "openai.yaml")) {
			broken = append(broken, name)
		}
	}
	if len(broken) == 0 {
		return HarnessCheck{Name: "skills", Severity: HarnessPass, Evidence: fmt.Sprintf("%d skill(s) each have SKILL.md + agents/openai.yaml", len(dirs))}
	}
	return HarnessCheck{Name: "skills", Severity: HarnessFail, Evidence: "incomplete skill(s): " + strings.Join(broken, ", ")}
}

// harnessRunAgentsCheck is the inline agents check (doctor.ts:313-323): the role TOMLs of the
// spawn configuration. An unreadable agents/ panics as the skills check does.
func harnessRunAgentsCheck(pluginRoot string) HarnessCheck {
	agentsDir := filepath.Join(pluginRoot, harnessRunAgentsDir)
	if !harnessReportIsDir(agentsDir) {
		return HarnessCheck{Name: "agents", Severity: HarnessWarn, Evidence: "no agents/ directory"}
	}
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		panic(harnessInstallScandirError(err))
	}
	tomls := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := harnessInstallNodeName(entry.Name())
		if strings.HasSuffix(name, ".toml") {
			tomls = append(tomls, name)
		}
	}
	if len(tomls) > 0 {
		return HarnessCheck{Name: "agents", Severity: HarnessPass, Evidence: fmt.Sprintf("%d role TOML(s): %s", len(tomls), strings.Join(tomls, ", "))}
	}
	return HarnessCheck{Name: "agents", Severity: HarnessWarn, Evidence: "no role TOMLs"}
}

// harnessRunPluginVersion is the report metadata read (doctor.ts:362-367): the manifest version
// when it is a string, nil when the manifest is unreadable, unparseable, not an object or has no
// string version (the oracle's undefined).
func harnessRunPluginVersion(manifestPath string) *string {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil
	}
	manifest, err := harnessInstallParseJSON(raw)
	if err != nil {
		return nil
	}
	object, ok := manifest.(pyjson.Object)
	if !ok {
		return nil
	}
	version, ok := object.Get("version").(string)
	if !ok {
		return nil
	}
	return &version
}

// harnessRunActiveSurface is activeSurface (doctor.ts:369): CODEX_SURFACE when it is set at all
// (the oracle's ?? keeps an empty string), else "app" when CODEX_APP_PORT is truthy, else nil.
func harnessRunActiveSurface(env host.LookupEnv) *string {
	if value, set := env("CODEX_SURFACE"); set {
		// An environment value is bytes Node's UTF-8 decoder already read: the raw ED A0 80 becomes
		// three U+FFFD there, where harnessReportJSONString reads the same bytes as a stored lone
		// surrogate. Decode here so the two do not disagree (source.DecodeUTF8 is that decoder).
		return harnessRunString(source.DecodeUTF8([]byte(value)))
	}
	if value, set := env("CODEX_APP_PORT"); set && value != "" {
		return harnessRunString("app")
	}
	return nil
}

func harnessRunString(value string) *string { return &value }

// RunHarnessDoctorCLI is the doctor path of cxc-ops/src/cli.ts (cli.ts:80-93): parse the options,
// assemble the report, print it as text unless --json and exit 1 on an overall FAIL. An option the
// parser rejects, a check that throws where the oracle throws outside every catch, and a working
// directory that cannot be read all take the entry point's catch (cli.ts:155-162). The plugin root
// is the host-provided PLUGIN_ROOT when it is set, else the one crw plugin root the Codex home's
// cache holds (harnessRunRoot); a root that cannot be resolved is the same catch, not the usage exit.
func RunHarnessDoctorCLI(args []string, stdout, stderr io.Writer, env host.LookupEnv, getwd func() (string, error), runner HarnessRunner, now time.Time) int {
	jsonMode := false
	hookArgs := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--json" {
			jsonMode = true
			continue
		}
		hookArgs = append(hookArgs, arg)
	}
	options, err := harnessRunParseOptions(hookArgs, env)
	if err != nil {
		return harnessRunCatch(stderr, err)
	}
	pluginRoot, err := harnessRunRoot(options, env)
	if err != nil {
		return harnessRunCatch(stderr, err)
	}
	projectRoot, err := getwd()
	if err != nil {
		return harnessRunCatch(stderr, err)
	}
	report, err := harnessRunDoctorRecovered(pluginRoot, runner, options, projectRoot, env, now)
	if err != nil {
		return harnessRunCatch(stderr, err)
	}
	if jsonMode {
		if err := harnessRunWriteJSON(stdout, report); err != nil {
			return 1
		}
	} else {
		if _, err := fmt.Fprintln(stdout, RenderHarnessReport(report)); err != nil {
			return 1
		}
	}
	if report.Overall == HarnessFail {
		return 1
	}
	return 0
}

// harnessRunRoot is the plugin package the harness checks read. The oracle derives it from its
// own module path (cli.ts:27-31), which a Go binary installed through the runtime pointer has no
// relation to; the port takes the host-provided PLUGIN_ROOT when it is set (the port's convention,
// internal/harness/observation.go and internal/role/spawn/hook.go) and otherwise the one crw
// plugin root the Codex home's cache holds, found as HarnessInstalledRootCheck finds it.
//
// Exactly one root is diagnosed. No root, several roots or a cache that cannot be read is the
// catch path (cli.ts:155-162), never a guessed root and never the usage exit: the command has an
// answer to give either way.
func harnessRunRoot(options HarnessOptions, env host.LookupEnv) (string, error) {
	if value, set := env("PLUGIN_ROOT"); set && value != "" {
		return value, nil
	}
	codexHome, err := harnessInstallCodexHome(options.CodexHome, record.Environ(env), harnessInstallPasswdHome)
	if err != nil {
		return "", err
	}
	cacheRoot := filepath.Join(codexHome, "plugins", "cache")
	found, err := harnessRunRootScan(cacheRoot)
	if err != nil {
		return "", err
	}
	if len(found) == 1 {
		return found[0], nil
	}
	if len(found) == 0 {
		return "", fmt.Errorf("no installed crw plugin under %s: no plugins/cache/<marketplace>/crw/<version> directory holds .codex-plugin/plugin.json; set PLUGIN_ROOT to the plugin package to diagnose", cacheRoot)
	}
	return "", fmt.Errorf("%d installed crw plugin roots under %s (%s); set PLUGIN_ROOT to the one to diagnose", len(found), cacheRoot, strings.Join(found, ", "))
}

// harnessRunRootScan lists the version directories under cacheRoot that hold a plugin
// manifest, the way harnessInstallRootBody scans the same tree (harness_install.go): every
// marketplace segment, the crw folder, then each version. A path that is simply absent
// contributes nothing; a directory that exists and cannot be read is an error, because the scan
// then cannot see the whole cache and no root may be picked from an incomplete count.
func harnessRunRootScan(cacheRoot string) ([]string, error) {
	markets, err := os.ReadDir(cacheRoot)
	if err != nil {
		if harnessRunRootMissing(err) {
			return nil, nil
		}
		return nil, harnessRunRootUnreadable(cacheRoot, err)
	}
	found := []string{}
	for _, market := range markets {
		dir := filepath.Join(cacheRoot, harnessInstallNodeName(market.Name()), harnessRunRootPluginFolder)
		versions, err := os.ReadDir(dir)
		if err != nil {
			if harnessRunRootMissing(err) {
				continue
			}
			return nil, harnessRunRootUnreadable(dir, err)
		}
		for _, entry := range versions {
			root := filepath.Join(dir, harnessInstallNodeName(entry.Name()))
			manifest := filepath.Join(root, harnessRunManifestRelative)
			if _, err := os.Stat(manifest); err != nil {
				if harnessRunRootMissing(err) {
					continue
				}
				return nil, harnessRunRootUnreadable(manifest, err)
			}
			found = append(found, root)
		}
	}
	return found, nil
}

// harnessRunRootMissing reports the failures that mean there is nothing there: a path that does
// not exist, or one whose parent is not a directory. Any other failure leaves the scan blind.
func harnessRunRootMissing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// harnessRunRootUnreadable is the refusal for a cache the scan cannot see whole: an unreadable
// directory may hold another installed root, so choosing the ones that remain would be a guess.
func harnessRunRootUnreadable(path string, err error) error {
	return fmt.Errorf("cannot read %s: %v; the plugin cache must be readable to pick the installed root, or set PLUGIN_ROOT to the plugin package to diagnose", path, err)
}

// harnessRunCatch is the entry point's catch (cli.ts:155-162): the message on stderr, exit 1.
func harnessRunCatch(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "crw-ops error: "+err.Error())
	return 1
}

// harnessRunDoctorRecovered turns the panic a check throws where the oracle throws outside every
// catch into an error, the CLI boundary's shape of that throw.
func harnessRunDoctorRecovered(pluginRoot string, runner HarnessRunner, options HarnessOptions, projectRoot string, env host.LookupEnv, now time.Time) (report HarnessReport, err error) {
	defer func() {
		if value := recover(); value != nil {
			if thrown, ok := value.(error); ok {
				err = thrown
				return
			}
			err = errors.New(fmt.Sprint(value))
		}
	}()
	return RunHarnessDoctor(pluginRoot, runner, options, projectRoot, env, now), nil
}

// harnessRunParseOptions is parseHookOptions (cli.ts:46-66): --bootstrap-ok, --key <value> and
// --codex-home <value>, and anything else is the "unknown hooks option" the catch prints. The
// default Codex home is CODEX_HOME when set (the oracle's ?? keeps an empty string), else the
// account's ~/.codex.
func harnessRunParseOptions(args []string, env host.LookupEnv) (HarnessOptions, error) {
	options := HarnessOptions{}
	if value, set := env("CODEX_HOME"); set {
		options.CodexHome = value
	} else {
		home, err := host.Home(env)
		if err != nil {
			return options, err
		}
		options.CodexHome = filepath.Join(home, ".codex")
	}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "--bootstrap-ok":
			// The bootstrap flag belongs to the retrust write; the doctor path accepts it and does nothing.
		case "--key", "--codex-home":
			if index+1 >= len(args) {
				return options, errors.New(arg + " requires a value")
			}
			value := args[index+1]
			if arg == "--key" {
				options.PluginKey = value
			} else if resolved, err := filepath.Abs(value); err == nil {
				options.CodexHome = resolved
			} else {
				options.CodexHome = value
			}
			index++
		default:
			return options, errors.New("unknown hooks option: " + arg)
		}
	}
	return options, nil
}

// harnessRunWriteJSON is JSON.stringify(report, null, 2) + "\n" (cli.ts:83-84): two-space indent,
// a trailing newline and no HTML escaping, so <, >, & and the em dash stay as the oracle printed
// them.
func harnessRunWriteJSON(w io.Writer, report HarnessReport) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return err
	}
	_, err := w.Write(harnessRunUnescapeSeparators(buffer.Bytes()))
	return err
}

// harnessRunUnescapeSeparators rewrites the \u2028 and \u2029 escapes encoding/json always emits
// back to the characters themselves, which JSON.stringify writes literally (cli.ts:83). The
// report carries the manifest version and the environment's active surface, so a separator in
// either must not change the bytes. An escaped backslash (\\u2028) is data and stays as it is.
func harnessRunUnescapeSeparators(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); {
		if data[i] == '\\' && i+1 < len(data) {
			if i+5 < len(data) && data[i+1] == 'u' && (string(data[i+2:i+6]) == "2028" || string(data[i+2:i+6]) == "2029") {
				if string(data[i+2:i+6]) == "2028" {
					out = utf8.AppendRune(out, '\u2028')
				} else {
					out = utf8.AppendRune(out, '\u2029')
				}
				i += 6
				continue
			}
			out = append(out, data[i], data[i+1])
			i += 2
			continue
		}
		out = append(out, data[i])
		i++
	}
	return out
}

// harnessRunExec is the real HarnessRunner: run file with args under the timeout and answer the
// exit status and the two streams. A process the timeout killed answers harnessDriftKilled (the
// oracle's null status for a probe that did not finish, which the ast-grep check reads as "sg not
// resolved"); a process that could not start answers a nil status (the oracle's spawn error,
// which the ast-grep check reads as the missing interpreter and the features check as an
// unreadable probe).
func harnessRunExec(file string, args []string, timeout time.Duration) HarnessRun {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, file, args...)
	command.WaitDelay = commandWaitDelay
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	// The two streams are bytes a process wrote, and the oracle reads them through Node's UTF-8
	// decoder (spawnSync with encoding utf8): a raw invalid sequence is U+FFFD there, not a lone
	// surrogate harnessReportJSONString would spell as its escape. Decode here, so every consumer of
	// a run -- the codex version, the features stderr cut and the ast-grep output -- sees the same
	// text the oracle saw. A string a test passes to the check directly is unaffected.
	out, errOut := source.DecodeUTF8(stdout.Bytes()), source.DecodeUTF8(stderr.Bytes())
	if ctx.Err() == context.DeadlineExceeded || errors.Is(err, exec.ErrWaitDelay) {
		return HarnessRun{Status: harnessRunInt(harnessDriftKilled), Stdout: out, Stderr: errOut}
	}
	if err == nil {
		return HarnessRun{Status: harnessRunInt(0), Stdout: out, Stderr: errOut}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		return HarnessRun{Status: &code, Stdout: out, Stderr: errOut}
	}
	return HarnessRun{Status: nil, Stdout: out, Stderr: errOut}
}

func harnessRunInt(value int) *int { return &value }
