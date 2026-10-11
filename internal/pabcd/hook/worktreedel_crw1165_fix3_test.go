package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// CRW-1165 pre-merge evaluation of 9201b469 (two P0), against the worktree guard, the memory gate and real shells: a startup variable
// the command sets through a name it builds from known parts, and ZDOTDIR in the environment, hand a script an exit function from a file.

// The failed cd of the script is followed by its deletion in a real shell, so the guard has to deny every text that reaches it.
func TestCRW1165FixRound3NamedStartupVariablesReachTheScriptAndAreDenied(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	r := newDelRig(t)
	missing := filepath.Join(r.other, "missing")
	envFile := filepath.Join(r.other, "env.sh")
	wtWrite(t, envFile, "exit() { :; }\n")
	judged := filepath.Join(r.other, "cleanup.sh")
	safe := filepath.Join(r.other, "safe.sh")
	marker := filepath.Join(r.other, "marker")
	wtWrite(t, judged, "cd "+missing+" || exit 1\nrm -rf ../repo\n")
	wtWrite(t, safe, "cd "+missing+" || exit 1\ntouch "+marker+"\n")
	// the variable is exported (empty) in the environment the command inherits, as the hook sees it
	t.Setenv("BASH_ENV", "")
	for _, pre := range []string{
		"N=BASH_; M=ENV; printf -v \"$N$M\" '%s' " + envFile + "; ",
		"N=BASH_; N+=ENV; printf -v \"$N\" '%s' " + envFile + "; ",
		"N=BASH_ENV; export \"$N=" + envFile + "\"; ",
		"N=BASH_ENV; read \"$N\" <<< " + envFile + "; ",
		": ${BASH_ENV:=" + envFile + "}; ",
		"for BASH_ENV in " + envFile + "; do :; done; ",
		"read BASH_ENV <<< " + envFile + "; ",
	} {
		_ = os.Remove(marker)
		cmd := exec.Command(bash, "--noprofile", "--norc", "-c", pre+"bash "+safe)
		cmd.Dir = r.checkout
		cmd.Env = []string{"HOME=" + r.home, "PATH=/usr/bin:/bin", "BASH_ENV="}
		_, _ = cmd.CombinedOutput()
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("%q: bash did not go on after the failed cd in the script (the premise of the test): %v", pre, err)
		}
		r.denied(t, pre+"bash "+judged)
	}
	// the script on its own and a text that sets a variable that is no startup variable keep the proof
	plain := filepath.Join(r.other, "plain.sh")
	wtWrite(t, plain, "cd "+r.other+" || exit 1\nrm -rf .\n")
	r.allowed(t, "bash "+plain, "N=OUT; printf -v \"$N\" '%s' x; bash "+plain, "N=BASH_; M=ENVELOPE; printf -v \"$N$M\" '%s' x; bash "+plain)
	r.intact(t)
}

// zsh reads $ZDOTDIR/.zshenv before a script, and that file can define exit: ZDOTDIR in the hook's environment withdraws the proof.
func TestCRW1165FixRound3ZdotdirOfTheEnvironmentReachesTheScriptAndIsDenied(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is not installed")
	}
	r := newDelRig(t)
	missing := filepath.Join(r.other, "missing")
	zdot := filepath.Join(r.other, "zdot")
	wtWrite(t, filepath.Join(zdot, ".zshenv"), "exit() { :; }\n")
	judged := filepath.Join(r.other, "cleanup.sh")
	safe := filepath.Join(r.other, "safe.sh")
	marker := filepath.Join(r.other, "marker")
	wtWrite(t, judged, "cd "+missing+" || exit 1\nrm -rf ../repo\n")
	wtWrite(t, safe, "cd "+missing+" || exit 1\ntouch "+marker+"\n")
	plain := filepath.Join(r.other, "plain.sh")
	wtWrite(t, plain, "cd "+r.other+" || exit 1\nrm -rf .\n")
	// without ZDOTDIR the form is proven, for the text and for the script
	r.allowed(t, "cd "+r.other+" || exit 1; rm -rf .", "zsh "+plain, "zsh "+judged)
	t.Setenv("ZDOTDIR", zdot)
	cmd := exec.Command(zsh, safe)
	cmd.Dir = r.checkout
	cmd.Env = []string{"HOME=" + r.home, "PATH=/usr/bin:/bin", "ZDOTDIR=" + zdot}
	_, _ = cmd.CombinedOutput()
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("zsh did not go on after the failed cd in the script (the premise of the test): %v", err)
	}
	r.denied(t, "zsh "+judged, "cd "+r.other+" || exit 1; rm -rf .", "zsh "+plain)
	// an empty ZDOTDIR is unset for zsh
	t.Setenv("ZDOTDIR", "")
	r.allowed(t, "cd "+r.other+" || exit 1; rm -rf .", "zsh "+plain)
	r.intact(t)
}

// The memory gate reads the same environment it is given: the write the script makes after the failed cd lands in the memory root.
func TestCRW1165FixRound3MemoryGateWithdrawsTheProofForStartupVariables(t *testing.T) {
	cwd, root, _ := gateScene(t)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	envFile := filepath.Join(other, "env.sh")
	wtWrite(t, envFile, "exit() { :; }\n")
	script := filepath.Join(other, "cleanup.sh")
	wtWrite(t, script, "cd "+filepath.Join(other, "missing")+" || exit 1\nprintf x > note.md\n")
	// the command runs in the memory root, so a write that stays where the failed cd left the shell is a memory write
	run := func(vars map[string]string, command string) string {
		return HandleMemoryWriteGate(gateBash(t, root, command), gateEnvOf(vars))
	}
	_ = cwd
	base := map[string]string{"HOME": filepath.Join(other, "home"), "CODEX_HOME": filepath.Dir(root)}
	with := func(k, v string) map[string]string {
		m := map[string]string{k: v}
		for a, b := range base {
			m[a] = b
		}
		return m
	}
	// control: the form proves the cd, the write lands in the target outside the root
	if out := run(base, "bash "+script); out != "" {
		t.Fatalf("the form was not proven for the script: %s", out)
	}
	for _, c := range []struct {
		vars map[string]string
		text string
	}{
		{with("BASH_ENV", ""), "N=BASH_; M=ENV; printf -v \"$N$M\" '%s' " + envFile + "; bash " + script},
		{with("BASH_ENV", ""), ": ${BASH_ENV:=" + envFile + "}; bash " + script},
		{with("BASH_ENV", "x"), "bash " + script},
		{with("ZDOTDIR", filepath.Join(other, "zdot")), "zsh " + script},
		{with("ZDOTDIR", filepath.Join(other, "zdot")), "cd " + other + " || exit 1; printf x > note.md"},
	} {
		if out := run(c.vars, c.text); out == "" {
			t.Errorf("%q with %v: the write after the failed cd was allowed", c.text, c.vars)
		}
	}
	if out := run(with("ZDOTDIR", ""), "zsh "+script); out != "" {
		t.Errorf("an empty ZDOTDIR withdrew the proof: %s", out)
	}
}

// MemoryGateCommandReadable answers as the gate does (the differential fuzz counts a command the gate allows as readable).
func TestCRW1187FixRound3MemoryGateCommandReadableMatchesTheGate(t *testing.T) {
	cwd, _, _ := gateScene(t)
	for name, body := range map[string]string{"d.json": "{}", "keyword.py": "x = 1\n"} {
		wtWrite(t, filepath.Join(cwd, name), body)
	}
	unset := gateEnvOf(map[string]string{})
	const cmd = "python3 -P -B -m json.tool d.json"
	if out := HandleMemoryWriteGate(gateBash(t, cwd, cmd), unset); out != "" {
		t.Fatalf("the gate refused the safe run: %s", out)
	}
	if !MemoryGateCommandReadable(cmd, cwd, unset) {
		t.Error("the gate allows the -P run with PYTHONPATH unset and MemoryGateCommandReadable calls it unreadable")
	}
	set := gateEnvOf(map[string]string{"PYTHONPATH": "."})
	if out := HandleMemoryWriteGate(gateBash(t, cwd, cmd), set); out == "" {
		t.Fatal("the gate allowed the -P run with PYTHONPATH=.")
	}
	if MemoryGateCommandReadable(cmd, cwd, set) {
		t.Error("the gate refuses the -P run with PYTHONPATH=. and MemoryGateCommandReadable calls it readable")
	}
}
