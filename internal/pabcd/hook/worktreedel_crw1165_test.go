package hook

import (
	"os"
	"path/filepath"
	"testing"
)

// CRW-1165 through the guards. One narrow form proves a cd: at the top of the text, `cd <literal path> || exit <digits>`; a failed
// cd ends the shell, so the later relative removals run in the target. Every bypass the first attempt (lane worktree-cd-proof, 13
// P0 findings of the verification) and this one's own review found stays a deny: a removal of the checkout's own directory reached
// when the cd fails and the text goes on.
func TestCRW1165CdOrExitProvesOnlyTheOneForm(t *testing.T) {
	r := newDelRig(t)
	sub := filepath.Join(r.checkout, "sub")
	wtWrite(t, filepath.Join(sub, "keep"), "x")
	o := r.other
	// the form: the target is outside the checkout, so the removals in it are the target's
	r.allowed(t,
		"cd "+o+" || exit 1; rm -rf .",
		"cd "+o+" || exit 1\nrm -rf .",
		"cd "+o+" || exit 0; rm -rf ./build",
		"cd "+o+" || exit 127; find . -delete",
		"cd "+o+" || exit 1; git clean -fdx",
		"echo start; cd "+o+" || exit 1; rm -rf .",
		"cd "+filepath.Dir(o)+" || exit 1; cd build || exit 1; rm -rf .",
		"cd "+o+" || exit 1; rm -rf . ./x",
		"cd "+sub+" || exit 1; rm -rf ./build",
	)
	// a failed cd must not leave the removal to the checkout: every other form
	r.denied(t,
		// plain forms
		"cd "+o+"; rm -rf .",
		"cd "+o+" || true; rm -rf .",
		"cd "+o+" || exit; rm -rf .",
		"cd "+o+" || return; rm -rf .",
		"cd "+o+" || exit $?; rm -rf .",
		"cd "+o+" || exit \"$?\"; rm -rf .",
		"cd "+o+" || exit $X; rm -rf .",
		// the first attempt's 13
		"cd "+o+" || { rm -rf .; exit 1; }; rm -rf ./x",
		"cd "+o+" || { echo no; rm -rf .; exit 1; }",
		"f() { cd "+o+" || return; rm -rf .; }; f",
		"f() { cd "+o+" || return; }; f; rm -rf .",
		"f() { return; set -e; cd "+o+"; rm -rf .; }; f",
		"exit() { :; }; cd "+o+" || exit 1; rm -rf .",
		"function exit { :; }; cd "+o+" || exit 1; rm -rf .",
		"cd "+o+" || { exit() { :; }; exit 1; }; rm -rf .",
		"cd "+o+" || exit 1 2; rm -rf .",
		"cd "+o+" || exit $(echo 1 2); rm -rf .",
		"set -e; cd "+o+" 2>/nonexistent/dir/x; rm -rf .",
		"set -e; cd "+o+" >/nonexistent/dir/x; rm -rf .",
		"set -Qe; cd "+o+"; rm -rf .",
		"set -e; cd "+o+"; rm -rf .",
		"trap : DEBUG; shopt -s extdebug; cd "+o+" || exit 1; rm -rf .",
		"trap 'rm -rf .' EXIT; cd "+o+" || exit 1; rm -rf ./x",
		"enable -n exit; cd "+o+" || exit 1; rm -rf .",
		"source /dev/null; set -e; cd "+o+"; rm -rf .",
		". /dev/null; cd "+o+" || exit 1; rm -rf .",
		"cd \"${NOPE?}\" || exit 1; rm -rf .",
		"cd \"${NOPE:?x}\" || exit 1; rm -rf .",
		"f() { 'local' x=1; cd "+o+" || return; rm -rf .; }; f",
		"alias exit=true; shopt -s expand_aliases; cd "+o+" || exit 1; rm -rf .",
		"eval 'exit() { :; }'; cd "+o+" || exit 1; rm -rf .",
		"builtin eval 'exit() { :; }'; cd "+o+" || exit 1; rm -rf .",
		"command eval 'exit() { :; }'; cd "+o+" || exit 1; rm -rf .",
		"E=eval; $E 'exit() { :; }'; cd "+o+" || exit 1; rm -rf .",
		"unset -f exit; cd "+o+" || exit 1; rm -rf .",
		// the cd or the exit is not the plain form
		"cd -P "+o+" || exit 1; rm -rf .",
		"cd "+o+" 2>/dev/null || exit 1; rm -rf .",
		"cd \"$X\" || exit 1; rm -rf .",
		"cd $X || exit 1; rm -rf .",
		"cd "+o+"/* || exit 1; rm -rf .",
		"cd ~ || exit 1; rm -rf .",
		"cd .. || exit 1; rm -rf .",
		"cd "+o+"/../.. || exit 1; rm -rf .",
		"! cd "+o+" || exit 1; rm -rf .",
		"cd "+o+" || ! exit 1; rm -rf .",
		"cd "+o+" || exit 1 &\nrm -rf .",
		"cd "+o+" || exit 1 | cat; rm -rf .",
		"cd "+o+" || command exit 1; rm -rf .",
		"cd "+o+" || builtin exit 1; rm -rf .",
		"true && cd "+o+" || exit 1; rm -rf .",
		"cd "+o+" || exit 1 || true; rm -rf .",
		"cd "+o+" || exit 1 && rm -rf .",
		// not the top of the text
		"(cd "+o+" || exit 1); rm -rf .",
		"{ cd "+o+" || exit 1; }; rm -rf .",
		"{ cd "+o+" || exit 1; rm -rf .; }; rm -rf .",
		"if true; then cd "+o+" || exit 1; fi; rm -rf .",
		"for i in 1; do cd "+o+" || exit 1; done; rm -rf .",
		"f() { cd "+o+" || exit 1; }; f; rm -rf .",
		"bash -c 'cd "+o+" || exit 1; rm -rf .'",
		"eval 'cd "+o+" || exit 1; rm -rf .'",
		"echo x | { cd "+o+" || exit 1; rm -rf .; }",
		"echo $(cd "+o+" || exit 1; rm -rf .)",
		"find . -name x -exec sh -c 'cd "+o+" || exit 1; rm -rf .' \\;",
		// the removal reaches the checkout from the proven directory
		"cd "+sub+" || exit 1; rm -rf ..",
		"cd "+sub+" || exit 1; rm -rf ../../repo",
		"cd "+o+" || exit 1; rm -rf "+r.checkout,
		"cd "+o+" || exit 1; rm -rf "+r.slotRoot,
		"cd "+r.checkout+" || exit 1; rm -rf .",
		"cd . || exit 1; rm -rf .",
		"cd "+o+" || exit 1; cd "+r.checkout+" || exit 1; rm -rf .",
		"cd "+o+" || exit 1; cd .. ; rm -rf ../.codex/worktrees",
	)
	// a script file is a text of its own: the form at its top proves the cd for the rest of the script, and only there
	wtWrite(t, filepath.Join(o, "exit1.sh"), "cd "+o+" || exit 1\nrm -rf .\n")
	wtWrite(t, filepath.Join(o, "bare.sh"), "cd "+o+" || exit\nrm -rf .\n")
	wtWrite(t, filepath.Join(o, "group.sh"), "{ cd "+o+" || exit 1; }\nrm -rf .\n")
	r.allowed(t, "bash "+filepath.Join(o, "exit1.sh"))
	r.denied(t, "bash "+filepath.Join(o, "bare.sh"), "bash "+filepath.Join(o, "group.sh"))
	r.intact(t)
}

// The existing && form and the unproven ; form are as they were.
func TestCRW1165AndFormAndSemicolonFormAreUnchanged(t *testing.T) {
	r := newDelRig(t)
	r.allowed(t, "cd "+r.other+" && rm -rf .", "(cd "+r.other+" && rmdir .)")
	r.denied(t, "cd "+r.other+"; rm -rf .", "cd "+r.other+" || true; rm -rf .", `find . -name '*.o' -exec sh -c 'cd build; rm -f {}' \;`, "cd /tmp; echo \"$(rm -rf ../repo)\"")
}

// The memory write gate reads the same: a relative write after the form lands in the cd target.
func TestCRW1165MemoryGateReadsTheFormLikeTheWorktreeGuard(t *testing.T) {
	cwd, root, env := gateScene(t)
	other := t.TempDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// into the memory root: still an attempt, whatever the cwd
	if out := HandleMemoryWriteGate(gateBash(t, cwd, "cd "+root+" || exit 1; printf x > note.md"), env); out == "" {
		t.Error("a write into the memory root after the form was allowed")
	}
	if out := HandleMemoryWriteGate(gateBash(t, cwd, "cd "+root+"; printf x > note.md"), env); out == "" {
		t.Error("a write into the memory root after ; was allowed")
	}
	// out of it: the form proves the directory, ; does not
	if out := HandleMemoryWriteGate(gateBash(t, cwd, "cd "+other+" || exit 1; printf x > note.md"), env); out != "" {
		t.Errorf("a write outside the memory root after the form was refused: %s", out)
	}
	if out := HandleMemoryWriteGate(gateBash(t, cwd, "cd "+other+"; printf x > note.md"), env); out == "" {
		t.Error("a write after ; went unjudged")
	}
}
