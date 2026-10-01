package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The frozen record schemas, as the repository's own jsonschema Draft7Validator judged each case
// in an isolated Python, as test_schema_conformance.py does (draft7Verdict). No new Go module is
// involved.
var recordSchemas = []string{"relationship", "completion-receipt", "delivery-attempt", "acknowledgement", "verification-verdict"}

// schemaPins is the sha256 of the raw bytes of each schema in contract/schema as the Draft 7
// validator judged it. The verdicts in draft7-verdicts.json.gz are answers for exactly these
// bytes, and no Go validator can judge the cases again, so a schema whose bytes differ fails the
// test (schemaPinProblems) until the cases for it are re-judged. Change a pin only together with
// the verdicts of the cases it covers, never alone; the pin exists so that review has to ask
// whether the cases were judged again.
//
// The pins are the bytes of the schemas as first imported (commit 9bbb3cb4) and unchanged in every
// commit since, including b6d14a8a, which added the fixture; the recording run read
// contract/schema. They are not TestSchemaFiles_python_packaged_contract's digests, which follow
// the contract bundle's revision: these move only when the cases are judged again. The five
// schemas hold only internal "#/definitions" references; a "$ref" to another file would escape
// the pin, and that file would need a pin too.
var schemaPins = map[string]string{
	"acknowledgement":      "193c2a1dbdb6197f852aaa38c6b8b4e55ba804ffc66e7b2a73925365b133eb7c",
	"completion-receipt":   "8111438e60b46b209a33902dd9080426953dfaaf2a7025cb47c2336d72b49317",
	"delivery-attempt":     "e821647e35bee9179321332d8b7df06f0fc61f2da49c7b74fb6128b8802650d1",
	"relationship":         "c8ebaf4559fa1ac6df26d98c4214938caf78bd8c61659bce026da6a3595a4b90",
	"verification-verdict": "0b3f8f4b061cff2992fc60a7c1f45dec6f803116894735751c40df3a8d356af9",
}

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
// written to the fixture. A verdict also holds only for the schema it was judged against, which
// schemaPins fixes.
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
// schema validated. Before grading it requires every schema to be the one the verdicts were
// judged against (schemaPins): verdicts of a changed schema grade nothing.
func validateAgainstSchemas(t *testing.T, cases []schemaCase) (map[string][]string, map[string]int) {
	t.Helper()
	perSchema := map[string]int{}
	for _, c := range cases {
		perSchema[c.Schema]++
	}
	if problems := schemaPinProblems(filepath.Join(repositoryRoot(t), "contract", "schema"), schemaPins, perSchema); len(problems) > 0 {
		t.Fatalf("the schemas are not the ones the Draft 7 verdicts were judged against:\n%s", strings.Join(problems, "\n"))
	}
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

// schemaPinProblems says, one line per schema, why the verdicts of its cases cannot be trusted:
// the schema's file in dir is not the pinned bytes or cannot be read, a schema the cases use has
// no pin, or a pin has no case. cases counts the cases per schema name.
func schemaPinProblems(dir string, pinned map[string]string, cases map[string]int) []string {
	names := map[string]bool{}
	for name := range pinned {
		names[name] = true
	}
	for name := range cases {
		names[name] = true
	}
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(names)) {
		pin, isPinned := pinned[name]
		switch {
		case !isPinned:
			problems = append(problems, fmt.Sprintf("schema %s has no pinned digest and %d cases are judged against it: pin the schema file they were judged against", name, cases[name]))
		case cases[name] == 0:
			problems = append(problems, fmt.Sprintf("a digest is pinned for schema %s, which no case is judged against: remove the pin", name))
		default:
			data, err := os.ReadFile(filepath.Join(dir, name+".json"))
			if err != nil {
				problems = append(problems, fmt.Sprintf("schema %s cannot be read, so its %d cases cannot be graded: %v", name, cases[name], err))
				continue
			}
			sum := sha256.Sum256(data)
			if now := hex.EncodeToString(sum[:]); now != pin {
				problems = append(problems, fmt.Sprintf("schema %s (contract/schema/%s.json) changed since the Draft 7 verdicts were recorded: sha256 %s, pinned %s; its %d cases must be re-judged: judge them against the changed schema, write their verdicts to draft7-verdicts.json.gz, then update schemaPins", name, name, now, pin, cases[name]))
			}
		}
	}
	return problems
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

// The guard has to fail when a schema changes, and the real schemas are frozen, so each failure
// is made on a copy of them in a temporary directory: one problem, naming the one schema at fault.
func TestDraft7SchemaPins_name_the_schema_to_judge_again(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(t *testing.T, dir string, pinned map[string]string, cases map[string]int)
		schema string // the schema the one problem names; empty when nothing is wrong
		says   string
	}{
		{name: "the schemas as pinned", change: func(*testing.T, string, map[string]string, map[string]int) {}},
		{name: "one byte added to a schema", schema: "acknowledgement", says: "must be re-judged", change: func(t *testing.T, dir string, _ map[string]string, _ map[string]int) {
			path := filepath.Join(dir, "acknowledgement.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a schema file removed", schema: "relationship", says: "cannot be read", change: func(t *testing.T, dir string, _ map[string]string, _ map[string]int) {
			if err := os.Remove(filepath.Join(dir, "relationship.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a schema without a pin", schema: "delivery-attempt", says: "no pinned digest", change: func(_ *testing.T, _ string, pinned map[string]string, _ map[string]int) {
			delete(pinned, "delivery-attempt")
		}},
		{name: "a pin no case is judged against", schema: "verification-verdict", says: "no case is judged against", change: func(_ *testing.T, _ string, _ map[string]string, cases map[string]int) {
			delete(cases, "verification-verdict")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, pinned, cases := t.TempDir(), maps.Clone(schemaPins), map[string]int{}
			for name := range schemaPins {
				data, err := os.ReadFile(filepath.Join(repositoryRoot(t), "contract", "schema", name+".json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name+".json"), data, 0o600); err != nil {
					t.Fatal(err)
				}
				cases[name] = 3
			}
			c.change(t, dir, pinned, cases)
			problems := schemaPinProblems(dir, pinned, cases)
			if c.schema == "" {
				if len(problems) > 0 {
					t.Fatalf("the pinned schemas are reported: %q", problems)
				}
				return
			}
			if len(problems) != 1 || !strings.Contains(problems[0], c.schema) || !strings.Contains(problems[0], c.says) {
				t.Fatalf("want one problem naming %s and saying %q, got %q", c.schema, c.says, problems)
			}
			for name := range schemaPins {
				if name != c.schema && strings.Contains(problems[0], name) {
					t.Errorf("the problem for %s also names %s: %s", c.schema, name, problems[0])
				}
			}
		})
	}
}
