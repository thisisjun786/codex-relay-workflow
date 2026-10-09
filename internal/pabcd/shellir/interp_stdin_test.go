package shellir

import "testing"

// TestOpaqueInterpreterProgramPositions: php, lua, Rscript, tclsh and osascript have no reader in this port. A program they read
// from standard input, from a descriptor alias, or from a -e or -r option is code the text shows and the port cannot read:
// unreadable (CRW-851 item 5, CRW-894 item 5). A script file operand is not judged, as for the readers' own interpreters.
func TestOpaqueInterpreterProgramPositions(t *testing.T) {
	cases := []struct {
		cmd        string
		unreadable bool
	}{
		{"php -f /dev/stdin <<< '<?php echo 1;'", true},
		{"php <<'EOF'\n<?php echo 1;\nEOF", true},
		{"printf x | php", true},
		{"lua <<'EOF'\nprint(1)\nEOF", true},
		{"printf x | lua -", true},
		{"Rscript -e 'print(1)'", true},
		{"osascript -e 'beep'", true},
		{"tclsh <<'EOF'\nputs 1\nEOF", true},
		{"php -f script.php", false},
		{"php script.php", false},
		{"lua script.lua arg", false},
		{"printf x | php script.php", false},
		{"php --version", false},
	}
	for _, c := range cases {
		_, err := Analyze(c.cmd, "/work")
		if got := err != nil; got != c.unreadable {
			t.Errorf("%q: unreadable=%v, want %v (%v)", c.cmd, got, c.unreadable, err)
		}
	}
}

// TestNodeBooleanLongOptions: a node option that takes no value does not end the options, so the script operand after it is a
// script file and the pipe is data. An option the reader does not know may take a value and stays unreadable.
func TestNodeBooleanLongOptions(t *testing.T) {
	for _, cmd := range []string{
		"printf x | node --no-warnings script.js",
		"printf x | node --no-deprecation --trace-warnings script.js",
		"printf x | node --harmony --use-strict script.js",
	} {
		if _, err := Analyze(cmd, "/work"); err != nil {
			t.Errorf("%q: unreadable: %v", cmd, err)
		}
	}
	for _, cmd := range []string{
		"printf x | node --no-warnings --require fs",
		"printf x | node --frobnicate script.js",
	} {
		if _, err := Analyze(cmd, "/work"); err == nil {
			t.Errorf("%q: read, want unreadable", cmd)
		}
	}
	// CRW-1058: the bytes a literal printf writes are the program node reads from standard input.
	if _, err := Analyze("printf x | node --no-warnings", "/work"); err != nil {
		t.Errorf("printf x | node --no-warnings: unreadable: %v", err)
	}
}

// TestInterpreterStdinFile: an interpreter whose standard input is a file the text names (python3 < prog.py) runs that file like
// a script file operand: not judged (CRW-894 c10, the </dev/null control). A descriptor alias and a pipe-fed command stay
// unreadable; a file redirect does not take the place of a pipe (zsh MULTIOS reads both).
func TestInterpreterStdinFile(t *testing.T) {
	for _, cmd := range []string{
		"python3 </dev/null",
		"python3 < prog.py",
		"python3 </dev/null; { cat; } <<'EOF'\nhello\nEOF",
		"node < prog.js",
	} {
		if _, err := Analyze(cmd, "/work"); err != nil {
			t.Errorf("%q: unreadable: %v", cmd, err)
		}
	}
	for _, cmd := range []string{
		"python3 </dev/stdin",
		"python3 </dev/fd/3",
		"printf x | python3 </dev/null",
		"python3 < \"$F\"",
	} {
		if _, err := Analyze(cmd, "/work"); err == nil {
			t.Errorf("%q: read, want unreadable", cmd)
		}
	}
}

// TestShellStdinFile: a shell in a carried text whose standard input is a file the text names reads that file as its program:
// /dev/null holds none and another file is a script file for the consumers. The redirection of the carrier shell applies
// (bash -c 'bash -c bash </dev/null' reads /dev/null even when the outer command is fed by a pipe); the right side of a pipe in
// the same text is not covered, because zsh with MULTIOS reads both.
func TestShellStdinFile(t *testing.T) {
	for _, cmd := range []string{
		"printf x | bash -c 'bash -c bash </dev/null'",
		"printf x | bash -c 'bash </dev/null'",
		"bash -c bash </dev/null",
	} {
		if _, err := Analyze(cmd, "/work"); err != nil {
			t.Errorf("%q: unreadable: %v", cmd, err)
		}
	}
	res, err := Analyze("bash -c 'sh < run.sh'", "/work")
	if err != nil {
		t.Fatalf("bash -c 'sh < run.sh': %v", err)
	}
	found := false
	for _, e := range res.Execs {
		found = found || e.Kind == KindScriptFile && e.Script.Value == "run.sh"
	}
	if !found {
		t.Errorf("bash -c 'sh < run.sh': no script file record for run.sh: %+v", res.Execs)
	}
	for _, cmd := range []string{
		"printf x | bash </dev/null",
		"printf x | bash </dev/stdin",
		"bash </dev/stdin",
		"bash < \"$F\"",
		"printf x | bash -c 'bash'",
		"printf x | bash -c 'exec </dev/null; bash'",
		"bash </dev/null",
		"printf 'rm -rf ../repo\\n' | bash -c 'bash 3<&0 </dev/null 0>&3'",
	} {
		if _, err := Analyze(cmd, "/work"); err == nil {
			t.Errorf("%q: read, want unreadable", cmd)
		}
	}
}
