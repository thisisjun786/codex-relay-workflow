package script

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
)

const metaLine = "export const meta = { name: 'x' }\n"

// Valid JavaScript with a division is not read as a regular expression, and regular expressions are still found where
// a statement or an operand starts.
func TestLexerSeparatesDivisionFromRegex(t *testing.T) {
	for name, c := range map[string]struct {
		body string
		want any
	}{
		"postfix increment":                      {"let i = 3; return i++ / 2", 1.5},
		"postfix decrement":                      {"let i = 4; return i-- / 2", 2.0},
		"property named in":                      {"const stats = { in: 3, total: 2 }; return stats.in / stats.total", 1.5},
		"property named of":                      {"const s = { of: 6 }; return s.of / 3", 2.0},
		"property named return":                  {"const s = { return: 8 }; return s.return / 4", 2.0},
		"optional chain property":                {"const s = { typeof: 9 }; return s?.typeof / 3", 3.0},
		"after call":                             {"const f = () => 8; return f() / 2", 4.0},
		"after index":                            {"const a = [8]; return a[0] / 2", 4.0},
		"after parenthesis":                      {"return (8) / 2 / 2", 2.0},
		"after number":                           {"return 8 / 2 / 2", 2.0},
		"after object literal":                   {"const o = {}; const d = ({ a: 8 }).a / 2; return d", 4.0},
		"object literal then divide":             {"let x = 8, y = 2; const z = { a: 1 }; return x / y / z.a", 4.0},
		"two divisions on a line":                {"const a = 8, b = 2, c = 2; return a / b / c", 2.0},
		"regex after return":                     {"return /a+/.test('caa')", true},
		"regex after assignment":                 {"const re = /[/]x/g; return re.test('/x')", true},
		"regex after open paren":                 {"return ['a1'].filter((s) => /\\d/.test(s)).length", 1.0},
		"regex after if header":                  {"let n = 0; if (true) /x/.test('x') && n++; return n", 1.0},
		"regex after while header":               {"let n = 0; while (n < 1) /x/.test('x') && n++; return n", 1.0},
		"regex after for header":                 {"let n = 0; for (;n < 1;) /x/.test('x') && n++; return n", 1.0},
		"regex after block":                      {"{ }\n/x/.test('x')\nreturn 1", 1.0},
		"regex after function body":              {"function f() { return 1 }\n/x/.test('x')\nreturn f()", 1.0},
		"regex after else":                       {"let n = 0; if (false) n = 2; else /x/.test('x') && n++; return n", 1.0},
		"regex after colon":                      {"const o = { r: /a/ }; return o.r.source", "a"},
		"regex in template":                      {"return `${/a/.source}`", "a"},
		"division in template":                   {"const a = 8; return `${a / 2}`", "4"},
		"quote in a regex after an if header":    {"let n = 0; if (true) /'/.test(\"'\") && n++; return n", 1.0},
		"backtick in a regex after a block":      {"function f() { return 1 }\n/`/.test('x')\nreturn f()", 1.0},
		"postfix on a property":                  {"const o = { n: 3 }; return o.n++ / 3", 1.0},
		"division after a closed object literal": {"const o = { a: 8 }\nconst d = o.a / 2 / 2; return d", 2.0},
		"regex after comma":                      {"return [/a/, /b/].length", 2.0},
		"regex after typeof":                     {"return typeof /a/", "object"},
		"keyword as object key":                  {"const o = { in: 1, of: 2 }; return o.in + o.of", 3.0},
		"import as a property":                   {"const o = { import: 4 }; return o.import / 2", 2.0},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := runSource(t, Source{Name: "d.js", Text: []byte(metaLine + c.body)}, Limits{})
			if err != nil {
				t.Fatalf("%s: %v", c.body, err)
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("%s = %v (%T), want %v", c.body, got, got, c.want)
			}
		})
	}
	// an expression that really holds an unterminated regular expression is still a load error
	if msg := loadErr(t, Source{Name: "d.js", Text: []byte(metaLine + "const r = /abc\n")}); !strings.Contains(msg, "unterminated regular expression") {
		t.Errorf("%s", msg)
	}
}

// A helper may not reach another module at run time: its dynamic import() is a load error, direct or through another helper.
func TestLoaderRejectsDynamicImportInHelpers(t *testing.T) {
	for name, dir := range map[string]fstest.MapFS{
		"direct":     {"helper.js": {Data: []byte(`export const f = () => import('./other.js')`)}, "other.js": {Data: []byte(`export const v = 1`)}},
		"transitive": {"helper.js": {Data: []byte(`export { f } from './mid.js'`)}, "mid.js": {Data: []byte(`export const f = () => import('./other.js')`)}, "other.js": {Data: []byte(`export const v = 1`)}},
		"computed":   {"helper.js": {Data: []byte(`export const f = (n) => import(n)`)}},
		"escaping":   {"helper.js": {Data: []byte(`export const f = () => import('../x.js')`)}},
	} {
		t.Run(name, func(t *testing.T) {
			msg := loadErr(t, Source{Name: "s.js", Text: []byte("import { f } from './helper.js'\n" + metaLine + "return 1"), Dir: dir})
			if !strings.Contains(msg, "dynamic import") {
				t.Errorf("want a dynamic import error: %s", msg)
			}
		})
	}
	// static imports between helpers stay fine
	dir := fstest.MapFS{"helper.js": {Data: []byte(`import { v } from './other.js'; export const f = () => v`)}, "other.js": {Data: []byte(`export const v = 7`)}}
	got, err := runSource(t, Source{Name: "s.js", Text: []byte("import { f } from './helper.js'\n" + metaLine + "return f()"), Dir: dir}, Limits{})
	if err != nil || fmt.Sprint(got) != "7" {
		t.Errorf("%v %v", got, err)
	}
}

// meta is evaluated under the same determinism stubs as the body: the clock and randomness are not available, a Date
// built from arguments is.
func TestMetaEvaluationIsDeterministic(t *testing.T) {
	for name, c := range map[string]struct {
		meta    string
		wantErr string
	}{
		"Date.now":       {"{ at: Date.now() }", "Date.now()"},
		"Math.random":    {"{ nonce: Math.random() }", "Math.random()"},
		"new Date()":     {"{ at: new Date().toISOString() }", "new Date()"},
		"Date()":         {"{ at: Date() }", "Date()"},
		"explicit date":  {"{ at: new Date(0).toISOString() }", ""},
		"explicit parse": {"{ at: Date.UTC(2026, 0, 1) }", ""},
	} {
		t.Run(name, func(t *testing.T) {
			src := Source{Name: "m.js", Text: []byte("export const meta = " + c.meta + "\nreturn 1")}
			if c.wantErr == "" {
				if _, err := Load(src); err != nil {
					t.Fatal(err)
				}
				return
			}
			if msg := loadErr(t, src); !strings.Contains(msg, "meta:") || !strings.Contains(msg, c.wantErr) {
				t.Errorf("%s", msg)
			}
		})
	}
	p, err := Load(Source{Name: "m.js", Text: []byte("export const meta = { at: new Date(86400000).toISOString() }\nreturn 1")})
	if err != nil || p.Meta["at"] != "1970-01-02T00:00:00.000Z" {
		t.Errorf("%v %v", p, err)
	}
}
