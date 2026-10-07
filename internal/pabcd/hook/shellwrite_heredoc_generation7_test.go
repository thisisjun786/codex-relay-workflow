package hook

import (
	"slices"
	"strings"
	"testing"
)

// CRW-765 correction generation 7. Generation 6 held H1, H2's named forms and every earlier rule, but the independent
// review and the parent's re-run found here-documents still read as data although a real shell runs them as a program: a
// sed option bundle (sed -nf - <<'EOF', sed -Ef - x.txt <<'EOF'), ed -s /dev/null <<'EOF', sqlite3 <<'EOF' and
// make -f - <<'EOF'. The deny list is replaced by one closed rule over the verb a here-document is attached to:
//
//	C1 data verbs: R1's list (cat, tee, head, tail, wc, grep, egrep, fgrep, sort, uniq, cut, tr, jq, base64, read and
//	   while/until read loops) and a git commit that reads its message from standard input (-F -, --file=-, --file -)
//	   are data, as before.
//	C2 the modelled interpreters generation 6 already reads as programs (the python, node and sh families) keep being
//	   read as that interpreter's program with the write reading.
//	C3 every other verb is denied as an unreadable here-document program (ed, ex, sqlite3, make, awk, perl and any verb
//	   never seen fall under this one rule).
//	The sed exception: its options are read letter by letter through every bundle (-nf, -Ef, an attached -f-), and a
//	   bundle holding f, or a --file / --file= option, denies; otherwise its here-document is input data.
//
// R0, G1 to G4 and H1 stay. The rows below are the ruling's red-first list; on 8baa7d74 each denied row was allowed.

// shellWriteHeredocGeneration7Rows are the ruling's red shapes: an option bundle that hides sed's -f, interpreters the
// reader cannot read, and a verb it has never seen.
func shellWriteHeredocGeneration7Rows(mem string) []struct{ name, command string } {
	return []struct{ name, command string }{
		{"sed reads its script from standard input through an option bundle", "sed -nf - <<'EOF'\nw " + mem + "/a\nEOF"},
		{"sed reads its script from an attached -f-", "sed -nf- <<'EOF'\nw " + mem + "/a\nEOF"},
		{"sed reads its script from standard input beside another option", "sed -Ef - x.txt <<'EOF'\nw " + mem + "/a\nEOF"},
		{"ed reads its script from standard input", "ed -s /dev/null <<'EOF'\na\nx\n.\nw " + mem + "/a\nEOF"},
		{"sqlite3 reads its script from standard input", "sqlite3 <<'EOF'\n.output " + mem + "/a\nEOF"},
		{"make reads its makefile from standard input", "make -f - <<'EOF'\nall:\n\techo x > " + mem + "/a\nEOF"},
		{"an unseen verb may be an interpreter the reader cannot rule out", "mytool --input - <<'EOF'\necho x > " + mem + "/a\nEOF"},
	}
}

// TestShellWriteHeredocGeneration7Denied is the denied case: each shape is denied as the shell surface, by naming the
// protected path (the program is read) or by the fail-closed reason.
func TestShellWriteHeredocGeneration7Denied(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, row := range shellWriteHeredocGeneration7Rows(root) {
		t.Run(row.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env)
			if !shellWriteHeredocGateDenied(got, root+"/a") {
				t.Errorf("%q: %+v, want a deny naming the protected path or the fail-closed reason", row.command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration7Controls is the invariant case: an ordinary sed script, the data verbs, a harmless
// interpreter program, a commit message and a read loop stay allowed.
func TestShellWriteHeredocGeneration7Controls(t *testing.T) {
	const mem = "/h/memories"
	for _, c := range []struct{ name, command string }{
		{"a sed script operand", "sed 's/a/b/' <<'EOF'\n" + mem + "\nEOF"},
		{"a sed script operand with a quiet option", "sed -n p <<'EOF'\n" + mem + "\nEOF"},
		{"a sed -e script operand", "sed -e 's/a/b/' <<'EOF'\n" + mem + "\nEOF"},
		{"a harmless python program from standard input", "python3 - <<'EOF'\nprint('hi')\nEOF"},
		{"a data here-document for cat", "cat > note.md <<'EOF'\n" + mem + "\nEOF"},
		{"a data here-document for jq", "jq . <<'EOF'\n" + mem + "\nEOF"},
		{"a data here-document for sort", "sort <<'EOF'\n" + mem + "\nEOF"},
		{"a commit message that mentions an interpreter", "git commit -F - <<'EOF'\nrun python3 -c pass\nEOF"},
		{"a commit message from an attached -F-", "git commit -F- <<'EOF'\nrun python3 -c pass\nEOF"},
		{"a read loop with no interpreter outside the body", "while read l; do echo \"$l\"; done <<'EOF'\n" + mem + "\nEOF"},
		{"a body alone naming an interpreter", "cat <<'EOF' > x.md\nrun python3 -c pass\nEOF"},
		{"a body written to a script file then run", "cat > x.py <<'EOF'\nopen('" + mem + "/a','w')\nEOF\npython3 x.py"},
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

// TestShellWriteHeredocGeneration7ClosedRule pins the three sets directly: C1 and C2 are decided, sed is the one
// exception, and every other verb is refused, so no verb outside C1 and C2 can leave a here-document as data.
func TestShellWriteHeredocGeneration7ClosedRule(t *testing.T) {
	const mem = "/h/memories"
	data := []struct{ name, command string }{
		{"cat", "cat <<'EOF'\n" + mem + "\nEOF"},
		{"read", "read x <<'EOF'\n" + mem + "\nEOF"},
		{"git commit -F -", "git commit -F - <<'EOF'\nrun python3 -c pass\nEOF"},
		{"sed without -f", "sed 's/a/b/' <<'EOF'\n" + mem + "\nEOF"},
	}
	for _, c := range data {
		t.Run("data: "+c.name, func(t *testing.T) {
			if got, ok := shellWriteHeredocUnreadable(c.command); ok {
				t.Errorf("%q reported unreadable %q, want data", c.command, got)
			}
		})
	}
	refused := []struct{ name, command string }{
		{"ed", "ed -s /dev/null <<'EOF'\nw " + mem + "/a\nEOF"},
		{"ex", "ex -s <<'EOF'\nw " + mem + "/a\nEOF"},
		{"sqlite3", "sqlite3 <<'EOF'\n.output " + mem + "/a\nEOF"},
		{"make", "make -f - <<'EOF'\nall:\n\techo x\nEOF"},
		{"awk", "awk -f /dev/stdin <<'EOF'\nBEGIN { print \"x\" }\nEOF"},
		{"perl", "perl <<'EOF'\nprint \"x\"\nEOF"},
		{"sed -nf -", "sed -nf - <<'EOF'\nw " + mem + "/a\nEOF"},
		{"sed --file=-", "sed --file=- <<'EOF'\nw " + mem + "/a\nEOF"},
		{"sed --file -", "sed --file - <<'EOF'\nw " + mem + "/a\nEOF"},
		{"an unseen verb", "mytool <<'EOF'\n" + mem + "\nEOF"},
	}
	for _, c := range refused {
		t.Run("refused: "+c.name, func(t *testing.T) {
			if got, ok := shellWriteHeredocUnreadable(c.command); !ok || got != shellWriteHeredocWhatWant {
				t.Errorf("%q: got %q, %v; want %q, true", c.command, got, ok, shellWriteHeredocWhatWant)
			}
		})
	}
}

// TestShellWriteHeredocGeneration7SedOptions pins the sed exception's option reading: f anywhere in a short-option
// bundle, and a --file or --file= option, make sed take its script from the here-document.
func TestShellWriteHeredocGeneration7SedOptions(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"-nf", "-"}, true},
		{[]string{"-nf-"}, true},
		{[]string{"-Ef", "-", "x.txt"}, true},
		{[]string{"-f", "-"}, true},
		{[]string{"-f-"}, true},
		{[]string{"--file", "-"}, true},
		{[]string{"--file=-"}, true},
		{[]string{"--file=/dev/stdin"}, true},
		{[]string{"s/a/b/"}, false},
		{[]string{"-n", "p"}, false},
		{[]string{"-e", "s/a/b/"}, false},
		{[]string{"-E", "-n", "p"}, false},
		{[]string{"-nf", "-e", "p"}, true},
	} {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			if got := shellWriteHeredocSedReadsScript(c.args); got != c.want {
				t.Errorf("%q: got %v, want %v", c.args, got, c.want)
			}
		})
	}
}
