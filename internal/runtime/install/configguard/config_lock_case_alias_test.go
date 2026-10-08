package configguard

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// CRW-993 c1b and d2 (CRW-899 evaluation d2): a case-only alias of the locked entry is accepted when the
// directory folds case and the by-name lookup through the held descriptor reaches the pinned file. A
// case-sensitive directory refuses, because a differently cased name is a different entry even when it
// is a hard link (E2). An unknown answer, or a lookup that fails, refuses with the reason.

// configLockPathsCaseAliasSeams answers the fold probe and, when lookup is set, the entry lookup, for one test.
func configLockPathsCaseAliasSeams(t *testing.T, fold configLockPathsFold, lookup func(*configLockPathsPin, string) (uint64, uint64, error)) {
	t.Helper()
	prevFold, prevLookup := configLockPathsFoldProbe, configLockPathsEntryLookup
	configLockPathsFoldProbe = func(*configLockPathsPin) configLockPathsFold { return fold }
	if lookup != nil {
		configLockPathsEntryLookup = lookup
	}
	t.Cleanup(func() {
		configLockPathsFoldProbe = prevFold
		configLockPathsEntryLookup = prevLookup
	})
}

// configLockPathsCaseAliasSameEntry answers the lookup with the identity of the config file at cfg, which is
// the pinned entry while the test runs.
func configLockPathsCaseAliasSameEntry(t *testing.T, cfg string) func(*configLockPathsPin, string) (uint64, uint64, error) {
	t.Helper()
	return func(_ *configLockPathsPin, name string) (uint64, uint64, error) {
		if name != "CONFIG.TOML" {
			t.Errorf("the lookup named %q, want the manifest's spelling CONFIG.TOML", name)
		}
		info, err := os.Stat(cfg)
		if err != nil {
			t.Fatal(err)
		}
		dev, ino, ok := configLockPathsIdentity(info)
		if !ok {
			t.Fatal("no device and inode for the config file")
		}
		return dev, ino, nil
	}
}

// configLockPathsCaseAliasRun deactivates a config.toml whose directory loses read permission after the pin,
// while the second manifest names the same entry as CONFIG.TOML. seams runs after config.toml exists and
// before the deactivation, so a test can answer the probe for it. When hardLink is set, a hard link
// named CONFIG.TOML exists as well. It returns the config path and the deactivation's error.
func configLockPathsCaseAliasRun(t *testing.T, hardLink bool, seams func(cfg string)) (string, error) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0300 directory")
	}
	home := configLockActivationHome(t)
	dir := filepath.Join(home, "locked")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	cfg := filepath.Join(dir, "config.toml")
	activationWrite(t, cfg, deactivationConfig)
	if hardLink {
		configLockPathsRequiresCaseSensitive(t, dir)
		if err := os.Link(cfg, filepath.Join(dir, "CONFIG.TOML")); err != nil {
			t.Skipf("this filesystem does not support hard links: %v", err)
		}
	}
	if seams != nil {
		seams(cfg)
	}
	hash, err := hashOrNull(cfg)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}
	stale := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: cfg, PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})
	fresh := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: filepath.Join(dir, "CONFIG.TOML"), PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})
	held := configLockWritersHold(t, cfg)
	configLockPathsHandoverRetarget(t, home, stale, func() error {
		return os.Chmod(dir, 0o300)
	}, held.Release, fresh)
	_, err = Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
	if cerr := os.Chmod(dir, 0o700); cerr != nil {
		t.Fatal(cerr)
	}
	return cfg, err
}

// The d2 case: a folding directory with no read permission accepts the case-only alias when the by-name
// lookup reaches the pinned file, and restores the key through the locked path.
func TestConfigLockCaseAliasFoldingDirectoryIsRestored(t *testing.T) {
	var cfgPath string
	cfg, err := configLockPathsCaseAliasRun(t, false, func(cfg string) {
		cfgPath = cfg
		configLockPathsCaseAliasSeams(t, configLockPathsFoldFolds, configLockPathsCaseAliasSameEntry(t, cfg))
	})
	if err != nil {
		t.Fatalf("a case-only alias of the locked entry was refused in a folding directory: %v", err)
	}
	if cfgPath != cfg {
		t.Fatalf("the test read another config path")
	}
	if got := activationRead(t, cfg); strings.Contains(got, "dedicated_tools") {
		t.Fatalf("the case-only alias was accepted but the key was not restored: %q", got)
	}
}

// The d2 refusal: a lookup that reaches a different entry is refused and the key stays.
func TestConfigLockCaseAliasFoldingDirectoryRefusesADifferentEntry(t *testing.T) {
	cfg, err := configLockPathsCaseAliasRun(t, false, func(cfg string) {
		configLockPathsCaseAliasSeams(t, configLockPathsFoldFolds, func(*configLockPathsPin, string) (uint64, uint64, error) {
			return 1, 1, nil
		})
	})
	if err == nil || !strings.Contains(err.Error(), "different config file") {
		t.Fatalf("a lookup that reached another entry was accepted: %v", err)
	}
	if got := activationRead(t, cfg); !strings.Contains(got, "dedicated_tools") {
		t.Fatalf("the refused deactivation changed the config: %q", got)
	}
}

// The d2 reason: a lookup that fails (EACCES, a directory that is not searchable) refuses with the reason.
func TestConfigLockCaseAliasLookupFailureNamesTheReason(t *testing.T) {
	cfg, err := configLockPathsCaseAliasRun(t, false, func(cfg string) {
		configLockPathsCaseAliasSeams(t, configLockPathsFoldFolds, func(*configLockPathsPin, string) (uint64, uint64, error) {
			return 0, 0, syscall.EACCES
		})
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be looked up") || !strings.Contains(err.Error(), "cannot be proven") {
		t.Fatalf("a failed lookup did not name its reason: %v", err)
	}
	if got := activationRead(t, cfg); !strings.Contains(got, "dedicated_tools") {
		t.Fatalf("the refused deactivation changed the config: %q", got)
	}
}

// The d2 unknown answer: a directory whose case behaviour cannot be probed refuses with the reason.
func TestConfigLockCaseAliasUnknownFoldNamesTheReason(t *testing.T) {
	cfg, err := configLockPathsCaseAliasRun(t, false, func(cfg string) {
		configLockPathsCaseAliasSeams(t, configLockPathsFoldUnknown, configLockPathsCaseAliasSameEntry(t, cfg))
	})
	if err == nil || !strings.Contains(err.Error(), "does not answer whether it folds case") {
		t.Fatalf("an unknown case answer did not name its reason: %v", err)
	}
	if got := activationRead(t, cfg); !strings.Contains(got, "dedicated_tools") {
		t.Fatalf("the refused deactivation changed the config: %q", got)
	}
}

// The E2 case on the real filesystem: a case-sensitive directory refuses a case-variant hard link even
// with no read permission, because the real probe answers case-sensitive here.
func TestConfigLockCaseAliasCaseSensitiveRefusesAHardLinkUnreadable(t *testing.T) {
	cfg, err := configLockPathsCaseAliasRun(t, true, nil)
	if err == nil || !strings.Contains(err.Error(), "different config file") {
		t.Fatalf("a case-variant hard link was accepted in a case-sensitive directory: %v", err)
	}
	if got := activationRead(t, cfg); !strings.Contains(got, "dedicated_tools") {
		t.Fatalf("the refused deactivation changed the config: %q", got)
	}
}

// The real probe answers case-sensitive for this host's temporary directory.
func TestConfigLockCaseAliasRealProbeAnswersCaseSensitiveHere(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	activationWrite(t, cfg, deactivationConfig)
	pin := configLockPathsTestPin(t, cfg)
	if configLockPathsProbeFold(pin) == configLockPathsFoldFolds {
		t.Skip("this filesystem folds case")
	}
	if got := configLockPathsProbeFold(pin); got != configLockPathsFoldSensitive {
		t.Fatalf("the probe answered %v for a case-sensitive directory", got)
	}
}

// The probe swaps ASCII letters only, and a name with none is unknown.
func TestConfigLockCaseAliasSwapsOnlyASCIILetters(t *testing.T) {
	if got, ok := configLockPathsCaseSwap("config.toml.crw-lock"); !ok || got != "CONFIG.TOML.CRW-LOCK" {
		t.Fatalf("swap of the sidecar name = %q, %v", got, ok)
	}
	if _, ok := configLockPathsCaseSwap("1.2-3"); ok {
		t.Fatal("a name with no ASCII letter reported letters")
	}
}

// The record of the case-only alias matches the probe: CRW-899.md no longer claims the enumeration-only
// refusal of an unreadable parent, and CRW-993.md states the one exception to the same-file promise.
func TestConfigLockCaseAliasRecordMatchesTheProbe(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	old := read("../../../../docs/port-cxc/known-defects/CRW-899.md")
	for _, stale := range []string{"refuses there", "refuses whenever the parent cannot be enumerated", "TestConfigLockPathsDeactivateRefusesACaseVariantUnderAnUnreadableParent"} {
		if strings.Contains(old, stale) {
			t.Errorf("CRW-899.md still says %q", stale)
		}
	}
	rec := read("../../../../docs/port-cxc/known-defects/CRW-993.md")
	if !strings.Contains(rec, "The one exception to the same-file restore promise") {
		t.Error("CRW-993.md does not state the exception to the same-file restore promise")
	}
}

// CRW-993 d4: a directory that loses search permission after the pin is refused with the reason, not as a
// different file, because the lock's sidecar can no longer be looked up.
func TestConfigLockPathsCaseAliasSearchDeniedNamesTheReason(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root searches a mode-0200 directory")
	}
	home := configLockActivationHome(t)
	dir := filepath.Join(home, "locked")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	cfg := filepath.Join(dir, "config.toml")
	activationWrite(t, cfg, deactivationConfig)
	hash, err := hashOrNull(cfg)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}
	manifest := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: cfg, PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})
	held := configLockWritersHold(t, cfg)
	configLockPathsHandoverRetarget(t, home, manifest, func() error {
		return os.Chmod(dir, 0o200)
	}, held.Release, manifest)
	_, err = Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
	if cerr := os.Chmod(dir, 0o700); cerr != nil {
		t.Fatal(cerr)
	}
	if err == nil || !strings.Contains(err.Error(), "cannot be searched") {
		t.Fatalf("a directory that lost search permission did not name the reason: %v", err)
	}
	if got := activationRead(t, cfg); !strings.Contains(got, "dedicated_tools") {
		t.Fatalf("the refused deactivation changed the config: %q", got)
	}
}
