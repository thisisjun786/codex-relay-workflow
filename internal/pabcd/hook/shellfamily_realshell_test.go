package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-894 after the evaluation of 1688f7c5: the facts the reader's rules rest on, run under the real programs. Each case runs a
// command text that prints a marker when the piped program runs, and checks that it does: the reader must refuse the text
// (rows/19-crw-894-eval-1688f7c5.txt) because the program that runs is the pipe, not the file or the redirection the text names.
func TestPipedProgramRunsUnderRealPrograms(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "safe.sh"), []byte(":\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "safe.py"), []byte("pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const marker = "from-pipe"
	cases := []struct {
		name   string
		runner string // the program that runs the command text
		cmd    string
		prog   string // the program that reads the pipe
	}{
		// d4: zsh with MULTIOS (the shell of the Bash tool) reads the pipe as well as the file the redirection names, so
		// `bash </dev/null` behind a pipe runs the pipe: the issue's control is refused on purpose.
		{"zsh MULTIOS with /dev/null", "zsh", "printf 'echo " + marker + "\\n' | bash </dev/null", "bash"},
		{"zsh -f MULTIOS with /dev/null", "zsh -f", "printf 'echo " + marker + "\\n' | bash </dev/null", "bash"},
		// d2: with -s the operands are positional parameters; the program is the pipe
		{"bash -s file", "bash", "printf 'echo " + marker + "\\n' | bash -s safe.sh", "bash"},
		{"bash -xs file", "bash", "printf 'echo " + marker + "\\n' | bash -xs safe.sh", "bash"},
		{"bash -os posix file", "bash", "printf 'echo " + marker + "\\n' | bash -os posix safe.sh", "bash"},
		{"dash -s file", "bash", "printf 'echo " + marker + "\\n' | dash -s safe.sh", "dash"},
		{"busybox ash -s file", "bash", "printf 'echo " + marker + "\\n' | busybox ash -s safe.sh", "busybox"},
		// d1: every spelling of the descriptor alias reads the pipe
		{"python3 /dev/./stdin", "bash", "printf 'print(\"" + marker + "\")\\n' | python3 /dev/./stdin", "python3"},
		{"python3 //dev/stdin", "bash", "printf 'print(\"" + marker + "\")\\n' | python3 //dev/stdin", "python3"},
		{"python3 /dev/../dev/stdin", "bash", "printf 'print(\"" + marker + "\")\\n' | python3 /dev/../dev/stdin", "python3"},
		{"python3 /dev//fd//0", "bash", "printf 'print(\"" + marker + "\")\\n' | python3 /dev//fd//0", "python3"},
		{"python3 /proc/self/root/dev/stdin", "bash", "printf 'print(\"" + marker + "\")\\n' | python3 /proc/self/root/dev/stdin", "python3"},
		{"cd /dev; python3 stdin", "bash", "cd /dev; printf 'print(\"" + marker + "\")\\n' | python3 stdin", "python3"},
		{"cd /dev; python3 /proc/self/cwd/stdin", "bash", "cd /dev; printf 'print(\"" + marker + "\")\\n' | python3 /proc/self/cwd/stdin", "python3"},
		// the interpreter's REPL option reads the pipe after its script or string
		{"python3 -i script", "bash", "printf 'print(\"" + marker + "\")\n' | python3 -i safe.py", "python3"},
		{"python3 -i -c", "bash", "printf 'print(\"" + marker + "\")\n' | python3 -i -c pass", "python3"},
		{"node -e 0 -i", "bash", "printf 'console.log(\"" + marker + "\")\n' | node -e 0 -i", "node"},
		// the kernel follows a process link before the dot-dot segment after it
		{"python3 /proc/self/root/../../dev/stdin", "bash", "printf 'print(\"" + marker + "\")\\n' | python3 /proc/self/root/../../dev/stdin", "python3"},
		{"cd /; python3 /proc/self/cwd/../dev/stdin", "bash", "cd /; printf 'print(\"" + marker + "\")\\n' | python3 /proc/self/cwd/../dev/stdin", "python3"},
		{"cd /; python3 dev/stdin", "bash", "cd /; printf 'print(\"" + marker + "\")\\n' | python3 dev/stdin", "python3"},
		{"cd /; python3 ./dev/./fd/0", "bash", "cd /; printf 'print(\"" + marker + "\")\\n' | python3 ./dev/./fd/0", "python3"},
		{"bash /proc/self/root/../../dev/stdin", "bash", "printf 'echo " + marker + "\\n' | bash /proc/self/root/../../dev/stdin", "bash"},
		{"bash /dev/./stdin", "bash", "printf 'echo " + marker + "\\n' | bash /dev/./stdin", "bash"},
		{"bash </dev/./stdin", "bash", "printf 'echo " + marker + "\\n' | bash </dev/./stdin", "bash"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			runner := strings.Fields(c.runner)
			for _, prog := range []string{runner[0], c.prog} {
				if _, err := exec.LookPath(prog); err != nil {
					t.Skipf("%s is not installed", prog)
				}
			}
			cmd := exec.Command(runner[0], append(runner[1:], "-c", c.cmd)...)
			cmd.Dir = dir
			out, _ := cmd.CombinedOutput()
			if !strings.Contains(string(out), marker) {
				t.Fatalf("the piped program did not run under %s: %q (output %q)", c.runner, c.cmd, out)
			}
			// and the reader refuses the same text through the three gates
			if got := reproGotText(t, c.cmd); got != reproClasses["U"] {
				t.Errorf("memory/github/worktree = %v, want every gate to refuse a program it cannot read: %q", got, c.cmd)
			}
		})
	}
}

// reproGotText judges a command text with the three gates in a scene with a safe.sh in the working directory and the checkout.
func reproGotText(t *testing.T, cmd string) [3]string {
	t.Helper()
	r := newDelRig(t)
	cwd, root, env := gateScene(t)
	for _, base := range []string{cwd, r.checkout} {
		for name, body := range map[string]string{"safe.sh": ":\n", "safe.py": "pass\n"} {
			if err := os.WriteFile(filepath.Join(base, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return reproGot(r, cwd, root, env, cmd)
}

// TestPythonJSONToolRunsTheLocalModule: python3 -m json.tool puts the working directory first on the module search path, so a
// json package there runs instead of the standard library (finding 3 of the verifier of 4636e20a). The rows
// (rows/20-crw-894-verifier-4636e20a.txt) refuse that text, and allow it when the directory holds no such module.
func TestPythonJSONToolRunsTheLocalModule(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	const marker = "from-local-json"
	dir := t.TempDir()
	for name, body := range map[string]string{"json/__init__.py": "print('" + marker + "')\n", "json/tool.py": "print('" + marker + "')\n"} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "-c", "printf '{}' | python3 -m json.tool")
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), marker) {
		t.Fatalf("the local json package did not run: %q", out)
	}
	clean := exec.Command("bash", "-c", "printf '{}' | python3 -m json.tool")
	clean.Dir = t.TempDir()
	out, err := clean.CombinedOutput()
	if err != nil || strings.Contains(string(out), marker) {
		t.Fatalf("the standard library module did not run in an empty directory: %q (%v)", out, err)
	}
}

// TestPythonJSONToolRunsALocalStandardModule (CRW-894, fix round 2): json.tool imports argparse after the working directory heads
// the module search path, so a local argparse.py runs under the real python3; the reader refuses python -m json.tool in a
// directory that holds any python module (row vr-31).
func TestPythonJSONToolRunsALocalStandardModule(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed")
	}
	const marker = "from-local-argparse"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "argparse.py"), []byte("print('"+marker+"')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", "printf '{}' | python3 -m json.tool")
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), marker) {
		t.Fatalf("the local argparse module did not run: %q", out)
	}
}
