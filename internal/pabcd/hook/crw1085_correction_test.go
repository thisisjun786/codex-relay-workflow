package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCRW1085CorrectionPermissionPaths(t *testing.T) {
	for _, tc := range []struct {
		name, cmd string
		files     map[string]string
	}{
		{"interactive", `bash --noprofile --norc -in -c 'printf x > MEMORY/n.md'`, nil},
		{"interactive-option", `bash -n -o interactive -c 'printf x > MEMORY/n.md'`, nil},
		{"unknown-option", `bash -n +o "$(printf noexec)" -c 'printf x > MEMORY/n.md'`, nil},
		{"dynamic-module", `python3 -B -m unittest test_safe.py`, map[string]string{"test_safe.py": "exec(input())"}},
		{"package-init", `python3 -B -m unittest pkg.test_safe`, map[string]string{"pkg/test_safe.py": "import unittest", "pkg/__init__.py": `open('MEMORY/n.md','w')`}},
		{"helper", `python3 -B -m unittest test_safe.py`, map[string]string{"test_safe.py": "import helper", "helper.py": `open('MEMORY/n.md','w')`}},
		{"conftest", `python3 -B -m pytest test_safe.py`, map[string]string{"test_safe.py": "def test_ok(): pass", "conftest.py": `open('MEMORY/n.md','w')`}},
		{"dependency-shadow", `python3 -B -m unittest test_safe.py`, map[string]string{"test_safe.py": "import unittest", "gettext.py": `open('MEMORY/n.md','w')`}},
		{"compile-prefix", `PYTHONPYCACHEPREFIX=MEMORY python3 -m py_compile x.py`, map[string]string{"x.py": "print(1)"}},
		{"import-prefix", `PYTHONPYCACHEPREFIX=MEMORY python3 -m unittest test_safe.py`, map[string]string{"test_safe.py": "import unittest"}},
		{"import-cache", `cd MEMORY; python3 -m unittest test_safe.py`, map[string]string{"MEMORY/test_safe.py": "import unittest"}},
		{"awk-second", `awk -f clean.awk -f writer.awk`, map[string]string{"clean.awk": `BEGIN { print "ok" }`, "writer.awk": `BEGIN { print "x" > "MEMORY/n.md" }`}},
		{"awk-attached", `awk -fclean.awk -fwriter.awk`, map[string]string{"clean.awk": `BEGIN { print "ok" }`, "writer.awk": `BEGIN { print "x" > "MEMORY/n.md" }`}},
		{"shell-child", `sh outer.sh`, map[string]string{"outer.sh": "./inner.sh", "inner.sh": "#!/bin/sh\nprintf x > MEMORY/n.md"}},
		{"python-child", `sh outer.sh`, map[string]string{"outer.sh": "python3 payload.py", "payload.py": `open('MEMORY/n.md','w')`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd, root, env := gateScene(t)
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			for f, b := range tc.files {
				p := filepath.Join(cwd, f)
				if strings.HasPrefix(f, "MEMORY/") {
					p = strings.ReplaceAll(f, "MEMORY", root)
				}
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(strings.ReplaceAll(b, "MEMORY", root)), 0700); err != nil {
					t.Fatal(err)
				}
			}
			cmd := strings.ReplaceAll(tc.cmd, "MEMORY", root)
			if HandleMemoryWriteGate(gateBash(t, cwd, cmd), env) == "" {
				t.Fatalf("protected effect allowed: %s", cmd)
			}
		})
	}
}

func TestCRW1085CorrectionSharedGuards(t *testing.T) {
	for _, kind := range []string{"interactive", "module-helper", "module-conftest", "module-init", "module-shadow"} {
		t.Run(kind, func(t *testing.T) {
			cwd, _, _ := gateScene(t)
			body := `import subprocess; subprocess.run(['gh','pr','comment','1','--body','x']); import shutil; shutil.rmtree('` + cwd + `')`
			cmd := `bash -in -c 'gh pr comment 1 --body x; rm -rf ` + cwd + `'`
			files := map[string]string{}
			switch kind {
			case "module-helper":
				cmd = "python3 -B -m unittest test_safe.py"
				files = map[string]string{"test_safe.py": "import helper", "helper.py": body}
			case "module-conftest":
				cmd = "python3 -B -m pytest test_safe.py"
				files = map[string]string{"test_safe.py": "def test_ok(): pass", "conftest.py": body}
			case "module-init":
				cmd = "python3 -B -m unittest pkg.test_safe"
				files = map[string]string{"pkg/test_safe.py": "import unittest", "pkg/__init__.py": body}
			case "module-shadow":
				cmd = "python3 -B -m unittest test_safe.py"
				files = map[string]string{"test_safe.py": "import unittest", "gettext.py": body}
			}
			for f, b := range files {
				p := filepath.Join(cwd, f)
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(b), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if HandleGitHubPostGuard(gateBash(t, cwd, cmd)) == "" {
				t.Error("hidden post allowed")
			}
			if !evaluateCommand(cmd, cwd, WorktreeIdentity{Managed: true, CheckoutRoot: cwd}).Deny {
				t.Error("hidden deletion allowed")
			}
		})
	}
}
