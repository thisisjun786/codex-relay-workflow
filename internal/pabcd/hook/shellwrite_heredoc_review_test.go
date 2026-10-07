package hook

import (
	"slices"
	"testing"
	"unicode/utf16"
)

// CRW-765 review regressions: every case is one of the Devin or Codex findings on this pull request, each a real
// shell form that reached the first head with the program unread. The rows pin the fixed behaviour.

// TestShellWriteHeredocReviewReads pins the findings where the here-document still feeds the interpreter but the first
// head did not see it.
func TestShellWriteHeredocReviewReads(t *testing.T) {
	const mem = "/h/memories"
	for _, c := range []struct{ name, command string }{
		{"a redirect precedes the interpreter", "<<'PY' python3\nopen('" + mem + "/a','w')\nPY"},
		{"the last of two heredocs supplies the program", "python3 <<'IGNORED' <<'PY'\nignored\nIGNORED\nopen('" + mem + "/a','w')\nPY"},
		{"an explicitly empty quoted delimiter", "python3 <<''\nopen('" + mem + "/a','w')\n\n"},
		{"node -C takes its value", "node -C development <<'EOF'\nrequire('fs').writeFileSync('" + mem + "/a','x')\nEOF"},
		{"python -i reads stdin after -c", "python3 -i -c pass <<'PY'\nopen('" + mem + "/a','w')\nPY"},
		{"python reads its script from /dev/stdin", "python3 /dev/stdin <<'PY'\nopen('" + mem + "/a','w')\nPY"},
		{"python reads its script from /proc/self/fd/0", "python3 /proc/self/fd/0 <<'PY'\nopen('" + mem + "/a','w')\nPY"},
		{"a <<- body is read with its tabs stripped", "python3 - <<-'EOF'\n\topen('" + mem + "/a','w')\n\tEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ShellWriteDestinations(c.command); !slices.Contains(got, mem+"/a") {
				t.Errorf("%q named %q, want %q", c.command, got, mem+"/a")
			}
		})
	}
}

// TestShellWriteHeredocReviewUnreadable pins the findings where the body is a program the reader cannot finish, so the
// gate must fail closed rather than pass it.
func TestShellWriteHeredocReviewUnreadable(t *testing.T) {
	for _, c := range []struct{ name, command string }{
		{"a nested -c program holds an unquoted heredoc", "bash -c 'python3 <<EOF\nopen(\"$P\",\"w\")\nEOF'"},
		{"eval holds an unquoted heredoc", "eval 'python3 <<EOF\nopen(\"$P\",\"w\")\nEOF'"},
		// Devin's red finding: a backslash-newline joins the lines and the shell still expands the following $, so the
		// body is unreadable. shellWriteHeredocBodyExpands treats the backslash as escaping only the newline, so the $
		// is seen (this row pins the answer that makes the finding a false positive). The command is built from a
		// literal so the source shows the real backslash-newline pair.
		{"a continued line still expands its $", "python3 <<EOF\nopen('x" + "\\\n" + "$P','w')\nEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := shellWriteHeredocUnreadable(c.command)
			if !ok || got != shellWriteHeredocWhatWant {
				t.Errorf("%q: got %q, %v; want %q, true", c.command, got, ok, shellWriteHeredocWhatWant)
			}
		})
	}
}

// TestShellWriteHeredocReviewUnchanged pins the row that stays read. Correction 6 (rule H1) removed the -n/-o noexec
// exception: a here-document an interpreter owns is always that interpreter's program whatever its flags, so the three
// syntax-check rows that stood here now name the protected path (pinned below), and only the +o noexec row, which turns
// execution back on, is left in this list.
func TestShellWriteHeredocReviewUnchanged(t *testing.T) {
	const mem = "/h/memories"
	// +o noexec turns execution back on, so the body runs and is read.
	if got := ShellWriteDestinations("bash +o noexec <<'EOF'\necho hi > " + mem + "/a\nEOF"); !slices.Contains(got, mem+"/a") {
		t.Errorf("+o noexec named %q, want the protected path", got)
	}
	// Correction 6: a syntax check no longer makes the body data, because the interpreter still owns the here-document.
	for _, c := range []struct{ name, command string }{
		{"-n parses without running", "bash -n <<'EOF'\necho hi > " + mem + "/a\nEOF"},
		{"-o noexec parses without running", "bash -o noexec <<'EOF'\necho hi > " + mem + "/a\nEOF"},
		{"a bundle carrying n parses without running", "bash -ne <<'EOF'\necho hi > " + mem + "/a\nEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ShellWriteDestinations(c.command); !slices.Contains(got, mem+"/a") {
				t.Errorf("%q named %q, want %q", c.command, got, mem+"/a")
			}
		})
	}
}

// TestShellWriteHeredocReviewDepth pins the depth limit, which the first head reset at every -c level. A chain of
// nested shell heredocs is read up to the limit and refused past it, both as a destination scan and as a fail-closed
// reason, and the same holds for a chain inside one bash -c (so the counter is not restarted by the -c hop).
func TestShellWriteHeredocReviewDepth(t *testing.T) {
	nested := func(levels int) string {
		body := "echo x > /m/a"
		for i := 0; i < levels; i++ {
			tag := "D" + string(rune('0'+i%10))
			body = "bash <<'" + tag + "'\n" + body + "\n" + tag
		}
		return body
	}
	// At the limit the program is still read; past it the chain is refused.
	for _, c := range []struct {
		name    string
		command string
		read    bool
	}{
		{"eight levels are read", nested(8), true},
		{"nine levels are refused", nested(9), false},
		{"twelve levels are refused", nested(12), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := ShellWriteDestinations(c.command)
			if c.read && !slices.Contains(got, "/m/a") {
				t.Errorf("a chain at the limit named %q, want /m/a", got)
			}
			if !c.read && slices.Contains(got, "/m/a") {
				t.Errorf("a chain past the limit named %q", got)
			}
			w, ok := shellWriteHeredocUnreadable(c.command)
			if c.read && ok {
				t.Errorf("a chain at the limit reported unreadable %q", w)
			}
			if !c.read && (!ok || w != shellWriteHeredocWhatWant) {
				t.Errorf("a chain past the limit: got %q, %v; want %q, true", w, ok, shellWriteHeredocWhatWant)
			}
		})
	}
	// A chain inside one bash -c also counts from the -c level, so the counter does not restart there.
	if got, ok := shellWriteHeredocUnreadable("bash -c \"" + nested(9) + "\""); !ok || got != shellWriteHeredocWhatWant {
		t.Errorf("a chain inside -c: got %q, %v; want %q, true", got, ok, shellWriteHeredocWhatWant)
	}
}

// TestShellWriteHeredocReviewSimpleCommand pins the strict reader the closed rule uses: the owning simple command is
// proven only when every word is literal and every operator is a here-document operator or a redirection from the fixed
// set with a literal target.
func TestShellWriteHeredocReviewSimpleCommand(t *testing.T) {
	for _, c := range []struct{ name, segment string }{
		{"a plain delimiter", "python3 - <<EOF"},
		{"a backslash-escaped delimiter", "python3 <<\\EOF"},
		{"a quoted delimiter", "python3 <<'EOF'"},
		{"two operators", "python3 <<'A' <<'B'"},
		{"a redirect before the verb", "2>&1 python3 - <<EOF"},
		{"a redirect after the operator", "bash <<'EOF' 2>/dev/null"},
	} {
		t.Run(c.name, func(t *testing.T) {
			words, ok := shellWriteHeredocSimpleCommand(utf16.Encode([]rune(c.segment)))
			if !ok || len(words) == 0 || !shellWriteHeredocInterpreterName(words[0]) {
				t.Errorf("%q read as %q, ok=%v; want a proven interpreter command", c.segment, words, ok)
			}
		})
	}
	// A word with an expansion or a compound command is not proven.
	for _, segment := range []string{"x=$(bash <<'EOF'", "{ python3 -; } <<'EOF'", "f() { bash; } <<'EOF'"} {
		if _, ok := shellWriteHeredocSimpleCommand(utf16.Encode([]rune(segment))); ok {
			t.Errorf("%q was proven, want not proven", segment)
		}
	}
}
