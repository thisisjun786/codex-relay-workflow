package manage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// C1: a stop at each of steps 1 to 4 calls no later step. The fakes' received calls are the
// proof: the steps after the stop never ran.
func TestUpgradeStopsBeforeTheNextStep(t *testing.T) {
	commit := upgradeGoodCommit
	for _, tc := range []struct {
		name   string
		opts   upgradeHarnessOptions
		break_ func(t *testing.T, h *upgradeEnv)
		code   int
		reason string
		absent []string
	}{
		{name: "step 1",
			opts: upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", gh: upgradeGhPaths(commit)},
			break_: func(t *testing.T, h *upgradeEnv) {
				if err := os.WriteFile(filepath.Join(h.release, upgradeSumsName), []byte("deadbeef  crw_0.4.0_linux_amd64.tar.gz\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			code: 2, reason: upgradeReasonSumsFailed, absent: []string{"commits/", "check-runs", "service"}},
		{name: "step 2",
			opts: upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", gh: map[string]upgradeGhAnswer{}},
			code: 2, reason: upgradeReasonCommitUnknown, absent: []string{"check-runs", "service"}},
		{name: "step 3",
			opts: upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", gh: map[string]upgradeGhAnswer{
				"repos/owner/repo/commits/eb2567df7":                 {Body: "{\"sha\":\"" + commit + "\"}"},
				"repos/owner/repo/commits/" + commit + "/check-runs": {Body: "{\"check_runs\":[]}"},
			}},
			code: 2, reason: upgradeReasonDevGate, absent: []string{"service"}},
		{name: "step 4",
			opts: upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", openAttempts: 1, gh: upgradeGhPaths(commit)},
			code: 3, reason: upgradeReasonOpenAttempts, absent: []string{"service"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := upgradeHarness(t, tc.opts)
			if tc.break_ != nil {
				tc.break_(t, h)
			}
			if code := h.run("--release-dir", h.release); code != tc.code {
				t.Fatalf("exit %d, want %d", code, tc.code)
			}
			record := h.recordOf(t)
			if record.Reason != tc.reason {
				t.Errorf("reason %q, want %q", record.Reason, tc.reason)
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
// and its extract directory behind.
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
	if _, err := os.Stat(record.ExtractDir); err != nil {
		t.Errorf("the extract directory is gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(record.ExtractDir, "crw")); err != nil {
		t.Errorf("the extracted crw is gone: %v", err)
	}
}

// C2: start is called even when the update fails.
func TestUpgradeStartsAfterAFailedUpdate(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", gh: upgradeGhPaths(upgradeGoodCommit), pointer: true, installExit: 1})
	if code := h.run("--release-dir", h.release); code == 0 {
		t.Fatal("a failed update reported success")
	}
	var stop, start int
	for _, line := range h.crwCalls() {
		if strings.Contains(line, "service stop") {
			stop++
		}
		if strings.Contains(line, "service start") {
			start++
		}
	}
	if stop != 1 || start != 1 {
		t.Errorf("stop %d start %d, want 1 each: %q", stop, start, h.crwCalls())
	}
}

// C3: when config.toml changes during the run the command exits 4 and names the post-check.
func TestUpgradeExitsFourWhenTheConfigChanged(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", gh: upgradeGhPaths(upgradeGoodCommit), pointer: true, mutateConfig: true})
	if code := h.run("--release-dir", h.release); code != upgradeExitPostCheck {
		t.Fatalf("exit %d, want %d", code, upgradeExitPostCheck)
	}
	record := h.recordOf(t)
	if record.Reason != upgradeReasonPostCheck {
		t.Errorf("reason %q, want %q", record.Reason, upgradeReasonPostCheck)
	}
}

// The repository is a precondition: an unconfigured one is refused before any gh call, dry run
// included.
func TestUpgradeRefusesAnUnconfiguredRepository(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", gh: upgradeGhPaths(upgradeGoodCommit)})
	old := upgradeConfig
	upgradeConfig = func(e *Env) *Config { return coreDefaults(e) }
	defer func() { upgradeConfig = old }()
	var out, errOut strings.Builder
	code := Run(context.Background(), []string{"runtime-upgrade", "--release-dir", h.release}, strings.NewReader(""), &out, &errOut)
	if code != upgradeExitRefused {
		t.Fatalf("exit %d, want %d", code, upgradeExitRefused)
	}
	if !strings.Contains(errOut.String(), upgradeReasonRepository) {
		t.Errorf("the refusal does not name %s: %q", upgradeReasonRepository, errOut.String())
	}
	if calls := h.ghCallLines(); len(calls) != 0 {
		t.Errorf("an unconfigured repository still called gh: %q", calls)
	}
}

// An unusable runtime pointer is refused before the service is touched.
func TestUpgradeRefusesAMissingPointer(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", gh: upgradeGhPaths(upgradeGoodCommit)})
	if code := h.run("--release-dir", h.release); code != upgradeExitRefused {
		t.Fatalf("exit %d, want %d", code, upgradeExitRefused)
	}
	for _, line := range h.crwCalls() {
		if strings.Contains(line, "service") {
			t.Errorf("a missing pointer still touched the service: %q", line)
		}
	}
}

// The command prints its usage: exit 0 for the help flags, exit 2 for anything unusable.
func TestUpgradeCommandPrintsItsUsage(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{version: "v0.4.0-4633-geb2567df7", gh: upgradeGhPaths(upgradeGoodCommit)})
	for _, args := range [][]string{{"-h"}, {"--help"}, {"nope"}, {}} {
		var out, errOut strings.Builder
		code := Run(context.Background(), append([]string{"runtime-upgrade"}, args...), strings.NewReader(""), &out, &errOut)
		if len(args) > 0 && args[0] == "-h" || len(args) > 0 && args[0] == "--help" {
			if code != 0 || !strings.Contains(out.String(), "runtime-upgrade") {
				t.Errorf("%q: exit %d %q", args, code, out.String())
			}
			continue
		}
		if code != usageExit {
			t.Errorf("%q: exit %d, want %d", args, code, usageExit)
		}
	}
	_ = h
}
