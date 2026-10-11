package shellir

import "testing"

// CRW-1165 / CRW-1183 / CRW-1187 pre-merge evaluation of 9201b469: a startup variable named through a name the text builds from known
// parts (printf -v "$N$M" with N=BASH_ and M=ENV), a name the text spells in a form that is not a plain assignment (${BASH_ENV:=x},
// for BASH_ENV in, read BASH_ENV) and ZDOTDIR in the environment each reach the shell that runs a script, whose exit may then be a
// function of a file: the script is not given the proof of `cd <literal> || exit <digits>`.

func crw1165Fix3Tainted(t *testing.T, text string, o ScriptOptions) {
	t.Helper()
	r, err := AnalyzeScriptProven(text, "/work", o)
	if err != nil {
		return // refusing the text is as good
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

func TestCRW1165FixRound3StartupVariablesNamedFromKnownPartsTaintTheScripts(t *testing.T) {
	for _, text := range []string{
		// a name built from known parts (nothing in the text spells it)
		"N=BASH_; M=ENV; printf -v \"$N$M\" '%s' /x/env.sh; bash ./cleanup.sh",
		"N=BASH_; N+=ENV; printf -v \"$N\" '%s' /x/env.sh; bash ./cleanup.sh",
		"N=ENV; printf -v \"$N\" '%s' /x/env.sh; sh ./cleanup.sh",
		"N=ZDOTDIR; printf -v \"$N\" '%s' /x; zsh ./cleanup.sh",
		"N=SHELLOPTS; printf -v \"$N\" '%s' posix; bash ./cleanup.sh",
		"N=BASHOPTS; printf -v \"$N\" '%s' x; bash ./cleanup.sh",
		"N='BASH_FUNC_exit%%'; printf -v \"$N\" '%s' '() { :; }'; bash ./cleanup.sh",
		"N=BASH_ENV; export \"$N=/x/env.sh\"; bash ./cleanup.sh",
		"N=BASH_ENV; declare -x \"$N=/x/env.sh\"; bash ./cleanup.sh",
		"N=BASH_ENV; export \"$N\"; bash ./cleanup.sh",
		"N=BASH_ENV; read \"$N\" <<< /x/env.sh; bash ./cleanup.sh",
		"N=BASH_ENV; mapfile \"$N\" < /x/list; bash ./cleanup.sh",
		"N=BASH_ENV; declare -n r=$N; r=/x/env.sh; bash ./cleanup.sh",
		"N=BASH_ENV[0]; printf -v \"$N\" '%s' /x/env.sh; bash ./cleanup.sh",
		// the name spelled, in a form that is not a plain assignment
		": ${BASH_ENV:=/x/env.sh}; bash ./cleanup.sh",
		"for BASH_ENV in /x/env.sh; do :; done; bash ./cleanup.sh",
		"read BASH_ENV <<< /x/env.sh; bash ./cleanup.sh",
		"printf -v BASH_ENV '%s' /x/env.sh; bash ./cleanup.sh",
		"(( ZDOTDIR = 5 )); zsh ./cleanup.sh",
		"getopts a ENV; bash ./cleanup.sh",
		"bash ./cleanup.sh; read ZDOTDIR <<< x",
		"bash -c 'read BASH_ENV <<< /x/env.sh'; bash ./cleanup.sh",
	} {
		for _, o := range []ScriptOptions{{}, {Startup: func(string) (string, bool) { return "", false }}} {
			crw1165Fix3Tainted(t, text, o)
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
	// the same text with a name that is no startup variable keeps the proof for the script
	for _, text := range []string{
		"N=OUT; printf -v \"$N\" '%s' x; bash ./cleanup.sh",
		"N=BASH_; M=ENVELOPE; printf -v \"$N$M\" '%s' x; bash ./cleanup.sh",
		"export SOMEENV=1; bash ./cleanup.sh",
		"echo ENVIRONMENT; bash ./cleanup.sh",
	} {
		r, err := AnalyzeScriptProven(text, "/work", ScriptOptions{})
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		for _, e := range r.Execs {
			if e.Kind == KindScriptFile && e.ExitTainted {
				t.Errorf("%q: the script record is tainted", text)
			}
		}
	}
}

// The text that names a startup variable is read again without the proof, as a text that builds names it does not spell is.
func TestCRW1165FixRound3AProofUsedBeforeANamedStartupVariableIsWithdrawn(t *testing.T) {
	for _, text := range []string{
		"cd /outside || exit 1; N=BASH_; N+=ENV; printf -v \"$N\" '%s' /x/env.sh; rm x",
		"cd /outside || exit 1; : ${BASH_ENV:=/x/env.sh}; rm x",
	} {
		got := crw1165Dirs(t, text, nil, false)
		if len(got) != 1 || got[0] == "/outside" {
			t.Errorf("%q: the directory %q was proven", text, got)
		}
	}
}

// ZDOTDIR names the directory of the .zshenv that a zsh reads before a script: a function named exit may stand in it.
func TestCRW1165FixRound3ZdotdirInTheEnvironmentWithdrawsTheProof(t *testing.T) {
	const command = "cd /outside || exit 1; rm x"
	set := func(k string) (string, bool) { return "/z", k == "ZDOTDIR" }
	if got := crw1165Dirs(t, command, set, false); len(got) != 1 || got[0] == "/outside" {
		t.Errorf("ZDOTDIR in the environment: directory %q was proven", got)
	}
	r, err := AnalyzeScriptProven("cd /outside || exit 1\nrm x\n", "/work", ScriptOptions{Startup: set})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range r.Execs {
		if e.Name == "rm" && e.Dir.Known && e.Dir.Path == "/outside" {
			t.Error("ZDOTDIR in the startup environment: the directory of a script was proven")
		}
	}
	empty := func(k string) (string, bool) { return "", k == "ZDOTDIR" }
	if got := crw1165Dirs(t, command, empty, false); len(got) != 1 || got[0] != "/outside" {
		t.Errorf("an empty ZDOTDIR: %q", got)
	}
}
