package state

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// testdata/oracle-restore.json holds what the CXC v0.2.40 oracle's readStateStrict answered for each persisted text, recorded
// once by testdata/record-oracle.mjs under Node 24 (no Node runs here): whether it called the file unreadable and the state it
// rebuilt, as JSON. Restore must agree on both. Written and read again, a state must give the oracle's second answer too, which
// is the state itself except for the D-close markers that lose their legacy flag ("again").
func TestRestoreMatchesTheRecordedOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/oracle-restore.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		ID         string
		Raw        string
		Unreadable bool
		State      json.RawMessage
		Again      json.RawMessage
	}
	if err := json.Unmarshal(data, &cases); err != nil || len(cases) < 100 {
		t.Fatalf("%d recorded cases, %v", len(cases), err)
	}
	// dcloseLegacy names the recorded cases where this port departs from the oracle, by decision (CRW-648, a loss of state fixed
	// under the parity rule revision of 2026-10-03): the port reads back the legacy flag it persists, so a refused D-close marker
	// keeps its distinction across a write where the oracle drops the key it never reads. "flag" is a case whose recorded second
	// answer lost the flag (the port's second read now equals its first); "successor" is a stored legacy true over a successor,
	// which the port keeps and clears to null. The recording stays what the oracle answered; the replay expects the new answer,
	// and a tag whose recorded answer already equals the new one fails.
	dcloseLegacy := map[string]string{
		"marker_absent_next": "flag", "marker_number_next": "flag", "marker_empty_next": "flag",
		"marker_array_next": "flag", "marker_object_next": "flag", "marker_legacy_input": "successor",
	}
	seen := map[string]bool{}
	for _, c := range cases {
		seen[c.ID] = true
		s, unreadable := restore("rec-s1", []byte(c.Raw), at())
		got, want := compactEncoding(t, c.ID, s), compactJSON(t, c.ID, c.State)
		wantAgain := want
		if c.Again != nil {
			wantAgain = compactJSON(t, c.ID, c.Again)
		}
		switch dcloseLegacy[c.ID] {
		case "flag": // the flag survives the write, so the second answer is the first
			if wantAgain == want {
				t.Errorf("%s is tagged intentionally-changed but the recorded second answer already equals the first", c.ID)
			}
			wantAgain = want
		case "successor": // the stored flag is kept and the successor cleared
			replaced := strings.Replace(want, `"nextWorkPhaseId":"wp2"`, `"nextWorkPhaseId":null,"legacy":true`, 1)
			if replaced == want {
				t.Errorf("%s is tagged intentionally-changed but the recorded answer holds no successor", c.ID)
			}
			want, wantAgain = replaced, replaced
		}
		if unreadable != c.Unreadable || got != want {
			t.Errorf("%s: unreadable %v (oracle %v)\n got %s\nwant %s", c.ID, unreadable, c.Unreadable, got, want)
		}
		// written and read again: the oracle's second answer is the state itself, or the recorded "again"
		enc, _ := Encode(s)
		again, unreadableAgain := restore("rec-s1", enc, at())
		if got := compactEncoding(t, c.ID, again); unreadableAgain || got != wantAgain {
			t.Errorf("%s: second pass\n got %s\nwant %s", c.ID, got, wantAgain)
		}
	}
	for id := range dcloseLegacy {
		if !seen[id] {
			t.Errorf("%s is tagged intentionally-changed but is not a recorded case", id)
		}
	}
}

func compactEncoding(t *testing.T, id string, s State) string {
	t.Helper()
	enc, err := Encode(s)
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	return compactJSON(t, id, enc)
}

func compactJSON(t *testing.T, id string, raw []byte) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	return b.String()
}
