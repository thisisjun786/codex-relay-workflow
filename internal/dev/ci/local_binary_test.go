//go:build dev

package ci

import (
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// CRW-1025 (pre-merge evaluation of 71490802, D1): a clean checkout does not prove which engine runs. A
// prebuilt crw-dev carries the plan and the executor it was compiled from, so a binary built from another
// revision, or from a modified tree, would record a pass for the verified commit with a table and executor the
// commit does not hold. go build stamps the revision and the modified flag in the binary; the run compares
// them with the verified commit's engine paths. A binary without a revision (go run, go test) is compiled from
// the working tree the source check has just compared with the commit.

func removeForTest(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// stampedBuild is the build record of a binary go build made in a repository.
func stampedBuild(revision, modified string) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		settings := []debug.BuildSetting{{Key: "vcs", Value: "git"}}
		if revision != "" {
			settings = append(settings, debug.BuildSetting{Key: "vcs.revision", Value: revision})
		}
		if modified != "" {
			settings = append(settings, debug.BuildSetting{Key: "vcs.modified", Value: modified})
		}
		return &debug.BuildInfo{Settings: settings}, true
	}
}

func verifyWithBuild(t *testing.T, repo *localFixture, build func() (*debug.BuildInfo, bool)) (localResult string, err error) {
	t.Helper()
	opts := localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "r.json"))
	opts.buildInfo = build
	made, _, err := localVerify(opts, "", io.Discard)
	return made.Result, err
}

func TestLocal_a_binary_built_from_another_engine_revision_refuses_the_run(t *testing.T) {
	repo := localEngineRepo(t)
	old := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	repo.write("internal/dev/ci/engine.go", "package ci\n// the next revision\n")
	repo.commit()
	result, err := verifyWithBuild(t, repo, stampedBuild(old, "false"))
	if err == nil || !strings.Contains(err.Error(), "engine_differs_from_commit") || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("err = %v, result = %q; want an engine_differs_from_commit refusal naming the binary", err, result)
	}
}

func TestLocal_a_binary_built_from_a_modified_tree_refuses_the_run(t *testing.T) {
	repo := localEngineRepo(t)
	head := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	result, err := verifyWithBuild(t, repo, stampedBuild(head, "true"))
	if err == nil || !strings.Contains(err.Error(), "engine_differs_from_commit") || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("err = %v, result = %q; want an engine_differs_from_commit refusal naming the modified tree", err, result)
	}
}

func TestLocal_a_binary_built_from_a_revision_not_in_the_repository_refuses_the_run(t *testing.T) {
	repo := localEngineRepo(t)
	result, err := verifyWithBuild(t, repo, stampedBuild(strings.Repeat("a", 40), "false"))
	if err == nil || !strings.Contains(err.Error(), "engine_differs_from_commit") {
		t.Fatalf("err = %v, result = %q; want an engine_differs_from_commit refusal", err, result)
	}
}

// Controls: a clean binary built from the verified commit, from another commit with the same engine files,
// and a binary without a revision all run.
func TestLocal_a_binary_of_the_verified_engine_runs(t *testing.T) {
	repo := localEngineRepo(t)
	built := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	repo.write("README.md", "outside the engine paths\n")
	repo.commit()
	for name, build := range map[string]func() (*debug.BuildInfo, bool){
		"built from the verified commit":                          stampedBuild(strings.TrimSpace(repo.git("rev-parse", "HEAD")), "false"),
		"built from an earlier commit with the same engine files": stampedBuild(built, "false"),
		"no revision (go run, go test)":                           stampedBuild("", ""),
		"no build record":                                         func() (*debug.BuildInfo, bool) { return nil, false },
	} {
		t.Run(name, func(t *testing.T) {
			result, err := verifyWithBuild(t, repo, build)
			if err != nil {
				t.Fatal(err)
			}
			if result != localPass {
				t.Errorf("result = %q, want %q", result, localPass)
			}
		})
	}
}
