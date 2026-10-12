package script

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dop251/goja"
)

// runSource loads src, runs its body and returns the exported value the body returned.
func runSource(t *testing.T, src Source, limits Limits) (any, error) {
	t.Helper()
	p, err := Load(src)
	if err != nil {
		return nil, err
	}
	rt, err := p.NewRuntime(limits)
	if err != nil {
		return nil, err
	}
	prom, err := rt.Main()
	if err != nil {
		return nil, err
	}
	var loopErr error
	rt.Loop.Run(func(e error) { loopErr = e })
	if loopErr != nil {
		return nil, loopErr
	}
	switch prom.State() {
	case goja.PromiseStateFulfilled:
		return prom.Result().Export(), nil
	case goja.PromiseStateRejected:
		return nil, errors.New(prom.Result().String())
	}
	return nil, errors.New("the script body did not settle")
}

func loadErr(t *testing.T, src Source) string {
	t.Helper()
	_, err := Load(src)
	if err == nil {
		t.Fatalf("Load(%q) succeeded, want a load error", src.Text)
	}
	var le *LoadError
	if !errors.As(err, &le) {
		t.Fatalf("Load error %T %v is not a *LoadError", err, err)
	}
	return err.Error()
}

// One file: export meta, a crw:lib import, a relative import, top-level await and top-level return all run.
func TestLoaderRunsMetaHelperAwaitReturn(t *testing.T) {
	dir := fstest.MapFS{
		"util.js":     {Data: []byte("import { twice } from './lib/math.js'\nexport function quad(x) { return twice(twice(x)) }\nexport default 'dflt'\n")},
		"lib/math.js": {Data: []byte("export const twice = (x) => x * 2\n")},
	}
	lib := fstest.MapFS{
		"words.js": {Data: []byte("export const GREETING = 'hello'\nexport const KIND = { a: 1, b: [2, 3] }\n")},
	}
	text := `import { GREETING, KIND as K } from 'crw:lib/words'
import dflt, { quad } from './util.js'
import * as ns from "crw:lib/words"
export const meta = { name: 'all-in-one', phases: ['one', 'two'], note: 'a } brace and a ` + "`" + `tick` + "`" + `' }
const first = await new Promise((resolve) => resolve(quad(5)))
const second = await Promise.resolve(first + 1)
if (second > 100) return { early: true }
const spread = { ...K, extra: ns.GREETING.length }
return { greeting: GREETING, first, second, dflt, spread, line: new Error('x').stack !== undefined }
`
	got, err := runSource(t, Source{Name: "all.js", Text: []byte(text), Dir: dir, Lib: lib}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(got)
	want := `{"dflt":"dflt","first":20,"greeting":"hello","line":true,"second":21,"spread":{"a":1,"b":[2,3],"extra":5}}`
	if string(data) != want {
		t.Fatalf("result %s, want %s", data, want)
	}
	p, err := Load(Source{Name: "all.js", Text: []byte(text), Dir: dir, Lib: lib})
	if err != nil {
		t.Fatal(err)
	}
	if string(p.MetaJSON) != `{"name":"all-in-one","note":"a } brace and a `+"`"+`tick`+"`"+`","phases":["one","two"]}` {
		t.Errorf("meta %s", p.MetaJSON)
	}
	if len(p.Helpers) != 2 || p.Digest == "" || p.Helpers[0].Digest == "" || len(p.Helpers[1].Files) != 2 {
		t.Errorf("helper digests %+v digest %q", len(p.Helpers), p.Digest)
	}
	if p.Helpers[0].Spec != "crw:lib/words" || p.Helpers[1].Spec != "./util.js" {
		t.Errorf("helper specs %+v", p.Helpers)
	}
}

// A body without imports and a meta of its own (the Claude Workflow subset) runs unchanged, and the body keeps its
// line numbers in error messages.
func TestLoaderBodyKeepsLines(t *testing.T) {
	text := "export const meta = { name: 'x' }\n\n\nconst a = 1\nthrow new Error('line 5 ' + a)\n"
	_, err := runSource(t, Source{Name: "lines.js", Text: []byte(text)}, Limits{})
	if err == nil || !strings.Contains(err.Error(), "line 5 1") {
		t.Fatalf("err %v", err)
	}
	p, _ := Load(Source{Name: "lines.js", Text: []byte(text)})
	rt, _ := p.NewRuntime(Limits{})
	prom, _ := rt.Main()
	rt.Loop.Run(nil)
	stack := prom.Result().ToObject(rt.VM).Get("stack").String()
	if !strings.Contains(stack, "lines.js:5") {
		t.Errorf("stack %q does not name lines.js:5", stack)
	}
}

func TestLoaderRejectsLateImportAndDynamicImport(t *testing.T) {
	meta := "export const meta = { name: 'x' }\n"
	for name, c := range map[string]struct{ text, want string }{
		"import after the header":                 {meta + "const a = 1\nimport { x } from 'crw:lib/schemas'\n", "import after the header"},
		"side-effect import late":                 {meta + "import 'crw:lib/schemas'\n", "import after the header"},
		"import between statements":               {"import { RECEIPT } from 'crw:lib/schemas'\nconst a = 1\n" + meta, "found \"const\""},
		"dynamic import in the body":              {meta + "const m = await import('./x.js')\n", "dynamic import() is not allowed"},
		"dynamic import in template":              {meta + "const s = `${ await import('./x.js') }`\n", "dynamic import() is not allowed"},
		"dynamic import in header":                {"import('./x.js')\n" + meta, "dynamic import() is not allowed"},
		"import.meta":                             {meta + "const u = import.meta.url\n", "import.meta"},
		"export after the header":                 {meta + "export const other = 1\n", "export after the header"},
		"export function late":                    {meta + "export function f() {}\n", "export after the header"},
		"export default":                          {meta + "export default 1\n", "export after the header"},
		"export other than meta":                  {"export const other = 1\n" + meta, "the only export a script may have"},
		"no meta":                                 {"const a = 1\n", "found \"const\""},
		"empty":                                   {"", "no `export const meta"},
		"meta not an object":                      {"export const meta = 'x'\n", "meta must be an object literal"},
		"meta unterminated":                       {"export const meta = { a: 1\n", "not closed"},
		"meta with a call":                        {"export const meta = { a: foo() }\n", "meta:"},
		"meta with a function":                    {"export const meta = { a: () => 1 }\n", "meta must serialise to JSON"},
		"unterminated string":                     {meta + "const s = 'abc\n", "unterminated string"},
		"top-level await in a non-async function": {meta + "function f() { await 1 }\n", "await"},
	} {
		t.Run(name, func(t *testing.T) {
			msg := loadErr(t, Source{Name: "bad.js", Text: []byte(c.text)})
			if !strings.Contains(msg, c.want) {
				t.Errorf("error %q does not contain %q", msg, c.want)
			}
		})
	}
	// A property or key named import/export, and the words inside strings, comments and regexes, are not imports.
	ok := meta + "const o = { import: 1, export: 2 }\nconst s = \"import('x')\" + `export const y` + /import\\(/.source // import('x')\n/* export const z = 1 */\nreturn o.import + o.export + s.length\n"
	if _, err := runSource(t, Source{Name: "ok.js", Text: []byte(ok)}, Limits{}); err != nil {
		t.Fatalf("a body that only mentions import and export in strings: %v", err)
	}
}

func TestLoaderRejectsHelperOutsideWorkflowsDir(t *testing.T) {
	dir := fstest.MapFS{
		"ok.js":       {Data: []byte("export const v = 1\n")},
		"sub/deep.js": {Data: []byte("import { v } from '../ok.js'\nexport const w = v + 1\n")},
		"sub/esc.js":  {Data: []byte("import { x } from '../../outside.js'\nexport const w = x\n")},
		"sub/abs.js":  {Data: []byte("import { x } from '/etc/passwd'\nexport const w = 1\n")},
		"sub/bare.js": {Data: []byte("import fs from 'fs'\nexport const w = fs\n")},
		"sub/url.js":  {Data: []byte("import x from 'https://example.com/x.js'\nexport const w = x\n")},
		"sub/lib.js":  {Data: []byte("import { y } from 'crw:lib/../secret'\nexport const w = y\n")},
	}
	meta := "export const meta = { name: 'x' }\nreturn 1\n"
	for name, c := range map[string]struct{ imp, want string }{
		"parent directory":     {"import { x } from '../outside.js'\n", "leaves the workflows directory"},
		"through a subdir":     {"import { w } from './sub/esc.js'\n", "leaves the workflows directory"},
		"absolute path":        {"import { w } from './sub/abs.js'\n", "is not allowed"},
		"absolute in the head": {"import x from '/etc/passwd'\n", "is not allowed"},
		"bare package":         {"import { w } from './sub/bare.js'\n", "is not allowed"},
		"url":                  {"import { w } from './sub/url.js'\n", "is not allowed"},
		"lib name escape":      {"import { w } from './sub/lib.js'\n", "not a valid library module name"},
		"missing":              {"import { w } from './nope.js'\n", "was not found"},
		"unknown lib":          {"import { w } from 'crw:lib/nope'\n", "was not found"},
		"no directory at all":  {"import { w } from './ok.js'\n", "no workflows directory"},
	} {
		t.Run(name, func(t *testing.T) {
			src := Source{Name: "bad.js", Text: []byte(c.imp + meta), Dir: dir}
			if name == "no directory at all" {
				src.Dir = nil
			}
			msg := loadErr(t, src)
			if !strings.Contains(msg, c.want) {
				t.Errorf("error %q does not contain %q", msg, c.want)
			}
		})
	}
	// A relative import that stays inside the directory, from a subdirectory, resolves.
	good := Source{Name: "good.js", Text: []byte("import { w } from './sub/deep.js'\nexport const meta = { name: 'x' }\nreturn w\n"), Dir: dir}
	got, err := runSource(t, good, Limits{})
	if err != nil || got != int64(2) {
		t.Fatalf("got %v %v", got, err)
	}
}

// A helper with top-level await is rejected by the IIFE bundle, which is the intended error.
func TestLoaderRejectsHelperTopLevelAwait(t *testing.T) {
	dir := fstest.MapFS{"tla.js": {Data: []byte("export const v = await Promise.resolve(1)\n")}}
	msg := loadErr(t, Source{Name: "bad.js", Text: []byte("import { v } from './tla.js'\nexport const meta = { name: 'x' }\n"), Dir: dir})
	if !strings.Contains(msg, "Top-level await") {
		t.Errorf("error %q", msg)
	}
}

func TestLoaderChecksNamedExports(t *testing.T) {
	dir := fstest.MapFS{"h.js": {Data: []byte("export const a = 1\n")}}
	p, err := Load(Source{Name: "bad.js", Text: []byte("import { a, missing } from './h.js'\nexport const meta = { name: 'x' }\n"), Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.NewRuntime(Limits{}); err == nil || !strings.Contains(err.Error(), `no export "missing"`) {
		t.Fatalf("err %v", err)
	}
}

// The digests the run record keeps change with the script and with a helper's sources.
func TestLoaderDigests(t *testing.T) {
	dir := func(v string) fstest.MapFS {
		return fstest.MapFS{"h.js": {Data: []byte("export const a = " + v + "\n")}}
	}
	text := []byte("import { a } from './h.js'\nexport const meta = { name: 'x' }\nreturn a\n")
	p1, _ := Load(Source{Name: "s.js", Text: text, Dir: dir("1")})
	p2, _ := Load(Source{Name: "s.js", Text: text, Dir: dir("1")})
	p3, _ := Load(Source{Name: "s.js", Text: text, Dir: dir("2")})
	if p1.Digest != p2.Digest || p1.Helpers[0].Digest != p2.Helpers[0].Digest {
		t.Error("the same sources must digest the same")
	}
	if p1.Digest != p3.Digest || p1.Helpers[0].Digest == p3.Helpers[0].Digest {
		t.Error("a changed helper must change its digest and not the script's")
	}
}

func TestDeterminismStubsThrow(t *testing.T) {
	for name, c := range map[string]string{
		"Date.now":    "Date.now()",
		"Math.random": "Math.random()",
		"new Date()":  "new Date()",
		"Date()":      "Date()",
	} {
		t.Run(name, func(t *testing.T) {
			text := "export const meta = { name: 'x' }\ntry { " + c + " } catch (e) { return e.name + ': ' + e.message }\nreturn 'no error'\n"
			got, err := runSource(t, Source{Name: "d.js", Text: []byte(text)}, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if s, _ := got.(string); !strings.HasPrefix(s, "DeterminismError: ") || !strings.Contains(s, "crw.clock()") {
				t.Errorf("got %v", got)
			}
		})
	}
	// Dates built from arguments and the static helpers keep working.
	text := `export const meta = { name: 'x' }
const d = new Date(86400000)
return [d.toISOString(), d instanceof Date, Date.UTC(2026, 0, 1), Date.parse('2026-01-01T00:00:00Z'), typeof Math.floor(1.5), new Date(2026, 0, 1).getFullYear(), Object.getPrototypeOf(d) === Date.prototype]
`
	got, err := runSource(t, Source{Name: "d.js", Text: []byte(text)}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(got)
	if string(data) != `["1970-01-02T00:00:00.000Z",true,1767225600000,1767225600000,"number",2026,true]` {
		t.Errorf("got %s", data)
	}
	// There is no fs, net, exec or require.
	probe := "export const meta = { name: 'x' }\nreturn [typeof require, typeof process, typeof fetch, typeof XMLHttpRequest, typeof setTimeout, typeof module, typeof Buffer]\n"
	got, err = runSource(t, Source{Name: "p.js", Text: []byte(probe)}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(got)
	if string(data) != `["undefined","undefined","undefined","undefined","undefined","undefined","undefined"]` {
		t.Errorf("globals %s", data)
	}
}

func TestStepLimitInterrupts(t *testing.T) {
	text := "export const meta = { name: 'x' }\nlet n = 0\nwhile (true) { n++ }\n"
	started := time.Now()
	_, err := runSource(t, Source{Name: "loop.js", Text: []byte(text)}, Limits{Step: 100 * time.Millisecond})
	var le *LimitError
	if !errors.As(err, &le) || le.Kind != "step" {
		t.Fatalf("err %v, want a step LimitError", err)
	}
	if d := time.Since(started); d > 5*time.Second {
		t.Errorf("the interrupt took %v", d)
	}
	// A step that awaits between short stretches is not limited by the sum of them.
	ok := "export const meta = { name: 'x' }\nlet n = 0\nfor (let i = 0; i < 20; i++) { for (let j = 0; j < 2000; j++) n++; await null }\nreturn n\n"
	got, err := runSource(t, Source{Name: "ok.js", Text: []byte(ok)}, Limits{Step: 5 * time.Second})
	if err != nil || got != int64(40000) {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestHeapLimitInterrupts(t *testing.T) {
	text := "export const meta = { name: 'x' }\nconst keep = []\nwhile (true) { keep.push('x'.repeat(1 << 20) + keep.length) }\n"
	_, err := runSource(t, Source{Name: "heap.js", Text: []byte(text)}, Limits{Step: 30 * time.Second, HeapBytes: 64 << 20})
	var le *LimitError
	if !errors.As(err, &le) || le.Kind != "heap" {
		t.Fatalf("err %v, want a heap LimitError", err)
	}
}

func TestSchemaLintStrictMode(t *testing.T) {
	load := func(schemas string) (string, error) {
		lib := fstest.MapFS{"schemas.js": {Data: []byte(schemas)}}
		_, err := Load(Source{Name: "s.js", Text: []byte("import { S } from 'crw:lib/schemas'\nexport const meta = { name: 'x' }\nreturn S\n"), Lib: lib})
		if err != nil {
			return err.Error(), err
		}
		return "", nil
	}
	cases := map[string]struct{ schema, want string }{
		"additionalProperties missing": {`export const S = { type: 'object', properties: { a: { type: 'string' } }, required: ['a'] }`, "needs additionalProperties: false"},
		"additionalProperties true":    {`export const S = { type: 'object', properties: { a: { type: 'string' } }, required: ['a'], additionalProperties: true }`, "needs additionalProperties: false"},
		"property not in required":     {`export const S = { type: 'object', properties: { a: { type: 'string' }, b: { type: 'string' } }, required: ['a'], additionalProperties: false }`, `property "b" is not in required`},
		"required names a ghost":       {`export const S = { type: 'object', properties: { a: { type: 'string' } }, required: ['a', 'g'], additionalProperties: false }`, `required names "g"`},
		"oneOf":                        {`export const S = { type: 'object', properties: { a: { oneOf: [{ type: 'string' }, { type: 'number' }] } }, required: ['a'], additionalProperties: false }`, `keyword "oneOf" is not supported`},
		"allOf":                        {`export const S = { type: 'object', properties: {}, required: [], additionalProperties: false, allOf: [] }`, `keyword "allOf" is not supported`},
		"patternProperties":            {`export const S = { type: 'object', properties: {}, required: [], additionalProperties: false, patternProperties: {} }`, `keyword "patternProperties" is not supported`},
		"nested object":                {`export const S = { type: 'object', properties: { o: { type: 'object', properties: { x: { type: 'string' } }, required: ['x'] } }, required: ['o'], additionalProperties: false }`, "/properties/o: an object needs additionalProperties: false"},
		"array without items":          {`export const S = { type: 'object', properties: { l: { type: 'array' } }, required: ['l'], additionalProperties: false }`, `needs "items"`},
		"root is not an object":        {`export const S = { type: 'array', items: { type: 'string' } }`, `the root must have type "object"`},
		"root anyOf":                   {`export const S = { anyOf: [{ type: 'object', properties: {}, required: [], additionalProperties: false }] }`, "the root"},
		"too deep":                     {`let n = { type: 'string' }; for (let i = 0; i < 12; i++) n = { type: 'object', properties: { c: n }, required: ['c'], additionalProperties: false }; export const S = n`, "nesting deeper than"},
		"dangling ref":                 {`export const S = { type: 'object', properties: { a: { $ref: '#/$defs/nope' } }, required: ['a'], additionalProperties: false }`, "$ref"},
		"one bad among good":           {`export const GOOD = { type: 'object', properties: {}, required: [], additionalProperties: false }; export const S = { type: 'object', properties: { a: { type: 'string' } }, required: [] , additionalProperties: false }`, `property "a" is not in required`},
		"not a schema":                 {`export const S = 'text'`, "the schema must be a JSON object"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			msg, err := load(c.schema)
			if err == nil {
				t.Fatal("a strict-mode violation must be a load error")
			}
			if !strings.Contains(msg, c.want) {
				t.Errorf("error %q does not contain %q", msg, c.want)
			}
		})
	}
	good := `export const S = { type: 'object', properties: { a: { type: ['string', 'null'] }, l: { type: 'array', items: { anyOf: [{ type: 'string' }, { type: 'null' }] } }, r: { $ref: '#/$defs/R' } }, required: ['a', 'l', 'r'], additionalProperties: false, $defs: { R: { type: 'string', enum: ['x', 'y'] } } }`
	_ = good // $defs only on the root of a property schema is valid; the library schemas below are the real corpus.
	if _, err := load(`export const S = { type: 'object', properties: { a: { type: ['string', 'null'] }, n: { type: 'integer', minimum: 0 } }, required: ['a', 'n'], additionalProperties: false }`); err != nil {
		t.Errorf("a strict schema must load: %v", err)
	}
	// Every schema of the embedded library passes, through an import of the module.
	p, err := Load(Source{Name: "lib.js", Text: []byte("import * as all from 'crw:lib/schemas'\nexport const meta = { name: 'x' }\nreturn Object.keys(all).sort()\n")})
	if err != nil {
		t.Fatalf("crw:lib/schemas must pass the strict lint: %v", err)
	}
	rt, err := p.NewRuntime(Limits{})
	if err != nil {
		t.Fatal(err)
	}
	prom, _ := rt.Main()
	rt.Loop.Run(nil)
	data, _ := json.Marshal(prom.Result().Export())
	if string(data) != `["DESIGN","DESIGN_AUDIT","FINDINGS","QA_CAPTURE","QA_PASS","QA_RECEIPT","RECEIPT","REFLECTION","VERDICT","VERDICT_BATCH","VERDICT_FINAL"]` {
		t.Errorf("library schemas %s", data)
	}
}

func TestLintStrictSchemaUnit(t *testing.T) {
	if errs := LintStrictSchemaJSON([]byte(`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":false}`)); len(errs) != 0 {
		t.Errorf("%v", errs)
	}
	errs := LintStrictSchemaJSON([]byte(`{"type":"object","properties":{"a":{"type":"string","default":"x"}},"required":["a"],"additionalProperties":false}`))
	if len(errs) != 1 || !strings.Contains(errs[0], `/properties/a`) {
		t.Errorf("%v", errs)
	}
}
