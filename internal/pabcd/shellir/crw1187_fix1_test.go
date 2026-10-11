package shellir

import (
	"strings"
	"testing"
)

// CRW-1187 verification fix round 1 (four P0, one P1).

// P0: the Python source reader splits what the tokenizer splits. Three static spellings of sys.path.insert were missed: an explicit
// line join inside the attribute chain, an alias spelled with characters Python folds to ASCII, and a string with a bracket in it
// before the import statement that names the alias.
func TestCRW1187FixRound1SourceSpellingsThatMoveTheImportPathAreRefused(t *testing.T) {
	for label, body := range map[string]string{
		"line join in the chain":     "import sys\nsys.\\\npath.insert(0, LIB)\nimport evil\n",
		"line join, CRLF":            "import sys\r\nsys.\\\r\npath.insert(0, LIB)\r\nimport evil\r\n",
		"line join before the dot":   "import sys\nsys\\\n.path.insert(0, LIB)\nimport evil\n",
		"comment inside brackets":    "import sys\n(sys.  # note\n path).insert(0, LIB)\nimport evil\n",
		"folded alias":               "import sys as \U0001d4fc\n\U0001d4fc.path.insert(0, LIB)\nimport evil\n",
		"fullwidth name":             "import sys\n\uff53ys.path.insert(0, LIB)\nimport evil\n",
		"non-ASCII name in a from":   "from sys import path as \u1d56\nimport evil\n",
		"bracket in a string":        "note = '('\nimport sys as s\ns.path.insert(0, LIB)\nimport evil\n",
		"closing bracket in string":  "note = ')]}'\nimport sys as s\ns.path.insert(0, LIB)\nimport evil\n",
		"quote in a comment":         "# it's\nimport sys as s\ns.path.insert(0, LIB)\nimport evil\n",
		"triple string with bracket": "note = '''(\n'''\nimport sys as s\ns.path.insert(0, LIB)\nimport evil\n",
		"colon glued to the import":  "if True:import sys as s\ns.path.insert(0, LIB)\nimport evil\n",
		"alias after a lambda":       "f = lambda: 0\nimport sys as s\ns.path.insert(0, LIB)\nimport evil\n",
		"semicolons":                 "x = 1; import sys as s; s.path.insert(0, LIB); import evil\n",
		"continued import":           "import os, \\\n    sys as s\ns.path.insert(0, LIB)\nimport evil\n",
		"bracketed import":           "from sys import (\n    path as p,\n)\np.insert(0, LIB)\nimport evil\n",
		"alias in a compound header": "while False: import sys as s\ns.path.insert(0, LIB)\nimport evil\n",
		"class body alias":           "class A:\n    import sys as s\n    s.path.insert(0, LIB)\nimport evil\n",
		"sys as an attribute":        "import os\nos.sys.path.insert(0, LIB)\nimport evil\n",
		"sys from another module":    "from os import sys as s\ns.path.insert(0, LIB)\nimport evil\n",
		"sys kept in a name":         "import os\ns = os.sys\ns.path.insert(0, LIB)\nimport evil\n",
		"attribute in an f-string":   "import os\nx = f\"{os.sys}\"\nimport evil\n",
		"attribute in a raw f":       "import os\nx = rf'{os.sys}'\nimport evil\n",
		"string prefix then alias":   "x = r'\\''\nimport sys as s\ns.path.insert(0, LIB)\nimport evil\n",
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

// What stays allowed: ordinary sources, with strings, comments and non-ASCII text in the places a name cannot be.
func TestCRW1187FixRound1OrdinarySourcesWithStringsAndCommentsStayAllowed(t *testing.T) {
	for label, body := range map[string]string{
		"non-ASCII string":          "import unittest\nGREETING = '\uc548\ub155 (\ud14c\uc2a4\ud2b8)'\n",
		"non-ASCII comment":         "import unittest\n# \ud55c\uae00 \uc8fc\uc11d (\n\nx = 1\n",
		"non-ASCII docstring":       "\"\"\"\ud14c\uc2a4\ud2b8 (\"\"\"\nimport unittest\n",
		"byte order mark":           "\ufeffimport unittest\nx = 1\n",
		"bracket in a string":       "import unittest\nnote = '('\nvalue = ')'\nimport calc\n",
		"line join in a call":       "import unittest\nvalue = max(1, \\\n    2)\n",
		"argv and exit":             "import sys\nimport unittest\nif len(sys.argv) > 3:\n    sys.exit(1)\n",
		"dict and slices":           "import unittest\nd = {1: 2}\nx = [1, 2, 3][1:2]\n",
		"annotations and lambda":    "import unittest\nx: int = 3\nf = lambda a: a + 1\n",
		"import_module by name":     "import importlib\nm = importlib.import_module('calc')\n",
		"patch target string":       "import unittest\nfrom unittest import mock\n@mock.patch('app.sys.argv', ['x'])\ndef f(): pass\n",
		"dotted name in a string":   "import unittest\nNAME = 'a.sys'\nDOC = \"\"\"os.sys is not used\"\"\"\n",
		"names that start with sys": "import unittest\nx = obj.sysinfo + obj.sys_version\n",
	} {
		if what := moduleSourceRefusal(body); what != "" {
			t.Errorf("%s: refused: %s", label, what)
		}
	}
}

// P0: sort writes the file of -o/--output, and a sort in the pipeline of the module run (beside it) creates a module it imports.
func TestCRW1187FixRound1SortOutputBesideTheModuleRunIsRefused(t *testing.T) {
	cwd := crw1187Project(t, "import unittest\n", map[string]string{"payload.txt": "x = 1\n"})
	for _, cmd := range []string{
		"python3 -B -m unittest | sort payload.txt -o late.py",
		"python3 -B -m unittest | sort -o late.py payload.txt",
		"python3 -B -m unittest | sort -olate.py payload.txt",
		"python3 -B -m unittest | sort -ro late.py payload.txt",
		"python3 -B -m unittest | sort --output late.py payload.txt",
		"python3 -B -m unittest | sort --output=late.py payload.txt",
		"python3 -B -m unittest | sort --out=late.py payload.txt",
		"python3 -B -m unittest | sort --o late.py payload.txt",
		"python3 -B -m unittest | sort -o late.pyc payload.txt",
		"python3 -B -m unittest | sort -o __pycache__/late.cpython-314.pyc payload.txt",
		"python3 -B -m unittest | sort -o \"$OUT\" payload.txt",
		"python3 -B -m unittest | sort -k 1 -o late.py payload.txt",
		"python3 -B -m unittest | tee out.log | sort -o late.py payload.txt",
		"sort payload.txt -o late.py | python3 -B -m unittest",
		"sort -o late.py payload.txt & python3 -B -m unittest",
		"python3 -B -m unittest & sort -o late.py payload.txt",
		// before the run, in the same text: the destination is a code path
		"sort -o late.py payload.txt; python3 -B -m unittest",
		"python3 -B -m json.tool d.json | sort -o late.py payload.txt",
		"python3 -I -B -m json.tool d.json | sort -o late.py payload.txt",
		"python3 -B -m unittest | uniq payload.txt late.py",
		"python3 -B -m unittest | split payload.txt late.py",
	} {
		if reason := crw1187Reason(t, cmd, cwd); reason == "" {
			t.Errorf("%s: allowed", cmd)
		}
	}
	for _, cmd := range []string{
		"python3 -B -m unittest | sort -o result.txt",
		"python3 -B -m unittest | sort",
		"python3 -B -m unittest | sort payload.txt",
		"python3 -B -m unittest | sort -k 1 payload.txt | tee out.log",
		"python3 -B -m unittest; sort -o late.py payload.txt",
		"python3 -B -m unittest && sort -o late.py payload.txt",
		"sort -o result.txt payload.txt; python3 -B -m unittest",
	} {
		if reason := crw1187Reason(t, cmd, cwd); reason != "" {
			t.Errorf("%s: refused: %s", cmd, reason)
		}
	}
}

// P0: -P keeps PYTHONPATH. A reading that is given no environment cannot say it is unset, so it proves nothing for -P; the guard that
// has the environment answers (the GitHub guard reads the hook's own PYTHONPATH now, see the hook tests). -I stays safe either way.
func TestCRW1187FixRound1NoEnvironmentProvesNothingForP(t *testing.T) {
	more := map[string]string{"keyword.py": "x = 1\n", "d.json": "{}"}
	// (-EP is not here: -E ignores PYTHONPATH, so it needs no environment, see TestCRW1187FixRound3EIgnoresPythonPathForP)
	for _, cmd := range []string{"python3 -P -B -m json.tool d.json", "python3 -P -m json.tool d.json"} {
		cwd := crw1187Project(t, "x = 1\n", more)
		if reason := crw1187ReasonEnv(t, cmd, cwd, nil); reason == "" {
			t.Errorf("%s: allowed with no environment", cmd)
		}
		unset := func(string) (string, bool) { return "", false }
		if reason := crw1187ReasonEnv(t, cmd, cwd, unset); reason != "" {
			t.Errorf("%s: refused with PYTHONPATH unset: %s", cmd, reason)
		}
		empty := func(k string) (string, bool) { return "", k == "PYTHONPATH" }
		if reason := crw1187ReasonEnv(t, cmd, cwd, empty); reason != "" {
			t.Errorf("%s: refused with PYTHONPATH empty: %s", cmd, reason)
		}
		set := func(k string) (string, bool) { return ".", k == "PYTHONPATH" }
		if reason := crw1187ReasonEnv(t, cmd, cwd, set); reason == "" {
			t.Errorf("%s: allowed with PYTHONPATH=.", cmd)
		}
	}
	cwd := crw1187Project(t, "x = 1\n", more)
	if reason := crw1187ReasonEnv(t, "python3 -I -B -m json.tool d.json", cwd, nil); reason != "" {
		t.Errorf("-I with no environment: refused: %s", reason)
	}
}

// P1: the isolated exception holds for the names of the fixed list too (json, py_compile, os, re ...), and for packages of those names.
func TestCRW1187FixRound1IsolatedRunIgnoresShadowsOfFixedNames(t *testing.T) {
	shadows := map[string]string{
		"json.py":                 "raise SystemExit('shadow')\n",
		"json/__init__.py":        "raise SystemExit('shadow')\n",
		"py_compile.py":           "raise SystemExit('shadow')\n",
		"os.pyc":                  "x",
		"argparse.cpython-314.so": "x",
		"re.py":                   "raise SystemExit('shadow')\n",
		"keyword.py":              "raise SystemExit('shadow')\n",
		"sitecustomize.py":        "raise SystemExit('shadow')\n",
	}
	for name, body := range shadows {
		for _, cmd := range []string{"python3 -I -B -m json.tool d.json", "python3 -I -m py_compile app.py", "python3 -P -B -m json.tool d.json", "python3 -EP -B -m py_compile app.py"} {
			cwd := crw1187Project(t, "x = 1\n", map[string]string{name: body, "d.json": "{}", "app.py": "x = 1\n"})
			if reason := crw1187Reason(t, cmd, cwd); reason != "" {
				t.Errorf("%s with %s: refused: %s", cmd, name, reason)
			}
			// without the flag the shadow is imported
			plain := strings.NewReplacer("-I ", "", "-P ", "", "-EP ", "").Replace(cmd)
			if reason := crw1187Reason(t, plain, cwd); reason == "" {
				t.Errorf("%s with %s: allowed", plain, name)
			}
		}
	}
	// the isolation does not excuse a writer beside the run, nor a unittest run
	cwd := crw1187Project(t, "import unittest\n", map[string]string{"json.py": "x = 1\n", "d.json": "{}", "payload.txt": "x = 1\n"})
	if reason := crw1187Reason(t, "python3 -I -B -m json.tool d.json | sort -o late.py payload.txt", cwd); reason == "" {
		t.Error("an isolated run with a writer beside it was allowed")
	}
	if reason := crw1187Reason(t, "python3 -I -B -m unittest", cwd); reason == "" {
		t.Error("a unittest run with a shadow of json was allowed")
	}
}
