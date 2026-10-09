//go:build dev

package ci

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1025: the engine the caller runs is the verified commit's. The plan a run executes is compiled
// from the caller's engine source (internal/dev/ci, cmd/crw-dev, Makefile), so an engine file that
// differs from the commit, tracked or untracked, would let a run record a clean commit's pass for
// steps the commit does not define. Such a run is refused before any step runs or any record is reused.

// localEngineRepo is a fixture repository whose HEAD carries one engine file.
func localEngineRepo(t *testing.T) *localFixture {
	t.Helper()
	repo := newLocalFixture(t)
	repo.write("internal/dev/ci/engine.go", "package ci\n")
	repo.commit()
	return repo
}

// A tracked engine file edited in the caller's tree refuses the run: the plan it would execute is
// not the one the commit's engine produces.
func TestLocal_a_dirty_tracked_engine_source_refuses_the_run(t *testing.T) {
	repo := localEngineRepo(t)
	repo.write("internal/dev/ci/engine.go", "package ci\n// edited, not committed\n")
	record := filepath.Join(t.TempDir(), "r.json")
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), record), "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "engine_differs_from_commit") {
		t.Fatalf("err = %v, result = %q; want an engine_differs_from_commit refusal", err, made.Result)
	}
}

// An untracked engine file in the caller's tree refuses the run the same way.
func TestLocal_an_untracked_engine_source_refuses_the_run(t *testing.T) {
	repo := localEngineRepo(t)
	repo.write("internal/dev/ci/extra_engine.go", "package ci\n")
	record := filepath.Join(t.TempDir(), "r.json")
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), record), "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "engine_differs_from_commit") {
		t.Fatalf("err = %v, result = %q; want an engine_differs_from_commit refusal", err, made.Result)
	}
}

// A clean engine tree runs as before: the check refuses only a difference from the commit.
func TestLocal_a_clean_engine_source_runs(t *testing.T) {
	repo := localEngineRepo(t)
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "r.json")), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localPass {
		t.Errorf("result = %q, want %q", made.Result, localPass)
	}
}
