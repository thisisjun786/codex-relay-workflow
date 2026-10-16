package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
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

// designJSON is a DESIGN with the issues given (each a map of its members), one decision D1 and nothing else.
func designJSON(t *testing.T, issues ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"decisions": []any{map[string]any{"id": "D1", "evidence": []string{}, "change": "c", "alternatives": []string{}, "assumptions": []string{}}},
		"issues": issues, "open_questions": []any{}, "risks": []string{}, "dispositions": nil})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// designIssueJSON is the plan of one issue of a DESIGN of the S-1 shape: it carries the sub_tasks member (subTasks, nil for null).
func designIssueJSON(node, fit string, regions []string, subTasks any) map[string]any {
	reason := ""
	if fit != "executor" {
		reason = "two packages"
	}
	return map[string]any{"issue": "CRW-" + node, "node": node, "size_fit": fit, "size_reason": reason, "issue_size_decision": "ok", "edit_regions": regions,
		"approach": "a", "tests_first": []string{"TestA"}, "done_condition": "d", "decision_refs": []string{"D1"}, "order_after": []string{}, "group": nil, "sub_tasks": subTasks}
}

// subTaskJSON is one sub-task of a split node.
func subTaskJSON(id string, regions []string, after any) map[string]any {
	return map[string]any{"id": id, "summary": "do " + id, "edit_regions": regions, "done_condition": id + " is done", "after": after}
}

// A split node of a DESIGN of the S-1 shape names 2 to 4 sub-tasks (CRW-1435): each with an id a sub-task key and a worktree path can
// carry, unique in the node, edit regions inside the node's own, and an `after` that names only earlier sub-tasks (the array order is a
// topological order). An executor or bundle_split node names none (null or empty).
func TestDecodeDesignSplitNeedsSubTasks(t *testing.T) {
	regions := []string{"pkg/", "cmd/main.go", "lib/x.go#F"}
	split := func(subTasks any) []byte {
		return designJSON(t, designIssueJSON("n1", "split", regions, subTasks))
	}
	two := []any{subTaskJSON("a", []string{"pkg/a.go"}, nil), subTaskJSON("b", []string{"cmd/main.go#Run"}, []string{"a"})}
	d, err := DecodeDesign(split(two))
	if err != nil {
		t.Fatalf("a split node with two sub-tasks is refused: %v", err)
	}
	b, _ := json.Marshal(d)
	var back map[string]any
	_ = json.Unmarshal(b, &back)
	issues, _ := back["issues"].([]any)
	if len(issues) != 1 {
		t.Fatalf("the decoded DESIGN %s", b)
	}
	got, _ := json.Marshal(issues[0].(map[string]any)["sub_tasks"])
	want := `[{"after":null,"done_condition":"a is done","edit_regions":["pkg/a.go"],"id":"a","summary":"do a"},{"after":["a"],"done_condition":"b is done","edit_regions":["cmd/main.go#Run"],"id":"b","summary":"do b"}]`
	if string(got) != want {
		t.Errorf("the sub-tasks decode to %s, want %s", got, want)
	}

	accepted := map[string]any{
		"four sub-tasks": []any{subTaskJSON("a", []string{"pkg/"}, nil), subTaskJSON("b", []string{"pkg/b/c.go#G"}, []string{}), subTaskJSON("c", []string{"cmd/main.go"}, []string{"a", "b"}),
			subTaskJSON("d-2", []string{"lib/x.go#F"}, []string{"c"})},
		"a region equal to the node's after cleaning": []any{subTaskJSON("a", []string{"./pkg/"}, nil), subTaskJSON("b", []string{"cmd//main.go"}, nil)},
	}
	for name, subs := range accepted {
		if _, err := DecodeDesign(split(subs)); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	refused := map[string]any{
		"no sub-tasks (null)":            nil,
		"no sub-tasks (empty)":           []any{},
		"one sub-task":                   []any{subTaskJSON("a", []string{"pkg/a.go"}, nil)},
		"five sub-tasks":                 []any{subTaskJSON("a", []string{"pkg/"}, nil), subTaskJSON("b", []string{"pkg/"}, nil), subTaskJSON("c", []string{"pkg/"}, nil), subTaskJSON("d", []string{"pkg/"}, nil), subTaskJSON("e", []string{"pkg/"}, nil)},
		"two sub-tasks of one id":        []any{subTaskJSON("a", []string{"pkg/a.go"}, nil), subTaskJSON("a", []string{"cmd/main.go"}, nil)},
		"an id with a capital":           []any{subTaskJSON("A", []string{"pkg/a.go"}, nil), subTaskJSON("b", []string{"cmd/main.go"}, nil)},
		"an id with a slash":             []any{subTaskJSON("a/b", []string{"pkg/a.go"}, nil), subTaskJSON("b", []string{"cmd/main.go"}, nil)},
		"an id that starts with a dash":  []any{subTaskJSON("-a", []string{"pkg/a.go"}, nil), subTaskJSON("b", []string{"cmd/main.go"}, nil)},
		"an id of 33 characters":         []any{subTaskJSON(strings.Repeat("a", 33), []string{"pkg/a.go"}, nil), subTaskJSON("b", []string{"cmd/main.go"}, nil)},
		"a sub-task with no regions":     []any{subTaskJSON("a", []string{}, nil), subTaskJSON("b", []string{"cmd/main.go"}, nil)},
		"a region outside the node":      []any{subTaskJSON("a", []string{"pkg/a.go"}, nil), subTaskJSON("b", []string{"other/b.go"}, nil)},
		"a path that only starts alike":  []any{subTaskJSON("a", []string{"pkgx/a.go"}, nil), subTaskJSON("b", []string{"cmd/main.go"}, nil)},
		"a file under a file region":     []any{subTaskJSON("a", []string{"pkg/a.go"}, nil), subTaskJSON("b", []string{"cmd/main.go/x"}, nil)},
		"another symbol of a symbol":     []any{subTaskJSON("a", []string{"pkg/a.go"}, nil), subTaskJSON("b", []string{"lib/x.go#G"}, nil)},
		"the file of a symbol region":    []any{subTaskJSON("a", []string{"pkg/a.go"}, nil), subTaskJSON("b", []string{"lib/x.go"}, nil)},
		"after names a later sub-task":   []any{subTaskJSON("a", []string{"pkg/a.go"}, []string{"b"}), subTaskJSON("b", []string{"cmd/main.go"}, nil)},
		"after names itself":             []any{subTaskJSON("a", []string{"pkg/a.go"}, nil), subTaskJSON("b", []string{"cmd/main.go"}, []string{"b"})},
		"after names no sub-task":        []any{subTaskJSON("a", []string{"pkg/a.go"}, nil), subTaskJSON("b", []string{"cmd/main.go"}, []string{"z"})},
		"a sub-task without its summary": []any{map[string]any{"id": "a", "edit_regions": []string{"pkg/a.go"}, "done_condition": "d", "after": nil}, subTaskJSON("b", []string{"cmd/main.go"}, nil)},
	}
	for name, subs := range refused {
		if _, err := DecodeDesign(split(subs)); err == nil {
			t.Errorf("%s: a split node is accepted", name)
		}
	}

	// the root tree holds every relative path inside it, and no path that leaves it
	root := func(region string) []byte {
		return designJSON(t, designIssueJSON("n1", "split", []string{"./"}, []any{subTaskJSON("a", []string{"pkg/a.go"}, nil), subTaskJSON("b", []string{region}, nil)}))
	}
	for _, region := range []string{"cmd/main.go#Run", "docs/", "./README.md"} {
		if _, err := DecodeDesign(root(region)); err != nil {
			t.Errorf("a sub-task region %q under the root tree is refused: %v", region, err)
		}
	}
	for _, region := range []string{"../outside.go", "/tmp/outside.go", "..", "pkg/../../outside.go"} {
		if _, err := DecodeDesign(root(region)); err == nil {
			t.Errorf("a sub-task region %q that leaves the root tree is accepted", region)
		}
	}

	for _, fit := range []string{"executor", "bundle_split"} {
		for _, subs := range []any{nil, []any{}} {
			if _, err := DecodeDesign(designJSON(t, designIssueJSON("n1", fit, regions, subs))); err != nil {
				t.Errorf("a %s node with sub_tasks %v is refused: %v", fit, subs, err)
			}
		}
		if _, err := DecodeDesign(designJSON(t, designIssueJSON("n1", fit, regions, two))); err == nil {
			t.Errorf("a %s node with sub-tasks is accepted", fit)
		}
	}
	// a bundle_split names why, as every node that is not an executor's does
	noReason := designIssueJSON("n1", "bundle_split", regions, nil)
	noReason["size_reason"] = ""
	if _, err := DecodeDesign(designJSON(t, noReason)); err == nil {
		t.Error("a bundle_split node with no size_reason is accepted")
	}
}

// A DESIGN of the S-1 shape (an issue carries the sub_tasks member) sizes a node for an executor, a split or a bundle split: the `child`
// session path is retired (CRW-1415), so a fresh answer of the architect cannot size a node for a child, and the schema says so. Every
// issue of it carries sub_tasks (strict mode).
func TestDecodeDesignRejectsChildForNewDesign(t *testing.T) {
	var sch struct {
		Properties struct {
			Issues struct {
				Items struct {
					Properties map[string]struct {
						Enum []string `json:"enum"`
					} `json:"properties"`
					Required []string `json:"required"`
				} `json:"items"`
			} `json:"issues"`
		} `json:"properties"`
	}
	text, _ := SchemaText(SchemaDesign)
	if err := json.Unmarshal([]byte(text), &sch); err != nil {
		t.Fatal(err)
	}
	items := sch.Properties.Issues.Items
	if got := items.Properties["size_fit"].Enum; !reflect.DeepEqual(got, []string{"executor", "split", "bundle_split"}) {
		t.Errorf("the size_fit of a DESIGN is one of %v, want executor, split, bundle_split", got)
	}
	if !slices.Contains(items.Required, "sub_tasks") {
		t.Errorf("an issue of a DESIGN does not require sub_tasks: %v", items.Required)
	}
	child := designJSON(t, designIssueJSON("n1", "child", []string{"a/"}, nil))
	if err := Validate(SchemaDesign, child); err == nil {
		t.Error("the schema accepts a node sized for a child")
	}
	if _, err := DecodeDesign(child); err == nil {
		t.Error("a DESIGN of the S-1 shape with a node sized for a child decodes")
	}
	executor := designJSON(t, designIssueJSON("n1", "executor", []string{"a/"}, nil), designIssueJSON("n2", "split", []string{"b/"},
		[]any{subTaskJSON("a", []string{"b/a.go"}, nil), subTaskJSON("b", []string{"b/b.go"}, nil)}))
	if err := Validate(SchemaDesign, executor); err != nil {
		t.Errorf("the schema refuses an executor and a split node with sub_tasks: %v", err)
	}
	// strict mode: an issue without the member is not a fresh answer (the engine holds the architect to the schema)
	var m map[string]any
	_ = json.Unmarshal(executor, &m)
	delete(m["issues"].([]any)[0].(map[string]any), "sub_tasks")
	partial, _ := json.Marshal(m)
	if err := Validate(SchemaDesign, partial); err == nil {
		t.Error("the schema accepts an issue without sub_tasks")
	}
}

// ledgeredLegacy makes the rows of a milestone M whose DESIGN, recorded by a binary before S-1 (no issue carries sub_tasks), passed its
// audit and became revision r0: the rows an old run left in the journal.
func (w *world) ledgeredLegacy(d Design) json.RawMessage {
	w.t.Helper()
	design := legacyDesignJSON(w.t, d)
	w.must("milestone", "plan-1", "M")
	cand := Candidate{Design: design, Answers: json.RawMessage(`[]`)}
	var res AuditResult
	_ = json.Unmarshal(w.must("designAudit", "M", 1, cand, call("c_arch", "completed", "claude-x", nil),
		call("c_aud", "completed", "gpt-y", map[string]any{"verdict": "approved", "findings": []any{}, "summary": "ok"})), &res)
	if res.State != "approved" {
		w.t.Fatalf("the audit of a legacy DESIGN: %+v", res)
	}
	w.must("designLedger", "M", map[string]any{"revision": "r0", "design": design, "answers": json.RawMessage(`[]`), "architect": call("c_arch", "completed", "claude-x", nil)})
	return design
}

// The readers of a ledger row read a DESIGN an earlier binary recorded (CRW-1435): no issue carries sub_tasks and a node is sized for a
// child. DecodeDesign reads it; crw.issuePlan gives the node's plan with its decisions, and the final gate's
// decisions name the nodes of each decision. The last two ignore a decode error and would degrade to no decisions without a word.
func TestLedgerReaderAcceptsLegacyChild(t *testing.T) {
	w := newWorld(t)
	w.relay.plan = planOfNodes(map[string]string{"A": "sa", "B": "sb"}, "A", "B")
	d := designOf("A", "B")
	d.Issues[1].SizeFit, d.Issues[1].SizeReason, d.Issues[1].IssueSizeDecision = "child", "a child of its own", "over_line"
	design := w.ledgeredLegacy(d)
	if bytes.Contains(design, []byte("sub_tasks")) {
		t.Fatalf("the legacy DESIGN carries sub_tasks: %s", design)
	}

	got, err := DecodeDesign(design)
	if err != nil {
		t.Fatalf("the legacy DESIGN does not decode: %v", err)
	}
	if len(got.Issues) != 2 || got.Issues[1].SizeFit != "child" {
		t.Fatalf("the legacy DESIGN decodes to %+v", got.Issues)
	}

	w.must("release", "plan-1", "B", "M")
	var plan IssuePlan
	if err := json.Unmarshal(w.must("issuePlan", "B"), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.SizeFit != "child" || len(plan.Decisions) != 1 || plan.Decisions[0].ID != "D1" {
		t.Errorf("crw.issuePlan of the child-sized node over the legacy row: size_fit %q, decisions %+v, want child and D1", plan.SizeFit, plan.Decisions)
	}
	gd := gateDecisions(rows(w.j.Events()), "M")
	if len(gd) != 1 || gd[0].ID != "D1" || !reflect.DeepEqual(gd[0].Nodes, []string{"A", "B"}) {
		t.Errorf("the final gate's decisions over the legacy row: %+v, want D1 of A and B", gd)
	}
}

// The DESIGN schema holds the sub-task rules a strict schema can express (CRW-1435, the DI's P2): at most 4 sub-tasks, the sub-task id
// form and at least one edit region per sub-task, in the Go copy and in crw:lib/schemas alike (TestSchemasProducerConsumerPaired). So an
// architect answer that breaks one is refused by the engine's schema check of the answer. The rules that depend on the issue or on the
// other sub-tasks (2 to 4 for a split node and none for another, unique ids, regions within the node's, `after` naming only earlier
// sub-tasks) pass the schema and are design rules: DecodeDesign answers them as a *DesignRuleError naming the node, which
// crw.designAudit turns into a finding of the audit.
func TestTheDesignSchemaHoldsTheSubTaskRulesItCanExpress(t *testing.T) {
	regions := []string{"pkg/", "cmd/main.go"}
	split := func(fit string, subs any) []byte {
		return designJSON(t, designIssueJSON("n1", fit, regions, subs))
	}
	ok := subTaskJSON("b", []string{"cmd/main.go"}, nil)
	schemaRefused := map[string][]byte{
		"five sub-tasks": split("split", []any{subTaskJSON("a", []string{"pkg/"}, nil), subTaskJSON("b", []string{"pkg/"}, nil), subTaskJSON("c", []string{"pkg/"}, nil),
			subTaskJSON("d", []string{"pkg/"}, nil), subTaskJSON("e", []string{"pkg/"}, nil)}),
		"an id with a capital":          split("split", []any{subTaskJSON("S1", []string{"pkg/a.go"}, nil), ok}),
		"an id with a slash":            split("split", []any{subTaskJSON("a/b", []string{"pkg/a.go"}, nil), ok}),
		"an id that starts with a dash": split("split", []any{subTaskJSON("-a", []string{"pkg/a.go"}, nil), ok}),
		"an id of 33 characters":        split("split", []any{subTaskJSON(strings.Repeat("a", 33), []string{"pkg/a.go"}, nil), ok}),
		"a sub-task with no regions":    split("split", []any{subTaskJSON("a", []string{}, nil), ok}),
	}
	for name, raw := range schemaRefused {
		if err := Validate(SchemaDesign, raw); err == nil {
			t.Errorf("%s: the DESIGN schema accepts it", name)
		}
		var rule *DesignRuleError
		if _, err := DecodeDesign(raw); err == nil || errors.As(err, &rule) {
			t.Errorf("%s: DecodeDesign answers %v, want a refusal of the schema", name, err)
		}
	}
	ruleRefused := map[string][]byte{
		"one sub-task of a split":      split("split", []any{subTaskJSON("a", []string{"pkg/a.go"}, nil)}),
		"no sub-task of a split":       split("split", nil),
		"sub-tasks of an executor":     split("executor", []any{subTaskJSON("a", []string{"pkg/a.go"}, nil), ok}),
		"two sub-tasks of one id":      split("split", []any{subTaskJSON("a", []string{"pkg/a.go"}, nil), subTaskJSON("a", []string{"cmd/main.go"}, nil)}),
		"a region outside the node":    split("split", []any{subTaskJSON("a", []string{"pkg/a.go"}, nil), subTaskJSON("b", []string{"other/b.go"}, nil)}),
		"after names a later sub-task": split("split", []any{subTaskJSON("a", []string{"pkg/a.go"}, []string{"b"}), ok}),
	}
	for name, raw := range ruleRefused {
		if err := Validate(SchemaDesign, raw); err != nil {
			t.Errorf("%s: the DESIGN schema refuses it (%v); it is a design rule", name, err)
		}
		var rule *DesignRuleError
		if _, err := DecodeDesign(raw); !errors.As(err, &rule) || rule.Node != "n1" {
			t.Errorf("%s: DecodeDesign answers %v, want a design rule of node n1", name, err)
		}
	}
}

// crw.designAudit judges a candidate that breaks a design rule (TestTheDesignSchemaHoldsTheSubTaskRulesItCanExpress) as a DESIGN to fix,
// not as an op error: the audit's state is needs_changes with a P1 finding that names the node and the rule, added to the critic's own
// findings, also when the critic approved, so the lane sends the DESIGN back to the architect for a design-fix round. A call of the
// critic that did not complete keeps its state. A candidate that is no DESIGN of the schema is still an op error.
func TestDesignAuditSendsABrokenDesignRuleBackToTheArchitect(t *testing.T) {
	w := newWorld(t)
	w.must("milestone", "plan-1", "M")
	broken := designJSON(t, designIssueJSON("n1", "split", []string{"pkg/"}, []any{subTaskJSON("a", []string{"pkg/a.go"}, nil), subTaskJSON("b", []string{"other/b.go"}, nil)}))
	cand := Candidate{Design: broken, Answers: json.RawMessage(`[]`)}
	audit := func(critic CallRef) AuditResult {
		t.Helper()
		var r AuditResult
		_ = json.Unmarshal(w.must("designAudit", "M", 1, cand, call("c_arch", "completed", "claude-x", nil), critic), &r)
		return r
	}
	approved := audit(call("c_aud", "completed", "gpt-y", map[string]any{"verdict": "approved", "findings": []any{}, "summary": "ok"}))
	if approved.State != AuditNeedsChanges || len(approved.Findings) != 1 || approved.Findings[0].Ref != "n1" || approved.Findings[0].Severity != "P1" ||
		!strings.Contains(approved.Findings[0].Finding, "other/b.go") {
		t.Errorf("an approved audit of a DESIGN that breaks a design rule: %+v", approved)
	}
	own := map[string]any{"ref": "D1", "severity": "P2", "finding": "the decision needs evidence", "evidence": []string{}}
	needs := audit(call("c_aud2", "completed", "gpt-y", map[string]any{"verdict": "needs_changes", "findings": []any{own}, "summary": "fix"}))
	if needs.State != AuditNeedsChanges || len(needs.Findings) != 2 || needs.Findings[0].Ref != "D1" || needs.Findings[1].Ref != "n1" {
		t.Errorf("a needs_changes audit of a DESIGN that breaks a design rule: %+v", needs)
	}
	if held := audit(call("c_aud3", "held", "gpt-y", nil)); held.State != StateHeld {
		t.Errorf("a held critic call: %+v", held)
	}
	bad := Candidate{Design: designJSON(t, designIssueJSON("n1", "split", []string{"pkg/"}, []any{subTaskJSON("S1", []string{"pkg/a.go"}, nil), subTaskJSON("b", []string{"pkg/b.go"}, nil)})),
		Answers: json.RawMessage(`[]`)}
	if _, err := w.op("designAudit", "M", 1, bad, call("c_arch", "completed", "claude-x", nil), call("c_aud4", "completed", "gpt-y", map[string]any{"verdict": "approved", "findings": []any{}, "summary": "ok"})); err == nil {
		t.Error("a candidate the DESIGN schema refuses is audited")
	}
}
