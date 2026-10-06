package manage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// C1: a stop at each of steps 1 to 4 calls no later step, proven by the fakes' received calls.
// The pointer is in place in every case, so a fall-through would really reach the snapshot's
// install call and the service calls; the absence assertion is therefore not vacuous.
func TestUpgradeStopsBeforeTheNextStep(t *testing.T) {
	commit := upgradeGoodCommit
	badSums := func(t *testing.T, h *upgradeEnv) {
		if err := os.WriteFile(filepath.Join(h.release, upgradeSumsName), []byte("deadbeef  crw_0.4.0_linux_amd64.tar.gz\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gate := func(body string) map[string]upgradeGhAnswer {
		return map[string]upgradeGhAnswer{
			"repos/owner/repo/commits/eb2567df7":                 {Body: "{\"sha\":\"" + commit + "\"}"},
			"repos/owner/repo/commits/" + commit + "/check-runs": {Body: body},
		}
	}
	for _, tc := range []struct {
		name   string
		opts   upgradeHarnessOptions
		break_ func(*testing.T, *upgradeEnv)
		code   int
		reason string
		absent []string
	}{
		{"step 1", upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", gh: upgradeGhPaths(commit), pointer: true}, badSums,
			2, upgradeReasonSumsFailed, []string{"commits/", "check-runs", "install", "service"}},
		{"step 2", upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", gh: map[string]upgradeGhAnswer{}, pointer: true}, nil,
			2, upgradeReasonCommitUnknown, []string{"check-runs", "install", "service"}},
		{"step 3", upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", gh: gate("{\"check_runs\":[]}"), pointer: true}, nil,
			2, upgradeReasonDevGate, []string{"install", "service"}},
		{"step 4", upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", openAttempts: 1, gh: upgradeGhPaths(commit), pointer: true}, nil,
			3, upgradeReasonOpenAttempts, []string{"install", "service"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := upgradeHarness(t, tc.opts)
			if tc.break_ != nil {
				tc.break_(t, h)
			}
			if code := h.run("--release-dir", h.release); code != tc.code {
				t.Fatalf("exit %d, want %d", code, tc.code)
			}
			if got := h.recordOf(t).Reason; got != tc.reason {
				t.Errorf("reason %q, want %q", got, tc.reason)
			}
			joined := strings.Join(append(h.crwCalls(), h.ghCallLines()...), " ")
			for _, needle := range tc.absent {
				if strings.Contains(joined, needle) {
					t.Errorf("a call after the stop carried %q: %s", needle, joined)
				}
			}
		})
	}
}

// C4 and C5: a dry run performs steps 1 to 4, calls no service command, and leaves its record
// and extract directory behind; a second run in the same second refuses rather than overwriting.
func TestUpgradeDryRunCallsNoServiceCommand(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", gh: upgradeGhPaths(upgradeGoodCommit)})
	if code := h.run("--release-dir", h.release, "--dry-run"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, line := range append(h.crwCalls(), h.ghCallLines()...) {
		if strings.Contains(line, "service") {
			t.Errorf("a dry run called a service command: %q", line)
		}
	}
	record := h.recordOf(t)
	if !record.DryRun || record.Outcome != "ok" {
		t.Errorf("the record is %+v", record)
	}
	if _, err := os.Stat(filepath.Join(record.ExtractDir, "crw")); err != nil {
		t.Errorf("the extracted crw is gone: %v", err)
	}
	if code := h.run("--release-dir", h.release, "--dry-run"); code == 0 {
		t.Fatal("a second run in the same second overwrote the first")
	}
}

// C2: start is called even when the update fails.
func TestUpgradeStartsAfterAFailedUpdate(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", gh: upgradeGhPaths(upgradeGoodCommit), pointer: true, installExit: 1})
	if code := h.run("--release-dir", h.release); code == 0 {
		t.Fatal("a failed update reported success")
	}
	joined := strings.Join(h.crwCalls(), " ")
	if !strings.Contains(joined, "service stop") || !strings.Contains(joined, "service start") {
		t.Errorf("a failed update did not stop then start: %q", joined)
	}
}

// C3: when config.toml changes during the run the command exits 4 and names the post-check. The
// differential is TestUpgradeSucceedsOnAHealthyHost: the same run exits 0 with the configuration
// left alone, so this exit 4 is the configuration comparison, not the status check.
func TestUpgradeExitsFourWhenTheConfigChanged(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", gh: upgradeGhPaths(upgradeGoodCommit), pointer: true, mutateConfig: true})
	if code := h.run("--release-dir", h.release); code != upgradeExitPostCheck {
		t.Fatalf("exit %d, want %d", code, upgradeExitPostCheck)
	}
	if got := h.recordOf(t).Reason; got != upgradeReasonPostCheck {
		t.Errorf("reason %q, want %q", got, upgradeReasonPostCheck)
	}
}

// A stop that runs but fails stops the run before the runtime is replaced: the record names the
// stop and no install follows.
func TestUpgradeRefusesAFailedStop(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", gh: upgradeGhPaths(upgradeGoodCommit), pointer: true, stopExit: 1})
	if code := h.run("--release-dir", h.release); code != upgradeExitRefused {
		t.Fatalf("exit %d, want %d", code, upgradeExitRefused)
	}
	if got := h.recordOf(t).Reason; got != upgradeReasonStopFailed {
		t.Errorf("reason %q, want %q", got, upgradeReasonStopFailed)
	}
	if joined := strings.Join(h.crwCalls(), " "); strings.Contains(joined, "install update") {
		t.Errorf("a failed stop still installed: %q", joined)
	}
}

// The preconditions: an unconfigured repository before any gh call, an unusable pointer before
// the service is touched.
func TestUpgradeRefusesBeforeTheSteps(t *testing.T) {
	t.Run("repository", func(t *testing.T) {
		h := upgradeHarness(t, upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", gh: upgradeGhPaths(upgradeGoodCommit)})
		old := upgradeConfig
		upgradeConfig = func(e *Env) *Config { return coreDefaults(e) }
		defer func() { upgradeConfig = old }()
		var out, errOut strings.Builder
		code := Run(context.Background(), []string{"runtime-upgrade", "--release-dir", h.release}, strings.NewReader(""), &out, &errOut)
		if code != upgradeExitRefused || !strings.Contains(errOut.String(), upgradeReasonRepository) {
			t.Fatalf("exit %d %q, want %d naming %s", code, errOut.String(), upgradeExitRefused, upgradeReasonRepository)
		}
		if calls := h.ghCallLines(); len(calls) != 0 {
			t.Errorf("an unconfigured repository still called gh: %q", calls)
		}
	})
	t.Run("pointer", func(t *testing.T) {
		h := upgradeHarness(t, upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", gh: upgradeGhPaths(upgradeGoodCommit)})
		if code := h.run("--release-dir", h.release); code != upgradeExitRefused {
			t.Fatalf("exit %d, want %d", code, upgradeExitRefused)
		}
		for _, line := range h.crwCalls() {
			if strings.Contains(line, "service") {
				t.Errorf("a missing pointer still touched the service: %q", line)
			}
		}
	})
}

// The command prints its usage: exit 0 for the help flags, exit 2 for anything unusable.
func TestUpgradeCommandPrintsItsUsage(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"-h"}, 0}, {[]string{"--help"}, 0},
		{[]string{"nope"}, usageExit}, {nil, usageExit},
	} {
		var out, errOut strings.Builder
		if code := Run(context.Background(), append([]string{"runtime-upgrade"}, tc.args...), strings.NewReader(""), &out, &errOut); code != tc.code {
			t.Errorf("%q: exit %d, want %d", tc.args, code, tc.code)
		}
	}
}

// A released tag version resolves through the tag ref (and its v-prefixed form); a git-describe
// version resolves through its short hash.
func TestUpgradeCommitRefs(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    string
	}{
		{"v0.4.0-4633-geb2567df7", "eb2567df7"},
		{"v0.4.1", "v0.4.1"},
		{"0.4.1", "0.4.1,v0.4.1"},
		{"", ""},
	} {
		if got := strings.Join(upgradeCommitRefs(tc.version), ","); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.version, got, tc.want)
		}
	}
}

// An entry through an existing symlink is refused, so a crafted archive cannot reach a file
// outside the extract directory.
func TestUpgradeExtractRefusesASymlinkEscape(t *testing.T) {
	dir, outside := t.TempDir(), filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "crw")); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(archive, upgradeTarGz(t, map[string]string{"crw": "overwritten"}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := upgradeExtract(archive, dir); err == nil {
		t.Fatal("an entry through a symlink was accepted")
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "original" {
		t.Errorf("the file outside the extract directory changed: %q %v", data, err)
	}
}

// The full success flow for both version shapes: with the pointer in place, an update that
// succeeds and a service that runs and matches, every step runs and the command exits 0.
func TestUpgradeSucceedsOnAHealthyHost(t *testing.T) {
	tag := upgradeGhPaths(upgradeGoodCommit)
	delete(tag, "repos/owner/repo/commits/eb2567df7")
	tag["repos/owner/repo/commits/v0.4.1"] = upgradeGhAnswer{Body: "{\"sha\":\"" + upgradeGoodCommit + "\"}"}
	for _, tc := range []struct {
		version string
		gh      map[string]upgradeGhAnswer
	}{{"v0.4.0-4633-geb2567df7", upgradeGhPaths(upgradeGoodCommit)}, {"v0.4.1", tag}} {
		h := upgradeHarness(t, upgradeHarnessOptions{version: tc.version, gh: tc.gh, pointer: true})
		if code := h.run("--release-dir", h.release); code != 0 {
			t.Fatalf("%s: exit %d, want 0", tc.version, code)
		}
		record := h.recordOf(t)
		if record.Outcome != "ok" || record.Reason != "" {
			t.Errorf("%s: the record is %+v", tc.version, record)
		}
		var steps []string
		for _, s := range record.Steps {
			steps = append(steps, s.Step)
		}
		joined := strings.Join(steps, ",")
		for _, want := range []string{upgradeStepSums, upgradeStepExtract, upgradeStepCommit, upgradeStepDevGate,
			upgradeStepAttempts, upgradeStepSnapshot, upgradeStepStop, upgradeStepUpdate, upgradeStepStart, upgradeStepPostCheck} {
			if !strings.Contains(joined, want) {
				t.Errorf("%s: the record does not name the step %q: %v", tc.version, want, joined)
			}
		}
	}
}
