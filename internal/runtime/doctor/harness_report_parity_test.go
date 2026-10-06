package doctor

// This file is the CRW-640 parity test for two doctor harness report defects found while
// reviewing the report core port (CRW-346, PR #572): the stderr cut loses a lone high
// surrogate, and an explicit empty repair cannot be told from an absent one. It is written
// red first against the baseline and green after the fix.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// harnessReportCutStderr is the issue's reproduction: "x" repeated 159 times, an astral
// character (U+1F600, a UTF-16 surrogate pair) and "y". Trimmed and cut at 160 UTF-16 units,
// the cut falls between the pair's two halves, so the oracle keeps the lone high surrogate.
func harnessReportCutStderr() string {
	return strings.Repeat("x", 159) + "\U0001F600" + "y"
}

// TestHarnessReportParityCutKeepsLoneHighSurrogate is defect 1: harnessReportCut must keep the
// kept prefix's lone high surrogate as its WTF-8 bytes (the representation pyjson reads), the
// JSON output must write it as the escape JSON.stringify writes, and the text output must keep
// U+FFFD. On the baseline the cut appends U+FFFD instead, so the JSON carries the U+FFFD bytes.
func TestHarnessReportParityCutKeepsLoneHighSurrogate(t *testing.T) {
	check := HarnessFeaturesCheck(HarnessRun{Status: harnessReportStatus(3), Stderr: harnessReportCutStderr()})

	// The evidence ends in 159 x followed by the lone high surrogate's three WTF-8 bytes.
	wantTail := strings.Repeat("x", 159) + "\xed\xa0\xbd"
	if !strings.HasSuffix(check.Evidence, wantTail) {
		t.Fatalf("evidence tail = % x\nwant suffix % x", []byte(check.Evidence), []byte(wantTail))
	}

	// The JSON output writes the escape JSON.stringify writes for a lone high surrogate.
	report := HarnessReport{SchemaVersion: HarnessSchemaVersion, Overall: HarnessWarn, Checks: []HarnessCheck{check}}
	var buf bytes.Buffer
	if err := harnessRunWriteJSON(&buf, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "\\ud83d") {
		t.Fatalf("JSON output does not carry the \\ud83d escape:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "\xef\xbf\xbd") {
		t.Fatalf("JSON output carries the U+FFFD bytes instead of the escape")
	}

	// The text output keeps U+FFFD, what the UTF-8 encoder writes for the lone surrogate.
	text := RenderHarnessReport(report)
	if !strings.Contains(text, strings.Repeat("x", 159)+"\uFFFD") {
		t.Fatalf("text does not carry the U+FFFD the encoder writes")
	}
	if strings.Contains(text, "\xed\xa0\xbd") {
		t.Fatalf("text carries the WTF-8 surrogate instead of U+FFFD")
	}
}

// TestHarnessReportParityEmptyRepairRoundTrips is defect 2: a check whose repair is an explicit
// empty string must keep the "repair" key (the oracle keeps a present-but-empty value), while a
// check with no repair omits it. On the baseline both are the same string, so the key is always
// dropped and the distinction cannot be expressed at all.
func TestHarnessReportParityEmptyRepairRoundTrips(t *testing.T) {
	empty := ""
	present := HarnessCheck{Name: "x", Severity: HarnessWarn, Evidence: "e", Repair: &empty}
	raw, err := json.Marshal(present)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\"repair\":\"\"") {
		t.Fatalf("explicit empty repair dropped from JSON: %s", raw)
	}
	absent := HarnessCheck{Name: "x", Severity: HarnessWarn, Evidence: "e"}
	raw, err = json.Marshal(absent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "repair") {
		t.Fatalf("absent repair kept in JSON: %s", raw)
	}
}
