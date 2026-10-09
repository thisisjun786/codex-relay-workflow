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
