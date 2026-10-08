package hook

import (
	"strings"
	"testing"
)

// TestWorktreeDelDenyRowsHaveControls: a corpus row that denies a removal of the managed checkout or its slot has a control, the
// same command aimed at the unrelated neighbour directory, which the guard allows. A row whose removal target arrives at run
// time or whose program is refused as unreadable has no control: the reader cannot see the other target, so the control is
// refused for the same reason (see the exempt list and CRW-1028.md).
func TestWorktreeDelDenyRowsHaveControls(t *testing.T) {
	r := newDelRig(t)
	fill := strings.NewReplacer("{CHECKOUT}", r.checkout, "{SLOT}", r.slotRoot, "{WORKTREES}", r.worktrees,
		"{OTHER}", r.other, "{HOME}", r.home)
	for _, row := range worktreeDelCorpusRows() {
		if !row.deny || !(strings.Contains(row.cmd, "{CHECKOUT}") || strings.Contains(row.cmd, "{SLOT}")) {
			continue
		}
		if worktreeDelControlExempt(row.cmd) {
			continue
		}
		control := strings.NewReplacer("{CHECKOUT}", "{OTHER}", "{SLOT}", "{OTHER}").Replace(row.cmd)
		if v := r.verdict(fill.Replace(control)); v.Deny {
			t.Errorf("control of %q: refused (%s), want allowed: %q", row.cmd, v.Reason, control)
		}
	}
}

// worktreeDelControlExempt names the rows whose removal target the reader cannot place at the other directory: a run-time
// operand (xargs, find, entr, parallel), an unreadable program, a shell string, a here-document, or a git subcommand in a
// spelling the closed list does not hold (a non-ASCII spelling, such as a Kelvin sign in a git subcommand, is refused as unreadable).
func worktreeDelControlExempt(cmd string) bool {
	for _, r := range cmd {
		if r > 127 {
			return true // a non-ASCII spelling (a Kelvin sign in a git subcommand) is refused as unreadable
		}
	}
	for _, w := range []string{"xargs", "find", "entr", "parallel", "$", "\x60", "<<", "eval", "bash", "sh ", "dash", "zsh", "exec", "source", ". ", "python", "node", "perl", "ruby"} {
		if strings.Contains(cmd, w) {
			return true
		}
	}
	return false
}
