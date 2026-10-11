package script

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/evanw/esbuild/pkg/api"
)

// The closing brace of a function or class expression ends an operand, so a slash after it divides; the closing brace
// of a declaration ends a statement, so a slash after it starts a regular expression.
func TestLexerTellsExpressionBodiesFromDeclarations(t *testing.T) {
	for name, c := range map[string]struct {
		body string
		want any
	}{
		"function expression then divide":          {"const f = function() {} / 2; return Number.isNaN(f)", true},
		"named function expression then divide":    {"const f = function g(a) { return a } / 2; return Number.isNaN(f)", true},
		"async function expression then divide":    {"const f = async function() {} / 2; return Number.isNaN(f)", true},
		"generator expression then divide":         {"const f = function* () {} / 2; return Number.isNaN(f)", true},
		"class expression then divide":             {"const f = class {} / 2; return Number.isNaN(f)", true},
		"class expression with heritage":           {"class B {}\nconst f = class extends B { m() { return 1 } } / 2; return Number.isNaN(f)", true},
		"function expression argument then divide": {"const g = (x) => x; return Number.isNaN(g(function () { return 1 } / 2))", true},
		"function expression in template":          {"return `${function () {} / 2}`", "NaN"},
		"returned function expression divided":     {"return Number.isNaN((() => { return function () {} / 2 })())", true},
		"regex after function declaration":         {"function f() { return 1 }\n/x/.test('x')\nreturn f()", 1.0},
		"regex after async function declaration":   {"async function f() { return 1 }\n/x/.test('x')\nreturn 1", 1.0},
		"regex after class declaration":            {"class A { m() { return 1 } }\n/x/.test('x')\nreturn new A().m()", 1.0},
		"regex after class declaration heritage":   {"class A {}\nclass B extends A {}\n/x/.test('x')\nreturn 1", 1.0},
		"regex after function in block":            {"{ function f() {} }\n/x/.test('x')\nreturn 1", 1.0},
		"regex after method body":                  {"const o = { m() { return 2 } }\nreturn [o.m(), /x/.source].join('')", "2x"},
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
}

// A helper whose source the import check cannot read is a load error: the check fails closed, so a computed import()
// that the bundle would drop as unused never slips past it.
func TestHelperImportCheckFailsClosed(t *testing.T) {
	dir := fstest.MapFS{"helper.js": {Data: []byte("export const f = () => 1; const x = function () {} / 2; const unused = () => import(name);\n")}}
	msg := loadErr(t, Source{Name: "s.js", Text: []byte("import { f } from './helper.js'\n" + metaLine + "return f()"), Dir: dir})
	if !strings.Contains(msg, "dynamic import") {
		t.Errorf("want a dynamic import error: %s", msg)
	}
	// the helper loader refuses a source it cannot tokenize instead of skipping the check
	b := &bundler{files: map[string][]byte{nsDir + ":h.js": []byte("export const f = 1\n/* never closed\n")}}
	_, err := b.load(api.OnLoadArgs{Path: "h.js", Namespace: nsDir})
	if err == nil || !strings.Contains(err.Error(), "unterminated comment") {
		t.Errorf("load of an unreadable helper: %v", err)
	}
}
