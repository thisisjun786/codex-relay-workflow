package hook

import (
	"slices"
	"testing"
	"unicode/utf16"
)

// CRW-765 correction generation 3: the closed rule replaces the shape-by-shape header parser. A here-document is read as
// an interpreter's program only when the owning simple command is proven to be an interpreter that reads standard input;
// it is data (today's oracle behaviour) only when the owning simple command is proven to be a non-interpreter; and
// everything else fails closed when the command text outside the here-document bodies names an interpreter. The rows
// below are the ruling's red-first list; on 331a30b2 each denied row was allowed.

// shellWriteHeredocClosedRuleRows are the header shapes the ruling named: each feeds an interpreter the here-document as
// its program, and each was allowed on 331a30b2.
func shellWriteHeredocClosedRuleRows(mem string) []struct{ name, command string } {
	py := "open('" + mem + "/a','w')"
	sh := "echo x > " + mem + "/a"
	js := "require('fs').writeFileSync('" + mem + "/a','x')"
	return []struct{ name, command string }{
		{"a header comment after the operator", "python3 <<'EOF' # note\n" + py + "\nEOF"},
		{"a shell header comment", "bash <<'EOF' # comment\n" + sh + "\nEOF"},
		{"a node header comment", "node <<'EOF' # js\n" + js + "\nEOF"},
		{"the clobber redirect after the operator", "bash <<'EOF' >|out.txt\n" + sh + "\nEOF"},
		{"a redirect to a filename after the operator", "bash <<'EOF' >&out.txt\n" + sh + "\nEOF"},
		{"a process-substitution redirect target", "bash <<'EOF' >> >(cat)\n" + sh + "\nEOF"},
		{"a process-substitution input redirect", "bash < <(true) <<'EOF'\n" + sh + "\nEOF"},
		{"a command-substitution redirect target", "bash <<'EOF' >$(mktemp)\n" + sh + "\nEOF"},
		{"an fd-0 here-document", "bash 0<<'EOF'\n" + sh + "\nEOF"},
		{"an fd-numbered here-document", "bash /dev/fd/3 3<<'EOF'\n" + sh + "\nEOF"},
		{"a brace group", "{ python3 -; } <<'EOF'\n" + py + "\nEOF"},
		{"an if-then-fi group", "if true; then bash; fi <<'EOF'\n" + sh + "\nEOF"},
		{"a function definition then a call", "f() { bash; }; f <<'EOF'\n" + sh + "\nEOF"},
		{"a here-document inside a command substitution", "x=$(bash <<'EOF'\n" + sh + "\nEOF\n)"},
	}
}

// TestShellWriteHeredocClosedRuleDenied is the denied case: each header shape is denied, either by naming the protected
// path (the program is read) or by the fail-closed reason.
func TestShellWriteHeredocClosedRuleDenied(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, row := range shellWriteHeredocClosedRuleRows(root) {
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

// TestShellWriteHeredocClosedRuleControls is the invariant case: a proven non-interpreter command's body stays data, and
// a command that names no interpreter keeps today's behaviour.
func TestShellWriteHeredocClosedRuleControls(t *testing.T) {
	const mem = "/h/memories"
	for _, c := range []struct{ name, command string }{
		{"a note quoting the memories path", "cat > note.md <<'EOF'\n" + mem + "\nEOF"},
		{"a script operand makes the body data", "python3 script.py <<'EOF' 2>/dev/null\nopen('" + mem + "/a','w')\nEOF"},
		{"a shell parsing without running reads no program", "bash -n <<'EOF'\necho x > " + mem + "/a\nEOF"},
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

// TestShellWriteHeredocClosedRuleSimpleCommand pins the strict reader: the owning simple command is proven only when
// every word is literal and every operator is a here-document operator (fd 0 or none) or a redirection from the fixed
// set with a literal target.
func TestShellWriteHeredocClosedRuleSimpleCommand(t *testing.T) {
	proven := []struct{ name, segment string }{
		{"a plain delimiter", "python3 - <<EOF"},
		{"a backslash-escaped delimiter", "python3 <<\\EOF"},
		{"a quoted delimiter", "python3 <<'EOF'"},
		{"two operators", "python3 <<'A' <<'B'"},
		{"a redirect before the verb", "2>&1 python3 - <<EOF"},
		{"a redirect after the operator", "bash <<'EOF' 2>/dev/null"},
		{"a wrapper before the verb", "sudo python3 - <<EOF"},
	}
	for _, c := range proven {
		t.Run(c.name, func(t *testing.T) {
			words, ok := shellWriteHeredocSimpleCommand(utf16.Encode([]rune(c.segment)))
			rest := shellVerbSkipWrappers(words)
			if !ok || len(rest) == 0 || !shellWriteHeredocInterpreterName(shellVerbName(rest[0])) {
				t.Errorf("%q read as %q, ok=%v; want a proven interpreter command", c.segment, words, ok)
			}
		})
	}
	unproven := []struct{ name, segment string }{
		{"a command substitution", "x=$(bash <<'EOF'"},
		{"a brace group", "{ python3 -; } <<'EOF'"},
		{"a function definition", "f() { bash; } <<'EOF'"},
		{"a clobber redirect", "bash <<'EOF' >|out.txt"},
		{"an fd-numbered here-document", "bash 3<<'EOF'"},
		{"a redirect to a substitution", "bash <<'EOF' >$(mktemp)"},
	}
	for _, c := range unproven {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := shellWriteHeredocSimpleCommand(utf16.Encode([]rune(c.segment))); ok {
				t.Errorf("%q was proven, want not proven", c.segment)
			}
		})
	}
}
