//go:build dev

package ci

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-964 parent ruling 2, d3: --reuse never trusts a stored pin mismatch. A record whose mismatch
// was cleared and whose digest was recomputed is refused, because the engine recomputes the pins
// from the verified commit and the observed versions.
func TestLocalReuse_a_record_whose_pin_mismatch_was_cleared_is_refused(t *testing.T) {
	repo := newLocalFixture(t)
	repo.write(".github/workflows/ci.yml", strings.Replace(localFixtureWorkflow, "'24.20.0'", "'0.0.1'", 1))
	repo.commit()
	options := func(record string) localOptions {
		return localRunOptions(repo, localFixturePlan("echo hello"), record)
	}
	record := filepath.Join(t.TempDir(), "record.json")
	made, _, err := localVerify(options(record), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(made.PinMismatch) == 0 {
		t.Fatal("the fixture's Node pin 0.0.1 is not named as a mismatch")
	}
	made.PinMismatch = nil
	if _, err := writeRecord(record, made); err != nil {
		t.Fatal(err)
	}
	_, reused, err := localVerify(options(filepath.Join(t.TempDir(), "again.json")), record, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Error("a record whose stored pin mismatch was cleared is reused")
	}
}
