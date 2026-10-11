package shellir

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// crw1187Project is a two-file project whose test module has the given text.
func crw1187Project(t *testing.T, testSource string, more map[string]string) string {
	t.Helper()
	cwd := t.TempDir()
	files := map[string]string{"calc.py": "def add(a, b):\n    return a + b\n", "test_calc.py": testSource}
	for k, v := range more {
		files[k] = v
	}
	for name, body := range files {
		p := filepath.Join(cwd, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cwd
}

func crw1187Reason(t *testing.T, cmd, cwd string) string {
	t.Helper()
	// -P keeps PYTHONPATH, so a run with it is judged with an environment that is known and holds none of the PYTHON* variables; a
	// reading that is given no environment proves nothing for -P (TestCRW1187FixRound1NoEnvironmentProvesNothingForP).
	if strings.Contains(cmd, " -P") || strings.Contains(cmd, " -IP") || strings.Contains(cmd, " -EP") {
		return crw1187ReasonEnv(t, cmd, cwd, func(string) (string, bool) { return "", false })
	}
	_, err := Analyze(cmd, cwd)
	var u *Unreadable
	if !errors.As(err, &u) {
		return ""
	}
	return u.Reason
}

func crw1187ReasonEnv(t *testing.T, cmd, cwd string, lookup func(string) (string, bool)) string {
	t.Helper()
	_, err := AnalyzeEnv(cmd, cwd, lookup)
	var u *Unreadable
	if !errors.As(err, &u) {
		return ""
	}
	return u.Reason
}

// (1) json.tool and py_compile read no inventory: every importable name of the standard library in the directory is a shadow, as a
// source, a compiled file, an extension or a package, and not only the fixed list (3.14 imports keyword, reprlib, fnmatch, dis,
// weakref, _colorize, annotationlib, compression and textwrap after it sets sys.path[0]).
func TestCRW1187ShadowOfAnyStdlibNameRefusedForJsonToolAndPyCompile(t *testing.T) {
	shadows := map[string]string{
		"keyword.py":  "x = 1\n",
		"reprlib.pyc": "x",
		"fnmatch.cpython-314-x86_64-linux-gnu.so": "x",
		"dis.pyd":                 "x",
		"weakref/__init__.py":     "x = 1\n",
		"_colorize.py":            "x = 1\n",
		"annotationlib.py":        "x = 1\n",
		"compression/__init__.py": "x = 1\n",
		"textwrap.py":             "x = 1\n",
		"_futurekind.py":          "x = 1\n", // any private name: a version may add one
	}
	for name, body := range shadows {
		for _, cmd := range []string{"python3 -m json.tool d.json", "python3 -B -m json.tool d.json", "python3 -E -s -m json.tool d.json", "python3 -m py_compile app.py", "python3 -m py_compile app.py other.py"} {
			cwd := crw1187Project(t, "x = 1\n", map[string]string{name: body, "d.json": "{}", "app.py": "x = 1\n", "other.py": "y = 2\n"})
			reason := crw1187Reason(t, cmd, cwd)
			base := filepath.Base(name)
			if strings.HasPrefix(name, "weakref/") || strings.HasPrefix(name, "compression/") {
				base = filepath.Dir(name)
			}
			if reason == "" || !strings.Contains(reason, base) || !strings.Contains(reason, "python -I") {
				t.Errorf("%s with %s: want a refusal that names the file and python -I, got %q", cmd, name, reason)
			}
		}
	}
}

// -I leaves the directory out of sys.path, and -P does too while PYTHONPATH stays out of it; both are allowed, and so is a directory
// with no shadow, a file named like no module and an operand of py_compile.
func TestCRW1187SafePathAndPlainRunsStayAllowed(t *testing.T) {
	more := map[string]string{"keyword.py": "x = 1\n", "d.json": "{}", "app.py": "x = 1\n"}
	for _, cmd := range []string{
		"python3 -I -m json.tool d.json",
		"python3 -I -B -m py_compile app.py",
		"python3 -P -m json.tool d.json",
		"python3 -P -B -m py_compile app.py",
		"python3 -IP -m json.tool d.json",
		"python3 -E -P -m json.tool d.json",
	} {
		cwd := crw1187Project(t, "x = 1\n", more)
		if reason := crw1187Reason(t, cmd, cwd); reason != "" {
			t.Errorf("%s: refused: %s", cmd, reason)
		}
	}
	for _, cmd := range []string{"python3 -m json.tool d.json", "python3 -B -m py_compile app.py", "python3 -m py_compile app.py"} {
		cwd := crw1187Project(t, "x = 1\n", map[string]string{"d.json": "{}", "app.py": "x = 1\n", "notes.txt": "x", "keyword.txt": "x", "keywords.py": "x = 1\n", "__init__.py": "", "__main__.py": ""})
		if reason := crw1187Reason(t, cmd, cwd); reason != "" {
			t.Errorf("%s with no shadow: refused: %s", cmd, reason)
		}
	}
}

// -P keeps PYTHONPATH: when the text or the environment can set it, the working directory may still be on the path, so the run is
// judged as without -P. The same for a name built at run time. -I ignores PYTHON* variables, so it stays allowed.
func TestCRW1187PythonPathKeepsTheDirectoryOnTheSearchPath(t *testing.T) {
	more := map[string]string{"keyword.py": "x = 1\n", "d.json": "{}"}
	for _, cmd := range []string{
		"PYTHONPATH=. python3 -P -m json.tool d.json",
		"export PYTHONPATH=.; python3 -P -m json.tool d.json",
		"env PYTHONPATH=. python3 -P -m json.tool d.json",
		"set -a; N=PYTHONPATH; : ${!N:=.}; python3 -P -m json.tool d.json",
		"python3 -P -m json.tool d.json # PYTHONPATH",
	} {
		cwd := crw1187Project(t, "x = 1\n", more)
		if reason := crw1187Reason(t, cmd, cwd); reason == "" {
			t.Errorf("%s: allowed although PYTHONPATH may name the directory", cmd)
		}
	}
	cwd := crw1187Project(t, "x = 1\n", more)
	env := func(k string) (string, bool) {
		if k == "PYTHONPATH" {
			return ".", true
		}
		return "", false
	}
	if _, err := AnalyzeEnv("python3 -P -m json.tool d.json", cwd, env); err == nil {
		t.Error("-P with PYTHONPATH in the environment was allowed")
	}
	if _, err := AnalyzeEnv("python3 -I -m json.tool d.json", cwd, env); err != nil {
		t.Errorf("-I with PYTHONPATH in the environment was refused: %v", err)
	}
}

// The list is checked against the interpreter on the host: a module name its sys.stdlib_module_names holds that the reader does not
// refuse as a shadow fails here, so a newer interpreter cannot outrun the list unnoticed.
func TestCRW1187StdlibListCoversTheHostInterpreter(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	cmd := exec.Command(python, "-I", "-c", "import sys; print('\\n'.join(sorted(set(sys.stdlib_module_names) | set(sys.builtin_module_names))))")
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("python3 cannot list its modules: %v", err)
	}
	var missing []string
	for _, n := range strings.Fields(string(out)) {
		if !stdlibShadowName(n) {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("stdlibModuleNames lacks %v of the host interpreter %s", missing, python)
	}
}

// (2) A writer that runs beside the module (the other stage of its pipeline, a background job or a coprocess on either side, a
// process substitution) may create the module a test imports later. Writers after the module has ended stay allowed.
func TestCRW1187WriterBesideTheModuleRunIsRefused(t *testing.T) {
	for _, cmd := range []string{
		"python3 -B -m unittest | tee test_late.py",
		"python3 -B -m unittest 2>&1 | tee -a late.py",
		"python3 -B -m unittest | cp x.txt late.py",
		"python3 -B -m unittest | sh -c 'printf x > late.py'",
		"python3 -B -m unittest | python3 -c 'open(\"late.py\",\"w\")'",
		"python3 -B -m unittest | cat > late.py",
		"python3 -B -m unittest | cat > \"$OUT\"",
		"python3 -B -m unittest | cat | tee late.py",
		"python3 -B -m unittest | { cat; printf x > late.py; }",
		"python3 -B -m unittest | (cat; printf x > late.py)",
		"(printf x > late.py) | python3 -B -m unittest",
		"(sleep 1; printf x > late.py) & python3 -B -m unittest",
		"(sleep 1; printf x > late.py) & echo; python3 -B -m unittest",
		"python3 -B -m unittest; (sleep 1; printf x > late.py) &",
		"{ sleep 1; printf x > late.py; } & python3 -B -m unittest",
		"printf x > late.py & python3 -B -m unittest",
		"coproc { sleep 1; printf x > late.py; }; python3 -B -m unittest",
		"coproc W { printf x > late.py; }; python3 -B -m unittest",
		"python3 -B -m unittest; coproc { printf x > late.py; }",
		"python3 -B -m unittest <(printf x > late.py)",
		"python3 -B -m unittest -v & cat",
	} {
		cwd := crw1187Project(t, "import unittest\nimport calc\n", nil)
		_, err := Analyze(cmd, cwd)
		if err == nil {
			t.Errorf("%s: a writer beside the module run was allowed", cmd)
		}
	}
	// Reasons say what runs beside what.
	cwd := crw1187Project(t, "import unittest\nimport calc\n", nil)
	reason := crw1187Reason(t, "python3 -B -m unittest | tee late.py", cwd)
	if !strings.Contains(reason, "beside the module run") || !strings.Contains(reason, "separate command") {
		t.Errorf("reason: %q", reason)
	}
}

func TestCRW1187SequentialAndHarmlessCompanionsStayAllowed(t *testing.T) {
	for _, cmd := range []string{
		"python3 -B -m unittest",
		"python3 -B -m unittest | tee out.log",
		"python3 -B -m unittest 2>&1 | tail -5",
		"python3 -B -m unittest | cat",
		"python3 -B -m unittest | head -20 | wc -l",
		"python3 -B -m unittest > out.log 2>&1",
		"python3 -B -m unittest; printf x > late.py",
		"python3 -B -m unittest && printf x > late.py",
		"python3 -B -m unittest; cp x.txt late.py",
		"python3 -B -m unittest; sh -c 'printf x > late.py'",
		"sleep 1 & python3 -B -m unittest",
		"printf x | tee out.txt | cat; python3 -B -m unittest",
		"echo a | cat; python3 -B -m unittest | cat",
	} {
		cwd := crw1187Project(t, "import unittest\nimport calc\n", map[string]string{"x.txt": "x"})
		if _, err := Analyze(cmd, cwd); err != nil {
			t.Errorf("%s: refused: %v", cmd, err)
		}
	}
}

// (3) A source that changes where the interpreter finds code, loads a file by its path, or writes a module file is refused, whatever
// the startup flags: -I and -E -B do not change what the test module does itself.
func TestCRW1187SourceThatMovesTheImportsIsRefused(t *testing.T) {
	cases := map[string]string{
		"sys.path.insert":      "import sys\nsys.path.insert(0, 'lib')\nimport helper\n",
		"sys.path.append":      "import sys\nsys.path.append('lib')\n",
		"sys.path.extend":      "import sys\nsys.path.extend(['lib'])\n",
		"sys.path assignment":  "import sys\nsys.path = ['lib'] + sys.path\n",
		"sys.path +=":          "import sys\nsys.path += ['lib']\n",
		"sys.path slice":       "import sys\nsys.path[0:0] = ['lib']\n",
		"sys.path read":        "import sys\nprint(sys.path)\n",
		"sys . path spaced":    "import sys\nsys . path . insert(0, 'lib')\n",
		"sys.pycache_prefix":   "import sys\nsys.pycache_prefix = '/tmp/pfx'\nimport calc\n",
		"sys.meta_path":        "import sys\nsys.meta_path.insert(0, object())\n",
		"sys.path_hooks":       "import sys\nsys.path_hooks.append(object())\n",
		"alias import":         "import sys as s\ns.path.insert(0, 'lib')\n",
		"alias in a list":      "import os, sys as sy\nsy.path.insert(0, 'lib')\n",
		"alias in try":         "try: import sys as s\nexcept ImportError: pass\ns.path.insert(0, 'lib')\n",
		"from sys import path": "from sys import path\npath.insert(0, 'lib')\n",
		"from sys as":          "from sys import path as p\np.insert(0, 'lib')\n",
		"from sys list":        "from sys import argv, path\npath.insert(0, 'lib')\n",
		"from sys paren":       "from sys import (\n    argv,\n    path,\n)\npath.insert(0, 'lib')\n",
		"from sys backslash":   "from sys import argv, \\\n    pycache_prefix\n",
		"from sys star":        "from sys import *\n",
		"semicolon":            "import os; from sys import path\n",
		"getattr":              "import sys\ngetattr(sys, 'path').insert(0, 'lib')\n",
		"getattr alias":        "import sys as s\nsetattr(s, 'pycache_prefix', 'x')\n",
		"vars":                 "import sys\nvars(sys)['path'] = []\n",
		"__dict__":             "import sys\nsys.__dict__['path'] = []\n",
		"__import__ sys":       "__import__('sys').path.insert(0, 'lib')\n",
		"import_module sys":    "import importlib\nimportlib.import_module(\"sys\").path.insert(0, 'lib')\n",
		"SourcelessFileLoader": "import importlib.machinery as m\nm.SourcelessFileLoader('x', 'x.pyc').load_module()\n",
		"loader from import":   "from importlib.machinery import SourcelessFileLoader as L\nL('x', 'x.pyc')\n",
		"loader alias bare":    "from importlib import machinery as mm\nmm.SourcelessFileLoader\n",
		"spec_from_file":       "import importlib.util\nspec = importlib.util.spec_from_file_location('m', '/x/m.py')\n",
		"from importlib.util":  "from importlib.util import spec_from_file_location as f\nf('m', 'm.py')\n",
		"from importlib util":  "from importlib import util\nutil.find_spec('x')\n",
		"importlib.machinery":  "import importlib\nimportlib.machinery.PathFinder\n",
		"imp":                  "import imp\nimp.load_source('m', 'm.py')\n",
		"from imp":             "from imp import load_compiled\n",
		"runpy.run_path":       "import runpy\nrunpy.run_path('x.py')\n",
		"zipimport":            "import zipimport\nzipimport.zipimporter('x.zip')\n",
		"marshal":              "import marshal\nexec(marshal.loads(b''))\n",
		"copy pyc then import": "import shutil\nshutil.copy('a.pyc', 'b.pyc')\nimport b\n",
		"copyfile":             "from shutil import copyfile\ncopyfile('a.txt', 'helper.py')\n",
		"from shutil import":   "from shutil import copy\ncopy('a.txt', 'helper.py')\n",
		"from shutil paren":    "from shutil import (\n    copy,\n)\ncopy('a.txt', 'helper.py')\n",
		"os.replace":           "import os\nos.replace('a.txt', 'helper.py')\n",
		"os.symlink":           "import os\nos.symlink('/x/m.py', 'helper.py')\n",
		"write_text":           "import pathlib\npathlib.Path('helper.py').write_text('x = 1')\nimport helper\n",
		"open w":               "open('helper.py', 'w').write('x = 1')\nimport helper\n",
		"open wb pyc":          "with open('helper.pyc', 'wb') as f:\n    f.write(b'')\n",
		"open mode kw":         "open('helper.py', mode='w')\n",
		"open append":          "open(\"__pycache__/x.cpython-314.pyc\", 'ab')\n",
		"so write":             "import shutil\nshutil.move('a.bin', 'ext.so')\n",
		"pth":                  "import shutil\nshutil.copyfile('a', 'x.pth')\n",
		"env PYTHONPATH":       "import os\nos.environ['PYTHONPATH'] = 'lib'\n",
		"env prefix":           "import os\nos.environ['PYTHONPYCACHEPREFIX'] = 'lib'\n",
		"addsitedir":           "import site\nsite.addsitedir('lib')\n",
		"compileall":           "import compileall\ncompileall.compile_dir('.')\n",
		"py_compile":           "import py_compile\npy_compile.compile('x.py')\n",
		"__import__ built":     "__import__('s' + 'ys').path.insert(0, 'lib')\n",
		"__import__ variable":  "name = 'sys'\n__import__(name)\n",
		"import_module built":  "import importlib\nimportlib.import_module('s' + 'ys')\n",
		"import_module fstr":   "import importlib\nx = 'sys'\nimportlib.import_module(f'{x}')\n",
		"import_module alias":  "from importlib import import_module as im\nim('s' + 'ys')\n",
		"import_module paren":  "from importlib import (\n    reload,\n    import_module as im,\n)\nim(name)\n",
		"__import__ reference": "f = __import__\nf('sys')\n",
		"__import__ assigned":  "import builtins\nbuiltins.__import__ = lambda *a: None\n",
		"builtins getattr":     "import builtins\ngetattr(builtins, '__imp' + 'ort__')\n",
		"builtins alias":       "import builtins as b\nvars(b)['x'] = 1\n",
		"importlib getattr":    "import importlib\ngetattr(importlib, 'mach' + 'inery')\n",
		"importlib dict":       "import importlib\nimportlib.__dict__['mach' + 'inery']\n",
		"importlib alias attr": "import importlib as il\nil.machinery.PathFinder\n",
		"importlib abc":        "import importlib\nimportlib.abc.MetaPathFinder\n",
		"__path__":             "import calc\ncalc.__path__ = ['lib']\n",
		"extend_path":          "import pkgutil\n__path__ = pkgutil.extend_path(__path__, __name__)\n",
		"search locations":     "import calc\ncalc.__spec__.submodule_search_locations.append('lib')\n",
	}
	for name, src := range cases {
		for _, cmd := range []string{"python3 -B -m unittest", "python3 -I -B -m unittest", "python3 -E -B -m unittest", "python3 -B -m pytest"} {
			cwd := crw1187Project(t, src, nil)
			reason := crw1187Reason(t, cmd, cwd)
			if reason == "" {
				t.Errorf("%s / %s: allowed", name, cmd)
				continue
			}
			if !strings.Contains(reason, "test_calc.py") || !strings.Contains(reason, "cannot read what it loads") || strings.Contains(reason, cwd) {
				t.Errorf("%s / %s: the reason lacks the file or the cause, or holds the host path: %s", name, cmd, reason)
			}
		}
	}
}

// The same constructs in the other sources a run reads: a package below the project, the conftest files of pytest, a helper module.
func TestCRW1187SourceInOtherInventoryFilesIsRefused(t *testing.T) {
	bad := "import sys\nsys.path.insert(0, 'lib')\n"
	for where, more := range map[string]map[string]string{
		"helper module":    {"helper.py": bad},
		"package init":     {"pkg/__init__.py": bad},
		"deep test":        {"tests/unit/test_deep.py": bad},
		"conftest":         {"conftest.py": bad},
		"nested conftest":  {"tests/conftest.py": bad},
		"setup in package": {"pkg/sub/mod.py": "from importlib.machinery import SourcelessFileLoader\n"},
	} {
		for _, cmd := range []string{"python3 -B -m unittest", "python3 -B -m pytest"} {
			cwd := crw1187Project(t, "import unittest\nimport calc\n", more)
			reason := crw1187Reason(t, cmd, cwd)
			if reason == "" {
				if strings.Contains(where, "conftest") && cmd == "python3 -B -m unittest" {
					continue // unittest never reads a conftest
				}
				t.Errorf("%s / %s: allowed", where, cmd)
				continue
			}
			if !strings.Contains(reason, ".py") || strings.Contains(reason, cwd) {
				t.Errorf("%s / %s: reason: %s", where, cmd, reason)
			}
		}
	}
	// An ancestor's conftest is read for pytest too.
	parent := t.TempDir()
	child := filepath.Join(parent, "proj")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "conftest.py"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "test_a.py"), []byte("x = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if reason := crw1187Reason(t, "python3 -B -m pytest", child); reason == "" {
		t.Error("an ancestor conftest that moves sys.path was allowed")
	}
}

// Ordinary test sources stay allowed: imports, importlib.import_module and reload, sys.argv, sys.exit, sys.modules, sys.executable,
// sys.stdout, files that are not modules, copy.deepcopy and str.replace next to a literal that ends in .py.
func TestCRW1187OrdinarySourcesStayAllowed(t *testing.T) {
	cases := map[string]string{
		"plain":                  "import unittest\nimport calc\nclass T(unittest.TestCase):\n    def test_add(self):\n        self.assertEqual(calc.add(1, 2), 3)\n",
		"sys basics":             "import sys\nprint(sys.argv, sys.executable, sys.version_info, sys.platform)\nsys.stdout.write('x')\nif len(sys.argv) > 3:\n    sys.exit(1)\n",
		"sys modules":            "import sys\nsys.modules['fake'] = object()\nassert 'calc' in sys.modules or True\n",
		"from sys":               "from sys import argv, exit, modules, stdout\n",
		"importlib":              "import importlib\nm = importlib.import_module('calc')\nimportlib.reload(m)\n",
		"data writes":            "import shutil, os\nopen('out.txt', 'w').write('x')\nshutil.copy('a.txt', 'b.txt')\nos.replace('a.txt', 'c.txt')\n",
		"replace on py":          "name = 'calc.py'\nbase = name.replace('.py', '')\nimport copy\nx = copy.deepcopy([name])\n",
		"read of py":             "text = open('calc.py').read()\nopen('calc.py', 'r').close()\n",
		"mock path":              "import unittest.mock\nwith unittest.mock.patch('sys.stdout'):\n    pass\n",
		"os.path":                "import os\nprint(os.path.join('a', 'b.py'), os.getcwd())\n",
		"pathlib":                "import pathlib\nprint(pathlib.Path('x.py').exists())\n",
		"word path":              "path = 'x'\nsyspath = 1\nsys_path = 2\nimport os\nos.path.exists(path)\n",
		"import by name":         "import importlib\nm = importlib.import_module('calc')\nn = __import__('calc')\nimportlib.reload(m)\nimportlib.invalidate_caches()\n",
		"import_module from":     "from importlib import import_module\nm = import_module(\"calc\")\nprint(m)\n",
		"import_module list":     "from importlib import (\n    import_module,\n    reload,\n)\nreload(import_module('calc'))\n",
		"import_module alias ok": "from importlib import import_module as im\nim('calc')\n",
		"importlib data":         "import importlib.resources\nimport importlib.metadata\nprint(importlib.resources.files('calc'), importlib.metadata.version('x'))\n",
		"builtins open":          "import builtins\nfrom unittest import mock\nwith mock.patch.object(builtins, 'open'):\n    pass\n",
	}
	for name, src := range cases {
		for _, cmd := range []string{"python3 -B -m unittest", "python3 -I -B -m unittest", "python3 -E -B -m unittest", "python3 -B -m pytest"} {
			cwd := crw1187Project(t, src, map[string]string{"a.txt": "x"})
			if reason := crw1187Reason(t, cmd, cwd); reason != "" {
				t.Errorf("%s / %s: refused: %s", name, cmd, reason)
			}
		}
	}
	// py_compile compiles its operands and imports nothing from them: their text is not judged.
	cwd := crw1187Project(t, "x = 1\n", map[string]string{"tool.py": "import sys\nsys.path.insert(0, 'lib')\n"})
	if reason := crw1187Reason(t, "python3 -B -m py_compile tool.py", cwd); reason != "" {
		t.Errorf("py_compile of a file that sets sys.path was refused: %s", reason)
	}
}

// The runs the decision keeps: an ordinary run without a cache, -B after a separate removal of the cache, -E -B, and a module with the
// reader's other options.
func TestCRW1187KeptRunsStayAllowed(t *testing.T) {
	cwd := crw1187Project(t, "import unittest\nimport calc\n", nil)
	for _, cmd := range []string{
		"python3 -m unittest",
		"python3 -B -m unittest",
		"python3 -E -B -m unittest",
		"python3 -B -m pytest",
		"rm -rf __pycache__; python3 -B -m unittest",
		"python3 -B -m unittest -v test_calc | tee out.log",
		"python3 -m json.tool d.json",
	} {
		if reason := crw1187Reason(t, cmd, cwd); reason != "" {
			t.Errorf("%s: refused: %s", cmd, reason)
		}
	}
}
