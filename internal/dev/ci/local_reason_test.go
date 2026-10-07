//go:build dev

package ci

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// A failed step's reason carries the end of its output, so the record shows which test failed,
// and the reason stays bounded however long the output is.
func TestLocal_a_failed_step_reason_carries_the_failing_output(t *testing.T) {
	repo := newLocalFixture(t)
	record := filepath.Join(t.TempDir(), "record.json")
	command := `i=0; while [ $i -lt 400 ]; do echo "filler line $i"; i=$((i+1)); done; echo "--- FAIL: TestMarker"; echo "FAIL example"; echo "make: *** [Makefile:1] Error 1"; exit 1`
	opts := localRunOptions(repo, localFixturePlan(command), record)
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	reason := made.Jobs[0].Steps[1].Reason
	if !strings.Contains(reason, "--- FAIL: TestMarker") {
		t.Errorf("the reason %q does not carry the failing line", reason)
	}
	if len(reason) > 8192 {
		t.Errorf("the reason is %d bytes, want it bounded", len(reason))
	}
}

// A Go test timeout is named as a timeout, not as an unexplained exit status.
func TestLocal_a_test_timeout_is_named_in_the_reason(t *testing.T) {
	repo := newLocalFixture(t)
	record := filepath.Join(t.TempDir(), "record.json")
	command := `echo "panic: test timed out after 10m0s"; exit 2`
	opts := localRunOptions(repo, localFixturePlan(command), record)
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	reason := made.Jobs[0].Steps[1].Reason
	if !strings.HasPrefix(reason, "timeout") {
		t.Errorf("the reason %q does not name the timeout", reason)
	}
}
