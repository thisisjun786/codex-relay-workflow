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
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
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
// command a non-PASS severity may carry. Repair keeps the oracle's three states (repair?:
// string): nil is an absent repair, which --json omits and the text drops, while a pointer to an
// empty string is a present empty value, which --json keeps and the text still drops.
type HarnessCheck struct {
	Name     string          `json:"name"`
	Severity HarnessSeverity `json:"severity"`
	Evidence string          `json:"evidence"`
	Repair   *string         `json:"repair,omitempty"`
}

// harnessReportRepair is the repair this port's own builders pass: they use "" for an omitted
// repair (the oracle's builders either set a repair or leave it undefined, doctor.ts:138-162,
// :410-443), so an empty s is nil and any other s is a present value. A present-but-empty repair
// is expressible with a pointer to "" directly; no builder here produces one, because no oracle
// builder does.
func harnessReportRepair(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// harnessReportJSONString is JSON.stringify of a string: pyjson's writer with ensure_ascii=false
// (Unicode) and ReplacedBytes. A WTF-8 lone surrogate -- the three bytes harnessReportCut keeps --
// is written as its \u escape (lower-case hex, the \udXXX JSON.stringify writes), while any other
// byte that is not UTF-8 is written as U+FFFD: that is what Node's UTF-8 decoder already did to
// the oracle's stderr string, so a probe's invalid byte is U+FFFD in both, where SurrogateEscapes
// would spell it \udcXX and encoding/json alone would spell the kept surrogate U+FFFD. The quote,
// the backslash and the controls are escaped and every other character -- the em dash included --
// stands as it is.
func harnessReportJSONString(s string) string {
	return pyjson.Dumps(s, pyjson.Options{Unicode: true, Bytes: pyjson.ReplacedBytes})
}

// MarshalJSON is JSON.stringify of a check (doctor.ts:25-32 read by cli.ts:84): the four fields
// in that order, the repair key present only when Repair is non-nil, and every string quoted as
// harnessReportJSONString quotes it. The encoder re-indents these bytes to the report's indent, so
// the --json output matches JSON.stringify(report, null, 2) byte for byte.
func (c HarnessCheck) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"name":`)
	b.WriteString(harnessReportJSONString(c.Name))
	b.WriteString(`,"severity":`)
	b.WriteString(harnessReportJSONString(string(c.Severity)))
	b.WriteString(`,"evidence":`)
	b.WriteString(harnessReportJSONString(c.Evidence))
	if c.Repair != nil {
		b.WriteString(`,"repair":`)
		b.WriteString(harnessReportJSONString(*c.Repair))
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// HarnessReport is DoctorReport (doctor.ts:34-45): the checks, their rollup, and the versions
// the header shows. The three optional strings keep the oracle three states: nil is an undefined
// field, which --json omits and the header drops, while a pointer to an empty string is a
// present empty value, which --json keeps and the header still drops. MarshalJSON below keeps
// that shape while writing every string the way JSON.stringify writes it.
type HarnessReport struct {
	SchemaVersion int             `json:"schemaVersion"`
	Overall       HarnessSeverity `json:"overall"`
	Checks        []HarnessCheck  `json:"checks"`
	PluginVersion *string         `json:"pluginVersion,omitempty"`
	CodexVersion  *string         `json:"codexVersion,omitempty"`
	ActiveSurface *string         `json:"activeSurface,omitempty"`
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
// the file, its arguments, the per-call timeout the oracle passes, and the completed run. The
// Codex version probe passes 5 s (doctor.ts:91) and the features probe runDoctor makes passes
// 8 s (doctor.ts:349). A runner that cannot start the process, or kills one that did not finish
// in time, answers HarnessRun{Status: nil, Stderr: <message>}, the object the oracle catch
// clause builds.
type HarnessRunner func(file string, args []string, timeout time.Duration) HarnessRun

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
	result := run("codex", []string{"--version"}, 5*time.Second)
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
	// The runner's killed marker is the oracle's null status: spawnSync answers status null when its
	// timeout ended the probe, so no exit number belongs in the evidence (harnessRunExec, doctor.ts:352).
	if run.Status == nil || *run.Status != 0 {
		evidence := "could not read 'codex features list'"
		if run.Status != nil && *run.Status != harnessDriftKilled {
			evidence += fmt.Sprintf(" (exit %d)", *run.Status)
		}
		if stderr := text.Trim(run.Stderr); stderr != "" {
			evidence += ": " + harnessReportCut(stderr, 160)
		}
		return HarnessCheck{
			Name:     "features",
			Severity: HarnessWarn,
			Evidence: evidence,
			Repair:   harnessReportRepair("ensure the `codex` binary is on PATH, then re-run `crw doctor harness`"),
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
			Repair:   harnessReportRepair("crw install features enable"),
		}
	}
	return HarnessCheck{
		Name:     "features",
		Severity: HarnessWarn,
		Evidence: fmt.Sprintf("%d/%d enabled; optional [%s] off — request_user_input is not exposed in Default mode (Plan mode is unaffected)", total-len(off), total, strings.Join(off, ", ")),
		Repair:   harnessReportRepair("codex features enable " + strings.Join(off, " ")),
	}
}

// harnessReportCut is the JavaScript slice(0, n) measured in UTF-16 code units (doctor.ts:137).
// When the cut falls inside a surrogate pair the oracle keeps the lone high surrogate: this
// returns the kept prefix with that surrogate's WTF-8 bytes (ED A0..BF 80..BF), the representation
// internal/pyjson reads, so the JSON writer spells it as the \udXXX escape JSON.stringify writes
// and the text renderer spells it as the U+FFFD the UTF-8 encoder writes.
func harnessReportCut(s string, n int) string {
	units := 0
	for i, r := range s {
		size := 1
		if r > 0xFFFF {
			size = 2
		}
		if units+size > n {
			if units == n-1 && size == 2 {
				return s[:i] + harnessReportWTF8HighSurrogate(r)
			}
			return s[:i]
		}
		units += size
	}
	return s
}

// harnessReportWTF8HighSurrogate is the three WTF-8 bytes of r's high surrogate (r is the astral
// rune whose pair the cut split): the encoding of a lone surrogate a Go string can hold, which
// pyjson.CodePoint reads as that surrogate again.
func harnessReportWTF8HighSurrogate(r rune) string {
	high := rune(0xD800 + (r-0x10000)>>10)
	return string([]byte{byte(0xE0 | high>>12), byte(0x80 | (high>>6)&0x3F), byte(0x80 | high&0x3F)})
}

// harnessReportText is the text the UTF-8 encoder writes for s: a lone high surrogate (the three
// WTF-8 bytes harnessReportCut keeps) becomes U+FFFD, as Node's encoder and toWellFormed() write
// it. Every other character stands as it is.
func harnessReportText(s string) string {
	lone := false
	for i := 0; i < len(s); {
		r, size := pyjson.CodePoint(s, i)
		if pyjson.IsSurrogate(r) {
			lone = true
			break
		}
		i += size
	}
	if !lone {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := pyjson.CodePoint(s, i)
		if pyjson.IsSurrogate(r) {
			b.WriteRune(utf8.RuneError)
		} else {
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
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
		line := "[" + string(check.Severity) + "] " + harnessReportText(check.Name) + ": " + harnessReportText(check.Evidence)
		if check.Repair != nil && *check.Repair != "" && check.Severity != HarnessPass {
			line += "\n    repair: " + harnessReportText(*check.Repair)
		}
		lines = append(lines, line)
	}
	if report.PluginVersion != nil && *report.PluginVersion != "" {
		lines = append([]string{harnessReportText("crw v" + *report.PluginVersion)}, lines...)
	}
	if report.CodexVersion != nil && *report.CodexVersion != "" {
		lines = append([]string{harnessReportText("codex v" + *report.CodexVersion)}, lines...)
	}
	lines = append(lines, "overall: "+string(report.Overall))
	return strings.Join(lines, "\n")
}

// MarshalJSON is JSON.stringify of the report (doctor.ts:34-45 read by cli.ts:83-84): the six
// fields in the oracle's order, each optional string present only when it is not undefined, and
// every string quoted as harnessReportJSONString quotes it -- so a lone surrogate the manifest
// version, the codex version or the active surface carries is written as its escape, not as the
// U+FFFD the text renderer writes for the same string. The checks array is marshalled by
// encoding/json, which delegates each element to HarnessCheck.MarshalJSON.
func (r HarnessReport) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"schemaVersion":`)
	b.WriteString(strconv.Itoa(r.SchemaVersion))
	b.WriteString(`,"overall":`)
	b.WriteString(harnessReportJSONString(string(r.Overall)))
	b.WriteString(`,"checks":`)
	checks, err := harnessReportChecksJSON(r.Checks)
	if err != nil {
		return nil, err
	}
	b.Write(checks)
	if r.PluginVersion != nil {
		b.WriteString(`,"pluginVersion":`)
		b.WriteString(harnessReportJSONString(*r.PluginVersion))
	}
	if r.CodexVersion != nil {
		b.WriteString(`,"codexVersion":`)
		b.WriteString(harnessReportJSONString(*r.CodexVersion))
	}
	if r.ActiveSurface != nil {
		b.WriteString(`,"activeSurface":`)
		b.WriteString(harnessReportJSONString(*r.ActiveSurface))
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// harnessReportChecksJSON is JSON.stringify of the checks array (cli.ts:84): encoding/json with HTML
// escaping off, so an evidence or repair string holding <, > or & keeps the character JSON.stringify
// writes instead of encoding/json's \u003c, \u003e and \u0026. Each element is still written by
// HarnessCheck.MarshalJSON, so the lone-surrogate representation is kept; the encoder's own newline
// is trimmed, since the array sits inside this object.
func harnessReportChecksJSON(checks []HarnessCheck) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(checks); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// HarnessOptions is DoctorOptions (doctor.ts:47-62), minus wslDeps (WSL is out of scope).
