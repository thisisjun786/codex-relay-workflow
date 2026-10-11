package shellir

import (
	"strings"
	"testing"
)

// CRW-1187 verification of 73f063e9: a path object is not a literal, and extract is judged by its receiver.

// P1: a call of Path, PurePath, PosixPath or x.Path is not a string literal: the source may define or rebind the name (def Path returning
// a code name), so the operand is one the reader cannot evaluate and the call keeps the rule it had: refused beside a code name.
func TestCRW1187FixRound5PathCallsAreNotLiterals(t *testing.T) {
	for label, body := range map[string]string{
		"spoofed def Path":         "import shutil as sh\ndef Path(name):\n    return 'late.py'\nsh.copyfile('payload.txt', Path('result.txt'))\nimport late\n",
		"spoofed lambda Path":      "import shutil as sh\nPath = lambda name: 'late.py'\nsh.copyfile('payload.txt', Path('result.txt'))\nimport late\n",
		"pathlib Path rebound":     "import shutil as sh\nfrom pathlib import Path\nPath = lambda name: 'late.py'\nsh.copyfile('payload.txt', Path('result.txt'))\nimport late\n",
		"pathlib.Path attribute":   "import shutil as sh\nimport pathlib\npathlib.Path = lambda name: 'late.py'\nsh.copyfile('payload.txt', pathlib.Path('result.txt'))\nimport late\n",
		"spoofed PurePath":         "import os\nclass PurePath(str):\n    def __new__(cls, n):\n        return str.__new__(cls, 'late.py')\nos.rename('payload.txt', PurePath('result.txt'))\nimport late\n",
		"spoofed PosixPath source": "import shutil\ndef PosixPath(n):\n    return 'late.py'\nshutil.copy('result.txt', PosixPath('x.txt'))\n",
		"attribute of an object":   "import shutil as sh\nsh.copy('payload.txt', obj.Path('result.txt'))\nimport late\nNAME = 'late.py'\n",
		"Path operands, code name": "import shutil as sh\nfrom pathlib import Path\nNAME = 'calc.py'\nsh.copyfile(Path('data.txt'), Path('out/result.txt'))\nimport unittest\n",
		"open of a Path":           "def Path(n):\n    return 'late.py'\nopen(Path('result.txt'), 'w').write(PAYLOAD)\nimport late\n",
		"extract into a Path":      "import zipfile\nNAME = 'x.py'\nzipfile.ZipFile('p.zip').extract('data.txt', Path('out.d'))\n",
		"open of a call, write":    "def name():\n    return 'late.py'\nopen(name(), 'w').write(PAYLOAD)\nimport late\n",
		"open, mode a name":        "M = 'w'\nopen('late.py', M).write(PAYLOAD)\nimport late\n",
		"method open, mode a name": "from pathlib import Path\nM = 'w'\nPath('late.py').open(M).write(PAYLOAD)\nimport late\n",
	} {
		what := moduleSourceRefusal(body)
		if what == "" {
			t.Errorf("%s: the source was accepted:\n%s", label, body)
		}
		cwd := crw1187Project(t, body, nil)
		reason := crw1187Reason(t, "python3 -B -m unittest", cwd)
		if reason == "" || !strings.Contains(reason, "module source test_calc.py") {
			t.Errorf("%s: the module run was not refused by the reader: %q", label, reason)
		}
	}
	// plain literals stay judged as before, and an open for reading of any operand writes nothing
	for label, body := range map[string]string{
		"literal operands":     "import shutil as sh\nNAME = 'calc.py'\nsh.copyfile('data.txt', 'out/result.txt')\nimport unittest\n",
		"open of a call, read": "import os\nNAME = 'calc.py'\nwith open(os.path.join('d', 'data.txt')) as f:\n    f.read()\nimport unittest\n",
		"open of a call, 'r'":  "NAME = 'calc.py'\nopen(where('data.txt'), 'r').read()\nimport unittest\n",
		"method open, read":    "from pathlib import Path\nNAME = 'calc.py'\nPath('data.txt').open('rb').read()\nimport unittest\n",
	} {
		if what := moduleSourceRefusal(body); what != "" {
			t.Errorf("%s: refused: %s", label, what)
		}
	}
}

// P1: a method named extract on an instance of a plain class the source defines (no base, no decorator, its own undecorated extract,
// nothing that hands back another object or another attribute) writes nothing, whatever archive module or word and whatever code name
// the source holds; an archive's extract, an unknown receiver beside an archive and a receiver the reader cannot prove plain stay refused.
func TestCRW1187FixRound5ExtractIsJudgedByItsReceiver(t *testing.T) {
	for label, body := range map[string]string{
		"unrelated archive import":    "import zipfile\nNAME='calc.py'\nclass Reader:\n    def extract(self):\n        return 1\nReader().extract()\nimport unittest\n",
		"ordinary metadata operand":   "class Reader:\n    def extract(self, name):\n        return name.rsplit('.', 1)[0]\nReader().extract('calc.py')\nimport unittest\n",
		"archive word in a string":    "FORMAT='zipfile'\nNAME='calc.py'\nclass Reader:\n    def extract(self):\n        return 1\nReader().extract()\nimport unittest\n",
		"object base, tarfile import": "import tarfile\nclass Reader(object):\n    def __init__(self, n):\n        self.n = n\n    def extract(self, name):\n        return name\nReader('x').extract('late.py')\nimport unittest\n",
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
		"archive base":             "import zipfile\nclass Reader(zipfile.ZipFile):\n    pass\nReader('p.zip').extract('late.py')\n",
		"archive base, no operand": "import zipfile\nNAME = 'late.py'\nclass Reader(zipfile.ZipFile):\n    def extract(self, m=NAME):\n        return super().extract(m)\nReader('p.zip').extract()\n",
		"__new__ returns archive":  "import zipfile\nNAME = 'late.py'\nclass Reader:\n    def __new__(cls):\n        return zipfile.ZipFile('p.zip')\n    def extract(self, m):\n        return 1\nReader().extract(NAME)\n",
		"no own extract":           "import zipfile\nNAME = 'late.py'\nclass Reader:\n    def __getattr__(self, a):\n        return getattr(zipfile.ZipFile('p.zip'), a)\nReader().extract(NAME)\n",
		"extract assigned":         "import zipfile\nNAME = 'late.py'\nclass Reader:\n    def extract(self, m):\n        return 1\nReader.extract = zipfile.ZipFile('p.zip').extract\nReader().extract(NAME)\n",
		"extract set by name":      "import zipfile\nNAME = 'late.py'\nclass Reader:\n    def extract(self, m):\n        return 1\nsetattr(Reader, 'extract', zz)\nReader().extract(NAME)\n",
		"decorated extract":        "import zipfile\nNAME = 'late.py'\nclass Reader:\n    @staticmethod\n    def extract(m):\n        return 1\nReader().extract(NAME)\n",
		"decorated class":          "import zipfile\nNAME = 'late.py'\n@archive\nclass Reader:\n    def extract(self, m):\n        return 1\nReader().extract(NAME)\n",
		"class rebound":            "import zipfile\nNAME = 'late.py'\nclass Reader:\n    def extract(self, m):\n        return 1\nReader = zipfile.ZipFile\nReader('p.zip').extract(NAME)\n",
		"class bound by with":      "import zipfile\nNAME = 'late.py'\nclass Reader:\n    def extract(self, m):\n        return 1\nwith zipfile.ZipFile('p.zip') as Reader:\n    pass\nReader().extract(NAME)\n",
		"class by a string":        "import zipfile\nNAME = 'late.py'\nclass Reader:\n    def extract(self, m):\n        return 1\nglobals()['Reader'] = zz\nReader().extract(NAME)\n",
		"metaclass":                "import zipfile\nNAME = 'late.py'\nclass Reader(metaclass=M):\n    def extract(self, m):\n        return 1\nReader().extract(NAME)\n",
		"build_class hook":         "import zipfile, builtins\nNAME = 'late.py'\nbuiltins.__build_class__ = zz\nclass Reader:\n    def extract(self, m):\n        return 1\nReader().extract(NAME)\n",
		"two class headers":        "import zipfile\nNAME = 'late.py'\nclass Reader:\n    def extract(self, m):\n        return 1\nclass Reader(zipfile.ZipFile):\n    pass\nReader('p.zip').extract(NAME)\n",
		"receiver an attribute":    "import zipfile\nNAME = 'late.py'\nclass Reader:\n    def extract(self, m):\n        return 1\nmod.Reader().extract(NAME)\n",
		"unknown receiver":         "import zipfile\nNAME = 'calc.py'\nzf.extract(member)\n",
		"unknown, code member":     "NAME = 'calc.py'\nfixture.extract('late.py')\n",
		"chained receiver":         "import zipfile\nNAME = 'late.py'\nclass Reader:\n    def extract(self, m):\n        return 1\n    def zip(self):\n        return zipfile.ZipFile('p.zip')\nReader().zip().extract(NAME)\n",
	} {
		if what := moduleSourceRefusal(body); what == "" {
			t.Errorf("%s: the source was accepted:\n%s", label, body)
		}
	}
}

// Another source of the run can rebind the class whose extract was proven plain (an attribute of the test module, a monkeypatch target,
// type(x).extract): the run is refused then, and allowed when no other source can.
func TestCRW1187FixRound5PlainReceiverHeldAcrossTheInventory(t *testing.T) {
	const body = "class Reader:\n    def extract(self, m):\n        return 1\ndef test_x():\n    Reader().extract('late.py')\nimport unittest\n"
	for label, helper := range map[string]string{
		"module attribute":    "import zipfile\nimport test_calc\ntest_calc.Reader = lambda: zipfile.ZipFile('p.zip')\n",
		"monkeypatch by name": "def fixture(monkeypatch):\n    monkeypatch.setattr('test_calc.Reader', ZZ)\n",
		"type(x).extract":     "import zipfile\ndef patch(x):\n    type(x).extract = zipfile.ZipFile('p.zip').extract\n",
		"__class__ swap":      "import zipfile\ndef patch(x):\n    x.__class__ = zipfile.ZipFile\n",
		"imported class":      "import zipfile\nfrom test_calc import Reader\nReader.extract = zz\n",
	} {
		if what := moduleSourceRefusal(body); what != "" {
			t.Fatalf("the source alone was refused: %s", what)
		}
		cwd := crw1187Project(t, body, map[string]string{"helper.py": helper})
		reason := crw1187Reason(t, "python3 -B -m unittest", cwd)
		if !strings.Contains(reason, "test_calc.py calls extract on Reader(), which helper.py may rebind") {
			t.Errorf("%s: the run was not refused for the other source: %q", label, reason)
		}
	}
	cwd := crw1187Project(t, body, map[string]string{"helper.py": "def mul(a, b):\n    return a * b\n"})
	if reason := crw1187Reason(t, "python3 -B -m unittest", cwd); reason != "" {
		t.Errorf("an unrelated helper made the run refused: %s", reason)
	}
}

// The proof of a plain receiver holds against what binds the class, its method or the call's receiver without the reader's per-line
// view seeing it: a decorator over several lines (or a lambda), a name after a dot on the line before (inside a bracket or after a line
// join), a star import that rebinds the class, a decorated extract whose decorator spans lines, and, from another source of the run,
// attributes set through __setattr__, the code or the globals of the method.
func TestCRW1187FixRound5PlainReceiverProofHolds(t *testing.T) {
	for label, body := range map[string]string{
		"decorator over lines":       "import zipfile\nNAME = 'late.py'\n@(lambda c: lambda: zipfile.ZipFile('p.zip')\n)\nclass Reader:\n    def extract(self, m):\n        return 1\nReader().extract(NAME)\nimport late\n",
		"decorator call over lines":  "import zipfile\nNAME = 'late.py'\n@swap(\n    zipfile.ZipFile)\nclass Reader:\n    def extract(self, m):\n        return 1\nReader().extract(NAME)\n",
		"lambda decorator":           "import zipfile\nNAME = 'late.py'\n@lambda c: lambda: zipfile.ZipFile('p.zip')\nclass Reader:\n    def extract(self, m):\n        return 1\nReader().extract(NAME)\n",
		"attribute across a newline": "import zipfile\nclass ZipFile:\n    def extract(self, m):\n        return 1\nprint(zipfile.\n      ZipFile('p.zip').extract('late.py'))\nimport late\n",
		"attribute after a join":     "import zipfile\nclass ZipFile:\n    def extract(self, m):\n        return 1\nzipfile.\\\nZipFile('p.zip').extract('late.py')\nimport late\n",
		"star import rebinds":        "import zipfile\nclass ZipFile:\n    def extract(self, m):\n        return 1\nfrom zipfile import *\nZipFile('p.zip').extract('late.py')\nimport late\n",
		"extract decorated, lines":   "import zipfile\nNAME = 'late.py'\nclass Reader:\n    @swap(\n        1)\n    def extract(self, m):\n        return 1\nReader().extract(NAME)\n",
		"class header in a string":   "import zipfile\nNAME = 'late.py'\nDOC = '''\nclass Reader:\n    def extract(self, m):\n        return 1\n'''\nReader().extract(NAME)\n",
		"method code swapped":        "import zipfile\nNAME = 'late.py'\nclass Reader:\n    def extract(self, m):\n        return 1\nf = getattr(Reader, 'ext' + 'ract')\nf.__code__ = g.__code__\nReader().extract(NAME)\n",
	} {
		if what := moduleSourceRefusal(body); what == "" {
			t.Errorf("%s: the source was accepted:\n%s", label, body)
		}
	}
	const body = "class Reader:\n    def extract(self, m):\n        return 1\ndef test_x():\n    Reader().extract('late.py')\nimport unittest\n"
	for label, helper := range map[string]string{
		"type.__setattr__": "import zipfile, test_calc\nc = getattr(test_calc, 'Rea' + 'der')\ntype.__setattr__(c, 'ext' + 'ract', lambda s, m: 1)\n",
		"__code__ swap":    "import test_calc\nf = getattr(getattr(test_calc, 'Rea' + 'der'), 'ext' + 'ract')\nf.__code__ = g.__code__\n",
		"__globals__":      "import test_calc\ndef g():\n    pass\ng2 = getattr(test_calc, 'test_x')\ng2.__globals__['Rea' + 'der'] = ZZ\n",
		"gc referents":     "import gc, test_calc\nd = gc.get_referents(getattr(getattr(test_calc, 'Rea' + 'der'), '__di' + 'ct__'))[0]\n",
	} {
		cwd := crw1187Project(t, body, map[string]string{"helper.py": helper})
		reason := crw1187Reason(t, "python3 -B -m unittest", cwd)
		if !strings.Contains(reason, "test_calc.py calls extract on Reader(), which helper.py may rebind") {
			t.Errorf("%s: the run was not refused for the other source: %q", label, reason)
		}
	}
	// a decorator over one line on another class or function, and a call of the class inside a bracket, keep the proof
	for label, body := range map[string]string{
		"decorated function elsewhere": "import zipfile\nNAME = 'calc.py'\nclass Reader:\n    def extract(self, m):\n        return 1\n@staticmethod\ndef helper():\n    pass\nprint(\n    Reader().extract(NAME))\nimport unittest\n",
		"call inside brackets":         "import zipfile\nNAME = 'calc.py'\nclass Reader:\n    def extract(self, m):\n        return 1\nx = [\n    Reader().extract(NAME),\n]\nimport unittest\n",
	} {
		if what := moduleSourceRefusal(body); what != "" {
			t.Errorf("%s: refused: %s", label, what)
		}
	}
}
