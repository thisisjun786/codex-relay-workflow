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
// harness branch reads the host-provided PLUGIN_ROOT instead (the port's convention,
// internal/harness/observation.go and internal/role/spawn/hook.go). The difference is a defect
// line in docs/port-cxc/known-defects.md.
package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// The oracle's paths and probe timeouts (doctor.ts:263, :291, :313, :91, :350).
const (
	harnessRunManifestRelative = ".codex-plugin/plugin.json"
	harnessRunSkillsDir        = "skills"
	harnessRunAgentsDir        = "agents"
	harnessRunVersionTimeout   = 5 * time.Second
	harnessRunFeaturesTimeout  = 8 * time.Second
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
	checks = append(checks, HarnessInstalledRootCheck(pluginRoot, options, env))
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
		return &value
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
// comes from PLUGIN_ROOT; without it there is nothing to diagnose, so the command is a usage error.
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
	pluginRoot, _ := env("PLUGIN_ROOT")
	if pluginRoot == "" {
		fmt.Fprintln(stderr, "crw doctor harness: PLUGIN_ROOT is not set; the harness checks read the plugin package it names")
		return usageExit
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
// --codex-home <value>, and anything else is the "unknown hooks option" the catch prints. An option
// is filled only when its flag was given -- the checks resolve CODEX_HOME and the home themselves
// -- and an empty flag value is refused, as the oracle's `if (!value) throw` refuses it (cli.ts:49).
func harnessRunParseOptions(args []string, env host.LookupEnv) (HarnessOptions, error) {
	options := HarnessOptions{}
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
			if value == "" {
				// The oracle refuses an empty value too (`if (!value) throw`, cli.ts:49), so an
				// explicitly empty option is expressible through the Go API only.
				return options, errors.New(arg + " requires a value")
			}
			if arg == "--key" {
				options.PluginKey = harnessRunString(value)
			} else if resolved, err := filepath.Abs(value); err == nil {
				options.CodexHome = harnessRunString(resolved)
			} else {
				options.CodexHome = harnessRunString(value)
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
	if ctx.Err() == context.DeadlineExceeded || errors.Is(err, exec.ErrWaitDelay) {
		return HarnessRun{Status: harnessRunInt(harnessDriftKilled), Stdout: stdout.String(), Stderr: stderr.String()}
	}
	if err == nil {
		return HarnessRun{Status: harnessRunInt(0), Stdout: stdout.String(), Stderr: stderr.String()}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code := exit.ExitCode()
		return HarnessRun{Status: &code, Stdout: stdout.String(), Stderr: stderr.String()}
	}
	return HarnessRun{Status: nil, Stdout: stdout.String(), Stderr: stderr.String()}
}

func harnessRunInt(value int) *int { return &value }
