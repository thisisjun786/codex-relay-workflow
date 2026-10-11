package shellir

import "testing"

// CRW-1187 / CRW-1183 pre-merge evaluation of 9201b469.

// P0 (1183 d2): -P keeps PYTHONPATH. A text that sets it through a name it builds from known parts (printf -v "$N$M" with N=PYTHON and
// M=PATH) spells nothing the reader looks for, and the lookup still answers the environment's empty value.
func TestCRW1187FixRound3PythonPathNamedFromKnownPartsProvesNothingForP(t *testing.T) {
	unset := func(string) (string, bool) { return "", false }
	more := map[string]string{"json.py": "raise SystemExit('shadow')\n", "keyword.py": "x = 1\n", "d.json": "{}"}
	cwd := crw1187Project(t, "x = 1\n", more)
	for _, cmd := range []string{
		"N=PYTHON; M=PATH; printf -v \"$N$M\" '%s' .; python3 -P -B -m json.tool d.json",
		"N=PYTHON; N+=PATH; printf -v \"$N\" '%s' .; python3 -P -B -m json.tool d.json",
		"N=PYTHONPATH; export \"$N=.\"; python3 -P -B -m json.tool d.json",
		"N=PYTHONPATH; declare -x \"$N=.\"; python3 -P -B -m json.tool d.json",
		"N=PYTHONPATH; read \"$N\" <<< .; python3 -P -B -m json.tool d.json",
		"N=PYTHONPATH; declare -n r=$N; r=.; python3 -P -B -m json.tool d.json",
		"N=PYTHON; M=PATH; printf -v \"$N$M\" '%s' .; python3 -P -B -m py_compile app.py",
	} {
		if reason := crw1187ReasonEnv(t, cmd, cwd, unset); reason == "" {
			t.Errorf("%s: allowed", cmd)
		}
	}
	// a name that is not PYTHONPATH keeps the safe run
	for _, cmd := range []string{
		"N=OUT; printf -v \"$N\" '%s' .; python3 -P -B -m json.tool d.json",
		"N=PYTHON; M=SPATH; printf -v \"$N$M\" '%s' .; python3 -P -B -m json.tool d.json",
	} {
		if reason := crw1187ReasonEnv(t, cmd, cwd, unset); reason != "" {
			t.Errorf("%s: refused: %s", cmd, reason)
		}
	}
}

// P1 (1183 d3): -E makes the interpreter ignore PYTHONPATH, so -EP (and -IP) leaves the directory off sys.path whatever the
// environment holds.
func TestCRW1187FixRound3EIgnoresPythonPathForP(t *testing.T) {
	more := map[string]string{"keyword.py": "x = 1\n", "json.py": "x = 1\n", "d.json": "{}", "app.py": "x = 1\n"}
	cwd := crw1187Project(t, "x = 1\n", more)
	set := func(k string) (string, bool) { return ".", k == "PYTHONPATH" }
	for _, cmd := range []string{
		"python3 -EP -B -m json.tool d.json", "python3 -PE -B -m json.tool d.json", "python3 -E -P -B -m json.tool d.json",
		"python3 -IP -B -m json.tool d.json", "python3 -EP -m py_compile app.py",
	} {
		if reason := crw1187ReasonEnv(t, cmd, cwd, set); reason != "" {
			t.Errorf("%s with PYTHONPATH=.: refused: %s", cmd, reason)
		}
		// and it needs no environment: the reading that is given none allows it too
		if reason := crw1187ReasonEnv(t, cmd, cwd, nil); reason != "" {
			t.Errorf("%s with no environment: refused: %s", cmd, reason)
		}
	}
	// -E alone leaves the directory on sys.path, and -P alone keeps PYTHONPATH
	for _, cmd := range []string{
		"python3 -E -B -m json.tool d.json", "python3 -P -B -m json.tool d.json", "python3 -P -B -m py_compile app.py",
	} {
		if reason := crw1187ReasonEnv(t, cmd, cwd, set); reason == "" {
			t.Errorf("%s with PYTHONPATH=.: allowed", cmd)
		}
	}
}

// P0 (1187 d3): a program that writes a file an operand names, beside a module run, is refused when an operand it is given cannot be
// evaluated, as cp, tee and sort are: the operand may be the module it writes.
func TestCRW1187FixRound3UnknownOperandsOfWritersBesideTheRunAreRefused(t *testing.T) {
	cwd := crw1187Project(t, "import unittest\n", map[string]string{"payload.txt": "x = 1\n", "payload.gz": "x"})
	for _, cmd := range []string{
		"read OUT <<< late.py; python3 -B -m unittest | uniq payload.txt \"$OUT\"",
		"read OUT <<< late.py; python3 -B -m unittest | uniq payload.txt $OUT",
		"read OUT <<< late.py; python3 -B -m unittest | split payload.txt \"$OUT\"",
		"read OUT <<< late.py; python3 -B -m unittest | csplit payload.txt \"$OUT\"",
		"read OUT <<< late.py; python3 -B -m unittest | patch \"$OUT\" payload.txt",
		"read OUT <<< late.py; python3 -B -m unittest | rsync payload.txt \"$OUT\"",
		"read OUT <<< late.py; python3 -B -m unittest | gzip -c payload.txt \"$OUT\"",
		"read OUT <<< late.py; python3 -B -m unittest | zip \"$OUT\" payload.txt",
		"read OUT <<< late.py; python3 -B -m unittest | sed -n w\"$OUT\" payload.txt",
		"read OUT <<< late.py; python3 -B -m unittest | sed -i p \"$OUT\"",
		"read OUT <<< late.py; uniq payload.txt \"$OUT\" & python3 -B -m unittest",
		"read OUT <<< late.py; python3 -B -m unittest & uniq payload.txt \"$OUT\"",
		"read OUT <<< late.py; uniq payload.txt \"$OUT\"; python3 -B -m unittest",
		"python3 -B -m unittest | uniq payload.txt \"$OUT\"",
		"python3 -B -m unittest | uniq payload.txt late.py",
		"python3 -B -m unittest | shuf -o late.py payload.txt",
		"python3 -B -m unittest | iconv -o late.py payload.txt",
		"python3 -B -m unittest | xxd -r payload.txt late.py",
		"python3 -B -m unittest | openssl enc -d -in payload.txt -out late.py",
		"python3 -B -m unittest | truncate -s 0 late.py",
		"read OUT <<< late.py; python3 -B -m unittest | shuf -o \"$OUT\" payload.txt",
	} {
		if reason := crw1187Reason(t, cmd, cwd); reason == "" {
			t.Errorf("%s: allowed", cmd)
		}
	}
	// operands that are known and name no module, and filters with no operand, stay allowed
	for _, cmd := range []string{
		"python3 -B -m unittest | uniq", "python3 -B -m unittest | uniq - out.txt",
		"python3 -B -m unittest | gzip -c > out.gz", "python3 -B -m unittest | split - part.",
		"python3 -B -m unittest | tee out.log", "python3 -B -m unittest; uniq payload.txt \"$OUT\"",
		"python3 -B -m unittest && sed -i p out.txt",
	} {
		if reason := crw1187Reason(t, cmd, cwd); reason != "" {
			t.Errorf("%s: refused: %s", cmd, reason)
		}
	}
}

// P0 (1187 d2): the module the source writes is named through a name the source gives to os, shutil, posix or pathlib, or through a path
// object: the write is the same however the module was imported.
func TestCRW1187FixRound3AliasedWriteAPIsAtAnImportLocationAreRefused(t *testing.T) {
	for label, body := range map[string]string{
		"shutil as sh":            "import shutil as sh\nsh.copy('payload.txt', 'late.py')\nimport late\n",
		"shutil as sh, move":      "import shutil as sh\nsh.move('payload.txt', 'late.py')\nimport late\n",
		"os as o, rename":         "import os as o\no.rename('payload.txt', 'late.py')\nimport late\n",
		"os as o, replace":        "import os as o\no.replace('payload.txt', 'late.py')\nimport late\n",
		"os as o, link":           "import os as o\no.link('payload.txt', 'late.py')\nimport late\n",
		"os as o, symlink":        "import os as o\no.symlink('payload.txt', 'late.py')\nimport late\n",
		"posix as px":             "import posix as px\npx.rename('payload.txt', 'late.py')\nimport late\n",
		"pathlib as pl":           "import pathlib as pl\npl.Path('payload.txt').rename('late.py')\nimport late\n",
		"shutil in a list":        "import os, shutil as sh, sys\nsh.copy('payload.txt', 'late.py')\nimport late\n",
		"alias in a function":     "def f():\n    import shutil as sh\n    sh.copy('payload.txt', 'late.py')\nf()\nimport late\n",
		"alias of an alias":       "import shutil as sh\ncp = sh.copy\ncp('payload.txt', 'late.py')\nimport late\n",
		"shutil.unpack_archive":   "import shutil\nshutil.unpack_archive('payload.zip', 'late.py')\n",
		"shutil.make_archive":     "import shutil as sh\nsh.make_archive('late.py', 'zip', 'dir')\n",
		"copyfileobj":             "import shutil as sh\nsh.copyfileobj(open('payload.txt'), open('late.py', 'w'))\n",
		"Path rename one operand": "from pathlib import Path\nPath('payload.txt').rename('late.py')\nimport late\n",
		"Path replace":            "from pathlib import Path as P\nP('payload.txt').replace('late.py')\nimport late\n",
		"Path in a call":          "import pathlib\npathlib.Path('payload.txt').rename('late.py')\n",
		"zipfile extract":         "import zipfile\nzipfile.ZipFile('p.zip').extract('late.py')\n",
		"tarfile extract":         "import tarfile\ntarfile.open('p.tar').extract('late.py')\n",
		"pyc target":              "import shutil as sh\nsh.copy('payload.txt', '__pycache__/late.cpython-314.pyc')\n",
		"so target":               "import os as o\no.rename('payload.txt', 'late.so')\n",
	} {
		if what := moduleSourceRefusal(body); what == "" {
			t.Errorf("%s: the source was accepted:\n%s", label, body)
		}
		cwd := crw1187Project(t, body, nil)
		if reason := crw1187Reason(t, "python3 -B -m unittest", cwd); reason == "" {
			t.Errorf("%s: the module run was allowed", label)
		}
	}
	// what stays allowed: a call of the same name on something else, or no import location in the call
	for label, body := range map[string]string{
		"str replace":                 "name = 'a.py'.replace('.py', '')\nimport unittest\n",
		"list copy":                   "import shutil as sh\nx = [1].copy()\nNAME = 'calc.py'\nimport unittest\n",
		"shutil alias, data file":     "import shutil as sh\nsh.copy('payload.txt', 'result.txt')\nimport unittest\n",
		"os alias, data file":         "import os as o\no.rename('payload.txt', 'result.txt')\nimport unittest\n",
		"Path rename, data file":      "from pathlib import Path\nPath('a.txt').rename('b.txt')\nNAME = 'calc.py'\nimport unittest\n",
		"alias read of a code file":   "import shutil as sh\nprint(open('calc.py').read())\nimport unittest\n",
		"path join with a code name":  "import os as o\np = o.path.join('x', 'calc.py')\nimport unittest\n",
		"replace on a string var":     "name = 'calc.py'\nbase = name.replace('.py', '')\nimport unittest\n",
		"os.path alias, not a writer": "import os.path as op\np = op.join('x', 'calc.py')\nimport unittest\n",
	} {
		if what := moduleSourceRefusal(body); what != "" {
			t.Errorf("%s: refused: %s", label, what)
		}
	}
}
