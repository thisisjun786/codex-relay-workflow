package hook

import (
	"testing"
	"unicode/utf16"
)

// TestShellWriteHeredocGeneration4HeaderProof pins rules G1 and G3 on the header itself. Correction 8's rule U1 splits
// the line at the control operators, so the two rows that carry a pipe or a second command are no longer unproven: the
// command that owns the here-document is read instead. Those two rows are listed in the pull request body.
func TestShellWriteHeredocGeneration4HeaderProof(t *testing.T) {
	for _, c := range []struct {
		name     string
		command  string
		unproven bool
	}{
		{"one plain command", "python3 <<'EOF'\nopen('x','w')\nEOF", false},
		{"a redirect after the operator", "bash <<'EOF' 2>/dev/null\nx\nEOF", false},
		{"a redirect before the verb", "2>&1 python3 - <<'EOF'\nx\nEOF", false},
		{"a backslash in the command word", "bas\\h <<'EOF'\nx\nEOF", true},
		{"a line continuation inside the word", "bas\\\nh <<'EOF'\nx\nEOF", true},
		{"a continuation after the header", "bash <<'EOF' \\\n2>/dev/null\nx\nEOF", true},
		{"a pipe after the operator", "bash <<'EOF' | cat\nx\nEOF", false},
		{"a second command on the line", "cat <<'A'; python3 <<'B'\nx\nA\ny\nB", false},
		{"a function defined with the keyword", "function f { bash; }; f <<'EOF'\nx\nEOF", true},
		{"a name() function definition", "f() { bash; } <<'EOF'\nx\nEOF", true},
		{"a subshell-body function definition", "f() ( bash ) <<'EOF'\nx\nEOF", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			hs := shellWriteHeredocs(utf16.Encode([]rune(c.command)))
			if len(hs) == 0 {
				t.Fatalf("%q collected no here-document", c.command)
			}
			if got := !shellWriteHeredocHeaderProven(hs[0]); got != c.unproven {
				t.Errorf("%q unproven=%v, want %v", c.command, got, c.unproven)
			}
		})
	}
}

// TestShellWriteHeredocGeneration4Canonical pins rule G2: the canonical text drops quotes and backslashes and lowercases
// ASCII, so an interpreter spelling the shell unescapes is still found.
func TestShellWriteHeredocGeneration4Canonical(t *testing.T) {
	for _, s := range []string{"bas\\h <<'EOF'", "pytho\\n3 <<'EOF'", "PYTHON3 <<'EOF'", "/usr/bin/BASH <<'EOF'", "nod\\e <<'EOF'"} {
		t.Run(s, func(t *testing.T) {
			if !shellWriteHeredocNamesInterpreter(utf16.Encode([]rune(s))) {
				t.Errorf("%q named no interpreter, want one", s)
			}
		})
	}
	for _, s := range []string{"cat <<'EOF'", "tee /m/a", "grep -n x notes.md"} {
		t.Run(s, func(t *testing.T) {
			if shellWriteHeredocNamesInterpreter(utf16.Encode([]rune(s))) {
				t.Errorf("%q named an interpreter, want none", s)
			}
		})
	}
}
