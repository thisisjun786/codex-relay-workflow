package skill

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The estimate signal and the calibrate command (CRW-739): the check reads a line estimate the body
// states and scales it by the measured actual/estimate ratios from the calibration table; calibrate
// prints that table's ratio distribution. The expected ratios were computed independently with exact
// fractions (median 18961/9870, p75 8509/4000).

const (
	wantSizeEstimateCalibrationRows = 8
	wantSizeEstimateCalibrationP50  = 1.9210739614994934 // 18961/9870
	wantSizeEstimateCalibrationP75  = 2.12725            // 8509/4000
)

// sizeEstimateInput is a description body plus optional extra input fields.
func sizeEstimateInput(body string, extra map[string]any) string {
	m := map[string]any{"id": "CRW-SYN", "title": "synthetic", "description": body}
	for k, v := range extra {
		m[k] = v
	}
	raw, _ := json.Marshal(m)
	return string(raw)
}

// sizeEstimateCheck runs the check on a body and returns the exit code, the decoded report and the raw
// output.
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

// sizeEstimateNumber reads one number out of the report's estimate object.
func sizeEstimateNumber(t testing.TB, m map[string]any, key string) float64 {
	t.Helper()
	f, ok := at(m, "estimate", key).(float64)
	if !ok {
		t.Fatalf("estimate.%s is not a number in %v", key, at(m, "estimate"))
	}
	return f
}

// sizeEstimateTableFile writes a table built from rows and answers its path.
func sizeEstimateTableFile(t testing.TB, rows []map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"schema": "crw-issue-size-calibration/1", "rows": rows})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "table.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// sizeEstimateReportNumber reads one number out of the report's top level (the calibrate report's ratios).
func sizeEstimateReportNumber(t testing.TB, m map[string]any, key string) float64 {
	t.Helper()
	f, ok := at(m, key).(float64)
	if !ok {
		t.Fatalf("%s is not a number in %v", key, m)
	}
	return f
}

func TestIssueSizeEstimateForms(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
		want int
	}{
		{"추정 N줄", "추정 300줄", 300},
		{"약 N줄", "약 500줄", 500},
		{"range takes the upper bound", "450~580줄", 580},
		{"estimate N lines", "estimate 400 lines", 400},
		{"about N lines", "about 350 lines", 350},
		{"the largest value wins", "약 200줄, 약 450줄", 450},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, r, out := sizeEstimateCheck(t, sizeEstimateBody(test.text), nil)
			if got := atNum(t, r, "estimate", "stated"); got != test.want {
				t.Errorf("%q: stated %d, want %d: %s", test.text, got, test.want, out)
			}
		})
	}
}

// An estimate written in a section the check does not read (the completion criteria) is not read.
func TestIssueSizeEstimateIgnoresTheCriteriaSection(t *testing.T) {
	body := "## 완료 기준\n1. 약 450줄\n2. b\n"
	_, r, out := sizeEstimateCheck(t, body, nil)
	if at(r, "estimate") != nil {
		t.Errorf("an estimate was read from the criteria section: %s", out)
	}
}

func TestIssueSizeEstimateScalesOverTheCeiling(t *testing.T) {
	code, r, out := sizeEstimateCheck(t, sizeEstimateBody("약 450줄"), nil)
	if code != 1 {
		t.Errorf("exit %d, want 1 (split_recommended): %s", code, out)
	}
	if got := at(r, "decision"); got != "split_recommended" {
		t.Errorf("decision %v: %s", got, out)
	}
	if got := at(r, "assignable"); got != false {
		t.Errorf("assignable %v", got)
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
	if got := atNum(t, r, "estimate", "stated"); got != 450 {
		t.Errorf("stated %d", got)
	}
	if got := atNum(t, r, "estimate", "ceiling"); got != 600 {
		t.Errorf("ceiling %d", got)
	}
	if got := atNum(t, r, "estimate", "table_rows"); got != wantSizeEstimateCalibrationRows {
		t.Errorf("table_rows %d", got)
	}
	if got := sizeEstimateNumber(t, r, "p75_ratio"); got != wantSizeEstimateCalibrationP75 {
		t.Errorf("p75_ratio %v, want %v", got, wantSizeEstimateCalibrationP75)
	}
	if got := sizeEstimateNumber(t, r, "p50_ratio"); got != wantSizeEstimateCalibrationP50 {
		t.Errorf("p50_ratio %v, want %v", got, wantSizeEstimateCalibrationP50)
	}
	if got := sizeEstimateNumber(t, r, "scaled_p75"); got != 450*wantSizeEstimateCalibrationP75 {
		t.Errorf("scaled_p75 %v, want %v", got, 450*wantSizeEstimateCalibrationP75)
	}
}

// A ceiling the input states replaces the 600 default: an estimate that scales under it keeps the
// ok decision but still reports the estimate object.
func TestIssueSizeEstimateCeilingFromInput(t *testing.T) {
	code, r, out := sizeEstimateCheck(t, sizeEstimateBody("약 450줄"), map[string]any{"size_ceiling": 2000})
	if code != 0 {
		t.Errorf("exit %d, want 0: %s", code, out)
	}
	if got := at(r, "decision"); got != "ok" {
		t.Errorf("decision %v", got)
	}
	if got := atNum(t, r, "estimate", "ceiling"); got != 2000 {
		t.Errorf("ceiling %d", got)
	}
	if got := atNum(t, r, "estimate", "stated"); got != 450 {
		t.Errorf("stated %d", got)
	}
}

// A negative ceiling is refused as unreadable input.
func TestIssueSizeEstimateRefusesANegativeCeiling(t *testing.T) {
	code, _, errOut := sizeCall(sizeEstimateInput(sizeEstimateBody("약 450줄"), map[string]any{"size_ceiling": -1}))
	if code != 2 || !strings.Contains(errOut, "size_ceiling") {
		t.Errorf("exit %d, stderr %q", code, errOut)
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

// A user-approved exception makes the scaled-over-ceiling reason assignable again.
func TestIssueSizeEstimateException(t *testing.T) {
	exception := map[string]any{"issue": "CRW-SYN", "approved_by": "Reviewer", "approved_on": "2026-10-06", "statement": "Keep it as one issue."}
	code, r, out := sizeEstimateCheck(t, sizeEstimateBody("약 450줄"), map[string]any{"exception": exception})
	if code != 0 {
		t.Errorf("exit %d, want 0: %s", code, out)
	}
	if got := at(r, "assignable"); got != true {
		t.Errorf("assignable %v", got)
	}
	if got := at(r, "decision"); got != "split_recommended" {
		t.Errorf("decision %v", got)
	}
	if at(r, "exception_record") == nil {
		t.Errorf("no exception_record: %s", out)
	}
}

// calibrate prints the table's ratio distribution.
func TestIssueSizeCalibrateOutput(t *testing.T) {
	code, out, errOut := call([]string{"issue-size", "calibrate"}, "")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	r := decodeReport(t, out)
	if got := at(r, "schema"); got != "crw-issue-size-calibrate/1" {
		t.Errorf("schema %v", got)
	}
	if got := atNum(t, r, "table_rows"); got != wantSizeEstimateCalibrationRows {
		t.Errorf("table_rows %d", got)
	}
	if got := sizeEstimateReportNumber(t, r, "p75_ratio"); got != wantSizeEstimateCalibrationP75 {
		t.Errorf("p75_ratio %v, want %v", got, wantSizeEstimateCalibrationP75)
	}
	if got := sizeEstimateReportNumber(t, r, "p50_ratio"); got != wantSizeEstimateCalibrationP50 {
		t.Errorf("p50_ratio %v, want %v", got, wantSizeEstimateCalibrationP50)
	}
	if got := len(atList(r, "ratios")); got != wantSizeEstimateCalibrationRows {
		t.Errorf("%d ratio rows", got)
	}
}

// calibrate --table reads a table the caller names.
func TestIssueSizeCalibrateFromAFile(t *testing.T) {
	path := sizeEstimateTableFile(t, []map[string]any{
		{"issue": "A", "estimate_low": 100, "estimate_high": 100, "actual_impl": 150, "actual_test": 50},
		{"issue": "B", "estimate_low": nil, "estimate_high": 200, "actual_impl": 100, "actual_test": 100},
	})
	code, out, errOut := call([]string{"issue-size", "calibrate", "--table", path}, "")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d: %s%s", code, out, errOut)
	}
	r := decodeReport(t, out)
	if got := atNum(t, r, "table_rows"); got != 2 {
		t.Errorf("table_rows %d", got)
	}
	// ratios 2.0 and 1.0; the median is 1.5.
	if got := sizeEstimateReportNumber(t, r, "p50_ratio"); got != 1.5 {
		t.Errorf("p50_ratio %v, want 1.5", got)
	}
}

func TestIssueSizeCalibrateIsDeterministic(t *testing.T) {
	code1, out1, _ := call([]string{"issue-size", "calibrate"}, "")
	code2, out2, _ := call([]string{"issue-size", "calibrate"}, "")
	if code1 != 0 || out1 == "" || out1 != out2 {
		t.Errorf("two runs differ: exit %d/%d\n%s\n---\n%s", code1, code2, out1, out2)
	}
}

// A table whose rows are missing cells is refused rather than read as zero.
func TestIssueSizeCalibrateRefusesAnIncompleteTable(t *testing.T) {
	path := sizeEstimateTableFile(t, []map[string]any{
		{"issue": "A", "estimate_low": 100, "estimate_high": 100, "actual_impl": 150, "actual_test": nil},
	})
	code, _, errOut := call([]string{"issue-size", "calibrate", "--table", path}, "")
	if code != 2 || errOut == "" {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}
