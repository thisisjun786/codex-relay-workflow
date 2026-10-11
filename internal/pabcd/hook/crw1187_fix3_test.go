package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// CRW-1187 / CRW-1183 pre-merge evaluation of 9201b469, end to end with the real interpreter and both guards. Each case ran code the
// guards had not read (the shadow or the module writes the memory note); both guards refuse it.

// P0 (1183 d2): -P keeps PYTHONPATH, and the text sets it through a name it builds from known parts.
func TestCRW1187FixRound3PythonPathBuiltFromPartsIsRefusedByBothGuards(t *testing.T) {
	python, bash := crw1178Python(t)
	cwd, root, env := gateScene(t)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "injected.md")
	crw1187Write(t, cwd, map[string]string{
		"d.json":  "{}",
		"json.py": "open(" + strconv.Quote(marker) + ", 'w').write('unread code ran')\nraise SystemExit(0)\n",
	})
	t.Setenv("PYTHONPATH", "")
	for _, cmd := range []string{
		"N=PYTHON; M=PATH; printf -v \"$N$M\" '%s' .; python3 -P -B -m json.tool d.json",
		"N=PYTHONPATH; export \"$N=.\"; python3 -P -B -m json.tool d.json",
	} {
		_ = os.Remove(marker)
		run := exec.Command(bash, "-c", cmd)
		run.Dir = cwd
		run.Env = []string{"HOME=" + filepath.Dir(root), "PATH=" + filepath.Dir(python) + ":/usr/bin:/bin", "TZ=UTC", "PYTHONPATH="}
		_, _ = run.CombinedOutput()
		if b, err := os.ReadFile(marker); err != nil || string(b) != "unread code ran" {
			t.Fatalf("%s: the shadow did not run (%v)", cmd, err)
		}
		crw1187Refused(t, cwd, env, cmd, "json.py")
	}
	// the run with nothing built is the safe one
	crw1187Allowed(t, cwd, env, "python3 -P -B -m json.tool d.json")
}

// P1 (1183 d3): -EP ignores PYTHONPATH and leaves the directory off sys.path, so a shadow whose name is not on the fixed list is not
// imported, and the run is allowed whatever the environment holds.
func TestCRW1187FixRound3EPRunIsAllowedWithPythonPathSet(t *testing.T) {
	python, _ := crw1178Python(t)
	cwd, root, env := gateScene(t)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "injected.md")
	crw1187Write(t, cwd, map[string]string{
		"d.json":     "{}",
		"keyword.py": "open(" + strconv.Quote(marker) + ", 'w').write('shadow ran')\n",
	})
	t.Setenv("PYTHONPATH", ".")
	withPath := gateEnvOf(map[string]string{"PYTHONPATH": ".", "HOME": filepath.Dir(root), "CODEX_HOME": filepath.Dir(root)})
	_ = env
	const cmd = "python3 -EP -B -m json.tool d.json"
	crw1187Allowed(t, cwd, withPath, cmd)
	run := exec.Command(python, "-EP", "-B", "-m", "json.tool", "d.json")
	run.Dir = cwd
	run.Env = []string{"HOME=" + filepath.Dir(root), "PATH=/usr/bin:/bin", "PYTHONPATH=."}
	if out, err := run.CombinedOutput(); err != nil || !strings.Contains(string(out), "{}") {
		t.Fatalf("-EP run failed: %v %s", err, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("-EP imported the shadow")
	}
	// -P alone keeps PYTHONPATH and imports it
	crw1187Refused(t, cwd, withPath, "python3 -P -B -m json.tool d.json", "keyword.py")
}

// P0 (1187 d3): uniq writes the file of its second operand; the operand is a name the reader cannot evaluate (read OUT).
func TestCRW1187FixRound3UniqWithAnUnknownOutputBesideTheRunIsRefusedByBothGuards(t *testing.T) {
	python, bash := crw1178Python(t)
	cwd, root, env := gateScene(t)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "injected.md")
	crw1187Write(t, cwd, map[string]string{
		"test_probe.py": "import time\ntime.sleep(1)\nimport late\n",
		"payload.txt":   "open(" + strconv.Quote(marker) + ", 'w').write('unread code ran')\n",
	})
	const cmd = "read OUT <<< late.py; python3 -B -m unittest | uniq payload.txt \"$OUT\""
	crw1187Refused(t, cwd, env, cmd, "beside")
	crw1178Run(t, bash, python, cwd, filepath.Dir(root), cmd)
	if b, err := os.ReadFile(marker); err != nil || string(b) != "unread code ran" {
		t.Fatalf("the probe did not execute the unread code (%v)", err)
	}
	_ = os.Remove(filepath.Join(cwd, "late.py"))
	crw1187Allowed(t, cwd, env, "python3 -B -m unittest | uniq - result.txt")
}

// P0 (1187 d2): the test copies the payload to a module through a name it gives shutil or os.
func TestCRW1187FixRound3AliasedCopyOfTheModuleIsRefusedByBothGuards(t *testing.T) {
	python, bash := crw1178Python(t)
	for label, body := range map[string]string{
		"shutil_alias": "import shutil as sh\nsh.copy('payload.txt', 'late.py')\nimport late\n",
		"os_alias":     "import os as o\no.rename('payload.txt', 'late.py')\nimport late\n",
		"path_rename":  "from pathlib import Path\nPath('payload.txt').rename('late.py')\nimport late\n",
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
