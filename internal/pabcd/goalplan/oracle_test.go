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

// compact is JSON.stringify(v): no HTML escaping, and U+2028 and U+2029 written as themselves, where encoding/json escapes them.
func compact(t *testing.T, v any) string {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	in, out := bytes.TrimSuffix(b.Bytes(), []byte("\n")), []byte{}
	// A backslash and the byte after it are copied together, so an escaped backslash before "u2028" stays text.
	for i := 0; i < len(in); i++ {
		switch {
		case in[i] != '\\':
			out = append(out, in[i])
		case bytes.HasPrefix(in[i:], []byte(`\u2028`)):
			out, i = append(out, "\u2028"...), i+5
		case bytes.HasPrefix(in[i:], []byte(`\u2029`)):
			out, i = append(out, "\u2029"...), i+5
		default:
			out, i = append(out, in[i], in[i+1]), i+1
		}
	}
	return string(out)
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
	// The one recorded case where this port departs from the oracle, by decision: a review finding of kind security (plan-file
	// paths that leave the working directory are hashed by the oracle's staleness check). It is tagged intentionally-changed.
	// The recording stays what the oracle answered; the replay expects the new answer, and a tag whose recorded answer already
	// equals the new one fails.
	changed := map[string]string{
		"files_path_absolute_and_dotdot_accepted": `{"reviewRounds":[{"roundId":"r1","purpose":"plan_audit","planPath":"devlog/_plan/260101_demo","planSha256":"ab12","status":"pending","lane":{"launchId":"r1-20260101000000"},"openedAt":"2026-01-01T00:00:00.000Z"}]}`,
	}
	for name := range changed {
		if _, ok := cases[name]; !ok {
			t.Fatalf("%s is tagged intentionally-changed but is not a recorded case", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cases)) {
		c := cases[name]
		t.Run(name, func(t *testing.T) {
			if c.Oracle == nil {
				t.Fatal("the oracle refused the plan; no recorded case expects that")
			}
			want := *c.Oracle
			if port, ok := changed[name]; ok {
				if port == want {
					t.Fatal("tagged intentionally-changed but the recorded answer is the new one")
				}
				want = port
			}
			got := compact(t, reviveBoth(c.Input))
			if got != want {
				t.Fatalf("revived\n%s\nwant\n%s", got, want)
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
