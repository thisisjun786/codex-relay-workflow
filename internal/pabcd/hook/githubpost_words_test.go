package hook

import "testing"

// CRW-917 post-evaluation cases (evaluation 3a4a172b, defects d1, d2, d3). Each one fails on the commit before its fix.

// TestCRW917DuplicateJSONKeysAreAllScanned: a JSON object that repeats a key sends every value of that key in gh's request,
// so the secret scan reads all of them, not only the last one.
func TestCRW917DuplicateJSONKeysAreAllScanned(t *testing.T) {
	cwd := t.TempDir()
	githubPostWrite(t, cwd, "dup-secret.json", "{\"body\":\"MY_API_KEY=sentinel\",\"body\":\"hello\"}\n")
	githubPostWrite(t, cwd, "dup-clean.json", "{\"body\":\"a\",\"body\":\"b\"}\n")
	cases := []struct{ label, command, rule, place string }{
		{"repeated key, secret in the first value", "gh api repos/o/r/issues/1/comments --input dup-secret.json", githubPostRuleSecret, "dup-secret.json:2"},
		{"repeated key, both values clean (control)", "gh api repos/o/r/issues/1/comments --input dup-clean.json", "", ""},
	}
	for _, c := range cases {
		githubPostWant(t, githubPostShell(t, cwd, c.command), c.label, c.rule, c.place)
	}
}

// TestCRW917BodyFilesAreScannedAsText: --body-file and -F body=@ post the file's text as it is, so a JSON-looking file is
// scanned as that text. An escape sequence that spells a secret is not a secret in the posted text, and a raw secret still is.
func TestCRW917BodyFilesAreScannedAsText(t *testing.T) {
	cwd := t.TempDir()
	githubPostWrite(t, cwd, "escaped.md", "{\"body\":\"\\u0073k-aaaaaaaaaaaaaaaa\"}\n")
	githubPostWrite(t, cwd, "raw-secret.json", "{\"body\":\"sk-aaaaaaaaaaaaaaaaaaaa\"}\n")
	cases := []struct{ label, command, rule, place string }{
		{"escape text as a body file (raw text)", "gh pr comment 1 --body-file escaped.md", "", ""},
		{"escape text as a field file (raw text)", "gh api repos/o/r/issues/1/comments -F body=@escaped.md", "", ""},
		{"JSON-looking body file with a raw secret", "gh pr comment 1 --body-file raw-secret.json", githubPostRuleSecret, "raw-secret.json:1"},
	}
	for _, c := range cases {
		githubPostWant(t, githubPostShell(t, cwd, c.command), c.label, c.rule, c.place)
	}
}

// TestCRW917InterpreterProgramIsCanonical: an interpreter's program text is judged as the shell reads it, with quotes and
// backslashes deleted, so a gh name built from empty quote pieces in the program is the post it spells.
func TestCRW917InterpreterProgramIsCanonical(t *testing.T) {
	cwd := t.TempDir()
	cases := []struct{ label, command, rule, place string }{
		{"gh built from quote pieces inside python", "true; python3 -c 'import os; os.system(\"g\\\"\\\"h pr comment 1 --body MY_API_KEY=sentinel\")'", githubPostRuleUnread, githubPostWhereCommand},
		{"python program with no post (control)", "python3 -c 'print(1+2)'", "", ""},
	}
	for _, c := range cases {
		githubPostWant(t, githubPostShell(t, cwd, c.command), c.label, c.rule, c.place)
	}
}
