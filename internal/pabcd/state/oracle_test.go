package state

import (
	"bytes"
	"encoding/json"
	"os"
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
	for _, c := range cases {
		s, unreadable := restore("rec-s1", []byte(c.Raw), at())
		if got, want := compact(t, c.ID, s), compactJSON(t, c.ID, c.State); unreadable != c.Unreadable || got != want {
			t.Errorf("%s: unreadable %v (oracle %v)\n got %s\nwant %s", c.ID, unreadable, c.Unreadable, got, want)
		}
		// written and read again: the oracle's second answer is the state itself, or the recorded "again"
		enc, _ := Encode(s)
		again, unreadableAgain := restore("rec-s1", enc, at())
		want := compactJSON(t, c.ID, c.State)
		if c.Again != nil {
			want = compactJSON(t, c.ID, c.Again)
		}
		if got := compact(t, c.ID, again); unreadableAgain || got != want {
			t.Errorf("%s: second pass\n got %s\nwant %s", c.ID, got, want)
		}
	}
}

func compact(t *testing.T, id string, s State) string {
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
