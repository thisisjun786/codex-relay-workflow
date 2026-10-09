package shellir

import (
	"strings"
	"testing"
)

// CRW-1058 post-evaluation (crw1058-e83d8ec4 findings d1 to d9): each shape the grader showed reads the bytes the shell runs,
// or refuses them, and the harmless variants stay readable.

// execDirs returns the working directory of each program record of the given name, in order.
func execDirs(t *testing.T, cmd, cwd, name string) []string {
	t.Helper()
	r, err := Analyze(cmd, cwd)
	if err != nil {
		t.Fatalf("%q: %v", cmd, err)
	}
	var out []string
	for _, e := range r.Execs {
		if e.Kind == KindCommand && e.Name == name {
			out = append(out, e.Dir.Path)
		}
	}
	return out
}

// hasScriptFile reports whether the text makes a record of a script file the shell runs.
func hasScriptFile(t *testing.T, cmd string) bool {
	t.Helper()
	r, err := Analyze(cmd, "/work")
	if err != nil {
		t.Fatalf("%q: %v", cmd, err)
	}
	for _, e := range r.Execs {
		if e.Kind == KindScriptFile {
			return true
		}
	}
	return false
}

// d1: echo takes every leading -n, and bash also takes -e and -E; a word that starts with a dash after the first is an option
// the reader does not model, so the program is unreadable.
func TestCRW1058EchoRepeatedOptions(t *testing.T) {
	wantExecs(t, "echo -n -n 'echo hi' | bash", "echo", []string{"hi"})
	for _, cmd := range []string{
		"echo -n -e 'rm -rf ../repo' | bash",
		"echo -n -E 'rm -rf ../repo' | bash",
		"echo -e -n 'rm -rf ../repo' | bash",
		"echo -- 'rm -rf ../repo' | bash",
	} {
		wantUnreadable(t, cmd)
	}
}

// d2: a function named echo or printf runs in place of the program the pipe producer names, so no producer names a program then.
func TestCRW1058EchoShadowed(t *testing.T) {
	for _, cmd := range []string{
		"echo() { printf 'rm -rf ../repo'; }; echo ignored | bash",
		"printf() { echo 'rm -rf ../repo'; }; printf x | bash",
	} {
		wantUnreadable(t, cmd)
	}
}

// d3: bash, dash and zsh read a lone - like --: the operand after it is the script file, and the piped text is not the program.
func TestCRW1058DashEndsOptions(t *testing.T) {
	if !hasScriptFile(t, "printf ':' | bash - ./remove.sh") {
		t.Errorf("bash - ./remove.sh: the operand is not read as a script file")
	}
	wantExecs(t, "printf 'echo hi' | bash - ./remove.sh", "echo")
	wantExecs(t, "printf 'echo hi' | bash -s - x", "echo", []string{"hi"})
}

// d4: a here-string or here-document on a pipe's right side replaces the pipe for bash and is read with it by zsh: unreadable.
// d9: a descriptor 0 copied from another descriptor is the body that descriptor holds, when the copy comes after it.
func TestCRW1058StdinBodies(t *testing.T) {
	wantUnreadable(t, "printf 'rm -rf ../repo\\n' | bash /dev/stdin <<<'echo hi'")
	wantUnreadable(t, "printf 'rm -rf ../repo\\n' | bash <<<'echo hi'")
	wantExecs(t, "bash /dev/stdin 3<<<'echo hi' 0<&3", "echo", []string{"hi"})
	wantExecs(t, "bash 3<<<'echo hi' 0<&3", "echo", []string{"hi"})
	wantUnreadable(t, "bash 0<&3 3<<<'echo hi'")
	r, err := Analyze("python3 /dev/fd/0 3<<<'print(1)' 0<&3", "/work")
	if err != nil {
		t.Fatalf("a python program copied onto descriptor 0: %v", err)
	}
	found := false
	for _, e := range r.Execs {
		if e.Inline != nil && e.Inline.Language == "python" && strings.TrimSpace(e.Inline.Source.Value) == "print(1)" {
			found = true
		}
	}
	if !found {
		t.Errorf("the here-string behind descriptor 3 is not read as the python program: %+v", r.Execs)
	}
}

// d8: a pipe into a shell whose own standard output goes to a file keeps the piped program.
func TestCRW1058PipeWithStdoutRedirect(t *testing.T) {
	wantExecs(t, "printf 'echo hi\\n' | bash >/dev/null", "echo", []string{"hi"})
	wantExecs(t, "printf 'echo hi\\n' | bash >&2", "echo", []string{"hi"})
}

// d5: each parallel job is its own shell that starts in the directory of the text, so a cd in one job does not reach another.
func TestCRW1058ParallelJobsStartFresh(t *testing.T) {
	got := execDirs(t, "parallel -j1 cd '{}' ';' rm -rf ../repo ::: sub .", "/work/slot/repo", "rm")
	if len(got) != 2 || got[0] != "/work/slot/repo/sub" || got[1] != "/work/slot/repo" {
		t.Errorf("rm directories %q, want the first job in sub and the second job in the text's directory", got)
	}
}

// d6: {.} removes the extension from the final path component; a dot in a parent directory stays.
func TestCRW1058ParallelDottedParent(t *testing.T) {
	wantExecs(t, "parallel echo {.} ::: /tmp/wt/slot.v1/repo", "echo", []string{"/tmp/wt/slot.v1/repo"})
	wantExecs(t, "parallel echo {.} ::: /tmp/wt/slot.v1/repo.txt", "echo", []string{"/tmp/wt/slot.v1/repo"})
	wantExecs(t, "parallel echo {/.} ::: /tmp/wt/slot.v1/repo.txt", "echo", []string{"repo"})
}

// d7: an xargs operand is one more ::: source of parallel; the reader does not see those values, so the program is unreadable.
func TestCRW1058XargsFeedsParallel(t *testing.T) {
	wantUnreadable(t, "echo /tmp/wt/slot/repo | xargs parallel rm -rf '{}' ::: /tmp/unprotected")
}

