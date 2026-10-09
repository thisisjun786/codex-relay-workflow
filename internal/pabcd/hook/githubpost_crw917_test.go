package hook

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// CRW-917 cases. The --input body of gh api is JSON: a body that does not parse is unreadable, and the secret scan reads every
// string of the document, keys included. The pinned shapes below are judged as the issue asks on the base commit; they stay pinned
// so that a later change to the reader cannot let them through.

func TestCRW917InputIsJSONOrRefused(t *testing.T) {
	cwd := t.TempDir()
	githubPostWrite(t, cwd, "plain.md", "a clean body\n")
	githubPostWrite(t, cwd, "broken.json", "{\"body\": \"hello\"\n")
	githubPostWrite(t, cwd, "trailing.json", "{\"body\": \"hello\"} trailing\n")
	githubPostWrite(t, cwd, "clean.json", "{\"body\": \"hello\"}\n")
	githubPostWrite(t, cwd, "escaped-key.json", "{\"\\u004dY_API_KEY=sentinel\": \"x\"}\n")
	cases := []struct{ label, command, rule, place string }{
		{"plain text input", "gh api repos/o/r/issues/1/comments --input plain.md", githubPostRuleUnread, "plain.md"},
		{"broken JSON input", "gh api repos/o/r/issues/1/comments --input broken.json", githubPostRuleUnread, "broken.json"},
		{"JSON with trailing text", "gh api repos/o/r/issues/1/comments --input trailing.json", githubPostRuleUnread, "trailing.json"},
		{"clean JSON input (control)", "gh api repos/o/r/issues/1/comments --input clean.json", "", ""},
		{"escaped secret key in JSON input", "gh api repos/o/r/issues/1/comments --input escaped-key.json", githubPostRuleSecret, "escaped-key.json:1"},
		{"plain text as a field file (control)", "gh api repos/o/r/issues/1/comments -F body=@plain.md", "", ""},
		{"plain text as a body file (control)", "gh pr comment 1 --body-file plain.md", "", ""},
	}
	for _, c := range cases {
		githubPostWant(t, githubPostShell(t, cwd, c.command), c.label, c.rule, c.place)
	}
}

// TestCRW917ShellWordsAreNormalised pins the issue's P0 shell shapes: quoted options, a glued redirect and a quoted program name
// are read as the words the shell builds, and a word that still expands is refused.
func TestCRW917ShellWordsAreNormalised(t *testing.T) {
	cwd := t.TempDir()
	githubPostWrite(t, cwd, "secret.md", "MY_API_KEY=sentinel\n")
	githubPostWrite(t, cwd, "clean.md", "a clean body\n")
	cases := []struct{ label, command, rule, place string }{
		{"quoted --body and value", "gh pr comment 1 '--body' 'MY_API_KEY=sentinel'", githubPostRuleInline, githubPostWhereCommand},
		{"quoted --body-file names a secret file", "gh pr comment 1 '--body-file' secret.md", githubPostRuleSecret, "secret.md:1"},
		{"redirect glued to the subcommand", "gh pr</dev/null comment 1 --body-file secret.md", githubPostRuleSecret, "secret.md:1"},
		{"quoted program name", "g\"\"h pr comment 1 --body 'x'", githubPostRuleInline, githubPostWhereCommand},
		{"expansion in a body file name", "gh pr comment 1 --body-file \"$F\"", githubPostRuleUnread, githubPostWhereCommand},
		{"git checkout -b is no post", "git checkout -b topic && git status", "", ""},
		{"quoted clean body file (control)", "gh pr comment 1 '--body-file' clean.md", "", ""},
	}
	for _, c := range cases {
		githubPostWant(t, githubPostShell(t, cwd, c.command), c.label, c.rule, c.place)
	}
}

// TestCRW917ArgvWordsAreNotSplitAgain pins the argv form: a word of a command given as an array is one word, not split again.
func TestCRW917ArgvWordsAreNotSplitAgain(t *testing.T) {
	cwd := t.TempDir()
	githubPostWrite(t, cwd, "clean.md", "a clean body\n")
	cases := []struct {
		label       string
		argv        []any
		rule, place string
	}{
		{"argv title with spaces", []any{"gh", "pr", "create", "--title", "Plain title", "--body-file", "clean.md"}, "", ""},
		{"argv secret title", []any{"gh", "pr", "create", "--title", "MY_API_KEY=sentinel", "--body-file", "clean.md"}, githubPostRuleSecret, githubPostWhereTitle},
		{"argv inline body", []any{"gh", "pr", "comment", "1", "--body", "a b"}, githubPostRuleInline, githubPostWhereCommand},
	}
	for _, c := range cases {
		githubPostWant(t, githubPostPayload(t, "Bash", cwd, map[string]any{"command": c.argv}), c.label, c.rule, c.place)
	}
}

// TestCRW917FIFOBodyIsRefusedWithoutBlocking pins the named-pipe body: the guard refuses it and does not wait on the pipe.
func TestCRW917FIFOBodyIsRefusedWithoutBlocking(t *testing.T) {
	cwd := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(cwd, "body.fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { done <- githubPostReason(t, githubPostShell(t, cwd, "gh pr comment 1 --body-file body.fifo")) }()
	select {
	case reason := <-done:
		if !strings.Contains(reason, "(unreadable-github-post)") {
			t.Fatalf("a FIFO body was not refused as unreadable: %q", reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the guard blocked on a FIFO body")
	}
}
