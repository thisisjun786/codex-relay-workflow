package shellir

import (
	"testing"
)

// TestFdAliasPathSpellings (CRW-894, d1 of the evaluation of 1688f7c5): every spelling the kernel resolves to a descriptor of the
// process is the alias; a path that merely looks like one is not.
func TestFdAliasPathSpellings(t *testing.T) {
	known := Dir{Path: "/work/repo", Known: true}
	dev := Dir{Path: "/dev", Known: true}
	procfd := Dir{Path: "/proc/self/fd", Known: true}
	for _, c := range []struct {
		p     string
		dir   Dir
		alias bool
	}{
		{"/dev/stdin", known, true},
		{"/dev/./stdin", known, true},
		{"//dev/stdin", known, true},
		{"/dev/../dev/stdin", known, true},
		{"/dev//fd//0", known, true},
		{"/dev/fd/0", known, true},
		{"/dev/fd/../fd/3", known, true},
		{"/proc/self/fd/0", known, true},
		{"/proc/self/../self/fd/0", known, true},
		{"/proc/./self/fd/0", known, true},
		{"/proc/12345/fd/0", known, true},
		{"/proc/12345/task/12345/fd/0", known, true},
		{"/proc/self/root/dev/stdin", known, true},
		{"/proc/1/root/proc/self/fd/0", known, true},
		{"/proc/self/root/proc/self/root/dev/stdin", known, true},
		{"/proc/self/cwd/../../../../../../../dev/stdin", known, true},
		{"/proc/4242/cwd/x", known, true},
		{"stdin", dev, true},
		{"./stdin", dev, true},
		{"fd/0", dev, true},
		{"0", procfd, true},
		{"/proc/self/cwd/stdin", dev, true},
		{"/proc/self/cwd/stdin", Dir{}, true},
		{"stdin", Dir{}, true},
		{"fd/0", Dir{}, true},
		{"../dev/stdin", Dir{}, true},
		// the verifier of 4636e20a: dot-dot after a process link is resolved after the link, not before it
		{"/proc/self/root/../../dev/stdin", known, true},
		{"/proc/self/root/../dev/stdin", known, true},
		{"/proc/1/root/../../dev/fd/0", known, true},
		{"/proc/self/cwd/../dev/stdin", Dir{Path: "/", Known: true}, true},
		{"/proc/self/cwd/../../dev/stdin", Dir{Path: "/x", Known: true}, true},
		{"/proc/self/cwd/../../dev/stdin", Dir{Path: "/x/y", Known: true}, true},
		{"/proc/self/cwd/../../dev/stdin", Dir{Path: "/x/y/z", Known: true}, false},
		{"/proc/self/task/12/root/../dev/stdin", known, true},
		{"/proc/self/root/../tmp/x.py", known, false},
		{"dev/stdin", Dir{Path: "/", Known: true}, true},
		{"dev/stdin", Dir{}, true},
		{"dev/fd/0", Dir{}, true},
		{"./dev/./stdin", Dir{}, true},
		{"proc/self/fd/0", Dir{}, true},
		{"proc/self/root/../dev/stdin", Dir{}, true},
		{"../../dev/stdin", Dir{}, true},
		{"0", Dir{}, true},
		{"10", Dir{}, true},
		{"x/fd/3", Dir{}, true},
		// relative script paths in a directory the reader does not know are no alias
		{"../tools/gen.py", Dir{}, false},
		{"build/fd/gen.py", Dir{}, false},
		{"fd/gen.py", Dir{}, false},
		{"stdin_reader.py", Dir{}, false},
		{"../stdin.py", Dir{}, false},
		{"./tools/fd10.py", Dir{}, false},
		{"../..", Dir{}, false},
		{"tools/stdin/run.py", Dir{}, false},
		{"/dev/null", known, false},
		{"/dev/stdout", known, false},
		{"/dev/stdin2", known, false},
		{"/dev/fd", known, false},
		{"/proc/self/fd", known, false},
		{"/proc/self/fd/", known, false},
		{"/proc/self/status", known, false},
		{"/proc/self/cwd/script.py", known, false},
		{"/tmp/dev/stdin", known, false},
		{"dev/stdin", known, false},
		{"./dev/stdin", known, false},
		{"stdin", known, false},
		{"script.py", Dir{}, false},
		{"", known, false},
	} {
		if got := fdAliasPath(c.p, c.dir); got != c.alias {
			t.Errorf("fdAliasPath(%q, %+v) = %v, want %v", c.p, c.dir, got, c.alias)
		}
	}
}

// TestInterpreterAliasSpellingsAreUnreadable: a program piped to an interpreter whose script operand is any spelling of the
// alias is unreadable, as /dev/stdin itself is.
func TestInterpreterAliasSpellingsAreUnreadable(t *testing.T) {
	for _, in := range []string{"python3", "node", "perl", "ruby", "php", "lua"} {
		for _, p := range []string{"/dev/stdin", "/dev/./stdin", "//dev/stdin", "/dev/../dev/stdin", "/dev//fd//0", "/proc/self/root/dev/stdin"} {
			cmd := "printf x | " + in + " " + p
			if _, err := Analyze(cmd, "/work"); err == nil {
				t.Errorf("%q is read; the interpreter reads the pipe", cmd)
			}
		}
		if _, err := Analyze("printf x | "+in+" ./dev/stdin", "/work"); err != nil {
			t.Errorf("%s with a script file named dev/stdin is refused: %v", in, err)
		}
	}
	for _, cmd := range []string{"cd /dev; printf x | python3 stdin", "cd /dev; printf x | python3 /proc/self/cwd/stdin", "cd /proc/self/fd; printf x | python3 0"} {
		if _, err := Analyze(cmd, "/work"); err == nil {
			t.Errorf("%q is read; the relative alias is the pipe", cmd)
		}
	}
	for _, cmd := range []string{"printf x | source /dev/./stdin", "printf x | . //dev/stdin", "printf x | bash /dev/./stdin", "printf x | sh /dev//stdin",
		"printf x | bash </dev/./stdin", "cd /dev; printf x | bash <stdin", "sed -f /dev/./stdin x.txt", "awk -f //dev/stdin x.txt"} {
		if _, err := Analyze(cmd, "/work"); err == nil {
			t.Errorf("%q is read; the operand or redirection is the pipe", cmd)
		}
	}
}

// TestShellStdinOption (CRW-894, d2): with -s the operands are positional parameters and the program is read from standard input;
// they are no script file. -c wins over -s.
func TestShellStdinOption(t *testing.T) {
	for _, sh := range []string{"bash", "sh", "dash", "zsh", "ash", "mksh", "hush", "busybox ash", "busybox sh"} {
		for _, c := range []struct {
			cmd        string
			unreadable bool
		}{
			{"printf x | " + sh + " -s safe.sh", true},
			{"printf x | " + sh + " -s -- safe.sh", true},
			{"printf x | " + sh + " -xs safe.sh", true},
			{"printf x | " + sh + " -sx safe.sh", true},
			{"printf x | " + sh + " +s safe.sh", true},
			{"printf x | " + sh + " -s", true},
			{"printf x | " + sh + " -os posix safe.sh", true},
			{"printf x | " + sh + " -oc posix 'echo hi'", true},
			{sh + " -s", true},
			{"printf x | bash -c '" + sh + " -s safe.sh'", true},
			{"printf x | exec -a x " + sh + " -s safe.sh", true},
			{sh + " -s safe.sh arg <<< ':'", false},
			{sh + " -s safe.sh <<'EOF'\n:\nEOF", false},
			{sh + " -sc 'echo hi' safe.sh", false},
			{sh + " -cs 'echo hi' safe.sh", false},
			{"printf x | " + sh + " safe.sh", false},
			{"printf x | " + sh + " -x safe.sh", false},
			{"printf x | " + sh + " -- safe.sh", false},
		} {
			_, err := Analyze(c.cmd, "/work")
			if got := err != nil; got != c.unreadable {
				t.Errorf("%q: unreadable=%v, want %v (%v)", c.cmd, got, c.unreadable, err)
			}
		}
	}
	// the heredoc program is walked, and safe.sh is no script record
	res, err := Analyze("bash -s safe.sh <<'EOF'\nrm -rf ../repo\nEOF", "/work")
	if err != nil {
		t.Fatal(err)
	}
	sawRm := false
	for _, e := range res.Execs {
		if e.Kind == KindScriptFile {
			t.Errorf("script record %q for a -s operand", e.Script.Value)
		}
		if e.Name == "rm" {
			sawRm = true
		}
	}
	if !sawRm {
		t.Error("the here-document program of bash -s is not walked")
	}
}

// TestPythonModuleIsRefused (CRW-894, fix round 3): python -m MODULE is refused, json.tool included. python -m puts the working
// directory first on the module search path, and json.tool imports argparse, re, shutil, inspect and more through it, so no
// text proves what runs (the issue's allow control for json.tool is withdrawn; known-defects/CRW-894.md). The reading with no
// directory refuses it too.
func TestPythonModuleIsRefused(t *testing.T) {
	for _, cmd := range []string{
		"printf x | python3 -m json.tool",
		"printf x | python3 -mjson.tool",
		"printf x | python3 -B -m json.tool",
		"printf x | python3 -Bm json.tool",
		"printf x | python3 -m json.tool --sort-keys --no-ensure-ascii",
		"printf x | python3 -m json.tool --indent 2",
		"printf x | python3.11 -m json.tool",
		"python3 -m json.tool",
		"printf x | python3 -m json.tool in.json",
		"printf x | python3 -m json.tool -c 'import os'",
		"printf x | python3 -m json",
		"printf x | python3 -m json.tool.evil",
		"printf x | python3 -m pdb",
		"printf x | python3 -m code",
		"printf x | python3 -m runpy",
		"printf x | python3 -m http.server",
		"printf x | python3 -m",
		"printf x | python3 -m \"$M\"",
		"printf x | python3 -c 'print(1)' -m json.tool",
		"cat in.json | python3 -m json.tool > out.json",
	} {
		if _, err := Analyze(cmd, t.TempDir()); err == nil {
			t.Errorf("%q is read; python -m is refused", cmd)
		}
		if _, err := Analyze(cmd, "/work"); err == nil {
			t.Errorf("%q is read in a directory that does not exist; python -m is refused", cmd)
		}
		if _, err := AnalyzeNoDir(cmd); err == nil {
			t.Errorf("%q is read with no directory; python -m is refused", cmd)
		}
	}
}

// TestShebangShellNames: the shells a direct script can name are the reader's shells (githubPostShellName takes this list).
func TestShebangShellNames(t *testing.T) {
	for _, n := range []string{"sh", "bash", "dash", "zsh", "ksh", "ash", "mksh", "hush", "pdksh", "oksh", "posh", "yash", "rbash"} {
		if !IsShell(n) {
			t.Errorf("%s is not a shell", n)
		}
	}
	for _, n := range []string{"python3", "perl", "busybox", "env", "cat", ""} {
		if IsShell(n) {
			t.Errorf("%s is a shell", n)
		}
	}
}

// TestUnknownScriptOperandBehindStandardInput: a script operand the reader cannot evaluate may name the stdin alias. The
// interpreters the reader models refuse a word they cannot evaluate; the opaque ones (php, lua) do so when standard input is a
// pipe, a here-document or a here-string, and otherwise take it for a script file they do not judge.
func TestUnknownScriptOperandBehindStandardInput(t *testing.T) {
	for _, in := range []string{"python3", "node", "perl", "ruby", "php", "lua", "tclsh"} {
		opaque := isOpaqueInterpreter(in)
		for _, c := range []struct {
			cmd        string
			unreadable bool
		}{
			{`printf x | ` + in + ` "$S"`, true},
			{`printf x | ` + in + ` "$(echo /dev/stdin)"`, true},
			{in + ` "$S" <<< x`, true},
			{`printf x | ` + in + ` script.py`, false},
			{`S=script.py; printf x | ` + in + ` "$S"`, false},
			{in + ` "$S"`, !opaque},
		} {
			_, err := Analyze(c.cmd, "/work")
			if got := err != nil; got != c.unreadable {
				t.Errorf("%q: unreadable=%v, want %v (%v)", c.cmd, got, c.unreadable, err)
			}
		}
	}
}

// TestInterpreterReplOptionReadsTheInput: python -i and node -i read commands from standard input after their program, so a
// pipe, a here-document or a here-string is a program whatever the script operand or the -c string shows.
func TestInterpreterReplOptionReadsTheInput(t *testing.T) {
	for _, c := range []struct {
		cmd        string
		unreadable bool
	}{
		{"printf x | python3 -i safe.py", true},
		{"printf x | python3 -i -c pass", true},
		{"printf x | python3 -ic pass", true},
		{"printf x | python3 -Bi safe.py", true},
		{"printf x | python3 -i -m json.tool", true},
		{"python3 -i safe.py <<< x", true},
		{"python3 -i safe.py <<'EOF'\nx\nEOF", true},
		{"printf x | node -e 0 -i", true},
		{"printf x | node -i safe.js", true},
		{"python3 -i safe.py", false},
		{"python3 -i safe.py < prog.txt", false},
		{"printf x | python3 safe.py", false},
		{"printf x | python3 -c pass -i", true}, // after -c the word is an argument for python, the reader keeps reading options: refused
		{"printf x | node safe.js", false},
	} {
		_, err := Analyze(c.cmd, "/work")
		if got := err != nil; got != c.unreadable {
			t.Errorf("%q: unreadable=%v, want %v (%v)", c.cmd, got, c.unreadable, err)
		}
	}
}

// TestAnalyzeNoDirRelativeAliases: the reading that is given no directory at all leaves the directory judgments (a relative
// script path that may be a stdin alias) to the readings that have one, but a directory that becomes unknown inside the text,
// and an absolute alias spelling, are judged.
func TestAnalyzeNoDirRelativeAliases(t *testing.T) {
	if _, err := AnalyzeNoDir("printf x | python3 ./dev/stdin"); err != nil {
		t.Errorf("AnalyzeNoDir refuses a relative script path: %v", err)
	}
	for _, c := range []string{"cd \"$D\"; printf x | python3 dev/stdin", "printf x | python3 /dev/./stdin"} {
		if _, err := AnalyzeNoDir(c); err == nil {
			t.Errorf("AnalyzeNoDir reads %q", c)
		}
	}
}

// TestAnalyzeNoDirKeepsNoDirectoryThroughStatements: a statement that does not change the directory (an if, a case, a loop)
// leaves the reading with no directory as it was, so an ordinary script path is not judged after it; a statement that changes the
// directory to a place the reader cannot name (in any branch, or in a loop body) makes the reading refuse a path that may be an alias.
func TestAnalyzeNoDirKeepsNoDirectoryThroughStatements(t *testing.T) {
	for _, c := range []struct {
		cmd        string
		unreadable bool
	}{
		{"printf x | python3 stdin", false},
		{"if true; then :; fi; printf x | python3 stdin", false},
		{"if true; then :; else :; fi; printf x | python3 fd/0", false},
		{"case x in x) :;; esac; printf x | python3 stdin", false},
		{"for i in 1 2; do :; done; printf x | python3 stdin", false},
		{"if true; then cd \"$D\"; fi; printf x | python3 dev/stdin", true},
		{"if true; then cd \"$A\"; else cd \"$B\"; fi; printf x | python3 dev/stdin", true},
		{"for i in 1; do cd \"$D\"; done; printf x | python3 dev/stdin", true},
		{"if true; then :; fi; printf x | python3 /dev/./stdin", true},
	} {
		_, err := AnalyzeNoDir(c.cmd)
		if got := err != nil; got != c.unreadable {
			t.Errorf("AnalyzeNoDir(%q): unreadable=%v, want %v (%v)", c.cmd, got, c.unreadable, err)
		}
	}
}

// TestLoopThatKeepsTheDirectory: a loop (or a case arm that falls through) whose condition and body change nothing about the
// directory leaves a known directory known, so an ordinary script named stdin or fd/0 there is the script; a cd, pushd, popd, or a
// function that may run one, in the condition or the body makes the directory unknown and a path that may be an alias is refused.
func TestLoopThatKeepsTheDirectory(t *testing.T) {
	for _, c := range []struct {
		cmd        string
		unreadable bool
	}{
		{"for i in 1 2; do :; done; printf x | python3 stdin", false},
		{"while false; do :; done; printf x | python3 fd/0", false},
		{"for i in 1; do :; done; printf x | node stdin", false},
		{"until true; do :; done; printf x | python3 stdin", false},
		{"for i in 1 2; do echo \"$i\"; n=$i; done; printf x | python3 stdin", false},
		{"for ((i=0; i<2; i++)); do :; done; printf x | python3 dev/stdin", false},
		{"case x in x) : ;& y) : ;; esac; printf x | python3 stdin", false},
		{"for i in 1 2; do cd \"$D\"; done; printf x | python3 dev/stdin", true},
		{"for i in 1; do cd /; done; printf x | python3 dev/stdin", true},
		{"while false; do pushd \"$D\"; done; printf x | python3 dev/stdin", true},
		{"until true; do popd; done; printf x | python3 dev/stdin", true},
		{"f() { cd \"$D\"; }; for i in 1; do f; done; printf x | python3 dev/stdin", true},
		{"while cd \"$D\"; do break; done; printf x | python3 dev/stdin", true},
		{"for i in 1; do $C \"$D\"; done; printf x | python3 dev/stdin", true},
		{"case x in x) cd \"$D\" ;& y) : ;; esac; printf x | python3 dev/stdin", true},
		{"for i in 1 2; do :; done; printf x | python3 /dev/./stdin", true},
	} {
		_, err := Analyze(c.cmd, "/work")
		if got := err != nil; got != c.unreadable {
			t.Errorf("Analyze(%q, /work): unreadable=%v, want %v (%v)", c.cmd, got, c.unreadable, err)
		}
	}
}
