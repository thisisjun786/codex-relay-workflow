package hook

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
		"bash -oerrexit post.sh",
		"bash -Oextglob post.sh",
		"zsh -oerrexit post.sh",
		"bash --rcfile=x post.sh",
		"bash --init-file=x post.sh",
		"bash --posix post.sh",
		"bash -p post.sh",
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
		"bash -oerrexit clean.sh",
		"zsh -oerrexit clean.sh",
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

// TestGitHubPostGuardRefusesAScriptThatIsNotARegularFile: opening a FIFO read-only waits for a writer, so
// a path that is not a regular file is refused before the open instead of hanging the hook.
func TestGitHubPostGuardRefusesAScriptThatIsNotARegularFile(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	if err := syscall.Mkfifo(filepath.Join(cwd, "pipe.sh"), 0o600); err != nil {
		t.Skipf("mkfifo is unavailable here: %v", err)
	}
	command := "bash pipe.sh"
	githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "pipe.sh")
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

// TestGitHubPostGuardReadsAScriptBehindASignedOption: a plus-prefixed option is an option too, so
// bash +o errexit post.sh and bash +x post.sh read post.sh and must not be refused at the option word.
func TestGitHubPostGuardReadsAScriptBehindASignedOption(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "clean.sh", "echo hi\n")
	for _, command := range []string{
		"bash +o errexit post.sh",
		"bash +O extglob post.sh",
		"bash +x post.sh",
		"sh +x post.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "post.sh:1")
	}
	for _, command := range []string{"bash +o errexit clean.sh", "bash +x clean.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

// TestGitHubPostGuardReadsAScriptBeforeATrailingOption: an option after the operand is an argument, so
// bash post.sh -c runs post.sh and the guard still reads it.
func TestGitHubPostGuardReadsAScriptBeforeATrailingOption(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	for _, command := range []string{"bash post.sh -c", "sh post.sh -c", "bash post.sh -x", "bash -x post.sh -c"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "post.sh:1")
	}
}

// TestGitHubPostGuardReadsAScriptAfterASeparator: everything after -- is an operand, so a script named
// after the separator is still the program file even when its name starts with a dash.
func TestGitHubPostGuardReadsAScriptAfterASeparator(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "-post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	for _, command := range []string{"bash -- -post.sh", "sh -- -post.sh", "bash -x -- -post.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "-post.sh:1")
	}
}

// TestGitHubPostGuardReadsAScriptBehindABundledValueOption: -o and -O take their value from the next word
// inside a bundle too (bash -eo errexit post.sh), so the value is not the program file.
func TestGitHubPostGuardReadsAScriptBehindABundledValueOption(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "clean.sh", "echo hi\n")
	for _, command := range []string{
		"bash -eo errexit post.sh",
		"bash -eO extglob post.sh",
		"bash -xeo pipefail post.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "post.sh:1")
	}
	for _, command := range []string{"bash -eo errexit clean.sh", "bash -eO extglob clean.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

// TestGitHubPostGuardJudgesAScriptArgument: a shell runs its own arguments, so a script that names no post
// does not end the judgement: a post in the arguments is still refused.
func TestGitHubPostGuardJudgesAScriptArgument(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "runner.sh", "\"$@\"\n")
	githubPostWrite(t, cwd, "quiet.sh", "echo hi\n")
	for _, command := range []string{
		"bash runner.sh gh pr comment 1 -b plain",
		"bash runner.sh gh api repos/o/r/issues/1/comments -f body=plain",
		"bash quiet.sh gh pr comment 1 -b plain",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, githubPostWhereCommand)
	}
	// The control: a script that names no post with arguments that name none is not a target.
	for _, command := range []string{"bash quiet.sh", "bash runner.sh ls -l"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

// TestGitHubPostGuardLeavesAShellReadingItsProgramFromStandardInput: -s takes the program from standard
// input, so the first operand is an argument and not the program file; the guard must not read it as one.
// A program the guard cannot read (standard input) is the same boundary as bash < file: the command text
// names no post, so it is not a target.
func TestGitHubPostGuardLeavesAShellReadingItsProgramFromStandardInput(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	for _, command := range []string{"bash -s post.sh", "sh -s post.sh", "bash -es post.sh", "bash -s", "bash < post.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
	piped := "cat post.sh | bash -s"
	githubPostWant(t, githubPostShell(t, cwd, piped), piped, "", "")
}

// TestGitHubPostGuardReadsASourcedPathFile: source and the dot builtin search PATH for a name with no
// slash before the working directory, so the file that runs may be the PATH one and the guard reads it.
func TestGitHubPostGuardReadsASourcedPathFile(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	githubPostWrite(t, bin, "lib.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "lib.sh", "echo hi\n")
	for _, command := range []string{"source lib.sh", ". lib.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, filepath.Join(bin, "lib.sh")+":1")
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

// TestGitHubPostGuardReadsTheScriptTheKernelWouldOpen: a name holding .. is resolved against the real
// directory tree, so a link in the middle of the name is followed before the .. applies. Cleaning the name
// first would read a different file than the shell opens.
func TestGitHubPostGuardReadsTheScriptTheKernelWouldOpen(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	inside := t.TempDir()
	_ = inside
	if err := os.MkdirAll(filepath.Join(cwd, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cwd, "decoy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(cwd, "real"), filepath.Join(cwd, "decoy", "link")); err != nil {
		t.Fatal(err)
	}
	// decoy/link/../post.sh: the kernel follows decoy/link to cwd/real and its .. to cwd, so it runs
	// cwd/post.sh (posting). Collapsing the name first would read cwd/decoy/post.sh (the clean decoy).
	githubPostWrite(t, cwd, "decoy/post.sh", "echo clean\n")
	githubPostWrite(t, cwd, "post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	command := "bash decoy/link/../post.sh"
	githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "decoy/link/../post.sh:1")
}

// TestGitHubPostGuardRefusesAQuotedStandardInputBody: the sentinel is the argument gh receives, so a quoted
// or attached dash is the same standard-input body, not a file name.
func TestGitHubPostGuardRefusesAQuotedStandardInputBody(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "-", "a clean body\n")
	for _, command := range []string{
		"gh pr comment 1 --body-file '-'",
		"gh pr comment 1 --body-file=\"-\"",
		"gh api repos/o/r/issues/1/comments --input '-'",
		"gh api repos/o/r/issues/1/comments -F 'body=@-'",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleInline, githubPostWhereCommand)
	}
}

// TestGitHubPostGuardReadsAScriptBehindAnAttachedValueOption: o and O take their value from the same word
// when more of the bundle follows (zsh -ocorrect post.sh), so the next word is the program file and the
// letters inside the value are not flags.
func TestGitHubPostGuardReadsAScriptBehindAnAttachedValueOption(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "clean.sh", "echo hi\n")
	for _, command := range []string{
		"zsh -ocorrect post.sh",
		"zsh -Oglobdots post.sh",
		"zsh -oshwordsplit post.sh",
		"bash -euxo pipefail post.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "post.sh:1")
	}
	for _, command := range []string{"zsh -ocorrect clean.sh", "bash -euxo pipefail clean.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

// TestGitHubPostGuardReadsAScriptBehindAQuotedPathOperand: a quoted operand is the word the shell runs, so
// quote pieces are not removed twice and a literal close stays in the name.
func TestGitHubPostGuardReadsAScriptBehindAQuotedPathOperand(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "'post.sh'", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "post.sh)", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "post.sh", "echo clean\n")
	for _, c := range []struct{ command, place string }{
		{"bash \"'post.sh'\"", "'post.sh':1"},
		{"bash 'post.sh)'", "post.sh):1"},
	} {
		githubPostWant(t, githubPostShell(t, cwd, c.command), c.command, githubPostRuleUnread, c.place)
	}
	// The controls: a quoted plain name runs the clean file, and an unquoted one is the clean file too.
	for _, command := range []string{"bash 'post.sh'", "bash post.sh", "(bash post.sh)"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

// TestGitHubPostGuardReadsAScriptInTheDirectoryACdMovesTo: a cd before the command moves the working
// directory the shell resolves a relative operand in, so the file read is the one that runs.
func TestGitHubPostGuardReadsAScriptInTheDirectoryACdMovesTo(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	if err := os.Mkdir(filepath.Join(cwd, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	githubPostWrite(t, cwd, "post.sh", "echo clean\n")
	githubPostWrite(t, cwd, "sub/post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "sub/clean.sh", "echo hi\n")
	for _, c := range []struct{ command, place string }{
		{"cd sub && bash post.sh", "post.sh:1"},
		{"cd sub && ./post.sh", "./post.sh:1"},
	} {
		githubPostWant(t, githubPostShell(t, cwd, c.command), c.command, githubPostRuleUnread, c.place)
	}
	// The controls: the moved-to directory's clean script, and the clean file in the payload's directory.
	for _, command := range []string{"cd sub && bash clean.sh", "bash post.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

// TestGitHubPostGuardReadsAPathNamedLikeAWrapper: a command word that is a path names a file the shell
// runs, not the wrapper command its last element happens to spell (./env, ./sh).
func TestGitHubPostGuardReadsAPathNamedLikeAWrapper(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	for _, name := range []string{"env", "sh", "nice", "timeout"} {
		githubPostWrite(t, cwd, name, "gh pr comment 1 -b \"$(env)\"\n")
		if err := os.Chmod(filepath.Join(cwd, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"env", "sh", "nice", "timeout"} {
		for _, command := range []string{"./" + name, "bash ./" + name} {
			githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "./"+name+":1")
		}
	}
	githubPostWant(t, githubPostShell(t, cwd, "./env post.sh"), "./env post.sh", githubPostRuleUnread, "./env:1")
}

// TestGitHubPostGuardReadsAQuotePieceProgram: the shell joins quote pieces into one word, so a program
// spelled in pieces (ba'sh' post.sh) is the shell the guard must read the script of.
func TestGitHubPostGuardReadsAQuotePieceProgram(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	for _, command := range []string{"ba'sh' post.sh", "cd . && ba'sh' post.sh", "sour'ce' post.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "post.sh:1")
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

// TestGitHubPostGuardReadsAScriptBehindAWrapperOrInAList is CRW-875's generation-2 rule D1: the same
// command decomposition CRW-783's reader uses to find a gh post (the list split at ;, &&, ||, &, | and a
// newline; the leading variable assignments; the wrapper commands with their options) finds the shell
// that takes a posting script file as its program, so a wrapper or a list no longer hides the file.
func TestGitHubPostGuardReadsAScriptBehindAWrapperOrInAList(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "clean.sh", "gh pr comment 1 --body-file "+filepath.Join(cwd, "b.md")+"\n")
	githubPostWrite(t, cwd, "b.md", "a clean body\n")
	for _, command := range []string{
		"timeout 30 bash post.sh",
		"sudo bash post.sh",
		"sudo -u root bash post.sh",
		"timeout -s KILL 30 bash post.sh",
		"timeout --signal=KILL 30 bash post.sh",
		"env X=1 bash post.sh",
		"env -i bash post.sh",
		"nohup bash post.sh",
		"X=1 bash post.sh",
		"command bash post.sh",
		"exec bash post.sh",
		"nice bash post.sh",
		"nice -n 5 bash post.sh",
		"time bash post.sh",
		"time -p bash post.sh",
		"stdbuf -o0 bash post.sh",
		"doas bash post.sh",
		"setsid bash post.sh",
		"'timeout' 30 bash post.sh",
		"\"bash\" post.sh",
		"'bash' post.sh",
		"if true; then bash post.sh; fi",
		"! bash post.sh",
		"cd . && bash post.sh",
		"true; bash post.sh",
		"bash post.sh &",
		"bash post.sh | cat",
		"false || bash post.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "post.sh:1")
	}
	// A wrapper over the directly executed script is judged the same way.
	githubPostWant(t, githubPostShell(t, cwd, "sudo ./post.sh"), "sudo ./post.sh", githubPostRuleUnread, "./post.sh:1")
	githubPostWant(t, githubPostShell(t, cwd, "timeout 30 ./post.sh"), "timeout 30 ./post.sh", githubPostRuleUnread, "./post.sh:1")
	// A grouping word, a control word and a trailing option do not hide the shell either.
	for _, command := range []string{
		"(bash post.sh)",
		"{ bash post.sh; }",
		"for i in 1; do bash post.sh; done",
		"while true; do bash post.sh; done",
		"bash post.sh 2>/dev/null",
		"bash post.sh >/dev/null",
		"bash -n post.sh",
		"if bash post.sh; then :; fi",
		"until bash post.sh; do :; done",
		"bash post.sh || true",
		"bash post.sh && true",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "post.sh:1")
	}
	// A wrapper's own -- separator is an operand boundary the shell drops, so the command after it runs.
	for _, command := range []string{
		"timeout 30 -- bash post.sh",
		"timeout -- 30 bash post.sh",
		"sudo -- bash post.sh",
		"env -- bash post.sh",
		"nice -- bash post.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "post.sh:1")
	}
	// The controls for the wrapper separator and the chained lists.
	for _, command := range []string{
		"timeout 30 -- bash clean.sh",
		"sudo -- bash clean.sh",
		"bash clean.sh || true",
		"foo=1 timeout 30 bash clean.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
	// The controls: a wrapper over a script that names no post, commands that run no script, and a wrapper
	// whose option makes it run nothing, stay allowed.
	for _, command := range []string{
		"timeout 30 bash clean.sh",
		"sudo bash clean.sh",
		"cd . && bash clean.sh",
		"cd . && make",
		"sudo apt-get update",
		"timeout 30 ls -l",
		"command -v bash post.sh",
		"sudo -l bash post.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

// TestGitHubPostGuardReadsADirectlyExecutedScript is CRW-875's generation-2 rule D2: a command word that
// is a path holding / and names a regular text file (a #! line, or no NUL byte in its first 4 KiB) is a
// script the shell runs, so the guard reads it and applies the generation-1 line rule.
func TestGitHubPostGuardReadsADirectlyExecutedScript(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "shebang.sh", "#!/bin/sh\ngh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "pyshebang.py", "#!/usr/bin/env python3\nimport subprocess\nsubprocess.run(['gh','pr','comment','1','-b','x'])\n")
	githubPostWrite(t, cwd, "clean.sh", "#!/bin/sh\ngh pr comment 1 --body-file "+filepath.Join(cwd, "b.md")+"\n")
	githubPostWrite(t, cwd, "b.md", "a clean body\n")
	for _, name := range []string{"post.sh", "shebang.sh", "pyshebang.py", "clean.sh"} {
		if err := os.Chmod(filepath.Join(cwd, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(cwd, "scripts"), 0o700); err != nil {
		t.Fatal(err)
	}
	githubPostWrite(t, cwd, "scripts/post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	if err := os.Mkdir(filepath.Join(cwd, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	githubPostWrite(t, cwd, "bin/tool", "\x00\x01binary gh pr comment 1 -b plain\n")
	for _, c := range []struct{ command, place string }{
		{"./post.sh", "./post.sh:1"},
		{"./shebang.sh", "./shebang.sh:2"}, // the #! line is a comment, so the post is on line 2
		// A non-shell shebang script that names a post cannot satisfy the line rule, so it is refused too.
		{"./pyshebang.py", "./pyshebang.py:2"},
		{"scripts/post.sh", "scripts/post.sh:1"},
		{"cd . && ./post.sh", "./post.sh:1"},
		{"./nowhere.sh", "./nowhere.sh"}, // a text script the guard cannot read, as in generation 1
		{"./scripts", "./scripts"},       // a directory is not a text script either
	} {
		githubPostWant(t, githubPostShell(t, cwd, c.command), c.command, githubPostRuleUnread, c.place)
	}
	// The controls: a clean script, a binary (a NUL byte in its first 4 KiB) and a name with no slash stay
	// allowed.
	for _, command := range []string{"./clean.sh", "./bin/tool", "scripts", "clean.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

// githubPostArgvPayload is one already-split argv payload: the shell builds these words itself, so a quote
// character inside a word is part of the word and not something to remove again.
func githubPostArgvPayload(t *testing.T, cwd string, words ...string) string {
	t.Helper()
	items := make([]any, len(words))
	for i, w := range words {
		items[i] = w
	}
	return githubPostPayload(t, "exec_command", cwd, map[string]any{"cmd": items})
}

// TestGitHubPostGuardReadsAnArgvValueAsTheShellBuiltIt is the pre-merge evaluation's d1: an argv array is
// already decoded, so a quote character in a value is part of the file name the shell would pass. Removing
// it again makes the guard read a different (clean) file than the one the command names.
func TestGitHubPostGuardReadsAnArgvValueAsTheShellBuiltIt(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "body.md", "a clean body\n")
	githubPostWrite(t, cwd, "'body.md'", "token "+githubPostFake("ghp_", 20)+"\n")
	for _, c := range []struct {
		label string
		words []string
	}{
		{"argv --body-file 'body.md'", []string{"gh", "pr", "comment", "1", "--body-file", "'body.md'"}},
		{"argv --body-file='body.md'", []string{"gh", "pr", "comment", "1", "--body-file='body.md'"}},
		{"argv --input 'body.md'", []string{"gh", "api", "repos/o/r/issues/1/comments", "--input", "'body.md'"}},
		{"argv --field body=@'body.md'", []string{"gh", "api", "repos/o/r/issues/1/comments", "--field", "body=@'body.md'"}},
		{"argv -F body=@'body.md'", []string{"gh", "api", "repos/o/r/issues/1/comments", "-F", "body=@'body.md'"}},
	} {
		githubPostWant(t, githubPostArgvPayload(t, cwd, c.words...), c.label, githubPostRuleSecret, "'body.md':1")
	}
	// The control: the same argv with the plain clean name stays allowed.
	githubPostWant(t, githubPostArgvPayload(t, cwd, "gh", "pr", "comment", "1", "--body-file", "body.md"),
		"argv --body-file body.md", "", "")
}

// TestGitHubPostGuardReadsAPathReachedAfterAWrapper is the pre-merge evaluation's d2: a path that is a
// command word is the file the shell runs, whatever its last element spells, and that holds after a
// wrapper has been peeled too (command ./env, env ./env).
func TestGitHubPostGuardReadsAPathReachedAfterAWrapper(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	for _, name := range []string{"env", "sh", "command"} {
		githubPostWrite(t, cwd, name, "gh pr comment 1 -b \"$(env)\"\n")
		if err := os.Chmod(filepath.Join(cwd, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ command, place string }{
		{"command ./env", "./env:1"},
		{"env ./env", "./env:1"},
		{"nice ./env", "./env:1"},
		{"timeout 5 ./env", "./env:1"},
		{"sudo -u root ./env", "./env:1"},
		{"exec ./sh", "./sh:1"},
		{"./command", "./command:1"},
	} {
		githubPostWant(t, githubPostShell(t, cwd, c.command), c.command, githubPostRuleUnread, c.place)
	}
}

// TestGitHubPostGuardReadsAPathNamedLikeGh is the pre-merge evaluation's d3: a command word that is a path
// names the file the shell runs, so a readable text script called gh is read and judged rather than trusted
// as the gh program. A path that is not a text script leaves the form to judge the command as before.
func TestGitHubPostGuardReadsAPathNamedLikeGh(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "gh", "gh pr comment 1 -b \"$(env)\"\n")
	if err := os.Chmod(filepath.Join(cwd, "gh"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"./gh pr view 1", "./gh issue list", "./gh pr status"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "./gh:1")
	}
	// A path whose last element is not gh is judged by D2 as it already was.
	githubPostWrite(t, cwd, "quiet-gh", "echo hi\n")
	if err := os.Chmod(filepath.Join(cwd, "quiet-gh"), 0o700); err != nil {
		t.Fatal(err)
	}
	githubPostWant(t, githubPostShell(t, cwd, "./quiet-gh pr view 1"), "./quiet-gh pr view 1", "", "")
	// The control: the plain gh program is still a gh read, not a file.
	githubPostWant(t, githubPostShell(t, cwd, "gh pr view 1"), "gh pr view 1", "", "")
}

// TestGitHubPostGuardSearchesTheCommandsOwnPath is the pre-merge evaluation's d4: a leading PATH= word
// decides where source and the dot builtin search, so the guard must not fall back to its own environment
// and read a clean copy that the shell never reads.
func TestGitHubPostGuardSearchesTheCommandsOwnPath(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	evil := t.TempDir()
	githubPostWrite(t, evil, "lib.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "lib.sh", "echo hi\n")
	// The guard's own PATH holds neither directory, so only the command's assignment can find the script.
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	for _, command := range []string{
		"PATH=" + evil + ":/usr/bin:/bin source lib.sh",
		"PATH=" + evil + ":/usr/bin:/bin . lib.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, filepath.Join(evil, "lib.sh")+":1")
	}
	// The controls: an assignment naming no posting directory, and the guard's own clean search.
	for _, command := range []string{"PATH=other source lib.sh", "PATH=/usr/bin:/bin source lib.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

// TestGitHubPostGuardKeepsASubshellCdInsideItsGroup is the pre-merge evaluation's d5: a cd inside a subshell
// ends with the subshell, so a later relative operand is read in the payload's directory; a cd inside a
// brace group runs in the current shell, so it persists.
func TestGitHubPostGuardKeepsASubshellCdInsideItsGroup(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	if err := os.Mkdir(filepath.Join(cwd, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	githubPostWrite(t, cwd, "post.sh", "echo clean\n")
	githubPostWrite(t, cwd, "sub/post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	// The subshell's cd ends with the group, so bash runs the payload directory's clean script.
	for _, command := range []string{"(cd sub) && bash post.sh", "(cd sub); bash post.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
	// Inside the group the cd still applies, so the posting script under sub is read.
	for _, command := range []string{"(cd sub && bash post.sh)", "{ cd sub; bash post.sh; }"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "post.sh:1")
	}
	// A bare cd still moves the directory for the commands after it.
	githubPostWant(t, githubPostShell(t, cwd, "cd sub && bash post.sh"), "cd sub && bash post.sh", githubPostRuleUnread, "post.sh:1")
}

// TestGitHubPostGuardReadsTheProgramFileInTheWrappersDirectory is the pre-merge evaluation's d6: a wrapper's
// own directory option (env -C dir, env --chdir=dir, env -Cdir) moves the directory the program file is read
// in, so the guard must not read the payload directory's copy instead.
func TestGitHubPostGuardReadsTheProgramFileInTheWrappersDirectory(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	if err := os.Mkdir(filepath.Join(cwd, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The moved-to directory holds the posting script, the payload directory a clean one.
	githubPostWrite(t, cwd, "post.sh", "echo clean\n")
	githubPostWrite(t, cwd, "sub/post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	for _, command := range []string{
		"env -C sub bash post.sh",
		"env --chdir=sub bash post.sh",
		"env -Csub bash post.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "post.sh:1")
	}
	// The control: the moved-to directory holds a clean script, so nothing is refused.
	githubPostWrite(t, cwd, "post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "sub/post.sh", "echo clean\n")
	for _, command := range []string{
		"env -C sub bash post.sh",
		"env --chdir=sub bash post.sh",
		"env -Csub bash post.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

// TestGitHubPostGuardReadsAQuotedOptionName is the fourth pre-merge evaluation's d1: the option name is the
// word the shell builds, so a quoted '--body-file' is the same option and its value is read and checked.
func TestGitHubPostGuardReadsAQuotedOptionName(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "private.md", "GH_TOKEN="+githubPostFake("a", 20)+"\n")
	githubPostWrite(t, cwd, "clean.md", "a clean body\n")
	for _, c := range []struct{ command, place string }{
		{"gh pr comment 1 '--body-file' private.md", "private.md:1"},
		{"gh pr comment 1 \"--body-file\" private.md", "private.md:1"},
		{"gh pr comment 1 '--body-file=private.md'", "private.md:1"},
		{"gh api repos/o/r/issues/1/comments '--input' private.md", "private.md:1"},
		{"gh api repos/o/r/issues/1/comments '-F' 'body=@private.md'", "private.md:1"},
	} {
		githubPostWant(t, githubPostShell(t, cwd, c.command), c.command, githubPostRuleSecret, c.place)
	}
	// A quoted inline body is still the inline-body rule, and the script line that spells it is refused.
	for _, command := range []string{"gh pr comment 1 '--body' 'plain text'", "gh pr comment 1 '--body-file' '-'"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleInline, githubPostWhereCommand)
	}
	// The control: a quoted clean body file stays allowed.
	githubPostWant(t, githubPostShell(t, cwd, "gh pr comment 1 '--body-file' clean.md"),
		"gh pr comment 1 '--body-file' clean.md", "", "")
	// The same shapes inside a script are read through the script's own lines.
	githubPostWrite(t, cwd, "quoted.sh", "gh pr comment 1 '--body-file' private.md\n")
	githubPostWant(t, githubPostShell(t, cwd, "bash quoted.sh"), "bash quoted.sh", githubPostRuleUnread, "quoted.sh:1")
}

// TestGitHubPostGuardKeepsTheShellStateACertainCommandSet is the fourth evaluation's d2: a cd or a PATH= word
// moves the shell only when the command certainly ran in it, so a short-circuited, pipelined or background
// cd must not redirect the file read.
func TestGitHubPostGuardKeepsTheShellStateACertainCommandSet(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	if err := os.Mkdir(filepath.Join(cwd, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	githubPostWrite(t, cwd, "post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "sub/post.sh", "echo clean\n")
	// The cd may not have run, so the shell still runs the payload directory's posting script.
	for _, command := range []string{
		"false && cd sub; bash post.sh",
		"true || cd sub; bash post.sh",
		"cd sub | cat; bash post.sh",
		"cd sub & bash post.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "post.sh:1")
	}
	// A bare cd certainly runs, so the moved-to directory's script is the one read.
	githubPostWrite(t, cwd, "post.sh", "echo clean\n")
	githubPostWrite(t, cwd, "sub/post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWant(t, githubPostShell(t, cwd, "cd sub && bash post.sh"), "cd sub && bash post.sh", githubPostRuleUnread, "post.sh:1")
	// The control: the short-circuited cd with a clean payload script is not a target.
	githubPostWrite(t, cwd, "post.sh", "echo clean\n")
	githubPostWrite(t, cwd, "sub/post.sh", "echo clean\n")
	for _, command := range []string{"false && cd sub; bash post.sh", "cd sub | cat; bash post.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

// TestGitHubPostGuardReadsAQuotedRedirectionLookingOperand is the fourth evaluation's d3: quote removal
// happens after the syntax is read, so a quoted word that only spells a redirection is a literal program
// file and is read, while an unquoted one is a redirection.
func TestGitHubPostGuardReadsAQuotedRedirectionLookingOperand(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, ">post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "<post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	for _, c := range []struct{ command, place string }{
		{"bash '>post.sh'", ">post.sh:1"},
		{"bash '<post.sh'", "<post.sh:1"},
		{"sh '>post.sh'", ">post.sh:1"},
		{"bash \">post.sh\"", ">post.sh:1"},
	} {
		githubPostWant(t, githubPostShell(t, cwd, c.command), c.command, githubPostRuleUnread, c.place)
	}
	// The control: the unquoted operator is a redirection, so no program file is named and the text names
	// no post (the file it would create does not exist yet).
	githubPostWrite(t, cwd, "post.sh", "echo clean\n")
	for _, command := range []string{"bash > post.sh", "bash < post.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

// TestGitHubPostGuardSearchesTheSourcedPathAcrossAList is the fourth evaluation's d4: a PATH= word an earlier
// command sets is still in force for a later source or dot in the same shell.
func TestGitHubPostGuardSearchesTheSourcedPathAcrossAList(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	evil := t.TempDir()
	githubPostWrite(t, evil, "lib.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "lib.sh", "echo hi\n")
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	for _, command := range []string{
		"PATH=" + evil + ":/usr/bin:/bin; source lib.sh",
		"PATH=" + evil + ":/usr/bin:/bin; . lib.sh",
		"export PATH=" + evil + ":/usr/bin:/bin; source lib.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, filepath.Join(evil, "lib.sh")+":1")
	}
	// A PATH= word on a command that may not run is still read: the guard judges the file the text names,
	// and a command that might source the posting file is refused (fail closed).
	githubPostWant(t, githubPostShell(t, cwd, "false && PATH="+evil+" source lib.sh"),
		"false && PATH=... source lib.sh", githubPostRuleUnread, filepath.Join(evil, "lib.sh")+":1")
	// The control: a PATH naming no posting file.
	for _, command := range []string{"PATH=/usr/bin:/bin; source lib.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
}

// TestGitHubPostGuardSearchesASourcedPathWithoutCleaning is the fourth evaluation's d5: a PATH entry holding
// .. is resolved by the kernel against the real tree, so a link in the middle of it is followed before the
// .. applies and the guard reads the file the shell would read.
func TestGitHubPostGuardSearchesASourcedPathWithoutCleaning(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real", "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "decoy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real", "child"), filepath.Join(root, "decoy", "link")); err != nil {
		t.Fatal(err)
	}
	// The kernel follows decoy/link to real/child and its .. to real, so it reads real/lib.sh (posting).
	// Collapsing the entry first would read decoy/lib.sh (the clean decoy).
	githubPostWrite(t, root, "real/lib.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, root, "decoy/lib.sh", "echo clean\n")
	githubPostWrite(t, cwd, "lib.sh", "echo clean\n")
	t.Setenv("PATH", filepath.Join(root, "decoy")+"/link/..:/usr/bin:/bin")
	command := "source lib.sh"
	githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread,
		filepath.Join(root, "decoy")+"/link/../lib.sh:1")
}

// TestGitHubPostGuardRefusesAnUnreadablePathScriptNamedGh is the fourth evaluation's d6: a regular file named
// gh that the guard cannot read as a text script is refused, not trusted by the form's name.
func TestGitHubPostGuardRefusesAnUnreadablePathScriptNamedGh(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "gh", "#!/bin/sh\n"+strings.Repeat("#", githubPostMaxFileBytes)+"\ngh pr comment 1 -b \"$(env)\"\n")
	if err := os.Chmod(filepath.Join(cwd, "gh"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"./gh pr view 1", "./gh issue list"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "./gh")
	}
	// The control: a readable clean path script named gh is read and allowed.
	githubPostWrite(t, cwd, "gh", "#!/bin/sh\necho hi\n")
	githubPostWant(t, githubPostShell(t, cwd, "./gh pr view 1"), "./gh pr view 1", "", "")
}

// TestGitHubPostGuardReadsAPathLineInsideAScript is the fourth evaluation's d7: a line of a script that runs
// a path named gh is judged by the direct-execution read, not trusted by the program's name.
func TestGitHubPostGuardReadsAPathLineInsideAScript(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "clean.md", "a clean body\n")
	githubPostWrite(t, cwd, "gh", "#!/bin/sh\ngh pr comment 1 -b \"$(env)\"\n")
	if err := os.Chmod(filepath.Join(cwd, "gh"), 0o700); err != nil {
		t.Fatal(err)
	}
	githubPostWrite(t, cwd, "wrapper.sh", "./gh pr comment 1 --body-file clean.md\n")
	for _, command := range []string{"bash wrapper.sh", "source wrapper.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "wrapper.sh:1")
	}
	// The control: a clean path named gh inside a script stays allowed.
	githubPostWrite(t, cwd, "gh", "#!/bin/sh\necho hi\n")
	githubPostWrite(t, cwd, "quiet.sh", "./gh pr view 1\n")
	githubPostWant(t, githubPostShell(t, cwd, "bash quiet.sh"), "bash quiet.sh", "", "")
}

// TestGitHubPostGuardBoundsTheScriptInScriptWalk: a script whose line runs another path script is judged a
// bounded number of levels deep and then refused, so a self-referential pair terminates instead of recursing.
func TestGitHubPostGuardBoundsTheScriptInScriptWalk(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	// A clean reading line keeps the script a target by the mention test; the self-reference is the loop.
	githubPostWrite(t, cwd, "loop.sh", "git commit -m 'gh pr comment 1 -b x'\n./loop.sh\n")
	githubPostWrite(t, cwd, "pair1.sh", "git commit -m 'gh pr comment 1 -b x'\n./pair2.sh\n")
	githubPostWrite(t, cwd, "pair2.sh", "git commit -m 'gh pr comment 1 -b x'\n./pair1.sh\n")
	for _, name := range []string{"loop.sh", "pair1.sh", "pair2.sh"} {
		if err := os.Chmod(filepath.Join(cwd, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ command, place string }{
		{"./loop.sh", "./loop.sh:2"},
		{"bash loop.sh", "loop.sh:2"},
		{"./pair1.sh", "./pair1.sh:2"},
	} {
		githubPostWant(t, githubPostShell(t, cwd, c.command), c.command, githubPostRuleUnread, c.place)
	}
}
