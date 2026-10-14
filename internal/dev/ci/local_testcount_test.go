//go:build dev

package ci

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// CRW-1328: go test exits 0 when no test ran (a -run pattern that matches nothing, a package whose tests a build tag leaves out), so a
// passing record says nothing of how much was tested. The record counts the go test packages its steps ran tests in, from the result
// lines go test prints ("ok <package> <time>"; "[no tests to run]" and "[no test files]" ran none). go test prints no per-test count
// without -v, so the count is of packages; 0 is still "no test ran". The member is additive: a record of a run whose steps printed no
// go test result (and every record written before it) has none, which a reader takes as unknown, never as zero.

// TestVerificationRecordCountsTheTestPackagesThatRan (CRW-1328): the record carries testPackagesRun, the packages whose tests ran in
// its steps; a run whose go test ran no test records 0; a run with no go test output records no member, and a record without it
// reads back with the count unknown and the same digest.
func TestVerificationRecordCountsTheTestPackagesThatRan(t *testing.T) {
	repo := newLocalFixture(t)
	run := func(command string) (verificationRecord, map[string]any) {
		t.Helper()
		record := filepath.Join(t.TempDir(), "record.json")
		made, _, err := localVerify(localRunOptions(repo, localFixturePlan(command), record), "", io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writeRecord(record, made); err != nil {
			t.Fatal(err)
		}
		back, err := readRecord(record)
		if err != nil {
			t.Fatalf("the record does not read back: %v", err)
		}
		raw, _ := os.ReadFile(record)
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		return back, fields
	}
	ran := "printf 'ok  \\tfixture/a\\t0.012s\\nok  \\tfixture/b\\t(cached)\\nok  \\tfixture/c\\t0.010s [no tests to run]\\n?   \\tfixture/d\\t[no test files]\\n'"
	rec, fields := run(ran)
	if rec.Result != localPass || rec.TestPackagesRun == nil || *rec.TestPackagesRun != 2 || fields["testPackagesRun"] != float64(2) {
		t.Errorf("two packages ran tests: %+v %v", rec.TestPackagesRun, fields["testPackagesRun"])
	}
	none := "printf 'ok  \\tfixture/a\\t0.010s [no tests to run]\\n?   \\tfixture/d\\t[no test files]\\n'"
	if rec, fields := run(none); rec.Result != localPass || rec.TestPackagesRun == nil || *rec.TestPackagesRun != 0 || fields["testPackagesRun"] != float64(0) {
		t.Errorf("no test ran, and the run still passes: %+v %v (result %s)", rec.TestPackagesRun, fields["testPackagesRun"], rec.Result)
	}
	if rec, fields := run("echo hello"); rec.TestPackagesRun != nil {
		t.Errorf("a run with no go test output counts %d", *rec.TestPackagesRun)
	} else if _, has := fields["testPackagesRun"]; has {
		t.Error("a run with no go test output records the member")
	}
	// a failing package is a package whose tests ran
	if got, saw := localTestPackagesRun("--- FAIL: TestX (0.00s)\nFAIL\nFAIL\tfixture/a\t0.020s\nok  \tfixture/b\t0.1s\nFAIL\tfixture/c [build failed]\n"); !saw || got != 2 {
		t.Errorf("a failed package and a passed one: %d %v", got, saw)
	}
}
