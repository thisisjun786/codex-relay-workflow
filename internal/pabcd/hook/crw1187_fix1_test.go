package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// CRW-1187 verification fix round 1, end to end with the real interpreter and both guards. Every case that ran code the guards had
// not read is refused by both; the isolated runs the issue promises stay allowed and do not import the shadow.

func TestCRW1187FixRound1SourceSpellingsAreRefusedByBothGuards(t *testing.T) {
	python, bash := crw1178Python(t)
	for label, prefix := range map[string]string{
		"attribute_continuation": "import sys\nsys.\\\npath.insert(0, LIB)\nimport evil\n",
		"unicode_alias":          "import sys as \U0001d4fc\n\U0001d4fc.path.insert(0, LIB)\nimport evil\n",
		"string_bracket":         "note = '('\nimport sys as s\ns.path.insert(0, LIB)\nimport evil\n",
		"colon_glued":            "if True:import sys as s\ns.path.insert(0, LIB)\nimport evil\n",
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
			// the interpreter does run it: that is what the refusal is for
			crw1178Run(t, bash, python, cwd, filepath.Dir(root), cmd)
			if b, err := os.ReadFile(marker); err != nil || string(b) != "unread code ran" {
				t.Fatalf("the probe did not execute the unread code (%v)", err)
			}
		})
	}
}

func TestCRW1187FixRound1SortOutputBesideTheRunIsRefusedByBothGuards(t *testing.T) {
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
	const cmd = "python3 -B -m unittest | sort payload.txt -o late.py"
	crw1187Refused(t, cwd, env, cmd, "beside")
	crw1178Run(t, bash, python, cwd, filepath.Dir(root), cmd)
	if b, err := os.ReadFile(marker); err != nil || string(b) != "unread code ran" {
		t.Fatalf("the probe did not execute the unread code (%v)", err)
	}
	// the run without the writer is allowed
	_ = os.Remove(filepath.Join(cwd, "late.py"))
	crw1187Allowed(t, cwd, env, "python3 -B -m unittest | sort -o result.txt")
}

// The GitHub guard reads the hook's own PYTHONPATH, as the memory gate reads its environment: -P keeps it, and a PYTHONPATH that names the
// directory puts the shadow back on the path.
func TestCRW1187FixRound1TheGitHubGuardReadsPythonPath(t *testing.T) {
	python, _ := crw1178Python(t)
	cwd, root, env := gateScene(t)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "injected.md")
	crw1187Write(t, cwd, map[string]string{
		"d.json":     "{}",
		"keyword.py": "open(" + strconv.Quote(marker) + ", 'w').write('shadow ran')\nkwlist=[]\nsoftkwlist=[]\niskeyword=frozenset().__contains__\nissoftkeyword=iskeyword\n",
	})
	const cmd = "python3 -P -B -m json.tool d.json"
	t.Setenv("PYTHONPATH", ".")
	_, gh := crw1187Both(t, cwd, env, cmd)
	if gh == "" {
		t.Error("the GitHub guard ignored the inherited PYTHONPATH")
	}
	run := exec.Command(python, "-P", "-B", "-m", "json.tool", "d.json")
	run.Dir = cwd
	run.Env = []string{"HOME=" + filepath.Dir(root), "PATH=/usr/bin:/bin", "PYTHONPATH=."}
	_, _ = run.CombinedOutput()
	if b, err := os.ReadFile(marker); err != nil || string(b) != "shadow ran" {
		t.Fatalf("the shadow did not run with PYTHONPATH=. (%v)", err)
	}
	// -I ignores it
	if out := HandleGitHubPostGuard(gateBash(t, cwd, "python3 -I -B -m json.tool d.json")); out != "" {
		t.Errorf("-I with PYTHONPATH set was refused: %s", githubPostAnswerReason(t, out))
	}
	// unset or empty: -P keeps the directory off the path
	t.Setenv("PYTHONPATH", "")
	if out := HandleGitHubPostGuard(gateBash(t, cwd, cmd)); out != "" {
		t.Errorf("-P with PYTHONPATH empty was refused: %s", githubPostAnswerReason(t, out))
	}
}

// The isolated exception covers the names of the fixed list: json.py in the directory is not imported by -I.
func TestCRW1187FixRound1IsolatedRunsOfFixedNamesAreAllowedAndSafe(t *testing.T) {
	python, _ := crw1178Python(t)
	cwd, root, env := gateScene(t)
	crw1187Write(t, cwd, map[string]string{"d.json": "{}", "json.py": "raise SystemExit('shadow ran')\n", "re/__init__.py": "raise SystemExit('shadow ran')\n"})
	for _, flags := range []string{"-I", "-I -B", "-EP", "-P"} {
		cmd := "python3 " + flags + " -m json.tool d.json"
		t.Setenv("PYTHONPATH", "")
		crw1187Allowed(t, cwd, env, cmd)
		run := exec.Command("sh", "-c", python+" "+flags+" -m json.tool d.json")
		run.Dir = cwd
		run.Env = []string{"HOME=" + filepath.Dir(root), "PATH=/usr/bin:/bin"}
		if out, err := run.CombinedOutput(); err != nil {
			t.Errorf("%s: the isolated run failed: %v %s", cmd, err, out)
		}
	}
	// without the flag the shadow is imported and both guards refuse
	crw1187Refused(t, cwd, env, "python3 -m json.tool d.json", "-I")
	// the other side of the exception: a writer beside the run is still refused
	crw1187Write(t, cwd, map[string]string{"payload.txt": "x = 1\n"})
	crw1187Refused(t, cwd, env, "python3 -I -B -m json.tool d.json | sort -o late.py payload.txt", "beside")
}
