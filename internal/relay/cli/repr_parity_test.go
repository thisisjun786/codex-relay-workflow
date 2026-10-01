package cli_test

import (
	"path/filepath"
	"testing"
)

// A refusal that echoes an argument does so as repr() of the str Python holds for it: a
// character str.isprintable() refuses (U+00A0, U+2028, U+200B) is escaped, and an argv byte that
// is not UTF-8, which Python holds surrogate-escaped, prints as that surrogate. The Go CLI
// answers each argv in a store of its own with the bytes its golden holds (the fence's answers,
// at first).
func TestAnEchoedArgumentIsPythonsReprOfIt(t *testing.T) {
	root := t.TempDir()
	var argvs [][]string
	for _, text := range []string{"x\U000000a0y", "x\U00002028y", "x\U0000200by", "x\xffy", "it's"} {
		argvs = append(argvs, []string{"--json", "intent-resolve", "--workspace", "w", "--assignment", "a", "--chosen-task", "t", "--chosen-session", "s", "--reason", "r", "--adjudicate", text})
		// For "x\xffy" both raise UnicodeEncodeError binding the surrogate into SQLite before they
		// look (a host error, exit 3).
		argvs = append(argvs, []string{"--json", "relationship-status", "--relationship", text, "--status", "paused", "--actor", "a"})
	}
	var goArgvs [][]string
	var answers []answer
	for _, argv := range argvs {
		argv = append([]string{"--state", filepath.Join(root, "go")}, argv...)
		got := goCLI(t, argv...)
		goArgvs, answers = append(goArgvs, argv), append(answers, got)
	}
	expectAnswers(t, batchKey(t, goArgvs...), answers, goArgvs...)
}
