//go:build dev

package ci

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// C3: a record that matches every key answers the run, and a changed key runs again. The fixture's
// Node pin is the host's Node, so the record carries no pin mismatch and is eligible for reuse.
func TestLocalReuse_an_answered_record_is_reused_and_a_changed_tree_reruns(t *testing.T) {
	repo := newLocalFixture(t)
	node := localToolVersions(localPathEnv(nil))["node"]
	if node == "" {
		t.Skip("no node on PATH, so the fixture's Node pin cannot match this host")
	}
	if workflow := strings.Replace(localFixtureWorkflow, "'24.20.0'", "'"+node+"'", 1); workflow != localFixtureWorkflow {
		repo.write(".github/workflows/ci.yml", workflow)
		repo.commit()
	}
	record := filepath.Join(t.TempDir(), "record.json")
	first, reused, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), record), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Fatal("a run with no record reports reuse")
	}
	if _, err := writeRecord(record, first); err != nil {
		t.Fatal(err)
	}
	if len(first.PinMismatch) != 0 {
		t.Fatalf("the fixture's pins do not match this host: %v", first.PinMismatch)
	}
	_, reused, err = localVerify(localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "again.json")), record, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !reused {
		t.Error("a record that matches every key is not reused")
	}
	repo.write("file.txt", "two\n")
	repo.commit()
	_, reused, err = localVerify(localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "third.json")), record, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Error("a changed tree is reused")
	}
}
