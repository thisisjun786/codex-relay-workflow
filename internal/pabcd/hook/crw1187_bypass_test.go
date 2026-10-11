package hook

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
)

// CRW-1187 against the real interpreter. Each source below is a test module that loads code from outside the project (a module in
// LIB beside it, or compiled code, or a file it copies in). The test runs it with python3 -B -m unittest in a temporary home; where the
// code outside ran (it writes a marker), both guards must have refused the same run beforehand. A mechanism that does not work with
// the host interpreter is reported and not counted.
func TestCRW1187EveryWorkingImportMechanismIsRefusedByBothGuards(t *testing.T) {
	python, bash := crw1178Python(t)
	prelude := "import unittest\nimport calc\nclass T(unittest.TestCase):\n    def test_x(self):\n        self.assertEqual(calc.add(1, 2), 3)\n"
	sources := map[string]string{
		"sys.path.insert":    "import sys\nsys.path.insert(0, LIB)\nimport evil\n",
		"sys.path.append":    "import sys\nsys.path.append(LIB)\nimport evil\n",
		"sys.path.extend":    "import sys\nsys.path.extend([LIB])\nimport evil\n",
		"sys.path +=":        "import sys\nsys.path += [LIB]\nimport evil\n",
		"sys.path =":         "import sys\nsys.path = sys.path + [LIB]\nimport evil\n",
		"sys.path slice":     "import sys\nsys.path[0:0] = [LIB]\nimport evil\n",
		"alias":              "import sys as s\ns.path.insert(0, LIB)\nimport evil\n",
		"from sys import":    "from sys import path\npath.insert(0, LIB)\nimport evil\n",
		"from sys import as": "from sys import path as p\np.append(LIB)\nimport evil\n",
		"getattr":            "import sys\ngetattr(sys, 'pa' + 'th').insert(0, LIB)\nimport evil\n",
		"vars":               "import sys\nvars(sys)['pa' + 'th'].insert(0, LIB)\nimport evil\n",
		"sys.__dict__":       "import sys\nsys.__dict__['pa' + 'th'].insert(0, LIB)\nimport evil\n",
		"__import__":         "__import__('s' + 'ys').path.insert(0, LIB)\nimport evil\n",
		"import_module":      "import importlib\nimportlib.import_module('s' + 'ys').path.insert(0, LIB)\nimport evil\n",
		"import_module sys":  "import importlib\nimportlib.import_module('sys').path.insert(0, LIB)\nimport evil\n",
		"meta_path":          "import sys, importlib.abc\nclass F(importlib.abc.MetaPathFinder):\n    def find_spec(self, name, path, target=None):\n        return None\nsys.meta_path.append(F())\nimport evil\n",
		"spec_from_file":     "import importlib.util\nspec = importlib.util.spec_from_file_location('evil', LIB + '/evil.py')\nm = importlib.util.module_from_spec(spec)\nspec.loader.exec_module(m)\n",
		"sourceless":         "from importlib.machinery import SourcelessFileLoader as L\nL('evilc', LIB + '/evilc.pyc').load_module()\n",
		"machinery getattr":  "import importlib\nm = getattr(importlib, 'mach' + 'inery')\nm.SourcelessFileLoader('evilc', LIB + '/evilc.pyc').load_module()\n",
		"importlib dict":     "import importlib\nm = importlib.__dict__['mach' + 'inery']\nm.SourcelessFileLoader('evilc', LIB + '/evilc.pyc').load_module()\n",
		"importlib attr":     "import importlib\nimportlib.machinery.SourcelessFileLoader('evilc', LIB + '/evilc.pyc').load_module()\n",
		"copy then import":   "import shutil\nshutil.copy(LIB + '/evil.py', 'evil.py')\nimport evil\n",
		"open write":         "open('evil.py', 'w').write(open(LIB + '/evil.py').read())\nimport evil\n",
		"write_text":         "import pathlib\npathlib.Path('evil.py').write_text(pathlib.Path(LIB, 'evil.py').read_text())\nimport evil\n",
		"symlink":            "import os\nos.symlink(LIB + '/evil.py', 'evil.py')\nimport evil\n",
		"hardlink":           "import os\nos.link(LIB + '/evil.py', 'evil.py')\nimport evil\n",
		"rename":             "import os\nos.rename(LIB + '/evil.py', 'evil.py')\nimport evil\n",
		"pyc copy":           "import shutil\nshutil.copy(LIB + '/evilc.pyc', 'evilc.pyc')\nimport evilc\n",
		"run_path":           "import runpy\nrunpy.run_path(LIB + '/evil.py')\n",
		"chdir":              "import os\nos.chdir(LIB)\nimport evil\n",
		"__path__":           "import calc\ncalc.__path__ = [LIB]\n",
		"package __path__":   "import pkg\npkg.__path__.append(LIB)\nimport pkg.evil\n",
		"sys.path_hooks":     "import sys\nsys.path_hooks.insert(0, lambda p: (_ for _ in ()).throw(ImportError))\nsys.path.insert(0, LIB)\nimport evil\n",
		"addsitedir":         "import site\nsite.addsitedir(LIB)\nimport evil\n",
		"environment":        "import os, subprocess, sys\nsubprocess.run([sys.executable, '-c', 'import evil'], env={'PYTHONPATH': LIB})\n",
		"pycache_prefix":     "import sys\nsys.pycache_prefix = LIB + '/pfx'\nimport calc\n",
		"globals import":     "g = globals()\ng['__builtins__']['__import__']('sys').path.insert(0, LIB)\nimport evil\n",
		"builtins import":    "import builtins\ngetattr(builtins, '__imp' + 'ort__')('s' + 'ys').path.insert(0, LIB)\nimport evil\n",
		"exec string":        "exec('import sys; sys.path.insert(0, LIB)')\nimport evil\n",
	}
	// Code the source builds or reads at run time and runs with exec is the reader's general limit (the GitHub post guard does not
	// refuse exec in any program, with or without a module run), not an import mechanism of this issue: reported, not asserted.
	general := map[string]string{
		"exec of a file": "exec(compile(open(LIB + '/evil.py').read(), 'evil', 'exec'))\n",
		"exec pieces":    "exec('import s' + 'ys; sys.pa' + 'th.insert(0, LIB)')\nimport evil\n",
	}
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	var worked, unworked, missed, generalOpen []string
	for name, src := range general {
		sources[name] = src
	}
	general2 := map[string]bool{}
	for name := range general {
		general2[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cwd, root, env := gateScene(t)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		lib := filepath.Join(filepath.Dir(cwd), "lib")
		marker := filepath.Join(root, "injected.md")
		src := "LIB = " + strconv.Quote(lib) + "\n" + sources[name]
		crw1187Write(t, lib, map[string]string{"evil.py": "open(" + strconv.Quote(marker) + ", 'w').write('unread code ran')\n"})
		crw1187Write(t, cwd, map[string]string{"calc.py": "def add(a, b):\n    return a + b\n", "test_calc.py": prelude + src, "pkg/__init__.py": ""})
		setup := "import py_compile,sys\npy_compile.compile(sys.argv[1] + '/evil.py', cfile=sys.argv[1] + '/evilc.pyc', doraise=True)\n"
		if out, err := crw1187Isolated(t, python, "-B", "-c", setup, lib).CombinedOutput(); err != nil {
			t.Fatalf("setup: %v %s", err, out)
		}
		cmd := "python3 -B -m unittest"
		crw1178Run(t, bash, python, cwd, filepath.Dir(root), cmd)
		_, ran := os.Stat(marker)
		if ran != nil {
			unworked = append(unworked, name)
			continue
		}
		worked = append(worked, name)
		mem, gh := crw1187Both(t, cwd, env, cmd)
		if (mem == "" || gh == "") && general2[name] {
			generalOpen = append(generalOpen, name+" (memory gate refused: "+boolWord(mem != "")+", GitHub guard refused: "+boolWord(gh != "")+")")
			continue
		}
		if mem == "" || gh == "" {
			missed = append(missed, name+" (memory gate refused: "+boolWord(mem != "")+", GitHub guard refused: "+boolWord(gh != "")+")")
		}
	}
	t.Logf("working mechanisms (%d): %v", len(worked), worked)
	t.Logf("mechanisms that did not load the code on this interpreter (%d): %v", len(unworked), unworked)
	t.Logf("the general limit, not asserted: %v", generalOpen)
	if len(worked) == 0 {
		t.Fatal("no mechanism worked, so this test proves nothing")
	}
	for _, m := range missed {
		t.Errorf("a working mechanism ran code outside the inventory and a guard allowed it: %s", m)
	}
}

func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
