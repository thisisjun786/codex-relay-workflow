package hook

import (
	"testing"
)

// The pre-merge evaluation of head 24f3a6f1b (score 4) found five defects inside the same promise. The rows below are red
// on 24f3a6f1b. Each denied row is one the shell runs, and each control is an ordinary command that must stay allowed.

// TestShellWriteHeredocGeneration9EighthDenied covers the four denied findings: an env wrapper whose argument builds the
// command line, a quoted verb that binds a name, an ANSI-C octal escape the shell truncates to a byte, and a here-document
// hidden in a split double-quoted substitution.
func TestShellWriteHeredocGeneration9EighthDenied(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, command string }{
		{"env with an empty split string runs the trailing program", "env --split-string='' python3 -c \"open('" + mem + "/a','w')\""},
		{"a quoted hash binds cat to python", "'hash' -p /usr/bin/python3 cat\ncat <<'PY'\nopen('" + mem + "/a','w')\nPY"},
		{"an octal escape the shell truncates to a byte", "cat <<'D'$'\\501'\ntext\nDA\npython3 - <<'PY'\nopen('" + mem + "/a','w')\nPY"},
		{"a here-document in a split double-quoted substitution", "result=\"$(printf '%s' \"ok\"; python3 - <<'PY'\nopen('" + mem + "/a','w')\nPY\n)\""},
	} {
		t.Run(row.name, func(t *testing.T) {
			got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env)
			if got.Surface == "" {
				t.Errorf("%q must be denied: %+v", row.command, got)
			}
		})
	}
}

// TestShellWriteHeredocGeneration9EighthControls pins the controls the fixes must keep: a data sed script with a bracket
// expression, an ordinary env option, and a quoted operand that binds nothing.
func TestShellWriteHeredocGeneration9EighthControls(t *testing.T) {
	cwd, root, env := gateScene(t)
	mem := root
	for _, row := range []struct{ name, command string }{
		{"a sed script with a bracket expression", "sed -e's/[ab]/c/' <<'EOF'\na\nEOF"},
		{"a sed script with a bracket beside an option", "sed -n -e's/[ab]/c/p' <<'EOF'\na\nEOF"},
		{"an ordinary env option", "env -u FOO cat <<'EOF'\n" + mem + "\nEOF"},
		{"a quoted hash operand", "echo 'hash -p /bin/bash b'; cat <<'EOF'\nhello\nEOF"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := memoryGateClassify("Bash", map[string]any{"command": row.command}, cwd, env); got.Surface != "" {
				t.Errorf("%q must pass: %+v", row.command, got)
			}
		})
	}
}
