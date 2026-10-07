package hook

import (
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-765: a here-document attached to an interpreter that reads its program from standard input is read as program
// text before the body is stripped, so an open() or a redirect inside the program names its path and the memory gate
// denies it; an unquoted delimiter whose body holds a shell expansion is a program the gate cannot read and fails
// closed. Every case is a real shell command; on dev each readable row named nothing and passed.
//
// The oracle strips every heredoc body (shell-write-destinations.ts stripHeredocBodies :57-100), so this is a security
// fix under the parity rule revision of 2026-10-03.

// shellWriteHeredocWhatWant is the fail-closed what this issue adds (shellWriteHeredocUnreadableWhat).
const shellWriteHeredocWhatWant = "an interpreter program read from a here-document"

// shellWriteHeredocRows are the readable rows of the issue: each names the protected path it writes.
func shellWriteHeredocRows(mem string) []struct{ name, command string } {
	return []struct{ name, command string }{
		{"python3 dash operand", "python3 - <<'EOF'\nopen('" + mem + "/a','w')\nEOF"},
		{"python3 no script operand", "python3 <<EOF\nopen(file='" + mem + "/a', mode='w')\nEOF"},
		{"python3 triple f-string body", "python3 - <<'EOF'\nf''' '{open(file='" + mem + "/a', mode='w')} '''\nEOF"},
		{"node no script operand", "node <<'EOF'\nrequire('fs').writeFileSync('" + mem + "/a','x')\nEOF"},
		{"bash no -c", "bash <<'EOF'\necho x > " + mem + "/a\nEOF"},
	}
}

// TestShellWriteHeredocReads is the reader case: the issue's readable rows name their path, and the option, quote,
// delimiter and interpreter variants do too. On dev each named nothing.
func TestShellWriteHeredocReads(t *testing.T) {
	const mem = "/h/memories"
	for _, row := range shellWriteHeredocRows(mem) {
		t.Run(row.name, func(t *testing.T) {
			if got := ShellWriteDestinations(row.command); !slices.Contains(got, mem+"/a") {
				t.Errorf("%q named %q, want %q", row.command, got, mem+"/a")
			}
		})
	}
	for _, c := range []struct{ name, command string }{
		{"double-quoted delimiter", "python3 <<\"EOF\"\nopen('/m/a', 'w')\nEOF"},
		// The backslash-escaped delimiter row moved to TestShellWriteHeredocUnreadable: correction 4's rule G1 proves a
		// header only when its physical line holds no backslash, so `python3 <<\\EOF` now fails closed.
		{"dash operand for node", "node - <<'EOF'\nrequire('fs').writeFileSync('/m/a','x')\nEOF"},
		{"sh -s", "sh -s <<'EOF'\necho x > /m/a\nEOF"},
		{"versioned python", "python3.11 <<'EOF'\nopen('/m/a','w')\nEOF"},
		{"strip-tabs heredoc", "python3 - <<-'EOF'\n\topen('/m/a','w')\n\tEOF"},
		{"python Path write_text", "python3 - <<'EOF'\nPath('/m/a').write_text('x')\nEOF"},
		{"node createWriteStream", "node <<'EOF'\nrequire('fs').createWriteStream('/m/a')\nEOF"},
		{"python2 heredoc", "python2 <<'EOF'\nopen('/m/a','w')\nEOF"},
		{"mksh is in the closed rule's shell set", "mksh <<'EOF'\necho x > /m/a\nEOF"},
		{"wrapper before the interpreter", "sudo python3 - <<'EOF'\nopen('/m/a','w')\nEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ShellWriteDestinations(c.command); !slices.Contains(got, "/m/a") {
				t.Errorf("%q named %q, want /m/a", c.command, got)
			}
		})
	}
}

// TestShellWriteHeredocUnchanged is the invariant case: a non-interpreter command's body is data.
//
// Correction 6 (rule H1) changed five rows that stood here: a here-document attached to an interpreter is always read as
// that interpreter's program, whatever its flags and operands, so `python3 script.py <<'EOF'`, `python3 -c ... <<'EOF'`,
// `python3 -m ... <<'EOF'`, `node -e ... <<'EOF'` and `bash -c ... <<'EOF'` now name the protected path instead of
// passing. They are pinned by TestShellWriteHeredocInterpreterAlwaysReads below, and the pull request body lists them.
func TestShellWriteHeredocUnchanged(t *testing.T) {
	const mem = "/h/memories"
	for _, c := range []struct{ name, command string }{
		{"cat quoting the memories path", "cat > note.md <<'EOF'\n" + mem + " is where notes live\nEOF"},
		{"tee body is data", "tee note.md <<'EOF'\n" + mem + "\nEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := ShellWriteDestinations(c.command); slices.Contains(got, mem+"/a") {
				t.Errorf("%q named the protected path: %q", c.command, got)
			}
		})
	}
	// The oracle's own answer for a heredoc body that quotes the memories path is unchanged.
	if got := ShellWriteDestinations("cat > note.md <<'EOF'\n" + mem + "\nEOF"); !slices.Equal(got, []string{"note.md"}) {
		t.Errorf("the cat row named %q, want [note.md]", got)
	}
}

// TestShellWriteHeredocUnreadable is the fail-closed case: an unquoted delimiter whose body holds an expansion reports
// the reason, a quoted body that itself holds an unreadable program reports that program's reason, and a well-formed
// body reports none.
func TestShellWriteHeredocUnreadable(t *testing.T) {
	for _, c := range []struct{ name, command string }{
		{"a dollar expansion", "python3 <<EOF\nopen('$P','w')\nEOF"},
		{"a braced expansion", "python3 <<EOF\nopen('${P}','w')\nEOF"},
		{"a command substitution", "python3 <<EOF\nopen('$(p)','w')\nEOF"},
		{"a backtick expansion", "python3 <<EOF\nopen('`p`','w')\nEOF"},
		{"a shell body with an expansion", "bash <<EOF\necho x > $P\nEOF"},
		{"a node body with an expansion", "node <<EOF\nrequire('fs').writeFileSync('$P','x')\nEOF"},
		{"a backslash-escaped delimiter is an unproven header", "python3 <<\\EOF\nopen('/m/a', 'w')\nEOF"},
		{"a line continuation inside the command word", "bas\\\nh <<'EOF'\necho x > /m/a\nEOF"},
		// Correction 6 (rule H1): an interpreter's here-document is a program whatever its flags and operands, so the
		// body is read and an unquoted one that the outer shell expands is unreadable. These two rows stood in the
		// reports-nothing list before this correction.
		{"an inline -c program still reads the here-document", "python3 -c 'print(1)' <<EOF\n$P\nEOF"},
		{"a script operand still reads the here-document", "python3 script.py <<EOF\n$P\nEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := shellWriteHeredocUnreadable(c.command)
			if !ok || got != shellWriteHeredocWhatWant {
				t.Errorf("%q: got %q, %v; want %q, true", c.command, got, ok, shellWriteHeredocWhatWant)
			}
		})
	}
	// A quoted body holding a program the Python reader cannot finish reports that reader's reason instead.
	if got, ok := shellWriteHeredocUnreadable("python3 - <<'EOF'\nexec(src)\nEOF"); !ok || got != shellWriteExecWhatWant {
		t.Errorf("a quoted body with an unreadable program: got %q, %v; want %q, true", got, ok, shellWriteExecWhatWant)
	}
	// A body with no expansion is read like the quoted case and reports nothing.
	for _, command := range []string{
		"python3 <<EOF\nopen('/m/a','w')\nEOF",
		"cat <<EOF\n/memories\nEOF",
	} {
		if got, ok := shellWriteHeredocUnreadable(command); ok {
			t.Errorf("%q reported unreadable %q", command, got)
		}
	}
}

// TestShellWriteHeredocGate is the gate case: the issue's rows are denied as the shell surface with the path as the
// target, an unquoted body with an expansion is an attempt of its own with the fail-closed reason, and a command that
// names no protected path is no attempt.
func TestShellWriteHeredocGate(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, row := range shellWriteHeredocRows(root) {
		t.Run(row.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env)
			if got.Surface != "shell" || got.Target != root+"/a" {
				t.Errorf("%+v, want the shell surface and %s", got, root+"/a")
			}
		})
	}
	want := "(a program the gate cannot read: " + shellWriteHeredocWhatWant + ")"
	if got := memoryGateClassify("Bash", map[string]any{"command": "python3 <<EOF\nopen('$P','w')\nEOF"}, cwd, env); got.Surface != "shell" || got.Target != want {
		t.Errorf("unreadable: %+v, want the shell surface and %s", got, want)
	}
	for _, command := range []string{
		"cat > note.md <<'EOF'\n" + root + "\nEOF",
	} {
		if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Surface != "" {
			t.Errorf("%q must pass: %+v", command, got)
		}
	}
	// Correction 6 (rule H1): a script operand no longer makes the body data, so this row is denied and names the
	// protected path (it stood in the must-pass list before this correction).
	script := "python3 script.py <<'EOF'\nopen('" + root + "/a','w')\nEOF"
	if got := memoryGateClassify("Bash", map[string]any{"command": script}, cwd, env); !shellWriteHeredocGateDenied(got, root+"/a") {
		t.Errorf("a script operand: %+v, want a deny naming %s", got, root+"/a")
	}
}

// TestShellWriteHeredocGateDeniesAndSpends is the envelope case: denied without a grant, and with one the write passes
// and the grant is consumed, for both a readable body and an unreadable one.
func TestShellWriteHeredocGateDeniesAndSpends(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, c := range []struct{ name, command string }{
		{"readable program", shellWriteHeredocRows(root)[0].command},
		{"unreadable program", "python3 <<EOF\nopen('$P','w')\nEOF"},
	} {
		t.Run(c.name, func(t *testing.T) {
			payload := gateBash(t, cwd, c.command)
			reason := gateDeny(t, HandleMemoryWriteGate(payload, env))
			if !strings.Contains(reason, root+"/a") && !strings.Contains(reason, shellWriteHeredocWhatWant) {
				t.Errorf("the reason names neither the path nor the reason: %s", reason)
			}
			gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
			if out := HandleMemoryWriteGate(payload, env); out != "" {
				t.Errorf("a grant must let it pass: %q", out)
			}
			if state.ReadState(cwd, gateSession).MemoryWriteGrant {
				t.Error("the grant was not consumed")
			}
		})
	}
}

// TestShellWriteHeredocDepthStaysBounded is the bound case: a chain of nested shell heredocs is refused at the depth
// limit instead of walked to the bottom, and the hidden program is a write attempt the gate cannot read, so the fix
// fails closed rather than open on its deepest case.
func TestShellWriteHeredocDepthStaysBounded(t *testing.T) {
	command := "bash <<'INNER'\necho x > /m/a\nINNER"
	for i := 0; i < 12; i++ {
		tag := "OUT" + string(rune('A'+i))
		command = "bash <<'" + tag + "'\n" + command + "\n" + tag
	}
	if got := ShellWriteDestinations(command); slices.Contains(got, "/m/a") {
		t.Errorf("a chain past the depth limit named %q", got)
	}
	if got, ok := shellWriteHeredocUnreadable(command); !ok || got != shellWriteHeredocWhatWant {
		t.Errorf("a chain past the depth limit: got %q, %v; want %q, true", got, ok, shellWriteHeredocWhatWant)
	}
	cwd, _, env := gateScene(t)
	want := "(a program the gate cannot read: " + shellWriteHeredocWhatWant + ")"
	if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Surface != "shell" || got.Target != want {
		t.Errorf("a chain past the depth limit: %+v, want the shell surface and %s", got, want)
	}
}
