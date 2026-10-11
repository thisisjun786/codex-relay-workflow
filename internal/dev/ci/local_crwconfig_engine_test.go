//go:build dev

package ci

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-958 fix round 3: the local engine executes the tool-root environment code of internal/crwconfig
// (LoadToolRoots, ToolRoots.Apply) from the caller's compiled source, so that package is engine source
// like internal/dev/ci: an edited, extra or deleted file of it refuses the run, and a binary built from
// a revision whose crwconfig differs from the verified commit's refuses too.
func localToolEnvRepo(t *testing.T) *localFixture {
	t.Helper()
	repo := localEngineRepo(t)
	repo.write("internal/crwconfig/toolenv.go", "package crwconfig\n")
	repo.commit()
	return repo
}

func TestLocal_an_edited_crwconfig_source_refuses_the_run(t *testing.T) {
	repo := localToolEnvRepo(t)
	repo.write("internal/crwconfig/toolenv.go", "package crwconfig\n// Apply is edited, not committed\n")
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "r.json")), "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "engine_differs_from_commit") {
		t.Fatalf("err = %v, result = %q; want an engine_differs_from_commit refusal", err, made.Result)
	}
}

func TestLocal_an_extra_crwconfig_source_refuses_the_run(t *testing.T) {
	repo := localToolEnvRepo(t)
	repo.write("internal/crwconfig/extra.go", "package crwconfig\n")
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "r.json")), "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "engine_differs_from_commit") {
		t.Fatalf("err = %v, result = %q; want an engine_differs_from_commit refusal", err, made.Result)
	}
}

func TestLocal_a_binary_built_from_another_crwconfig_revision_refuses_the_run(t *testing.T) {
	repo := localToolEnvRepo(t)
	old := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	repo.write("internal/crwconfig/toolenv.go", "package crwconfig\n// the next revision\n")
	repo.commit()
	result, err := verifyWithBuild(t, repo, stampedBuild(old, "false"))
	if err == nil || !strings.Contains(err.Error(), "engine_differs_from_commit") || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("err = %v, result = %q; want an engine_differs_from_commit refusal naming the binary", err, result)
	}
}

// Control: a commit that leaves crwconfig alone still runs.
func TestLocal_a_clean_crwconfig_source_runs(t *testing.T) {
	repo := localToolEnvRepo(t)
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "r.json")), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localPass {
		t.Errorf("result = %q, want %q", made.Result, localPass)
	}
}
