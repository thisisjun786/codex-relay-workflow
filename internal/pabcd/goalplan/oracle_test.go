package goalplan

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The oracle's answers for the cases of testdata/oracle-revive.json were recorded once with testdata/record-oracle.mjs (CXC
// v0.2.40 under Node 24); no Node runs here. A case holds the raw reviewRounds and finalGate a plan file carried and the compact
// JSON the oracle's readGoalplan revived them to, key order included.

type oracleCase struct {
	Input  map[string]any `json:"input"`
	Oracle *string        `json:"oracle"` // null when readGoalplan refused the plan
}

// revived is the part of a plan the recorded answers hold.
type revived struct {
	ReviewRounds []ReviewRoundState `json:"reviewRounds,omitzero"`
	FinalGate    *FinalGateState    `json:"finalGate,omitempty"`
}

// compact is JSON.stringify(v): no HTML escaping. The recorded cases hold no U+2028 or U+2029, which encoding/json escapes.
func compact(t *testing.T, v any) string {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return string(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
}

func reviveBoth(input map[string]any) revived {
	return revived{reviveReviewRounds(input["reviewRounds"]), reviveFinalGate(input["finalGate"])}
}

func TestOracleParity(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "oracle-revive.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string]oracleCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 100 {
		t.Fatalf("%d recorded cases", len(cases))
	}
	for _, name := range slices.Sorted(maps.Keys(cases)) {
		c := cases[name]
		t.Run(name, func(t *testing.T) {
			if c.Oracle == nil {
				t.Fatal("the oracle refused the plan; no recorded case expects that")
			}
			got := compact(t, reviveBoth(c.Input))
			if got != *c.Oracle {
				t.Fatalf("revived\n%s\noracle\n%s", got, *c.Oracle)
			}
			// What a revived plan holds survives being written and read back: reviving it again changes nothing.
			var again map[string]any
			dec := json.NewDecoder(strings.NewReader(got))
			dec.UseNumber() // as a plan reader decodes (restore.go)
			if err := dec.Decode(&again); err != nil {
				t.Fatal(err)
			}
			if second := compact(t, reviveBoth(again)); second != got {
				t.Fatalf("reviving the revived record again gave\n%s\nwant\n%s", second, got)
			}
		})
	}
}
