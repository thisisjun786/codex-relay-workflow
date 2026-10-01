package cli_test

import (
	"path/filepath"
	"testing"
)

// A refusal that echoes an argument quotes it: --adjudicate's with Go's %q (a character Go does
// not print, U+00A0, U+2028, U+200B, is escaped, and so is an argv byte that is not UTF-8), and
// relationship-status's as the registry words it. Each argv answers in a store of its own with
// the bytes its golden holds.
func TestAnEchoedArgumentIsQuotedInTheRefusal(t *testing.T) {
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
