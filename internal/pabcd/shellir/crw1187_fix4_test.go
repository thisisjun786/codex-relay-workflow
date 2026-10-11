package shellir

import "testing"

// CRW-1187 verification of 8e670559: the writer checks judge the call, not the source.

// P1: a call through a writing module (an alias of it or its own name) whose path operands are all literal names no import can reach
// writes no module, whatever other string of the source ends .py; the call that does, or one the reader cannot evaluate, stays refused
// beside a code name. A Path('x') operand is not a literal (TestCRW1187FixRound5PathCallsAreNotLiterals).
func TestCRW1187FixRound4WriterCallsAreJudgedByTheirDestination(t *testing.T) {
	for label, body := range map[string]string{
		"shutil alias, data file": "import shutil as sh\nNAME = 'calc.py'\nsh.copy('data.txt', 'result.txt')\nimport unittest\n",
		"shutil alias, keywords":  "import shutil as sh\nNAME = 'calc.py'\nsh.copy(src='data.txt', dst='result.txt', follow_symlinks=False)\nimport unittest\n",
		"os alias, rename":        "import os as o\nNAME = 'calc.py'\no.rename('a.txt', 'b.txt')\nimport unittest\n",
		"shutil by name":          "import shutil\nNAME = 'calc.py'\nshutil.copy('data.txt', 'result.txt')\nimport unittest\n",
		"os by name, replace":     "import os\nNAME = 'calc.py'\nos.replace('a.json', 'b.json')\nimport unittest\n",
		"two calls, both data":    "import shutil as sh\nNAME = 'calc.py'\nsh.copy('a.txt', 'b.txt')\nsh.move('b.txt', 'c.txt')\nimport unittest\n",
		"open for writing, data":  "NAME = 'calc.py'\nopen('result.txt', 'w').write('x')\nimport unittest\n",
		"open, keyword file":      "NAME = 'calc.py'\nwith open(file='result.txt', mode='a') as f:\n    f.write('x')\nimport unittest\n",
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
		"destination in a name":       "import shutil as sh\nNAME = 'late.py'\nsh.copy('payload.txt', NAME)\nimport late\n",
		"destination a directory":     "import shutil as sh\nNAME = 'late.py'\nsh.copy('payload.txt', 'out')\n",
		"destination the cwd":         "import shutil as sh\nsh.copy('lib/late.py', '.')\nimport late\n",
		"code name as the source":     "import shutil as sh\nsh.copy('late.py', 'out/')\n",
		"one call data, one not":      "import shutil as sh\nsh.copy('a.txt', 'b.txt')\nsh.copy('payload.txt', 'late.py')\nimport late\n",
		"concatenated literal":        "import shutil as sh\nsh.copy('payload.txt', 'late' '.py')\nimport late\n",
		"escape in the destination":   "import shutil as sh\nNAME = 'x.py'\nsh.copy('payload.txt', 'late\\x2epy')\nimport late\n",
		"star operands":               "import shutil as sh\nNAME = 'late.py'\nsh.copy(*ARGS)\n",
		"f-string destination":        "import shutil as sh\nNAME = 'late.py'\nsh.copy('payload.txt', f'{NAME}')\n",
		"comment in the call":         "import shutil as sh\nsh.copy('payload.txt', # x\n 'late.py')\n",
		"alias without a call":        "import shutil as sh\ncp = sh.copy\ncp('payload.txt', 'late.py')\nimport late\n",
		"pyc in __pycache__":          "import os\nos.rename('payload.bin', '__pycache__/late.cpython-314.pyc')\n",
		"upper-case extension":        "import shutil\nNAME = 'x.py'\nshutil.copy('payload.txt', 'late.PY')\n",
		"package directory":           "import shutil\nNAME = 'x.py'\nshutil.copytree('fixture.d', 'late')\nimport late\n",
		"fdopen, no path operand":     "import os as o\nNAME = 'late.py'\no.fdopen(fd, 'w')\n",
		"open of a name":              "NAME = 'late.py'\nopen(NAME, 'w').write(PAYLOAD)\nimport late\n",
		"open of a code file":         "open('late.py', 'w').write(PAYLOAD)\nimport late\n",
		"Path open, mode only":        "from pathlib import Path\nPath('late.py').open(mode='w').write(PAYLOAD)\nimport late\n",
		"star import of shutil":       "from shutil import *\ncopy('payload.txt', 'late.py')\nimport late\n",
		"star import of os, rename":   "from os import *\nrename('payload.txt', 'late.py')\nimport late\n",
		"archive extract, by member":  "def test(archive):\n    archive.extract('late.py')\n",
		"archive module, unknown arg": "import zipfile\nNAME = 'calc.py'\nzipfile.ZipFile('p.zip').extract(member)\n",
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

// P1: .extract( is a writer for an archive the source opens (zipfile, tarfile) or when its member is a code name; a method of the same
// name on another object (a parser, a reader) writes nothing, whatever other string of the source ends .py.
func TestCRW1187FixRound4ExtractIsAWriterOnlyForAnArchive(t *testing.T) {
	for label, body := range map[string]string{
		"reader method, no operand":       "class Reader:\n    def extract(self):\n        return 1\nNAME = 'calc.py'\nReader().extract()\nimport unittest\n",
		"parser method, a name":           "import re\nNAME = 'calc.py'\nparser.extract(text)\nimport unittest\n",
		"regex helper, data literal":      "NAME = 'calc.py'\nfields = tool.extract('header.txt')\nimport unittest\n",
		"zip member, data file":           "import zipfile\nNAME = 'calc.py'\nzipfile.ZipFile('p.zip').extract('data.txt')\nimport unittest\n",
		"tar member, data file into dir.": "import tarfile\nNAME = 'calc.py'\ntarfile.open('p.tar').extract('data.txt', path='out.d')\nimport unittest\n",
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
		"zipfile extract":            "import zipfile\nzipfile.ZipFile('p.zip').extract('late.py')\n",
		"tarfile extract":            "import tarfile\ntarfile.open('p.tar').extract('late.py')\n",
		"ZipFile from-import":        "from zipfile import ZipFile\nNAME = 'late.py'\nZipFile('p.zip').extract(NAME)\nimport late\n",
		"archive into the cwd":       "import zipfile as z\nNAME = 'x.py'\nz.ZipFile('p.zip').extract('pkg', '.')\n",
		"member a code name, no zip": "NAME = 'calc.py'\nfixture.extract('late.py')\n",
		"pycache member":             "fixture.extract('__pycache__/late.cpython-314.pyc')\n",
	} {
		if what := moduleSourceRefusal(body); what == "" {
			t.Errorf("%s: the source was accepted:\n%s", label, body)
		}
	}
}
