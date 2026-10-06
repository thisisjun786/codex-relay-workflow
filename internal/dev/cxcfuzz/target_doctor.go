//go:build dev

package cxcfuzz

import (
	"fmt"
	"math/rand"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/cxccorpus"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
)

// doctorTarget is the differential-fuzz subject for the doctor harness report (CRW-710): one
// generated report value, and both sides' answers to it -- the report as the text renderer prints
// it and as the --json document the command writes.
//
//   - Go: doctor.RenderHarnessReport, the report JSON writer (doctor.RenderHarnessReportJSON) and
//     doctor.HarnessFeaturesCheck, in internal/runtime/doctor.
//   - Oracle: doctor.js renderDoctor, JSON.stringify(report, null, 2) plus the trailing newline
//     (cli.ts:83-84) and buildDeclaredFeaturesCheck, run by testdata/doctor/shim.mjs.
//
// The oracle answers in CXC names and the port answers in CRW names, so the comparison runs the
// oracle's answer through the corpus rename table (contract/schema/cxc/name-substitution.json,
// the table internal/contracttest replays a fixture with) before the two answers are compared as
// bytes. That is why a pinned case's oracle field holds the oracle's answer in the port's names,
// exactly as a corpus fixture's expectation does.
//
// The generator's own strings never spell text the table renames: the rename runs over the
// oracle's whole answer, generated content included, so a generated "codexclaw" would be renamed
// on one side only and read as a divergence the target invented.
// TestDoctorGeneratorStringsSurviveTheRename pins that.
func doctorTarget() Target {
	return Target{
		Name:     "doctor",
		Generate: doctorGenerate,
		Go:       doctorGo,
		Oracle:   Oracle{Command: "node", Shim: shimPath("doctor"), Root: DefaultOracleRoot},
		Compare:  doctorCompare(),
	}
}

// doctorSubstitution loads the corpus rename table from this checkout.
func doctorSubstitution() (*cxccorpus.Substituter, error) {
	root, err := repositoryRoot()
	if err != nil {
		return nil, err
	}
	return cxccorpus.LoadSubstitution(root)
}

// doctorCompare is the target's comparison: both sides answer {text, json}, the oracle's answer
// is renamed into the port's names, and the two are compared as bytes. A table that cannot be
// read, or an answer that is not the {text, json} shape (a worker error, a Go failure), is a
// difference rather than a pass.
func doctorCompare() func(goOut, oracleOut any) Verdict {
	sub, err := doctorSubstitution()
	return func(goOut, oracleOut any) Verdict {
		if err != nil {
			return Verdict{Kind: Differ, Detail: "the name-substitution table could not be read: " + err.Error()}
		}
		goText, goJSON, err := doctorAnswer(goOut)
		if err != nil {
			return Verdict{Kind: Differ, Detail: "the go answer: " + err.Error()}
		}
		oracleText, oracleJSON, err := doctorAnswer(oracleOut)
		if err != nil {
			return Verdict{Kind: Differ, Detail: "the oracle answer: " + err.Error()}
		}
		if want := sub.Expected(oracleText); goText != want {
			return Verdict{Kind: Differ, Detail: "the rendered text differs"}
		}
		if want := sub.Expected(oracleJSON); goJSON != want {
			return Verdict{Kind: Differ, Detail: "the --json document differs"}
		}
		return Verdict{Kind: Same}
	}
}

// doctorAnswer reads one side's answer: the report rendered as text and as the --json document.
func doctorAnswer(value any) (string, string, error) {
	text, err := doctorAnswerString(value, "text")
	if err != nil {
		return "", "", err
	}
	jsonText, err := doctorAnswerString(value, "json")
	if err != nil {
		return "", "", err
	}
	return text, jsonText, nil
}

func doctorAnswerString(value any, key string) (string, error) {
	raw, found := field(value, key)
	if !found {
		return "", fmt.Errorf("the answer carries no %s", key)
	}
	text, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("the answer's %s is %T, not a string", key, raw)
	}
	return text, nil
}

// doctorGo is the Go side: assemble the report from the input the way runDoctor assembles it and
// render it with the port's own text and JSON writers.
func doctorGo(input any, env Env) (any, error) {
	report, err := doctorReport(input)
	if err != nil {
		return nil, err
	}
	jsonText, err := doctor.RenderHarnessReportJSON(report)
	if err != nil {
		return nil, err
	}
	return pyjson.Object{
		{Key: "text", Value: doctor.RenderHarnessReport(report)},
		{Key: "json", Value: string(jsonText)},
	}, nil
}

// doctorReport builds the report: the input's checks, the declared-features check built from the
// input's probe run when it carries one, and the severity rollup over the whole list.
func doctorReport(input any) (doctor.HarnessReport, error) {
	// A missing report is an empty one, the way the oracle reads it: input.report is undefined
	// there and runDoctor's own default is an empty report object, so a case that carries only a
	// probe run still answers. Reading it as an error would make the shrinker's minimal input
	// ({} or a run alone) a divergence the target invented.
	source := any(nil)
	if raw, found := field(input, "report"); found {
		source = raw
	}
	if source == nil {
		source = pyjson.Object{}
	}
	checks, err := doctorChecks(source)
	if err != nil {
		return doctor.HarnessReport{}, err
	}
	if run, found := field(input, "run"); found && run != nil {
		check, err := doctorFeaturesCheck(run)
		if err != nil {
			return doctor.HarnessReport{}, err
		}
		checks = append(checks, check)
	}
	report := doctor.HarnessReport{
		SchemaVersion: doctor.HarnessSchemaVersion,
		Overall:       doctor.HarnessRollup(checks),
		Checks:        checks,
	}
	// The three optional strings keep the oracle's three states: an absent key is no value, a
	// present empty string is a present empty value.
	report.PluginVersion, err = doctorOptionalString(source, "pluginVersion")
	if err != nil {
		return doctor.HarnessReport{}, err
	}
	report.CodexVersion, err = doctorOptionalString(source, "codexVersion")
	if err != nil {
		return doctor.HarnessReport{}, err
	}
	report.ActiveSurface, err = doctorOptionalString(source, "activeSurface")
	if err != nil {
		return doctor.HarnessReport{}, err
	}
	return report, nil
}

// doctorChecks reads the report's checks; an absent list is an empty one, so the JSON document
// carries [] where the oracle's array is empty rather than Go's null.
func doctorChecks(source any) ([]doctor.HarnessCheck, error) {
	value, found := field(source, "checks")
	if !found || value == nil {
		return []doctor.HarnessCheck{}, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("checks must be an array, not %T", value)
	}
	checks := make([]doctor.HarnessCheck, 0, len(items))
	for i, item := range items {
		check, err := doctorCheck(item)
		if err != nil {
			return nil, fmt.Errorf("checks[%d]: %w", i, err)
		}
		checks = append(checks, check)
	}
	return checks, nil
}

// doctorCheck reads one check. A repair key that is present is a repair, an empty string included
// (the oracle's present-but-empty repair, which the text drops and the JSON keeps); an absent one
// is no repair at all.
func doctorCheck(item any) (doctor.HarnessCheck, error) {
	name, err := doctorOptionalString(item, "name")
	if err != nil {
		return doctor.HarnessCheck{}, err
	}
	severity, err := doctorOptionalString(item, "severity")
	if err != nil {
		return doctor.HarnessCheck{}, err
	}
	evidence, err := doctorOptionalString(item, "evidence")
	if err != nil {
		return doctor.HarnessCheck{}, err
	}
	check := doctor.HarnessCheck{Name: doctorText(name), Evidence: doctorText(evidence)}
	if severity != nil {
		check.Severity = doctor.HarnessSeverity(*severity)
	}
	repair, err := doctorOptionalString(item, "repair")
	if err != nil {
		return doctor.HarnessCheck{}, err
	}
	check.Repair = repair
	return check, nil
}

// doctorFeaturesCheck reads the probe run and asks the port's declared-features check.
func doctorFeaturesCheck(run any) (doctor.HarnessCheck, error) {
	status, err := doctorStatus(run)
	if err != nil {
		return doctor.HarnessCheck{}, err
	}
	stdout, err := doctorOptionalString(run, "stdout")
	if err != nil {
		return doctor.HarnessCheck{}, err
	}
	stderr, err := doctorOptionalString(run, "stderr")
	if err != nil {
		return doctor.HarnessCheck{}, err
	}
	return doctor.HarnessFeaturesCheck(doctor.HarnessRun{
		Status: status,
		Stdout: doctorText(stdout),
		Stderr: doctorText(stderr),
	}), nil
}

// doctorStatus reads the probe's exit status: absent or null is the oracle's null status (a probe
// the runner could not start, or one it killed), a number is the exit code.
func doctorStatus(run any) (*int, error) {
	value, found := field(run, "status")
	if !found || value == nil {
		return nil, nil
	}
	code, err := integer(value)
	if err != nil {
		return nil, fmt.Errorf("run.status: %w", err)
	}
	return &code, nil
}

// doctorOptionalString reads one optional string field: absent and null are nil, a string is the
// value (the empty string included), and any other type is an error.
func doctorOptionalString(value any, key string) (*string, error) {
	raw, found := field(value, key)
	if !found || raw == nil {
		return nil, nil
	}
	text, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("%s must be a string, not %T", key, raw)
	}
	return &text, nil
}

// doctorText is an optional string as the port reads it, with nil as the empty string.
func doctorText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// doctorFeatureNames is the declared-flag vocabulary the oracle lists (DOCTOR_DECLARED_FEATURES,
// doctor.js:114). The generator spells the oracle's list rather than the port's, so a drift
// between the two lists reads as a divergence instead of hiding behind the port's own list. It is
// a function, like every pool below, because this package does no work at program start (the
// harness rule: no package-level variable whose initializer does work).
func doctorFeatureNames() []string {
	return []string{"multi_agent", "goals", "hooks", "default_mode_request_user_input"}
}

// doctorTexts is the generator's string pool. It carries the shapes the issue names -- a lone
// surrogate, the two line separators, markup characters and an emoji -- and none of it spells
// text the rename table rewrites (see the package comment above).
func doctorTexts() []string {
	return []string{
		"",
		"ok",
		"<plugin> & <marketplace>",
		"a\u2028b",
		"a\u2029b",
		"\U0001F600 emoji",
		"지난번",
		"\xed\xa0\x80",
		"\xed\xbf\xbf",
		"quote \" backslash \\ tab \t",
		"\u0000nul",
		"\u0001\u001f",
		"e\u0301",
		"\u00a0",
		"\ufeff",
		"a\rb",
		"\u2028\u2029",
		"\U0001F600",
		"\xed\xa0\x80 end",
	}
}

// doctorLongTexts is the pool of longer strings, so a case sometimes crosses the 160 UTF-16 unit
// cut the features evidence takes, and the JSON writer has to spell a longer value.
func doctorLongTexts() []string {
	return []string{
		strings.Repeat("x", 159),
		strings.Repeat("x", 160),
		strings.Repeat("x", 161),
		strings.Repeat("x", 158) + "\U0001F600",
		strings.Repeat("x", 159) + "\U0001F600",
		strings.Repeat("x", 160) + "\U0001F600",
		strings.Repeat("x", 159) + "\xed\xa0\x80",
		strings.Repeat("\U0001F600", 40),
		strings.Repeat("a\u2028b", 40),
		strings.Repeat("<plugin> & <marketplace>", 8),
	}
}

// doctorRepairs is the repair pool: the empty string is the oracle's present-but-empty repair.
func doctorRepairs() []string {
	return []string{"", "codex plugin add <plugin>@<marketplace>", "restart the session"}
}

// doctorGenerate builds one report input: a check list, the optional report metadata, and the
// features probe run whose check the report appends.
func doctorGenerate(rng *rand.Rand, size int) any {
	checks := make([]any, 0, 3)
	for n := rng.Intn(4); n > 0; n-- {
		checks = append(checks, doctorGenerateCheck(rng))
	}
	report := pyjson.Object{
		{Key: "schemaVersion", Value: doctor.HarnessSchemaVersion},
		{Key: "checks", Value: checks},
	}
	for _, key := range []string{"pluginVersion", "codexVersion", "activeSurface"} {
		if rng.Intn(3) == 0 {
			report = report.Set(key, doctorGenerateText(rng, size))
		}
	}
	input := pyjson.Object{{Key: "report", Value: report}}
	if rng.Intn(4) > 0 {
		input = input.Set("run", doctorGenerateRun(rng, size))
	}
	return input
}

// doctorGenerateCheck builds one check, with a repair absent, present and empty, or present.
func doctorGenerateCheck(rng *rand.Rand) any {
	texts, repairs := doctorTexts(), doctorRepairs()
	check := pyjson.Object{
		{Key: "name", Value: texts[rng.Intn(len(texts))]},
		{Key: "severity", Value: []string{"PASS", "WARN", "FAIL"}[rng.Intn(3)]},
		{Key: "evidence", Value: texts[rng.Intn(len(texts))]},
	}
	switch rng.Intn(3) {
	case 0: // no repair key at all
	case 1:
		check = check.Set("repair", "")
	default:
		check = check.Set("repair", repairs[rng.Intn(len(repairs))])
	}
	return check
}

// doctorGenerateRun builds the features probe: the exit statuses the issue names -- 0, 3, the
// oracle's null and the port's killed marker -1 -- and a listing of the declared flags.
func doctorGenerateRun(rng *rand.Rand, size int) any {
	run := pyjson.Object{
		{Key: "stdout", Value: doctorGenerateListing(rng)},
		{Key: "stderr", Value: doctorGenerateStderr(rng, size)},
	}
	switch rng.Intn(4) {
	case 0:
		run = run.Set("status", 0)
	case 1:
		run = run.Set("status", 3)
	case 2:
		run = run.Set("status", -1)
	default:
		run = run.Set("status", nil)
	}
	return run
}

// doctorGenerateStderr builds a stderr near the 160 UTF-16 unit cut, so the cut's boundary case
// (a cut inside a surrogate pair) is generated, and otherwise a pool string.
func doctorGenerateStderr(rng *rand.Rand, size int) string {
	longs := doctorLongTexts()
	switch rng.Intn(3) {
	case 0:
		return strings.Repeat("x", 159) + "\U0001F600" + "y"
	case 1:
		return longs[rng.Intn(len(longs))]
	default:
		return doctorGenerateText(rng, size)
	}
}

// doctorGenerateListing builds a codex features list table over the declared flags, with the
// order shuffled and an undeclared sibling row sometimes present.
func doctorGenerateListing(rng *rand.Rand) string {
	names := doctorFeatureNames()
	lines := make([]string, 0, len(names)+1)
	for _, name := range names {
		stage := []string{"stable", "under development"}[rng.Intn(2)]
		lines = append(lines, fmt.Sprintf("%s  %s  %t", name, stage, rng.Intn(2) == 0))
	}
	if rng.Intn(4) == 0 {
		lines = append(lines, "multi_agent_v2  stable  true")
	}
	rng.Shuffle(len(lines), func(i, j int) { lines[i], lines[j] = lines[j], lines[i] })
	return strings.Join(lines, "\n")
}

// doctorGenerateText picks one pool string, sometimes repeated: size is the generator's case
// size, so a large case occasionally carries a longer evidence string.
func doctorGenerateText(rng *rand.Rand, size int) string {
	texts, longs := doctorTexts(), doctorLongTexts()
	if size%4 == 0 {
		return longs[rng.Intn(len(longs))]
	}
	text := texts[rng.Intn(len(texts))]
	if size%4 == 1 && text != "" {
		return strings.Repeat(text, 1+rng.Intn(3))
	}
	return text
}
