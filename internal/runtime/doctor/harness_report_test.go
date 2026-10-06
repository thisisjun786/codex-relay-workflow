package doctor

// This file replays testdata/harness/report/oracle.json over the harness report core and ports
// the B tests of CXC v0.2.40 cxc-ops/test/cxc-ops.test.ts and doctor-features.test.ts that call
// only its functions. The recorded answers keep the oracle spelling; before comparing, an
// expectation goes through the names decision (decision 1,
// contract/schema/cxc/name-substitution.json), the same renames the corpus replayer applies to
// a fixture: R32 (`codexclaw` -> `crw`), the cli table doctor row (`cxc doctor` -> `crw doctor
// harness`) and its enable row (`cxc enable` -> `crw install features enable`).

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/configguard"
)

// harnessReportText is a recorded string leaf that may carry a lone high surrogate. The oracle's
// JSON string holds it as the \udXXX escape (the stderr cut case), and pyjson.Loads reads that
// escape back as the three WTF-8 bytes the port produces; encoding/json alone would read it as
// U+FFFD, so the recorded escape would not match the port's output.
type harnessReportRecordedText string

func (t *harnessReportRecordedText) UnmarshalJSON(raw []byte) error {
	value, err := pyjson.Loads(string(raw), pyjson.LoadOptions{Surrogates: true, Deep: true})
	if err != nil {
		return err
	}
	s, ok := value.(string)
	if !ok {
		return fmt.Errorf("recorded string is %T, not a string", value)
	}
	*t = harnessReportRecordedText(s)
	return nil
}

// harnessReportCheckRecorded is one CheckResult in a recorded case (doctor.ts:25-32).
type harnessReportCheckRecorded struct {
	Name     string `json:"name"`
	Severity string `json:"severity"`
	Evidence string `json:"evidence"`
	Repair   string `json:"repair"`
}

// harnessReportRecorded is a DoctorReport in a recorded case (doctor.ts:34-45).
type harnessReportRecorded struct {
	SchemaVersion int                          `json:"schemaVersion"`
	Overall       string                       `json:"overall"`
	Checks        []harnessReportCheckRecorded `json:"checks"`
	PluginVersion string                       `json:"pluginVersion"`
	CodexVersion  string                       `json:"codexVersion"`
	ActiveSurface string                       `json:"activeSurface"`
}

type harnessReportRollupRecorded struct {
	Name    string                       `json:"name"`
	Checks  []harnessReportCheckRecorded `json:"checks"`
	Overall string                       `json:"overall"`
}

type harnessReportRenderRecorded struct {
	Name   string                `json:"name"`
	Report harnessReportRecorded `json:"report"`
	Text   string                `json:"text"`
}

type harnessReportParsedRecorded struct {
	Name   string          `json:"name"`
	Stdout string          `json:"stdout"`
	Parsed map[string]bool `json:"parsed"`
}

// harnessReportProbeRecorded is buildDeclaredFeaturesCheck's argument (doctor.ts:132): a status
// that is null when the spawn threw.
type harnessReportProbeRecorded struct {
	Status *int   `json:"status"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

type harnessReportFeaturesRecorded struct {
	Name     string                     `json:"name"`
	Run      harnessReportProbeRecorded `json:"run"`
	Severity string                     `json:"severity"`
	Evidence harnessReportRecordedText  `json:"evidence"`
	Repair   string                     `json:"repair"`
}

type harnessReportVersionRecorded struct {
	Name    string  `json:"name"`
	Status  *int    `json:"status"`
	Stdout  string  `json:"stdout"`
	Stderr  string  `json:"stderr"`
	Throws  bool    `json:"throws"`
	Version *string `json:"version"`
}

type harnessReportWslRecorded struct {
	Severity string `json:"severity"`
	Evidence string `json:"evidence"`
}

// harnessReportOracle is testdata/harness/report/oracle.json, written by
// testdata/harness/report/record-report.mjs.
type harnessReportOracle struct {
	Oracle         string                          `json:"oracle"`
	Dist           string                          `json:"dist"`
	Rollup         []harnessReportRollupRecorded   `json:"rollup"`
	Render         []harnessReportRenderRecorded   `json:"render"`
	FeaturesParsed []harnessReportParsedRecorded   `json:"featuresParsed"`
	FeaturesCheck  []harnessReportFeaturesRecorded `json:"featuresCheck"`
	CodexVersion   []harnessReportVersionRecorded  `json:"codexVersion"`
	WSL            harnessReportWslRecorded        `json:"wsl"`
}

func harnessReportOracleRecorded(t *testing.T) harnessReportOracle {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "harness", "report", "oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recorded harnessReportOracle
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorded.Oracle, "3c1459ac") {
		t.Fatalf("oracle.json names %q, want the CXC v0.2.40 commit", recorded.Oracle)
	}
	if len(recorded.Rollup) == 0 || len(recorded.Render) == 0 || len(recorded.FeaturesParsed) == 0 || len(recorded.FeaturesCheck) == 0 || len(recorded.CodexVersion) == 0 {
		t.Fatalf("oracle.json holds an empty group: %d rollup, %d render, %d parsed, %d features, %d version",
			len(recorded.Rollup), len(recorded.Render), len(recorded.FeaturesParsed), len(recorded.FeaturesCheck), len(recorded.CodexVersion))
	}
	return recorded
}

// harnessReportRenamed is the oracle text with the names decision applied, the renames the
// corpus replayer applies to an expectation (contract/schema/cxc/name-substitution.json).
func harnessReportRenamed(text string) string {
	return strings.NewReplacer(
		"codexclaw", "crw",
		"cxc doctor", "crw doctor harness",
		"cxc enable", "crw install features enable",
	).Replace(text)
}

// harnessReportPresent is a present optional string; the render cases record only the fields the
// oracle report held, so an empty recorded value is an absent one.
func harnessReportPresent(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// harnessReportRepairString is the string a check's repair pointer holds, and "" when it is nil:
// the recorded repair is the oracle's optional string, whose absent and empty cases both read "".
func harnessReportRepairString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// harnessReportChecks converts recorded checks into the port type, renaming the text the names
// decision covers; the recorded case is the oracle spelling, the port emits the renamed one.
func harnessReportChecks(recorded []harnessReportCheckRecorded) []HarnessCheck {
	checks := make([]HarnessCheck, 0, len(recorded))
	for _, c := range recorded {
		checks = append(checks, HarnessCheck{
			Name:     harnessReportRenamed(c.Name),
			Severity: HarnessSeverity(c.Severity),
			Evidence: harnessReportRenamed(c.Evidence),
			Repair:   harnessReportRepair(harnessReportRenamed(c.Repair)),
		})
	}
	return checks
}

// harnessReportStatus is a non-nil exit status.
func harnessReportStatus(code int) *int { return &code }

// harnessReportListing is codex features list over the declared flags (the recorder's helper,
// doctor-features.test.ts:19-21).
func harnessReportListing(states map[string]bool) string {
	lines := make([]string, 0, len(configguard.DeclaredFeatures()))
	for _, key := range configguard.DeclaredFeatures() {
		lines = append(lines, fmt.Sprintf("%s  stable  %t", key, states[string(key)]))
	}
	return strings.Join(lines, "\n")
}

func TestHarnessReportRollupRecorded(t *testing.T) {
	for _, recorded := range harnessReportOracleRecorded(t).Rollup {
		t.Run(recorded.Name, func(t *testing.T) {
			if got := HarnessRollup(harnessReportChecks(recorded.Checks)); got != HarnessSeverity(recorded.Overall) {
				t.Fatalf("HarnessRollup = %s, want %s", got, recorded.Overall)
			}
		})
	}
}

func TestHarnessReportRenderRecorded(t *testing.T) {
	for _, recorded := range harnessReportOracleRecorded(t).Render {
		t.Run(recorded.Name, func(t *testing.T) {
			report := HarnessReport{
				SchemaVersion: recorded.Report.SchemaVersion,
				Overall:       HarnessSeverity(recorded.Report.Overall),
				Checks:        harnessReportChecks(recorded.Report.Checks),
				PluginVersion: harnessReportPresent(recorded.Report.PluginVersion),
				CodexVersion:  harnessReportPresent(recorded.Report.CodexVersion),
				ActiveSurface: harnessReportPresent(recorded.Report.ActiveSurface),
			}
			want := harnessReportRenamed(recorded.Text)
			if got := RenderHarnessReport(report); got != want {
				t.Fatalf("RenderHarnessReport =\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestHarnessReportParseFeaturesRecorded(t *testing.T) {
	for _, recorded := range harnessReportOracleRecorded(t).FeaturesParsed {
		t.Run(recorded.Name, func(t *testing.T) {
			got := ParseHarnessFeatures(recorded.Stdout)
			if !maps.Equal(got, recorded.Parsed) {
				t.Fatalf("ParseHarnessFeatures(%q) = %v, want %v", recorded.Stdout, got, recorded.Parsed)
			}
		})
	}
}

func TestHarnessReportFeaturesCheckRecorded(t *testing.T) {
	for _, recorded := range harnessReportOracleRecorded(t).FeaturesCheck {
		t.Run(recorded.Name, func(t *testing.T) {
			run := HarnessRun{Status: recorded.Run.Status, Stdout: recorded.Run.Stdout, Stderr: recorded.Run.Stderr}
			want := HarnessCheck{
				Name:     "features",
				Severity: HarnessSeverity(recorded.Severity),
				Evidence: harnessReportRenamed(string(recorded.Evidence)),
				Repair:   harnessReportRepair(harnessReportRenamed(recorded.Repair)),
			}
			got := HarnessFeaturesCheck(run)
			if got.Name != want.Name || got.Severity != want.Severity || got.Evidence != want.Evidence || harnessReportRepairString(got.Repair) != harnessReportRepairString(want.Repair) {
				t.Fatalf("HarnessFeaturesCheck = %+v, want %+v", got, want)
			}
		})
	}
}

func TestHarnessReportCodexVersionRecorded(t *testing.T) {
	for _, recorded := range harnessReportOracleRecorded(t).CodexVersion {
		t.Run(recorded.Name, func(t *testing.T) {
			var calls []string
			run := func(file string, args []string, timeout time.Duration) HarnessRun {
				calls = append(calls, fmt.Sprintf("%s %s %s", file, strings.Join(args, " "), timeout))
				if recorded.Throws {
					return HarnessRun{Stderr: "codex could not be spawned"}
				}
				return HarnessRun{Status: recorded.Status, Stdout: recorded.Stdout, Stderr: recorded.Stderr}
			}
			got := harnessReportDetectCodexVersion(run)
			if len(calls) != 1 || calls[0] != "codex --version 5s" {
				t.Fatalf("runner calls = %v, want one codex --version call with the oracle 5s timeout", calls)
			}
			switch {
			case recorded.Version == nil && got != nil:
				t.Fatalf("version = %q, want undefined", *got)
			case recorded.Version != nil && got == nil:
				t.Fatalf("version = undefined, want %q", *recorded.Version)
			case recorded.Version != nil && *got != *recorded.Version:
				t.Fatalf("version = %q, want %q", *got, *recorded.Version)
			}
		})
	}
}

func TestHarnessReportWslRecorded(t *testing.T) {
	recorded := harnessReportOracleRecorded(t).WSL
	check := HarnessWslCheck()
	if string(check.Severity) != recorded.Severity || check.Evidence != recorded.Evidence {
		t.Fatalf("HarnessWslCheck = %+v, want %s %q", check, recorded.Severity, recorded.Evidence)
	}
}

// ---- the B tests of cxc-ops.test.ts and doctor-features.test.ts that call only these ----

// The --json contract keeps a present-but-empty optional string and omits an undefined one
// (doctor.ts:34-45 read by cli.ts:84-86); the recorded whitespace-only `codex --version` case
// answers the empty string.
func TestHarnessReportOptionalFieldsJSONPort(t *testing.T) {
	empty := ""
	raw, err := json.Marshal(HarnessReport{SchemaVersion: 1, Overall: HarnessPass, Checks: []HarnessCheck{}, CodexVersion: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\"codexVersion\":\"\"") {
		t.Fatalf("marshalled report omits a present-but-empty codexVersion: %s", raw)
	}
	raw, err = json.Marshal(HarnessReport{SchemaVersion: 1, Overall: HarnessPass, Checks: []HarnessCheck{}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "codexVersion") {
		t.Fatalf("marshalled report keeps an undefined codexVersion: %s", raw)
	}
}

func TestHarnessReportRollupPort(t *testing.T) {
	pass := HarnessCheck{Name: "a", Severity: HarnessPass}
	warn := HarnessCheck{Name: "b", Severity: HarnessWarn}
	fail := HarnessCheck{Name: "c", Severity: HarnessFail}
	for _, test := range []struct {
		name   string
		checks []HarnessCheck
		want   HarnessSeverity
	}{
		{"pass only", []HarnessCheck{pass}, HarnessPass},
		{"warn beats pass", []HarnessCheck{pass, warn}, HarnessWarn},
		{"fail beats warn", []HarnessCheck{warn, fail}, HarnessFail},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := HarnessRollup(test.checks); got != test.want {
				t.Fatalf("HarnessRollup = %s, want %s", got, test.want)
			}
		})
	}
}

func TestHarnessReportCodexVersionPort(t *testing.T) {
	for _, test := range []struct {
		name   string
		stdout string
		want   string
	}{
		{"semver in stdout", "codex-cli 1.2.3\n", "1.2.3"},
		{"trimmed stdout without a semver", "  nightly  \n", "nightly"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := harnessReportDetectCodexVersion(func(file string, args []string, timeout time.Duration) HarnessRun {
				return HarnessRun{Status: harnessReportStatus(0), Stdout: test.stdout}
			})
			if got == nil || *got != test.want {
				t.Fatalf("detected version = %v, want %q", got, test.want)
			}
		})
	}
}

func TestHarnessReportWslOffPort(t *testing.T) {
	check := HarnessWslCheck()
	if check.Name != "wsl" || check.Severity != HarnessPass || !strings.Contains(check.Evidence, "not running under WSL") {
		t.Fatalf("HarnessWslCheck = %+v, want the off-WSL PASS", check)
	}
}

func TestHarnessReportIsDirPort(t *testing.T) {
	dir := t.TempDir()
	if !harnessReportIsDir(dir) {
		t.Fatalf("harnessReportIsDir(%q) = false, want true", dir)
	}
	if harnessReportIsDir(filepath.Join(dir, "missing")) {
		t.Fatal("harnessReportIsDir(missing) = true, want false")
	}
}

func TestHarnessReportFeaturesPort(t *testing.T) {
	all := harnessReportAllOn()

	// All flags on: a PASS carrying the count as evidence.
	check := harnessReportFeatureCheck(t, all)
	if check.Severity != HarnessPass || !strings.Contains(check.Evidence, "4/4") {
		t.Fatalf("all on = %+v, want a 4/4 PASS", check)
	}

	// A missing SOFT flag WARNs and names what is lost, not just what is off.
	check = harnessReportFeatureCheck(t, harnessReportOff(all, "default_mode_request_user_input"))
	if check.Severity != HarnessWarn {
		t.Fatalf("soft off = %+v, want WARN: reduced capability is not breakage", check)
	}
	if !strings.Contains(check.Evidence, "request_user_input") || !strings.Contains(check.Evidence, "Default mode") {
		t.Fatalf("soft off evidence = %q, want what is lost", check.Evidence)
	}
	if harnessReportRepairString(check.Repair) != "codex features enable default_mode_request_user_input" {
		t.Fatalf("soft off repair = %q", harnessReportRepairString(check.Repair))
	}

	// A missing HARD flag FAILs and points at the install verb that turns flags on.
	check = harnessReportFeatureCheck(t, harnessReportOff(all, "goals"))
	if check.Severity != HarnessFail || !strings.Contains(check.Evidence, "goals") {
		t.Fatalf("hard off = %+v, want a FAIL naming goals", check)
	}
	if harnessReportRepairString(check.Repair) != "crw install features enable" {
		t.Fatalf("hard off repair = %q, want the crw install verb", harnessReportRepairString(check.Repair))
	}

	// A hard flag off outranks a soft flag off.
	check = harnessReportFeatureCheck(t, harnessReportOff(all, "hooks", "default_mode_request_user_input"))
	if check.Severity != HarnessFail || !strings.Contains(check.Evidence, "hooks") {
		t.Fatalf("hard and soft off = %+v, want a FAIL naming hooks", check)
	}

	// An unreachable codex WARNs rather than FAILs: a diagnostic must not invent a verdict.
	check = HarnessFeaturesCheck(HarnessRun{Status: harnessReportStatus(127), Stderr: "command not found"})
	if check.Severity != HarnessWarn || !strings.Contains(check.Evidence, "could not read") || !strings.Contains(check.Evidence, "exit 127") {
		t.Fatalf("unreachable = %+v, want a WARN naming exit 127", check)
	}

	// A spawn that threw (status null) still WARNs without printing a bogus exit code.
	check = HarnessFeaturesCheck(HarnessRun{Stderr: "EPERM"})
	if check.Severity != HarnessWarn || strings.Contains(check.Evidence, "exit") {
		t.Fatalf("thrown spawn = %+v, want a WARN without an exit code", check)
	}
}

func TestHarnessReportParseFeaturesPort(t *testing.T) {
	// The parser matches the first field exactly, so sibling keys cannot clobber.
	parsed := ParseHarnessFeatures(strings.Join([]string{
		"multi_agent_v2  experimental  true",
		"multi_agent  stable  false",
		"plugin_hooks  stable  true",
		"hooks  stable  false",
	}, "\n"))
	if parsed["multi_agent"] {
		t.Fatalf("multi_agent = true, want false: multi_agent_v2 must not satisfy it")
	}
	if parsed["hooks"] {
		t.Fatalf("hooks = true, want false: plugin_hooks must not satisfy it")
	}
	if _, ok := parsed["multi_agent_v2"]; ok {
		t.Fatalf("undeclared key multi_agent_v2 recorded: %v", parsed)
	}
	if _, ok := parsed["plugin_hooks"]; ok {
		t.Fatalf("undeclared key plugin_hooks recorded: %v", parsed)
	}

	// An unparseable trailing token reads as not enabled, the safe default.
	parsed = ParseHarnessFeatures("goals  stable  maybe")
	if _, ok := parsed["goals"]; ok {
		t.Fatalf("unparseable trailing token recorded a value: %v", parsed)
	}
	if check := HarnessFeaturesCheck(HarnessRun{Status: harnessReportStatus(0), Stdout: "goals  stable  maybe"}); check.Severity != HarnessFail {
		t.Fatalf("unparseable state = %+v, want FAIL", check)
	}
}

// TestHarnessReportSoftSetMatchesConfigguard is doctor-features.test.ts:85-91: the soft set the
// doctor splits severity on is the config guard soft set. Turning one declared flag off alone
// must WARN exactly for the flags configguard calls soft.
func TestHarnessReportSoftSetMatchesConfigguard(t *testing.T) {
	soft := map[string]bool{}
	for _, key := range configguard.SoftFeatures() {
		soft[string(key)] = true
	}
	all := harnessReportAllOn()
	for _, key := range configguard.DeclaredFeatures() {
		want := HarnessFail
		if soft[string(key)] {
			want = HarnessWarn
		}
		if got := harnessReportFeatureCheck(t, harnessReportOff(all, string(key))); got.Severity != want {
			t.Errorf("%s off: severity = %s, want %s (%+v)", key, got.Severity, want, got)
		}
	}
}

// harnessReportAllOn is every declared flag true.
func harnessReportAllOn() map[string]bool {
	states := map[string]bool{}
	for _, key := range configguard.DeclaredFeatures() {
		states[string(key)] = true
	}
	return states
}

// harnessReportOff copies states and turns the named flags off.
func harnessReportOff(states map[string]bool, keys ...string) map[string]bool {
	clone := maps.Clone(states)
	for _, key := range keys {
		clone[key] = false
	}
	return clone
}

// harnessReportFeatureCheck runs the features check over a listing of the states.
func harnessReportFeatureCheck(t *testing.T, states map[string]bool) HarnessCheck {
	t.Helper()
	return HarnessFeaturesCheck(HarnessRun{Status: harnessReportStatus(0), Stdout: harnessReportListing(states)})
}

// TestHarnessReportCorpusTextPort pins the exact text the recorded doctor fixtures cross-check,
// after the cli table maps `cxc enable` and `cxc doctor` and R32 maps `codexclaw`: the hard-flag
// fixture (cli__doctor__hard_flag_off_and_stale_install_root_fail) expects the FAIL evidence and
// repair below, and the text-report fixture (cli__doctor__text_report) expects the
// unreachable-probe WARN with the re-run hint that names the subcommand. These literals are the
// contract the CLI issue compares, not a restatement of harnessReportRenamed.
func TestHarnessReportCorpusTextPort(t *testing.T) {
	check := harnessReportFeatureCheck(t, harnessReportOff(harnessReportAllOn(), "multi_agent", "goals", "default_mode_request_user_input"))
	if want := "1/4 enabled; crw requires [multi_agent, goals]"; check.Evidence != want {
		t.Fatalf("hard flag evidence = %q, want %q", check.Evidence, want)
	}
	if want := "crw install features enable"; harnessReportRepairString(check.Repair) != want {
		t.Fatalf("hard flag repair = %q, want %q", harnessReportRepairString(check.Repair), want)
	}
	check = HarnessFeaturesCheck(HarnessRun{Status: harnessReportStatus(127), Stderr: "stub codex: not scripted"})
	if want := "could not read 'codex features list' (exit 127): stub codex: not scripted"; check.Evidence != want {
		t.Fatalf("unreachable evidence = %q, want %q", check.Evidence, want)
	}
	if want := "ensure the `codex` binary is on PATH, then re-run `crw doctor harness`"; harnessReportRepairString(check.Repair) != want {
		t.Fatalf("unreachable repair = %q, want %q", harnessReportRepairString(check.Repair), want)
	}
}
