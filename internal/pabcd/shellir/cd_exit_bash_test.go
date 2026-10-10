package shellir

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1165 against real shells (bash, and zsh where installed). The proof says: wherever the reader takes a command to run in the cd target, the command does not run
// when the cd fails. So every text below is run by bash with a target that does not exist; for each `touch` the reader places in the
// target (Dir known and equal to it), bash must not have made its marker. The texts are the forms the issue and the review name, put
// in front of and inside every wrapper that moves a statement away from the top, with every prefix that could change exit or cd.
func TestCRW1165ProofHoldsInRealBash(t *testing.T) {
	type shell struct {
		name string
		args []string
	}
	var shells []shell
	if p, err := exec.LookPath("bash"); err == nil {
		shells = append(shells, shell{p, []string{"--noprofile", "--norc", "-c"}})
	}
	if p, err := exec.LookPath("zsh"); err == nil {
		shells = append(shells, shell{p, []string{"-f", "-c"}})
	}
	if len(shells) == 0 {
		t.Skip("neither bash nor zsh is installed")
	}
	const target = "/nonexistent-crw1165/target"
	forms := []string{
		"cd T || exit 1",
		"cd T || exit 0",
		"cd T || exit",
		"cd T || exit $?",
		"cd T || exit 1 2",
		"cd T || exit 1 || true",
		"cd T || { exit 1; }",
		"cd T || (exit 1)",
		"cd T || return",
		"cd T || true",
		"cd T",
		"cd T 2>/dev/null || exit 1",
		"cd -P T || exit 1",
		"cd \"T\" || exit 1",
		"cd 'T' || exit 1",
		"! cd T || exit 1",
		"cd T || ! exit 1",
		"cd T || exit 1 &",
		"cd T || exit 1 | cat",
		"true && cd T || exit 1",
		"cd T || exit 1 && true",
		"cd T || command exit 1",
		"cd T || builtin exit 1",
		"cd T || \\exit 1",
	}
	prefixes := []string{
		"",
		"set -e; ",
		"set +e; ",
		"set -Qe; ",
		"set -o posix; ",
		"exit() { :; }; ",
		"function exit { :; }; ",
		"f() { :; }; ",
		"alias exit=true; ",
		"shopt -s expand_aliases; alias exit=true; ",
		"enable -n exit; ",
		"disable exit; ",
		"functions[exit]=:; ",
		"aliases[exit]=true\n",
		"aliases=(exit true)\n",
		"disable -r exit; ",
		"unhash -f exit; ",
		"unfunction exit; ",
		"exit() { :; }; export -f exit; ",
		"trap : DEBUG; ",
		"trap 'touch MARKDIR/trap' EXIT; ",
		"trap 'touch MARKDIR/err' ERR; set -E; ",
		"shopt -s extdebug; trap 'return 1' DEBUG; ",
		"eval 'exit() { :; }'; ",
		"source /dev/null; ",
		"unset -f exit; ",
		"hash -p /bin/true exit; ",
		"cd() { :; }; ",
		"CDPATH=/nonexistent-crw1165; ",
		"exec 3>&1; ",
		"local x 2>/dev/null; ",
		"declare -f > /dev/null; ",
		"echo start; ",
		"touch MARKDIR/before; ",
	}
	wrappers := []string{
		"%s",
		"{ %s; }",
		"( %s )",
		"if true; then %s; fi",
		"for i in 1; do %s; done",
		"f() { %s; }; f",
		"echo $(%s)",
		"bash -c '%s'",
		"eval '%s'",
		"trap '%s' EXIT",
	}
	scratch := t.TempDir()
	marks := filepath.Join(scratch, "marks")
	n, claims := 0, 0
	for _, wrap := range wrappers {
		for _, pre := range prefixes {
			for _, form := range forms {
				body := strings.ReplaceAll(form, "T", target) + "; touch MARKDIR/after"
				text := strings.ReplaceAll(pre+fmt.Sprintf(wrap, body), "MARKDIR", marks)
				res, err := AnalyzeEnvProvenDirectory(text, "/work", nil)
				if err != nil {
					continue // the reader refuses the text: nothing is claimed
				}
				var claimed []string
				for _, e := range res.Execs {
					if e.Name != "touch" || !e.Dir.Known || e.Dir.Path != target || len(e.Args) != 1 || !e.Args[0].Known {
						continue
					}
					claimed = append(claimed, e.Args[0].Value)
				}
				if len(claimed) == 0 {
					continue
				}
				claims++
				for _, sh := range shells {
					if err := os.RemoveAll(marks); err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(marks, 0o755); err != nil {
						t.Fatal(err)
					}
					run := exec.Command(sh.name, append(append([]string{}, sh.args...), text)...)
					run.Dir = scratch
					run.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + scratch, "LC_ALL=C"}
					_, _ = run.CombinedOutput()
					for _, m := range claimed {
						if _, err := os.Stat(m); err == nil {
							t.Errorf("the reader places %s in %s, but %s ran it with the cd failing: %s", m, target, filepath.Base(sh.name), text)
						}
					}
					n++
				}
			}
		}
	}
	if claims == 0 {
		t.Fatal("the reader claimed no directory in any text: the test proves nothing")
	}
	t.Logf("%d texts claimed a proven directory and were run by the shells", n)
}
