package shellir

import (
	"strings"
	"testing"
)

// CRW-1187 verification of 6be86fca: a star import after a semicolon or a compound header rebinds a class too, and a def of a method
// named open is no call of open.

// P1: a star import of an archive module after a semicolon, after a compound header (if, else, try) or as a relative import rebinds the
// class whose extract was taken for its own method, as one on a line of its own does: the reader refuses each.
func TestCRW1187FixRound6StarImportAfterASemicolonOrHeader(t *testing.T) {
	for label, stmt := range map[string]string{
		"standalone":         "from zipfile import *",
		"semicolon":          "import os; from zipfile import *",
		"compound header":    "if True: from zipfile import *",
		"else header":        "if False: pass\nelse: from zipfile import *",
		"try header":         "try: from zipfile import *\nexcept ImportError: pass",
		"semicolon in body":  "if True: x = 1; from zipfile import *",
		"no space before *":  "import os;from zipfile import*",
		"relative, no space": "from.zipfile import *",
	} {
		body := "import zipfile\nclass ZipFile:\n    def extract(self, name):\n        return name\n" + stmt + "\nZipFile('p.zip').extract('late.py')\nimport late\n"
		if what := moduleSourceRefusal(body); what == "" {
			t.Errorf("%s: the source was accepted:\n%s", label, body)
		}
		cwd := crw1187Project(t, body, nil)
		if reason := crw1187Reason(t, "python3 -B -m unittest", cwd); !strings.Contains(reason, "module source test_calc.py") {
			t.Errorf("%s: the module run was not refused by the reader: %q", label, reason)
		}
	}
	// a star import in a string, or a name that only begins with from, proves nothing against the class
	for label, stmt := range map[string]string{
		"star import in a string": "DOC = 'x; from zipfile import *'",
		"name beginning from":     "fromage = 1; from_x = 2",
	} {
		body := "NAME = 'calc.py'\nclass Reader:\n    def extract(self, name):\n        return name\n" + stmt + "\nReader().extract('calc.py')\nimport unittest\n"
		if what := moduleSourceRefusal(body); what != "" {
			t.Errorf("%s: refused: %s", label, what)
		}
	}
}

// P2: a def (or an async def) of a method named open declares it and opens nothing: its formal arguments are no mode. A call of open
// beside it is still judged by its operands, a def whose default value opens for writing is refused, and a def word in a comment ahead
// of a line join proves nothing.
func TestCRW1187FixRound6OpenDeclarationIsNoCall(t *testing.T) {
	for label, body := range map[string]string{
		"unused definition":       "NAME='calc.py'\nclass Reader:\n    def open(self, path, mode):\n        return None\nimport unittest\n",
		"async definition":        "NAME='calc.py'\nclass Reader:\n    async def open(self, path, mode):\n        return None\nimport unittest\n",
		"module-level definition": "NAME='calc.py'\ndef open(path, mode, *args, **kw):\n    return None\nimport unittest\n",
		"definition, tab":         "NAME='calc.py'\nclass Reader:\n\tdef\topen (self, path, mode):\n\t\treturn None\nimport unittest\n",
	} {
		if what := moduleSourceRefusal(body); what != "" {
			t.Errorf("%s: refused: %s", label, what)
		}
		cwd := crw1187Project(t, body, nil)
		if reason := crw1187Reason(t, "python3 -B -m unittest", cwd); reason != "" {
			t.Errorf("%s: the module run was refused: %s", label, reason)
		}
	}
	for label, body := range map[string]string{
		"call beside the definition": "class Reader:\n    def open(self, path, mode):\n        return None\nM = 'w'\nopen('late.py', M).write(PAYLOAD)\nimport late\n",
		"nested filename call":       "class Reader:\n    def open(self, path, mode):\n        return None\ndef name():\n    return 'late.py'\nopen(name(), 'w').write(PAYLOAD)\nimport late\n",
		"default value opens":        "NAME = 'late.py'\ndef open(p, f=open(NAME, 'w')):\n    return None\nimport late\n",
		"def in a comment, join":     "NAME = 'late.py'\nM = 'w'\nx = 1 # def \\\nopen(NAME, M).write(PAYLOAD)\nimport late\n",
		"def word, other name":       "NAME = 'late.py'\nM = 'w'\nundef = 1\nundef ;open(NAME, M).write(PAYLOAD)\nimport late\n",
	} {
		if what := moduleSourceRefusal(body); what == "" {
			t.Errorf("%s: the source was accepted:\n%s", label, body)
		}
	}
}
