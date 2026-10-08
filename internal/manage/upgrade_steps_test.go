package manage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// C1: a stop at each of steps 1 to 4 calls no later step, proven by the fakes' received calls.
// The pointer is in place in every case, so a fall-through would really reach the snapshot's
// install call and the service calls; the absence assertion is therefore not vacuous.
func TestUpgradeStopsBeforeTheNextStep(t *testing.T) {
	commit := upgradeGoodCommit
	badSums := func(t *testing.T, h *upgradeEnv) {
		if err := os.WriteFile(filepath.Join(h.release, upgradeSumsName), []byte("deadbeef  "+upgradeArchiveName+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gate := func(body string) map[string]upgradeGhAnswer {
		out := upgradeGhPaths(commit)
		out["repos/owner/repo/commits/"+commit+"/check-runs"] = upgradeGhAnswer{Body: body}
		return out
	}
	for _, tc := range []struct {
		name   string
		opts   upgradeHarnessOptions
		break_ func(*testing.T, *upgradeEnv)
		code   int
		reason string
		absent []string
	}{
		{"step 1", upgradeHarnessOptions{gh: upgradeGhPaths(commit), pointer: true}, badSums,
			2, upgradeReasonSumsFailed, []string{"commits/", "check-runs", "install", "service"}},
		{"step 2", upgradeHarnessOptions{gh: map[string]upgradeGhAnswer{}, pointer: true}, nil,
			2, upgradeReasonCommitUnknown, []string{"check-runs", "install", "service"}},
		{"step 3", upgradeHarnessOptions{gh: gate("{\"check_runs\":[]}"), pointer: true}, nil,
			2, upgradeReasonDevGate, []string{"install", "service"}},
		{"step 4", upgradeHarnessOptions{openAttempts: 1, gh: upgradeGhPaths(commit), pointer: true}, nil,
			3, upgradeReasonOpenAttempts, []string{"install", "service"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := upgradeHarness(t, tc.opts)
			if tc.break_ != nil {
				tc.break_(t, h)
			}
			if code := h.run("--release-dir", h.release); code != tc.code {
				t.Fatalf("exit %d, want %d; the record is %+v", code, tc.code, h.recordOf(t))
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

// C4, C5 and C8: a dry run performs steps 1 to 4, calls no service command, and leaves its record
// and extract directory behind. Two runs in one second cannot share a run directory, and the
// injected clock is what decides that: the same second refuses the second run, and one second
// later the run takes a directory of its own. Pinning Env.Now is what makes this deterministic:
// the wall clock crossing a second boundary between the two runs changes nothing.
func TestUpgradeDryRunCallsNoServiceCommand(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true})
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
	t.Run("the same second refuses", func(t *testing.T) {
		if code := h.run("--release-dir", h.release, "--dry-run"); code == 0 {
			t.Fatal("a second run in the same second overwrote the first")
		}
		if got := h.recordDirs(); len(got) != 1 {
			t.Errorf("the run directories are %v, want the one run", got)
		}
	})
	t.Run("the next second takes its own directory", func(t *testing.T) {
		h.advance(time.Second)
		if code := h.run("--release-dir", h.release, "--dry-run"); code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
		if got := h.recordDirs(); len(got) != 2 {
			t.Errorf("the run directories are %v, want two", got)
		}
	})
}

// C2: start is called even when the update fails, and the service comes back on the runtime the
// pointer still names.
func TestUpgradeStartsAfterAFailedUpdate(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true, installExit: 1})
	if code := h.run("--release-dir", h.release); code == 0 {
		t.Fatal("a failed update reported success")
	}
	if !h.calledFrom(filepath.Join(h.previous, "bin", "codex-session-relay"), "service start") {
		t.Errorf("a failed update did not stop then start: %q", h.callLines())
	}
}

// C3: when config.toml changes during the run the command exits 4 and names the post-check. The
// differential is TestUpgradeSucceedsOnAHealthyHost: the same run exits 0 with the configuration
// left alone, so this exit 4 is the configuration comparison, not the status check.
func TestUpgradeExitsFourWhenTheConfigChanged(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true,
		produceRuntime: true, pointAtIt: true, mutateConfig: true})
	if code := h.run("--release-dir", h.release); code != upgradeExitPostCheck {
		t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitPostCheck, h.recordOf(t))
	}
	if got := h.recordOf(t).Reason; got != upgradeReasonConfigChanged {
		t.Errorf("reason %q, want %q", got, upgradeReasonConfigChanged)
	}
}

// A stop that runs but fails stops the run before the runtime is replaced: the record names the
// stop and no install follows.
func TestUpgradeRefusesAFailedStop(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true, stopExit: 1})
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
		h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit)})
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
		h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit)})
		if code := h.run("--release-dir", h.release); code != upgradeExitRefused {
			t.Fatalf("exit %d, want %d", code, upgradeExitRefused)
		}
		for _, line := range h.callArgs() {
			if strings.Contains(line, "service") {
				t.Errorf("a missing pointer still touched the service: %q", line)
			}
		}
	})
}
