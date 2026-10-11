package script

import (
	"testing"
	"testing/fstest"
)

// d7: a property call is not a dynamic import whatever separates the dot from the name, and a method may be named import.
func TestBodyAllowsPropertyAndMethodNamedImport(t *testing.T) {
	meta := "export const meta = {}\n"
	for name, body := range map[string]string{
		"comment after the dot": "const o = { import: (x) => x }\nreturn o. /* c */ import(7)\n",
		"newline after the dot": "const o = { import: (x) => x }\nreturn o.\n  import(7)\n",
		"optional chain":        "const o = { import: (x) => x }\nreturn o?.import(7)\n",
		"object method":         "const o = { import(x) { return x } }\nreturn o.import(7)\n",
		"class method":          "class A { import(x) { return x } static export(x) { return x } }\nreturn new A().import(7)\n",
		"async method":          "const o = { async import(x) { return x } }\nreturn await o.import(7)\n",
		"getter":                "const o = { get import() { return 7 } }\nreturn o.import\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := runSource(t, Source{Name: "ok.js", Text: []byte(meta + body)}, Limits{}); err != nil {
				t.Fatalf("%v", err)
			}
		})
	}
	for name, body := range map[string]string{
		"spread":             "return [...import('./x.js')]\n",
		"call":               "return import('./x.js')\n",
		"call in block":      "{ import('./x.js')\n}\n",
		"newline then block": "import('./x.js')\n{ }\n",
		"after comma":        "const a = [1, import('./x.js')]\n",
		"after a dot dot":    "const a = 1 .. import('./x.js')\n",
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			msg := loadErr(t, Source{Name: "bad.js", Text: []byte(meta + body)})
			if msg == "" {
				t.Fatal("no message")
			}
		})
	}
}

// The same reading applies to a helper: a property call or a method named import passes, a spread of a dynamic import
// does not.
func TestHelperDynamicImportReadsPropertiesAndMethods(t *testing.T) {
	main := "import { v } from './h.js'\nexport const meta = {}\nreturn v\n"
	for name, c := range map[string]struct {
		helper string
		ok     bool
	}{
		"comment after the dot": {"const o = { import: (x) => x }\nexport const v = o. /* c */ import(7)\n", true},
		"method":                {"const o = { import(x) { return x } }\nexport const v = o.import(7)\n", true},
		"spread":                {"export const v = [...import('./h.js')]\n", false},
		"call":                  {"export const v = import('./h.js')\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			src := Source{Name: "m.js", Text: []byte(main), Dir: fstest.MapFS{"h.js": {Data: []byte(c.helper)}}}
			_, err := Load(src)
			if (err == nil) != c.ok {
				t.Errorf("Load error %v, want ok=%v", err, c.ok)
			}
		})
	}
}
