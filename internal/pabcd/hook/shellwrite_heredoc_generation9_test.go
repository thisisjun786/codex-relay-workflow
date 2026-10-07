package hook

import (
	"path/filepath"
	"slices"
	"strings"
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

// The blind pre-merge evaluation of head 05dc1a743 (score 4) found three more defects inside the same promise, so the
// same generation closes them: the hidden-operator scan ended at the first comment instead of the end of that line, an
// ANSI-C quoted delimiter was read with its $ left in place so the document never closed, and the header proof and the
// function-name scan still read a comment as syntax. The rows below are red on 05dc1a743.

// TestShellWriteHeredocGeneration9CommentLineDenied is the denied case for the scan that ended too early: a comment
// ends its own physical line only, so a here-document inside a double-quoted command substitution on a later line is
// still one the collector cannot reach.
func TestShellWriteHeredocGeneration9CommentLineDenied(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, command string }{
		{"a comment on the line before a hidden here-document", "echo start # harmless comment\nresult=\"$(python3 <<'PY'\nopen('" + mem + "/a','w')\nPY\n)\""},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env)
			if !shellWriteHeredocGateDenied(got, mem+"/a") {
				t.Errorf("%q: %+v, want a deny naming the protected path or the fail-closed reason", row.command, got)
			}
		})
	}
	// A comment swallows the rest of its own line, so a substitution written after it is documentation the shell never
	// runs: the scan continues on the next line without reading past the newline it stopped at.
	for _, c := range []struct{ name, command string }{
		{"a substitution written after a comment", "echo start # note; result=\"$(python3 <<'PY'\nopen('" + mem + "/a','w')\nPY\n)\""},
		{"a comment on the header line before a data here-document", "echo start # note\ncat <<'EOF'\n" + mem + "\nEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env); got.Surface != "" {
				t.Errorf("%q must pass: %+v", c.command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration9AnsiCDelimiterDenied is the denied case for an ANSI-C quoted delimiter: the shell
// reads $'EOF' as the word EOF, so the document closes at the first EOF line and the Python program that follows is its
// own command.
func TestShellWriteHeredocGeneration9AnsiCDelimiterDenied(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	command := "cat <<$'EOF'\ntext\nEOF\npython3 <<'PY'\nopen('" + mem + "/a','w')\nPY"
	hss := shellWriteHeredocs(utf16.Encode([]rune(command)))
	if len(hss) != 2 {
		t.Fatalf("collected %d here-documents, want 2: %+v", len(hss), hss)
	}
	if got := string(utf16.Decode(hss[0].delim)); got != "EOF" {
		t.Errorf("delimiter %q, want %q", got, "EOF")
	}
	got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env)
	if !shellWriteHeredocGateDenied(got, mem+"/a") {
		t.Errorf("%q: %+v, want a deny naming the protected path", command, got)
	}
}

// TestShellWriteHeredocGeneration9AnsiCDelimiterControls pins the ANSI-C escapes the delimiter reader decodes: the
// shell processes them before the word is used, and an escape it does not decode keeps its backslash.
func TestShellWriteHeredocGeneration9AnsiCDelimiterControls(t *testing.T) {
	for _, row := range []struct{ name, header, delim string }{
		{"a plain ANSI-C word", `cat <<$'EOF'`, "EOF"},
		{"an ANSI-C tab", `cat <<$'E	F'`, "E\tF"},
		{"an ANSI-C backslash", `cat <<$'E\\F'`, `E\F`},
		{"an ANSI-C hex escape", `cat <<$'\x45OF'`, "EOF"},
		{"a double-quoted word after a dollar", `cat <<$"EOF"`, "EOF"},
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

// TestShellWriteHeredocGeneration9HeaderCommentControls is the invariant case for the header proof: a word-initial #
// makes the rest of the physical line inert, so a backslash or a function example written in a comment is neither a
// continuation nor a function definition, and an ordinary documentation command stays allowed.
func TestShellWriteHeredocGeneration9HeaderCommentControls(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, command string }{
		{"a function example in a comment", "cat > note.md <<'EOF' # function example\nhello\nEOF"},
		{"a name() example in a comment", "cat > note.md <<'EOF' # example() { true; }\nhello\nEOF"},
		{"a backslash in a comment", "cat > note.md <<'EOF' # a\\b\nhello\nEOF"},
		{"a memories path in a comment", "cat > note.md <<'EOF' # do not write " + mem + "/a\nhello\nEOF"},
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

// The blind pre-merge evaluation of head a9ca76947 (score 4) found three more bypasses inside the same promise and one
// regression this change had introduced, so the same generation closes them: the ANSI-C delimiter reader did not decode
// every escape the shell decodes (and kept the backslash of one it does not), the comment boundary used the oracle's
// JavaScript blank set instead of the shell's ASCII blanks, the name-binding rule was not applied to the command text
// that carries a data here-document, and sed's short-bundle reader took the argument of -e for an option bundle. The
// rows below are red on a9ca76947.

// TestShellWriteHeredocGeneration9AnsiCEscapeDenied is the denied case for the escape forms: a delimiter the shell
// builds with a Unicode or control escape, or one holding an escape the shell does not decode, is the word the shell
// spells out, so the terminator closes there and the Python program after it is read.
func TestShellWriteHeredocGeneration9AnsiCEscapeDenied(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, delim, term string }{
		{"a Unicode escape", `$'\u0045OF'`, "EOF"},
		{"an eight-digit Unicode escape", `$'\U00000045OF'`, "EOF"},
		{"an unknown escape keeps its backslash", `$'E\qOF'`, `E\qOF`},
		{"a hex escape with no digit keeps the backslash", `$'\xOF'`, `\xOF`},
		{"a control escape", `$'\cA'`, "\x01"},
	} {
		t.Run(row.name, func(t *testing.T) {
			command := "cat <<" + row.delim + "\ntext\n" + row.term + "\npython3 <<'PY'\nopen('" + mem + "/a','w')\nPY"
			hss := shellWriteHeredocs(utf16.Encode([]rune(command)))
			if len(hss) != 2 {
				t.Fatalf("collected %d here-documents, want 2: %+v", len(hss), hss)
			}
			got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env)
			if !shellWriteHeredocGateDenied(got, mem+"/a") {
				t.Errorf("%q: %+v, want a deny naming the protected path", command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration9AnsiCEscapeControls pins the escapes the reader must still decode the shell's way.
func TestShellWriteHeredocGeneration9AnsiCEscapeControls(t *testing.T) {
	for _, row := range []struct{ name, header, delim string }{
		{"a plain ANSI-C word", `cat <<$'EOF'`, "EOF"},
		{"a two-digit hex escape", `cat <<$'\x45OF'`, "EOF"},
		{"a three-digit octal escape", `cat <<$'\101'`, "A"},
		{"a doubled backslash", `cat <<$'E\\F'`, `E\F`},
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

// TestShellWriteHeredocGeneration9NonASCIISpaceDenied is the denied case for the comment boundary: the shell splits a
// word at an ASCII blank only, so a non-breaking space before a # keeps the # inside the argument and the command after
// the semicolon still runs.
func TestShellWriteHeredocGeneration9NonASCIISpaceDenied(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, blank := range []struct{ name, c string }{
		{"a non-breaking space", "\u00a0"},
		{"an em space", "\u2003"},
		{"a vertical tab", "\v"},
		{"a form feed", "\f"},
	} {
		t.Run(blank.name, func(t *testing.T) {
			command := "echo x" + blank.c + "#; python3 - <<'PY'\nopen('" + mem + "/a','w')\nPY"
			got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env)
			if !shellWriteHeredocGateDenied(got, mem+"/a") {
				t.Errorf("%q: %+v, want a deny naming the protected path", command, got)
			}
		})
	}
	// An ASCII blank before the # still begins a comment, so nothing after it runs.
	for _, command := range []string{"echo x #; python3 - <<'PY'\nopen('" + mem + "/a','w')\nPY", "echo x\t#; python3 - <<'PY'\nopen('" + mem + "/a','w')\nPY"} {
		t.Run("an ASCII blank begins a comment: "+command[:8], func(t *testing.T) {
			if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Surface != "" {
				t.Errorf("%q must pass: %+v", command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration9NameBindingInCommandDenied is the denied case for a name the same command text binds
// to a program: a function definition or a hash entry that makes a data verb run the body.
func TestShellWriteHeredocGeneration9NameBindingInCommandDenied(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, command string }{
		{"a function definition reusing a data verb", "cat() { python3 -; }\ncat <<'PY'\nopen('" + mem + "/a','w')\nPY"},
		{"a hash entry reusing a data verb", "hash -p /usr/bin/python3 cat\ncat <<'PY'\nopen('" + mem + "/a','w')\nPY"},
		{"a function definition on the header line", "f() { python3 -; }; cat <<'PY'\nopen('" + mem + "/a','w')\nPY"},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env)
			if !shellWriteHeredocGateDenied(got, mem+"/a") {
				t.Errorf("%q: %+v, want a deny naming the protected path or the fail-closed reason", row.command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration9SedOptionArgumentControls is the invariant case for the sed option reader: the
// argument of -e, -i and -l is the script, not an option bundle, so its letters decide nothing, while a real -f bundle
// still refuses the here-document. Each row is an ordinary filter and must pass without a grant.
func TestShellWriteHeredocGeneration9SedOptionArgumentControls(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, command string }{
		{"an attached -e script holding an f", "sed -e's/foo/bar/' <<'EOF'\nfoo\nEOF"},
		{"an attached -e script holding f and l", "sed -es/foo/bar/l <<'EOF'\nfoo\nEOF"},
		{"an attached -i suffix", "sed -i.bak -es/foo/bar/ <<'EOF'\nfoo\nEOF"},
		{"a separated -e script", "sed -e 's/foo/bar/' <<'EOF'\nfoo\nEOF"},
		{"the -n and -p options", "sed -n p <<'EOF'\nfoo\nEOF"},
		{"a data here-document for jq", "jq . <<'EOF'\n" + mem + "\nEOF"},
		{"a pipe downstream to jq", "cat <<'EOF' | jq .\n" + mem + "\nEOF"},
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
	// A real -f bundle keeps refusing the here-document, however it is spelled.
	for _, args := range [][]string{{"-nf", "-"}, {"-nf-"}, {"-Ef", "-", "x.txt"}, {"-f", "-"}, {"--fil", "-"}} {
		t.Run("a -f bundle refuses: "+strings.Join(args, " "), func(t *testing.T) {
			if !shellWriteHeredocSedReadsScript(args) {
				t.Errorf("%q: got false, want true", args)
			}
		})
	}
}

// The blind pre-merge evaluation of head 4c8b1b9ea (score 4) found three more bypasses and one regression in the same
// promise: the name-binding scan read the text the oracle's legacy stripper left behind, a delimiter holding a
// non-ASCII blank was split at it, a Python program with a source-encoding declaration was read as its undecoded
// spelling, and a quoted delimiter written in two spans made an ordinary documentation command look like a binding.
// The rows below are red on 4c8b1b9ea.

// TestShellWriteHeredocGeneration9NonASCIISpaceDelimiterDenied is the denied case for a delimiter that holds a
// non-ASCII blank: the shell splits the word at an ASCII blank only, so the delimiter is the whole word and the
// document ends where the shell ends it.
func TestShellWriteHeredocGeneration9NonASCIISpaceDelimiterDenied(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, blank := range []struct{ name, c string }{
		{"a non-breaking space", "\u00a0"},
		{"an em space", "\u2003"},
	} {
		t.Run(blank.name, func(t *testing.T) {
			command := "cat <<EOF" + blank.c + "X\nsafe\nEOF" + blank.c + "X\npython3 - <<'PY'\nopen('" + mem + "/a','w')\nPY"
			got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env)
			if !shellWriteHeredocGateDenied(got, mem+"/a") {
				t.Errorf("%q: %+v, want a deny naming the protected path", command, got)
			}
		})
	}
	// An ASCII blank still separates the delimiter word from what follows it.
	if hss := shellWriteHeredocs(utf16.Encode([]rune("cat <<EOF \nx\nEOF"))); len(hss) != 1 || string(utf16.Decode(hss[0].delim)) != "EOF" {
		t.Errorf("an ASCII blank: %+v, want one here-document delimited by EOF", hss)
	}
}

// TestShellWriteHeredocGeneration9PythonEncodingDenied is the denied case for a Python program read from standard input
// that declares a source encoding: Python decodes those bytes under a codec this reader does not model, so the program
// it runs is not the text read here and the here-document is refused.
func TestShellWriteHeredocGeneration9PythonEncodingDenied(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, body string }{
		{"a UTF-7 cookie", "# coding: utf-7\n+AG8-pen('" + mem + "/a','w')\n"},
		{"a latin-1 cookie", "# -*- coding: latin-1 -*-\nopen('" + mem + "/a','w')\n"},
	} {
		t.Run(row.name, func(t *testing.T) {
			command := "python3 - <<'PY'\n" + row.body + "PY"
			got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env)
			if got.Surface == "" {
				t.Errorf("%q must be refused: %+v", command, got)
			}
		})
	}
	// A harmless program with no declaration stays readable.
	if got, ok := shellWriteHeredocUnreadable("python3 - <<'PY'\nprint(1)\nPY"); ok {
		t.Errorf("a harmless program reported unreadable %q", got)
	}
}

// TestShellWriteHeredocGeneration9SplitDelimiterControls is the invariant case for a quoted delimiter written in two
// spans: the shell concatenates them into one word, so the body ends there and the lines inside it are text, not
// commands.
func TestShellWriteHeredocGeneration9SplitDelimiterControls(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, command string }{
		{"a documentation body naming a binding verb", "cat > note.md <<'E''OF'\nE\nalias tool=python3\nEOF"},
		{"a documentation body naming a function", "cat > note.md <<'E''OF'\nE\nf() { python3 -; }\nEOF"},
		{"a documentation body naming the memories path", "cat > note.md <<'E''OF'\nE\n" + mem + "/a\nEOF"},
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
	// A binding written before the here-document still denies, whatever the delimiter's spelling.
	for _, command := range []string{
		"cat() { python3 -; }\ncat <<'E''OF'\nsafe\nEOF",
		"cat <<'DATA'\nsafe\nDATA\ncat() { python3 -; }\ncat <<'PY'\nopen('" + mem + "/a','w')\nPY",
	} {
		t.Run("a binding denies: "+command[:10], func(t *testing.T) {
			if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Surface == "" {
				t.Errorf("%q must be denied: %+v", command, got)
			}
		})
	}
}

// The blind pre-merge evaluation of head 9709977c5 (score 4) found two more bypasses inside the same promise and two
// regressions this change had introduced: env's split-string option built a command line this reader did not read, an
// ANSI-C delimiter holding a NUL was read past the shell's truncation, the nested unreadable scan still read the
// legacy stripper's text, and function detection read a quoted argument as a definition. The rows below are red on
// 9709977c5.

// TestShellWriteHeredocGeneration9EnvSplitStringDenied is the denied case for env's --split-string: env builds a
// command line out of the argument, so the command this reader would call the verb is not the one env runs.
func TestShellWriteHeredocGeneration9EnvSplitStringDenied(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, command string }{
		{"env -S with a long option", "env -S 'python3 -' cat <<'PY'\nopen('" + mem + "/a','w')\nPY"},
		{"env --split-string", "env --split-string 'python3 -' cat <<'PY'\nopen('" + mem + "/a','w')\nPY"},
		{"env --split-string=", "env --split-string='python3 -' cat <<'PY'\nopen('" + mem + "/a','w')\nPY"},
		{"env -S with a bundled short option", "env -iS 'python3 -' cat <<'PY'\nopen('" + mem + "/a','w')\nPY"},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env)
			if !shellWriteHeredocGateDenied(got, mem+"/a") {
				t.Errorf("%q: %+v, want a deny naming the protected path or the fail-closed reason", row.command, got)
			}
		})
	}
	// An ordinary env wrapper with no split-string stays readable.
	for _, command := range []string{"env cat <<'EOF'\n" + mem + "\nEOF", "env -i cat <<'EOF'\n" + mem + "\nEOF"} {
		t.Run("an ordinary env stays allowed: "+command[:7], func(t *testing.T) {
			if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Surface != "" {
				t.Errorf("%q must pass: %+v", command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration9AnsiCNulDenied is the denied case for an ANSI-C delimiter holding a NUL: the shell
// truncates the word there, so the delimiter is the part before it and the document closes where the shell closes it.
func TestShellWriteHeredocGeneration9AnsiCNulDenied(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, delim, term string }{
		{"a trailing NUL", `$'DATA\0ignored'`, "DATA"},
		{"a NUL inside the word", `$'DA\0TA'`, "DA"},
		{"a NUL before the end", `$'DATA\0'`, "DATA"},
	} {
		t.Run(row.name, func(t *testing.T) {
			command := "cat <<" + row.delim + "\ntext\n" + row.term + "\npython3 - <<'PY'\nopen('" + mem + "/a','w')\nPY"
			hss := shellWriteHeredocs(utf16.Encode([]rune(command)))
			if len(hss) != 2 {
				t.Fatalf("collected %d here-documents, want 2: %+v", len(hss), hss)
			}
			got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env)
			if !shellWriteHeredocGateDenied(got, mem+"/a") {
				t.Errorf("%q: %+v, want a deny naming the protected path", command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration9QuotedFunctionControls is the invariant case for function detection and the nested
// scan: a quoted argument naming a function or a nested program is text the shell passes through, and a documentation
// body holding a concatenated delimiter keeps its own lines inside the body.
func TestShellWriteHeredocGeneration9QuotedFunctionControls(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, command string }{
		{"a quoted function example on the header", "cat > note.md <<'DOC'; printf '%s' 'f()'\nhello\nDOC"},
		{"a quoted function example after a data here-document", "cat <<'EOF'\n" + mem + "\nEOF\nprintf '%s' 'f()'"},
		{"a documentation body with a concatenated delimiter", "cat > note.md <<'E''OF'\nE\nbash -c 'mytool <<INNER\nexample\nINNER'\nEOF"},
		{"a documentation body with a concatenated delimiter naming a path", "cat > note.md <<'E''OF'\nE\n" + mem + "/a\nEOF"},
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
	// A real function definition still makes the header unprovable.
	for _, command := range []string{"f() { true; }; cat <<'EOF'\n" + mem + "\nEOF", "function f { true; }; cat <<'EOF'\n" + mem + "\nEOF"} {
		t.Run("a real definition denies: "+command[:6], func(t *testing.T) {
			if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Surface == "" {
				t.Errorf("%q must be denied: %+v", command, got)
			}
		})
	}
}

// The blind pre-merge evaluation of head cf4c472b9 (score 4) found two more bypasses inside the same promise and one
// regression this change had introduced: a NUL truncated the whole delimiter word instead of one ANSI-C fragment, env's
// long-option abbreviation of split-string was not recognized, and the name-binding scan read a quoted operand as a
// PATH assignment or a command separator. The rows below are red on cf4c472b9.

// TestShellWriteHeredocGeneration9AnsiCNulContinuesDenied is the denied case for a delimiter whose ANSI-C fragment holds
// a NUL and is followed by more word: the shell truncates only that fragment's contribution, so the word continues.
func TestShellWriteHeredocGeneration9AnsiCNulContinuesDenied(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, delim, term string }{
		{"a literal fragment after the NUL", `$'DATA\0ignored'X`, "DATAX"},
		{"a fragment before and after", `$'DA\0TA'X`, "DAX"},
		{"a literal fragment before the ANSI-C word", `X$'DA\0TA'`, "XDA"},
	} {
		t.Run(row.name, func(t *testing.T) {
			command := "cat <<" + row.delim + "\ntext\n" + row.term + "\npython3 - <<'PY'\nopen('" + mem + "/a','w')\nPY"
			hss := shellWriteHeredocs(utf16.Encode([]rune(command)))
			if len(hss) != 2 {
				t.Fatalf("collected %d here-documents, want 2: %+v", len(hss), hss)
			}
			if got := string(utf16.Decode(hss[0].delim)); got != row.term {
				t.Errorf("delimiter %q, want %q", got, row.term)
			}
			got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env)
			if !shellWriteHeredocGateDenied(got, mem+"/a") {
				t.Errorf("%q: %+v, want a deny naming the protected path", command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration9EnvSplitStringAbbrevDenied is the denied case for a GNU getopt abbreviation of env's
// split-string option: any unambiguous prefix names it.
func TestShellWriteHeredocGeneration9EnvSplitStringAbbrevDenied(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, command string }{
		{"an abbreviation with a value", "env --split-str 'python3 -' cat <<'PY'\nopen('" + mem + "/a','w')\nPY"},
		{"a short abbreviation", "env --sp 'python3 -' cat <<'PY'\nopen('" + mem + "/a','w')\nPY"},
		{"an abbreviation with an attached value", "env --split-str='python3 -' cat <<'PY'\nopen('" + mem + "/a','w')\nPY"},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env)
			if !shellWriteHeredocGateDenied(got, mem+"/a") {
				t.Errorf("%q: %+v, want a deny naming the protected path or the fail-closed reason", row.command, got)
			}
		})
	}
	// An env option that is not a prefix of split-string is an ordinary option.
	for _, command := range []string{"env -u FOO cat <<'EOF'\n" + mem + "\nEOF", "env --unset=FOO cat <<'EOF'\n" + mem + "\nEOF"} {
		t.Run("an ordinary env option stays allowed: "+command[:7], func(t *testing.T) {
			if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Surface != "" {
				t.Errorf("%q must pass: %+v", command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration9QuotedOperandControls is the invariant case for the name-binding scan: a quoted
// operand is text the shell passes through, so a PATH= inside it is no assignment and a ; inside it splits nothing.
func TestShellWriteHeredocGeneration9QuotedOperandControls(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, command string }{
		{"a quoted PATH operand beside a data here-document", "echo 'PATH=/usr/bin'; cat > note.md <<'EOF'\nhello\nEOF"},
		{"a quoted separator and eval beside a data here-document", "echo 'example; eval placeholder'; cat <<'EOF'\nhello\nEOF"},
		{"a quoted binding verb beside a data here-document", "echo 'hash -p /bin/bash b'; cat <<'EOF'\nhello\nEOF"},
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
	// An unquoted binding still denies.
	for _, command := range []string{"PATH=/tmp/bin cat <<'EOF'\n" + mem + "\nEOF", "eval cat <<'EOF'\n" + mem + "\nEOF"} {
		t.Run("an unquoted binding denies: "+command[:6], func(t *testing.T) {
			if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Surface == "" {
				t.Errorf("%q must be denied: %+v", command, got)
			}
		})
	}
}
