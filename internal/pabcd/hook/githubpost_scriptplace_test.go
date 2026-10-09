package hook

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGitHubPostScriptRefusalPlace: a refusal inside a script file is reported at script-file:line, the line of the command (or of
// the statement that breaks the plain-line layout) in that script (CRW-875 item 2); a refusal inside a script that another script
// runs is reported at the innermost script.
func TestGitHubPostScriptRefusalPlace(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("post.sh", "# a comment\necho posting\ngh pr comment 1 -b \"$(env)\"\n", 0o644)
	write("lines.sh", "# post the report\n\nls\ngh pr comment 1 -b plain\n", 0o644)
	write("second.sh", "echo one\ngh pr comment 1 -b plain\n", 0o644)
	write("outer.sh", "echo outer\nbash post.sh\n", 0o644)
	write("run.sh", "#!/bin/sh\n\n\ngh pr comment 1 -b \"$(env)\"\n", 0o755)
	for _, c := range []struct{ cmd, place string }{
		{"bash post.sh", "post.sh:3"},
		{"bash lines.sh", "lines.sh:4"},
		{"bash second.sh", "second.sh:2"},
		{"source post.sh", "post.sh:3"},
		{"./run.sh", "./run.sh:4"},
		{"bash outer.sh", "post.sh:3"},
		{"gh pr comment 1 -b \"$(env)\"", "command"},
	} {
		site, denied := githubPostJudgeText(c.cmd, dir)
		if !denied || site.place != c.place {
			t.Errorf("%q: denied=%v place=%q, want a refusal at %q", c.cmd, denied, site.place, c.place)
		}
	}
}

// TestGitHubPostAPIStdinIsInline: gh api reads its body from standard input with --input - or a body=@- field; like --body-file - of
// form A1 that is an inline body, refused with the inline-github-body rule (CRW-875 item 5), whatever file named - exists.
func TestGitHubPostAPIStdinIsInline(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "-"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		"gh api repos/o/r/issues/1/comments --input -",
		"gh api repos/o/r/issues/1/comments --input=-",
		"gh api repos/o/r/issues/1/comments -F body=@-",
		"gh api repos/o/r/issues/1/comments --field body=@-",
		"gh pr comment 1 --body-file -",
	} {
		site, denied := githubPostJudgeText(cmd, dir)
		if !denied || site.rule != githubPostRuleInline {
			t.Errorf("%q: denied=%v rule=%q, want %s", cmd, denied, site.rule, githubPostRuleInline)
		}
	}
}

// TestGitHubPostAbsoluteScript: a script run by an absolute path is read like one run by a relative path (CRW-875 D2); a binary run
// by an absolute path is not a script, a link to a text script is read through, and a path with no file is not a script (nothing
// runs).
func TestGitHubPostAbsoluteScript(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	post := filepath.Join(other, "post.sh")
	write(post, "#!/bin/sh\ngh pr comment 1 -b \"$(env)\"\n", 0o755)
	clean := filepath.Join(other, "clean.sh")
	write(clean, "#!/bin/sh\necho ok\n", 0o755)
	bin := filepath.Join(other, "tool")
	write(bin, "#!\x00binary", 0o755)
	link := filepath.Join(other, "link.sh")
	if err := os.Symlink(post, link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		cmd    string
		denied bool
	}{
		{post, true},
		{"timeout 30 " + post, true},
		{"sudo " + post, true},
		{"cd / && " + post, true},
		{"CDPATH=/x; cd sub; " + post, true},
		{link, true},
		{clean, false},
		{bin, false},
		{filepath.Join(other, "missing.sh"), false}, // nothing runs: not a script
		{other, true},                               // a directory is not a program that can be read
		{"bash " + post, true},
	} {
		if _, got := githubPostJudgeText(c.cmd, dir); got != c.denied {
			t.Errorf("%q: denied=%v, want %v", c.cmd, got, c.denied)
		}
	}
}

// TestGitHubPostGitLongOptionValue: a long option written without = may take the next word as its value, and that word may be --;
// a -- after such an option is not the end of the options, so the --output after it is read (CRW-875). A -- after an option that
// takes no value ends the options as before.
func TestGitHubPostGitLongOptionValue(t *testing.T) {
	for _, c := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"diff", "--no-index", "--src-prefix", "--", "--output=/tmp/b.md", "/dev/null", "/tmp/s.txt"}, false},
		{[]string{"diff", "--src-prefix", "--", "--output", "/tmp/b.md"}, false},
		{[]string{"log", "--format", "--", "--output=x"}, false},
		{[]string{"diff", "--no-index", "--src-prefix", "a/", "--output=/tmp/b.md"}, false},
		{[]string{"log", "--oneline", "--", "--output=x"}, true},
		{[]string{"diff", "--numstat", "--", "--output=x"}, true},
		{[]string{"log", "--format=%H", "--", "--output=x"}, true},
		{[]string{"log", "--", "--output=x"}, true},
		{[]string{"status", "--short", "--branch"}, true},
		{[]string{"commit", "-m", "done"}, false},
		{[]string{"commit", "--allow-empty", "-m", "done"}, false},
	} {
		if got := githubPostGitReadLine(c.args); got != c.ok {
			t.Errorf("git %q: read-only line=%v, want %v", c.args, got, c.ok)
		}
	}
}
