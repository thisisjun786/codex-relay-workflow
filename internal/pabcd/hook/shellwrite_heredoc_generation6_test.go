package hook

import (
	"slices"
	"strings"
	"testing"
)

// CRW-765 correction generation 6. Generation 5 still let an interpreter run the here-document body as its program while
// the reader kept the body as data, because the rule kept the generation-1 exception (data when the interpreter runs an
// inline or script program) and because awk was not in the interpreter list: bash -c 'bash' <<'EOF', sh -c 'exec sh'
// <<'EOF', python3 -m code <<'EOF', a node -e program that evaluates its standard input, and awk -f /dev/stdin <<'EOF'
// all ran the body. The ruling closes both:
//
//	H1 a here-document attached to a modelled interpreter verb is ALWAYS read as that interpreter's program, whatever
//	   its flags (-c, -e, -m, -i, -n, -s and the rest), operands (a script file included) or environment, and the
//	   existing write reading applies to that body; the earlier data-with-a-script-operand exception is gone.
//	H2 a here-document attached to an interpreter whose language the reader does not model (awk, gawk, mawk, nawk, sed
//	   in its -f form, perl, ruby, php, lua, Rscript, tclsh, osascript) is unreadable and denied; rule R1's data list
//	   stays data.
//
// The rows below are the ruling's red-first list; on 9321d3a8 every denied row named nothing and passed.

// shellWriteHeredocGateDenied reports whether a gate result denies the shell surface, whatever reason the reader that
// decided it gave. Two readers answer for a shell command: CRW-726's guard refuses a here-document a listed shell reads
// as its program and a program position the outer shell builds at run time, and this issue's reader refuses every other
// interpreter's here-document. Both deny; the merge keeps CRW-726's own reason where the two overlap.
func shellWriteHeredocGateDenied(got MemoryWriteAttempt, path string) bool {
	if got.Surface != "shell" {
		return false
	}
	return got.Target == path || strings.HasPrefix(got.Target, "(a program the gate cannot read: ")
}

// shellWriteHeredocGeneration6Rows are the ruling's shapes: an interpreter whose own program comes from a flag, an
// operand or the environment, and an interpreter the reader does not model.
func shellWriteHeredocGeneration6Rows(mem string) []struct{ name, command string } {
	sh := "echo x > " + mem + "/a"
	js := "require('fs').writeFileSync('" + mem + "/a','x')"
	return []struct{ name, command string }{
		{"bash -c runs another bash that reads the here-document", "bash -c 'bash' <<'EOF'\n" + sh + "\nEOF"},
		{"sh -c execs a shell that reads the here-document", "sh -c 'exec sh' <<'EOF'\n" + sh + "\nEOF"},
		{"python3 -m runs a module that reads its program from standard input", "python3 -m code <<'EOF'\nopen('" + mem + "/a','w')\nEOF"},
		{"node -e evaluates its standard input", "node -e \"process.stdin.on('data', d => new Function(d.toString())())\" <<'EOF'\n" + js + "\nEOF"},
		{"awk reads its program from standard input", "awk -f /dev/stdin <<'EOF'\nBEGIN { print \"x\" > \"" + mem + "/a\" }\nEOF"},
		{"perl reads its program from standard input", "perl <<'EOF'\nopen(my $fh, '>', '" + mem + "/a')\nEOF"},
		{"ruby reads its program from standard input", "ruby <<'EOF'\nFile.write('" + mem + "/a', 'x')\nEOF"},
	}
}

// TestShellWriteHeredocGeneration6Denied is the denied case: each shape is denied as the shell surface, by naming the
// protected path (the program is read) or by the fail-closed reason.
func TestShellWriteHeredocGeneration6Denied(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, row := range shellWriteHeredocGeneration6Rows(root) {
		t.Run(row.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env)
			if !shellWriteHeredocGateDenied(got, root+"/a") {
				t.Errorf("%q: %+v, want a deny naming the protected path or the fail-closed reason", row.command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration6Controls is the invariant case: a harmless interpreter program, the data list, a
// commit message that mentions an interpreter and the generation-5 controls this rule does not change stay allowed.
func TestShellWriteHeredocGeneration6Controls(t *testing.T) {
	const mem = "/h/memories"
	for _, c := range []struct{ name, command string }{
		{"a harmless python program from standard input", "python3 - <<'EOF'\nprint('hi')\nEOF"},
		{"a data here-document for cat", "cat > note.md <<'EOF'\n" + mem + "\nEOF"},
		{"a data here-document for jq", "jq . <<'EOF'\n" + mem + "\nEOF"},
		{"a data here-document for sort", "sort <<'EOF'\n" + mem + "\nEOF"},
		{"a commit message that mentions an interpreter", "git commit -F - <<'EOF'\nrun python3 -c pass\nEOF"},
		{"a read loop with no interpreter outside the body", "while read l; do echo $l; done <<'EOF'\n" + mem + "\nEOF"},
		{"a body alone naming an interpreter", "cat <<'EOF' > x.md\nrun python3 -c pass\nEOF"},
		{"a body written to a script file then run", "cat > x.py <<'EOF'\nopen('" + mem + "/a','w')\nEOF\npython3 x.py"},
		{"an unmodelled verb naming no interpreter", "mytool --input - <<'EOF'\n" + mem + "\nEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ShellWriteDestinations(c.command); slices.Contains(got, mem+"/a") {
				t.Errorf("%q named the protected path: %q", c.command, got)
			}
			if got, ok := shellWriteHeredocUnreadable(c.command); ok {
				t.Errorf("%q reported unreadable %q", c.command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration6InterpreterAlwaysReads pins rule H1 on the reader itself: a modelled interpreter's
// here-document is a program whatever its flags and operands, so the write reading applies to the body.
func TestShellWriteHeredocGeneration6InterpreterAlwaysReads(t *testing.T) {
	const mem = "/h/memories"
	for _, c := range []struct{ name, command string }{
		{"python -c", "python3 -c 'print(1)' <<'EOF'\nopen('" + mem + "/a','w')\nEOF"},
		{"python -m", "python3 -m json.tool <<'EOF'\nopen('" + mem + "/a','w')\nEOF"},
		{"python script operand", "python3 script.py <<'EOF'\nopen('" + mem + "/a','w')\nEOF"},
		{"node -e", "node -e 'console.log(1)' <<'EOF'\nrequire('fs').writeFileSync('" + mem + "/a','x')\nEOF"},
		{"node script operand", "node script.js <<'EOF'\nrequire('fs').writeFileSync('" + mem + "/a','x')\nEOF"},
		{"bash -c", "bash -c 'echo hi' <<'EOF'\necho x > " + mem + "/a\nEOF"},
		{"bash -n", "bash -n <<'EOF'\necho x > " + mem + "/a\nEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ShellWriteDestinations(c.command); !slices.Contains(got, mem+"/a") {
				t.Errorf("%q named %q, want %q", c.command, got, mem+"/a")
			}
		})
	}
}

// TestShellWriteHeredocGeneration6UnmodelledInterpreter pins rule H2: an unmodelled interpreter's here-document is
// refused, and a verb that is not one of them is not.
func TestShellWriteHeredocGeneration6UnmodelledInterpreter(t *testing.T) {
	for _, c := range []struct {
		verb string
		args []string
		want bool
	}{
		{"awk", nil, true},
		{"gawk", []string{"{print}"}, true},
		{"mawk", nil, true},
		{"nawk", nil, true},
		{"perl", nil, true},
		{"ruby", nil, true},
		{"php", nil, true},
		{"lua", nil, true},
		{"rscript", nil, true},
		{"tclsh", nil, true},
		{"osascript", nil, true},
		{"sed", []string{"-f", "-"}, true},
		{"sed", []string{"--file=/dev/stdin"}, true},
		{"sed", []string{"-f", "script.sed"}, true},
		{"sed", []string{"s/a/b/"}, false},
		{"cat", nil, false},
		{"bash", nil, false},
		{"python3", nil, false},
		{"mytool", nil, false},
	} {
		t.Run(c.verb+" "+strings.Join(c.args, " "), func(t *testing.T) {
			if got := shellWriteHeredocUnmodelledInterpreter(c.verb, c.args); got != c.want {
				t.Errorf("%q %q: got %v, want %v", c.verb, c.args, got, c.want)
			}
		})
	}
}
