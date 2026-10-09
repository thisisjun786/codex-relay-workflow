package interview_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"

	I "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
)

type mindsOracleCases struct {
	Normalization []struct {
		ID         string
		Mind       I.Mind
		Raw, Round any
		Expected   []I.MindContradiction
	}
	Selection []struct {
		ID             string
		Tracker, Count any
		Expected       []I.Mind
	}
	Rounds []struct {
		Tracker  any
		Expected float64
	}
	Pending []struct {
		ID       string
		Rows     []any
		Tracker  any
		Expected I.PendingInterviewWork
	}
	Failures []struct {
		ID             string
		Tracker, Count any
		Error, Kind    string
	}
	Overflow struct {
		Row      string
		Expected I.PendingInterviewWork
	}
}

func mindsLoadCases(t *testing.T) mindsOracleCases {
	t.Helper()
	b, err := os.ReadFile("testdata/minds/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases mindsOracleCases
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}
func mindsValidRow() map[string]any {
	return map[string]any{"dimension": "goal", "contradiction": "gap", "severity": "high", "evidence": "plan.md:12"}
}
func mindsGolden(t *testing.T, name, got string) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", "minds", name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("%s golden differs: %d bytes, oracle %d", name, len(got), len(want))
	}
}
func TestMindsIDsAndPrompts(t *testing.T) {
	if got := I.Minds(); got != [5]I.Mind{"contrarian", "socratic", "ontologist", "evaluator", "simplifier"} {
		t.Fatalf("ids=%v", got)
	}
	for _, m := range I.Minds() {
		mindsGolden(t, string(m), I.MindRolePrompt(m))
	}
}
func TestMindsDispatchDirectiveGolden(t *testing.T) {
	mindsGolden(t, "dispatch", I.MindDispatchDirective)
}
func TestMindsDispatchOwnerPointer(t *testing.T) {
	ref := regexp.MustCompile(`references/[\w/-]+\.md`).FindString(I.MindDispatchDirective)
	if ref != "references/mind-dispatch.md" {
		t.Fatalf("owner pointer=%q", ref)
	}
	if _, err := os.Stat(filepath.Join("..", "..", "..", "plugins", "crw", "skills", "crw-interview", ref)); err != nil {
		t.Fatal(err)
	}
}
func TestMindsRejectInvalidFields(t *testing.T) {
	row := mindsValidRow()
	raw := []any{row, "not an object", map[string]any{"dimension": "nope"}, map[string]any{"dimension": "goal", "severity": "critical"}}
	got := I.NormalizeMindOutput(I.MindContrarian, raw)
	if len(got) != 1 || got[0].Dimension != I.DimensionGoal || got[0].Mind != I.MindContrarian {
		t.Fatalf("normalized=%v", got)
	}
}
func TestMindsMalformedTopLevel(t *testing.T) {
	for _, raw := range []any{nil, map[string]any{"not": "array"}, "[]"} {
		if got := I.NormalizeMindOutput(I.MindSocratic, raw); !reflect.DeepEqual(got, []I.MindContradiction{}) {
			t.Fatalf("malformed=%v", got)
		}
	}
	if got := I.NormalizeMindOutput("invalid", []any{mindsValidRow()}); !reflect.DeepEqual(got, []I.MindContradiction{}) {
		t.Fatalf("unknown mind=%v", got)
	}
}
func TestMindsExactCorrelation(t *testing.T) {
	for _, m := range I.Minds() {
		got := I.NormalizeMindOutput(m, []any{mindsValidRow()}, 4)
		if len(got) != 1 || got[0].Mind != m || got[0].CorrelationID != "4-"+string(m) {
			t.Fatalf("correlation=%v", got)
		}
	}
	got := I.NormalizeMindOutput(I.MindContrarian, []any{mindsValidRow()})
	if len(got) != 1 || got[0].CorrelationID != "0-contrarian" {
		t.Fatalf("default round=%v", got)
	}
}
func TestMindsStripSideEffectKeys(t *testing.T) {
	row := mindsValidRow()
	row["action"], row["question"], row["writeState"], row["plan"] = "edit", "ask?", true, "rewrite"
	got := I.NormalizeMindOutput(I.MindContrarian, []any{row}, 1)
	if len(got) != 1 {
		t.Fatalf("normalized=%v", got)
	}
	b, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"mind": "contrarian", "correlationId": "1-contrarian", "dimension": "goal", "contradiction": "gap", "severity": "high", "evidence": "plan.md:12"}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("fields=%v", fields)
	}
}
func TestMindsGroundedEvidence(t *testing.T) {
	raw := []any{}
	for _, e := range []string{"I think this is wrong", "src/x.ts:42", "see section Goals", `the spec says "must be max"`} {
		row := mindsValidRow()
		row["evidence"] = e
		raw = append(raw, row)
	}
	if got := I.NormalizeMindOutput(I.MindContrarian, raw); len(got) != 3 {
		t.Fatalf("grounded=%v", got)
	}
}
func TestMindsLowestDimensionsAndCap(t *testing.T) {
	tr := I.DefaultInterview(1)
	for _, d := range I.DimensionOrder() {
		tr.Dimensions.Score(d).Level = I.LevelMax
	}
	tr.Dimensions.Ontology.Level = I.LevelLow
	if got := I.SelectMinds(tr, 1); !reflect.DeepEqual(got, []I.Mind{I.MindOntologist}) {
		t.Fatalf("selected=%v", got)
	}
	if got := I.SelectMinds(tr, 99); len(got) != I.MindConcurrencyCap {
		t.Fatalf("cap=%v", got)
	}
	if got := I.SelectMinds(nil); len(got) != 2 {
		t.Fatalf("default=%v", got)
	}
}
func TestMindsRecordedOracle(t *testing.T) {
	cases := mindsLoadCases(t)
	for _, c := range cases.Normalization {
		t.Run(c.ID, func(t *testing.T) {
			round := c.Round
			switch round {
			case "negativeZero":
				round = math.Copysign(0, -1)
			case "nan":
				round = math.NaN()
			case "infinity":
				round = math.Inf(1)
			}
			got := I.NormalizeMindOutput(c.Mind, c.Raw, round)
			if !reflect.DeepEqual(got, c.Expected) {
				t.Fatalf("got %#v, oracle %#v", got, c.Expected)
			}
		})
	}
	for _, c := range cases.Selection {
		t.Run(c.ID, func(t *testing.T) {
			var got []I.Mind
			if c.Count == "default" {
				got = I.SelectMinds(c.Tracker)
			} else {
				n, _ := c.Count.(float64)
				switch c.Count {
				case "nan":
					n = math.NaN()
				case "infinity":
					n = math.Inf(1)
				case "negativeInfinity":
					n = math.Inf(-1)
				}
				got = I.SelectMinds(c.Tracker, n)
			}
			if !reflect.DeepEqual(got, c.Expected) {
				t.Fatalf("got %v, oracle %v", got, c.Expected)
			}
		})
	}
}

func TestMindsObjectCoercionFailure(t *testing.T) {
	for _, c := range mindsLoadCases(t).Failures {
		t.Run(c.ID, func(t *testing.T) {
			count, _ := c.Count.(float64)
			if c.Count == "nan" {
				count = math.NaN()
			}
			if c.Kind != "TypeError" {
				t.Fatalf("unexpected oracle failure kind %q", c.Kind)
			}
			defer func() {
				if got := recover(); got != c.Error {
					t.Errorf("coercion failure = %v, want oracle failure", got)
				}
			}()
			I.SelectMinds(c.Tracker, count)
		})
	}
}
