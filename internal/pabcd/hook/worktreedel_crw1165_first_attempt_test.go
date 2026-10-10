package hook

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The refusal tests of the first attempt at CRW-1165 (lane worktree-cd-proof, final ce5e66ca7), kept: each command is a way the
// deletion that follows a failed cd reaches the checkout, found by the verification of that attempt, one P0 per round. They are
// run as written (their `exit` has no status, which the narrow form does not accept) and again with a digit status (`exit 1`), the
// one spelling the narrow form accepts, so that each is refused by what the text around the form does and not by its spelling.
// The allowed shapes of that attempt (set -e, || return, if cd, a bare exit, a group in the failure branch) are not taken.

var crw1165ExitWord = regexp.MustCompile(`\bexit\b(\s*)([;&\n})]|$)`)

// crw1165WithStatus gives every bare exit of the commands the status 1.
func crw1165WithStatus(cmds []string) []string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = crw1165ExitWord.ReplaceAllString(c, "exit 1$1$2")
	}
	return out
}

func crw1165DenyBoth(t *testing.T, r delRig, cmds ...string) {
	t.Helper()
	r.denied(t, cmds...)
	r.denied(t, crw1165WithStatus(cmds)...)
}

// Round 0: the protection stays with the directory named, and a cd that can fail without stopping the text keeps the previous directory.
func TestCRW1165FirstAttemptRound0Refusals(t *testing.T) {
	r := newDelRig(t)
	o := r.other
	// The proof does not move the protection: the directory named is checked, not assumed safe.
	crw1165DenyBoth(t, r,
		"cd "+r.checkout+" || exit; rm -rf .",
		"cd "+r.slotRoot+" || exit; rm -rf .",
		"cd "+r.worktrees+" || exit; find . -delete",
		"cd "+o+" || exit; rm -rf "+r.checkout,
		"cd "+o+" || exit; cd .. ; rm -rf elsewhere ../.codex",
		"set -e; cd "+r.checkout+"; rm -rf .",
		"if cd "+r.checkout+"; then rm -rf .; fi",
		"f() { cd "+r.checkout+" || return; rm -rf .; }; f",
	)
	// A cd that can fail without stopping the text keeps the previous directory, so the relative deletion is refused.
	crw1165DenyBoth(t, r,
		"cd "+o+"; rm -rf .",
		"cd "+o+"; find . -delete",
		"cd "+o+" || true; rm -rf .",
		"cd "+o+" || :; rm -rf .",
		"cd "+o+" || echo no; rm -rf .",
		"cd "+o+" || { echo no; }; rm -rf .",
		"cd "+o+" || (exit 1); rm -rf .",
		"cd "+o+" || exit 1 2; rm -rf .",
		"cd "+o+" || exit >/nonexistent/crw1165; rm -rf .",
		"cd "+o+" || exit & rm -rf .",
		"cd "+o+" || { exit 1 & }; rm -rf .",
		"cd "+o+" || return; rm -rf .",
		"bash -c 'cd "+o+" || return; rm -rf .'",
		"f() { cd "+o+" || return; }; f; rm -rf .",
		"f() { bash -c 'cd "+o+" || return; rm -rf .'; }; f",
		"exit() { :; }; cd "+o+" || exit; rm -rf .",
		"! cd "+o+" || exit; rm -rf .",
		"cd "+o+" || cd "+o+" || exit; rm -rf .",
		"if true; then cd "+o+" || exit; fi; rm -rf .",
		"{ cd "+o+" || exit; } >/nonexistent/crw1165; rm -rf .", // the failed redirection skips the group and its exit
		"f() { cd "+o+" || exit; }; f >/nonexistent/crw1165; rm -rf .",
		"cd "+o+" || exit; cd sub; rm -rf .",
		"cd "+o+" || exit; cd \"$X\" || exit; rm -rf .",
		"set +e; cd "+o+"; rm -rf .",
		"set -e; set +e; cd "+o+"; rm -rf .",
		"cd "+o+"; set -e; rm -rf .",
		"set -e; cd "+o+" || true; rm -rf .",
		"set -e; cd "+o+" && true; rm -rf .",
		"set -e; true && cd "+o+"; rm -rf .",
		"set -e; ! cd "+o+"; rm -rf .",
		"set -e; { cd "+o+"; rm -rf .; } || true",
		"set -e; (cd "+o+"; rm -rf .) || true",
		"set -e; if { cd "+o+"; rm -rf .; }; then :; fi",
		"set -e; if cd "+o+"; then :; fi; rm -rf .",
		"set -e; if cd "+o+"; then :; else rm -rf .; fi",
		"set -e; f() { cd "+o+"; rm -rf .; }; f",
		"set -e; for i in 1; do cd "+o+"; rm -rf .; done",
		"set -e; echo $(cd "+o+"; rm -rf .)",
		"set -e; bash -c 'cd "+o+"; rm -rf .'",
		"set -e; trap 'echo done' EXIT; cd "+o+"; rm -rf .",
		"set -e; source ./lib.sh; cd "+o+"; rm -rf .",
		"set -e; command set +e; cd "+o+"; rm -rf .",
		"set -e; shopt -uo errexit; cd "+o+"; rm -rf .",
		"if cd "+o+"; then :; fi; rm -rf .",
		"if cd "+o+"; then :; else rm -rf .; fi",
		"if ! cd "+o+"; then rm -rf .; fi",
		"while cd "+o+"; do rm -rf .; break; done",
	)
	// carried scripts read their own text: set -e in the script counts, the caller's does not.
	wtWrite(t, filepath.Join(o, "loose.sh"), "cd "+o+"\nrm -rf .\n")
	wtWrite(t, filepath.Join(o, "inherit.sh"), "cd "+o+"\nrm -rf .\n")
	crw1165DenyBoth(t, r, "bash "+filepath.Join(o, "loose.sh"), "set -e; bash "+filepath.Join(o, "inherit.sh"))
}

// CRW-1165 verification round 1: the proof that a failed cd stops the text holds only where the stop is certain; the failure
// branch, an early return, a replaced exit, an operand that expands to several words, a skipped set -e and a failed set never
// make a deletion pass that could touch the checkout.
func TestCRW1165FirstAttemptRound1Refusals(t *testing.T) {
	r := newDelRig(t)
	o := r.other
	missing := filepath.Join(o, "missing")
	crw1165DenyBoth(t, r,
		"cd "+missing+" || { rm -rf .; exit; }",
		"cd "+missing+" || { echo $(rm -rf .); exit; }",
		"f() { cd "+missing+" || { rm -rf .; return; }; }; f",
		"f() { cd "+missing+" || { echo $(rm -rf .); return; }; }; f",
		"f() { cd "+missing+" || { return; exit; }; }; f; rm -rf .",
		"f() { return; cd "+o+" || exit; }; f; rm -rf .",
		"cd "+missing+" || { exit() { :; }; exit; }; rm -rf .",
		"cd "+missing+" || exit $(echo 1 2); rm -rf .",
		"cd "+missing+" || exit \"$@\"; rm -rf .",
		"cd "+missing+" || exit *; rm -rf .",
		"enable -n exit; cd "+missing+" || exit; rm -rf .",
		"set -e >/nonexistent/crw1165; cd "+missing+"; rm -rf .",
		"{ set -e; } >/nonexistent/crw1165; cd "+missing+"; rm -rf .",
		"set -Qe; cd "+missing+"; rm -rf .",
		"f() { return; set -e; }; f; cd "+missing+"; rm -rf .",
	)
}

// CRW-1165 verification round 2: the counterexamples of the second review. Each lets a failed cd go on in the checkout, so each
// relative deletion after it stays refused; the cleanup shapes the issue allows still pass.
func TestCRW1165FirstAttemptRound2Refusals(t *testing.T) {
	r := newDelRig(t)
	o, c := r.other, r.checkout
	missing := filepath.Join(o, "missing")
	wtWrite(t, filepath.Join(c, "lib.sh"), "trap 'set +e' DEBUG\n")
	crw1165DenyBoth(t, r,
		"cd "+o+" || exit; f() { cd "+c+" || exit; return; cd "+o+" || exit; }; f; rm -rf .",
		"set -e; f() { set +e; return; set -e; }; f; cd "+missing+"; rm -rf .",
		"trap 'set +e' DEBUG; set -e; cd "+missing+"; rm -rf .",
		"trap 'exit() { :; }' DEBUG; cd "+missing+" || exit; rm -rf .",
		"enable -n set; set -e; cd "+missing+"; rm -rf .",
		"source ./lib.sh; set -e; cd "+missing+"; rm -rf .",
		"cd "+missing+" || { echo $((1/0)); exit; }\nrm -rf .",
		"f() { exit() { :; }; }; f && cd "+missing+" || exit; rm -rf .",
		"for i in 1 2; do cd "+o+" || exit; rm -rf .; cd "+c+"; exit() { :; }; done",
		"trap 'cd "+o+" || exit; rm -rf .' EXIT",
	)
}

// CRW-1165 post-evaluation round (three P0 findings): a chain that moved and came back, a function's local variable and local
// option, and a program named by a path each leave the deletion in the checkout, so each stays refused. The one-command shapes
// and the real builtins keep passing.
func TestCRW1165FirstAttemptRound3Refusals(t *testing.T) {
	r := newDelRig(t)
	o, c := r.other, r.checkout
	missing := filepath.Join(o, "missing")
	crw1165DenyBoth(t, r,
		// d1
		"cd "+o+" || exit; if cd "+c+" && false && cd "+o+"; then :; else find . -delete; fi",
		"cd "+o+" || exit; if cd "+c+"; cd "+o+"; then :; else rm -rf .; fi",
		"cd "+o+" || exit; if true && cd "+c+" && cd "+o+"; then :; else find . -delete; fi",
		// d2
		"f() { local -; set -e; }; f; cd "+missing+"; find . -delete",
		"x="+c+"; f() { local x="+o+"; }; f; cd \"$x\" || exit; find . -delete",
		"x="+c+"; f() { declare x="+o+"; }; f; cd \"$x\" || exit; rm -rf .",
		"bash -c 'f() { local -; set -e; }; f; cd "+missing+"; find . -delete'",
		"bash -c 'x="+c+"; f() { local x="+o+"; }; f; cd \"$x\" || exit; find . -delete'",
		// d3
		"/tmp/crw1165-tools/cd "+o+" || exit; find . -delete",
		"/tmp/crw1165-tools/set -e; cd "+missing+"; find . -delete",
		"f() { /tmp/crw1165-tools/cd "+o+" || return; find . -delete; }; f",
		"bash -c '/tmp/crw1165-tools/cd "+o+" || exit; find . -delete'",
	)
}

// CRW-1165 verification round 3: a function's local variable or local options declared by a command the parser does not read as a
// declaration clause ('local' x, 'local' -, eval 'local x') leaves the caller in the checkout, so the deletion stays refused,
// directly and carried by bash -c.
func TestCRW1165FirstAttemptRound4Refusals(t *testing.T) {
	r := newDelRig(t)
	o, c := r.other, r.checkout
	missing := filepath.Join(o, "missing")
	crw1165DenyBoth(t, r,
		"x="+c+"; f() { 'local' x; x="+o+"; }; f; cd \"$x\" || exit; find . -delete",
		"f() { 'local' -; set -e; }; f; cd "+missing+"; find . -delete",
		"x="+c+"; f() { command local x="+o+"; }; f; cd \"$x\" || exit; rm -rf .",
		"x="+c+"; f() { eval 'local x="+o+"'; }; f; cd \"$x\" || exit; rm -rf .",
		"f() { eval 'local -'; set -e; }; f; cd "+missing+"; rm -rf .",
		"bash -c 'x="+c+"; f() { \"local\" x; x="+o+"; }; f; cd \"$x\" || exit; find . -delete'",
		"bash -c 'f() { \"local\" -; set -e; }; f; cd "+missing+"; find . -delete'",
	)
}

// The memory write gate reads the same: a chain that moved and came back, and a variable the text assigned and lost track of, must not
// make a write to MEMORY.md land in the directory a proof names while the shell writes into the memories directory.
func TestCRW1165FirstAttemptMemoryGateRefusals(t *testing.T) {
	cwd, root, env := gateScene(t)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	home, _ := env("HOME")
	cmds := []string{
		"cd " + root + " && false && cd " + cwd + " || { printf bad > MEMORY.md; exit; }",
		"cd " + cwd + " || exit; cd " + root + " && false && cd " + cwd + " || { printf bad > MEMORY.md; exit; }",
		"HOME=" + root + "; for i in 1; do :; done; cd \"$HOME\" || exit; printf x > MEMORY.md",
		"HOME=" + root + "; read y; cd \"$HOME\" || exit; printf x > MEMORY.md",
		"f() { local HOME=" + home + "; }; HOME=" + root + "; f; cd \"$HOME\" || exit; printf x > MEMORY.md",
		"/tmp/crw1165-tools/cd " + home + " || exit; cd " + root + " && printf x > MEMORY.md",
		"bash -c '" + "cd " + root + " && false && cd " + cwd + " || { printf bad > MEMORY.md; exit; }'",
		"cd " + root + " || exit; printf x > MEMORY.md",
		"cd " + root + " || exit; cd .. ; printf x > memories/MEMORY.md",
	}
	for _, cmd := range append(cmds, crw1165WithStatus(cmds)...) {
		if reason := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, cmd), env)); !strings.Contains(reason, "MEMORY-WRITE-GATE") {
			t.Errorf("%s: %s", cmd, reason)
		}
	}
}
