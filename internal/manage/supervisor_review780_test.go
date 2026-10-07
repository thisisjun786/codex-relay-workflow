package manage

import (
	"context"
	"strings"
	"testing"
)

// The two contract points the merged review of PR #780 raised against crw manage supervisor: a
// host-value option is refused before -h/--help is honored, and a failed write of the help output
// is reported rather than swallowed. Every name here carries the supervisorReview780 prefix so it
// cannot collide with a sibling issue's file in this package.

// supervisorReview780RunWithFailStdout runs one supervisor command line through the registry with a
// stdout that refuses every write, so a run whose usage never left the writer is distinguishable
// from one that wrote it.
func supervisorReview780RunWithFailStdout(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var errOut strings.Builder
	code := Run(context.Background(), args, strings.NewReader(""), coreFailWriter{}, &errOut)
	return code, errOut.String()
}

// C1: a host-value option is refused before -h/--help is honored. The refusal is the one the
// command already gives without help (exit 2 and the host-value note), and no relay call is made,
// so a command line that tries to override a host value cannot end as a successful help request.
func TestSupervisorReview780RemovedOptionBeatsHelp(t *testing.T) {
	for _, args := range [][]string{
		{"register", "--cwd=/override", "--help"},
		{"register", "--settings-file=/x", "-h"},
		{"register", "--cwd", "/override", "--help"},
		{"register", "--settings-file", "/x", "-h"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			record := supervisorFakeProcess(t, 0)
			section := supervisorTestSection()
			supervisorUseConfig(t, supervisorTestConfig(&section))
			code, _, errOut := supervisorRunLine(t, append([]string{"supervisor"}, args...)...)
			if code != usageExit {
				t.Fatalf("exit %d, want %d (stderr %q)", code, usageExit, errOut)
			}
			if !strings.Contains(errOut, supervisorHostValuesMessage) {
				t.Errorf("stderr %q does not name %q", errOut, supervisorHostValuesMessage)
			}
			if calls := supervisorRecordedCalls(t, record); len(calls) != 0 {
				t.Errorf("the relay was called %q", calls)
			}
		})
	}
}

// C2: a failed write of the help output is reported on stderr and the run ends with exit 1. The
// usage never reached the caller, so a help request that wrote nothing must not read as success.
func TestSupervisorReview780HelpWriteFailureExitsOne(t *testing.T) {
	for _, args := range [][]string{
		{"supervisor", "register", "--help"},
		{"supervisor", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, errOut := supervisorReview780RunWithFailStdout(t, args...)
			if code != 1 {
				t.Fatalf("exit %d, want 1 (stderr %q)", code, errOut)
			}
			if !strings.Contains(errOut, "crw manage supervisor: error: write the usage:") {
				t.Errorf("stderr %q does not carry the usage write failure", errOut)
			}
		})
	}
}

// The contrast: a plain --help with no host-value option is a successful help request (exit 0) that
// prints the usage on stdout and calls no relay. Without it the two rules above could be satisfied
// by refusing every help request.
func TestSupervisorReview780PlainHelpStaysZero(t *testing.T) {
	for _, args := range [][]string{
		{"supervisor", "--help"},
		{"supervisor", "register", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			record := supervisorFakeProcess(t, 0)
			section := supervisorTestSection()
			supervisorUseConfig(t, supervisorTestConfig(&section))
			code, out, errOut := supervisorRunLine(t, args...)
			if code != 0 {
				t.Fatalf("exit %d, want 0 (stderr %q)", code, errOut)
			}
			if !strings.Contains(out, "usage: crw manage supervisor") {
				t.Errorf("stdout %q does not carry the usage", out)
			}
			if calls := supervisorRecordedCalls(t, record); len(calls) != 0 {
				t.Errorf("a help request called the relay %q", calls)
			}
		})
	}
}
