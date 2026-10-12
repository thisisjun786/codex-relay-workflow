package shellir

import "testing"

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

// TestCRW1283LoopArgumentsFollowPositionalChangesOnly: a loop that assigns variables forwards the same "$@" in every iteration (the
// shape of heavy_slot.sh), one that can change the positional parameters (a zsh positional assignment word among them) does not.
func TestCRW1283LoopArgumentsFollowPositionalChangesOnly(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	args := []Word{{Known: true, Value: "go"}, {Known: true, Value: "version"}}
	for _, body := range []string{
		"while :; do x=1; nice \"$@\"; rc=$?; exit $rc; done",
		"for i in 1 2; do start=$(date +%s); ionice -c3 nice \"$@\"; done",
		"until false; do X=1 env \"$@\"; n=$((n + 1)); cd /tmp; break; done",
	} {
		if _, err := AnalyzeScriptEnvArgs(body, "/work", false, lookup, args, nil); err != nil {
			t.Errorf("a loop that assigns variables lost the arguments: %s: %v", body, err)
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
		if _, err := AnalyzeScriptEnvArgs(body, "/work", false, lookup, args, nil); err == nil {
			t.Errorf("a loop that changes the positional parameters kept the arguments: %s", body)
		}
	}
}

// TestCRW1283ZshParameterBuiltinsEndTheArguments: a zsh builtin that assigns or removes positional parameters (print -v 1, zformat,
// zparseopts, getln, vared) ends the arguments the reader follows, straight-line and in a loop; print without -v does not.
func TestCRW1283ZshParameterBuiltinsEndTheArguments(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	args := []Word{{Known: true, Value: "go"}, {Known: true, Value: "version"}}
	for _, body := range []string{
		"print hello; nice \"$@\"",
		"print -r -- 'a b'; nice \"$@\"",
		"while :; do print -n x; nice \"$@\"; break; done",
		"for i in 1 2; do nice \"$@\"; print done; done",
	} {
		if _, err := AnalyzeScriptEnvArgs(body, "/work", false, lookup, args, nil); err != nil {
			t.Errorf("a print without -v lost the arguments: %s: %v", body, err)
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
		if _, err := AnalyzeScriptEnvArgs(body, "/work", false, lookup, args, nil); err == nil {
			t.Errorf("a zsh builtin that assigns a positional parameter kept the arguments: %s", body)
		}
	}
}
