package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// CRW-1165 verification fix round 2 (two P0), against the worktree guard and real shells.

// zsh's `cd +0` succeeds and stays in the checkout: the guard must not take it for a cd into a directory named +0.
func TestCRW1165FixRound2ZshStackOperandDoesNotProveTheCd(t *testing.T) {
	r := newDelRig(t)
	r.denied(t,
		"cd +0 || exit 1; rm -rf .",
		"cd -0 || exit 1; rm -rf .",
		"cd +1 || exit 1; rm -rf .",
		"cd =ls || exit 1; rm -rf .",
		"cd +0 && rm -rf .",
		"cd +1 && rm -rf .",
		"cd =ls && rm -rf .",
	)
	if zsh, err := exec.LookPath("zsh"); err == nil {
		marker := filepath.Join(r.checkout, "marker")
		cmd := exec.Command(zsh, "-f", "-c", "cd +0 || exit 1; touch marker")
		cmd.Dir = r.checkout
		cmd.Env = []string{"HOME=" + r.home, "PATH=/usr/bin:/bin"}
		_, _ = cmd.CombinedOutput()
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("zsh did not stay in the checkout after cd +0: %v", err)
		}
		_ = os.Remove(marker)
	}
	r.intact(t)
}

// A text that sets BASH_ENV by a name it builds (read, printf -v) hands the script it runs an exit function from that file; the
// guard must not give the script's body the proof, though the text itself never used it.
func TestCRW1165FixRound2IndirectStartupReachesTheScript(t *testing.T) {
	r := newDelRig(t)
	missing := filepath.Join(r.other, "missing")
	envFile := filepath.Join(r.other, "env.sh")
	wtWrite(t, envFile, "exit() { :; }\n")
	judged := filepath.Join(r.other, "cleanup.sh")
	safe := filepath.Join(r.other, "safe.sh")
	marker := filepath.Join(r.other, "marker")
	wtWrite(t, judged, "cd "+missing+" || exit 1\nrm -rf .\n")
	wtWrite(t, safe, "cd "+missing+" || exit 1\ntouch "+marker+"\n")
	// the variable is exported (empty) in the environment the command inherits, as the hook sees it
	t.Setenv("BASH_ENV", "")
	pre := "read N <<< BASH_; read M <<< ENV; printf -v \"$N$M\" '%s' " + envFile + "; bash "
	r.denied(t, pre+judged, "read \"$N\" <<< "+envFile+"; bash "+judged)
	// the script on its own, and a text that spells every name it assigns, keep the proof
	plain := filepath.Join(r.other, "plain.sh")
	wtWrite(t, plain, "cd "+r.other+" || exit 1\nrm -rf .\n")
	r.allowed(t, "bash "+plain, "N=1; bash "+plain)
	if bash, err := exec.LookPath("bash"); err == nil {
		cmd := exec.Command(bash, "--noprofile", "--norc", "-c", pre+safe)
		cmd.Dir = r.checkout
		cmd.Env = []string{"HOME=" + r.home, "PATH=/usr/bin:/bin", "BASH_ENV="}
		_, _ = cmd.CombinedOutput()
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("bash did not go on after the failed cd in the script: %v", err)
		}
	}
	r.intact(t)
}
