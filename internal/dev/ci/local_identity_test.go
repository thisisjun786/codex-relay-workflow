//go:build dev

package ci

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-964 pre-merge finding d2: a record made for one repository is never reused for another. The
// repository a record names is one of the reuse keys.
func TestLocalReuse_a_record_of_another_repository_is_never_reused(t *testing.T) {
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
	options := func(path string) localOptions {
		return localRunOptions(repo, localFixturePlan("echo hello"), path)
	}
	made, _, err := localVerify(options(record), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeRecord(record, made); err != nil {
		t.Fatal(err)
	}
	if _, reused, err := localVerify(options(filepath.Join(t.TempDir(), "same.json")), record, io.Discard); err != nil || !reused {
		t.Fatalf("the unchanged record is not reusable (reused=%t, err=%v)", reused, err)
	}
	made.Repository = "https://example.invalid/other-origin.git"
	if _, err := writeRecord(record, made); err != nil {
		t.Fatal(err)
	}
	if _, reused, err := localVerify(options(filepath.Join(t.TempDir(), "other.json")), record, io.Discard); err != nil || reused {
		t.Errorf("a record of another repository is reused (reused=%t, err=%v)", reused, err)
	}
}
