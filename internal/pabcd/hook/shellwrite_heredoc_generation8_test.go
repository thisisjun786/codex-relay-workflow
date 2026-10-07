package hook

import (
	"slices"
	"strings"
	"testing"
)

// CRW-765 correction generation 8. Generation 7 held C1, C2 and every earlier row, but the independent review and the
// parent's re-run found here-documents read as data although a real shell runs them: a line whose header the reader
// could not prove went to Unknown, and Unknown denied only when the command text named an interpreter or bound a name,
// so `echo start; perl <<'EOF'`, `true | perl <<'EOF'`, `cd /tmp && mytool <<'EOF'`, `true; ed -s`, `true; sqlite3` and
// `sed -\f - <<'EOF'` passed; and sed options the shell builds (`sed -n$F -`, `sed -n$(printf f) -`) or spells with a
// GNU long-option prefix (`sed --fil -`, `sed --fi=-`) were not read as -f. The correction closes it with four rules:
//
//	U1 split the line the here-document is on at ;, &&, ||, & and | with the same quote-aware tokenizer, and classify
//	   the command that carries the operator by its own words under C1, C2 and C3.
//	U2 a while or until loop that takes the here-document passes only when every command in its condition and body is a
//	   data verb (C1), read, or one of echo, printf, true, false, :, test, [, break, continue, with every word readable.
//	U3 unknown always denies: a line U1 and U2 cannot split or classify is denied, and an interpreter-name test is no
//	   longer an allow condition.
//	S8 sed options are read after the CRW-783 normalisation (quotes and backslashes removed); an expansion left in an
//	   option word denies, a short bundle is read letter by letter, a long option by GNU getopt prefix (a name before =
//	   that is a prefix of file denies), and nothing after -- is an option.
//
// The rows below are the ruling's red-first list; on ab45f96d each denied row was allowed.

// shellWriteHeredocGeneration8Rows are the ruling's shapes: a here-document on a line whose other commands hide it, an
// interpreter whose own program comes from the loop, and a sed option the reader must read the way the shell builds it.
func shellWriteHeredocGeneration8Rows(mem string) []struct{ name, command string } {
	return []struct{ name, command string }{
		{"a command before the here-document on the same line", "echo start; perl <<'EOF'\nopen('" + mem + "/a','w')\nEOF"},
		{"a pipe before the here-document", "true | perl <<'EOF'\nopen('" + mem + "/a','w')\nEOF"},
		{"an unseen verb after a control operator", "cd /tmp && mytool --input - <<'EOF'\necho x > " + mem + "/a\nEOF"},
		{"ed after a control operator", "true; ed -s /dev/null <<'EOF'\na\nx\n.\nw " + mem + "/a\nEOF"},
		{"sqlite3 after a control operator", "true; sqlite3 <<'EOF'\n.output " + mem + "/a\nEOF"},
		{"a sed option the shell escapes", "sed -\\f - <<'EOF'\nw " + mem + "/a\nEOF"},
		{"a sed option built by a variable", "F=f; sed -n$F - <<'EOF'\nw " + mem + "/a\nEOF"},
		{"a sed option built by a substitution", "sed -n$(printf f) - <<'EOF'\nw " + mem + "/a\nEOF"},
		{"a sed long option by its GNU prefix", "sed --fil - <<'EOF'\nw " + mem + "/a\nEOF"},
		{"a sed long option prefix with an attached value", "sed --fi=- <<'EOF'\nw " + mem + "/a\nEOF"},
		{"a for loop that takes the here-document", "for x in a; do perl; done <<'EOF'\nopen('" + mem + "/a','w')\nEOF"},
	}
}

// TestShellWriteHeredocGeneration8Denied is the denied case: each shape is denied as the shell surface, by naming the
// protected path (the program is read) or by the fail-closed reason.
func TestShellWriteHeredocGeneration8Denied(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, row := range shellWriteHeredocGeneration8Rows(root) {
		t.Run(row.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env)
			if !shellWriteHeredocGateDenied(got, root+"/a") {
				t.Errorf("%q: %+v, want a deny naming the protected path or the fail-closed reason", row.command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration8Controls is the invariant case: an ordinary command beside the here-document, the
// data verbs, a harmless interpreter program, a commit message, a read loop, an ordinary sed script and a pipe
// downstream of the here-document's own command stay allowed.
func TestShellWriteHeredocGeneration8Controls(t *testing.T) {
	const mem = "/h/memories"
	for _, c := range []struct{ name, command string }{
		{"a command before a data here-document", "true; cat <<'EOF'\n" + mem + "\nEOF"},
		{"a command before a redirected data here-document", "mkdir -p d && cat > d/f.py <<'EOF'\nopen('" + mem + "/a','w')\nEOF"},
		{"a command before a harmless interpreter program", "cd x; python3 - <<'EOF'\nprint('hi')\nEOF"},
		{"a command before a commit message", "git add -A && git commit -F - <<'EOF'\nrun python3 -c pass\nEOF"},
		{"a read loop", "while read l; do echo \"$l\"; done <<'EOF'\n" + mem + "\nEOF"},
		{"an ordinary sed script", "sed 's/a/b/' <<'EOF'\n" + mem + "\nEOF"},
		{"an ordinary sed script with a quoted option", "sed -n'p' <<'EOF'\n" + mem + "\nEOF"},
		{"a pipe downstream of the here-document's own command", "cat <<'EOF' | jq .\n" + mem + "\nEOF"},
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

// TestShellWriteHeredocGeneration8LineSplit pins rule U1 on the reader itself: the command that carries the operator is
// the one classified, and a line the reader cannot split is denied (rule U3).
func TestShellWriteHeredocGeneration8LineSplit(t *testing.T) {
	const mem = "/h/memories"
	for _, c := range []struct{ name, command string }{
		{"a data verb after a control operator", "true; cat <<'EOF'\n" + mem + "\nEOF"},
		{"a data verb after &&", "cd /tmp && cat <<'EOF'\n" + mem + "\nEOF"},
		{"a data verb after a pipe", "true | cat <<'EOF'\n" + mem + "\nEOF"},
		{"a data verb inside a compound command", "if true; then cat <<'EOF'\n" + mem + "\nEOF\nfi"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := shellWriteHeredocUnreadable(c.command); ok {
				t.Errorf("%q reported unreadable %q, want data", c.command, got)
			}
		})
	}
	for _, c := range []struct{ name, command string }{
		{"an interpreter after a control operator", "true; perl <<'EOF'\n" + mem + "\nEOF"},
		{"an unseen verb after &&", "cd /tmp && mytool <<'EOF'\n" + mem + "\nEOF"},
		{"a for loop", "for x in a; do cat; done <<'EOF'\n" + mem + "\nEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := shellWriteHeredocUnreadable(c.command); !ok || got != shellWriteHeredocWhatWant {
				t.Errorf("%q: got %q, %v; want %q, true", c.command, got, ok, shellWriteHeredocWhatWant)
			}
		})
	}
}

// TestShellWriteHeredocGeneration8Loop pins rule U2: a while or until loop that takes the here-document is data only
// when every command in its condition and body never reads the here-document as a program.
func TestShellWriteHeredocGeneration8Loop(t *testing.T) {
	const mem = "/h/memories"
	for _, c := range []struct{ name, command string }{
		{"a read loop echoing", "while read l; do echo \"$l\"; done <<'EOF'\n" + mem + "\nEOF"},
		{"a read loop writing with cat", "while read l; do cat > out.md; done <<'EOF'\n" + mem + "\nEOF"},
		{"an until loop with true", "until true; do echo x; done <<'EOF'\n" + mem + "\nEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := shellWriteHeredocUnreadable(c.command); ok {
				t.Errorf("%q reported unreadable %q, want data", c.command, got)
			}
		})
	}
	for _, c := range []struct{ name, command string }{
		{"a read loop running an interpreter", "while read l; do bash; done <<'EOF'\n" + mem + "\nEOF"},
		{"a read loop running an unseen verb", "while read l; do mytool; done <<'EOF'\n" + mem + "\nEOF"},
		{"a read loop running a command substitution", "while read l; do echo \"$(bash)\"; done <<'EOF'\n" + mem + "\nEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := shellWriteHeredocUnreadable(c.command); !ok || got != shellWriteHeredocWhatWant {
				t.Errorf("%q: got %q, %v; want %q, true", c.command, got, ok, shellWriteHeredocWhatWant)
			}
		})
	}
}

// TestShellWriteHeredocGeneration8SedOptions pins rule S8: an option word is read after its quotes and backslashes are
// removed, an expansion left in it denies, a short bundle is read letter by letter, a long option by GNU getopt prefix,
// and nothing after -- is an option.
func TestShellWriteHeredocGeneration8SedOptions(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"-nf", "-"}, true},
		{[]string{"-nf-"}, true},
		{[]string{"-Ef", "-", "x.txt"}, true},
		{[]string{"-\\f", "-"}, true},
		{[]string{"-n'f'"}, true},
		{[]string{"--f", "-"}, true},
		{[]string{"--fi", "-"}, true},
		{[]string{"--fil", "-"}, true},
		{[]string{"--file", "-"}, true},
		{[]string{"--file=-"}, true},
		{[]string{"--fi=-"}, true},
		{[]string{"s/a/b/"}, false},
		{[]string{"-n", "p"}, false},
		{[]string{"-np"}, false},
		{[]string{"-e", "s/a/b/"}, false},
		{[]string{"--", "-f", "-"}, false},
		{[]string{"-f", "-", "--", "-f"}, true},
	} {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			if got := shellWriteHeredocSedReadsScript(c.args); got != c.want {
				t.Errorf("%q: got %v, want %v", c.args, got, c.want)
			}
		})
	}
}
