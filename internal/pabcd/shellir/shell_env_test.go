package shellir

import (
	"fmt"
	"testing"
)

// TestShellNameIsCodeBearing: flock -c, script -c, watch without -x and entr -s run $SHELL -c, so an assignment to SHELL
// (as a prefix, under env, or earlier in the text) picks the program that runs the text. The text is then unreadable.
func TestShellNameIsCodeBearing(t *testing.T) {
	wrappers := []string{
		"flock /tmp/l -c 'echo hi'",
		"script -qec 'echo hi' /dev/null",
		"watch 'echo hi'",
		"entr -s 'echo hi'",
	}
	forms := []string{
		"SHELL=/tmp/evil %s",
		"env SHELL=/tmp/evil %s",
		"SHELL=/tmp/evil; %s",
	}
	for _, w := range wrappers {
		if _, err := AnalyzeEnv(w, "/work", nil); err != nil {
			t.Errorf("control %q: unreadable: %v", w, err)
		}
		for _, f := range forms {
			cmd := fmt.Sprintf(f, w)
			if _, err := AnalyzeEnv(cmd, "/work", nil); err == nil {
				t.Errorf("%q: read, want unreadable (SHELL names the executor)", cmd)
			}
		}
	}
}
