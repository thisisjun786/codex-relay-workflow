package skill

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The estimate signal and the calibrate command (CRW-739). The expected ratios were computed
// independently with exact fractions (median 18961/9870, p75 8509/4000).

const (
	wantSizeEstimateRows = 8
	wantSizeEstimateP50  = 1.9210739614994934 // 18961/9870
	wantSizeEstimateP75  = 2.12725            // 8509/4000
)

func sizeEstimateInput(body string, extra map[string]any) string {
	m := map[string]any{"id": "CRW-SYN", "title": "synthetic", "description": body}
	for k, v := range extra {
		m[k] = v
	}
	raw, _ := json.Marshal(m)
	return string(raw)
}

func sizeEstimateCheck(t testing.TB, body string, extra map[string]any) (int, map[string]any, string) {
	t.Helper()
	code, out, errOut := sizeCall(sizeEstimateInput(body, extra))
	if errOut != "" {
		t.Fatalf("stderr: %s", errOut)
	}
	return code, decodeReport(t, out), out
}

// sizeEstimateBody is two completion criteria plus a size section holding text.
func sizeEstimateBody(text string) string {
	return "## 완료 기준\n1. a\n2. b\n\n## 예상 크기\n" + text + "\n"
}

func sizeEstimateNumber(t testing.TB, m map[string]any, key string) float64 {
	t.Helper()
	f, ok := at(m, "estimate", key).(float64)
	if !ok {
		t.Fatalf("estimate.%s is not a number in %v", key, at(m, "estimate"))
	}
	return f
}

// sizeEstimateTableFile writes a table with the given schema and answers its path.
func sizeEstimateTableFile(t testing.TB, schema string, rows []map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"schema": schema, "rows": rows})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "table.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIssueSizeEstimateForms(t *testing.T) {
	for _, test := range []struct {
		text string
		want int
	}{
		{"추정 300줄", 300},
		{"약 500줄", 500},
		{"450~580줄", 580},
		{"estimate 400 lines", 400},
		{"about 350 lines", 350},
		{"약 200줄, 약 450줄", 450},
		{"600 미만", 600},
	} {
		t.Run(test.text, func(t *testing.T) {
			_, r, out := sizeEstimateCheck(t, sizeEstimateBody(test.text), nil)
			if got := atNum(t, r, "estimate", "stated"); got != test.want {
				t.Errorf("%q: stated %d, want %d: %s", test.text, got, test.want, out)
			}
		})
	}
}

// An estimate stated as prose in the deliverables section is read too (the section keeps its full
// text, not only its list items), and one under a section the check does not read is not.
func TestIssueSizeEstimateReadsTheRightSections(t *testing.T) {
	if _, r, out := sizeEstimateCheck(t, "## 완료 기준\n1. a\n2. b\n\n## 산출물\n약 450줄\n", nil); atNum(t, r, "estimate", "stated") != 450 {
		t.Errorf("deliverables prose: estimate %v: %s", at(r, "estimate"), out)
	}
	if _, r, out := sizeEstimateCheck(t, "## 완료 기준\n1. 약 450줄\n2. b\n", nil); at(r, "estimate") != nil {
		t.Errorf("an estimate was read from the criteria section: %s", out)
	}
}

func TestIssueSizeEstimateScalesOverTheCeiling(t *testing.T) {
	code, r, out := sizeEstimateCheck(t, sizeEstimateBody("약 450줄"), nil)
	if code != 0 || at(r, "decision") != "over_line" {
		t.Errorf("exit %d, decision %v: %s", code, at(r, "decision"), out)
	}
	found := false
	for _, reason := range atList(r, "reasons") {
		if strings.Contains(fmt.Sprint(reason), "estimate_scaled_over_ceiling") {
			found = true
		}
	}
	if !found {
		t.Errorf("no estimate_scaled_over_ceiling reason: %s", out)
	}
	for key, want := range map[string]float64{"stated": 450, "ceiling": 600, "table_rows": wantSizeEstimateRows, "p50_ratio": wantSizeEstimateP50, "p75_ratio": wantSizeEstimateP75, "scaled_p75": 450 * wantSizeEstimateP75} {
		if got := sizeEstimateNumber(t, r, key); got != want {
			t.Errorf("estimate.%s %v, want %v", key, got, want)
		}
	}
}

// A stated ceiling replaces the default; an explicit zero is used as stated; a negative one is
// refused as unreadable input.
func TestIssueSizeEstimateCeiling(t *testing.T) {
	for _, test := range []struct {
		name    string
		ceiling any
		exit    int
		want    int
	}{
		{"stated", 2000, 0, 2000},
		{"zero is used as stated", 0, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, r, out := sizeEstimateCheck(t, sizeEstimateBody("약 450줄"), map[string]any{"size_ceiling": test.ceiling})
			if code != test.exit || atNum(t, r, "estimate", "ceiling") != test.want {
				t.Errorf("exit %d, ceiling %v, want exit %d ceiling %d: %s", code, at(r, "estimate", "ceiling"), test.exit, test.want, out)
			}
		})
	}
	if code, _, errOut := sizeCall(sizeEstimateInput(sizeEstimateBody("약 450줄"), map[string]any{"size_ceiling": -1})); code != 2 || !strings.Contains(errOut, "size_ceiling") {
		t.Errorf("negative ceiling: exit %d, stderr %q", code, errOut)
	}
}

// An estimate large enough that a naive int64 product would overflow is still compared exactly.
func TestIssueSizeEstimateLargeEstimateDoesNotOverflow(t *testing.T) {
	if code, r, out := sizeEstimateCheck(t, sizeEstimateBody("약 2000000000000000줄"), nil); code != 0 || at(r, "decision") != "over_line" {
		t.Errorf("exit %d, decision %v: %s", code, at(r, "decision"), out)
	}
}

// A body with no estimate is answered exactly as before: no estimate object and no new reason.
func TestIssueSizeNoEstimateIsUnchanged(t *testing.T) {
	code, r, out := sizeEstimateCheck(t, sizeEstimateBody("two files"), nil)
	if code != 0 || at(r, "decision") != "ok" {
		t.Errorf("exit %d, decision %v: %s", code, at(r, "decision"), out)
	}
	if strings.Contains(out, "estimate") {
		t.Errorf("a report with no estimate mentions one: %s", out)
	}
}

// A user-approved exception is recorded beside the scaled-over-ceiling reason; the answer stays
// advisory and the decision does not change.
func TestIssueSizeEstimateException(t *testing.T) {
	exception := map[string]any{"issue": "CRW-SYN", "approved_by": "Reviewer", "approved_on": "2026-10-06", "statement": "Keep it as one issue."}
	code, r, out := sizeEstimateCheck(t, sizeEstimateBody("약 450줄"), map[string]any{"exception": exception})
	if code != 0 || at(r, "decision") != "over_line" || at(r, "exception_record") == nil {
		t.Errorf("exit %d, decision %v: %s", code, at(r, "decision"), out)
	}
}

func TestIssueSizeCalibrateOutput(t *testing.T) {
	code, out, errOut := call([]string{"issue-size", "calibrate"}, "")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	r := decodeReport(t, out)
	if at(r, "schema") != "crw-issue-size-calibrate/1" || atNum(t, r, "table_rows") != wantSizeEstimateRows || len(atList(r, "ratios")) != wantSizeEstimateRows {
		t.Errorf("schema %v, table_rows %v, %d ratio rows", at(r, "schema"), at(r, "table_rows"), len(atList(r, "ratios")))
	}
	for key, want := range map[string]float64{"p50_ratio": wantSizeEstimateP50, "p75_ratio": wantSizeEstimateP75} {
		if got, _ := at(r, key).(float64); got != want {
			t.Errorf("%s %v, want %v", key, got, want)
		}
	}
}

// calibrate --table reads a table the caller names; a table that is not this schema, is missing a
// cell, or states a negative actual count is refused rather than read as zero or a negative ratio.
func TestIssueSizeCalibrateTables(t *testing.T) {
	good := []map[string]any{
		{"issue": "A", "estimate_low": 100, "estimate_high": 100, "actual_impl": 150, "actual_test": 50},
		{"issue": "B", "estimate_low": nil, "estimate_high": 200, "actual_impl": 100, "actual_test": 100},
	}
	code, out, errOut := call([]string{"issue-size", "calibrate", "--table", sizeEstimateTableFile(t, "crw-issue-size-calibration/1", good)}, "")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	if r := decodeReport(t, out); atNum(t, r, "table_rows") != 2 || at(r, "p50_ratio") != 1.5 { // ratios 2.0 and 1.0
		t.Errorf("table_rows %v, p50_ratio %v", at(r, "table_rows"), at(r, "p50_ratio"))
	}
	for _, test := range []struct {
		name   string
		schema string
		rows   []map[string]any
	}{
		{"foreign schema", "crw-issue-size-calibration/2", good},
		{"missing cell", "crw-issue-size-calibration/1", []map[string]any{{"issue": "A", "estimate_low": 100, "estimate_high": 100, "actual_impl": 150, "actual_test": nil}}},
		{"negative actual", "crw-issue-size-calibration/1", []map[string]any{{"issue": "A", "estimate_low": 100, "estimate_high": 100, "actual_impl": -150, "actual_test": 0}}},
		{"actual counts overflow", "crw-issue-size-calibration/1", []map[string]any{{"issue": "A", "estimate_low": 100, "estimate_high": 100, "actual_impl": 9223372036854775807, "actual_test": 1}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if code, _, errOut := call([]string{"issue-size", "calibrate", "--table", sizeEstimateTableFile(t, test.schema, test.rows)}, ""); code != 2 || errOut == "" {
				t.Errorf("exit %d, stderr %q", code, errOut)
			}
		})
	}
}

func TestIssueSizeCalibrateIsDeterministic(t *testing.T) {
	code1, out1, _ := call([]string{"issue-size", "calibrate"}, "")
	code2, out2, _ := call([]string{"issue-size", "calibrate"}, "")
	if code1 != 0 || out1 == "" || out1 != out2 {
		t.Errorf("two runs differ: exit %d/%d\n%s\n---\n%s", code1, code2, out1, out2)
	}
}

// The shipped table's eight rows are the issue's measured nodes: each row's actual is the issue
// body's recorded actual (implementation plus test lines). This pins the data, so a later edit that
// mistypes a row is caught even though the ratio distribution is recomputed from the table.
func TestIssueSizeCalibrationRowsMatchTheRecordedActuals(t *testing.T) {
	want := map[string]int64{
		"CRW-624": 1473, "CRW-369": 1082, "CRW-376": 924, "CRW-378": 788,
		"CRW-382": 784, "CRW-664": 1269, "CRW-685": 960, "CRW-716": 840,
	}
	rows, err := sizeEstimateReadCalibration(sizeEstimateCalibration)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(want) {
		t.Fatalf("%d rows, want %d", len(rows), len(want))
	}
	for _, row := range rows {
		recorded, ok := want[row.issue]
		if !ok {
			t.Errorf("unexpected row %q", row.issue)
			continue
		}
		if row.actual != recorded {
			t.Errorf("%s actual %d, recorded %d", row.issue, row.actual, recorded)
		}
	}
}
