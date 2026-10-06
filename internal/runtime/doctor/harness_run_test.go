package doctor

// This file replays testdata/harness/run/oracle.json over RunHarnessDoctor and the "crw doctor
// harness" branch, and ports the B tests of CXC v0.2.40 cxc-ops/test/cxc-ops.test.ts that call
// runDoctor or the doctor CLI (schemaVersion, pluginVersion, the render header, the clean pabcd
// PASS, exit 1 on FAIL, --json). The recorded answers keep the oracle spelling; before comparing,
// an expectation goes through the names decision (decision 1,
// contract/schema/cxc/name-substitution.json), the same renames the corpus replayer applies:
// R32 (codexclaw -> crw), the cli table doctor row (cxc doctor -> crw doctor harness) and the
// R29 rewrite (cxc-ops -> crw-ops).
//
// Two recorded checks are not compared byte for byte where the oracle throws an engine error and
// the port reports Go's words: drift:version, hook-trust and install-root carry V8's message for a
// missing or unparseable manifest (harnessRunEvidenceDiverges), the difference CRW-615 and CRW-616
// recorded in docs/port-cxc/known-defects.md.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

type harnessRunCheckRecorded struct {
	Name     string
	Severity string
	Evidence string
	Repair   string
}

type harnessRunCaseRecorded struct {
	Name          string
	Checks        []harnessRunCheckRecorded
	Overall       string
	PluginVersion *string
	CodexVersion  *string
	ActiveSurface *string
	Text          string
}

type harnessRunOracleRecorded struct {
	Cases []harnessRunCaseRecorded
}

func harnessRunOracle(t *testing.T) harnessRunOracleRecorded {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "harness", "run", "oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var oracle harnessRunOracleRecorded
	if err := json.Unmarshal(raw, &oracle); err != nil {
		t.Fatal(err)
	}
	return oracle
}

// harnessRunRenamed is the oracle text with the names decision applied.
func harnessRunRenamed(text string) string {
	return strings.NewReplacer(
		"codexclaw", "crw",
		"cxc doctor", "crw doctor harness",
		"cxc enable", "crw install features enable",
		"cxc reset", "crw pabcd reset",
		"cxc-ops", "crw-ops",
	).Replace(text)
}

// harnessRunEvidenceDiverges names the recorded cases and checks whose evidence the port cannot
// reproduce because the oracle printed an engine's error text and the port prints Go's words; for
// those the comparison is severity plus a non-empty evidence.
func harnessRunEvidenceDiverges(caseName, checkName string) bool {
	if caseName != "manifest_missing" && caseName != "manifest_unparseable" {
		return false
	}
	if caseName == "manifest_unparseable" && checkName == "manifest" {
		// V8's SyntaxError text where the port reports encoding/json's.
		return true
	}
	switch checkName {
	case "drift:version", "hook-trust", "install-root":
		return true
	}
	return false
}

// harnessRunPayloadName is the recorder's payload directory name for a case.
func harnessRunPayloadName(caseName string) string {
	return map[string]string{
		"healthy_assembly":            "healthy",
		"manifest_missing":            "missing",
		"manifest_unparseable":        "unparseable",
		"manifest_hooks_not_an_array": "hooks-not-array",
		"skills_absent":               "skills-absent",
		"skills_broken":               "skills-broken",
		"agents_absent":               "agents-absent",
		"no_plugin_version":           "no-version",
		"hard_flag_off_fails":         "hard-off",
		"surface_env":                 "surface",
		"surface_app_port":            "app-port",
		"surface_empty_port_is_unset": "empty-port",
		"codex_version_fallback":      "version-fallback",
	}[caseName]
}

// harnessRunHelperSkill is the ported name of the ast-grep skill whose helper the probe runs.
const harnessRunHelperSkill = "crw-ast-grep"

// harnessRunPayloadOptions are the knobs of the recorded payload builder.
type harnessRunPayloadOptions struct {
	manifest string // the plugin.json text; "missing" writes none
	noSkills bool
	skills   []string
	broken   []string
	noAgents bool
	roles    []string
}

// harnessRunPayload rebuilds the tree the recorder built for one case.
func harnessRunPayload(t *testing.T, tmp, name string, opts harnessRunPayloadOptions) string {
	t.Helper()
	root := filepath.Join(tmp, "payload-"+name)
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if opts.manifest != "missing" {
		write(filepath.Join(".codex-plugin", "plugin.json"), opts.manifest)
	}
	write(".mcp.json", "{\"mcpServers\":{\"test\":{\"command\":\"node\"}}}")
	write(filepath.Join("hooks", "a.json"), "{\"hooks\":{\"Stop\":[{\"hooks\":[{\"type\":\"command\",\"command\":\"node stub.js\"}]}]}}")
	if !opts.noSkills {
		names := opts.skills
		if names == nil {
			names = []string{"dev", harnessRunHelperSkill}
		}
		for _, skill := range names {
			write(filepath.Join("skills", skill, "SKILL.md"), "---\nname: x\n---\n")
			if !slicesContains(opts.broken, skill) {
				write(filepath.Join("skills", skill, "agents", "openai.yaml"), "policy: {}\n")
			}
		}
		if !slicesContains(names, harnessRunHelperSkill) {
			write(filepath.Join("skills", harnessRunHelperSkill, "SKILL.md"), "---\nname: "+harnessRunHelperSkill+"\n---\n")
			write(filepath.Join("skills", harnessRunHelperSkill, "agents", "openai.yaml"), "policy: {}\n")
		}
		write(filepath.Join("skills", harnessRunHelperSkill, "scripts", "ast_grep_helper.py"), "# stub\n")
	}
	if !opts.noAgents {
		roles := opts.roles
		if roles == nil {
			roles = []string{"explorer"}
		}
		for _, role := range roles {
			write(filepath.Join("agents", role+".toml"), "name=\""+role+"\"\n")
		}
	}
	return root
}

func slicesContains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// harnessRunListing is codex features list over the declared flags.
func harnessRunListing(states map[string]bool) string {
	lines := make([]string, 0, 4)
	for _, key := range []string{"multi_agent", "goals", "hooks", "default_mode_request_user_input"} {
		lines = append(lines, fmt.Sprintf("%s  stable  %t", key, states[key]))
	}
	return strings.Join(lines, "\n")
}

// harnessRunStub is the recorded stub runner: codex features list, codex --version and the
// ast-grep helper probe.
func harnessRunStub(states map[string]bool, version string) HarnessRunner {
	return func(file string, args []string, _ time.Duration) HarnessRun {
		switch {
		case file == "codex" && len(args) > 0 && args[0] == "features":
			return HarnessRun{Status: harnessRunInt(0), Stdout: harnessRunListing(states)}
		case file == "codex" && len(args) > 0 && args[0] == "--version":
			return HarnessRun{Status: harnessRunInt(0), Stdout: version}
		case file == "python3":
			return HarnessRun{Status: harnessRunInt(0), Stdout: "ast-grep binary: /stub/sg\n  version: ast-grep 0.44.0\n"}
		}
		return HarnessRun{Status: harnessRunInt(1)}
	}
}

// harnessRunCase is the payload, options and env the recorder used for one case.
func harnessRunCase(name string) (harnessRunPayloadOptions, HarnessOptions, map[string]string, map[string]bool, string) {
	allOn := map[string]bool{"multi_agent": true, "goals": true, "hooks": true, "default_mode_request_user_input": true}
	opts := harnessRunPayloadOptions{manifest: "{\"name\":\"crw\",\"version\":\"0.0.1\",\"hooks\":[\"./hooks/a.json\"],\"mcpServers\":\"./.mcp.json\"}"}
	options := HarnessOptions{SessionID: harnessRunString("rec-s1")}
	env := map[string]string{}
	states := allOn
	version := "codex-cli 1.2.3\n"
	switch name {
	case "manifest_missing":
		opts.manifest = "missing"
	case "manifest_unparseable":
		opts.manifest = "{not json"
	case "manifest_hooks_not_an_array":
		opts.manifest = "{\"name\":\"crw\",\"version\":\"0.0.1\",\"hooks\":\"nope\"}"
	case "skills_absent":
		opts.noSkills = true
	case "skills_broken":
		opts.skills = []string{"dev", "broken"}
		opts.broken = []string{"broken"}
	case "agents_absent":
		opts.noAgents = true
	case "no_plugin_version":
		opts.manifest = "{\"name\":\"crw\",\"hooks\":[\"./hooks/a.json\"]}"
	case "hard_flag_off_fails":
		states = map[string]bool{"multi_agent": true, "goals": false, "hooks": true, "default_mode_request_user_input": true}
	case "surface_env":
		env["CODEX_SURFACE"] = "cli"
	case "surface_app_port":
		env["CODEX_APP_PORT"] = "4500"
	case "surface_empty_port_is_unset":
		env["CODEX_APP_PORT"] = ""
	case "codex_version_fallback":
		version = "  nightly  \n"
	}
	return opts, options, env, states, version
}

func harnessRunEnv(values map[string]string) host.LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestHarnessRunDoctorRecorded(t *testing.T) {
	for _, recorded := range harnessRunOracle(t).Cases {
		t.Run(recorded.Name, func(t *testing.T) {
			tmp := t.TempDir()
			opts, options, env, states, version := harnessRunCase(recorded.Name)
			root := harnessRunPayload(t, tmp, harnessRunPayloadName(recorded.Name), opts)
			options.CodexHome = filepath.Join(tmp, "codex")
			if err := os.MkdirAll(options.CodexHome, 0o755); err != nil {
				t.Fatal(err)
			}
			projectRoot := filepath.Join(tmp, "cwd")
			if err := os.MkdirAll(projectRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			report := RunHarnessDoctor(root, harnessRunStub(states, version), options, projectRoot, harnessRunEnv(env), time.Now())
			if report.SchemaVersion != HarnessSchemaVersion {
				t.Errorf("schemaVersion = %d, want %d", report.SchemaVersion, HarnessSchemaVersion)
			}
			if got := string(report.Overall); got != recorded.Overall {
				t.Errorf("overall = %q, want %q", got, recorded.Overall)
			}
			if len(report.Checks) != len(recorded.Checks) {
				t.Fatalf("checks = %d, want %d: %+v", len(report.Checks), len(recorded.Checks), report.Checks)
			}
			for i, want := range recorded.Checks {
				got := report.Checks[i]
				if got.Name != want.Name || string(got.Severity) != want.Severity {
					t.Errorf("check %d = %s/%s, want %s/%s", i, got.Name, got.Severity, want.Name, want.Severity)
					continue
				}
				if harnessRunEvidenceDiverges(recorded.Name, want.Name) {
					if got.Evidence == "" {
						t.Errorf("check %s has empty evidence", got.Name)
					}
					continue
				}
				wantEvidence := harnessRunRenamed(strings.ReplaceAll(want.Evidence, harnessRunTempToken, tmp))
				if got.Evidence != wantEvidence {
					t.Errorf("check %s evidence:\n got %q\nwant %q", got.Name, got.Evidence, wantEvidence)
				}
				wantRepair := harnessRunRenamed(strings.ReplaceAll(want.Repair, harnessRunTempToken, tmp))
				if got.Repair != wantRepair {
					t.Errorf("check %s repair:\n got %q\nwant %q", got.Name, got.Repair, wantRepair)
				}
			}
			harnessRunCompareOptional(t, "pluginVersion", report.PluginVersion, recorded.PluginVersion)
			harnessRunCompareOptional(t, "codexVersion", report.CodexVersion, recorded.CodexVersion)
			harnessRunCompareOptional(t, "activeSurface", report.ActiveSurface, recorded.ActiveSurface)
		})
	}
}

// harnessRunTempToken is the recorder's placeholder for its temporary root.
const harnessRunTempToken = "$" + "{TEMP}"

func harnessRunCompareOptional(t *testing.T, name string, got, want *string) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("%s = %q, want absent", name, *got)
		}
		return
	}
	if got == nil {
		t.Errorf("%s absent, want %q", name, *want)
		return
	}
	if *got != *want {
		t.Errorf("%s = %q, want %q", name, *got, *want)
	}
}

func TestHarnessRunDoctorTextRender(t *testing.T) {
	tmp := t.TempDir()
	opts, options, env, states, version := harnessRunCase("healthy_assembly")
	root := harnessRunPayload(t, tmp, harnessRunPayloadName("healthy_assembly"), opts)
	options.CodexHome = filepath.Join(tmp, "codex")
	if err := os.MkdirAll(options.CodexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	report := RunHarnessDoctor(root, harnessRunStub(states, version), options, tmp, harnessRunEnv(env), time.Now())
	var want string
	for _, recorded := range harnessRunOracle(t).Cases {
		if recorded.Name == "healthy_assembly" {
			want = harnessRunRenamed(strings.ReplaceAll(recorded.Text, harnessRunTempToken, tmp))
		}
	}
	if got := RenderHarnessReport(report); got != want {
		t.Errorf("text render:\n got %q\nwant %q", got, want)
	}
}

// TestHarnessRunDoctorCLI ports the CLI B tests: text unless --json, exit 1 on FAIL, --key and
// --codex-home accepted, and the oracle's option-parser catch.
func TestHarnessRunDoctorCLI(t *testing.T) {
	run := func(t *testing.T, args []string, values map[string]string, opts harnessRunPayloadOptions, states map[string]bool) (int, string, string) {
		t.Helper()
		tmp := t.TempDir()
		root := harnessRunPayload(t, tmp, "cli", opts)
		values["PLUGIN_ROOT"] = root
		values["CODEX_HOME"] = filepath.Join(tmp, "codex")
		if err := os.MkdirAll(values["CODEX_HOME"], 0o755); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code := RunHarnessDoctorCLI(args, &stdout, &stderr, harnessRunEnv(values), func() (string, error) { return tmp, nil }, harnessRunStub(states, "codex-cli 1.2.3\n"), time.Now())
		return code, stdout.String(), stderr.String()
	}
	allOn := map[string]bool{"multi_agent": true, "goals": true, "hooks": true, "default_mode_request_user_input": true}
	healthy := harnessRunPayloadOptions{manifest: "{\"name\":\"crw\",\"version\":\"0.0.1\",\"hooks\":[\"./hooks/a.json\"],\"mcpServers\":\"./.mcp.json\"}"}

	t.Run("json is schemaVersion 1", func(t *testing.T) {
		code, out, errOut := run(t, []string{"--json"}, map[string]string{}, healthy, allOn)
		if code != 0 {
			t.Fatalf("exit = %d, want 0 (stderr %q)", code, errOut)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, out)
		}
		if parsed["schemaVersion"] != float64(1) {
			t.Errorf("schemaVersion = %v, want 1", parsed["schemaVersion"])
		}
		if _, ok := parsed["checks"].([]any); !ok {
			t.Errorf("checks is not an array: %v", parsed["checks"])
		}
	})

	t.Run("text is the rendered report", func(t *testing.T) {
		code, out, _ := run(t, nil, map[string]string{}, healthy, allOn)
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if !strings.Contains(out, "crw v0.0.1\n") || !strings.Contains(out, "overall: WARN\n") {
			t.Errorf("unexpected text output:\n%s", out)
		}
	})

	t.Run("a FAIL exits 1", func(t *testing.T) {
		states := map[string]bool{"multi_agent": true, "goals": false, "hooks": true, "default_mode_request_user_input": true}
		code, out, _ := run(t, []string{"--json"}, map[string]string{}, healthy, states)
		if code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if !strings.Contains(out, "\"overall\": \"FAIL\"") {
			t.Errorf("overall is not FAIL:\n%s", out)
		}
	})

	t.Run("--key and --codex-home are accepted", func(t *testing.T) {
		_, out, errOut := run(t, []string{"--json", "--key", "crw@local", "--codex-home", "/tmp/x"}, map[string]string{}, healthy, allOn)
		if strings.Contains(errOut, "unknown hooks option") {
			t.Fatalf("the parser rejected a supported option: %q", errOut)
		}
		if !strings.Contains(out, "/tmp/x/plugins/cache") {
			t.Errorf("--codex-home did not reach the checks:\n%s", out)
		}
	})

	t.Run("--help is the parser catch", func(t *testing.T) {
		code, out, errOut := run(t, []string{"--help"}, map[string]string{}, healthy, allOn)
		if code != 1 || out != "" || errOut != "crw-ops error: unknown hooks option: --help\n" {
			t.Fatalf("exit = %d stdout=%q stderr=%q", code, out, errOut)
		}
	})

	t.Run("an unset plugin root is a usage error", func(t *testing.T) {
		tmp := t.TempDir()
		var stdout, stderr bytes.Buffer
		code := RunHarnessDoctorCLI(nil, &stdout, &stderr, harnessRunEnv(map[string]string{}), func() (string, error) { return tmp, nil }, harnessRunStub(allOn, ""), time.Now())
		if code != usageExit {
			t.Fatalf("exit = %d, want %d", code, usageExit)
		}
	})
}

// TestHarnessRunExecTimeoutIsDriftKilled pins predecessor note 2: the real runner answers a probe
// it killed at the timeout with harnessDriftKilled, never a nil status (nil reads as "python3 not
// found").
func TestHarnessRunExecTimeoutIsDriftKilled(t *testing.T) {
	script := filepath.Join(t.TempDir(), "slow.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := harnessRunExec(script, nil, 50*time.Millisecond)
	if run.Status == nil {
		t.Fatal("a timeout kill answered a nil status; the ast-grep check would read it as python3 not found")
	}
	if *run.Status != harnessDriftKilled {
		t.Fatalf("status = %d, want %d", *run.Status, harnessDriftKilled)
	}
	// A binary that does not exist is the other shape: a nil status (the oracle's spawn error).
	if missing := harnessRunExec(filepath.Join(t.TempDir(), "nope"), nil, time.Second); missing.Status != nil {
		t.Fatalf("a missing binary answered status %d, want nil", *missing.Status)
	}
}

// TestHarnessRunDoctorRecoversCheckPanics is predecessor note 1: the CLI boundary turns the panic
// HarnessPabcdCheck throws where the oracle throws outside every catch into the same failure
// output and exit code cli.ts's catch gives.
func TestHarnessRunDoctorRecoversCheckPanics(t *testing.T) {
	tmp := t.TempDir()
	sessions := filepath.Join(tmp, ".crw", "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "s.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sessions, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sessions, 0o755) })
	if _, err := os.ReadDir(sessions); err == nil {
		t.Skip("the sessions directory stays readable despite mode 000 (privileged user)")
	}
	var stdout, stderr bytes.Buffer
	env := harnessRunEnv(map[string]string{"PLUGIN_ROOT": tmp})
	code := RunHarnessDoctorCLI([]string{"--json"}, &stdout, &stderr, env, func() (string, error) { return tmp, nil }, harnessRunStub(nil, ""), time.Now())
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if !strings.HasPrefix(stderr.String(), "crw-ops error: ") || !strings.HasSuffix(stderr.String(), "\n") {
		t.Errorf("stderr = %q, want the cli.ts catch text", stderr.String())
	}
}

// harnessRunFailWriter is a stream that cannot be written to.
type harnessRunFailWriter struct{}

func (harnessRunFailWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

// TestHarnessRunDoctorCLIFailsWhenTextCannotBeWritten: a report the caller never received is not a
// success, so the text path reports the write failure the JSON path already did.
func TestHarnessRunDoctorCLIFailsWhenTextCannotBeWritten(t *testing.T) {
	tmp := t.TempDir()
	root := harnessRunPayload(t, tmp, "cli-write", harnessRunPayloadOptions{manifest: "{\"name\":\"crw\",\"version\":\"0.0.1\",\"hooks\":[\"./hooks/a.json\"]}"})
	codexHome := filepath.Join(tmp, "codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	allOn := map[string]bool{"multi_agent": true, "goals": true, "hooks": true, "default_mode_request_user_input": true}
	env := harnessRunEnv(map[string]string{"PLUGIN_ROOT": root, "CODEX_HOME": codexHome})
	code := RunHarnessDoctorCLI(nil, harnessRunFailWriter{}, io.Discard, env, func() (string, error) { return tmp, nil }, harnessRunStub(allOn, "codex-cli 1.2.3\n"), time.Now())
	if code != 1 {
		t.Fatalf("exit = %d, want 1 when the text report cannot be written", code)
	}
}

// TestHarnessRunWriteJSONKeepsSeparators: JSON.stringify writes U+2028 and U+2029 literally, so
// the report bytes must keep them; an escaped backslash sequence is data and stays escaped.
func TestHarnessRunWriteJSONKeepsSeparators(t *testing.T) {
	report := HarnessReport{
		SchemaVersion: HarnessSchemaVersion,
		Overall:       HarnessPass,
		Checks:        []HarnessCheck{{Name: "manifest", Severity: HarnessPass, Evidence: "ok"}},
		PluginVersion: harnessRunString("a\u2028b"),
		ActiveSurface: harnessRunString("c\u2029d" + "\\u2028"),
	}
	var out bytes.Buffer
	if err := harnessRunWriteJSON(&out, report); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "\"a\u2028b\"") || !strings.Contains(got, "\"c\u2029d\\\\u2028\"") {
		t.Fatalf("the separators did not survive as JSON.stringify writes them:\n%s", got)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["pluginVersion"] != "a\u2028b" || parsed["activeSurface"] != "c\u2029d"+"\\u2028" {
		t.Fatalf("round trip = %v / %v", parsed["pluginVersion"], parsed["activeSurface"])
	}
}

// TestHarnessRunExecBoundsAnInheritedPipe: a probe whose descendant holds the output pipe must not
// hold the report open past the wait delay; it answers the killed status, never a hang.
func TestHarnessRunExecBoundsAnInheritedPipe(t *testing.T) {
	saved := commandWaitDelay
	commandWaitDelay = 200 * time.Millisecond
	t.Cleanup(func() { commandWaitDelay = saved })
	script := filepath.Join(t.TempDir(), "leaky.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 5 &\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	run := harnessRunExec(script, nil, 30*time.Second)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the run waited %s for an inherited pipe", elapsed)
	}
	if run.Status == nil || *run.Status != harnessDriftKilled {
		t.Fatalf("status = %v, want the killed status %d", run.Status, harnessDriftKilled)
	}
}
