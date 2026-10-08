package configguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-993 c1 (CRW-899 evaluation d1): the restore must publish to the file the manifest names, and the
// file the lock guards must be that file. The CRW-899 branch compared the manifest's file and parent by
// identity with the pinned pair, but never compared the live parent of the pinned pathname with the
// pinned directory. A directory moved away after the pin and replaced under the old name, with hard
// links to the file and its sidecar, passed every check: the restore published into the replacement
// and the file the manifest names kept the managed key.

// configLockPathsRestoreManifest is a manifest that owns memories.dedicated_tools in the file at path.
func configLockPathsRestoreManifest(t *testing.T, path string) []byte {
	t.Helper()
	hash, err := hashOrNull(path)
	if err != nil {
		t.Fatal(err)
	}
	return configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: path, PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}})
}

// The CRW-993 red case for d1: the manifest names the moved original directory, and the replacement
// directory holds hard links to the file and the sidecar the lock holds. The restore must be refused
// and nothing may be published. Red on the CRW-899 head, which restores the replacement's file and
// reports the key restored while the file the manifest names keeps it.
func TestConfigLockRestoreIdentityRefusesAHardLinkedReplacementDirectory(t *testing.T) {
	configLockActivationHome(t)
	root := t.TempDir()
	codex := filepath.Join(root, "codex")
	saved := filepath.Join(root, "codex.saved")
	if err := os.Mkdir(codex, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(codex, "config.toml")
	activationWrite(t, cfg, deactivationConfig)
	stale := configLockPathsRestoreManifest(t, cfg)
	fresh := configLockPathsRestoreManifest(t, filepath.Join(saved, "config.toml"))

	held := configLockWritersHold(t, cfg)
	configLockPathsHandoverRetarget(t, codex, stale, func() error {
		if err := os.Rename(codex, saved); err != nil {
			return err
		}
		if err := os.Mkdir(codex, 0o700); err != nil {
			return err
		}
		if err := os.Link(filepath.Join(saved, "config.toml"), cfg); err != nil {
			return err
		}
		return os.Link(filepath.Join(saved, "config.toml.crw-lock"), cfg+".crw-lock")
	}, held.Release, fresh)

	_, err := Deactivate(deactivationDeps(codex, func([]string) CodexRunResult { return CodexRunResult{} }))
	if err == nil {
		t.Fatal("the deactivation restored through a directory replaced after the pin: the manifest's file was not the one published")
	}
	if got := activationRead(t, filepath.Join(saved, "config.toml")); got != deactivationConfig {
		t.Fatalf("the file the manifest names was changed: %q", got)
	}
	if got := activationRead(t, cfg); got != deactivationConfig {
		t.Fatalf("the replacement path was published: %q", got)
	}
}

// Control: an ordinary deactivation restores the key in place.
func TestConfigLockRestoreIdentityRestoresAnOrdinaryFile(t *testing.T) {
	configLockActivationHome(t)
	codex := t.TempDir()
	cfg := filepath.Join(codex, "config.toml")
	activationWrite(t, cfg, deactivationConfig)
	if err := os.WriteFile(manifestPath(codex), configLockPathsRestoreManifest(t, cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Deactivate(deactivationDeps(codex, func([]string) CodexRunResult { return CodexRunResult{} }))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RestoredKeys) != 1 || strings.Contains(activationRead(t, cfg), "dedicated_tools") {
		t.Fatalf("the ordinary restore did not publish: restored=%v content=%q", res.RestoredKeys, activationRead(t, cfg))
	}
}

// Control: a directory moved before the deactivation starts keeps its identity, and a manifest that
// names it through its new spelling restores the file in the moved directory.
func TestConfigLockRestoreIdentityRestoresAMovedDirectoryWithIntactIdentity(t *testing.T) {
	configLockActivationHome(t)
	root := t.TempDir()
	codex := filepath.Join(root, "codex")
	moved := filepath.Join(root, "codex.moved")
	if err := os.Mkdir(codex, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(codex, "config.toml")
	activationWrite(t, cfg, deactivationConfig)
	if err := os.Rename(codex, moved); err != nil {
		t.Fatal(err)
	}
	movedCfg := filepath.Join(moved, "config.toml")
	if err := os.WriteFile(manifestPath(moved), configLockPathsRestoreManifest(t, movedCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Deactivate(deactivationDeps(moved, func([]string) CodexRunResult { return CodexRunResult{} }))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RestoredKeys) != 1 || strings.Contains(activationRead(t, movedCfg), "dedicated_tools") {
		t.Fatalf("the moved directory did not restore: restored=%v content=%q", res.RestoredKeys, activationRead(t, movedCfg))
	}
}
