package shellir

import (
	"strings"
	"testing"
)

// CRW-1165: one narrow form proves that a cd succeeded for the rest of the text: at the top of the text, exactly
// `cd <literal path> || exit <literal digits>`. A failed cd then ends the shell, so every later statement runs in the target.
// Every other form that looks like it (the 13 bypasses the first attempt found, and the forms around them) keeps the directory
// unproven, as `;` does.

func crw1165Dirs(t *testing.T, command string, lookup func(string) (string, bool), script bool) []string {
	t.Helper()
	dirs, err := crw1165Read(command, lookup, script)
	if err != nil {
		t.Fatalf("%q: %v", command, err)
	}
	return dirs
}

// crw1165Read is the directory of each rm record, "" where it is not proven; err is the reader's refusal of the text.
func crw1165Read(command string, lookup func(string) (string, bool), script bool) ([]string, error) {
	var r Result
	var err error
	if script {
		r, err = AnalyzeScriptProvenDirectory(command, "/work", false)
	} else {
		r, err = AnalyzeEnvProvenDirectory(command, "/work", lookup)
	}
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range r.Execs {
		if e.Name == "rm" {
			if e.Dir.Known {
				dirs = append(dirs, e.Dir.Path)
			} else {
				dirs = append(dirs, "")
			}
		}
	}
	return dirs, nil
}

func TestCRW1165TheOneFormProvesTheDirectory(t *testing.T) {
	for _, tc := range []struct {
		command string
		want    []string
	}{
		{"cd /outside || exit 1; rm x", []string{"/outside"}},
		{"cd /outside || exit 0; rm x", []string{"/outside"}},
		{"cd /outside || exit 127\nrm x", []string{"/outside"}},
		{"cd /outside || exit 1; rm x; rm y", []string{"/outside", "/outside"}},
		{"cd /outside || exit 1; (rm x)", []string{"/outside"}},
		{"cd /outside || exit 1; { rm x; }", []string{"/outside"}},
		{"cd /outside || exit 1; if true; then rm x; fi", []string{"/outside"}},
		{"cd /outside || exit 1; cd child || exit 1; rm x", []string{"/outside/child"}},
		{"cd /outside || exit 1; cd /another && rm x", []string{"/another"}},
		{"cd 'sp ace' || exit 1; rm x", []string{"/work/sp ace"}},
		{"cd child || exit 1; rm x", []string{"/work/child"}},
		{"cd ./child || exit 1; rm x", []string{"/work/child"}},
		{"echo a; cd /outside || exit 1; rm x", []string{"/outside"}},
		{"cd /a; cd /outside || exit 1; rm x", []string{"/outside"}},
		{"cd /outside || exit 1; rm x | cat", []string{"/outside"}},
		{"cd /outside || exit 1;\nrm x\nrm y", []string{"/outside", "/outside"}},
	} {
		for _, script := range []bool{false, true} {
			got := crw1165Dirs(t, tc.command, nil, script)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("script=%v %q: directories %q, want %q", script, tc.command, got, tc.want)
			}
		}
	}
}

// The forms that do not prove it. Each keeps every rm directory unknown ("") as the same text with `;` does.
func TestCRW1165EveryOtherFormKeepsTheDirectoryUnproven(t *testing.T) {
	for _, command := range []string{
		// the plain forms the Linear text lists
		"cd /outside; rm x",
		"cd /outside || true; rm x",
		"cd /outside || exit; rm x",
		"cd /outside || return; rm x",
		"set -e; cd /outside; rm x",
		"if cd /outside; then rm x; fi",
		// the exit status is not a literal number
		"cd /outside || exit $?; rm x",
		"cd /outside || exit \"$?\"; rm x",
		"cd /outside || exit $X; rm x",
		"cd /outside || exit $((1)); rm x",
		"cd /outside || exit `echo 1`; rm x",
		"cd /outside || exit $(echo 1 2); rm x",
		"cd /outside || exit 1 2; rm x",
		"cd /outside || exit -1; rm x",
		"cd /outside || exit +1; rm x",
		"cd /outside || exit 1x; rm x",
		"cd /outside || exit 0x1; rm x",
		"cd /outside || exit ''; rm x",
		"cd /outside || exit 1234567; rm x",
		"cd /outside || exit -- 1; rm x",
		"cd /outside || exit\t1 2; rm x",
		// the failure branch holds more than the exit
		"cd /outside || { rm x; exit 1; }; rm y",
		"cd /outside || { exit 1; }; rm x",
		"cd /outside || (exit 1); rm x",
		"cd /outside || { echo no; exit 1; }; rm x",
		"cd /outside || echo no && exit 1; rm x",
		"cd /outside || rm x; exit 1",
		"cd /outside || exit 1 || rm x; rm y",
		"cd /outside || ! exit 1; rm x",
		"cd /outside || exit 1 &\nrm x",
		"cd /outside || exit 1 | cat; rm x",
		"cd /outside || FOO=1 exit 1; rm x",
		"cd /outside || exit 1 >/dev/null; rm x",
		"cd /outside || exit 1 </dev/null; rm x",
		"cd /outside || command exit 1; rm x",
		"cd /outside || builtin exit 1; rm x",
		"cd /outside || \\exit 1; rm x",
		"cd /outside || 'exit' 1; rm x",
		"cd /outside || $E 1; rm x",
		// the cd is not the plain literal form
		"cd -P /outside || exit 1; rm x",
		"cd -L /outside || exit 1; rm x",
		"cd -- /outside || exit 1; rm x",
		"cd /outside >/dev/null || exit 1; rm x",
		"cd /outside 2>&1 || exit 1; rm x",
		"cd /outside </dev/null || exit 1; rm x",
		"cd $X || exit 1; rm x",
		"cd \"$X\" || exit 1; rm x",
		"cd ${X:-/outside} || exit 1; rm x",
		"cd $(pwd) || exit 1; rm x",
		"cd `pwd` || exit 1; rm x",
		"cd ~ || exit 1; rm x",
		"cd ~/x || exit 1; rm x",
		"cd /out* || exit 1; rm x",
		"cd /out? || exit 1; rm x",
		"cd /out[a] || exit 1; rm x",
		"cd /{a,b} || exit 1; rm x",
		"cd /out'*' || exit 1; rm x",
		"cd - || exit 1; rm x",
		"cd || exit 1; rm x",
		"cd .. || exit 1; rm x",
		"cd /a/../b || exit 1; rm x",
		"cd /outside child || exit 1; rm x",
		"cd '' || exit 1; rm x",
		"FOO=1 cd /outside || exit 1; rm x",
		"! cd /outside || exit 1; rm x",
		"command cd /outside || exit 1; rm x",
		"builtin cd /outside || exit 1; rm x",
		"chdir /outside || exit 1; rm x",
		"pushd /outside || exit 1; rm x",
		"cd /outside & rm x",
		// not the top of the text
		"true && cd /outside || exit 1; rm x",
		"cd /outside || exit 1 && rm x",
		"cd /outside || exit 1 || true; rm x",
		"{ cd /outside || exit 1; }; rm x",
		"{ cd /outside || exit 1; rm x; }",
		"(cd /outside || exit 1; rm x)",
		"(cd /outside || exit 1); (rm x)",
		"if true; then cd /outside || exit 1; fi; rm x",
		"if true; then cd /outside || exit 1; rm x; fi",
		"for i in 1; do cd /outside || exit 1; done; rm x",
		"while true; do cd /outside || exit 1; rm x; break; done",
		"case x in x) cd /outside || exit 1;; esac; rm x",
		"case x in x) cd /outside || exit 1; rm x;; esac",
		"f() { cd /outside || exit 1; rm x; }; f",
		"f() { cd /outside || exit 1; }; f; rm x",
		"function f { cd /outside || exit 1; rm x; }; f",
		"echo $(cd /outside || exit 1; rm x)",
		"cat <(cd /outside || exit 1; rm x)",
		"coproc { cd /outside || exit 1; rm x; }",
		"cd /outside || exit 1 &\nrm x",
		"time cd /outside || exit 1; rm x",
		"time { cd /outside || exit 1; rm x; }",
		"cd /outside || exit 1 | rm x",
		"x=$(cd /outside || exit 1); rm x",
		"bash -c 'cd /outside || exit 1; rm x'",
		"sh -c \"cd /outside || exit 1; rm x\"",
		"eval 'cd /outside || exit 1; rm x'",
		"trap 'cd /outside || exit 1; rm x' EXIT",
		"source ./x; cd /outside || exit 1; rm x",
		"find . -name x -exec sh -c 'cd /outside || exit 1; rm x' \\;",
		"echo x | xargs sh -c 'cd /outside || exit 1; rm x'",
		// a builtin or the exit itself may be changed by the rest of the text, anywhere in it
		"exit() { :; }; cd /outside || exit 1; rm x",
		"exit () { :; }; cd /outside || exit 1; rm x",
		"function exit { :; }; cd /outside || exit 1; rm x",
		"cd /outside || exit 1; exit() { :; }; rm x",
		"cd() { :; }; cd /outside || exit 1; rm x",
		"f() { exit() { :; }; }; f; cd /outside || exit 1; rm x",
		"f() { :; }; cd /outside || exit 1; rm x",
		"alias exit=true; cd /outside || exit 1; rm x",
		"shopt -s expand_aliases; alias exit=true; cd /outside || exit 1; rm x",
		"unalias -a; cd /outside || exit 1; rm x",
		"enable -n exit; cd /outside || exit 1; rm x",
		"enable -f /x/y.so exit; cd /outside || exit 1; rm x",
		"trap 'exit' ERR; cd /outside || exit 1; rm x",
		"trap : DEBUG; cd /outside || exit 1; rm x",
		"trap 'return 1' DEBUG; shopt -s extdebug; cd /outside || exit 1; rm x",
		"trap 'rm x' EXIT; cd /outside || exit 1; rm x",
		"source ./defs.sh; cd /outside || exit 1; rm x",
		". ./defs.sh; cd /outside || exit 1; rm x",
		"eval 'exit() { :; }'; cd /outside || exit 1; rm x",
		"eval \"$DEFS\"; cd /outside || exit 1; rm x",
		"builtin eval 'x'; cd /outside || exit 1; rm x",
		"command eval 'x'; cd /outside || exit 1; rm x",
		"$CMD; cd /outside || exit 1; rm x",
		"\"$CMD\" -n exit; cd /outside || exit 1; rm x",
		"${CMD} x; cd /outside || exit 1; rm x",
		"'eval' 'x'; cd /outside || exit 1; rm x",
		"\\eval x; cd /outside || exit 1; rm x",
		"declare -f exit; cd /outside || exit 1; rm x",
		"local x; cd /outside || exit 1; rm x",
		"'local' x=1; cd /outside || exit 1; rm x",
		"typeset -fx exit; cd /outside || exit 1; rm x",
		"unset -f exit; cd /outside || exit 1; rm x",
		"set -o posix; cd /outside || exit 1; rm x",
		"set -e; cd /outside || exit 1; rm x",
		"set -Qe; cd /outside || exit 1; rm x",
		"set +e; cd /outside || exit 1; rm x",
		"exec 3>/dev/null; cd /outside || exit 1; rm x",
		"hash -p /bin/true exit; cd /outside || exit 1; rm x",
		"coproc exit; cd /outside || exit 1; rm x",
		"cd /outside || exit 1; rm x; eval ':'",
		"cd /outside || exit 1; rm x; trap : DEBUG",
		"cd /outside || exit 1; rm x; source ./x",
		"setopt aliases; cd /outside || exit 1; rm x",
		"emulate sh; cd /outside || exit 1; rm x",
		"autoload exit; cd /outside || exit 1; rm x",
		// the cd search path
		"CDPATH=/a; cd out || exit 1; rm x",
		"export CDPATH=/a; cd out || exit 1; rm x",
	} {
		for _, script := range []bool{false, true} {
			got, err := crw1165Read(command, nil, script)
			if err != nil {
				continue // the reader refuses the text: nothing is proven
			}
			for _, d := range got {
				if d != "" && d != "/work" {
					t.Errorf("script=%v %q: a directory %q was proven (all %q)", script, command, d, got)
				}
			}
		}
	}
}

// The environment of the hook may define the builtin before the text runs: BASH_ENV is read by a non-interactive bash first, and an
// exported function named exit or cd comes with the environment.
func TestCRW1165TheEnvironmentCanRedefineTheBuiltins(t *testing.T) {
	const command = "cd /outside || exit 1; rm x"
	if got := crw1165Dirs(t, command, nil, false); len(got) != 1 || got[0] != "/outside" {
		t.Fatalf("without the variables: %q", got)
	}
	for _, name := range []string{"BASH_ENV", "ENV", "BASH_FUNC_exit%%", "BASH_FUNC_cd%%", "SHELLOPTS", "BASHOPTS", "CDPATH"} {
		lookup := func(k string) (string, bool) {
			if k == name {
				return "() { :; }", true
			}
			return "", false
		}
		if got := crw1165Dirs(t, command, lookup, false); len(got) != 1 || got[0] == "/outside" {
			t.Errorf("%s in the environment: directory %q was proven", name, got)
		}
	}
	// A variable the environment has but empty is unset for the shell.
	empty := func(k string) (string, bool) {
		if k == "BASH_ENV" {
			return "", true
		}
		return "", false
	}
	if got := crw1165Dirs(t, command, empty, false); len(got) != 1 || got[0] != "/outside" {
		t.Errorf("an empty BASH_ENV: %q", got)
	}
}

// The reading with the legacy rule (AnalyzeEnv, which assumes a literal cd succeeds) is unchanged, and so is the && form.
func TestCRW1165LegacyReadingAndAndFormAreUnchanged(t *testing.T) {
	r, err := AnalyzeEnv("cd /outside; rm x", "/work", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range r.Execs {
		if e.Name == "rm" && (!e.Dir.Known || e.Dir.Path != "/outside") {
			t.Errorf("legacy reading: %+v", e.Dir)
		}
	}
	if got := crw1165Dirs(t, "cd /outside && rm x", nil, false); len(got) != 1 || got[0] != "/outside" {
		t.Errorf("&& form: %q", got)
	}
}
