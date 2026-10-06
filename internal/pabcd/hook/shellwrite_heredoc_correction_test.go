package hook

import (
	"slices"
	"strings"
	"testing"
)

// CRW-765 correction generation 2. The parent's ruling found that the first correction still missed a here-document
// program whenever the header carried anything besides the << word, and over-blocked a non-interpreter body. These
// rows are the ruling's red-first list; on ebbb6d00 each denied row was allowed and the note control was denied.
//
// The general rule: the header is read the way the shell does - cut at pipes and control operators and judge the
// command the here-document is attached to, with every redirection and its target word dropped wherever it stands,
// before and after the verb. A here-document attached to a command that names one of the interpreters, which the
// reader cannot positively decide, is denied (fail closed).

// shellWriteHeredocCorrectionHeaderRows are the header-shape rows: a redirection, a pipe or a control operator beside
// the << word must not hide the interpreter or turn a redirection into the verb.
func shellWriteHeredocCorrectionHeaderRows(mem string) []struct{ name, command string } {
	return []struct{ name, command string }{
		{"a redirect after the operator", "bash <<'EOF' 2>/dev/null\necho x > " + mem + "/a\nEOF"},
		{"a pipe after the operator", "bash <<'EOF' | cat\necho x > " + mem + "/a\nEOF"},
		{"a redirect before the verb", "2>&1 python3 - <<'EOF'\nopen('" + mem + "/a','w')\nEOF"},
		{"an input redirect before the operator", "bash </dev/null <<'EOF'\necho x > " + mem + "/a\nEOF"},
		{"a redirect and the interpreter after the operator", "<<'EOF' >out.log python3 -\nopen('" + mem + "/a','w')\nEOF"},
		{"a redirect then a pipe after the operator", "python3 - <<'EOF' 2>&1 | tee x.log\nopen('" + mem + "/a','w')\nEOF"},
		{"an append redirect after the operator", "bash <<'EOF' >>out.log\necho x > " + mem + "/a\nEOF"},
		{"a stderr-to-stdout redirect after the operator", "node <<'EOF' 2>&1\nrequire('fs').writeFileSync('" + mem + "/a','x')\nEOF"},
	}
}

// TestShellWriteHeredocCorrectionReads is the reader case: each header shape names its path.
func TestShellWriteHeredocCorrectionReads(t *testing.T) {
	const mem = "/h/memories"
	for _, row := range shellWriteHeredocCorrectionHeaderRows(mem) {
		t.Run(row.name, func(t *testing.T) {
			if got := ShellWriteDestinations(row.command); !slices.Contains(got, mem+"/a") {
				t.Errorf("%q named %q, want %q", row.command, got, mem+"/a")
			}
		})
	}
}

// TestShellWriteHeredocCorrectionGate is the gate case: each header shape is denied as the shell surface with the path
// as the target, and the escaped -c row is an unreadable attempt.
func TestShellWriteHeredocCorrectionGate(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, row := range shellWriteHeredocCorrectionHeaderRows(root) {
		t.Run(row.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env)
			if got.Surface != "shell" || got.Target != root+"/a" {
				t.Errorf("%+v, want the shell surface and %s", got, root+"/a")
			}
		})
	}
	// A double-quoted -c program whose body the inner shell expands: the token holds an escaped $ the fail-closed walk
	// must still read through its shell-unescaped reading.
	for _, c := range []struct{ name, command string }{
		{"an escaped $ the outer shell unescapes", "bash -c \"python3 <<EOF\nopen('\\$P','w')\nEOF\""},
		{"a plain $ the outer shell expands", "P=" + root + "/a bash -c \"python3 <<EOF\nopen($P,'w')\nEOF\""},
	} {
		t.Run(c.name, func(t *testing.T) {
			want := "(a program the gate cannot read: " + shellWriteHeredocWhatWant + ")"
			if got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env); got.Surface != "shell" || got.Target != want {
				t.Errorf("%q: %+v, want the shell surface and %s", c.command, got, want)
			}
		})
	}
}

// TestShellWriteHeredocCorrectionControls is the invariant case: a non-interpreter body is data, a script operand means
// the body is the script's input, a shell parsing without running reads no program, and a note body that documents a
// command with an expansion is not scanned as a command.
func TestShellWriteHeredocCorrectionControls(t *testing.T) {
	const mem = "/h/memories"
	for _, c := range []struct{ name, command string }{
		{"a note quoting the memories path", "cat > note.md <<'EOF'\n" + mem + " is where notes live\nEOF"},
		{"a script operand means the body is data", "python3 script.py <<'EOF' 2>/dev/null\nopen('" + mem + "/a','w')\nEOF"},
		{"a shell parsing without running reads no program", "bash -n <<'EOF'\necho x > " + mem + "/a\nEOF"},
		{"a note documenting a -c command with an expansion", "cat > note.md <<'EOF'\nbash -c \"python3 <<X\nopen('$P','w')\nX\"\nEOF"},
		{"a note documenting a plain -c command", "cat > note.md <<'EOF'\nbash -c \"echo x > " + mem + "/a\"\nEOF"},
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
	// The note row names only its own destination, unchanged from the oracle.
	if got := ShellWriteDestinations("cat > note.md <<'EOF'\n" + mem + "\nEOF"); !slices.Equal(got, []string{"note.md"}) {
		t.Errorf("the note row named %q, want [note.md]", got)
	}
}

// TestShellWriteHeredocCorrectionUncertain is the fail-closed default: a here-document attached to a command that names
// one of the interpreters, whose program source an option the reader does not model leaves undecidable, is denied; a
// non-interpreter command with the same option is untouched.
func TestShellWriteHeredocCorrectionUncertain(t *testing.T) {
	const mem = "/h/memories"
	for _, c := range []struct{ name, command string }{
		{"a python option before a script operand", "python3 -Z script.py <<'EOF'\nopen('" + mem + "/a','w')\nEOF"},
		{"a shell long option before a script operand", "bash --norc script.sh <<'EOF'\necho x > " + mem + "/a\nEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := shellWriteHeredocUnreadable(c.command); !ok || got != shellWriteHeredocWhatWant {
				t.Errorf("%q: got %q, %v; want %q, true", c.command, got, ok, shellWriteHeredocWhatWant)
			}
		})
	}
	// A non-interpreter command with the same shape is untouched.
	if got, ok := shellWriteHeredocUnreadable("cat -A <<'EOF'\n" + mem + "\nEOF"); ok {
		t.Errorf("a cat row reported unreadable %q", got)
	}
}

// TestShellWriteHeredocCorrectionDepthParity pins that the fail-closed walk stops at the same depth and budget as the
// destination walk: a program past the limit is denied rather than walked to the bottom.
func TestShellWriteHeredocCorrectionDepthParity(t *testing.T) {
	nested := func(levels int) string {
		body := "echo x > /m/a"
		for i := 0; i < levels; i++ {
			tag := "C" + string(rune('0'+i%10))
			body = "bash <<'" + tag + "'\n" + body + "\n" + tag
		}
		return body
	}
	if got, ok := shellWriteHeredocUnreadable(nested(9)); !ok || got != shellWriteHeredocWhatWant {
		t.Errorf("nine levels: got %q, %v; want %q, true", got, ok, shellWriteHeredocWhatWant)
	}
	if got, ok := shellWriteHeredocUnreadable(nested(4)); ok {
		t.Errorf("four levels reported unreadable %q", got)
	}
	// A chain of nested -c levels stops at the depth limit rather than descending without bound: each level wraps the
	// one below, so a chain past the limit is denied even though it holds no here-document of its own.
	deep := "echo x > /m/a"
	for i := 0; i < 12; i++ {
		deep = "bash -c \"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(deep) + "\""
	}
	if got, ok := shellWriteHeredocUnreadable(deep); !ok || got != shellWriteHeredocWhatWant {
		t.Errorf("a deep -c chain: got %q, %v; want %q, true", got, ok, shellWriteHeredocWhatWant)
	}
}
