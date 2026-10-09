package shellir

import "testing"

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

// TestPythonModuleJSONTool (CRW-894, d4): python -m json.tool runs no program: the issue's control is read. Any other module,
// an operand of json.tool, an option outside the closed list, and a module together with a program are unreadable.
func TestPythonModuleJSONTool(t *testing.T) {
	for _, c := range []struct {
		cmd        string
		unreadable bool
	}{
		{"printf x | python3 -m json.tool", false},
		{"printf x | python3 -mjson.tool", false},
		{"printf x | python3 -B -m json.tool", false},
		{"printf x | python3 -Bm json.tool", false},
		{"printf x | python3 -m json.tool --sort-keys --no-ensure-ascii", false},
		{"printf x | python3 -m json.tool --indent 2", false},
		{"printf x | python3 -m json.tool --indent=4", false},
		{"printf x | python3 -m json.tool --tab --compact --json-lines", false},
		{"printf x | python3 -m json.tool --no-indent", false},
		{"printf x | python3.11 -m json.tool", false},
		{"python3 -m json.tool", false},
		{"printf x | python3 -m json.tool in.json", true},
		{"printf x | python3 -m json.tool - out.json", true},
		{"printf x | python3 -m json.tool --indent", true},
		{"printf x | python3 -m json.tool --indent x", true},
		{"printf x | python3 -m json.tool --indent=-1", true},
		{"printf x | python3 -m json.tool -c 'import os'", true},
		{"printf x | python3 -m json.tool --unknown", true},
		{"printf x | python3 -m json", true},
		{"printf x | python3 -m json.tool.evil", true},
		{"printf x | python3 -m pdb", true},
		{"printf x | python3 -m code", true},
		{"printf x | python3 -m runpy", true},
		{"printf x | python3 -m http.server", true},
		{"printf x | python3 -m", true},
		{"printf x | python3 -m \"$M\"", true},
		{"printf x | python3 -c 'print(1)' -m json.tool", true},
		{"printf x | python3 -m json.tool -c 'print(1)'", true},
		{"printf x | python3 -m json.tool \"$A\"", true},
	} {
		_, err := Analyze(c.cmd, "/work")
		if got := err != nil; got != c.unreadable {
			t.Errorf("%q: unreadable=%v, want %v (%v)", c.cmd, got, c.unreadable, err)
		}
	}
	// the module is no program: no inline record, no script record
	res, err := Analyze("printf x | python3 -m json.tool", "/work")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Execs {
		if e.Inline != nil || e.Kind == KindScriptFile {
			t.Errorf("python -m json.tool produced a program record: %+v", e)
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
