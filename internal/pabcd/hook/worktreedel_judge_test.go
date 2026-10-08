package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/shellir"
)

// delRig is the fixture of the worktree deletion guard: a managed checkout under the worktrees root and a neighbour.
type delRig struct {
	wtRig
	other string // a directory outside the worktrees root, the allowed neighbour of every denied target
}

func newDelRig(t *testing.T) delRig {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := delRig{wtRig: wtRig{home: home, codexHome: filepath.Join(home, ".codex")}}
	r.worktrees = filepath.Join(r.codexHome, "worktrees")
	r.slotRoot = filepath.Join(r.worktrees, "zk3q")
	r.checkout = filepath.Join(r.slotRoot, "repo")
	r.other = filepath.Join(home, "elsewhere", "build")
	wtWrite(t, filepath.Join(r.checkout, ".git"), "gitdir: /fake/main/.git/worktrees/zk3q\n")
	wtWrite(t, filepath.Join(r.other, "keep"), "x")
	return r
}

func (r delRig) id() WorktreeIdentity { return detectManagedWorktree(r.checkout, r.env()) }

func (r delRig) verdict(cmd string) GuardVerdict { return evaluateCommand(cmd, r.checkout, r.id()) }

// TestNonManagedCwdNeverDenies: outside a managed worktree the guard denies nothing.
func TestNonManagedCwdNeverDenies(t *testing.T) {
	outside := t.TempDir()
	id := detectManagedWorktree(outside, wtEnv(map[string]string{"HOME": outside, "CODEX_HOME": filepath.Join(outside, "nope")}))
	for _, cmd := range []string{"rm -rf " + outside, "rm -rf .", "git worktree remove .", ""} {
		if got := evaluateCommand(cmd, outside, id); got.Deny {
			t.Errorf("%q: denied outside a managed worktree", cmd)
		}
	}
}

// TestRemovalUnderXargsIsRefused: the operands of xargs and find -exec arrive at run time, so a recursive removal
// behind them is refused, while a non-recursive one names files only.
func TestRemovalUnderXargsIsRefused(t *testing.T) {
	r := newDelRig(t)
	r.denied(t, "xargs rm -rf ./build")
	r.denied(t, "find . -name zk3q | xargs rm -rf")
	r.denied(t, "find "+r.checkout+" -exec rm -rf {} +")
	r.allowed(t, "xargs rm ./build", "find . -name zk3q -exec ls {} +")
}

// TestScriptFileIsJudged: a script a shell runs is read in the directory the shell runs in and judged there.
func TestScriptFileIsJudged(t *testing.T) {
	r := newDelRig(t)
	wtWrite(t, filepath.Join(r.checkout, "clean.sh"), "echo ok\nrm -rf ./build\n")
	wtWrite(t, filepath.Join(r.checkout, "wipe.sh"), "rm -rf .\n")
	r.allowed(t, "bash clean.sh")
	r.denied(t, "bash wipe.sh")
	r.denied(t, "bash missing.sh")
}

// TestCommandSizeLimitIsEnforced: a command over the reader's size limit is refused at once, and a command at the limit
// is judged.
func TestCommandSizeLimitIsEnforced(t *testing.T) {
	r := newDelRig(t)
	at := "echo " + strings.Repeat("a", shellir.MaxCommandBytes-len("echo "))
	r.allowed(t, at)
	r.denied(t, at+"a")
	start := time.Now()
	big := "printf x | " + strings.Repeat("echo a ", (1<<20)/7)
	if got := r.verdict(big); !got.Deny {
		t.Errorf("a 1 MiB command was allowed")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("the verdict for 1 MiB took %v", elapsed)
	}
}

// TestWorktreeDelRemovalsAreJudgedByPath: a recursive removal denies when its target is the checkout, the slot, the
// working directory or an ancestor of it; a neighbour is allowed.
func TestWorktreeDelRemovalsAreJudgedByPath(t *testing.T) {
	r := newDelRig(t)
	r.denied(t, "rm -rf .")
	r.denied(t, "rm -rf "+r.checkout)
	r.denied(t, "rm -rf "+r.slotRoot)
	r.denied(t, "rmdir "+r.checkout)
	r.allowed(t, "rm -rf ./build", "rm -rf "+r.other, "rm "+r.checkout, "echo rm -rf .")
	_ = os.Remove
}

// denied and allowed assert the deny decision of each command the reader gives in the rig.
func (r delRig) denied(t *testing.T, cmds ...string) {
	t.Helper()
	for _, cmd := range cmds {
		if got := r.verdict(cmd); !got.Deny {
			t.Errorf("%q: allowed; want a deny", cmd)
		}
	}
}

func (r delRig) allowed(t *testing.T, cmds ...string) {
	t.Helper()
	for _, cmd := range cmds {
		if got := r.verdict(cmd); got.Deny {
			t.Errorf("%q: denied (%s)", cmd, got.Reason)
		}
	}
}

// intact asserts the checkout still exists after the verdicts ran.
func (r delRig) intact(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(r.checkout, ".git")); err != nil {
		t.Errorf("the checkout was changed: %v", err)
	}
}
