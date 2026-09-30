package cli_test

import (
	"path/filepath"
	"testing"
)

// A refusal that echoes an argument does so as repr() of the str Python holds for it: a
// character str.isprintable() refuses (U+00A0, U+2028, U+200B) is escaped, and an argv byte that
// is not UTF-8, which Python holds surrogate-escaped, prints as that surrogate. Both runtimes
// answer each argv in a store of their own; the printed answers must be the same bytes.
func TestAnEchoedArgumentIsPythonsReprOfIt(t *testing.T) {
	root := t.TempDir()
	var argvs [][]string
	for _, text := range []string{"x\U000000a0y", "x\U00002028y", "x\U0000200by", "x\xffy", "it's"} {
		argvs = append(argvs, []string{"--json", "intent-resolve", "--workspace", "w", "--assignment", "a", "--chosen-task", "t", "--chosen-session", "s", "--reason", "r", "--adjudicate", text})
		if text != "x\xffy" { // Python raises binding a surrogate into SQLite before it looks
			argvs = append(argvs, []string{"--json", "relationship-status", "--relationship", text, "--status", "paused", "--actor", "a"})
		}
	}
	var pythonArgvs [][]string
	for _, argv := range argvs {
		pythonArgvs = append(pythonArgvs, append([]string{"--state", filepath.Join(root, "python")}, argv...))
	}
	want := pythonCLI(t, pythonArgvs...)
	for i, argv := range argvs {
		got := goCLI(t, append([]string{"--state", filepath.Join(root, "go")}, argv...)...)
		if got != want[i] {
			t.Errorf("%q:\ngo     %d %s\npython %d %s", argv, got.code, got.stdout, want[i].code, want[i].stdout)
		}
	}
}
