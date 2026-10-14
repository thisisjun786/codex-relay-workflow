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

// schemaOwner says which issue builds the producer of a schema this build only decodes.
var schemaOwner = map[string]string{
	SchemaVerdictBatch: "CRW-1225 (the candidate batch review, SP-REV-2)",
}

// consumers are the decoders of the Go side, by schema.
var consumers = map[string]string{
	SchemaReceipt: "DecodeReceipt", SchemaVerdict: "DecodeVerdict", SchemaVerdictBatch: "DecodeVerdictBatch", SchemaVerdictFinal: "DecodeVerdictFinal",
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
	if _, err := DecodeVerdict([]byte(`{"verdict":"needs_changes","criteria":[],"defects":[{"ref":"d","kind":"evidence_missing","severity":"P2","introduced":false,"impact":"","inPromise":false,"file":"f","line":1,"finding":"x","evidence_question":null}],"summary":"s"}`)); err == nil {
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
