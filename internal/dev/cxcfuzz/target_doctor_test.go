//go:build dev

package cxcfuzz

// This file is CRW-710's own test for the doctor target: the shapes the issue names, the red
// case (the target is unknown before it is registered), the oracle-free replay the harness runs,
// and the pin that keeps the generator's strings out of the rename table's way. The seed cases
// themselves live in testdata/doctor/cases.json and are replayed by TestCases.

import (
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
)

// TestDoctorTargetIsRegistered is the issue's red case: before this target is registered,
// `crw-dev fuzz doctor` ends unknown target. The recorded red output is the coordinator's
// evidence under /state; this test is the in-repo half, so a later change that drops the
// registration fails here as well as on the command line.
func TestDoctorTargetIsRegistered(t *testing.T) {
	target, ok := Lookup("doctor")
	if !ok {
		t.Fatalf("the doctor target is not registered (registered: %s)", strings.Join(Names(), ", "))
	}
	if target.Name != "doctor" || target.Generate == nil || target.Go == nil || target.Compare == nil {
		t.Fatalf("the doctor target is incomplete: %+v", target)
	}
	if target.Oracle.Command != "node" {
		t.Fatalf("the doctor target's oracle command = %q, want node", target.Oracle.Command)
	}
	if want := filepath.Join("testdata", "doctor", "shim.mjs"); !strings.HasSuffix(target.Oracle.Shim, want) {
		t.Fatalf("the doctor target's shim = %q, want a path ending %q", target.Oracle.Shim, want)
	}
}

// TestDoctorGoAnswerShape pins what the Go side answers: the report as the text renderer prints
// it and as the --json document, the two things the comparison compares.
func TestDoctorGoAnswerShape(t *testing.T) {
	input := `{"report":{"checks":[{"name":"x","severity":"WARN","evidence":"e"}]}}`
	value, err := decode(input)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := doctorGo(value, RootEnv(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	text, jsonText, err := doctorAnswer(answer)
	if err != nil {
		t.Fatal(err)
	}
	if want := "[WARN] x: e\noverall: WARN"; text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
	if !strings.Contains(jsonText, "\"name\": \"x\"") || !strings.HasSuffix(jsonText, "}\n") {
		t.Fatalf("json = %q, want the indented report and a trailing newline", jsonText)
	}
	// The Go answer is the report core's own output, not a second renderer.
	if _, err := doctor.RenderHarnessReportJSON(doctor.HarnessReport{}); err != nil {
		t.Fatalf("the exported JSON wrapper failed: %v", err)
	}
}

// TestDoctorRepairStates pins the three states of a check's repair, which is the shape the issue
// names: a repair, a present empty repair (the JSON keeps the key, the text drops the line) and
// no repair key at all (the JSON omits it).
func TestDoctorRepairStates(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{"absent", `{"report":{"checks":[{"name":"x","severity":"WARN","evidence":"e"}]}}`, `{"name": "x", "severity": "WARN", "evidence": "e"}`},
		{"empty", `{"report":{"checks":[{"name":"x","severity":"WARN","evidence":"e","repair":""}]}}`, `"repair": ""`},
		{"present", `{"report":{"checks":[{"name":"x","severity":"WARN","evidence":"e","repair":"r"}]}}`, `"repair": "r"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			value, err := decode(test.input)
			if err != nil {
				t.Fatal(err)
			}
			answer, err := doctorGo(value, RootEnv(t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			_, jsonText, err := doctorAnswer(answer)
			if err != nil {
				t.Fatal(err)
			}
			switch test.name {
			case "absent":
				if strings.Contains(jsonText, "repair") {
					t.Fatalf("an absent repair was written: %s", jsonText)
				}
			default:
				if !strings.Contains(jsonText, test.want) {
					t.Fatalf("json %s does not carry %s", jsonText, test.want)
				}
			}
		})
	}
}

// TestDoctorCutKeepsALoneSurrogate is the CRW-346/618 reproduction the issue names as a seed
// case: a stderr cut at 160 UTF-16 units that falls between an astral pair's halves. The text
// drops the kept surrogate as U+FFFD and the JSON spells it as its \\ud83d escape.
func TestDoctorCutKeepsALoneSurrogate(t *testing.T) {
	stderr := strings.Repeat("x", 159) + "\U0001F600" + "y"
	check := doctor.HarnessFeaturesCheck(doctor.HarnessRun{Status: doctorStatusOf(t, 3), Stderr: stderr})
	if !strings.HasSuffix(check.Evidence, strings.Repeat("x", 159)+"\xed\xa0\xbd") {
		t.Fatalf("the evidence does not end in the kept lone high surrogate: % x", []byte(check.Evidence))
	}
	report := doctor.HarnessReport{SchemaVersion: doctor.HarnessSchemaVersion, Overall: doctor.HarnessRollup([]doctor.HarnessCheck{check}), Checks: []doctor.HarnessCheck{check}}
	raw, err := doctor.RenderHarnessReportJSON(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\\ud83d") {
		t.Fatalf("the JSON does not carry the lone surrogate as its escape:\n%s", raw)
	}
	if !strings.Contains(doctor.RenderHarnessReport(report), "\uFFFD") {
		t.Fatalf("the text does not spell the kept surrogate U+FFFD")
	}
}

// TestDoctorGeneratorStringsSurviveTheRename pins the target's one subtle invariant: the
// comparison renames the oracle's answer through contract/schema/cxc/name-substitution.json, and
// the rename runs over generated content too, so no string the generator can produce may spell
// text the table rewrites. A pool entry that did would be renamed on the oracle side only and
// read as a divergence the target invented.
func TestDoctorGeneratorStringsSurviveTheRename(t *testing.T) {
	sub, err := doctorSubstitution()
	if err != nil {
		t.Fatal(err)
	}
	pools := append(append([]string{}, doctorTexts()...), doctorLongTexts()...)
	for _, text := range pools {
		if got := sub.Expected(text); got != text {
			t.Errorf("the pool string %q is rewritten to %q by the rename table", text, got)
		}
	}
	for _, repair := range doctorRepairs() {
		if got := sub.Expected(repair); got != repair {
			t.Errorf("the repair %q is rewritten to %q by the rename table", repair, got)
		}
	}
	// The generated listing is a table of flag names, so it must survive too.
	for _, name := range doctorFeatureNames() {
		if got := sub.Expected(name); got != name {
			t.Errorf("the flag name %q is rewritten to %q by the rename table", name, got)
		}
	}
}

// TestDoctorGeneratorShapes walks the generator over many seeds and checks the shapes the issue
// names are actually reachable: an empty repair, a lone surrogate, the line separators, markup
// characters, an emoji, and each of the probe statuses.
func TestDoctorGeneratorShapes(t *testing.T) {
	// canonical writes a lone surrogate as its escape and every other character as itself, so the
	// expected shapes are the escape for a surrogate and the character for the rest.
	shapes := []string{"\\ud800", "\\udfff", "\\u2028", "\\u2029", "\\ud83d\\ude00", "<plugin>", "\"repair\": \"\"", "\"status\": -1", "\"status\": 3", "\"status\": null"}
	seen := map[string]bool{}
	for seed := int64(0); seed < 400; seed++ {
		value := doctorGenerate(rand.New(rand.NewSource(seed)), int(seed))
		text := canonical(value)
		for _, want := range shapes {
			if strings.Contains(text, want) {
				seen[want] = true
			}
		}
	}
	for _, want := range shapes {
		if !seen[want] {
			t.Errorf("400 seeds never produced %q", want)
		}
	}
}

// TestDoctorCasesAreReplayable is the harness's own replay, run here so a failure names the
// doctor target: every case must hold without Node.
func TestDoctorCasesAreReplayable(t *testing.T) {
	target, ok := Lookup("doctor")
	if !ok {
		t.Fatal("the doctor target is not registered")
	}
	cases, err := LoadCases(filepath.Join("testdata", "doctor"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no doctor cases")
	}
	tags := map[string]int{}
	for _, c := range cases {
		if problem := CheckCase(target, c); problem != "" {
			t.Errorf("%s: %s", c.Name, problem)
		}
		tags[c.Tag]++
	}
	if tags[TagIdentical] == 0 {
		t.Error("no identical doctor case")
	}
	if tags[TagOpen] == 0 {
		t.Error("no open doctor case: the divergence found by the campaign must stay pinned")
	}
}

// TestDoctorOpenCaseIsTheCutDivergence pins what the open case holds: the oracle keeps the whole
// WTF-8 lone surrogate, the port keeps two of its three bytes, so the case is pinned at the Go
// output and would fail if the port moved without the case being re-pinned.
func TestDoctorOpenCaseIsTheCutDivergence(t *testing.T) {
	cases, err := LoadCases(filepath.Join("testdata", "doctor"))
	if err != nil {
		t.Fatal(err)
	}
	var open *Case
	for i := range cases {
		if cases[i].Tag == TagOpen {
			open = &cases[i]
		}
	}
	if open == nil {
		t.Fatal("no open doctor case")
	}
	if open.Record == "" {
		t.Fatal("the open case names no follow-up record")
	}
	if !strings.Contains(open.Go, "\\udced") {
		t.Fatalf("the pinned Go answer does not hold the cut WTF-8 surrogate: %s", open.Go)
	}
}

// TestDoctorMissingRequiredFieldsReadEmpty pins the port half of the check contract the shrinker
// depends on: name, severity and evidence are required and have no absent state, so a check the
// generic reducer has stripped a field from reads as the empty string and the JSON keeps the key.
// The shim fills the same three fields with the empty string, so the two sides read one malformed
// check the same way. Without that, a shrink candidate that deleted a required field would still
// return Differ -- for a reason the shrinker invented -- and could replace the input that really
// differed in a saved divergence.
func TestDoctorMissingRequiredFieldsReadEmpty(t *testing.T) {
	input := "{\"report\":{\"checks\":[{\"evidence\":\"e\"},{\"name\":\"n\"}]}}"
	value, err := decode(input)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := doctorGo(value, RootEnv(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	text, jsonText, err := doctorAnswer(answer)
	if err != nil {
		t.Fatal(err)
	}
	if want := "[] : e\n[] n: \noverall: PASS"; text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
	for _, want := range []string{`"name": ""`, `"severity": ""`, `"evidence": ""`} {
		if !strings.Contains(jsonText, want) {
			t.Fatalf("json does not carry %s:\n%s", want, jsonText)
		}
	}
}

// TestDoctorMalformedCheckAgreesAcrossSides is the regression the campaign exposed: the generic
// reducer deletes an object keys before it reduces values, so a shrink candidate can lose a
// check required fields. That candidate must not become a difference of its own, or the shrinker
// (which keeps any candidate with the same verdict kind) would pin a malformed-input artifact in
// place of the input that really differed. The oracle side of the same input is the answer the
// shim produces: the three required fields filled with the empty string.
func TestDoctorMalformedCheckAgreesAcrossSides(t *testing.T) {
	target, ok := Lookup("doctor")
	if !ok {
		t.Fatal("the doctor target is not registered")
	}
	root, err := os.MkdirTemp("", "cxcfuzz-doctor-shrink-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	malformed, err := decode(`{"report":{"checks":[{"evidence":"e"}]}}`)
	if err != nil {
		t.Fatal(err)
	}
	goValue, err := target.Go(malformed, RootEnv(root))
	if err != nil {
		t.Fatal(err)
	}
	oracle, err := decode(`{"text":"[] : e\noverall: PASS","json":"{\n  \"schemaVersion\": 1,\n  \"overall\": \"PASS\",\n  \"checks\": [\n    {\n      \"name\": \"\",\n      \"severity\": \"\",\n      \"evidence\": \"e\"\n    }\n  ]\n}\n"}`)
	if err != nil {
		t.Fatal(err)
	}
	if verdict := target.Compare(goValue, oracle); verdict.Kind != Same {
		t.Fatalf("a check missing its required fields reads differently on the two sides: %s", verdict.Detail)
	}
}

// TestDoctorShrinkStaysInsideTheCheckContract drives the real reducer and the real oracle over an
// input that differs inside a check, which is the shape the shrinker would have wandered out of:
// the reducer deletes an object keys before it reduces values, so its candidates can lose a
// check required fields. Every candidate that does must compare Same (both sides read the missing
// field the same way), so the keep test rejects it and the shrunk input the campaign saves still
// carries the fields -- the difference that is pinned stays the difference that was found.
func TestDoctorShrinkStaysInsideTheCheckContract(t *testing.T) {
	requireNode(t)
	target, ok := Lookup("doctor")
	if !ok {
		t.Fatal("the doctor target is not registered")
	}
	pool, err := NewPool(target.Oracle, 1, DefaultTimeout, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.Close() }()
	evaluate := func(value any) Verdict {
		root, err := os.MkdirTemp("", "cxcfuzz-doctor-shrink-")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.RemoveAll(root) }()
		if err := PrepareRoot(root); err != nil {
			t.Fatal(err)
		}
		goValue, goErr := target.Go(value, RootEnv(root))
		var goOut any = goValue
		if goErr != nil {
			goOut = errorValue(goErr)
		}
		oracleText, err := pool.Call(canonical(value), root)
		if err != nil {
			t.Fatal(err)
		}
		oracleValue, err := decode(oracleText)
		if err != nil {
			t.Fatal(err)
		}
		return target.Compare(goOut, oracleValue)
	}
	// The difference is the WTF-8 lone surrogate harnessReportCut measures as three units; it sits
	// in the run stderr, and a check sits beside it so the reducer has fields to delete.
	input, err := decode(`{"report":{"checks":[{"name":"n","severity":"WARN","evidence":"e"}]},"run":{"stderr":"` + strings.Repeat("x", 157) + `\udced\udca0"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if verdict := evaluate(input); verdict.Kind != Differ {
		t.Fatalf("the input does not differ: %s", verdict.Detail)
	}
	shrunk, _ := Shrink(input, ShrinkAttempts, func(candidate any) bool {
		return evaluate(candidate).Kind == Differ
	})
	// The pinned input is still the one that really differs.
	if !strings.Contains(canonical(shrunk), `\udced`) {
		t.Fatalf("the shrunk input no longer carries the difference: %s", canonical(shrunk))
	}
	if verdict := evaluate(shrunk); verdict.Kind != Differ {
		t.Fatalf("the shrunk input does not differ: %s", verdict.Detail)
	}
	// The manufactured candidate the shrinker must never keep: a check missing its required
	// fields, with no run. Both sides read it the same way, so it compares Same and the keep
	// predicate rejects it.
	manufactured, err := decode(`{"report":{"checks":[{"evidence":"e"}]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if verdict := evaluate(manufactured); verdict.Kind != Same {
		t.Fatalf("a check missing its required fields is a difference the shrinker could pin: %s", verdict.Detail)
	}
}

func doctorStatusOf(t *testing.T, code int) *int {
	t.Helper()
	return &code
}
