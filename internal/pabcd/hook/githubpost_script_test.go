package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-875 red-first cases: a shell that takes its program from a file (bash post.sh, sh post.sh, zsh
// post.sh, dash post.sh, ksh post.sh, source post.sh, . post.sh) shows no post verb in the command text,
// so the guard reads that file and applies CRW-783's closed rule to it. The guard is CRW's own
// protection, so no oracle case is replayed here.

// TestGitHubPostGuardReadsAShellProgramFromAFile: bash, sh, zsh, dash and ksh taking a first operand
// file, and source and the dot builtin taking a file, make the guard read the file. A script whose text
// names a post is refused at the offending line when that line is not form A or exception B.
func TestGitHubPostGuardReadsAShellProgramFromAFile(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	for _, command := range []string{
		"bash post.sh",
		"sh post.sh",
		"zsh post.sh",
		"dash post.sh",
		"ksh post.sh",
		"source post.sh",
		". post.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "post.sh:1")
	}
}

// TestGitHubPostGuardReadsAShellProgramWithAnOption: the file is the shell's first operand, so an option
// before it does not hide the script, and an option that takes a value (-o, -O, --rcfile, --init-file)
// does not let its value stand in for the file.
func TestGitHubPostGuardReadsAShellProgramWithAnOption(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "clean.sh", "echo hi\n")
	for _, command := range []string{
		"bash -x post.sh",
		"sh -e post.sh",
		"bash --norc post.sh",
		"bash -x -e post.sh",
		"bash -o errexit post.sh",
		"sh -o errexit post.sh",
		"bash -O extglob post.sh",
		"bash --rcfile clean.sh post.sh",
		"bash --init-file clean.sh post.sh",
		"bash -- post.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "post.sh:1")
	}
	// The controls: an option that takes a value does not make a clean script a target.
	for _, command := range []string{
		"bash -o errexit clean.sh",
		"bash -O extglob clean.sh",
		"bash --rcfile clean.sh clean.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

// TestGitHubPostGuardAllowsACleanShellProgram: a script is allowed when every executable line satisfies
// form A or exception B on its own; a blank line, a comment line and a shebang are skipped.
func TestGitHubPostGuardAllowsACleanShellProgram(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "b.md", "a clean body\n")
	body := filepath.Join(cwd, "b.md")
	githubPostWrite(t, cwd, "clean.sh", "#!/bin/sh\n\ngh pr comment 1 --body-file "+body+"\n")
	githubPostWrite(t, cwd, "quiet.sh", "# a note\necho 'gh pr comment'\nrg -n 'gh pr comment' internal\n")
	for _, command := range []string{
		"bash clean.sh",
		"sh clean.sh",
		"source clean.sh",
		". clean.sh",
		"bash quiet.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

// TestGitHubPostGuardRefusesAScriptLineOutsideTheRule: a line that is neither form A nor exception B
// fails the script, and the refusal names the file and the line.
func TestGitHubPostGuardRefusesAScriptLineOutsideTheRule(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "b.md", "a clean body\n")
	body := filepath.Join(cwd, "b.md")
	githubPostWrite(t, cwd, "moves.sh", "cd /tmp\ngh pr comment 1 --body-file "+body+"\n")
	command := "bash moves.sh"
	githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "moves.sh:1")
}

// TestGitHubPostGuardRefusesAnUnreadableShellProgram: a script the guard cannot read is refused at the
// file name - absent, a directory, over the 1 MiB bound, or a read error.
func TestGitHubPostGuardRefusesAnUnreadableShellProgram(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	if err := os.Mkdir(filepath.Join(cwd, "adir"), 0o700); err != nil {
		t.Fatal(err)
	}
	githubPostWrite(t, cwd, "big.sh", "#"+strings.Repeat("x", githubPostMaxFileBytes)+"\n")
	for _, c := range []struct{ command, place string }{
		{"bash nowhere.sh", "nowhere.sh"},
		{"source nowhere.sh", "nowhere.sh"},
		{"sh adir", "adir"},
		{"bash big.sh", "big.sh"},
	} {
		githubPostWant(t, githubPostShell(t, cwd, c.command), c.command, githubPostRuleUnread, c.place)
	}
}

// TestGitHubPostGuardLeavesAScriptThatNamesNoPost: a script that names no post is not a target, as the
// closed rule already leaves such a command alone.
func TestGitHubPostGuardLeavesAScriptThatNamesNoPost(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "quiet.sh", "echo hi\nls -l\n")
	githubPostWrite(t, cwd, "reads.sh", "gh pr view 1\n")
	for _, command := range []string{"bash quiet.sh", "sh reads.sh", "source quiet.sh", ". reads.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

// TestGitHubPostGuardReadsAScriptThatNamesAPostThroughTheCreateAlias: gh pr new and gh issue new are the
// built-in aliases of create, so a script that posts through them names a post and must be judged rather
// than passed as a script that names none.
func TestGitHubPostGuardReadsAScriptThatNamesAPostThroughTheCreateAlias(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	for name, body := range map[string]string{
		"prnew.sh":    "gh pr new 1 -b \"$(env)\"\n",
		"issuenew.sh": "gh issue new 1 --body \"$(env)\"\n",
	} {
		githubPostWrite(t, cwd, name, body)
		command := "bash " + name
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, name+":1")
	}
}

// TestGitHubPostGuardRefusesAStandardInputBody: gh api's standard-input bodies are refused as
// inline-github-body at command, explicitly, even when a readable file named "-" sits under the payload's
// working directory, which the guard would otherwise read.
func TestGitHubPostGuardRefusesAStandardInputBody(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "-", "a clean body\n")
	for _, command := range []string{
		"gh api repos/o/r/issues/1/comments --input -",
		"gh api repos/o/r/issues/1/comments --input=-",
		"gh api repos/o/r/issues/1/comments -F body=@-",
		"gh api repos/o/r/issues/1/comments --field body=@-",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleInline, githubPostWhereCommand)
	}
}

// TestGitHubPostGuardResolvesABodyFileLink: a body file path is resolved through its links before the
// temporary-root containment check, so a link under a root that points outside it is refused, and a link
// that points at another file under a root still passes.
func TestGitHubPostGuardResolvesABodyFileLink(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "inside.md", "a clean body\n")
	if err := os.Symlink(filepath.Join(cwd, "inside.md"), filepath.Join(cwd, "inside-link.md")); err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		t.Skipf("the checkout's go.mod is not at %s: %v", repo, err)
	}
	if err := os.Symlink(filepath.Join(repo, "go.mod"), filepath.Join(cwd, "outside-link.md")); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"gh pr comment 1 --body-file inside.md",
		"gh pr comment 1 --body-file inside-link.md",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
	command := "gh pr comment 1 --body-file outside-link.md"
	githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "outside-link.md")
}
