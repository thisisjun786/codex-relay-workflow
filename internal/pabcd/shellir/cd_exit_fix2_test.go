package shellir

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// CRW-1165 verification fix round 2 (two P0).

// P0: zsh reads `cd +N` and `cd -N` as entries of the directory stack (`cd +0` is the current directory and succeeds), and `cd =name` as
// the path of a command: none of them is the literal directory, so the form proves nothing with such an operand.
func TestCRW1165FixRound2StackAndEqualsOperandsProveNothing(t *testing.T) {
	for _, command := range []string{
		"cd +0 || exit 1; rm x",
		"cd +1 || exit 1; rm x",
		"cd -0 || exit 1; rm x",
		"cd =ls || exit 1; rm x",
		"cd '+0' || exit 1; rm x",
		"cd \"+0\" || exit 1; rm x",
	} {
		for _, script := range []bool{false, true} {
			got, err := crw1165Read(command, nil, script)
			if err != nil {
				continue
			}
			for _, d := range got {
				if d != "" && d != "/work" {
					t.Errorf("script=%v %q: a directory %q was proven", script, command, d)
				}
			}
		}
	}
	// the same operands after && (the success proof the reader had before) name no directory either
	for _, command := range []string{"cd +0 && rm x", "cd -0 && rm x", "cd =ls && rm x", "cd +1 && rm x"} {
		for _, read := range []func(string) (Result, error){
			func(s string) (Result, error) { return AnalyzeEnvProvenDirectory(s, "/work", nil) },
			func(s string) (Result, error) { return Analyze(s, "/work") },
		} {
			r, err := read(command)
			if err != nil {
				continue
			}
			for _, e := range r.Execs {
				if e.Name == "rm" && e.Dir.Known && e.Dir.Path != "/work" {
					t.Errorf("%q: rm placed in %q", command, e.Dir.Path)
				}
			}
		}
	}
	// a literal name with + or = inside it is still a literal directory
	got, err := crw1165Read("cd /outside/a+0 || exit 1; rm x", nil, false)
	if err != nil || len(got) != 1 || got[0] != "/outside/a+0" {
		t.Errorf("cd /outside/a+0: %q %v", got, err)
	}
}

// Against real zsh: `cd +0` succeeds and stays where it was, which is why the operand proves nothing.
func TestCRW1165FixRound2ZshStaysInPlaceForPlusZero(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is not installed")
	}
	dir := t.TempDir()
	cmd := exec.Command(zsh, "-f", "-c", "cd +0 || exit 1; touch marker")
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("zsh: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "marker")); err != nil {
		t.Fatalf("zsh did not stay in the directory: %v", err)
	}
}

// P0: a text that assigns a variable by a name it does not spell (read "$N", printf -v "$N$M") may set BASH_ENV or ENV for the scripts it
// runs, whose exit is then a function of that file: every script record of such a text is tainted, whether or not the text itself
// used the proof.
func TestCRW1165FixRound2NamesTheTextDoesNotSpellTaintItsScripts(t *testing.T) {
	for _, text := range []string{
		"read N <<< BASH_; read M <<< ENV; printf -v \"$N$M\" '%s' /x/env.sh; bash ./cleanup.sh",
		"read \"$N\" <<< /x/env.sh; bash ./cleanup.sh",
		"printf -v \"$N\" '%s' /x/env.sh; sh ./cleanup.sh",
		"bash ./cleanup.sh; read \"$N\" <<< x",
	} {
		for _, o := range []ScriptOptions{{}, {Startup: func(string) (string, bool) { return "", false }}} {
			r, err := AnalyzeScriptProven(text, "/work", o)
			if err != nil {
				continue
			}
			seen := false
			for _, e := range r.Execs {
				if e.Kind == KindScriptFile {
					seen = true
					if !e.ExitTainted {
						t.Errorf("%q: the script record is not tainted", text)
					}
				}
			}
			if !seen {
				t.Errorf("%q: no script record", text)
			}
		}
		r, err := AnalyzeEnvProvenDirectory(text, "/work", nil)
		if err == nil {
			for _, e := range r.Execs {
				if e.Kind == KindScriptFile && !e.ExitTainted {
					t.Errorf("%q (env reading): the script record is not tainted", text)
				}
			}
		}
	}
}
