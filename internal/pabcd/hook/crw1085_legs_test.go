package hook_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
)

func TestCRW1085ReadableCommandsAtIngress(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
	for file, body := range map[string]string{"x": "echo ok\n", "x.py": "print('ok')\n", "test_calc.py": "import unittest\n", "notes.txt": "a\nb\n"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		cmd   string
		allow bool
	}{
		{"python3 -m unittest", true}, {"python -B -m unittest test_calc.py -v", true}, {"python3 -m pytest", true},
		{"python3 -m py_compile x.py", true}, {"python3 -m json.tool notes.txt", true},
		{"python3 -V", true}, {"python3 --version", true}, {"python3 -h", true}, {"node -v", true}, {"node --version", true}, {"ruby -v", true}, {"perl -v", true},
		{"[ -f x ]", true}, {"if [ -f f ]; then cat f; else echo y; fi", true},
		{"bash -n x", true}, {"sh -n x", true}, {"sh x", true}, {"bash x", true},
		{"awk 'END { print NR }' notes.txt", true}, {"awk -F: '{print $1}' notes.txt", true},
		{"awk '{getline; print}' notes.txt", true},
		{"python3 x.py", true}, {"python3 -u x.py", true}, {"test -f x", true}, {"ls", true},
		{"python3 -m http.server", false}, {"sh missing", false},
	}
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			for _, id := range []string{"pre-tool-use-guarding-memory-write", "pre-tool-use-guarding-github-post"} {
				raw, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "cwd": dir, "session_id": "s", "tool_input": map[string]any{"command": c.cmd}})
				var out string
				if id == "pre-tool-use-guarding-github-post" {
					out = hook.GitHubPostAnswer(strings.NewReader(string(raw)))
				} else {
					out = worktreeLeg(t, id).Handle(harness.Call{Raw: string(raw)})
				}
				if (out == "") != c.allow {
					t.Errorf("%s allow=%v want %v: %s", id, out == "", c.allow, out)
				}
			}
		})
	}
	for _, cmd := range []string{`awk '{print > "/x"}'`, `awk 'BEGIN{system("rm x")}'`, `awk '{getline < "/x"}'`} {
		raw, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": cmd}})
		if out := worktreeLeg(t, "pre-tool-use-guarding-memory-write").Handle(harness.Call{Raw: string(raw)}); !strings.Contains(out, "deny") {
			t.Errorf("writer allowed: %s", cmd)
		}
	}
	// Discovery must read the test program, not assume unittest/pytest are safe.
	if err := os.WriteFile(filepath.Join(dir, "test_bad.py"), []byte(`open("`+filepath.Join(dir, "codex", "memories", "x")+`","w")`), 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "cwd": dir, "tool_input": map[string]any{"command": "python3 -m unittest"}})
	if out := worktreeLeg(t, "pre-tool-use-guarding-memory-write").Handle(harness.Call{Raw: string(raw)}); !strings.Contains(out, "deny") {
		t.Fatal("protected discovery write allowed")
	}
}

func TestCRW1085ProtectedCompileAndShellFIFO(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("HOME", cwd)
	home := filepath.Join(cwd, "codex")
	t.Setenv("CODEX_HOME", home)
	root := filepath.Join(home, "memories")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "test_compile.py")
	if err := os.WriteFile(file, []byte("print(1)"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(cwd, "pipe.sh"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"python3 -m py_compile " + file, "sh pipe.sh"} {
		raw, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "cwd": cwd, "tool_input": map[string]any{"command": cmd}})
		if out := worktreeLeg(t, "pre-tool-use-guarding-memory-write").Handle(harness.Call{Raw: string(raw)}); out == "" {
			t.Errorf("protected write/unreadable FIFO allowed: %s", cmd)
		}
		if cmd == "sh pipe.sh" && hook.GitHubPostAnswer(strings.NewReader(string(raw))) == "" {
			t.Error("FIFO allowed by GitHub guard")
		}
	}
}
