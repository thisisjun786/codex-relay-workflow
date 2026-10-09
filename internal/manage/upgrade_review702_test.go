package manage

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The review-702 cases: the defects the post-merge review of PR #702 found in
// crw manage runtime-upgrade, one test per decided answer.

// upgradeReview702Swapped is an archive whose crw is not the verified one: it records that it ran
// and answers with a version that resolves nowhere. It is what an adversary puts in the release
// directory in the moment between the checksum being verified and the bytes being used.
func upgradeReview702Swapped(t *testing.T, marker string) []byte {
	t.Helper()
	return upgradeTarGz(t, map[string]string{"crw": "#!/bin/sh\n" +
		"printf 'swapped\\n' >> " + coreShellQuote(marker) + "\n" +
		"printf '%s\\n' 'v0.4.0-9999-gdeadbeef'\nexit 0\n"})
}

// TestUpgradeReview702SwappedArchiveIsNotRun: the release directory's archive is replaced at the
// moment the adversary has the chance - as soon as the checksum has been verified, before the bytes
// are used. The replacement really takes (the release directory holds the swapped archive when the
// run goes on), and it still changes nothing: the run unpacks the archive whose digest it verified,
// and the crw of the swapped archive never runs. The baseline hashes the original archive and then
// copies that same original, so the same replacement between those two reads is what it unpacks and
// runs; here the run has already copied the archive it verified, and the seam's swap reaches only
// the release directory.
func TestUpgradeReview702SwappedArchiveIsNotRun(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "swapped-crw-ran")
	swapped := upgradeReview702Swapped(t, marker)
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true})
	verified, err := os.ReadFile(filepath.Join(h.release, upgradeArchiveName))
	if err != nil {
		t.Fatal(err)
	}

	replaced := false
	old := upgradeSumsVerified
	upgradeSumsVerified = func(releaseDir string) {
		replaced = true
		if err := os.WriteFile(filepath.Join(releaseDir, upgradeArchiveName), swapped, 0o600); err != nil {
			t.Errorf("replace the release archive: %v", err)
		}
	}
	t.Cleanup(func() { upgradeSumsVerified = old })

	code := h.run("--release-dir", h.release, "--dry-run")

	if !replaced {
		t.Fatal("the release archive was never replaced, so this case proves nothing")
	}
	onDisk, err := os.ReadFile(filepath.Join(h.release, upgradeArchiveName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(onDisk, verified) {
		t.Fatal("the release directory still holds the verified archive, so the replacement did not take")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the crw of the archive that replaced the verified one ran")
	}
	if code != 0 {
		t.Fatalf("exit %d, want 0; the record is %+v", code, h.recordOf(t))
	}
	pinned, err := os.ReadFile(filepath.Join(h.recordOf(t).Directory, upgradeArchiveName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pinned, verified) {
		t.Error("the archive the run unpacked is not the archive whose checksum it verified")
	}
	if calls := strings.Join(h.ghCallLines(), " "); !strings.Contains(calls, "commits/eb2567df7") {
		t.Errorf("the verified archive's version did not resolve through gh: %q", calls)
	}
}

// TestUpgradeReview702StartRunsWhenUpdateBreaksThePointer: an update that removes the owned pointer
// and fails must still leave the service running, started from the runtime the pointer named before
// the stop, and the record must say so.
func TestUpgradeReview702StartRunsWhenUpdateBreaksThePointer(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit),
		pointer: true, installExit: 1, breakPointer: true})
	if code := h.run("--release-dir", h.release); code == 0 {
		t.Fatal("a broken pointer reported success")
	}
	if !h.calledFrom(filepath.Join(h.previous, "bin", "codex-session-relay"), "service start") {
		t.Errorf("the service was not started from the runtime the pointer named before the stop: %q", h.callLines())
	}
	if got, _ := h.recordJSON(t)["start_from"].(string); got != h.previous {
		t.Errorf("the record names %q as what the service was started from, want %q", got, h.previous)
	}
}

// TestUpgradeReview702ConfigChangeWinsOverUpdateFailure: a configuration change the post-check
// found outranks a failed update, and the record keeps both reasons.
func TestUpgradeReview702ConfigChangeWinsOverUpdateFailure(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit),
		pointer: true, installExit: 1, mutateConfig: true})
	if code := h.run("--release-dir", h.release); code != upgradeExitPostCheck {
		t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitPostCheck, h.recordOf(t))
	}
	if got := h.recordOf(t).Reason; got != upgradeReasonConfigChanged {
		t.Errorf("reason %q, want %q", got, upgradeReasonConfigChanged)
	}
	reasons := upgradeReview702Reasons(t, h)
	for _, want := range []string{upgradeReasonUpdateFailed, upgradeReasonConfigChanged} {
		if !slices.Contains(reasons, want) {
			t.Errorf("the record does not name %q: %v", want, reasons)
		}
	}
}

// upgradeReview702Reasons is the reasons the record names, read from the record as written.
func upgradeReview702Reasons(t *testing.T, h *upgradeEnv) []string {
	t.Helper()
	var out []string
	values, _ := h.recordJSON(t)["reasons"].([]any)
	for _, value := range values {
		if text, ok := value.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// TestUpgradeReview702StalePointerIsAMismatch: the post-check passes only when the pointer names
// the runtime the update reported it installed and that runtime reports the archive's version.
func TestUpgradeReview702StalePointerIsAMismatch(t *testing.T) {
	t.Run("the pointer still names the previous runtime", func(t *testing.T) {
		h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit),
			pointer: true, produceRuntime: true})
		if code := h.run("--release-dir", h.release); code != upgradeExitPostCheck {
			t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitPostCheck, h.recordOf(t))
		}
		if got := h.recordOf(t).Reason; got != upgradeReasonRuntimeMismatch {
			t.Errorf("reason %q, want %q", got, upgradeReasonRuntimeMismatch)
		}
		if target, err := h.pointerTarget(); err != nil || target != h.previous {
			t.Errorf("the pointer was moved to %q (%v), want it left at %q", target, err, h.previous)
		}
	})
	t.Run("the update names no runtime", func(t *testing.T) {
		h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true})
		if code := h.run("--release-dir", h.release); code != upgradeExitPostCheck {
			t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitPostCheck, h.recordOf(t))
		}
		if got := h.recordOf(t).Reason; got != upgradeReasonRuntimeMismatch {
			t.Errorf("reason %q, want %q", got, upgradeReasonRuntimeMismatch)
		}
	})
	t.Run("the installed runtime reports another version", func(t *testing.T) {
		h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true,
			produceRuntime: true, pointAtIt: true, installedVersion: "v0.4.0-4633-gdeadbeef"})
		if code := h.run("--release-dir", h.release); code != upgradeExitPostCheck {
			t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitPostCheck, h.recordOf(t))
		}
		if got := h.recordOf(t).Reason; got != upgradeReasonRuntimeMismatch {
			t.Errorf("reason %q, want %q", got, upgradeReasonRuntimeMismatch)
		}
	})
}

// TestUpgradeReview702SlowWorkerIsWaitedFor: a service that answers running but not yet matching is
// read again, and a worker that settles a little later is not treated as a failure.
func TestUpgradeReview702SlowWorkerIsWaitedFor(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true,
		produceRuntime: true, pointAtIt: true,
		statusAnswers: []string{upgradeStatusUnknown, upgradeStatusUnknown, upgradeStatusSame}})
	if code := h.run("--release-dir", h.release); code != 0 {
		t.Fatalf("exit %d, want 0; the record is %+v", code, h.recordOf(t))
	}
	if reads := upgradeReview702StatusReads(h); reads != 3 {
		t.Errorf("the service status was read %d times, want 3", reads)
	}
}

// TestUpgradeReview702OpenAttemptsFromDoctor: the open attempt count is the relay doctor's
// contents.openAttempts, read from the runtime the pointer names, and contents that could not be
// read are refused rather than read as none.
func TestUpgradeReview702OpenAttemptsFromDoctor(t *testing.T) {
	t.Run("two open attempts refuse", func(t *testing.T) {
		h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit),
			pointer: true, openAttempts: 2})
		if code := h.run("--release-dir", h.release, "--dry-run"); code != upgradeExitOpenAttempts {
			t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitOpenAttempts, h.recordOf(t))
		}
		if got := h.recordOf(t).Reason; got != upgradeReasonOpenAttempts {
			t.Errorf("reason %q, want %q", got, upgradeReasonOpenAttempts)
		}
		if !h.calledFrom(filepath.Join(h.previous, "bin", "codex-session-relay"), "doctor") {
			t.Errorf("the runtime the pointer names was not asked for its doctor answer: %q", h.callLines())
		}
	})
	t.Run("unreadable contents refuse", func(t *testing.T) {
		h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit),
			pointer: true, doctorUnavailable: true})
		if code := h.run("--release-dir", h.release, "--dry-run"); code != upgradeExitRefused {
			t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitRefused, h.recordOf(t))
		}
		if got := h.recordOf(t).Reason; got != upgradeReasonStoreRead {
			t.Errorf("reason %q, want %q", got, upgradeReasonStoreRead)
		}
	})
	t.Run("contents with no count refuse", func(t *testing.T) {
		h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit),
			pointer: true, doctorNoCount: true})
		if code := h.run("--release-dir", h.release, "--dry-run"); code != upgradeExitRefused {
			t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitRefused, h.recordOf(t))
		}
		if got := h.recordOf(t).Reason; got != upgradeReasonStoreRead {
			t.Errorf("reason %q, want %q", got, upgradeReasonStoreRead)
		}
	})
}

// upgradeReview702StatusReads is how many times the run read the service status.
func upgradeReview702StatusReads(h *upgradeEnv) int {
	reads := 0
	for _, line := range h.callArgs() {
		if strings.Contains(line, "service status") {
			reads++
		}
	}
	return reads
}

// TestUpgradeReview702RestartsFromThePreviousRuntimeWhenTheNewOneWillNotStart: the pointer can
// resolve to a directory the service cannot be started from (a runtime whose relay executable is
// missing, say). The service is stopped by then, so the restart falls back to the runtime the
// pointer named before the stop rather than leaving the relay down, and the record names the
// runtime the service came back on. The recovery is kept, but the run must not report success: the
// runtime the update installed is not the one in service, so the pointer and the running code
// disagree, which is a mismatch.
func TestUpgradeReview702RestartsFromThePreviousRuntimeWhenTheNewOneWillNotStart(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true,
		produceRuntime: true, pointAtIt: true, breakInstalledStart: true})
	if code := h.run("--release-dir", h.release); code != upgradeExitPostCheck {
		t.Fatalf("exit %d, want %d; the installed runtime is not in service: %+v", code, upgradeExitPostCheck, h.recordOf(t))
	}
	if got := h.recordOf(t).Reason; got != upgradeReasonRuntimeMismatch {
		t.Errorf("reason %q, want %q", got, upgradeReasonRuntimeMismatch)
	}
	if !h.calledFrom(filepath.Join(h.installed, "bin", "codex-session-relay"), "service start") {
		t.Errorf("the pointer's runtime was not tried: %q", h.callLines())
	}
	if !h.calledFrom(filepath.Join(h.previous, "bin", "codex-session-relay"), "service start") {
		t.Errorf("the previous runtime was not tried after the pointer's runtime failed to start: %q", h.callLines())
	}
	if got := h.recordOf(t).StartFrom; got != h.previous {
		t.Errorf("the record says the service came back on %q, want the previous runtime %q", got, h.previous)
	}
}

// TestUpgradeReview702ConfigDeletionWinsOverUpdateFailure: the snapshot only proceeded when it read
// a configuration file, so a file that is gone afterwards is a change the run can state exactly. It
// outranks the update failure, and the record names both reasons.
func TestUpgradeReview702ConfigDeletionWinsOverUpdateFailure(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true,
		installExit: 1, deleteConfigAfter: true})
	if code := h.run("--release-dir", h.release); code != upgradeExitPostCheck {
		t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitPostCheck, h.recordOf(t))
	}
	if got := h.recordOf(t).Reason; got != upgradeReasonConfigChanged {
		t.Errorf("reason %q, want %q", got, upgradeReasonConfigChanged)
	}
	reasons := upgradeReview702Reasons(t, h)
	for _, want := range []string{upgradeReasonUpdateFailed, upgradeReasonConfigChanged} {
		if !slices.Contains(reasons, want) {
			t.Errorf("the record does not name %q: %v", want, reasons)
		}
	}
}

// TestUpgradeReview702UnreadableConfigIsNotAChange: a configuration file that exists but cannot be
// read is not a configuration change: the run cannot tell whether it changed, so it is a post-check
// finding of its own. An update failure keeps its own exit code, and the record must not claim
// config_changed.
func TestUpgradeReview702UnreadableConfigIsNotAChange(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true,
		installExit: 1, unreadableConfigAfter: true})
	if code := h.run("--release-dir", h.release); code != upgradeExitUpdateFailed {
		t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitUpdateFailed, h.recordOf(t))
	}
	if got := h.recordOf(t).Reason; got != upgradeReasonUpdateFailed {
		t.Errorf("reason %q, want %q", got, upgradeReasonUpdateFailed)
	}
	if slices.Contains(h.recordOf(t).Reasons, upgradeReasonConfigChanged) {
		t.Errorf("an unreadable configuration was recorded as a change: %v", h.recordOf(t).Reasons)
	}
}

// TestUpgradeReview702PreUpdateRefusalNamesItsReason: a run that refuses before the update still
// names its reason in the record's reasons list, not only in the single reason field.
func TestUpgradeReview702PreUpdateRefusalNamesItsReason(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true})
	if err := os.WriteFile(filepath.Join(h.release, upgradeSumsName), []byte("deadbeef  "+upgradeArchiveName+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.run("--release-dir", h.release); code != upgradeExitRefused {
		t.Fatalf("exit %d, want %d", code, upgradeExitRefused)
	}
	if got := h.recordOf(t).Reasons; !slices.Contains(got, upgradeReasonSumsFailed) {
		t.Errorf("the record's reasons are %v, want them to name %s", got, upgradeReasonSumsFailed)
	}
}

// TestUpgradeReview702FailedUpdateIsNotAVersionMismatch: when the update did not land, the pointer
// correctly stays on the runtime it replaced, whose version is not the archive's. That is a
// rollback, not a mismatch, so the record must not claim runtime_mismatch.
func TestUpgradeReview702FailedUpdateIsNotAVersionMismatch(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true,
		installExit: 1, previousVersion: "v0.4.0-4633-gaaaaaaaa1"})
	if code := h.run("--release-dir", h.release); code != upgradeExitUpdateFailed {
		t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitUpdateFailed, h.recordOf(t))
	}
	// The pointer still names the runtime the failed update replaced, which reports the previous
	// version. That is the rollback working, not a mismatch.
	if target, err := h.pointerTarget(); err != nil || target != h.previous {
		t.Errorf("the pointer names %q (%v), want the previous runtime %q", target, err, h.previous)
	}
	if slices.Contains(h.recordOf(t).Reasons, upgradeReasonRuntimeMismatch) {
		t.Errorf("a correct rollback was recorded as a version mismatch: %v", h.recordOf(t).Reasons)
	}
}

// TestUpgradeReview702IncompletePromotionIsChecked: the installer's exit 3 is a promotion whose
// claim did not settle, so the runtime is in service and the pointer must name it. An update that
// ends that way still has to fail the run, and a pointer that does not name what it promoted is
// still a mismatch.
func TestUpgradeReview702IncompletePromotionIsChecked(t *testing.T) {
	t.Run("the pointer names what it promoted", func(t *testing.T) {
		h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true,
			produceRuntime: true, pointAtIt: true, installExit: 3})
		if code := h.run("--release-dir", h.release); code != upgradeExitUpdateFailed {
			t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitUpdateFailed, h.recordOf(t))
		}
		if got := h.recordOf(t).Reasons; slices.Contains(got, upgradeReasonRuntimeMismatch) {
			t.Errorf("the pointer names the runtime the update promoted, so there is no mismatch: %v", got)
		}
	})
	t.Run("the pointer names another runtime", func(t *testing.T) {
		h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true,
			produceRuntime: true, installExit: 3})
		if code := h.run("--release-dir", h.release); code != upgradeExitPostCheck {
			t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitPostCheck, h.recordOf(t))
		}
		if got := h.recordOf(t).Reason; got != upgradeReasonRuntimeMismatch {
			t.Errorf("reason %q, want %q", got, upgradeReasonRuntimeMismatch)
		}
		if got := h.recordOf(t).Reasons; !slices.Contains(got, upgradeReasonRuntimeMismatch) {
			t.Errorf("the pointer does not name the runtime the update promoted, so it is a mismatch: %v", got)
		}
	})
}

// TestUpgradeReview702ServiceAlreadyRunningIsNotAFailure: the runtime the update installed is
// already up when the restart runs, as it is when something started it between the stop and the
// restart. Its start answers already_running rather than launching a second one; that is the runtime
// the service is on, not a failed launch to fall back from, so the run must not report the healthy
// new runtime as a mismatch.
func TestUpgradeReview702ServiceAlreadyRunningIsNotAFailure(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true,
		produceRuntime: true, pointAtIt: true, installedStartAlreadyRunning: true})
	if code := h.run("--release-dir", h.release); code != 0 {
		t.Fatalf("exit %d, want 0; the service is up on the runtime the update installed: %+v", code, h.recordOf(t))
	}
	if got := h.recordOf(t).Reasons; slices.Contains(got, upgradeReasonRuntimeMismatch) {
		t.Errorf("a service already running on the installed runtime was recorded as a mismatch: %v", got)
	}
	if got := h.recordOf(t).StartFrom; got != h.installed {
		t.Errorf("the record says the service is on %q, want the installed runtime %q", got, h.installed)
	}
}

// TestUpgradeReview702PointerMovedToAThirdRuntimeIsAMismatch: an update that failed is a rollback
// only when the pointer still names the runtime it replaced. A pointer left on some other runtime
// was changed by something other than this run, and nothing here can show what is in service, so it
// is a mismatch rather than a rollback.
func TestUpgradeReview702PointerMovedToAThirdRuntimeIsAMismatch(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true,
		installExit: 1, pointAtOther: true})
	if code := h.run("--release-dir", h.release); code != upgradeExitPostCheck {
		t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitPostCheck, h.recordOf(t))
	}
	if got := h.recordOf(t).Reason; got != upgradeReasonRuntimeMismatch {
		t.Errorf("reason %q, want %q", got, upgradeReasonRuntimeMismatch)
	}
	if got := h.recordOf(t).Reasons; !slices.Contains(got, upgradeReasonRuntimeMismatch) {
		t.Errorf("a pointer on a third runtime was accepted as a rollback: %v", got)
	}
}

// TestUpgradeReview702PointerBrokenDuringTheWaitIsAMismatch: the install target is verified before
// the service wait and again after it. The wait can run for a minute, and another process on the
// host can move or remove the pointer while this run waits; the last status answer alone must not
// turn that into a success.
func TestUpgradeReview702PointerBrokenDuringTheWaitIsAMismatch(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true,
		produceRuntime: true, pointAtIt: true, breakPointerDuringWait: true,
		statusAnswers: []string{upgradeStatusUnknown, upgradeStatusSame}})
	if code := h.run("--release-dir", h.release); code != upgradeExitPostCheck {
		t.Fatalf("exit %d, want %d; the pointer was removed while the run waited: %+v", code, upgradeExitPostCheck, h.recordOf(t))
	}
	if got := h.recordOf(t).Reason; got != upgradeReasonRuntimeMismatch {
		t.Errorf("reason %q, want %q", got, upgradeReasonRuntimeMismatch)
	}
}

// TestUpgradeReview702ServedStoreOpenAttempts: the doctor says which store the relay service serves
// when discovery selected another directory. The selected directory's contents are not the served
// store's, so the served store must be asked for its own answer: a served store with open attempts
// refuses, and one that cannot be read is refused too rather than read as none.
func TestUpgradeReview702ServedStoreOpenAttempts(t *testing.T) {
	t.Run("the served store refuses", func(t *testing.T) {
		h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit),
			pointer: true, doctorServesAnotherStore: true, servedStoreOpenAttempts: 2})
		if code := h.run("--release-dir", h.release, "--dry-run"); code != upgradeExitOpenAttempts {
			t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitOpenAttempts, h.recordOf(t))
		}
		if got := h.recordOf(t).Reason; got != upgradeReasonOpenAttempts {
			t.Errorf("reason %q, want %q", got, upgradeReasonOpenAttempts)
		}
	})
	t.Run("the served store is read, not the selected one", func(t *testing.T) {
		h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit),
			pointer: true, doctorServesAnotherStore: true, servedStoreOpenAttempts: 0})
		if code := h.run("--release-dir", h.release, "--dry-run"); code != 0 {
			t.Fatalf("exit %d, want 0; the served store has nothing open: %+v", code, h.recordOf(t))
		}
		if !h.calledFrom(filepath.Join(h.previous, "bin", "codex-session-relay"), h.state+"-served") {
			t.Errorf("the served store was not asked for its own answer: %q", h.callLines())
		}
	})
}

// TestUpgradeReview702OversizedArchiveIsRefusedBeforeItIsCopied: the pre-verification copy reads at
// most the installer's own input bound. A release directory holding something larger than the
// archive is refused, and the partial copy is removed, so a file that is not the archive cannot
// fill the state disk the live relay shares before the checksum even runs.
func TestUpgradeReview702OversizedArchiveIsRefusedBeforeItIsCopied(t *testing.T) {
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true})
	old := upgradeMaxArchiveBytes
	upgradeMaxArchiveBytes = 8
	t.Cleanup(func() { upgradeMaxArchiveBytes = old })
	if code := h.run("--release-dir", h.release, "--dry-run"); code != upgradeExitRefused {
		t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitRefused, h.recordOf(t))
	}
	if got := h.recordOf(t).Reason; got != upgradeReasonSumsFailed {
		t.Errorf("reason %q, want %q", got, upgradeReasonSumsFailed)
	}
	record := h.recordOf(t)
	for _, name := range []string{upgradeArchiveName, upgradeSumsName} {
		if _, err := os.Stat(filepath.Join(record.Directory, name)); err == nil {
			t.Errorf("the refused copy left %s behind", name)
		}
	}
}

// TestUpgradeReview702ServiceWaitBudgetIsRecorded: a service that never reports itself running and
// matching is a post-check failure after the real budget, and the wait records the last answer it
// read. The budget is shrunk here so the test spends it rather than the wall clock's 60 seconds. The
// budget is far longer than one status read by a shell child, even on a loaded host, so the test
// does not depend on how fast a single read returns; the product's own budget is the default the run reads.
func TestUpgradeReview702ServiceWaitBudgetIsRecorded(t *testing.T) {
	oldBudget, oldInterval := upgradeServiceBudget, upgradeServiceInterval
	upgradeServiceBudget, upgradeServiceInterval = 2*time.Second, 50*time.Millisecond
	t.Cleanup(func() { upgradeServiceBudget, upgradeServiceInterval = oldBudget, oldInterval })
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true,
		produceRuntime: true, pointAtIt: true, statusAnswers: []string{upgradeStatusUnknown}})
	if code := h.run("--release-dir", h.release); code != upgradeExitPostCheck {
		t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitPostCheck, h.recordOf(t))
	}
	if got := h.recordOf(t).Reason; got != upgradeReasonPostCheck {
		t.Errorf("reason %q, want %q", got, upgradeReasonPostCheck)
	}
	recorded := ""
	for _, step := range h.recordOf(t).Steps {
		if step.Step == upgradeStepPostCheck && strings.Contains(step.Output, "matchesRunning") {
			recorded = step.Output
		}
	}
	if !strings.Contains(recorded, "unknown") {
		t.Errorf("the wait did not record its last status answer: %q", recorded)
	}
}

// TestUpgradeReview702ServiceWaitExhaustionOutranksUpdateFailure: a service that never reports itself
// running and matching is the host's wait ending, which outranks the update's failure. The run exits
// 4 with postcheck_failed, and the record still names the update failure among its reasons.
func TestUpgradeReview702ServiceWaitExhaustionOutranksUpdateFailure(t *testing.T) {
	oldBudget, oldInterval := upgradeServiceBudget, upgradeServiceInterval
	upgradeServiceBudget, upgradeServiceInterval = 50*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { upgradeServiceBudget, upgradeServiceInterval = oldBudget, oldInterval })
	h := upgradeHarness(t, upgradeHarnessOptions{gh: upgradeGhPaths(upgradeGoodCommit), pointer: true,
		installExit: 1, statusAnswers: []string{upgradeStatusUnknown}})
	if code := h.run("--release-dir", h.release); code != upgradeExitPostCheck {
		t.Fatalf("exit %d, want %d; the record is %+v", code, upgradeExitPostCheck, h.recordOf(t))
	}
	if got := h.recordOf(t).Reason; got != upgradeReasonPostCheck {
		t.Errorf("reason %q, want %q", got, upgradeReasonPostCheck)
	}
	if reasons := upgradeReview702Reasons(t, h); !slices.Contains(reasons, upgradeReasonUpdateFailed) {
		t.Errorf("the record does not name %q: %v", upgradeReasonUpdateFailed, reasons)
	}
}
