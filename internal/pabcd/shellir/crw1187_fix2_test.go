package shellir

import "testing"

// CRW-1187 verification fix round 2 (five P0): static spellings that reach sys.path or a dynamic import and were missed.
func TestCRW1187FixRound2SourceSpellingsThatMoveTheImportPathAreRefused(t *testing.T) {
	for label, body := range map[string]string{
		// sys in brackets, then an attribute
		"parenthesized sys":       "import sys\n(sys).path.insert(0, LIB)\nimport evil\n",
		"spaced brackets":         "import sys\n( sys ) . path.insert(0, LIB)\nimport evil\n",
		"nested brackets":         "import sys\n((sys)).path.insert(0, LIB)\nimport evil\n",
		"sys in a list":           "import sys\n[sys][0].path.insert(0, LIB)\nimport evil\n",
		"sys in a tuple":          "import sys\n(sys,)[0].path.insert(0, LIB)\nimport evil\n",
		"alias in brackets":       "import sys as s\n(s).path.insert(0, LIB)\nimport evil\n",
		"walrus in brackets":      "import sys\n(s := sys).path.insert(0, LIB)\nimport evil\n",
		"getattr of bracketed":    "import sys\ngetattr((sys), 'pa' + 'th').insert(0, LIB)\nimport evil\n",
		"sys or a value":          "import sys\n(sys or 0).path.insert(0, LIB)\nimport evil\n",
		"sys in a dict":           "import sys\n{'k': sys}['k'].path.insert(0, LIB)\nimport evil\n",
		"sys first in a tuple":    "import sys\n(sys, 0)[0].path.insert(0, LIB)\nimport evil\n",
		"sys across lines":        "import sys\n(sys\n).path.insert(0, LIB)\nimport evil\n",
		"getattr of __builtins__": "getattr(__builtins__, '__imp' + 'ort__')('sy' + 's').path.insert(0, LIB)\nimport evil\n",
		"importlib in brackets":   "import importlib\n(importlib).util.spec_from_file_location\n",
		// a from-import alias of __import__
		"builtins alias":          "from builtins import __import__ as f\nf('sy' + 's').path.insert(0, LIB)\nimport evil\n",
		"builtins alias in group": "from builtins import (print, __import__ as f)\nf('sy' + 's').path.insert(0, LIB)\nimport evil\n",
		"builtins alias listed":   "from builtins import print, __import__ as f\nf('sy' + 's').path.insert(0, LIB)\nimport evil\n",
		"importlib __import__":    "from importlib import __import__ as f\nf('sy' + 's').path.insert(0, LIB)\nimport evil\n",
		"builtins getattr alias":  "import sys\nfrom builtins import getattr as g\ng(sys, 'pa' + 'th').insert(0, LIB)\nimport evil\n",
		// a reference to __import__ outside an import statement
		"parenthesized import":  "(__import__)('sy' + 's').path.insert(0, LIB)\nimport evil\n",
		"import in a list":      "[__import__][0]('sy' + 's').path.insert(0, LIB)\nimport evil\n",
		"import as an argument": "f = max(__import__, key=id)\nf('sy' + 's').path.insert(0, LIB)\nimport evil\n",
		"import in a tuple":     "f, = (__import__,)\nf('sy' + 's').path.insert(0, LIB)\nimport evil\n",
		// a name Python folds to sys inside the expression of an f-string (or a t-string)
		"fullwidth in an f":    "import sys\nx = f\"{ｓｙｓ.path.insert(0, LIB)}\"\nimport evil\n",
		"fullwidth in a raw f": "import sys\nx = rf'{ｓｙｓ.path.insert(0, LIB)}'\nimport evil\n",
		"fullwidth in a t":     "import sys\nx = t\"{ｓｙｓ.path.insert(0, LIB)}\"\nimport evil\n",
		"fullwidth in a spec":  "import sys\nx = f\"{1:{ｓｙｓ.path.insert(0, LIB)}}\"\nimport evil\n",
		"fullwidth nested f":   "import sys\nx = f\"{f'{ｓｙｓ.path.insert(0, LIB)}'}\"\nimport evil\n",
		"fullwidth triple f":   "import sys\nx = f\"\"\"{\nｓｙｓ.path.insert(0, LIB)}\"\"\"\nimport evil\n",
		// PEP 701: the quote of the f-string inside its expression
		"same quote in field":     "import os\nd = {'k': 1}\nf\"{d[\"k\"]}{(s:=os.sys)}\"\ns.path.insert(0, LIB)\nimport evil\n",
		"same single quote":       "import os\nd = {'k': 1}\nf'{d['k']}{(s:=os.sys)}'\ns.path.insert(0, LIB)\nimport evil\n",
		"same triple quote":       "import os\nd = {'k': 1}\nf\"\"\"{d[\"\"\"k\"\"\"]}{(s:=os.sys)}\"\"\"\ns.path.insert(0, LIB)\nimport evil\n",
		"comment in a field":      "import os\nx = f\"{1 # \"\n}{(s:=os.sys)}\"\ns.path.insert(0, LIB)\nimport evil\n",
		"keyword before a string": "import os\nif\"{\": x = 1  # \"\nｓ = 1\nimport evil\n",
	} {
		if what := moduleSourceRefusal(body); what == "" {
			t.Errorf("%s: the source was accepted:\n%s", label, body)
		}
		cwd := crw1187Project(t, body, nil)
		if reason := crw1187Reason(t, "python3 -B -m unittest", cwd); reason == "" {
			t.Errorf("%s: the module run was allowed", label)
		}
	}
}

// What stays allowed: f-strings with non-ASCII text and nested quotes, sys passed as an argument, and the plain import forms.
func TestCRW1187FixRound2OrdinarySourcesStayAllowed(t *testing.T) {
	for label, body := range map[string]string{
		"non-ASCII f literal":     "import unittest\nname = 'x'\nmsg = f\"안녕 {name}\"\n",
		"non-ASCII nested string": "import unittest\nmsg = f\"{'안녕'}\"\n",
		"format spec":             "import unittest\nx, w = 1, 4\nmsg = f\"{x!r:>{w}} {{braces}}\"\n",
		"PEP 701 nested quote":    "import unittest\nd = {'k': 1}\nmsg = f\"{d[\"k\"]}\"\n",
		"argv in an f":            "import sys\nimport unittest\nmsg = f\"{len(sys.argv)}\"\n",
		"keyword before a string": "import unittest\nx = 'a'\nif\"a\" in x: pass\n",
		"argv and path calls":     "import os\nimport sys\nimport unittest\nprint(sys.argv, os.path.join('a', 'b'))\nx = os.path.dirname(sys.executable)\n",
		"sys in a call":           "import sys\nimport unittest\nprint('x', file=(sys.stderr))\n",
		"from builtins print":     "from builtins import print as p\nimport unittest\n",
		"import grouped":          "from importlib import (import_module,\n    reload)\nm = import_module('calc')\n",
		"import_module call":      "import importlib\nm = importlib.import_module('calc')\n",
		"dunder import plain":     "m = __import__('calc')\n",
	} {
		if what := moduleSourceRefusal(body); what != "" {
			t.Errorf("%s: refused: %s", label, what)
		}
	}
}
