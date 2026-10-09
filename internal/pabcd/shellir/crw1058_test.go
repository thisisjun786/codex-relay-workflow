package shellir

import (
	"strings"
	"testing"
)

// CRW-1058: a parallel template is read as the commands its ::: sources make, a program a pipe or a descriptor alias
// carries is read from the text, and anything the reader cannot prove stays unreadable.

// execsNamed returns the argument values of each program record of the given name, in order.
func execsNamed(t *testing.T, cmd, name string) [][]string {
	t.Helper()
	r, err := Analyze(cmd, "/work")
	if err != nil {
		t.Fatalf("%q: %v", cmd, err)
	}
	var out [][]string
	for _, e := range r.Execs {
		if e.Kind != KindCommand || e.Name != name {
			continue
		}
		var args []string
		for _, a := range e.Args {
			args = append(args, a.Value)
		}
		out = append(out, args)
	}
	return out
}

func wantExecs(t *testing.T, cmd, name string, want ...[]string) {
	t.Helper()
	got := execsNamed(t, cmd, name)
	if len(got) != len(want) {
		t.Fatalf("%q: %s records %q, want %q", cmd, name, got, want)
	}
	for i := range want {
		if strings.Join(got[i], "\x00") != strings.Join(want[i], "\x00") {
			t.Errorf("%q: %s record %d args %q, want %q", cmd, name, i, got[i], want[i])
		}
	}
}

func wantUnreadable(t *testing.T, cmd string) {
	t.Helper()
	if _, err := Analyze(cmd, "/work"); !isUnreadable(err) {
		t.Errorf("%q: err %v, want unreadable", cmd, err)
	}
}

func TestCRW1058ParallelTemplates(t *testing.T) {
	wantExecs(t, "parallel echo {} ::: a b", "echo", []string{"a"}, []string{"b"})
	wantExecs(t, "parallel rm -rf {} ::: /tmp/wt/slot/repo", "rm", []string{"-rf", "/tmp/wt/slot/repo"})
	wantExecs(t, "parallel -j2 echo {.} ::: a.txt", "echo", []string{"a"})
	wantExecs(t, "parallel echo {/} ::: dir/a.txt", "echo", []string{"a.txt"})
	wantExecs(t, "parallel echo {//} ::: dir/a.txt", "echo", []string{"dir"})
	wantExecs(t, "parallel echo {/.} ::: dir/a.tar.gz", "echo", []string{"a.tar"})
	wantExecs(t, "parallel echo {#} ::: a b", "echo", []string{"1"}, []string{"2"})
	wantExecs(t, "parallel echo {2} {1} ::: a ::: b", "echo", []string{"b", "a"})
	wantExecs(t, "parallel echo {1} {2} ::: a b :::+ c d", "echo", []string{"a", "c"}, []string{"b", "d"})
	wantExecs(t, "parallel echo ::: a", "echo", []string{"a"})
	wantExecs(t, "parallel -- echo {} ::: a", "echo", []string{"a"})

	for _, cmd := range []string{
		"parallel echo",                    // no source: the arguments come from standard input
		"parallel echo ::: $X",             // a source the reader cannot see
		"parallel -S host echo ::: a",      // a remote host runs the program
		"parallel -a list echo ::: x",      // arguments read from a file
		"parallel echo {} :::: list.txt",   // a file of sources
		"parallel echo {%} ::: a",          // a slot number
		"parallel {} ::: ls",               // the program is a source
		"parallel echo {} ::: 'a b'",       // a value that the shell would split or quote
		"parallel echo {} ::: a b :::+ c",  // linked sources of different lengths
		"parallel --pipe echo ::: a",       // the input is split from standard input
		"parallel -X echo {} ::: a",        // several arguments per run
		"parallel --dry-run echo {} ::: a", // not modelled
		"parallel echo {= 'a' =} ::: a",    // a perl expression
	} {
		wantUnreadable(t, cmd)
	}
}

func TestCRW1058PipeProducers(t *testing.T) {
	wantExecs(t, "printf 'echo hi\\n' | bash", "echo", []string{"hi"})
	wantExecs(t, "echo 'echo hi' | sh", "echo", []string{"echo hi"}, []string{"hi"})
	wantExecs(t, "printf 'echo hi' | bash -s", "echo", []string{"hi"})
	// With -s the operands are positional parameters: the program is still the piped text, not a script file named foo.
	wantExecs(t, "printf 'echo hi' | bash -s foo", "echo", []string{"hi"})
	wantExecs(t, "printf 'echo hi' | bash -s -- foo bar", "echo", []string{"hi"})

	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | bash </dev/null", // a redirection of the shell's own stdin stays refused (zsh MULTIOS)
		"printf '%s' 'echo hi' | bash",              // a conversion the reader does not evaluate
		"printf 'x' | cat | bash",                   // the producer is not printf or echo
		"printf 'x' ${y:- } | bash",                 // the producer's word is unknown
		"echo -e 'echo \\x68i' | bash",              // echo -e would interpret the escapes
		"printf 'echo \\x68i' | bash",               // a printf escape the reader does not model
	} {
		wantUnreadable(t, cmd)
	}
}

func TestCRW1058FdAliases(t *testing.T) {
	wantExecs(t, "bash /dev/fd/3 3<<'EOF'\necho hi\nEOF", "echo", []string{"hi"})
	wantExecs(t, "bash /dev/fd/3 3<<<'echo hi'", "echo", []string{"hi"})
	wantExecs(t, "bash /proc/self/fd/3 3<<<'echo hi'", "echo", []string{"hi"})
	wantExecs(t, "bash /proc/thread-self/fd/3 3<<<'echo hi'", "echo", []string{"hi"})

	r, err := Analyze("python3 /dev/fd/4 3<<'PY' 4<&3\nimport shutil\nPY", "/work")
	if err != nil {
		t.Fatalf("a descriptor copied from a here-document: %v", err)
	}
	found := false
	for _, e := range r.Execs {
		if e.Inline != nil && e.Inline.Language == "python" && strings.TrimSpace(e.Inline.Source.Value) == "import shutil" {
			found = true
		}
	}
	if !found {
		t.Errorf("the here-document behind descriptor 4 is not read as the python program: %+v", r.Execs)
	}

	for _, cmd := range []string{
		"bash /dev/fd/3",                                    // no descriptor 3 in the text
		"bash /proc/12345/fd/3 3<<<'echo hi'",               // another process's descriptor is not ours
		"bash /dev/fd/3 3< <(echo hi)",                      // a process substitution is not a here-document
		"bash /dev/fd/3 3<<'EOF' 3<<'EOF2'\nx\nEOF\nEOF2\n", // two bodies for one descriptor
	} {
		wantUnreadable(t, cmd)
	}
}
