package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1165 verification fix round 1 (two P0). The proof of `cd <literal> || exit <digits>` stands on `exit` ending the shell, so
// anything that makes exit do something else must withdraw it, in the text and in the scripts the text runs:
//   - zsh's disable removes a builtin: `disable exit; cd missing || exit 1; rm -rf .` goes on after the failed cd;
//   - zsh defines a function or an alias named exit by assigning to a table (`functions[exit]=:`, `aliases[exit]=true` on an earlier line);
//   - a function named exit, exported by the parent shell (export -f, or BASH_FUNC_exit%% through env), is inherited by the bash of a script
//     file whose body is then read on its own and looked plain.

// crw1165FixCommands are the texts the guard must deny, each with the deletion the failed cd would not stop.
func crw1165FixCommands(r delRig, script string) []string {
	o := r.other
	missing := filepath.Join(o, "missing")
	return []string{
		"disable exit; cd " + missing + " || exit 1; rm -rf .",
		"disable -r exit; cd " + missing + " || exit 1; rm -rf .",
		"unhash -f exit; cd " + missing + " || exit 1; rm -rf .",
		"unfunction exit; cd " + missing + " || exit 1; rm -rf .",
		"zmodload -u zsh/main; cd " + missing + " || exit 1; rm -rf .",
		"cd " + missing + " || exit 1; disable exit; rm -rf .",
		"functions[exit]=':'; cd " + missing + " || exit 1; rm -rf .",
		"aliases[exit]=true\ncd " + missing + " || exit 1\nrm -rf .",
		"aliases=(exit true)\ncd " + missing + " || exit 1\nrm -rf .",
		"N=functions; read \"$N[exit]\" <<< :; cd " + missing + " || exit 1; rm -rf .",
		"read \"$N\" <<< :; cd " + missing + " || exit 1; rm -rf .",
		// an exported function named exit reaches the script the text runs
		"exit() { :; }; export -f exit; bash " + script,
		"exit() { :; }; export -f exit; sh " + script,
		"function exit { :; }; export -f exit; bash " + script,
		"exit() { :; }; declare -fx exit; bash " + script,
		"export -f exit; bash " + script,
		"env 'BASH_FUNC_exit%%=() { :; }' bash " + script,
		"BASH_FUNC_exit%%='() { :; }' bash " + script,
		"env BASH_ENV=" + filepath.Join(o, "env.sh") + " bash " + script,
		"BASH_ENV=" + filepath.Join(o, "env.sh") + "; export BASH_ENV; bash " + script,
		"export BASH_ENV=" + filepath.Join(o, "env.sh") + "; bash " + script,
		"set -a; BASH_ENV=" + filepath.Join(o, "env.sh") + "; bash " + script,
		"env SHELLOPTS=posix bash " + script,
		"export SHELLOPTS=posix; bash " + script,
		// a text that is not plain for the proof does not hand it to the script it runs
		"trap : DEBUG; bash " + script,
		"alias ls=true; bash " + script,
		"set -e; bash " + script,
		"eval ':'; bash " + script,
	}
}

func TestCRW1165FixRound1Refusals(t *testing.T) {
	r := newDelRig(t)
	script := filepath.Join(r.other, "cleanup.sh")
	wtWrite(t, script, "cd "+filepath.Join(r.other, "missing")+" || exit 1\nrm -rf .\n")
	wtWrite(t, filepath.Join(r.other, "env.sh"), "exit() { :; }\n")
	r.denied(t, crw1165FixCommands(r, script)...)
	// the form alone, at the top of a text or a script, stays allowed
	wtWrite(t, filepath.Join(r.other, "plain.sh"), "cd "+r.other+" || exit 1\nrm -rf .\n")
	r.allowed(t,
		"cd "+r.other+" || exit 1; rm -rf .",
		"bash "+filepath.Join(r.other, "plain.sh"),
		"echo start; bash "+filepath.Join(r.other, "plain.sh"),
	)
	r.intact(t)
}

// The hook's own environment is what the command inherits: an exported exit function or a startup file in it withdraws the proof for the
// text and for the scripts it runs, as the memory gate does with the environment it is given.
func TestCRW1165FixRound1TheHookEnvironmentWithdrawsTheProof(t *testing.T) {
	for _, name := range []string{"BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS", "BASH_FUNC_exit%%", "BASH_FUNC_cd%%"} {
		t.Run(name, func(t *testing.T) {
			r := newDelRig(t)
			script := filepath.Join(r.other, "cleanup.sh")
			wtWrite(t, script, "cd "+filepath.Join(r.other, "missing")+" || exit 1\nrm -rf .\n")
			r.allowed(t, "cd "+r.other+" || exit 1; rm -rf .", "bash "+script)
			t.Setenv(name, "() { :; }")
			r.denied(t, "cd "+r.other+" || exit 1; rm -rf .", "bash "+script)
			// an empty variable is unset for the shell
			t.Setenv(name, "")
			r.allowed(t, "cd "+r.other+" || exit 1; rm -rf .", "bash "+script)
		})
	}
}

// Against real shells: whatever the guard allows, the shell must not have run the deletion's stand-in (a touch) after the failed cd.
func TestCRW1165FixRound1AllowedTextsStopInRealShells(t *testing.T) {
	r := newDelRig(t)
	missing := filepath.Join(r.other, "missing")
	marker := filepath.Join(r.other, "marker")
	for _, shell := range []string{"bash", "zsh"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		body := "cd " + missing + " || exit 1\n"
		safe := filepath.Join(r.other, "safe-"+shell+".sh")
		judged := filepath.Join(r.other, "judged-"+shell+".sh")
		wtWrite(t, safe, body+"touch "+marker+"\n")
		wtWrite(t, judged, body+"rm -rf .\n")
		prefixes := []string{
			"", "disable exit; ", "disable -r exit; ", "unhash -f exit; ", "unfunction exit; ", "functions[exit]=:; ", "aliases[exit]=true\n",
			"exit() { :; }; export -f exit; ", "env 'BASH_FUNC_exit%%=() { :; }' ", "trap : DEBUG; ", "alias exit=true; ",
		}
		for _, pre := range prefixes {
			for _, run := range []string{pre + "cd " + missing + " || exit 1; ", pre + shell + " "} {
				var real, judge string
				if strings.HasSuffix(run, "; ") {
					real, judge = run+"touch "+marker, run+"rm -rf ."
				} else {
					real, judge = run+safe, run+judged
				}
				verdict := r.verdict(judge)
				_ = os.Remove(marker)
				cmd := exec.Command(path, "-f", "-c", real)
				if shell == "bash" {
					cmd = exec.Command(path, "--noprofile", "--norc", "-c", real)
				}
				cmd.Dir = r.checkout
				cmd.Env = []string{"HOME=" + r.home, "PATH=/usr/bin:/bin"}
				_, _ = cmd.CombinedOutput()
				if _, err := os.Stat(marker); err == nil && !verdict.Deny {
					t.Errorf("%s: %q was allowed, but the shell went on after the failed cd", shell, judge)
				}
			}
		}
	}
	r.intact(t)
}
