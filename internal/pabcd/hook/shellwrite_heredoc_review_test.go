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
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := shellWriteHeredocUnreadable(c.command)
			if !ok || got != shellWriteHeredocWhatWant {
				t.Errorf("%q: got %q, %v; want %q, true", c.command, got, ok, shellWriteHeredocWhatWant)
			}
		})
	}
}

// TestShellWriteHeredocReviewUnchanged pins the findings where the shell runs nothing, so the body is not a program and
// must not be denied.
func TestShellWriteHeredocReviewUnchanged(t *testing.T) {
	const mem = "/h/memories"
	for _, c := range []struct{ name, command string }{
		{"-n parses without running", "bash -n <<'EOF'\necho hi > " + mem + "/a\nEOF"},
		{"-o noexec parses without running", "bash -o noexec <<'EOF'\necho hi > " + mem + "/a\nEOF"},
		{"a bundle carrying n parses without running", "bash -ne <<'EOF'\necho hi > " + mem + "/a\nEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ShellWriteDestinations(c.command); slices.Contains(got, mem+"/a") {
				t.Errorf("%q named the protected path: %q", c.command, got)
			}
		})
	}
	// +o noexec turns execution back on, so the body runs and is read.
	if got := ShellWriteDestinations("bash +o noexec <<'EOF'\necho hi > " + mem + "/a\nEOF"); !slices.Contains(got, mem+"/a") {
		t.Errorf("+o noexec named %q, want the protected path", got)
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

// TestShellWriteHeredocReviewHeaderWords pins the header reader that removes every operator and delimiter word, so a
// backslash-escaped delimiter does not read as a script operand.
func TestShellWriteHeredocReviewHeaderWords(t *testing.T) {
	for _, c := range []struct{ name, header string }{
		{"a plain delimiter", "python3 - <<EOF"},
		{"a backslash-escaped delimiter", "python3 <<\\EOF"},
		{"a quoted delimiter", "python3 <<'EOF'"},
		{"two operators", "python3 <<'A' <<'B'"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := shellWriteHeredocHeaderWords(utf16.Encode([]rune(c.header)))
			if len(got) < 1 || got[0] != "python3" {
				t.Errorf("%q read as %q, want the interpreter first", c.header, got)
			}
			for _, w := range got {
				if w == "EOF" || w == "A" || w == "B" || w == "\\EOF" {
					t.Errorf("%q kept a delimiter word: %q", c.header, got)
				}
			}
		})
	}
}
