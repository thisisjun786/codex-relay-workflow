package hook

import (
	"path/filepath"
	"slices"
	"testing"
	"unicode/utf16"
)

// CRW-765 correction generation 9. Generation 8 held C1, C2, C3, U1 to U3 and S8 and passed its own independent review,
// but the blind pre-merge evaluation (score 4) found three P0s and two regressions inside this issue. The correction
// reuses the judgements the reader already has instead of adding another shape:
//
//	K1 the declaration collector reads the same text the command reader does, so a word-initial # ends the physical line
//	   and a << inside a comment is no declaration.
//	K2 an unquoted body is read as the shell delivers it - \\, \$, \` and a backslash-newline processed the way the
//	   outer shell processes them - before the interpreter reading; a remaining expansion stays unreadable as today.
//	K3 a shell here-document body goes through the same whole judgement a top-level Bash command gets: the write
//	   destinations, CRW-741's and CRW-754's Python program checks, CRW-726's reading and this issue's here-document
//	   judgement, under today's depth limit.
//	K4 <<< is a here-string token and never a here-document.
//	K5 the hidden here-document check runs on the text with the data bodies removed.
//
// The rows below are the ruling's red-first list; on 5a36ccca each denied row was allowed and each allowed row was denied.

// shellWriteHeredocGeneration9Rows are the evaluation's five reproductions: a declaration hidden in a comment, an
// unquoted body whose backslashes the shell collapses, a Python program the reader cannot finish inside a shell body, a
// here-string misread as a here-document, and a documentation example read as a command.
func shellWriteHeredocGeneration9Rows(mem, dir string) []struct{ name, command string } {
	bs := "\\" // one real backslash; the shell turns a pair into one, so the language then sees \x6d
	return []struct{ name, command string }{
		{"a declaration hidden in a comment", "cat <<'DATA' # <<'HIDDEN'\ntext\nDATA\npython3 <<'PY'\nopen('" + mem + "/a','w')\nPY"},
		{"an unquoted node body the shell unescapes", "node <<EOF\nrequire('fs').writeFileSync('" + dir + "/" + bs + bs + "x6demories/a','x')\nEOF"},
		{"a python -c program inside a shell body", "bash <<'SH'\npython3 -c \"p='" + mem + "/a'; code='open(p, chr(119)).write(chr(120))'; exec(code)\"\nSH"},
	}
}

// TestShellWriteHeredocGeneration9Denied is the denied case: each reproduction is denied as the shell surface, either by
// naming the protected path (the program is read) or by the fail-closed reason.
func TestShellWriteHeredocGeneration9Denied(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, row := range shellWriteHeredocGeneration9Rows(root, filepath.Dir(root)) {
		t.Run(row.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env)
			if !shellWriteHeredocGateDenied(got, root+"/a") {
				t.Errorf("%q: %+v, want a deny naming the protected path or the fail-closed reason", row.command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration9CommentDeclaration pins rule K1: the collector and the command reader agree that a
// word-initial # ends the line, so the interpreter here-document after a commented-out declaration is read as its own
// command instead of being swallowed as the cat data.
func TestShellWriteHeredocGeneration9CommentDeclaration(t *testing.T) {
	const mem = "/h/memories"
	command := "cat <<'DATA' # <<'HIDDEN'\ntext\nDATA\npython3 <<'PY'\nopen('" + mem + "/a','w')\nPY"
	hss := shellWriteHeredocs(utf16.Encode([]rune(command)))
	if len(hss) != 2 {
		t.Fatalf("collected %d here-documents, want 2 (the comment's << is no declaration): %+v", len(hss), hss)
	}
	if got := ShellWriteDestinations(command); !slices.Contains(got, mem+"/a") {
		t.Errorf("named %q, want %q", got, mem+"/a")
	}
	// A # inside a word is not a comment, so its << stays a declaration.
	if n := len(shellWriteHeredocs(utf16.Encode([]rune("cat a#b <<'EOF'\nx\nEOF")))); n != 1 {
		t.Errorf("a # inside a word: collected %d, want 1", n)
	}
}

// TestShellWriteHeredocGeneration9ShellBody pins rule K2 (the shell processes an unquoted body's escapes before the
// language reads it) and rule K3 (a shell body gets the whole judgement a top-level command gets).
func TestShellWriteHeredocGeneration9ShellBody(t *testing.T) {
	const mem = "/h/memories"
	// K2: the shell turns \\ into \, so the language sees \x6d and the path is the memories directory.
	if got := ShellWriteDestinations("node <<EOF\nrequire('fs').writeFileSync('" + mem + "/\\\\x6demories/a','x')\nEOF"); !slices.Contains(got, mem+"/memories/a") {
		t.Errorf("an unescaped node body named %q, want %q", got, mem+"/memories/a")
	}
	// K3: the shell body's python -c program is checked the way a top-level one is, so exec of a non-literal denies.
	body := "bash <<'SH'\npython3 -c \"p='" + mem + "/a'; code='open(p, chr(119)).write(chr(120))'; exec(code)\"\nSH"
	if got, ok := shellWriteHeredocUnreadable(body); !ok {
		t.Errorf("a shell body with an unreadable python -c: got %q, %v; want a reason", got, ok)
	}
	// A shell body that is an ordinary program stays readable.
	for _, c := range []struct{ name, command string }{
		{"a shell body echoing", "bash <<'SH'\necho hi\nSH"},
		{"a shell body redirecting", "bash <<'SH'\necho x > " + mem + "/a\nSH"},
		{"a shell body running a harmless python", "bash <<'SH'\npython3 -c 'print(1)'\nSH"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := shellWriteHeredocUnreadable(c.command); ok {
				t.Errorf("%q reported unreadable %q", c.command, got)
			}
		})
	}
}

// The blind pre-merge evaluation of head a742344da found four more defects inside this issue's own promise, so the
// same generation closes them: a backslash-newline in an unquoted terminator is joined by the shell before delimiter
// matching (the collector compared physical lines only), an escaped quote in plain state opened a bogus quoted span
// that hid both real operators, and rule G4's hidden-operator check ran over a Node program body as if it were outer
// shell text. The rows below are red on a742344da.

// TestShellWriteHeredocGeneration9PreMergeDenied is the denied case: each bypass the evaluation found is denied.
func TestShellWriteHeredocGeneration9PreMergeDenied(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, command string }{
		{"a continued terminator joins before matching", "cat <<EOF\nsafe\nE\\\nOF\npython3 <<'PY'\nopen('" + mem + "/a','w')\nPY"},
		{"an escaped quote opens no quoted span", "echo \\' <<'DATA'\ntext\nDATA\npython3 <<'PY'\nopen('" + mem + "/a','w')\nPY"},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env)
			if !shellWriteHeredocGateDenied(got, mem+"/a") {
				t.Errorf("%q: %+v, want a deny naming the protected path or the fail-closed reason", row.command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration9PreMergeControls is the invariant case: the shell never joins a quoted terminator, so
// the here-document that follows stays inside the data body, and a Node program body is read by Node, so a string that
// looks like a shell substitution inside it is no command.
func TestShellWriteHeredocGeneration9PreMergeControls(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, command string }{
		{"a quoted terminator is not joined, so the body stays data", "cat <<'EOF'\nsafe\nE\\\nOF\npython3 <<'PY'\nopen('" + mem + "/a','w')\nPY"},
		{"a node program body is read by node", "node <<'EOF'\nconsole.log(\"$(cat <<INNER)\");\nEOF"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env); got.Surface != "" {
				t.Errorf("%q must pass: %+v", row.command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration9HereString pins rule K4: <<< is a here-string, so a plain data here-string needs no
// grant and is not collected as a here-document.
func TestShellWriteHeredocGeneration9HereString(t *testing.T) {
	cwd, _, env := gateScene(t)
	for _, c := range []struct{ name, command string }{
		{"a cat here-string", "cat <<<hello"},
		{"a jq here-string", "jq . <<<'{}'"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if n := len(shellWriteHeredocs(utf16.Encode([]rune(c.command)))); n != 0 {
				t.Errorf("%q collected %d here-documents, want 0", c.command, n)
			}
			if got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env); got.Surface != "" {
				t.Errorf("%q must pass: %+v", c.command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration9DocumentationBody pins rule K5: a here-document judged data is no command, so a
// documentation example inside it - a here-document inside a command substitution - is not read as one.
func TestShellWriteHeredocGeneration9DocumentationBody(t *testing.T) {
	const mem = "/h/memories"
	for _, c := range []struct{ name, command string }{
		{"a documentation example inside a quoted body", "cat > note.md <<'DOC'\nx=\"$(cat <<'INNER'\nexample\nINNER\n)\"\nDOC"},
		{"a documentation example naming the memories path", "cat > note.md <<'DOC'\nx=\"$(cat <<'INNER'\n" + mem + "\nINNER\n)\"\nDOC"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := shellWriteHeredocUnreadable(c.command); ok {
				t.Errorf("%q reported unreadable %q, want data", c.command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration9Controls is the invariant case: every earlier control stays allowed, so the correction
// over-blocks nothing an ordinary command needs.
func TestShellWriteHeredocGeneration9Controls(t *testing.T) {
	const mem = "/h/memories"
	for _, c := range []struct{ name, command string }{
		{"a plain here-document with no comment", "cat <<'EOF'\n" + mem + "\nEOF"},
		{"a quoted body with backslashes", "cat <<'EOF'\na\\\\b\nEOF"},
		{"a script operand's data body", "node script.js <<'EOF'\n" + mem + "\nEOF"},
		{"a shell body with backslashes", "bash <<'EOF'\necho 'a\\\\b'\nEOF"},
		{"a data here-string beside a here-document", "cat <<<hello; cat <<'EOF'\n" + mem + "\nEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := shellWriteHeredocUnreadable(c.command); ok {
				t.Errorf("%q reported unreadable %q", c.command, got)
			}
		})
	}
}

// The blind pre-merge evaluation of head 6f0fcf3f1 (score 4) found three more defects inside rule K1's own promise -
// the collector and the command reader must read the same text - so the same generation closes them. The rows below are
// red on 6f0fcf3f1: an escaped blank before a # is no comment boundary, the shell's own quote removal applies inside a
// double-quoted delimiter, and a word-initial # makes the rest of the physical line inert for the header splitter and
// the hidden-operator scan too.

// TestShellWriteHeredocGeneration9EscapedHashDenied is the denied case for d1: `arg\ #text` is one word, so the # is a
// literal character, the here-document operator after it is real, and bash -s runs the body as its program.
func TestShellWriteHeredocGeneration9EscapedHashDenied(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, command string }{
		{"an escaped blank before a hash keeps the heredoc", "bash -s arg\\ #text <<'EOF'\necho x > " + mem + "/a\nEOF"},
		{"the same with a plain bash verb", "bash arg\\ #text <<'EOF'\necho x > " + mem + "/a\nEOF"},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env)
			if !shellWriteHeredocGateDenied(got, mem+"/a") {
				t.Errorf("%q: %+v, want a deny naming the protected path", row.command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration9EscapedHashControls pins the two sides of rule K1's comment boundary: a real comment
// declares no here-document, and an escaped blank before a hash keeps one.
func TestShellWriteHeredocGeneration9EscapedHashControls(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	// A real comment after the header declares no here-document, so the body lines are not a program.
	if n := len(shellWriteHeredocs(utf16.Encode([]rune("bash -s arg #text <<'EOF'")))); n != 0 {
		t.Errorf("a real comment: collected %d here-documents, want 0", n)
	}
	// The escaped blank keeps the here-document, so its body is read and the write is denied.
	if n := len(shellWriteHeredocs(utf16.Encode([]rune("bash -s arg\\ #text <<'EOF'")))); n != 1 {
		t.Errorf("an escaped blank: collected %d here-documents, want 1", n)
	}
	// A commented-out operator is inert: the following line is an ordinary command of the outer script, not a body.
	if got := memoryGateClassify("Bash", map[string]any{"command": "echo start #text <<'EOF'\necho done"}, cwd, env); got.Surface != "" {
		t.Errorf("a commented-out here-document must pass: %+v", got)
	}
	// The escaped blank keeps the operator, so the body is the interpreter's program and the write is read.
	if got := memoryGateClassify("Bash", map[string]any{"command": "bash -s arg\\ #text <<'EOF'\necho x > " + mem + "/a\nEOF"}, cwd, env); !shellWriteHeredocGateDenied(got, mem+"/a") {
		t.Errorf("an escaped blank must deny the write: %+v", got)
	}
}

// TestShellWriteHeredocGeneration9DoubleQuotedDelimiterDenied is the denied case for d2: the shell removes one backslash
// of a pair inside a double-quoted word, so the delimiter is E\OF, the document closes there, and the Python program
// after it is its own command and is read.
func TestShellWriteHeredocGeneration9DoubleQuotedDelimiterDenied(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	command := "cat <<\"E\\\\OF\"\ntext\nE\\OF\npython3 <<'PY'\nopen('" + mem + "/a','w')\nPY"
	// The delimiter the shell spells out is E\OF (one backslash), not the raw E\\OF.
	hss := shellWriteHeredocs(utf16.Encode([]rune(command)))
	if len(hss) != 2 {
		t.Fatalf("collected %d here-documents, want 2: %+v", len(hss), hss)
	}
	if got := string(utf16.Decode(hss[0].delim)); got != `E\OF` {
		t.Errorf("delimiter %q, want %q", got, `E\OF`)
	}
	got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env)
	if !shellWriteHeredocGateDenied(got, mem+"/a") {
		t.Errorf("%q: %+v, want a deny naming the protected path", command, got)
	}
}

// TestShellWriteHeredocGeneration9DoubleQuotedDelimiterControls pins the quote removal that fix needs: the shell removes
// a backslash only before $, `, " and \ inside a double-quoted word, and keeps every other one.
func TestShellWriteHeredocGeneration9DoubleQuotedDelimiterControls(t *testing.T) {
	for _, row := range []struct{ name, header, delim string }{
		{"a doubled backslash removes one", `cat <<"E\\OF"`, `E\OF`},
		{"a backslash before an ordinary letter stays", `cat <<"E\QOF"`, `E\QOF`},
		{"a backslash before a double quote removes it", `cat <<"E\"OF"`, `E"OF`},
		{"a single-quoted word is literal", `cat <<'E\\OF'`, `E\\OF`},
	} {
		t.Run(row.name, func(t *testing.T) {
			hss := shellWriteHeredocs(utf16.Encode([]rune(row.header + "\nx\n" + row.delim)))
			if len(hss) != 1 {
				t.Fatalf("collected %d here-documents, want 1: %+v", len(hss), hss)
			}
			if got := string(utf16.Decode(hss[0].delim)); got != row.delim {
				t.Errorf("delimiter %q, want %q", got, row.delim)
			}
		})
	}
}

// TestShellWriteHeredocGeneration9CommentSyntaxControls is the invariant case for d3: a word-initial # makes the rest of
// the physical line inert, so an apostrophe or a quoted command substitution written in a comment is no syntax and no
// here-document. Each row is an ordinary command an operator may run, and each must pass without a grant.
func TestShellWriteHeredocGeneration9CommentSyntaxControls(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, command string }{
		{"an apostrophe in a comment after a data heredoc", "cat > note.md <<'EOF' # don't parse this\nhello\nEOF"},
		{"a quoted substitution example inside a comment", "echo hi # \"$(cat <<INNER)\""},
		{"an apostrophe in a comment naming the memories path", "cat > note.md <<'EOF' # don't write " + mem + "/a\nhello\nEOF"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got, ok := shellWriteHeredocUnreadable(row.command); ok {
				t.Errorf("%q reported unreadable %q, want data", row.command, got)
			}
			if got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env); got.Surface != "" {
				t.Errorf("%q must pass: %+v", row.command, got)
			}
		})
	}
}
