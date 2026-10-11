package hook

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// CRW-1187 verification of 6be86fca, with both guards and the real interpreter in a temporary home.

// P1: a star import of zipfile after a semicolon or a compound header rebinds the module's own ZipFile class to the archive's: 6be86fca
// let both guards allow the run, and the interpreter extracted late.py (not in the inventory) from p.zip and ran its unread code. Both
// guards refuse the three forms now with the reader's reason naming the module source.
func TestCRW1187FixRound6StarImportArchiveIsRefusedByBothGuards(t *testing.T) {
	python, bash := crw1178Python(t)
	for label, stmt := range map[string]string{
		"standalone": "from zipfile import *",
		"semicolon":  "import os; from zipfile import *",
		"compound":   "if True: from zipfile import *",
	} {
		t.Run(label, func(t *testing.T) {
			cwd, root, env := gateScene(t)
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "injected.md")
			body := "import zipfile\nclass ZipFile:\n    def extract(self, name):\n        return name\n" + stmt + "\nZipFile('p.zip').extract('late.py')\nimport late\n"
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

// P2: a test module that only declares a method open(self, path, mode) beside a code name writes nothing: 6be86fca read the formal
// argument as a mode it cannot evaluate and refused the run in both guards. The GitHub guard allows it again; the memory gate's own
// destination check is not the reader's and is not asserted here beyond the reader's reason. The interpreter runs the module.
func TestCRW1187FixRound6OpenDeclarationIsAllowed(t *testing.T) {
	python, bash := crw1178Python(t)
	for label, decl := range map[string]string{
		"def":       "    def open(self, path, mode):\n        return None\n",
		"async_def": "    async def open(self, path, mode):\n        return None\n",
	} {
		t.Run(label, func(t *testing.T) {
			cwd, root, env := gateScene(t)
			crw1187Write(t, cwd, map[string]string{
				"test_probe.py": "NAME='calc.py'\nclass Reader:\n" + decl + "import unittest\nclass T(unittest.TestCase):\n    def test_ok(self):\n        self.assertTrue(True)\n",
			})
			const cmd = "python3 -B -m unittest"
			mem, gh := crw1187Both(t, cwd, env, cmd)
			if gh != "" {
				t.Errorf("the GitHub guard refused the run: %s", gh)
			}
			for _, r := range []string{mem, gh} {
				if strings.Contains(r, "module source test_probe.py") {
					t.Errorf("a guard refused the run for the reader: %s", r)
				}
			}
			if out := crw1178Run(t, bash, python, cwd, filepath.Dir(root), cmd); !strings.Contains(out, "OK") {
				t.Fatalf("the run failed: %s", out)
			}
		})
	}
}
