//go:build dev

package cxcfuzz

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A worker that stalls at its start-up handshake times out every case. Each case's failing input is written as a
// record under the output directory, and the summary names the records, so a timed-out run can be replayed
// (CRW-978 c7, CRW-972 item 4). Red before the records were written: the directory held summary.json alone.
func TestCampaignRecordsEveryTimedOutCase(t *testing.T) {
	requireNode(t)
	out := t.TempDir()
	summary, err := Campaign(Config{Target: echoTarget(), Cases: 3, Seed: 9, Workers: 1, Out: out,
		Timeout: 200 * time.Millisecond, StartupTimeout: 500 * time.Millisecond, Env: append(os.Environ(), "CXCFUZZ_STALL=1")})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Timeouts != 3 {
		t.Fatalf("summary timeouts %d, want 3", summary.Timeouts)
	}
	if len(summary.Failures) != 3 {
		t.Fatalf("summary names %v, want three case records", summary.Failures)
	}
	for _, name := range summary.Failures {
		raw, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatalf("the record %s is not written: %v", name, err)
		}
		var record Failure
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		if record.Cause != CauseTimeout || record.Case < 1 || record.Input == "" || !strings.HasPrefix(name, "failures/timeout-") {
			t.Fatalf("the record %s holds %+v, want a timeout with its case number and input", name, record)
		}
	}
}

// Control: a clean run writes no failure records and names none in its summary.
func TestCleanCampaignWritesNoFailureRecords(t *testing.T) {
	requireNode(t)
	out := t.TempDir()
	summary, err := Campaign(Config{Target: echoTarget(), Cases: 5, Seed: 4, Workers: 1, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Failures) != 0 {
		t.Fatalf("a clean run names failure records: %v", summary.Failures)
	}
	if _, err := os.Stat(filepath.Join(out, "failures")); !os.IsNotExist(err) {
		t.Fatalf("a clean run created the failures directory (stat: %v)", err)
	}
}
