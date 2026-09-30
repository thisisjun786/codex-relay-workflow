package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The frozen record schemas, as the repository's own jsonschema Draft7Validator judged each case
// in an isolated Python, as test_schema_conformance.py does (draft7Verdict). No new Go module is
// involved.
var recordSchemas = []string{"relationship", "completion-receipt", "delivery-attempt", "acknowledgement", "verification-verdict"}

type schemaCase struct {
	Test     string          `json:"-"`
	Schema   string          `json:"schema"`
	Label    string          `json:"label"`
	Instance json.RawMessage `json:"instance"`
	Valid    bool            `json:"valid"`
}

// draft7Verdict is what the repository's jsonschema Draft7Validator, run in the retired Python
// implementation (test_schema_conformance.py's validator), answered for one case: the fixture
// draft7-verdicts.json.gz holds one per case, in the order the test builds them, with the
// instance it judged as normalInstance spells it. No Go validator of Draft 7 is in the module, so the verdicts are frozen: a case
// whose instance changes in substance fails until the new instance is judged and its verdict
// written to the fixture.
type draft7Verdict struct {
	Schema   string   `json:"schema"`
	Label    string   `json:"label"`
	Instance string   `json:"instance"`
	Errors   []string `json:"errors"`
}

var (
	// judgedTemporary is a temporary directory an instance names: this run's, or the one the
	// fixture's Python run named, <TMP:name>/NNN. The validator judged a path's form only.
	judgedTemporary = regexp.MustCompile(regexp.QuoteMeta(filepath.Clean(os.TempDir())) + `/[^/"\\]+/[0-9]+|<TMP:[^>]*>/[0-9]+`)
)

// normalInstance is an instance as the verdict fixture keeps it: its JSON with sorted keys, every
// temporary directory <TMP>, and every value a rerun changes (a digest, a tempfile name, a clock
// time) spelled by its kind and length alone (maskNoise), since the validator judged their form.
func normalInstance(t *testing.T, instance json.RawMessage) string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(instance))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("instance %s: %v", instance, err)
	}
	encoded, err := golden.Encode(value)
	if err != nil {
		t.Fatal(err)
	}
	return maskNoise(judgedTemporary.ReplaceAllString(string(encoded), "<TMP>"))
}

// validateAgainstSchemas grades every case with the validator's verdict (the fixture). A case
// fails when its outcome differs from what it declares: a positive with errors, or a negative the
// validator accepted. It returns the failures per Python test name and how many positives each
// schema validated.
func validateAgainstSchemas(t *testing.T, cases []schemaCase) (map[string][]string, map[string]int) {
	t.Helper()
	var fixture []draft7Verdict
	readFixture(t, "draft7-verdicts.json", &fixture)
	if len(fixture) != len(cases) {
		t.Fatalf("the verdict fixture holds %d verdicts for %d cases: judge the cases anew", len(fixture), len(cases))
	}
	failures, counts := map[string][]string{}, map[string]int{}
	for i, c := range cases {
		verdict := fixture[i]
		if instance := normalInstance(t, c.Instance); verdict.Schema != c.Schema || verdict.Label != c.Label || instance != verdict.Instance {
			t.Errorf("case %d, the %s case %q, is not the instance the validator judged (%s %q); judge it anew\njudged:\n%s\nnow:\n%s", i, c.Schema, c.Label, verdict.Schema, verdict.Label, verdict.Instance, instance)
			continue
		}
		switch {
		case c.Valid && len(verdict.Errors) > 0:
			failures[c.Test] = append(failures[c.Test], fmt.Sprintf("%s should satisfy %s: %v", c.Label, c.Schema, verdict.Errors))
		case !c.Valid && len(verdict.Errors) == 0:
			failures[c.Test] = append(failures[c.Test], c.Label+" must be rejected by "+c.Schema+"; a validator that accepts it proves nothing")
		case c.Valid:
			counts[c.Schema]++
		}
	}
	return failures, counts
}

// mutated returns a deep copy of a JSON record with one change applied to its object form.
func mutated(t *testing.T, record string, change func(map[string]any)) json.RawMessage {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal([]byte(record), &fields); err != nil {
		t.Fatal(err)
	}
	change(fields)
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
