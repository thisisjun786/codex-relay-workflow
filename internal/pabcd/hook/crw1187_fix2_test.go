package hook

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// CRW-1187 verification fix round 2, end to end with the real interpreter and both guards: each source ran code outside the inventory
// (evil.py writes the memory note) and both guards refuse the run.
func TestCRW1187FixRound2SourceSpellingsAreRefusedByBothGuards(t *testing.T) {
	python, bash := crw1178Python(t)
	for label, prefix := range map[string]string{
		"parenthesized_sys":    "import sys\n(sys).path.insert(0, LIB)\nimport evil\n",
		"builtins_alias":       "from builtins import __import__ as f\nf('sy' + 's').path.insert(0, LIB)\nimport evil\n",
		"parenthesized_import": "(__import__)('sy' + 's').path.insert(0, LIB)\nimport evil\n",
		"unicode_fstring":      "import sys\nx = f\"{ｓｙｓ.path.insert(0, LIB)}\"\nimport evil\n",
		"pep701_os_sys":        "import os\nd = {'k': 1}\nf\"{d[\"k\"]}{(s:=os.sys)}\"\ns.path.insert(0, LIB)\nimport evil\n",
	} {
		t.Run(label, func(t *testing.T) {
			cwd, root, env := gateScene(t)
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			lib := filepath.Join(filepath.Dir(cwd), "lib")
			marker := filepath.Join(root, "injected.md")
			crw1187Write(t, lib, map[string]string{"evil.py": "open(" + strconv.Quote(marker) + ", 'w').write('unread code ran')\n"})
			crw1187Write(t, cwd, map[string]string{"test_probe.py": "LIB = " + strconv.Quote(lib) + "\n" + prefix})
			const cmd = "python3 -B -m unittest"
			crw1187Refused(t, cwd, env, cmd, "test_probe.py")
			// the interpreter does run it (Python 3.12 or later reads the PEP 701 form): that is what the refusal is for
			crw1178Run(t, bash, python, cwd, filepath.Dir(root), cmd)
			if b, err := os.ReadFile(marker); err != nil || string(b) != "unread code ran" {
				t.Fatalf("the probe did not execute the unread code (%v)", err)
			}
		})
	}
}
