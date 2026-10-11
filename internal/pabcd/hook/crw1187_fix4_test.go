package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// CRW-1187 verification of 8e670559, with both guards and the real interpreter in a temporary home.

// P1: a test module that copies a data file through an alias of shutil (or calls a method named extract on its own object) and names a
// code file elsewhere writes no module: both guards allow the run, and the interpreter runs it as read.
func TestCRW1187FixRound4DataWritesBesideACodeNameAreAllowedByBothGuards(t *testing.T) {
	python, bash := crw1178Python(t)
	for label, body := range map[string]string{
		"shutil_alias_copy": "import shutil as sh\nNAME = 'calc.py'\nsh.copy('data.txt', 'result.txt')\n",
		"os_alias_rename":   "import os as o\nNAME = 'calc.py'\nopen('a.txt', 'w').close()\no.rename('a.txt', 'result.txt')\n",
		"shutil_by_name":    "import shutil\nNAME = 'calc.py'\nshutil.copy('data.txt', 'result.txt')\n",
		"reader_extract":    "class Reader:\n    def extract(self):\n        open('result.txt', 'w').write('x')\nNAME = 'calc.py'\nReader().extract()\n",
	} {
		t.Run(label, func(t *testing.T) {
			cwd, root, env := gateScene(t)
			crw1187Write(t, cwd, map[string]string{
				"data.txt":      "x\n",
				"test_probe.py": body + "import unittest\nclass T(unittest.TestCase):\n    def test_ok(self):\n        self.assertTrue(True)\n",
			})
			const cmd = "python3 -B -m unittest"
			crw1187Allowed(t, cwd, env, cmd)
			out := crw1178Run(t, bash, python, cwd, filepath.Dir(root), cmd)
			if !strings.Contains(out, "OK") {
				t.Fatalf("the run failed: %s", out)
			}
			if _, err := os.Stat(filepath.Join(cwd, "result.txt")); err != nil {
				t.Fatalf("the probe wrote no data file: %v", err)
			}
		})
	}
}

// A star import of a writing module brings its writers in under their bare names (copy, rename); the bare call copied unread text onto
// an import location with both guards allowing the run. Both refuse it now, and the interpreter shows the copy is imported.
func TestCRW1187FixRound4StarImportOfAWriterIsRefusedByBothGuards(t *testing.T) {
	python, bash := crw1178Python(t)
	for label, body := range map[string]string{
		"shutil_star": "from shutil import *\ncopy('payload.txt', 'late.py')\nimport late\n",
		"os_star":     "from os import *\nrename('payload.txt', 'late.py')\nimport late\n",
	} {
		t.Run(label, func(t *testing.T) {
			cwd, root, env := gateScene(t)
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "injected.md")
			crw1187Write(t, cwd, map[string]string{
				"test_probe.py": body,
				"payload.txt":   "open(" + strconv.Quote(marker) + ", 'w').write('unread code ran')\n",
			})
			const cmd = "python3 -B -m unittest"
			crw1187Refused(t, cwd, env, cmd, "test_probe.py")
			crw1178Run(t, bash, python, cwd, filepath.Dir(root), cmd)
			if b, err := os.ReadFile(marker); err != nil || string(b) != "unread code ran" {
				t.Fatalf("the probe did not execute the unread code (%v)", err)
			}
		})
	}
}

// P1: the PEP 701 probe runs the interpreter in a temporary directory and home with a fixed environment: nothing of the host's
// (PYTHONPATH, PYTHONSTARTUP, HOME and its user site) reaches it.
func TestCRW1187FixRound4PEP701ProbeIsIsolatedFromTheHost(t *testing.T) {
	python, _ := crw1178Python(t)
	host := t.TempDir()
	marker := filepath.Join(host, "host-code-ran")
	hook := "open(" + strconv.Quote(marker) + ", 'a').write('ran\\n')\n"
	lib := filepath.Join(host, "lib")
	crw1187Write(t, lib, map[string]string{"sitecustomize.py": hook, "usercustomize.py": hook})
	home := filepath.Join(host, "home")
	run := exec.Command(python, "-c", "import site; print(site.getusersitepackages())")
	run.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	usersite, err := run.Output()
	if err != nil {
		t.Fatal(err)
	}
	crw1187Write(t, strings.TrimSpace(string(usersite)), map[string]string{"usercustomize.py": hook})
	t.Setenv("PYTHONPATH", lib)
	t.Setenv("HOME", home)
	t.Setenv("PYTHONSTARTUP", filepath.Join(lib, "sitecustomize.py"))
	t.Setenv("PYTHONUSERBASE", "")
	_ = crw1187PEP701(t, python)
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("the probe ran host code: %q", b)
	}
}
