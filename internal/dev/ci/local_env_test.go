package ci

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// A step follows the caller's TMPDIR, as the heavy-check rule requires: the isolated environment
// does not drop it, so a test that makes a socket or a temporary file never falls back to /tmp.
func TestLocal_a_step_follows_the_callers_TMPDIR(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	repo := newLocalFixture(t)
	record := filepath.Join(t.TempDir(), "record.json")
	command := fmt.Sprintf(`test "$TMPDIR" = %q`, dir)
	opts := localRunOptions(repo, localFixturePlan(command), record)
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if step := made.Jobs[0].Steps[1]; step.Result != localPassed {
		t.Errorf("the step is %q (%s), want it to see the caller's TMPDIR", step.Result, step.Reason)
	}
}

// A failing test that is not among the last lines of a long output still reaches the reason.
func TestLocal_a_failed_test_before_a_long_tail_still_reaches_the_reason(t *testing.T) {
	repo := newLocalFixture(t)
	record := filepath.Join(t.TempDir(), "record.json")
	command := `echo "--- FAIL: TestEarly"; i=0; while [ $i -lt 80 ]; do echo "ok line $i"; i=$((i+1)); done; echo "make: *** Error 1"; exit 1`
	opts := localRunOptions(repo, localFixturePlan(command), record)
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if reason := made.Jobs[0].Steps[1].Reason; !strings.Contains(reason, "--- FAIL: TestEarly") {
		t.Errorf("the reason %q does not carry the failing test", reason)
	}
}
