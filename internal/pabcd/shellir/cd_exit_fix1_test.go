package shellir

import (
	"strings"
	"testing"
)

// CRW-1165 verification fix round 1: what withdraws the proof of `cd <literal> || exit <digits>`, in the text and across the script
// boundary (an exported function named exit, or a text that is not plain, reaches the script files it runs).

func TestCRW1165FixRound1MoreWaysToChangeExit(t *testing.T) {
	for _, command := range []string{
		"disable exit; cd /outside || exit 1; rm x",
		"disable -r exit; cd /outside || exit 1; rm x",
		"cd /outside || exit 1; disable exit; rm x",
		"unhash -f exit; cd /outside || exit 1; rm x",
		"unfunction exit; cd /outside || exit 1; rm x",
		"zmodload -u zsh/main; cd /outside || exit 1; rm x",
		"zcompile x; cd /outside || exit 1; rm x",
		"sched +1 exit; cd /outside || exit 1; rm x",
		"export -f exit; cd /outside || exit 1; rm x",
		"readonly -f exit; cd /outside || exit 1; rm x",
		"'disable' exit; cd /outside || exit 1; rm x",
		"\\disable exit; cd /outside || exit 1; rm x",
		"$D exit; cd /outside || exit 1; rm x",
		// zsh defines functions and aliases by assigning to a table, bash has BASH_ALIASES, and options are a table too
		"functions[exit]=':'; cd /outside || exit 1; rm x",
		"functions[exit]=:\ncd /outside || exit 1\nrm x",
		"cd /outside || exit 1; functions[exit]=:; rm x",
		"aliases[exit]=true\ncd /outside || exit 1\nrm x",
		"aliases=(exit true)\ncd /outside || exit 1\nrm x",
		"functions+=(exit :)\ncd /outside || exit 1\nrm x",
		"FOO=1 functions[exit]=:; cd /outside || exit 1; rm x",
		"galiases[exit]=true\ncd /outside || exit 1\nrm x",
		"BASH_ALIASES[exit]=true\ncd /outside || exit 1\nrm x",
		"options[cdablevars]=on; cd out || exit 1; rm x",
		"echo functions; cd /outside || exit 1; rm x",
		// a name the text does not spell may be one of those tables: the form is read again without the proof
		"read \"$N\" <<< :; cd /outside || exit 1; rm x",
		"printf -v \"$N\" :; cd /outside || exit 1; rm x",
		"export \"$N\"=:; cd /outside || exit 1; rm x",
	} {
		for _, script := range []bool{false, true} {
			got, err := crw1165Read(command, nil, script)
			if err != nil {
				continue
			}
			for _, d := range got {
				if d != "" && d != "/work" {
					t.Errorf("script=%v %q: a directory %q was proven (all %q)", script, command, d, got)
				}
			}
		}
	}
}

// A function named exit is refused by the reader whatever else the text holds: it would shadow a modelled program, and it is the
// way an exported function reaches the scripts the text runs.
func TestCRW1165FixRound1AFunctionNamedExitIsRefused(t *testing.T) {
	for _, command := range []string{
		"exit() { :; }",
		"function exit { :; }",
		"exit() { :; }; export -f exit; bash ./x.sh",
		"f() { exit() { :; }; }; f",
		"echo $(exit() { :; })",
	} {
		for _, read := range []func(string) error{
			func(s string) error { _, err := AnalyzeEnvProvenDirectory(s, "/work", nil); return err },
			func(s string) error { _, err := AnalyzeScriptProvenDirectory(s, "/work", false); return err },
			func(s string) error { _, err := Analyze(s, "/work"); return err },
		} {
			if err := read(command); err == nil {
				t.Errorf("%q was read", command)
			}
		}
	}
}

// BASH_FUNC_* names an exported function: assigning one before a shell runs changes the builtins of its script, as BASH_ENV does.
func TestCRW1165FixRound1ExportedFunctionVariablesAreCodeEnvironment(t *testing.T) {
	for _, command := range []string{
		"env 'BASH_FUNC_exit%%=() { :; }' bash ./x.sh",
		"BASH_FUNC_exit%%='() { :; }' bash ./x.sh",
		"env SHELLOPTS=posix bash ./x.sh",
		"SHELLOPTS=posix bash ./x.sh",
		"env BASHOPTS=expand_aliases bash ./x.sh",
	} {
		if _, err := AnalyzeScriptProvenDirectory(command, "/work", false); err == nil {
			t.Errorf("%q was read", command)
		}
	}
}

// The script boundary: a record for a script file says whether the text that runs it left exit alone, and the reader of the body
// starts from that (ScriptOptions.ExitTainted). A body read on its own with nothing known proves the form as before.
func TestCRW1165FixRound1TheScriptInheritsTheTaint(t *testing.T) {
	const body = "cd /outside || exit 1\nrm x\n"
	dirs := func(taint bool) []string {
		r, err := AnalyzeScriptProven(body, "/work", ScriptOptions{ExitTainted: taint})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range r.Execs {
			if e.Name == "rm" && e.Dir.Known {
				out = append(out, e.Dir.Path)
			}
		}
		return out
	}
	if got := dirs(false); strings.Join(got, "|") != "/outside" {
		t.Errorf("untainted: %q", got)
	}
	if got := dirs(true); strings.Join(got, "|") == "/outside" {
		t.Errorf("tainted: the directory %q was proven", got)
	}
	for _, tc := range []struct {
		text    string
		tainted bool
	}{
		{"bash ./x.sh", false},
		{"echo start; bash ./x.sh", false},
		{"cd /outside || exit 1; bash ./x.sh", false},
		{"trap : DEBUG; bash ./x.sh", true},
		{"set -e; bash ./x.sh", true},
		{"unset X; bash ./x.sh", true},
		{"eval ':'; bash ./x.sh", true},
		{"disable exit; bash ./x.sh", true},
		{"f() { :; }; bash ./x.sh", true},
		{"bash ./x.sh; set -e", true},
	} {
		r, err := AnalyzeScriptProven(tc.text, "/work", ScriptOptions{})
		if err != nil {
			t.Errorf("%q: %v", tc.text, err)
			continue
		}
		seen := false
		for _, e := range r.Execs {
			if e.Kind == KindScriptFile {
				seen = true
				if e.ExitTainted != tc.tainted {
					t.Errorf("%q: ExitTainted %v, want %v", tc.text, e.ExitTainted, tc.tainted)
				}
			}
		}
		if !seen {
			t.Errorf("%q: no script record", tc.text)
		}
	}
	// a taint is passed on by the body that is itself plain
	r, err := AnalyzeScriptProven("bash ./y.sh\n", "/work", ScriptOptions{ExitTainted: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range r.Execs {
		if e.Kind == KindScriptFile && !e.ExitTainted {
			t.Error("a tainted text handed a clean state to the script it runs")
		}
	}
}

// The startup variables the hook knows (its own environment) withdraw the proof for a reading that has no environment of its own.
func TestCRW1165FixRound1StartupLookupWithdrawsTheProof(t *testing.T) {
	const body = "cd /outside || exit 1\nrm x\n"
	for _, name := range []string{"BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS", "BASH_FUNC_exit%%", "BASH_FUNC_cd%%", "CDPATH"} {
		set := func(k string) (string, bool) {
			if k == name {
				return "x", true
			}
			return "", false
		}
		r, err := AnalyzeScriptProven(body, "/work", ScriptOptions{Startup: set})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range r.Execs {
			if e.Name == "rm" && e.Dir.Known && e.Dir.Path == "/outside" {
				t.Errorf("%s in the startup environment: the directory was proven", name)
			}
		}
	}
}
