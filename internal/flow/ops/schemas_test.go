package ops

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows/lib"
)

// librarySchemas are the schemas crw:lib/schemas exports, as the script engine sees them.
func librarySchemas(t *testing.T) map[string]any {
	t.Helper()
	prog, err := script.Load(script.Source{Name: "schemas.js", Text: []byte("import * as S from 'crw:lib/schemas'\nexport const meta = { name: 'schemas' }\nreturn S\n")})
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Run(context.Background(), prog, engine.Config{Backend: engine.NewFake(), Policy: engine.FakePolicy()})
	if err != nil || res.Status != engine.RunCompleted {
		t.Fatalf("%v %+v", err, res)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Value, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// schemaOwner says which issue builds the producer of a schema this build only decodes, or retired it: the build then decodes what an
// old run's producer left in its rows.
var schemaOwner = map[string]string{
	SchemaVerdictBatch: "CRW-1225 (the candidate batch review, SP-REV-2)",
	SchemaVerdict:      "CRW-1447 (the node reviewer is retired: crw.accept decodes the VERDICT of an old run's crw.review row)",
}

// consumers are the decoders of the Go side, by schema.
var consumers = map[string]string{
	SchemaReceipt: "DecodeReceipt", SchemaVerdict: "DecodeVerdict", SchemaVerdictBatch: "DecodeVerdictBatch",
	SchemaDesign: "DecodeDesign", SchemaDesignAudit: "DecodeDesignAudit", SchemaReflection: "DecodeReflection", SchemaFindings: "DecodeFindings", SchemaFindingsEnd: "DecodeFindings",
	SchemaQACapture: "DecodeQACapture", SchemaQAPass: "DecodeQAPass", SchemaQAReceipt: "DecodeQAReceipt",
}

// readAll reads the text of every file of a directory tree of an embedded or real file system.
func readAll(t *testing.T, fsys fs.FS, suffix string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, suffix) {
			return err
		}
		b, err := fs.ReadFile(fsys, path)
		out[path] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Every schema of the library has a validation copy in Go that is the same document, passes the strict structured-output lint,
// has a producer (a workflow or a library helper hands it to an agent call as its schema) and a consumer (a decoder of the Go
// side that an operation calls); a schema that only the Go side decodes in this build is owned by the issue that builds its
// producer, and no workflow of this build produces it.
func TestSchemasProducerConsumerPaired(t *testing.T) {
	js := librarySchemas(t)
	for name := range js {
		if _, ok := schemaTexts[name]; !ok {
			t.Errorf("crw:lib/schemas exports %s and ops/schemas.go has no copy of it", name)
		}
	}
	producers := map[string][]string{}
	sources := map[string]string{}
	for p, s := range readAll(t, lib.FS, ".js") {
		if p != "schemas.js" {
			sources["lib/"+p] = s
		}
	}
	for p, s := range readAll(t, workflows.FS, ".js") {
		sources[p] = s
	}
	for name := range schemaTexts {
		want, ok := js[name]
		if !ok {
			t.Errorf("ops/schemas.go holds a copy of %s and crw:lib/schemas does not export it", name)
			continue
		}
		var got any
		if err := json.Unmarshal([]byte(schemaTexts[name]), &got); err != nil {
			t.Errorf("%s: the Go copy is not JSON: %v", name, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			gb, _ := json.MarshalIndent(got, "", " ")
			wb, _ := json.MarshalIndent(want, "", " ")
			t.Errorf("%s: the Go copy differs from the library\nGo:\n%s\nlibrary:\n%s", name, gb, wb)
		}
		if errs := script.LintStrictSchema(want); len(errs) > 0 {
			t.Errorf("%s fails the strict structured-output lint: %v", name, errs)
		}
		re := regexp.MustCompile(`schema:\s*` + name + `\b`)
		for p, s := range sources {
			if re.MatchString(s) {
				producers[name] = append(producers[name], p)
			}
		}
		// the consumer is a function of this package that a non-test file calls
		fn := consumers[name]
		if fn == "" {
			t.Errorf("%s: no consumer is named", name)
			continue
		}
		called := false
		ents, _ := os.ReadDir(".")
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") && e.Name() != "schemas.go" {
				b, _ := os.ReadFile(e.Name())
				if strings.Contains(string(b), fn+"(") {
					called = true
				}
			}
		}
		if !called && schemaOwner[name] == "" {
			t.Errorf("%s: %s is not called by any operation", name, fn)
		}
	}
	for name := range schemaTexts {
		owner, deferred := schemaOwner[name]
		switch {
		case deferred && len(producers[name]) > 0:
			t.Errorf("%s is owned by %s, and %v already produce it: move it out of the deferred list", name, owner, producers[name])
		case !deferred && len(producers[name]) == 0:
			t.Errorf("%s has no producer: no workflow or helper hands it to an agent call", name)
		}
	}
	// the Go side validates what it decodes: a document that breaks the rules the JSON Schema cannot say is not a verdict
	if _, err := DecodeVerdict([]byte(`{"verdict":"needs_changes","criteria":[],"defects":[{"ref":"d","kind":"evidence_missing","severity":"P2","introduced":false,"impact":"minor_separable","inPromise":false,"file":"f","line":1,"finding":"x","evidence_question":null}],"summary":"s"}`)); err == nil {
		t.Errorf("an evidence_missing defect without its evidence_question decoded")
	}
}

// No file of internal/flow holds the text of a spawn-policy rule id except rules.go: everything else names a rule by its key.
func TestRuleIDsHaveOneSource(t *testing.T) {
	quoted := regexp.MustCompile("[\"'`]SP-[A-Z]+-[0-9]+")
	root := ".."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if (!strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, ".js")) || strings.HasSuffix(name, "_test.go") || filepath.ToSlash(path) == "../ops/rules.go" {
			return nil
		}
		if strings.Contains(filepath.ToSlash(path), "/tracetest/") {
			return nil // the test builder of the trace package is a test double, not a writer
		}
		if filepath.ToSlash(path) == "../engine/legacy.go" {
			return nil // the legacy refusal reads old rows by the retired ids; engine/legacy_ops_test.go pins them to ops/rules.go
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if loc := quoted.FindIndex(b); loc != nil {
			t.Errorf("%s holds a rule id literal (%q): take it from ops/rules.go", path, b[loc[0]:min(loc[1]+4, len(b))])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for key, id := range RuleKeys {
		if !regexp.MustCompile(`^SP-[A-Z]+-[0-9]+$`).MatchString(id) {
			t.Errorf("rule %s has the id %q", key, id)
		}
		if other, dup := seen[id]; dup {
			t.Errorf("rules %s and %s share the id %s", key, other, id)
		}
		seen[id] = key
		if RuleKey(id) != key {
			t.Errorf("RuleKey(%s) = %s, want %s", id, RuleKey(id), key)
		}
	}
	if NativeRule != RuleNat1 {
		t.Errorf("the native spawn rule is %s, the rule table says %s", NativeRule, RuleNat1)
	}
}

// A DESIGN carries dispositions: null in the first DESIGN and a re-plan answer, and in a design-fix answer one entry per finding
// id of the fix packet, each folded or rebutted with its reason. The key is required (strict mode) of a fresh answer, a DESIGN
// recorded before the key existed is read with it null, and the disposition is a closed set.
func TestDesignDispositionsAreNullableAndClosed(t *testing.T) {
	design := func(dispositions string) []byte {
		head := `{"decisions":[],"issues":[{"issue":"CRW-1","node":"n1","size_fit":"executor","size_reason":"","issue_size_decision":"ok",` +
			`"edit_regions":["a.go"],"approach":"a","tests_first":["TestA"],"done_condition":"d","decision_refs":[],"order_after":[],"group":null}],` +
			`"open_questions":[],"risks":[]`
		if dispositions == "" {
			return []byte(head + `}`)
		}
		return []byte(head + `,"dispositions":` + dispositions + `}`)
	}
	reencode := func(t *testing.T, d Design) map[string]any {
		t.Helper()
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	d, err := DecodeDesign(design(`null`))
	if err != nil {
		t.Fatalf("a DESIGN with dispositions null is refused: %v", err)
	}
	if m := reencode(t, d); m["dispositions"] != nil {
		t.Errorf("dispositions null decodes to %v, want null", m["dispositions"])
	} else if _, has := m["dispositions"]; !has {
		t.Errorf("a decoded DESIGN has no dispositions field: %v", m)
	}

	d, err = DecodeDesign(design(`[{"finding":"r1.1","disposition":"folded","reason":"the unit gains the caller"},` +
		`{"finding":"r1.2","disposition":"rebutted","reason":"the region is not shared"}]`))
	if err != nil {
		t.Fatalf("a DESIGN with a folded/rebutted list is refused: %v", err)
	}
	got, _ := json.Marshal(reencode(t, d)["dispositions"])
	want := `[{"disposition":"folded","finding":"r1.1","reason":"the unit gains the caller"},{"disposition":"rebutted","finding":"r1.2","reason":"the region is not shared"}]`
	if string(got) != want {
		t.Errorf("dispositions decode to %s, want %s", got, want)
	}

	if _, err := DecodeDesign(design(`[{"finding":"r1.1","disposition":"ignored","reason":"r"}]`)); err == nil {
		t.Error("a DESIGN with the disposition ignored is accepted")
	}
	if _, err := DecodeDesign(design(`[{"finding":"r1.1","disposition":"folded"}]`)); err == nil {
		t.Error("a disposition without its reason is accepted")
	}
	// a fresh answer without the key breaks the strict schema; a DESIGN an earlier binary recorded without it is read as null
	if err := Validate(SchemaDesign, design(``)); err == nil {
		t.Error("the schema accepts a DESIGN without the dispositions key")
	}
	if d, err := DecodeDesign(design(``)); err != nil || d.Dispositions != nil {
		t.Errorf("a DESIGN recorded without the dispositions key: %+v %v, want it read with dispositions null", d, err)
	}
	if _, err := DecodeDesign([]byte(`{"decisions":[],"issues":[],"open_questions":[],"risks":[],"dispositions":"none"}`)); err == nil {
		t.Error("a DESIGN whose dispositions is no list and not null is accepted")
	}
}
