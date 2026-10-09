//go:build dev

package ci

import (
	"io"
	"os"
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

// An engine source file hidden from git by .git/info/exclude is still compiled by Go, so it refuses
// the run like any other untracked engine file (verifier P1).
func TestLocal_an_ignored_go_engine_source_refuses_the_run(t *testing.T) {
	repo := localEngineRepo(t)
	repo.write(".git/info/exclude", "internal/dev/ci/override.go\n")
	repo.write("internal/dev/ci/override.go", "package ci\n")
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "r.json")), "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "engine_differs_from_commit") {
		t.Fatalf("err = %v, result = %q; want an engine_differs_from_commit refusal", err, made.Result)
	}
}

// An ignored file Go does not compile (a build artefact, a log) is not engine source: the run proceeds.
func TestLocal_an_ignored_artefact_under_the_engine_paths_does_not_refuse(t *testing.T) {
	repo := localEngineRepo(t)
	repo.write(".git/info/exclude", "*.out\n")
	repo.write("internal/dev/ci/coverage.out", "mode: set\n")
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "r.json")), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localPass {
		t.Errorf("result = %q, want %q", made.Result, localPass)
	}
}

// Git's assume-unchanged and skip-worktree index marks hide a working tree edit from git diff; Go still
// compiles the edited file, so the engine bytes are compared with the commit's regardless of the marks
// (verifier P1).
func TestLocal_an_index_marked_tracked_engine_edit_refuses_the_run(t *testing.T) {
	for _, mark := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(mark, func(t *testing.T) {
			repo := localEngineRepo(t)
			repo.git("update-index", mark, "internal/dev/ci/engine.go")
			repo.write("internal/dev/ci/engine.go", "package ci\n// edited behind an index mark\n")
			made, _, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "r.json")), "", io.Discard)
			if err == nil || !strings.Contains(err.Error(), "engine_differs_from_commit") {
				t.Fatalf("err = %v, result = %q; want an engine_differs_from_commit refusal", err, made.Result)
			}
		})
	}
}

// A committed engine file that is deleted in the working tree is a difference too.
func TestLocal_a_deleted_tracked_engine_source_refuses_the_run(t *testing.T) {
	repo := localEngineRepo(t)
	if err := os.Remove(filepath.Join(repo.root, "internal/dev/ci/engine.go")); err != nil {
		t.Fatal(err)
	}
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "r.json")), "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "engine_differs_from_commit") {
		t.Fatalf("err = %v, result = %q; want an engine_differs_from_commit refusal", err, made.Result)
	}
}

// CRW-1027 (merged into CRW-1025): the secrets step downloads the linux x64 Gitleaks archive and calls
// sha256sum, so a plan that carries it refuses a host that is not linux x64 before any step runs, and no
// record is written. A plan without the step is platform independent.
func localSecretsPlan(marker string) []localJob {
	return []localJob{{
		name: "secrets",
		steps: []localStep{
			{kind: localAction, action: localCheckout, scope: "full"},
			{kind: localRun, command: localSecretsCommand, scope: "range", heavy: true},
			{name: "Marker", kind: localRun, command: "touch " + marker, scope: "full", heavy: true},
		},
	}}
}

func TestLocal_a_host_that_is_not_linux_x64_refuses_the_secrets_step_before_any_step_runs(t *testing.T) {
	for _, host := range [][2]string{{"darwin", "arm64"}, {"linux", "arm64"}, {"darwin", "amd64"}} {
		t.Run(host[0]+"_"+host[1], func(t *testing.T) {
			repo := newLocalFixture(t)
			marker := filepath.Join(t.TempDir(), "ran")
			record := filepath.Join(t.TempDir(), "r.json")
			opts := localRunOptions(repo, localSecretsPlan(marker), record)
			opts.hostOS, opts.hostArch = host[0], host[1]
			_, _, err := localVerify(opts, "", io.Discard)
			if err == nil || !strings.Contains(err.Error(), "unsupported_platform") || !strings.Contains(err.Error(), host[0]+"/"+host[1]) {
				t.Fatalf("err = %v; want an unsupported_platform refusal naming %s/%s", err, host[0], host[1])
			}
			if _, statErr := os.Stat(marker); statErr == nil {
				t.Error("a step ran before the platform was refused")
			}
			if _, statErr := os.Stat(record); statErr == nil {
				t.Error("a record was written for a refused platform")
			}
		})
	}
}

func TestLocal_a_plan_without_the_secrets_step_runs_on_any_platform(t *testing.T) {
	repo := newLocalFixture(t)
	opts := localRunOptions(repo, localFixturePlan("echo hello"), filepath.Join(t.TempDir(), "r.json"))
	opts.hostOS, opts.hostArch = "darwin", "arm64"
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.OS != "darwin" || made.Arch != "arm64" || made.Result != localPass {
		t.Errorf("record = %s/%s %q, want darwin/arm64 pass", made.OS, made.Arch, made.Result)
	}
}

func TestLocal_the_secrets_step_runs_on_linux_x64(t *testing.T) {
	repo := newLocalFixture(t)
	marker := filepath.Join(t.TempDir(), "ran")
	opts := localRunOptions(repo, localSecretsPlan(marker), filepath.Join(t.TempDir(), "r.json"))
	opts.hostOS, opts.hostArch = "linux", "amd64"
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("the plan did not run: result %q, %v", made.Result, statErr)
	}
}
