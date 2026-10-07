package hook

import (
	"slices"
	"testing"
)

// CRW-765 correction generation 4. The independent review (GLM 5.3, each trigger run in a real shell) and the parent's
// probe found six more here-documents that generation 3 read as data although the shell feeds them to an interpreter as
// its program. The correction states general rules instead of more shapes:
//
//	G1 the header's physical line must hold no backslash at all, the line before it must not end with a backslash, and
//	   outside quotes it must hold no ;, &, |, && or || - one proven command on one line, or the header is unprovable.
//	G2 rule 3's interpreter test reads the canonical text: quotes and backslashes (and backslash-newline) deleted and
//	   ASCII lowercased, so a spelling the shell unescapes is still seen.
//	G3 a function definition in any form (name(), function name, function name()) makes the header unprovable.
//	G4 a << inside a double-quoted $( ... ) or backtick span is collected as a here-document, or the text is unprovable.
//
// The rows below are the ruling's red-first list; on 3f2922f6 each denied row was allowed.

// shellWriteHeredocGeneration4Rows are the ruling's six groups, each feeding the interpreter the here-document as its
// program with the body writing the memories path.
func shellWriteHeredocGeneration4Rows(mem string) []struct{ name, command string } {
	py := "open('" + mem + "/a','w')"
	sh := "echo x > " + mem + "/a"
	js := "require('fs').writeFileSync('" + mem + "/a','x')"
	return []struct{ name, command string }{
		{"a backslash in the command word, bash", "bas\\h <<'EOF'\n" + sh + "\nEOF"},
		{"a backslash in the command word, python3", "pytho\\n3 <<'EOF'\n" + py + "\nEOF"},
		{"a backslash in the command word, node", "nod\\e <<'EOF'\n" + js + "\nEOF"},
		{"a line continuation inside the command word", "bas\\\nh <<'EOF'\n" + sh + "\nEOF"},
		{"a line continuation after the header, bash", "bash <<'EOF' \\\n2>/dev/null\n" + sh + "\nEOF"},
		{"a line continuation after the header, python3", "python3 <<'EOF' \\\n2>/dev/null\n" + py + "\nEOF"},
		{"a function defined with the keyword", "function f { bash; }; f <<'EOF'\n" + sh + "\nEOF"},
		{"two commands on one header line", "cat <<'A'; python3 <<'B'\nnote\nA\n" + py + "\nB"},
		{"a here-document inside a double-quoted command substitution", "x=\"$(python3 <<'EOF'\n" + py + "\nEOF\n)\""},
	}
}

// TestShellWriteHeredocGeneration4Denied is the denied case: each header shape is denied as the shell surface, by naming
// the protected path (the program is read) or by the fail-closed reason.
func TestShellWriteHeredocGeneration4Denied(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, row := range shellWriteHeredocGeneration4Rows(root) {
		t.Run(row.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env)
			if got.Surface != "shell" {
				t.Fatalf("%q must be an attempt: %+v", row.command, got)
			}
			if got.Target != root+"/a" && got.Target != "(a program the gate cannot read: "+shellWriteHeredocWhatWant+")" {
				t.Errorf("%q named %q, want the protected path or the fail-closed reason", row.command, got.Target)
			}
		})
	}
}

// TestShellWriteHeredocGeneration4Controls is the invariant case: a script operand makes the body data (the ruling's
// P2), a body written to a script file then run is data, and the generation-3 controls stay allowed.
func TestShellWriteHeredocGeneration4Controls(t *testing.T) {
	const mem = "/h/memories"
	for _, c := range []struct{ name, command string }{
		{"a node script operand makes the body data", "node script.js <<'EOF'\nrequire('fs').writeFileSync('" + mem + "/a','x')\nEOF"},
		{"a body written to a script file then run", "cat > /tmp/x.py <<'EOF'\nopen('" + mem + "/a','w')\nEOF\npython3 /tmp/x.py"},
		{"a note quoting the memories path", "cat > note.md <<'EOF'\n" + mem + "\nEOF"},
		{"a python script operand", "python3 script.py <<'EOF' 2>/dev/null\nopen('" + mem + "/a','w')\nEOF"},
		{"a shell parsing without running", "bash -n <<'EOF'\necho x > " + mem + "/a\nEOF"},
		{"a read loop with no interpreter outside the body", "while read l; do echo $l; done <<'EOF'\n" + mem + "\nEOF"},
		{"a body alone naming an interpreter", "cat <<'EOF' > x.md\nrun python3 -c pass\nEOF"},
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
