//go:build dev

package cxcfuzz

import (
	"strings"
	"testing"
)

// A campaign that lost cases to the harness -- a timeout, a worker that died (a reused json.tool process included), a
// reply that answered nothing, a failure of the case's own machinery -- says so on stderr, names the counts and where
// the records are, and fails; one that lost none says nothing (CRW-1066 c5: warn and record, never a silent pass).
func TestFailureWarningNamesTheUnansweredCasesAndTheirRecords(t *testing.T) {
	if got := failureWarning(Summary{Cases: 10, Same: 10}, "/out"); got != "" {
		t.Fatalf("a clean campaign warned: %q", got)
	}
	got := failureWarning(Summary{Cases: 10, Same: 4, Timeouts: 2, DeadWorkers: 3, NoAnswers: 1}, "/out")
	for _, want := range []string{"warning", "6 cases", "timeout 2", "dead worker 3", "no answer 1", "error 0", "/out/failures", "the campaign fails"} {
		if !strings.Contains(got, want) {
			t.Errorf("the warning %q does not hold %q", got, want)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("the warning is more than one line: %q", got)
	}
}
