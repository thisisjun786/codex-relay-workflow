package shellir

import (
	"strings"
	"testing"
)

func TestCRW1283UnknownPathDoesNotRecoverTheOldEnvironment(t *testing.T) {
	lookup := func(name string) (string, bool) { return "/usr/bin:/bin", name == "PATH" }
	for _, command := range []string{
		"PATH=$UNKNOWN; PATH=$PATH; cat /dev/null",
		"unset PATH; PATH=$PATH; cat /dev/null",
		"export \"PATH=$UNKNOWN\"; cat /dev/null",
	} {
		if _, err := AnalyzeEnv(command, "/work", lookup); err == nil {
			t.Errorf("unknown PATH was replaced with the old environment: %s", command)
		}
	}
}

// TestCRW1283LoopArgumentsFollowPositionalChangesOnly: plan C (Sion 10-13) withdrew the reading of a script's forwarded "$@", so a
// loop that forwards it is refused whether it only assigns variables (the shape of heavy_slot.sh, refused as a named limit; trusted
// wrappers are CRW-1290) or can change the positional parameters, and the refusal names the path the user can follow.
func TestCRW1283LoopArgumentsFollowPositionalChangesOnly(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	for _, body := range []string{
		"while :; do x=1; nice \"$@\"; rc=$?; exit $rc; done",
		"for i in 1 2; do start=$(date +%s); ionice -c3 nice \"$@\"; done",
		"until false; do X=1 env \"$@\"; n=$((n + 1)); cd /tmp; break; done",
	} {
		_, err := AnalyzeScriptBody(body, "/work", false, lookup, nil)
		if err == nil || !strings.Contains(err.Error(), "run the forwarded command directly") {
			t.Errorf("a loop that forwards \"$@\" was not refused with the followable path: %s: %v", body, err)
		}
	}
	for _, body := range []string{
		"while :; do nice \"$@\"; 1=env; done",
		"while :; do nice \"$@\"; 2+=x; done",
		"while :; do x=1; nice \"$@\"; if :; then 1=env; fi; done",
		"f() { 1=env; }; while :; do nice \"$@\"; f; done",
		"while :; do nice \"$@\"; argv=(env gh); done",
		"while :; do nice \"$@\"; shift; done",
		"while :; do nice \"$@\"; for 1 in env; do :; done; done",
	} {
		if _, err := AnalyzeScriptBody(body, "/work", false, lookup, nil); err == nil {
			t.Errorf("a loop that changes the positional parameters kept the arguments: %s", body)
		}
	}
}

// TestCRW1283ZshParameterBuiltinsEndTheArguments: a forwarded "$@" is not followed after a zsh builtin that assigns or removes
// positional parameters (print -v 1, zformat, zparseopts, getln, vared), straight-line and in a loop, and since plan C not after a
// print without -v either.
func TestCRW1283ZshParameterBuiltinsEndTheArguments(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	for _, body := range []string{
		"print hello; nice \"$@\"",
		"print -r -- 'a b'; nice \"$@\"",
		"while :; do print -n x; nice \"$@\"; break; done",
		"for i in 1 2; do nice \"$@\"; print done; done",
	} {
		if _, err := AnalyzeScriptBody(body, "/work", false, lookup, nil); err == nil {
			t.Errorf("a print without -v forwarded \"$@\": %s", body)
		}
	}
	for _, body := range []string{
		"print -v 1 env; nice \"$@\"",
		"print -rv 1 env; nice \"$@\"",
		"print -v argv env; nice \"$@\"",
		"print $O env; nice \"$@\"",
		"zformat -f 1 '%a' a:env; nice \"$@\"",
		"zformat -a argv : env:x; nice \"$@\"",
		"zparseopts -D n:=o; nice \"$@\"",
		"zparseopts -a argv n:=o; nice \"$@\"",
		"getln 1; nice \"$@\"",
		"vared 1; nice \"$@\"",
		"command print -v 1 env; nice \"$@\"",
		"noglob shift; nice \"$@\"",
		"echo env | read 1; nice \"$@\"",
		"true | shift; nice \"$@\"",
		"while :; do nice \"$@\"; echo env | read 1; done",
		"while :; do nice \"$@\"; print -v 1 env; done",
		"while :; do nice \"$@\"; print -rv argv env; done",
		"while :; do x=1; nice \"$@\"; zformat -f 1 '%a' a:env; done",
		"until false; do nice \"$@\"; zparseopts -D n:=o; done",
		"for i in 1 2; do nice \"$@\"; getln 1; done",
		"for i in 1 2; do nice \"$@\"; vared 1; done",
		"f() { print -v 1 env; }; while :; do nice \"$@\"; f; done",
	} {
		if _, err := AnalyzeScriptBody(body, "/work", false, lookup, nil); err == nil {
			t.Errorf("a zsh builtin that assigns a positional parameter kept the arguments: %s", body)
		}
	}
}
