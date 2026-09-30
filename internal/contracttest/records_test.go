package contracttest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseStepScript_is_the_steps_one_literal_shell_block(t *testing.T) {
	// Given: the release workflow the release runner reads.
	raw, err := os.ReadFile(filepath.Join(RootMust(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	// When
	script, err := releaseStepScript(string(raw), "validate", "release-inputs")
	// Then: the block, dedented, and nothing of the step's metadata or the next step.
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(script, "set -euo pipefail\n") || !strings.Contains(script, "Only the repository owner may authorize a release.") || strings.Contains(script, "Checkout release source") {
		t.Fatalf("release-inputs script:\n%s", script)
	}
	for _, missing := range [][2]string{{"validate", "no-such-step"}, {"no-such-job", "release-inputs"}} {
		if _, err := releaseStepScript(string(raw), missing[0], missing[1]); err == nil {
			t.Fatalf("%v: expected a refusal", missing)
		}
	}
}

func TestReleaseFakes_read_the_state_the_runner_writes(t *testing.T) {
	// Given: fake state as runRelease writes it, one file per key.
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	for name, values := range map[string]map[string]any{"ci": {"case": "missing"}, "tag": {"case": "present", "object_type": "tag"}, "push": {"lie_main": true, "fail_main": false}} {
		if err := writeReleaseState(filepath.Join(state, name), values); err != nil {
			t.Fatal(err)
		}
	}
	fakes := filepath.Join(RootMust(t), "internal", "contracttest", "testdata", "release")
	run := func(fake string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(filepath.Join(fakes, fake), args...)
		cmd.Env = append(os.Environ(), "GH_STATE="+state, "GH_LOG="+filepath.Join(dir, "gh.log"), "RELEASE_SHA=abc", "RELEASE_TAG=v0.1.0", "GIT_REAL=/bin/false")
		answer, err := runProcess(cmd)
		if err != nil {
			t.Fatal(err)
		}
		return string(answer.stdout), answer.exit
	}
	// When / Then: each fake answers from the key it reads.
	if out, code := run("fake_gh.sh", "api", "repos/o/r/actions/workflows/ci.yml/runs"); code != 0 || out != "{\"workflow_runs\":[]}\n" {
		t.Fatalf("gh ci missing: %d %q", code, out)
	}
	if out, code := run("fake_gh.sh", "api", "repos/o/r/git/ref/tags/v0.1.0"); code != 0 || out != "{\"ref\":\"refs/tags/v0.1.0\",\"object\":{\"sha\":\"abc\",\"type\":\"tag\"}}\n" {
		t.Fatalf("gh tag: %d %q", code, out)
	}
	if out, code := run("fake_git.sh", "ls-remote", "origin", "refs/heads/main"); code != 0 || out != "0000000000000000000000000000000000000000\trefs/heads/main\n" {
		t.Fatalf("git ls-remote lie: %d %q", code, out)
	}
	if log, err := os.ReadFile(filepath.Join(dir, "gh.log")); err != nil || strings.Count(string(log), "\n") != 2 {
		t.Fatalf("gh.log: %q %v", log, err)
	}
}
