package hook

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// CRW-1187 verification of 73f063e9, with both guards and the real interpreter in a temporary home.

// P1: a test module that defines its own Path and passes Path('result.txt') as the destination of a copy wrote the unread payload to
// late.py and imported it, with the GitHub guard allowing the run and the memory gate refusing it only for its own destination check.
// Both guards refuse it now with the reader's reason naming the module source; the interpreter shows the copy runs the unread code.
func TestCRW1187FixRound5SpoofedPathOperandIsRefusedByBothGuards(t *testing.T) {
	python, bash := crw1178Python(t)
	for label, body := range map[string]string{
		"def_Path":     "import shutil as sh\ndef Path(name):\n    return 'late.py'\nsh.copyfile('payload.txt', Path('result.txt'))\nimport late\n",
		"rebound_Path": "import shutil as sh\nfrom pathlib import Path\nPath = lambda name: 'late.py'\nsh.copyfile('payload.txt', Path('result.txt'))\nimport late\n",
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
			crw1187Refused(t, cwd, env, cmd, "module source test_probe.py")
			crw1178Run(t, bash, python, cwd, filepath.Dir(root), cmd)
			if b, err := os.ReadFile(marker); err != nil || string(b) != "unread code ran" {
				t.Fatalf("the probe did not execute the unread code (%v)", err)
			}
		})
	}
}

// P1: a method named extract on an instance of a plain class the module defines writes nothing, beside an unrelated archive import, an
// archive word in a string or a code name as its operand: both guards allow the run, and the interpreter runs it.
func TestCRW1187FixRound5OrdinaryExtractReceiverIsAllowedByBothGuards(t *testing.T) {
	python, bash := crw1178Python(t)
	for label, body := range map[string]string{
		"unrelated_archive_import":  "import zipfile\nNAME='calc.py'\nclass Reader:\n    def extract(self):\n        return 1\nReader().extract()\n",
		"ordinary_metadata_operand": "class Reader:\n    def extract(self, name):\n        return name.rsplit('.', 1)[0]\nReader().extract('calc.py')\n",
		"archive_word_in_string":    "FORMAT='zipfile'\nNAME='calc.py'\nclass Reader:\n    def extract(self):\n        return 1\nReader().extract()\n",
	} {
		t.Run(label, func(t *testing.T) {
			cwd, root, env := gateScene(t)
			crw1187Write(t, cwd, map[string]string{
				"test_probe.py": body + "import unittest\nclass T(unittest.TestCase):\n    def test_ok(self):\n        self.assertTrue(True)\n",
			})
			const cmd = "python3 -B -m unittest"
			crw1187Allowed(t, cwd, env, cmd)
			if out := crw1178Run(t, bash, python, cwd, filepath.Dir(root), cmd); !strings.Contains(out, "OK") {
				t.Fatalf("the run failed: %s", out)
			}
		})
	}
}

// The archive's own extract stays refused by both guards.
func TestCRW1187FixRound5ArchiveExtractStaysRefusedByBothGuards(t *testing.T) {
	for label, body := range map[string]string{
		"zipfile_code_member":  "import zipfile\nzipfile.ZipFile('p.zip').extract('late.py')\nimport late\n",
		"archive_base_class":   "import zipfile\nclass Reader(zipfile.ZipFile):\n    pass\nReader('p.zip').extract('late.py')\nimport late\n",
		"unknown_receiver_zip": "import zipfile\nNAME = 'late.py'\nzf.extract(member)\n",
	} {
		t.Run(label, func(t *testing.T) {
			cwd, _, env := gateScene(t)
			crw1187Write(t, cwd, map[string]string{"test_probe.py": body})
			crw1187Refused(t, cwd, env, "python3 -B -m unittest", "module source test_probe.py")
		})
	}
}

// P1 (review of the round-5 fix): the proof that extract is a plain class's own method holds against a decorator over several lines, a
// class name after a dot on the line before inside a bracket and a star import that rebinds the class. Both guards refuse each with the
// reader's reason; the interpreter shows each extracts late.py from p.zip and runs its unread code.
func TestCRW1187FixRound5PlainReceiverProofHoldsForBothGuards(t *testing.T) {
	python, bash := crw1178Python(t)
	for label, body := range map[string]string{
		"decorator_over_lines":       "import zipfile\nNAME = 'late.py'\n@(lambda c: lambda: zipfile.ZipFile('p.zip')\n)\nclass Reader:\n    def extract(self, m):\n        return 1\nReader().extract(NAME)\nimport late\n",
		"attribute_across_a_newline": "import zipfile\nclass ZipFile:\n    def extract(self, m):\n        return 1\nprint(zipfile.\n      ZipFile('p.zip').extract('late.py'))\nimport late\n",
		"star_import_rebinds":        "import zipfile\nclass ZipFile:\n    def extract(self, m):\n        return 1\nfrom zipfile import *\nZipFile('p.zip').extract('late.py')\nimport late\n",
	} {
		t.Run(label, func(t *testing.T) {
			cwd, root, env := gateScene(t)
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "injected.md")
			crw1187Write(t, cwd, map[string]string{"test_probe.py": body})
			f, err := os.Create(filepath.Join(cwd, "p.zip"))
			if err != nil {
				t.Fatal(err)
			}
			zw := zip.NewWriter(f)
			w, err := zw.Create("late.py")
			if err == nil {
				_, err = w.Write([]byte("open(" + strconv.Quote(marker) + ", 'w').write('unread code ran')\n"))
			}
			if err != nil || zw.Close() != nil || f.Close() != nil {
				t.Fatalf("p.zip: %v", err)
			}
			const cmd = "python3 -B -m unittest"
			crw1187Refused(t, cwd, env, cmd, "module source test_probe.py")
			crw1178Run(t, bash, python, cwd, filepath.Dir(root), cmd)
			if b, err := os.ReadFile(marker); err != nil || string(b) != "unread code ran" {
				t.Fatalf("the probe did not execute the unread code (%v)", err)
			}
		})
	}
}
