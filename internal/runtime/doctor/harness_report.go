// Harness report core, ported from CXC v0.2.40 cxc-ops/src/doctor.ts (commit
// 3c1459acadeb1906d97c00a598e1457327ae372d): the report types (:23-63), isDir (:64-70), the
// severity rollup (:73-78), the Codex version probe (:89-98), the declared-feature reader and
// its check (:114-165), the not-under-WSL branch of the residency check (:193-196) and the text
// renderer (:648-661). The command that assembles a report from the checks and prints it --
// `crw doctor harness`, text or --json, exit 1 on FAIL -- belongs to its own port issue; this
// file is the report core: it adds no command, reads no configuration and writes nothing.
//
// Two name rules apply (decision 1, contract/schema/cxc/name-substitution.json): R32 makes
// `codexclaw` `crw`, and the CLI table maps `cxc doctor` to `crw doctor harness` and `cxc
// enable` to `crw install features enable`. Every other check name, severity, evidence string
// and repair hint is the oracle text, so a corpus expectation substituted the same way matches
// this output byte for byte.
//
// Windows and WSL are out of the port scope (inventory.md: win-exec.ts and wsl.ts are OUT):
// DoctorOptions.wslDeps and the warning branch of checkWslResidency are not ported, and
// HarnessWslCheck answers the branch the oracle takes off WSL. Every function that would run
// `codex` takes the runner as an argument, so no caller or test needs a real binary.
package doctor

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install/configguard"
)

// HarnessSeverity is Severity (doctor.ts:23): the state of one check, and of the rollup.
type HarnessSeverity string

// The three severities, spelled as the oracle spells them.
const (
	HarnessPass HarnessSeverity = "PASS"
	HarnessWarn HarnessSeverity = "WARN"
	HarnessFail HarnessSeverity = "FAIL"
)

// HarnessSchemaVersion is the schemaVersion DoctorReport carries for --json consumers
// (doctor.ts:36).
const HarnessSchemaVersion = 1

// HarnessCheck is CheckResult (doctor.ts:25-32): one check, its severity and the concrete
// evidence behind it (a path, a count or a parsed value, never a bare verdict), with the repair
// command a non-PASS severity may carry.
type HarnessCheck struct {
	Name     string          `json:"name"`
	Severity HarnessSeverity `json:"severity"`
	Evidence string          `json:"evidence"`
	Repair   string          `json:"repair,omitempty"`
}

// HarnessReport is DoctorReport (doctor.ts:34-45): the checks, their rollup, and the versions
// the header shows. An empty optional field is omitted, as the oracle omits an undefined one.
type HarnessReport struct {
	SchemaVersion int             `json:"schemaVersion"`
	Overall       HarnessSeverity `json:"overall"`
	Checks        []HarnessCheck  `json:"checks"`
	PluginVersion string          `json:"pluginVersion,omitempty"`
	CodexVersion  string          `json:"codexVersion,omitempty"`
	ActiveSurface string          `json:"activeSurface,omitempty"`
}

// HarnessOptions is DoctorOptions (doctor.ts:47-62), minus wslDeps (WSL is out of scope).
// SessionID and AgentID keep the three states the oracle reads: nil is an absent option, a
// pointer is the explicit value, and a pointer to the empty string is the oracle explicit
// null -- both are falsy there, which requests an unverified report. The observation fields
// are the oracle numbers: epoch milliseconds, nil unset.
type HarnessOptions struct {
	CodexHome           string
	PluginKey           string
	SessionID           *string
	AgentID             *string
	ObservationNow      *int64
	ObservationMaxAgeMS *int64
}

// HarnessRun is one runner answer (doctor.ts:132): the exit status and the two streams. Status
// nil is the oracle null status, a spawnSync that threw or a process ended by a signal.
type HarnessRun struct {
	Status *int
	Stdout string
	Stderr string
}

// HarnessRunner is the injected subprocess seam (the runner / agRunner argument of the oracle):
// the file and its arguments, and the completed run. A runner that cannot start the process
// answers HarnessRun{Status: nil, Stderr: <message>}, the object the oracle catch clause builds.
type HarnessRunner func(file string, args []string) HarnessRun

// harnessReportIsDir is isDir (doctor.ts:64-70): a stat that reads false instead of throwing.
func harnessReportIsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// HarnessRollup is rollup (doctor.ts:73-78): the worst severity wins, FAIL over WARN over PASS,
// and a list with neither FAIL nor WARN -- including the empty list -- PASSes.
func HarnessRollup(checks []HarnessCheck) HarnessSeverity {
	for _, check := range checks {
		if check.Severity == HarnessFail {
			return HarnessFail
		}
	}
	for _, check := range checks {
		if check.Severity == HarnessWarn {
			return HarnessWarn
		}
	}
	return HarnessPass
}

// harnessReportDetectCodexVersion is detectCodexVersion (doctor.ts:89-98): the first
// major.minor.patch in `codex --version` stdout, else the trimmed stdout, and nil when the run
// did not exit 0, printed nothing or never ran.
func harnessReportDetectCodexVersion(run HarnessRunner) *string {
	result := run("codex", []string{"--version"})
	if result.Status == nil || *result.Status != 0 || result.Stdout == "" {
		return nil
	}
	// JavaScript \d is an ASCII digit; the first run of three dotted groups wins.
	if match := regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+`).FindString(result.Stdout); match != "" {
		return &match
	}
	trimmed := text.Trim(result.Stdout)
	return &trimmed
}

// harnessReportJSSpace is the JavaScript \s set (WhiteSpace plus LineTerminator), the separator
// set of line.trim().split(/\s+/) in parseDoctorFeatures. internal/pabcd/text.Trim strips the
// same set: U+FEFF separates, U+0085 does not, so a BOM-prefixed line parses and a NEL-joined
// one stays a single field.
func harnessReportJSSpace(r rune) bool {
	switch r {
	case 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x20, 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// ParseHarnessFeatures is parseDoctorFeatures (doctor.ts:118-130): read `codex features list`,
// where each row is `name  stage  true|false`. The first field must equal a declared flag
// exactly (a sibling key like multi_agent_v2 never satisfies multi_agent), the last field must
// read true or false in any case (any other spelling leaves the flag unseen, which the check
// reports as not enabled), and a later row wins over an earlier one. Undeclared keys are
// ignored.
//
// The declared list is configguard.DeclaredFeatures, the config guard port of the same
// vocabulary (CRW-345): it is the oracle list -- multi_agent, goals, hooks,
// default_mode_request_user_input, in that order, without multi_agent_v2 -- so the doctor and
// the config guard cannot drift.
func ParseHarnessFeatures(stdout string) map[string]bool {
	declared := configguard.DeclaredFeatures()
	state := make(map[string]bool)
	for _, line := range text.SplitLines(stdout) {
		fields := strings.FieldsFunc(line, harnessReportJSSpace)
		if len(fields) < 2 || !slices.Contains(declared, configguard.DeclaredFeature(fields[0])) {
			continue
		}
		switch strings.ToLower(fields[len(fields)-1]) {
		case "true":
			state[fields[0]] = true
		case "false":
			state[fields[0]] = false
		}
	}
	return state
}

// harnessReportSoft is DOCTOR_SOFT_FEATURES membership (doctor.ts:115): the declared flag
// whose absence warns instead of failing. It reads the config guard soft set, the same
// vocabulary.
func harnessReportSoft(key string) bool {
	return slices.Contains(configguard.SoftFeatures(), configguard.DeclaredFeature(key))
}

// HarnessFeaturesCheck is buildDeclaredFeaturesCheck (doctor.ts:132-164): are the feature flags
// crw declares actually on? A missing hard flag fails (activation never ran); a missing soft
// flag only warns (reduced capability is not breakage, and a Plan-mode-only user must not sit
// permanently red); and an unreachable codex warns rather than fails, because a diagnostic
// must not manufacture a verdict about state it could not read.
func HarnessFeaturesCheck(run HarnessRun) HarnessCheck {
	if run.Status == nil || *run.Status != 0 {
		evidence := "could not read 'codex features list'"
		if run.Status != nil {
			evidence += fmt.Sprintf(" (exit %d)", *run.Status)
		}
		if stderr := text.Trim(run.Stderr); stderr != "" {
			evidence += ": " + harnessReportCut(stderr, 160)
		}
		return HarnessCheck{
			Name:     "features",
			Severity: HarnessWarn,
			Evidence: evidence,
			Repair:   "ensure the `codex` binary is on PATH, then re-run `crw doctor harness`",
		}
	}
	state := ParseHarnessFeatures(run.Stdout)
	declared := configguard.DeclaredFeatures()
	off := make([]string, 0, len(declared))
	for _, key := range declared {
		if !state[string(key)] {
			off = append(off, string(key))
		}
	}
	total := len(declared)
	if len(off) == 0 {
		return HarnessCheck{
			Name:     "features",
			Severity: HarnessPass,
			Evidence: fmt.Sprintf("%d/%d declared flag(s) enabled", total, total),
		}
	}
	hardOff := make([]string, 0, len(off))
	for _, key := range off {
		if !harnessReportSoft(key) {
			hardOff = append(hardOff, key)
		}
	}
	if len(hardOff) > 0 {
		return HarnessCheck{
			Name:     "features",
			Severity: HarnessFail,
			Evidence: fmt.Sprintf("%d/%d enabled; crw requires [%s]", total-len(off), total, strings.Join(hardOff, ", ")),
			Repair:   "crw install features enable",
		}
	}
	return HarnessCheck{
		Name:     "features",
		Severity: HarnessWarn,
		Evidence: fmt.Sprintf("%d/%d enabled; optional [%s] off — request_user_input is not exposed in Default mode (Plan mode is unaffected)", total-len(off), total, strings.Join(off, ", ")),
		Repair:   "codex features enable " + strings.Join(off, " "),
	}
}

// harnessReportCut is the JavaScript slice(0, n) measured in UTF-16 code units (doctor.ts:137).
// When the cut falls inside a surrogate pair the oracle keeps the lone high surrogate, and UTF-8
// encoding writes that as U+FFFD on the way to a terminal or a file, so this returns the kept
// prefix plus U+FFFD for that one case.
func harnessReportCut(s string, n int) string {
	units := 0
	for i, r := range s {
		size := 1
		if r > 0xFFFF {
			size = 2
		}
		if units+size > n {
			if units == n-1 && size == 2 {
				return s[:i] + "\uFFFD"
			}
			return s[:i]
		}
		units += size
	}
	return s
}

// HarnessWslCheck is the branch checkWslResidency takes off WSL (doctor.ts:193-196). The WSL
// probe and its warning branch belong to wsl.ts, which this port leaves out.
func HarnessWslCheck() HarnessCheck {
	return HarnessCheck{Name: "wsl", Severity: HarnessPass, Evidence: "not running under WSL"}
}

// RenderHarnessReport is renderDoctor (doctor.ts:648-661): one `[SEVERITY] name: evidence` line
// per check, the repair on its own indented line and only for a non-PASS severity, then the
// host versions above the checks (codex, then the plugin) and the overall line last. The
// reporter writes the result with its own trailing newline; this returns none.
func RenderHarnessReport(report HarnessReport) string {
	lines := make([]string, 0, len(report.Checks)+3)
	for _, check := range report.Checks {
		line := "[" + string(check.Severity) + "] " + check.Name + ": " + check.Evidence
		if check.Repair != "" && check.Severity != HarnessPass {
			line += "\n    repair: " + check.Repair
		}
		lines = append(lines, line)
	}
	if report.PluginVersion != "" {
		lines = append([]string{"crw v" + report.PluginVersion}, lines...)
	}
	if report.CodexVersion != "" {
		lines = append([]string{"codex v" + report.CodexVersion}, lines...)
	}
	lines = append(lines, "overall: "+string(report.Overall))
	return strings.Join(lines, "\n")
}
