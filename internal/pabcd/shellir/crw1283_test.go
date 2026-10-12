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
