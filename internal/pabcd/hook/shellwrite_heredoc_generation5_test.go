package hook

import (
	"slices"
	"testing"
	"unicode/utf16"
)

// CRW-765 correction generation 5. Generation 4 proved that the owning command was not an interpreter and kept the body
// as data, but a name bound to an interpreter elsewhere in the command reaches the same program: `eval bash <<'EOF'`,
// `hash -p /bin/bash b` then `b <<'EOF'`, `hash b=/bin/bash` then `b <<'EOF'`, `ln -sf /bin/bash X` then `X <<'EOF'` and
// `alias b=bash` then `b <<'EOF'` all run the body, and a verb that still holds an expansion (`"$B" <<'EOF'`) can be
// any program. The ruling inverts the proof into an allow list:
//
//	R0 the verb of the command the here-document is attached to must be literal after its quotes and backslashes are
//	   removed; a verb that still holds an expansion (a variable, $(), a backtick or a glob) is denied.
//	R1 a here-document is data unconditionally only when its verb is on the short list of programs that never execute
//	   their standard input: cat, tee, head, tail, wc, grep, egrep, fgrep, sort, uniq, cut, tr, jq, base64 and read.
//	R2 any other verb: the here-document is data only when the canonical text of the whole command outside the
//	   here-document bodies names no interpreter, and is denied when that text holds a name-binding construct (eval,
//	   source, ., alias, hash, ln, exec, enable, a function definition or an assignment to PATH) even with no
//	   interpreter named.
//
// The rows below are the ruling's red-first list; on 6271cb2c every denied row was allowed and named nothing.

// shellWriteHeredocGeneration5Rows are the ruling's shapes: a name bound to an interpreter beside the here-document, or
// a verb the reader cannot spell out. Each feeds the body to the interpreter as its program.
func shellWriteHeredocGeneration5Rows(mem string) []struct{ name, command string } {
	sh := "echo x > " + mem + "/a"
	return []struct{ name, command string }{
		{"eval in front of the interpreter", "eval bash <<'EOF'\n" + sh + "\nEOF"},
		{"hash -p binds the name", "hash -p /bin/bash b\nb <<'EOF'\n" + sh + "\nEOF"},
		{"hash binds the name with an assignment", "hash b=/bin/bash\nb <<'EOF'\n" + sh + "\nEOF"},
		{"ln binds a link to the interpreter", "ln -sf /bin/bash /tmp/x\n/tmp/x <<'EOF'\n" + sh + "\nEOF"},
		{"alias binds the name", "alias b=bash\nb <<'EOF'\n" + sh + "\nEOF"},
		{"the verb is a variable", "\"$B\" <<'EOF'\n" + sh + "\nEOF"},
		{"the verb is a braced variable", "${B} <<'EOF'\n" + sh + "\nEOF"},
	}
}

// TestShellWriteHeredocGeneration5Denied is the denied case: each bound name or expanding verb is denied as the shell
// surface, either by naming the protected path (the program is read) or by the fail-closed reason.
func TestShellWriteHeredocGeneration5Denied(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, row := range shellWriteHeredocGeneration5Rows(root) {
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

// TestShellWriteHeredocGeneration5Controls is the invariant case: the allow list must not over-block an ordinary
// command, and every generation-4 control stays allowed.
func TestShellWriteHeredocGeneration5Controls(t *testing.T) {
	const mem = "/h/memories"
	for _, c := range []struct{ name, command string }{
		{"a data program on the allow list", "sort <<'EOF'\n" + mem + "\nEOF"},
		{"jq with its filter operand", "jq . <<'EOF'\n" + mem + "\nEOF"},
		{"an unmodelled verb naming no interpreter", "mytool --input - <<'EOF'\n" + mem + "\nEOF"},
		{"a commit message that mentions an interpreter", "git commit -F - <<'EOF'\nrun python3 -c pass\nEOF"},
		{"a body written to a script file then run", "cat > x.py <<'EOF'\nopen('" + mem + "/a','w')\nEOF\npython3 x.py"},
		{"a note quoting the memories path", "cat > note.md <<'EOF'\n" + mem + "\nEOF"},
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
	// Correction 6 (rule H1): a script operand and a syntax check no longer make the body data, so these rows are read
	// as the interpreter's program and denied (they stood in the allowed list before this correction; the node row is
	// the accepted over-blocking the ruling records).
	for _, c := range []struct{ name, command string }{
		{"a node script operand makes the body data", "node script.js <<'EOF'\nrequire('fs').writeFileSync('" + mem + "/a','x')\nEOF"},
		{"a python script operand", "python3 script.py <<'EOF' 2>/dev/null\nopen('" + mem + "/a','w')\nEOF"},
		{"a shell parsing without running", "bash -n <<'EOF'\necho x > " + mem + "/a\nEOF"},
	} {
		t.Run(c.name+" (read now)", func(t *testing.T) {
			if got := ShellWriteDestinations(c.command); !slices.Contains(got, mem+"/a") {
				t.Errorf("%q named %q, want %q", c.command, got, mem+"/a")
			}
		})
	}
}

// TestShellWriteHeredocGeneration5DataList pins rule R1's list directly: a listed verb is data whatever else the
// command says, and a verb off the list is not data on its name alone.
func TestShellWriteHeredocGeneration5DataList(t *testing.T) {
	for _, verb := range []string{"cat", "tee", "head", "tail", "wc", "grep", "egrep", "fgrep", "sort", "uniq", "cut", "tr", "jq", "base64", "read"} {
		t.Run(verb, func(t *testing.T) {
			if !shellWriteHeredocNeverReadsStdin(verb) {
				t.Errorf("%q is off the data list, want on it", verb)
			}
		})
	}
	for _, verb := range []string{"bash", "python3", "node", "eval", "ln", "mytool", "sudo"} {
		t.Run(verb, func(t *testing.T) {
			if shellWriteHeredocNeverReadsStdin(verb) {
				t.Errorf("%q is on the data list, want off it", verb)
			}
		})
	}
}

// TestShellWriteHeredocGeneration5NameBinding pins rule R2's deny side: a command text that binds a name is refused
// even when it names no interpreter, and a text that does neither is not.
func TestShellWriteHeredocGeneration5NameBinding(t *testing.T) {
	for _, c := range []struct{ name, command string }{
		{"eval", "eval bash <<'EOF'"},
		{"source", "source /tmp/x <<'EOF'"},
		{"the dot builtin", ". /tmp/x <<'EOF'"},
		{"alias", "alias b=bash <<'EOF'"},
		{"hash", "hash -p /bin/bash b <<'EOF'"},
		{"ln", "ln -sf /bin/bash /tmp/x <<'EOF'"},
		{"exec", "exec /tmp/x <<'EOF'"},
		{"enable", "enable -f /tmp/x b <<'EOF'"},
		{"a function definition", "f() { cat; } <<'EOF'"},
		{"an assignment to PATH", "PATH=/tmp/bin:$PATH mytool <<'EOF'"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !shellWriteHeredocNameBinding(utf16.Encode([]rune(c.command))) {
				t.Errorf("%q bound no name, want one", c.command)
			}
		})
	}
	for _, command := range []string{"mytool --input - <<'EOF'", "git commit -F - <<'EOF'", "cat > x.py <<'EOF'", "mypath=x mytool <<'EOF'"} {
		t.Run(command, func(t *testing.T) {
			if shellWriteHeredocNameBinding(utf16.Encode([]rune(command))) {
				t.Errorf("%q bound a name, want none", command)
			}
		})
	}
}

// TestShellWriteHeredocGeneration5VerbExpansion pins rule R0: the verb is refused only when an expansion survives its
// quotes and backslashes, and a literal verb is read normally.
func TestShellWriteHeredocGeneration5VerbExpansion(t *testing.T) {
	for _, command := range []string{"\"$B\" <<'EOF'", "${B} <<'EOF'", "$(x) <<'EOF'", "`x` <<'EOF'", "*.sh <<'EOF'"} {
		t.Run(command, func(t *testing.T) {
			if !shellWriteHeredocVerbExpanded(utf16.Encode([]rune(command))) {
				t.Errorf("%q: the verb expands, want true", command)
			}
		})
	}
	for _, command := range []string{"bash <<'EOF'", "mytool --input - <<'EOF'", "2>&1 python3 - <<'EOF'", "<<'PY' python3", ">out.log mytool <<'EOF'"} {
		t.Run(command, func(t *testing.T) {
			if shellWriteHeredocVerbExpanded(utf16.Encode([]rune(command))) {
				t.Errorf("%q: the verb is literal, want false", command)
			}
		})
	}
}
