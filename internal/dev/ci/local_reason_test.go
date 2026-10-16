//go:build dev

package ci

import (
	"fmt"
	"io"
	"os"
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

// localReasonHead is the marked block a reason carries before its tail, split into lines.
func localReasonHead(t *testing.T, reason string) []string {
	t.Helper()
	head, _, found := strings.Cut(strings.TrimPrefix(reason, ": "), "\n...\n")
	if !found {
		t.Fatalf("the reason %q has no marked block", reason)
	}
	return strings.Split(head, "\n")
}

// localFiller is n lines of passing output, enough to push a failure out of the reason's tail.
func localFiller(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "ok  \tgithub.com/example/pkg%d\t0.01%ds\n", i, i%10)
	}
	return b.String()
}

// A failing test's message lines say why it failed. When more than the tail's lines follow, the
// reason keeps each --- FAIL line with its more-indented message lines, not the --- FAIL line alone.
func TestLocalReason_keeps_the_message_lines_of_a_failing_test(t *testing.T) {
	t.Run("captured real output", func(t *testing.T) {
		// The go test output of TestConfiguredRelayState under a 38-byte TMPDIR (CRW-1568's repro),
		// followed by more than localReasonLines lines. The message comes before the --- FAIL line
		// there, as go test -v prints it.
		data, err := os.ReadFile(filepath.Join("testdata", "configured_relay_state_fail.txt"))
		if err != nil {
			t.Fatal(err)
		}
		reason := localReason(string(data))
		head := strings.Join(localReasonHead(t, reason), "\n")
		for _, want := range []string{
			"--- FAIL: TestConfiguredRelayState (0.00s)",
			"    toolenv_test.go:262: configured: \"\", relay_state: socket_path_too_long: a store socket would be 118 bytes",
		} {
			if !strings.Contains(head, want) {
				t.Errorf("the marked block %q does not keep %q", head, want)
			}
		}
		if strings.Contains(head, "=== RUN") || strings.Contains(head, "--- PASS") {
			t.Errorf("the marked block %q keeps lines that are not a failure's", head)
		}
	})

	t.Run("nested shape", func(t *testing.T) {
		output := "--- FAIL: TestX (0.00s)\n" +
			"    x_test.go:10: first message\n" +
			"    x_test.go:11: second message\n" +
			"    --- FAIL: TestX/sub (0.00s)\n" +
			"        x_test.go:20: sub message\n" +
			localFiller(localReasonLines+5)
		head := localReasonHead(t, localReason(output))
		want := []string{
			"--- FAIL: TestX (0.00s)",
			"    x_test.go:10: first message",
			"    x_test.go:11: second message",
			"    --- FAIL: TestX/sub (0.00s)",
			"        x_test.go:20: sub message",
		}
		if strings.Join(head, "\n") != strings.Join(want, "\n") {
			t.Errorf("the marked block is\n%s\nwant\n%s", strings.Join(head, "\n"), strings.Join(want, "\n"))
		}
	})

	t.Run("at most 6 messages per FAIL line", func(t *testing.T) {
		const messages = 6
		output := "--- FAIL: TestMany (0.00s)\n"
		for i := 1; i <= messages+1; i++ {
			output += fmt.Sprintf("    many_test.go:%d: message %d\n", i, i)
		}
		output += localFiller(localReasonLines + 5)
		head := localReasonHead(t, localReason(output))
		if len(head) != 1+messages {
			t.Errorf("the marked block has %d lines %q, want the FAIL line and %d messages", len(head), head, messages)
		}
		if strings.Contains(strings.Join(head, "\n"), fmt.Sprintf("message %d", messages+1)) {
			t.Errorf("the marked block %q keeps a message past the bound", head)
		}
	})

	t.Run("each message cut to 300 bytes", func(t *testing.T) {
		long := "    long_test.go:1: " + strings.Repeat("é", localReasonLine)
		output := "--- FAIL: TestLong (0.00s)\n" + long + "\n" + localFiller(localReasonLines+5)
		head := localReasonHead(t, localReason(output))
		if len(head) != 2 {
			t.Fatalf("the marked block %q, want the FAIL line and its message", head)
		}
		if len(head[1]) > localReasonLine || !strings.HasPrefix(long, head[1]) {
			t.Errorf("the message is %d bytes %q, want the first %d bytes at most", len(head[1]), head[1], localReasonLine)
		}
	})

	t.Run("at most 20 marked lines in total", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 30; i++ {
			fmt.Fprintf(&b, "--- FAIL: Test%d (0.00s)\n    t_test.go:%d: message of %d\n", i, i, i)
		}
		head := localReasonHead(t, localReason(b.String()+localFiller(localReasonLines+5)))
		if len(head) != localReasonMarked {
			t.Errorf("the marked block has %d lines, want %d", len(head), localReasonMarked)
		}
		if head[0] != "--- FAIL: Test0 (0.00s)" || head[1] != "    t_test.go:0: message of 0" {
			t.Errorf("the marked block starts %q, want the first failure with its message", head[:2])
		}
	})

	t.Run("unindented stdout is not a message", func(t *testing.T) {
		output := "--- FAIL: TestOut (0.00s)\n" +
			"    out_test.go:3: the message\n" +
			"stdout the test printed\n" +
			"    an indented line after the stdout\n" +
			localFiller(localReasonLines+5)
		head := localReasonHead(t, localReason(output))
		want := "--- FAIL: TestOut (0.00s)\n    out_test.go:3: the message"
		if strings.Join(head, "\n") != want {
			t.Errorf("the marked block is %q, want %q", strings.Join(head, "\n"), want)
		}
	})
}
