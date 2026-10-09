package configguard

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// CRW-1150: the report-only SessionStart probe shares one short deadline, and an explicit command's
// verified listing, with the codex version and the config digest it was measured against, spares
// the repeated listing without ever silencing the notice.

// selfHealEvidenceRunner answers `--version` and `features list` and records every call.
type selfHealEvidenceRunner struct {
	version string
	listing string
	calls   [][]string
}

func (r *selfHealEvidenceRunner) run(args []string) CodexRunResult {
	r.calls = append(r.calls, append([]string(nil), args...))
	switch {
	case reflect.DeepEqual(args, []string{"--version"}):
		return CodexRunResult{Stdout: r.version + "\n"}
	case reflect.DeepEqual(args, []string{"features", "list"}):
		return CodexRunResult{Stdout: r.listing}
	}
	return CodexRunResult{ExitCode: 1}
}

func (r *selfHealEvidenceRunner) listings() int {
	n := 0
	for _, call := range r.calls {
		if reflect.DeepEqual(call, []string{"features", "list"}) {
			n++
		}
	}
	return n
}

// selfHealEvidenceCwd is the project directory (no config layer of its own) a test's codex runs in:
// next to the CODEX_HOME, inside the test's temporary tree.
func selfHealEvidenceCwd(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(home), "project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func selfHealEvidenceRecord(t *testing.T, home string, runner *selfHealEvidenceRunner) {
	t.Helper()
	selfHealEvidenceRecordIn(t, home, selfHealEvidenceCwd(t, home), runner)
}

func selfHealEvidenceRecordIn(t *testing.T, home, cwd string, runner *selfHealEvidenceRunner) {
	t.Helper()
	if err := RecordSelfHealEvidence(RecordSelfHealEvidenceDeps{CodexHome: home, Cwd: cwd, Run: runner.run, Now: func() string { return "2026-10-10T00:00:00.000Z" }}); err != nil {
		t.Fatal(err)
	}
}

func selfHealEvidenceWriteProjectConfig(t *testing.T, cwd, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cwd, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".codex", "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSelfHealEvidenceAllOnIsReusedWithoutAListing(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	runner := &selfHealEvidenceRunner{version: "codex-cli 1.2.3", listing: selfHealReportSoftOn}
	selfHealEvidenceRecord(t, home, runner)
	if runner.listings() != 1 {
		t.Fatalf("recording measured %d times, want once: %v", runner.listings(), runner.calls)
	}
	runner.calls = nil
	before := selfHealReportListing(t, home)
	for i := 0; i < 3; i++ {
		outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Cwd: selfHealEvidenceCwd(t, home), Run: runner.run})
		if len(outcomes) != 1 || outcomes[0].Action != SelfHealReportSkipped || outcomes[0].Reason != SelfHealReasonAlreadyEnabled {
			t.Fatalf("round %d: %+v", i, outcomes)
		}
	}
	if runner.listings() != 0 {
		t.Fatalf("repeated rounds listed %d times: %v", runner.listings(), runner.calls)
	}
	if after := selfHealReportListing(t, home); !reflect.DeepEqual(before, after) {
		t.Fatalf("the rounds wrote into CODEX_HOME:\n before %v\n after  %v", before, after)
	}
}

// The recorded SessionStart input run again and again through the real hook entry and a fake codex
// on PATH: the soft flag that is off is announced every session, and the listing is not repeated.
func TestSelfHealEvidenceOffStillWarnsEverySession(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	dir := selfHealReportFakeCodexAt(t)
	log := filepath.Join(dir, "calls.log")
	selfHealReportWriteFakeCodex(t, dir, "printf '%s\\n' \"$*\" >> \""+log+"\"\n"+
		"if [ \"$1\" = --version ]; then echo 'codex-cli 1.2.3'; exit 0; fi\n"+
		"if [ \"$1\" = features ] && [ \"$2\" = list ]; then printf '%s' '"+selfHealReportSoftOff+"'; exit 0; fi\n"+
		"exit 1\n")
	runner := &selfHealEvidenceRunner{version: "codex-cli 1.2.3", listing: selfHealReportSoftOff}
	selfHealEvidenceRecord(t, home, runner)
	before := selfHealReportListing(t, home)
	t.Chdir(selfHealEvidenceCwd(t, home))
	for i := 0; i < 3; i++ {
		out, code := selfHealReportRun(t, home, selfHealReportSessionStart)
		if code != 0 || !strings.HasPrefix(out, selfHealReportEnvelopePrefix) || !strings.Contains(out, "default_mode_request_user_input") {
			t.Fatalf("session %d: exit %d stdout %q", i, code, out)
		}
	}
	for _, call := range selfHealReportCalls(t, log) {
		if call != "--version" {
			t.Fatalf("a repeated session ran %q; only the version is read", call)
		}
	}
	if after := selfHealReportListing(t, home); !reflect.DeepEqual(before, after) {
		t.Fatalf("the sessions wrote into CODEX_HOME:\n before %v\n after  %v", before, after)
	}
	// A new codex version is diagnosed afresh, and an all-on listing is silent.
	if err := os.Remove(filepath.Join(dir, "codex")); err != nil {
		t.Fatal(err)
	}
	selfHealReportWriteFakeCodex(t, dir, "printf '%s\\n' \"$*\" >> \""+log+"\"\n"+
		"if [ \"$1\" = --version ]; then echo 'codex-cli 1.2.4'; exit 0; fi\n"+
		"if [ \"$1\" = features ] && [ \"$2\" = list ]; then printf '%s' '"+selfHealReportSoftOn+"'; exit 0; fi\n"+
		"exit 1\n")
	if out, code := selfHealReportRun(t, home, selfHealReportSessionStart); code != 0 || out != "" {
		t.Fatalf("exit %d stdout %q, want the fresh all-on listing to be silent", code, out)
	}
	calls := selfHealReportCalls(t, log)
	if calls[len(calls)-1] != "features list" {
		t.Fatalf("a new codex version was not diagnosed afresh: %v", calls)
	}
}

func TestSelfHealEvidenceChangedConfigOrVersionMeasuresAgain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, home string, r *selfHealEvidenceRunner)
	}{
		{"config bytes", func(t *testing.T, home string, r *selfHealEvidenceRunner) {
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[features]\nhooks = false\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"config removed", func(t *testing.T, home string, r *selfHealEvidenceRunner) {
			if err := os.Remove(filepath.Join(home, "config.toml")); err != nil {
				t.Fatal(err)
			}
		}},
		{"codex version", func(t *testing.T, home string, r *selfHealEvidenceRunner) { r.version = "codex-cli 1.2.4" }},
		{"version unreadable", func(t *testing.T, home string, r *selfHealEvidenceRunner) { r.version = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := selfHealReportTempHome(t)
			selfHealReportWriteConfig(t, home)
			runner := &selfHealEvidenceRunner{version: "codex-cli 1.2.3", listing: selfHealReportSoftOn}
			selfHealEvidenceRecord(t, home, runner)
			runner.calls = nil
			tc.change(t, home, runner)
			runner.listing = selfHealReportSoftOff
			outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Cwd: selfHealEvidenceCwd(t, home), Run: runner.run})
			if runner.listings() != 1 {
				t.Fatalf("listed %d times, want a fresh diagnosis: %v", runner.listings(), runner.calls)
			}
			if len(outcomes) != 1 || outcomes[0].Action != SelfHealReportOff {
				t.Fatalf("outcomes = %+v, want the fresh listing's off", outcomes)
			}
		})
	}
}

// An mtime is not evidence: touching config.toml without changing its bytes keeps the record, and a
// record whose digest does not match is dropped by a change that keeps the mtime.
func TestSelfHealEvidenceIsTheDigestNotTheMtime(t *testing.T) {
	home := selfHealReportTempHome(t)
	path := selfHealReportWriteConfig(t, home)
	runner := &selfHealEvidenceRunner{version: "codex-cli 1.2.3", listing: selfHealReportSoftOn}
	selfHealEvidenceRecord(t, home, runner)
	runner.calls = nil
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("model = \"gpt-5.5\"\n\n[features]\nhooks = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stat.ModTime(), stat.ModTime()); err != nil {
		t.Fatal(err)
	}
	runner.listing = selfHealReportSoftOff
	outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Cwd: selfHealEvidenceCwd(t, home), Run: runner.run})
	if runner.listings() != 1 || len(outcomes) != 1 || outcomes[0].Action != SelfHealReportOff {
		t.Fatalf("a changed config with an unchanged mtime kept the record: %+v %v", outcomes, runner.calls)
	}
}

func TestSelfHealEvidenceOptOutAndDeclineStayQuiet(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	runner := &selfHealEvidenceRunner{version: "codex-cli 1.2.3", listing: selfHealReportSoftOff}
	selfHealEvidenceRecord(t, home, runner)
	runner.calls = nil

	marker, err := ReadSelfHealMarkerFile(home)
	if err != nil || marker == nil || marker.Probe == nil {
		t.Fatalf("marker %+v %v", marker, err)
	}
	marker.HealedKeys = []string{"default_mode_request_user_input"}
	if err := WriteSelfHealMarkerFile(home, marker); err != nil {
		t.Fatal(err)
	}
	outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Cwd: selfHealEvidenceCwd(t, home), Run: runner.run})
	if len(outcomes) != 1 || outcomes[0].Action != SelfHealReportDeclined || RenderSelfHealReportContext(outcomes) != "" {
		t.Fatalf("declined: %+v", outcomes)
	}

	marker.HealedKeys = nil
	if err := WriteSelfHealMarkerFile(home, marker); err != nil {
		t.Fatal(err)
	}
	if err := MarkSelfHealOptedOut(home, "2026-10-10T00:00:00.000Z"); err != nil {
		t.Fatal(err)
	}
	runner.calls = nil
	outcomes = SelfHealReport(SelfHealReportDeps{CodexHome: home, Cwd: selfHealEvidenceCwd(t, home), Run: runner.run})
	if len(runner.calls) != 0 || len(outcomes) != 1 || outcomes[0].Reason != SelfHealReasonOptedOut {
		t.Fatalf("opted out: %+v %v", outcomes, runner.calls)
	}
	after, err := ReadSelfHealMarkerFile(home)
	if err != nil || after == nil || after.Probe != nil {
		t.Fatalf("an opt-out kept the evidence of flags it reverts: %+v %v", after, err)
	}
}

func TestSelfHealEvidenceRecordingKeepsConsentFieldsAndFailureDropsIt(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	selfHealReportWriteMarker(t, home, "{\"checkedAt\":\"2025-12-31T00:00:00.000Z\",\"allEnabled\":false,\"healedKeys\":[\"hooks\"],\"cachedKeys\":[\"goals\"],\"futureField\":1}\n")
	runner := &selfHealEvidenceRunner{version: "codex-cli 1.2.3", listing: selfHealReportSoftOn}
	selfHealEvidenceRecord(t, home, runner)
	marker, err := ReadSelfHealMarkerFile(home)
	if err != nil || marker == nil || marker.Probe == nil {
		t.Fatalf("marker %+v %v", marker, err)
	}
	if !reflect.DeepEqual(marker.HealedKeys, []string{"hooks"}) || !reflect.DeepEqual(marker.CachedKeys, []string{"goals"}) || marker.AllEnabled == nil || *marker.AllEnabled {
		t.Fatalf("consent fields lost: %+v", marker)
	}
	if marker.Probe.CodexVersion != "codex-cli 1.2.3" || !marker.Probe.Features["goals"] || marker.Probe.RecordedAt != "2026-10-10T00:00:00.000Z" {
		t.Fatalf("evidence %+v", marker.Probe)
	}
	// A measurement that cannot be made removes the older record.
	failing := &selfHealEvidenceRunner{version: "", listing: selfHealReportSoftOn}
	selfHealEvidenceRecord(t, home, failing)
	if marker, err = ReadSelfHealMarkerFile(home); err != nil || marker == nil || marker.Probe != nil {
		t.Fatalf("failed measurement kept the record: %+v %v", marker, err)
	}
	// ...and with no marker at all it writes none.
	empty := selfHealReportTempHome(t)
	selfHealEvidenceRecord(t, empty, failing)
	if _, err := os.Stat(SelfHealMarkerPath(empty)); !os.IsNotExist(err) {
		t.Fatalf("a failed measurement created a marker: %v", err)
	}
}

func TestSelfHealEvidenceConfigChangedWhileMeasuringIsNotRecorded(t *testing.T) {
	home := selfHealReportTempHome(t)
	path := selfHealReportWriteConfig(t, home)
	racing := &selfHealEvidenceRunner{version: "codex-cli 1.2.3", listing: selfHealReportSoftOn}
	run := func(args []string) CodexRunResult {
		if len(args) == 2 {
			if err := os.WriteFile(path, []byte("[features]\nhooks = false\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return racing.run(args)
	}
	if err := RecordSelfHealEvidence(RecordSelfHealEvidenceDeps{CodexHome: home, Cwd: selfHealEvidenceCwd(t, home), Run: run}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(SelfHealMarkerPath(home)); !os.IsNotExist(err) {
		t.Fatalf("evidence for a config that moved under the measurement was recorded: %v", err)
	}
}

// A stalled probe whose child holds the listing open ends inside the hook's 20 second limit (K1)
// and says nothing. Before CRW-1150 the hook waited for the whole listing: a 30 second child ran
// the hook 30 seconds.
func TestSelfHealReportStalledProbeEndsInsideTheHookLimit(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	dir := selfHealReportFakeCodexAt(t)
	selfHealReportWriteFakeCodex(t, dir, "sleep 30 &\nsleep 30\n")

	start := time.Now()
	out, code := selfHealReportRun(t, home, selfHealReportSessionStart)
	elapsed := time.Since(start)
	if code != 0 || out != "" {
		t.Fatalf("exit %d stdout %q, want silence", code, out)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("the hook ran %v, want it inside the 20 second hook limit with margin", elapsed)
	}
	if got := selfHealReportProbeDeadline + selfHealReportWaitDelay; got > 12*time.Second {
		t.Fatalf("deadline + pipe cleanup = %v, want at most 12s under the 20s hook limit", got)
	}
}

// A grandchild that left the probe's process group and still holds the pipe cannot hold the hook
// past the shared deadline plus the pipe cleanup.
func TestSelfHealReportEscapedGrandchildEndsAtTheSharedDeadline(t *testing.T) {
	prevDeadline, prevWait := selfHealReportProbeDeadline, selfHealReportWaitDelay
	selfHealReportProbeDeadline, selfHealReportWaitDelay = 400*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { selfHealReportProbeDeadline, selfHealReportWaitDelay = prevDeadline, prevWait })
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	dir := selfHealReportFakeCodexAt(t)
	selfHealReportWriteFakeCodex(t, dir, "setsid sleep 30 &\nsleep 30\n")

	start := time.Now()
	out, code := selfHealReportRun(t, home, selfHealReportSessionStart)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the hook ran %v past a 400ms deadline", elapsed)
	}
	if code != 0 || out != "" {
		t.Fatalf("exit %d stdout %q, want silence", code, out)
	}
}

// CRW-1150 verification round 1: the evidence describes the effective configuration, not only the
// user's config.toml. A project layer that changes a soft flag, a different project, and a working
// directory the hook cannot identify all give a fresh listing.
func TestSelfHealEvidenceProjectConfigChangeMeasuresAgain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, project string)
	}{
		{"project layer appears", func(t *testing.T, project string) {
			selfHealEvidenceWriteProjectConfig(t, project, "[features]\ndefault_mode_request_user_input = false\n")
		}},
		{"project layer edited", func(t *testing.T, project string) {
			selfHealEvidenceWriteProjectConfig(t, project, "[features]\ndefault_mode_request_user_input = false\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := selfHealReportTempHome(t)
			selfHealReportWriteConfig(t, home)
			project := selfHealEvidenceCwd(t, home)
			if tc.name == "project layer edited" {
				selfHealEvidenceWriteProjectConfig(t, project, "[features]\ndefault_mode_request_user_input = true\n")
			}
			runner := &selfHealEvidenceRunner{version: "codex-cli 1.2.3", listing: selfHealReportSoftOn}
			selfHealEvidenceRecord(t, home, runner)
			runner.calls = nil
			tc.change(t, project)
			runner.listing = selfHealReportSoftOff
			outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Cwd: project, Run: runner.run})
			if runner.listings() != 1 || len(outcomes) != 1 || outcomes[0].Action != SelfHealReportOff {
				t.Fatalf("a changed project layer kept the record: %+v %v", outcomes, runner.calls)
			}
		})
	}
}

// A session in a project that has its own layer is not described by the evidence another project's
// enable recorded, and one in an unlayered project still is.
func TestSelfHealEvidenceDifferentProjectMeasuresAgain(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	runner := &selfHealEvidenceRunner{version: "codex-cli 1.2.3", listing: selfHealReportSoftOn}
	selfHealEvidenceRecord(t, home, runner)
	runner.calls = nil

	other := filepath.Join(filepath.Dir(home), "other")
	selfHealEvidenceWriteProjectConfig(t, other, "[features]\ndefault_mode_request_user_input = false\n")
	runner.listing = selfHealReportSoftOff
	outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Cwd: other, Run: runner.run})
	if runner.listings() != 1 || len(outcomes) != 1 || outcomes[0].Action != SelfHealReportOff {
		t.Fatalf("another project's layer reused the record: %+v %v", outcomes, runner.calls)
	}

	runner.calls = nil
	plain := filepath.Join(filepath.Dir(home), "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	outcomes = SelfHealReport(SelfHealReportDeps{CodexHome: home, Cwd: plain, Run: runner.run})
	if runner.listings() != 0 || len(outcomes) != 1 || outcomes[0].Reason != SelfHealReasonAlreadyEnabled {
		t.Fatalf("an unlayered project did not reuse the record: %+v %v", outcomes, runner.calls)
	}
}

// A project layer in an ancestor of the working directory applies to it as well.
func TestSelfHealEvidenceAncestorProjectLayerMeasuresAgain(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	project := selfHealEvidenceCwd(t, home)
	nested := filepath.Join(project, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &selfHealEvidenceRunner{version: "codex-cli 1.2.3", listing: selfHealReportSoftOn}
	selfHealEvidenceRecordIn(t, home, nested, runner)
	runner.calls = nil
	selfHealEvidenceWriteProjectConfig(t, project, "[features]\ndefault_mode_request_user_input = false\n")
	runner.listing = selfHealReportSoftOff
	outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Cwd: nested, Run: runner.run})
	if runner.listings() != 1 || len(outcomes) != 1 || outcomes[0].Action != SelfHealReportOff {
		t.Fatalf("an ancestor layer kept the record: %+v %v", outcomes, runner.calls)
	}
}

// Without a working directory the layers cannot be named: nothing is recorded, nothing is reused.
func TestSelfHealEvidenceUnknownWorkingDirectoryIsNeverReused(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	runner := &selfHealEvidenceRunner{version: "codex-cli 1.2.3", listing: selfHealReportSoftOn}
	selfHealEvidenceRecordIn(t, home, "", runner)
	if marker, err := ReadSelfHealMarkerFile(home); err != nil || marker != nil {
		t.Fatalf("evidence was recorded without a working directory: %+v %v", marker, err)
	}
	selfHealEvidenceRecord(t, home, runner)
	runner.calls = nil
	outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Run: runner.run})
	if runner.listings() != 1 || len(outcomes) != 1 {
		t.Fatalf("a round with no working directory reused the record: %+v %v", outcomes, runner.calls)
	}
}

// The explicit command's off finding survives an older mtime-cache marker: the cache must not vouch
// ahead of the evidence, before or after the codex version or the config moves.
func TestSelfHealEvidenceLegacyCacheDoesNotOutrankTheEvidence(t *testing.T) {
	home := selfHealReportTempHome(t)
	path := selfHealReportWriteConfig(t, home)
	selfHealReportWriteMarker(t, home, "{\"checkedAt\":\"2025-12-31T00:00:00.000Z\",\"allEnabled\":true,"+
		"\"cachedKeys\":[\"default_mode_request_user_input\",\"goals\"],\"configMtimeMs\":"+selfHealReportMarkerMtimeMs(t, path)+"}\n")
	runner := &selfHealEvidenceRunner{version: "codex-cli 1.2.3", listing: selfHealReportSoftOff}
	selfHealEvidenceRecord(t, home, runner)
	marker, err := ReadSelfHealMarkerFile(home)
	// Recording the evidence retires the legacy cache (allEnabled false is the oracle's cache miss).
	if err != nil || marker == nil || marker.Probe == nil || marker.AllEnabled == nil || *marker.AllEnabled {
		t.Fatalf("setup: %+v %v", marker, err)
	}
	runner.calls = nil
	// The recorded off state is announced, with no listing, in spite of the legacy all-on cache.
	outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Cwd: selfHealEvidenceCwd(t, home), Run: runner.run})
	if runner.listings() != 0 || len(outcomes) == 0 || outcomes[0].Action != SelfHealReportOff {
		t.Fatalf("the legacy cache hid the recorded off state: %+v %v", outcomes, runner.calls)
	}
	// A new codex version is diagnosed afresh, not cached.
	runner.version, runner.listing, runner.calls = "codex-cli 1.2.4", selfHealReportSoftOff, nil
	outcomes = SelfHealReport(SelfHealReportDeps{CodexHome: home, Cwd: selfHealEvidenceCwd(t, home), Run: runner.run})
	if runner.listings() != 1 || len(outcomes) == 0 || outcomes[0].Action != SelfHealReportOff {
		t.Fatalf("the legacy cache hid a version change: %+v %v", outcomes, runner.calls)
	}
}

// CRW-1150 verification round 2: a layer that is a FIFO (or any file that is not a regular file)
// is never read. Before the fix the fingerprint opened it with a plain read and the hook waited for
// a writer past its own deadline and the K1 limit; now the layer counts as unreadable, the record is
// not reused and the round ends inside the shared deadline.
func TestSelfHealEvidenceFifoLayerHonorsTheDeadline(t *testing.T) {
	prevDeadline, prevWait := selfHealReportProbeDeadline, selfHealReportWaitDelay
	selfHealReportProbeDeadline, selfHealReportWaitDelay = 300*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { selfHealReportProbeDeadline, selfHealReportWaitDelay = prevDeadline, prevWait })
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	project := selfHealEvidenceCwd(t, home)
	selfHealEvidenceWriteProjectConfig(t, project, "[features]\nhooks = true\n")
	runner := &selfHealEvidenceRunner{version: "codex-cli 1.2.3", listing: selfHealReportSoftOn}
	selfHealEvidenceRecord(t, home, runner)
	layer := filepath.Join(project, ".codex", "config.toml")
	if err := os.Remove(layer); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(layer, 0o600); err != nil {
		t.Fatal(err)
	}
	// A reader the fix failed to avoid is released at the end, so the package does not hang.
	t.Cleanup(func() {
		if f, err := os.OpenFile(layer, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	})
	dir := selfHealReportFakeCodexAt(t)
	log := filepath.Join(dir, "calls.log")
	selfHealReportWriteFakeCodex(t, dir, "printf '%s\\n' \"$*\" >> \""+log+"\"\n"+
		"if [ \"$1\" = --version ]; then echo 'codex-cli 1.2.3'; exit 0; fi\n"+
		"if [ \"$1\" = features ] && [ \"$2\" = list ]; then printf '%s' '"+selfHealReportSoftOn+"'; exit 0; fi\n"+
		"exit 1\n")
	t.Chdir(project)
	type result struct {
		out  string
		code int
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		var out strings.Builder
		code := RunSelfHealReportHook(context.Background(), strings.NewReader(selfHealReportSessionStart), &out, selfHealReportEnv(home))
		done <- result{out.String(), code}
	}()
	select {
	case r := <-done:
		if r.code != 0 || r.out != "" {
			t.Fatalf("exit %d stdout %q, want silence", r.code, r.out)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the hook still ran %v after a 300ms deadline: a FIFO layer blocked the fingerprint", time.Since(start))
	}
	calls := selfHealReportCalls(t, log)
	if len(calls) == 0 || calls[len(calls)-1] != "features list" {
		t.Fatalf("an unreadable layer reused the record instead of listing: %v", calls)
	}
}

// A layer or a config.toml that names a device or a FIFO is an error of the fingerprint, returned at
// once, never a read.
func TestSelfHealEvidenceFingerprintRefusesFilesThatAreNotRegular(t *testing.T) {
	home := selfHealReportTempHome(t)
	project := selfHealEvidenceCwd(t, home)
	if err := os.MkdirAll(filepath.Join(project, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	layer := filepath.Join(project, ".codex", "config.toml")
	if err := os.Symlink("/dev/zero", layer); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := selfHealLayersDigest(home, project)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a device layer was fingerprinted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the fingerprint read a device layer without end")
	}
	fifo := filepath.Join(home, "config.toml")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	})
	go func() {
		_, err := selfHealConfigDigest(home)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO config.toml was fingerprinted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the config digest waited on a FIFO config.toml")
	}
}

// A working directory reached through a symbolic link is the physical directory codex runs in
// (getcwd); the layers of its physical ancestors apply, and a change there gives a fresh listing
// even while the hook's logical PWD stays the same.
func TestSelfHealEvidenceSymlinkPhysicalAncestorChangeMeasuresAgain(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	root := filepath.Dir(home)
	physical := filepath.Join(root, "physical", "parent", "project")
	if err := os.MkdirAll(physical, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "logical"), 0o755); err != nil {
		t.Fatal(err)
	}
	logical := filepath.Join(root, "logical", "link")
	if err := os.Symlink(physical, logical); err != nil {
		t.Fatal(err)
	}
	runner := &selfHealEvidenceRunner{version: "codex-cli 1.2.3", listing: selfHealReportSoftOn}
	selfHealEvidenceRecordIn(t, home, logical, runner)
	if marker, err := ReadSelfHealMarkerFile(home); err != nil || marker == nil || marker.Probe == nil {
		t.Fatalf("setup: %+v %v", marker, err)
	}
	selfHealEvidenceWriteProjectConfig(t, filepath.Join(root, "physical", "parent"), "[features]\ndefault_mode_request_user_input = false\n")

	runner.calls, runner.listing = nil, selfHealReportSoftOff
	outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Cwd: logical, Run: runner.run})
	if runner.listings() != 1 || len(outcomes) != 1 || outcomes[0].Action != SelfHealReportOff {
		t.Fatalf("a physical ancestor layer change kept the record: %+v %v", outcomes, runner.calls)
	}

	// The real hook, with the logical PWD the shell keeps.
	dir := selfHealReportFakeCodexAt(t)
	log := filepath.Join(dir, "calls.log")
	selfHealReportWriteFakeCodex(t, dir, "printf '%s\\n' \"$*\" >> \""+log+"\"\n"+
		"if [ \"$1\" = --version ]; then echo 'codex-cli 1.2.3'; exit 0; fi\n"+
		"if [ \"$1\" = features ] && [ \"$2\" = list ]; then printf '%s' '"+selfHealReportSoftOff+"'; exit 0; fi\n"+
		"exit 1\n")
	t.Chdir(logical)
	t.Setenv("PWD", logical)
	if wd, err := os.Getwd(); err != nil || wd != logical {
		t.Fatalf("setup: the hook's working directory reads %q (%v), want the logical %q", wd, err, logical)
	}
	out, code := selfHealReportRun(t, home, selfHealReportSessionStart)
	if code != 0 || !strings.Contains(out, "default_mode_request_user_input") {
		t.Fatalf("exit %d stdout %q, want the fresh off notice", code, out)
	}
	calls := selfHealReportCalls(t, log)
	if len(calls) == 0 || calls[len(calls)-1] != "features list" {
		t.Fatalf("the hook reused the record over a changed physical ancestor layer: %v", calls)
	}
}

// A recording attempt that fails must not hand authority back to the legacy mtime cache: the
// dropped evidence leaves a marker whose allEnabled no longer vouches, so the next round lists.
func TestSelfHealEvidenceFailedRecordingCannotResurrectTheLegacyCache(t *testing.T) {
	home := selfHealReportTempHome(t)
	path := selfHealReportWriteConfig(t, home)
	selfHealReportWriteMarker(t, home, "{\"checkedAt\":\"2025-12-31T00:00:00.000Z\",\"allEnabled\":true,"+
		"\"cachedKeys\":[\"default_mode_request_user_input\",\"goals\"],\"configMtimeMs\":"+selfHealReportMarkerMtimeMs(t, path)+"}\n")
	runner := &selfHealEvidenceRunner{version: "codex-cli 1.2.3", listing: selfHealReportSoftOff}
	selfHealEvidenceRecord(t, home, runner)
	failing := &selfHealEvidenceRunner{version: "", listing: selfHealReportSoftOff}
	selfHealEvidenceRecord(t, home, failing)
	marker, err := ReadSelfHealMarkerFile(home)
	if err != nil || marker == nil || marker.Probe != nil {
		t.Fatalf("setup: the failed recording kept the record: %+v %v", marker, err)
	}
	runner.calls = nil
	outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Cwd: selfHealEvidenceCwd(t, home), Run: runner.run})
	if runner.listings() != 1 || len(outcomes) == 0 || outcomes[0].Action != SelfHealReportOff {
		t.Fatalf("the legacy cache vouched again after a failed recording: %+v %v", outcomes, runner.calls)
	}

	// A legacy-only marker whose first recording attempt already fails is retired as well: the
	// command changed the flags the cache described.
	fresh := selfHealReportTempHome(t)
	freshPath := selfHealReportWriteConfig(t, fresh)
	selfHealReportWriteMarker(t, fresh, "{\"allEnabled\":true,\"cachedKeys\":[\"default_mode_request_user_input\",\"goals\"],\"configMtimeMs\":"+
		selfHealReportMarkerMtimeMs(t, freshPath)+"}\n")
	selfHealEvidenceRecord(t, fresh, failing)
	runner.calls = nil
	outcomes = SelfHealReport(SelfHealReportDeps{CodexHome: fresh, Cwd: selfHealEvidenceCwd(t, fresh), Run: runner.run})
	if runner.listings() != 1 {
		t.Fatalf("a legacy cache survived a failed recording attempt: %+v %v", outcomes, runner.calls)
	}
}

// probeEvidence that does not parse (an older form without layersSha256, or a damaged record) is not
// the same as no probeEvidence at all: the legacy cache beside it does not vouch, and a marker that
// never carried probeEvidence keeps the oracle's cache hit.
func TestSelfHealEvidenceUnparsableRecordDoesNotRestoreTheLegacyCache(t *testing.T) {
	home := selfHealReportTempHome(t)
	path := selfHealReportWriteConfig(t, home)
	legacy := "\"allEnabled\":true,\"cachedKeys\":[\"default_mode_request_user_input\",\"goals\"],\"configMtimeMs\":" + selfHealReportMarkerMtimeMs(t, path)
	selfHealReportWriteMarker(t, home, "{"+legacy+",\"probeEvidence\":{\"codexVersion\":\"codex-cli 1.2.3\",\"configSha256\":\"x\","+
		"\"recordedAt\":\"2026-10-10T00:00:00.000Z\",\"features\":{\"default_mode_request_user_input\":false}}}\n")
	runner := &selfHealEvidenceRunner{version: "codex-cli 1.2.3", listing: selfHealReportSoftOff}
	outcomes := SelfHealReport(SelfHealReportDeps{CodexHome: home, Cwd: selfHealEvidenceCwd(t, home), Run: runner.run})
	if runner.listings() != 1 || len(outcomes) == 0 || outcomes[0].Action != SelfHealReportOff {
		t.Fatalf("the legacy cache vouched beside an unparsable record: %+v %v", outcomes, runner.calls)
	}
	// Rewriting such a marker (an opt-out cleared) does not hand the cache back its authority.
	if err := ClearSelfHealOptOut(home); err != nil {
		t.Fatal(err)
	}
	runner.calls = nil
	outcomes = SelfHealReport(SelfHealReportDeps{CodexHome: home, Cwd: selfHealEvidenceCwd(t, home), Run: runner.run})
	if runner.listings() != 1 {
		t.Fatalf("a rewrite restored the legacy cache: %+v %v", outcomes, runner.calls)
	}

	plain := selfHealReportTempHome(t)
	plainPath := selfHealReportWriteConfig(t, plain)
	selfHealReportWriteMarker(t, plain, "{\"allEnabled\":true,\"cachedKeys\":[\"default_mode_request_user_input\",\"goals\"],\"configMtimeMs\":"+
		selfHealReportMarkerMtimeMs(t, plainPath)+"}\n")
	runner.calls = nil
	outcomes = SelfHealReport(SelfHealReportDeps{CodexHome: plain, Cwd: selfHealEvidenceCwd(t, plain), Run: runner.run})
	if len(runner.calls) != 0 || len(outcomes) != 1 || outcomes[0].Reason != SelfHealReasonCached {
		t.Fatalf("a legacy-only marker lost the cache hit: %+v %v", outcomes, runner.calls)
	}
}
