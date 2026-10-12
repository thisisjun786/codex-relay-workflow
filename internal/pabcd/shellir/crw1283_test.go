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
