package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The frozen record schemas, validated by the repository's own jsonschema Draft7Validator run
// in an isolated Python, as test_schema_conformance.py does. No new Go module is involved.
var recordSchemas = []string{"relationship", "completion-receipt", "delivery-attempt", "acknowledgement", "verification-verdict"}

type schemaCase struct {
	Test     string          `json:"-"`
	Schema   string          `json:"schema"`
	Label    string          `json:"label"`
	Instance json.RawMessage `json:"instance"`
	Valid    bool            `json:"valid"`
}

type schemaVerdict struct {
	Label  string   `json:"label"`
	Errors []string `json:"errors"`
}

const draft7Script = `
import json, sys
from pathlib import Path
from jsonschema import Draft7Validator
directory = Path(sys.argv[1])
schemas = {}
for name in sys.argv[2].split(","):
    schema = json.loads((directory / (name + ".json")).read_text())
    Draft7Validator.check_schema(schema)
    schemas[name] = Draft7Validator(schema)
verdicts = []
for case in json.load(sys.stdin):
    errors = sorted(schemas[case["schema"]].iter_errors(case["instance"]), key=lambda e: list(e.path))
    verdicts.append({"label": case["label"], "errors": [f"{list(e.path)}: {e.message}" for e in errors]})
print(json.dumps(verdicts))
`

// validateAgainstSchemas grades every case in one Python run. A case fails when its outcome
// differs from what it declares: a positive with errors, or a negative the validator accepted.
// It returns the failures per Python test name and how many positives each schema validated.
func validateAgainstSchemas(t *testing.T, cases []schemaCase) (map[string][]string, map[string]int) {
	t.Helper()
	input, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	stdin := filepath.Join(t.TempDir(), "cases.json")
	if err := os.WriteFile(stdin, input, 0o600); err != nil {
		t.Fatal(err)
	}
	script := "import sys; sys.stdin = open(" + pythonLiteral(stdin) + ")\n" + draft7Script
	out := pythonStoreValue(t, script, filepath.Join(repositoryRoot(t), "contract", "schema"), strings.Join(recordSchemas, ","))
	var verdicts []schemaVerdict
	if err := json.Unmarshal([]byte(out), &verdicts); err != nil || len(verdicts) != len(cases) {
		t.Fatalf("validator output %q: %v", out, err)
	}
	failures, counts := map[string][]string{}, map[string]int{}
	for i, verdict := range verdicts {
		c := cases[i]
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

func pythonLiteral(text string) string {
	encoded, _ := json.Marshal(text)
	return string(encoded)
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
