package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// crw1187Both judges one command with both guards and returns their reasons, "" for an allow.
func crw1187Both(t *testing.T, cwd string, env host.LookupEnv, cmd string) (mem, gh string) {
	t.Helper()
	if out := HandleMemoryWriteGate(gateBash(t, cwd, cmd), env); out != "" {
		mem = gateDeny(t, out)
	}
	if out := HandleGitHubPostGuard(gateBash(t, cwd, cmd)); out != "" {
		gh = githubPostAnswerReason(t, out)
	}
	return mem, gh
}

func crw1187Refused(t *testing.T, cwd string, env host.LookupEnv, cmd string, want ...string) {
	t.Helper()
	mem, gh := crw1187Both(t, cwd, env, cmd)
	for who, r := range map[string]string{"memory gate": mem, "GitHub guard": gh} {
		if r == "" {
			t.Errorf("%s allowed %q", who, cmd)
			continue
		}
		if !strings.Contains(r, "[crw command-reader]") {
			t.Errorf("%s, %q: not the reader's wording: %s", who, cmd, r)
		}
		if strings.Contains(r, "...") {
			t.Errorf("%s, %q: the reason is cut: %s", who, cmd, r)
		}
		for _, w := range want {
			if !strings.Contains(r, w) {
				t.Errorf("%s, %q: the reason lacks %q: %s", who, cmd, w, r)
			}
		}
	}
}

func crw1187Allowed(t *testing.T, cwd string, env host.LookupEnv, cmd string) {
	t.Helper()
	mem, gh := crw1187Both(t, cwd, env, cmd)
	if mem != "" || gh != "" {
		t.Errorf("%q refused: memory gate %q, GitHub guard %q", cmd, mem, gh)
	}
}

func crw1187Write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// CRW-1187 item 1, end to end: python3 -m json.tool and -m py_compile import keyword (3.14, after it sets sys.path[0]) from the
// working directory, and a keyword.py there writes the memory note. Both guards refuse the run with the file and python -I; the
// run with -I is allowed and the interpreter does not import the shadow.
func TestCRW1187StdlibShadowIsRefusedByBothGuardsEndToEnd(t *testing.T) {
	python, bash := crw1178Python(t)
	for _, module := range []string{"json.tool d.json", "py_compile app.py"} {
		t.Run(module, func(t *testing.T) {
			cwd, root, env := gateScene(t)
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "injected.md")
			crw1187Write(t, cwd, map[string]string{"d.json": "{}\n", "app.py": "x = 1\n",
				"keyword.py": "open(" + "'" + marker + "'" + ", 'w').write('shadow ran')\nkwlist = []\nsoftkwlist = []\niskeyword = frozenset().__contains__\nissoftkeyword = iskeyword\n"})
			home := filepath.Dir(root)
			plain := "python3 -B -m " + module
			crw1187Refused(t, cwd, env, plain, "keyword.py", "python -I")
			crw1187Refused(t, cwd, env, "python3 -E -s -B -m "+module, "keyword.py", "python -I")
			isolated := "python3 -I -B -m " + module
			crw1187Allowed(t, cwd, env, isolated)
			// The premise: the plain run imports the shadow, the -I run does not.
			got := crw1178Run(t, bash, python, cwd, home, isolated)
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("the -I run imported the shadow: %v %s", err, got)
			}
			got = crw1178Run(t, bash, python, cwd, home, plain)
			if b, err := os.ReadFile(marker); err != nil || string(b) != "shadow ran" {
				t.Fatalf("the shadow never ran, so this test proves nothing: %v %s", err, got)
			}
		})
	}
	// -P keeps PYTHONPATH: a run that can have it set is refused, and the hook's environment is read too.
	cwd, root, env := gateScene(t)
	crw1187Write(t, cwd, map[string]string{"d.json": "{}\n", "keyword.py": "x = 1\n"})
	crw1187Allowed(t, cwd, env, "python3 -P -B -m json.tool d.json")
	withPath := func(k string) (string, bool) {
		if k == "PYTHONPATH" {
			return ".", true
		}
		return env(k)
	}
	if out := HandleMemoryWriteGate(gateBash(t, cwd, "python3 -P -B -m json.tool d.json"), withPath); out == "" {
		t.Error("memory gate allowed -P with PYTHONPATH in its environment")
	}
	_ = root
}

// CRW-1187 item 3, end to end: a test module that puts a directory on sys.path, or sets sys.pycache_prefix, loads code the inventory
// did not read. -I and -E -B do not change what the module does itself. Both guards refuse every form, naming the file and the
// construct; the run of a plain test module is allowed.
func TestCRW1187SourceThatMovesTheImportsIsRefusedByBothGuardsEndToEnd(t *testing.T) {
	python, bash := crw1178Python(t)
	t.Run("sys.path", func(t *testing.T) {
		cwd, root, env := gateScene(t)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(root, "injected.md")
		crw1187Write(t, cwd, map[string]string{
			"calc.py":      "def add(a, b):\n    return a + b\n",
			"lib/evil.py":  "open(" + "'" + marker + "'" + ", 'w').write('unread code ran')\n",
			"test_calc.py": "import os, sys\nsys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), 'lib'))\nimport unittest\nimport calc\nimport evil\nclass T(unittest.TestCase):\n    def test_add(self):\n        self.assertEqual(calc.add(1, 2), 3)\n",
		})
		for _, cmd := range []string{"python3 -B -m unittest", "python3 -I -B -m unittest", "python3 -E -B -m unittest", "python3 -B -m unittest -v", "python3 -B -m unittest test_calc"} {
			crw1187Refused(t, cwd, env, cmd, "test_calc.py", "sys.path", "cannot read what it loads")
		}
		// The premise: even under -I the module's own sys.path change loads the directory it names.
		got := crw1178Run(t, bash, python, cwd, filepath.Dir(root), "python3 -I -B -m unittest")
		if b, err := os.ReadFile(marker); err != nil || string(b) != "unread code ran" {
			t.Fatalf("the unread directory never ran, so this test proves nothing: %v %s", err, got)
		}
	})
	t.Run("sys.pycache_prefix", func(t *testing.T) {
		_, bash, cwd, root, env, pfx, marker := crw1178PrefixScene(t)
		python, _ := crw1178Python(t)
		crw1187Write(t, cwd, map[string]string{
			"test_calc.py": "import sys\nsys.pycache_prefix = " + "'" + pfx + "'" + "\nimport unittest\nimport calc\nclass T(unittest.TestCase):\n    def test_add(self):\n        self.assertEqual(calc.add(1, 2), 3)\n",
		})
		for _, cmd := range []string{"python3 -B -m unittest", "python3 -E -B -m unittest", "python3 -I -B -m unittest"} {
			crw1187Refused(t, cwd, env, cmd, "test_calc.py", "pycache_prefix", "cannot read what it loads")
		}
		got := crw1178Run(t, bash, python, cwd, filepath.Dir(root), "python3 -E -B -m unittest")
		if b, err := os.ReadFile(marker); err != nil || string(b) != "unread bytecode ran" {
			t.Fatalf("the cache under the prefix never ran through the module's own setting, so this test proves nothing: %v %s", err, got)
		}
	})
	t.Run("plain test module", func(t *testing.T) {
		cwd, _, env := gateScene(t)
		crw1187Write(t, cwd, map[string]string{
			"calc.py":      "def add(a, b):\n    return a + b\n",
			"test_calc.py": "import sys, unittest\nimport calc\nclass T(unittest.TestCase):\n    def test_add(self):\n        self.assertEqual(calc.add(1, 2), 3)\n        sys.stdout.write('')\n",
		})
		for _, cmd := range []string{"python3 -m unittest", "python3 -B -m unittest", "python3 -E -B -m unittest", "python3 -I -B -m unittest", "python3 -B -m unittest | tee out.log"} {
			crw1187Allowed(t, cwd, env, cmd)
		}
		if err := os.MkdirAll(filepath.Join(cwd, "__pycache__"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(cwd, "__pycache__")); err != nil {
			t.Fatal(err)
		}
		crw1187Allowed(t, cwd, env, "python3 -B -m unittest")
	})
}

// CRW-1187 item 2 through both guards: a writer that runs beside the module run is refused, one that runs after it is not.
func TestCRW1187WriterBesideTheModuleRunIsRefusedByBothGuards(t *testing.T) {
	cwd, _, env := gateScene(t)
	crw1187Write(t, cwd, map[string]string{
		"calc.py":      "def add(a, b):\n    return a + b\n",
		"test_calc.py": "import unittest\nimport calc\nclass T(unittest.TestCase):\n    def test_add(self):\n        self.assertEqual(calc.add(1, 2), 3)\n",
	})
	for _, cmd := range []string{
		"python3 -B -m unittest | tee late.py",
		"python3 -B -m unittest | sh -c 'printf x > late.py'",
		"python3 -B -m unittest; (sleep 1; printf x > late.py) &",
		"python3 -B -m unittest; coproc { printf x > late.py; }",
	} {
		crw1187Refused(t, cwd, env, cmd, "beside the module run", "separate command")
	}
	// A writer that the text starts before the module run is judged by the records before it, as at the base.
	for _, cmd := range []string{
		"(sleep 1; printf x > late.py) & python3 -B -m unittest",
		"coproc { printf x > late.py; }; python3 -B -m unittest",
	} {
		crw1187Refused(t, cwd, env, cmd, "module import may be rewritten")
	}
	for _, cmd := range []string{
		"python3 -B -m unittest; printf x > late.py",
		"python3 -B -m unittest && cp calc.py late.py",
		"python3 -B -m unittest | tee out.log",
		"sleep 1 & python3 -B -m unittest",
	} {
		crw1187Allowed(t, cwd, env, cmd)
	}
}

// The module list pinned against the host interpreter is exercised in the shellir tests; this one runs the real interpreter in a
// temporary home to show the form the shadow reasons route to (python -I -m ...) is a command the interpreter accepts.
func TestCRW1187RouteCommandsRunWithTheRealInterpreter(t *testing.T) {
	python, _ := crw1178Python(t)
	cwd, root, _ := gateScene(t)
	crw1187Write(t, cwd, map[string]string{"d.json": `{"a": 1}`, "app.py": "x = 1\n", "keyword.py": "raise SystemExit('shadow')\n"})
	for _, args := range [][]string{{"-I", "-B", "-m", "json.tool", "d.json"}, {"-I", "-B", "-m", "py_compile", "app.py"}} {
		run := exec.Command(python, args...)
		run.Dir = cwd
		run.Env = []string{"HOME=" + filepath.Dir(root), "PATH=/usr/bin:/bin", "TZ=UTC"}
		if out, err := run.CombinedOutput(); err != nil {
			t.Errorf("python %v: %v %s", args, err, out)
		}
	}
}
