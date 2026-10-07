package configguard

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// CRW-899: the activation and the deactivation must agree with the sidecar lock about which file
// they are touching. Activate keeps the lock keyed by lock.Target but reads, writes its managed
// keys and hashes config.toml through the caller's path (the rule CRW-891 gave
// SetMultiAgentV2State); a contended lock is answered as it is, before the unlocked re-read that
// maps an unreadable file to the read path's message; and Deactivate compares the re-read
// manifest's config path with the locked file by identity rather than by spelling. The first and
// second behaviours are red on the CRW-877 baseline, the third on the CRW-877 baseline's string
// comparison.

// configLockPathsPre is the config.toml the symlink points at before the runner runs, and
// configLockPathsRunnerPost is what the injected CLI leaves at the caller's pathname.
const configLockPathsPre = "[features]\nhooks = false\n"
const configLockPathsRunnerPost = "[features]\nmulti_agent = true\ngoals = true\nhooks = true\ndefault_mode_request_user_input = true\n"

// configLockPathsFeatureList is the declared-state probe's answer with every flag off, so every
// declared flag is an enable this activation would run.
func configLockPathsFeatureList() string {
	rows := make([]string, 0, len(DeclaredFeatures()))
	for _, key := range DeclaredFeatures() {
		rows = append(rows, string(key)+" stable false")
	}
	return strings.Join(rows, "\n")
}

// The generation-2 d1 case: the manifest names the config through a directory alias, and the alias
// is retargeted while the deactivation waits on the lock. The sidecar the lock holds belongs to the
// old directory, so the deactivation must refuse rather than restore the new file under the old
// lock. The retarget lands before the pin, because the goroutine retargets and only then releases
// the lock the deactivation is blocked on, so the pin always resolves the alias at the new
// directory. That ordering is what makes this test fail if the sidecar proof is removed: without
// it the pinned path would be the new file and the restore would write it. Red on the generation-1
// head, which restored the new file's key.
func TestConfigLockPathsDeactivateRefusesARetargetedDirectoryAlias(t *testing.T) {
	home := configLockActivationHome(t)
	realA := filepath.Join(home, "realA")
	realB := filepath.Join(home, "realB")
	for _, dir := range []string{realA, realB} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	pathA := filepath.Join(realA, "config.toml")
	pathB := filepath.Join(realB, "config.toml")
	activationWrite(t, pathA, deactivationConfig)
	activationWrite(t, pathB, deactivationConfig)
	alias := filepath.Join(home, "alias")
	if err := os.Symlink("realA", alias); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(alias, "config.toml")
	hash, err := hashOrNull(pathA)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}
	// Both readings name the same alias spelling; only the directory the alias points at changes.
	manifest := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: aliasPath, PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})

	held := configLockWritersHold(t, aliasPath)
	configLockPathsHandoverRetarget(t, home, manifest, func() error {
		// The lock is held through the old alias: retarget it, then let the deactivation in. The
		// pin runs after the release, so it resolves the alias at the new directory.
		if err := os.Remove(alias); err != nil {
			return err
		}
		return os.Symlink("realB", alias)
	}, held.Release, manifest)

	_, err = Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
	// The command refuses either way — through the sidecar proof when the retarget lands before the
	// pin, or through the manifest comparison when it lands after it (the FIFO handshake makes the
	// second ordering the deterministic one here). What this test pins is the outcome the defect
	// needed: nothing is restored, and the new directory's key is untouched.
	if err == nil {
		t.Fatal("the deactivation did not refuse the retargeted alias")
	}
	if got := activationRead(t, pathB); got != deactivationConfig {
		t.Fatalf("the refused deactivation restored the new directory's key: %q", got)
	}
	if got := activationRead(t, pathA); got != deactivationConfig {
		t.Fatalf("the refused deactivation wrote the old file: %q", got)
	}
}

// The generation-2 d2 case: one file named absolutely and then relatively. Red on the generation-1
// head, where EvalSymlinks kept the relative result relative and the two never compared equal.
func TestConfigLockPathsDeactivateAcceptsARelativeSpelling(t *testing.T) {
	home := configLockActivationHome(t)
	real := filepath.Join(home, "real")
	if err := os.MkdirAll(real, 0700); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(real, "config.toml")
	activationWrite(t, abs, deactivationConfig)
	hash, err := hashOrNull(abs)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}
	stale := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: abs, PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})
	fresh := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: filepath.Join("real", "config.toml"), PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})

	t.Chdir(home)
	held := configLockWritersHold(t, abs)
	configLockActivationHandover(t, home, stale, func() error {
		return os.WriteFile(manifestPath(home), fresh, 0o644)
	}, held.Release)

	r, err := Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.NoManifest || len(r.RestoredKeys) != 1 || r.RestoredKeys[0] != "memories.dedicated_tools" {
		t.Fatalf("the deactivation did not accept the relative spelling: %+v", r)
	}
	if got := activationRead(t, abs); strings.Contains(got, "dedicated_tools") {
		t.Fatalf("the managed key was left behind: %q", got)
	}
}

// The HoldsSidecar contract, pinned directly: true for the sidecar this lock holds, false for
// another file that has its own sidecar (so the comparison is between two real sidecar inodes, not
// between a file and nothing), and false once the lock is released.
func TestConfigLockPathsHoldsSidecarContract(t *testing.T) {
	home := configLockActivationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, deactivationConfig)
	lock, err := crwdir.LockConfig(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if !lock.HoldsSidecar(path) {
		t.Fatal("the lock did not recognise its own sidecar")
	}
	// The other file gets its own sidecar, so this compares two distinct sidecar inodes.
	other := filepath.Join(home, "other.toml")
	activationWrite(t, other, deactivationConfig)
	otherLock, err := crwdir.LockConfig(other, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer otherLock.Release()
	if _, err := os.Stat(other + ".crw-lock"); err != nil {
		t.Fatalf("the other sidecar was not created: %v", err)
	}
	if lock.HoldsSidecar(other) {
		t.Fatal("the lock accepted another file's sidecar")
	}
}

// The E1 guard, pinned directly and deterministically. The lock is taken while the alias points at
// the old directory, and only then is the alias retargeted; the guard must refuse, because the
// sidecar beside the newly resolved path is not the file this lock holds. This is the test that
// fails if the sidecar proof is dropped: without it the pin would simply be the new directory and
// no error would be returned.
func TestConfigLockPathsPinnedRefusesARetargetedDirectoryAlias(t *testing.T) {
	home := configLockActivationHome(t)
	realA := filepath.Join(home, "realA")
	realB := filepath.Join(home, "realB")
	for _, dir := range []string{realA, realB} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	activationWrite(t, filepath.Join(realA, "config.toml"), deactivationConfig)
	activationWrite(t, filepath.Join(realB, "config.toml"), deactivationConfig)
	alias := filepath.Join(home, "alias")
	if err := os.Symlink("realA", alias); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(alias, "config.toml")

	lock, err := crwdir.LockConfig(aliasPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	// The lock is held through the old alias; the directory behind it now changes.
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("realB", alias); err != nil {
		t.Fatal(err)
	}
	if _, err := configLockPathsPinned(lock); err == nil || !strings.Contains(err.Error(), "directory changed") {
		t.Fatalf("the guard did not refuse the retargeted alias: %v", err)
	}
	// Control: with the alias still pointing at the directory the lock was taken through, the
	// same lock passes the guard.
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("realA", alias); err != nil {
		t.Fatal(err)
	}
	pin, err := configLockPathsPinned(lock)
	if err != nil {
		t.Fatalf("the guard refused a stable alias: %v", err)
	}
	if want, err := filepath.EvalSymlinks(filepath.Join(realA, "config.toml")); err != nil || pin.path != want {
		t.Fatalf("the guard pinned %q, want %q (%v)", pin.path, want, err)
	}
}

// configLockPathsRenameRunner is the issue's reproduction: the injected CLI writes the new settings

// The fifth-generation d1 case, pinned at the helper: the pinned file's DIRECTORY is replaced after
// the pin, and the comparison is asked about the file the replacement points at. The pin's captured
// identities are the ones from lock time, so a helper that re-resolved the pinned path as a
// comparison target would have accepted the replacement. The public entry point's behaviour for the
// same replacement is pinned by the two sixth-generation tests below, which drive Deactivate.
func TestConfigLockPathsRefusesAPinnedDirectoryReplacedAfterThePin(t *testing.T) {
	home := configLockActivationHome(t)
	dirA := filepath.Join(home, "A")
	dirB := filepath.Join(home, "B")
	for _, dir := range []string{dirA, dirB} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfgA := filepath.Join(dirA, "config.toml")
	cfgB := filepath.Join(dirB, "config.toml")
	activationWrite(t, cfgA, deactivationConfig)
	activationWrite(t, cfgB, deactivationConfig)

	pin := configLockPathsTestPin(t, cfgA)
	// Replace the pinned directory with a link to the other one; the manifest now names B.
	if err := os.Rename(dirA, dirA+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dirB, dirA); err != nil {
		t.Fatal(err)
	}
	if configLockPathsSameTarget(cfgB, pin) {
		t.Fatal("a directory replaced after the pin was accepted as the locked file")
	}
}

// The sixth-generation d1 case, through the public entry point: the manifest names the config
// through an alias whose directory is replaced after the pin, and the replacement now sits at the
// very path the alias resolves to. The resolved string equals the pinned one, so a comparison that
// answered true on the string alone would accept it and restore the replacement's key under the
// moved file's lock. The comparison must also prove the pinned file and parent directory still are
// the ones that path holds.
func TestConfigLockPathsDeactivateRefusesAPinnedDirectoryReplacedUnderTheSameSpelling(t *testing.T) {
	home := configLockActivationHome(t)
	dirA := filepath.Join(home, "A")
	dirB := filepath.Join(home, "B")
	for _, dir := range []string{dirA, dirB} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	activationWrite(t, filepath.Join(dirA, "config.toml"), deactivationConfig)
	activationWrite(t, filepath.Join(dirB, "config.toml"), deactivationConfig)
	alias := filepath.Join(home, "alias")
	if err := os.Symlink("A", alias); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(alias, "config.toml")
	hash, err := hashOrNull(filepath.Join(dirA, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}
	// Both readings name the same spelling; only the directory behind the alias changes, and the
	// replacement holds a config with the same owned key and the same hash.
	manifest := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: aliasPath, PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})

	held := configLockWritersHold(t, aliasPath)
	configLockPathsHandoverRetarget(t, home, manifest, func() error {
		if err := os.Rename(dirA, dirA+".saved"); err != nil {
			return err
		}
		return os.Rename(dirB, dirA)
	}, held.Release, manifest)

	_, err = Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
	if err == nil || !strings.Contains(err.Error(), "names a different config file") {
		t.Fatalf("the deactivation accepted a directory replaced under the pinned spelling: %v", err)
	}
	if got := activationRead(t, filepath.Join(dirA, "config.toml")); got != deactivationConfig {
		t.Fatalf("the refused deactivation restored the replacement's key: %q", got)
	}
	if got := activationRead(t, filepath.Join(dirA+".saved", "config.toml")); got != deactivationConfig {
		t.Fatalf("the refused deactivation wrote the moved file: %q", got)
	}
}

// The sixth-generation d2 case, through the public entry point: the manifest names the OLD file at
// its new location after the directory was swapped. That spelling's file and parent directory are
// still the pinned ones, so a comparison that judged only the candidate's two identities would
// accept it while the restore publishes to the pinned path, which now holds another file. The
// comparison must also prove the pinned path's own parent is still the pinned directory.
func TestConfigLockPathsDeactivateRefusesAManifestNamingTheMovedDirectory(t *testing.T) {
	home := configLockActivationHome(t)
	dirA := filepath.Join(home, "A")
	dirB := filepath.Join(home, "B")
	for _, dir := range []string{dirA, dirB} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfgA := filepath.Join(dirA, "config.toml")
	activationWrite(t, cfgA, deactivationConfig)
	activationWrite(t, filepath.Join(dirB, "config.toml"), deactivationConfig)
	hash, err := hashOrNull(cfgA)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}
	stale := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: cfgA, PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})
	// The fresh reading names the file the lock guards at the location it moved to, whose file and
	// parent identities both still match the pin.
	fresh := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: filepath.Join(dirA+".saved", "config.toml"), PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})

	held := configLockWritersHold(t, cfgA)
	configLockPathsHandoverRetarget(t, home, stale, func() error {
		if err := os.Rename(dirA, dirA+".saved"); err != nil {
			return err
		}
		return os.Rename(dirB, dirA)
	}, held.Release, fresh)

	_, err = Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
	if err == nil || !strings.Contains(err.Error(), "names a different config file") {
		t.Fatalf("the deactivation accepted a manifest naming the moved directory: %v", err)
	}
	if got := activationRead(t, filepath.Join(dirA+".saved", "config.toml")); got != deactivationConfig {
		t.Fatalf("the refused deactivation restored the moved file's key: %q", got)
	}
	if got := activationRead(t, filepath.Join(dirA, "config.toml")); got != deactivationConfig {
		t.Fatalf("the refused deactivation wrote the replacement: %q", got)
	}
}

// A case-insensitive spelling of the locked entry is the same target and is accepted. The test needs
// a filesystem that folds case; where the filesystem distinguishes the two spellings it skips, and
// the refusal pinned above is the correct answer there.
func TestConfigLockPathsAcceptsACaseVariantSpelling(t *testing.T) {
	home := configLockActivationHome(t)
	real := filepath.Join(home, "real")
	if err := os.MkdirAll(real, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(real, "config.toml")
	activationWrite(t, cfg, deactivationConfig)
	// The same directory and the same file through a differently cased spelling.
	upperDir := filepath.Join(home, "REAL")
	dirInfo, err := os.Stat(upperDir)
	if err != nil {
		t.Skipf("this filesystem does not fold directory case: %v", err)
	}
	realDirInfo, err := os.Stat(real)
	if err != nil || !os.SameFile(dirInfo, realDirInfo) {
		t.Skip("this filesystem distinguishes the two directory spellings")
	}
	upperCfg := filepath.Join(upperDir, "CONFIG.TOML")
	if _, err := os.Stat(upperCfg); err != nil {
		t.Skipf("this filesystem does not fold file case: %v", err)
	}
	if !configLockPathsSameTarget(upperCfg, configLockPathsTestPin(t, cfg)) {
		t.Fatal("a case-insensitive spelling of the locked entry was refused")
	}
}

// A hard link whose name differs only by case is still a different directory entry, and a rename over
// the locked path would not reach it, so it stays refused even though the inode and the directory are
// the same. This is the case that separates a hard link from a case-insensitive spelling.
func TestConfigLockPathsRefusesACaseVariantHardLink(t *testing.T) {
	home := configLockActivationHome(t)
	lower := filepath.Join(home, "config.toml")
	upper := filepath.Join(home, "CONFIG.TOML")
	activationWrite(t, lower, deactivationConfig)
	if err := os.Link(lower, upper); err != nil {
		t.Skipf("this filesystem does not support hard links: %v", err)
	}
	if configLockPathsSameTarget(upper, configLockPathsTestPin(t, lower)) {
		t.Fatal("a case-variant hard link was accepted as the locked directory entry")
	}
}

// The entry count that separates a case-insensitive spelling from a hard link comes from the file's
// own inode, not from reading the directory: a parent without read permission still allows the stat,
// open and rename the restore needs, so enumerating it would refuse a restore the kernel would have
// allowed (CRW-899's seventh evaluation, a mode-0300 parent on a case-insensitive filesystem).
func TestConfigLockPathsOneEntryNeedsNoDirectoryReadPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0300 directory")
	}
	home := configLockActivationHome(t)
	dir := filepath.Join(home, "locked")
	if err := os.MkdirAll(dir, 0300); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte(deactivationConfig), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	// The directory cannot be enumerated now, but the file's own identity still answers.
	if _, err := os.ReadDir(dir); err == nil {
		t.Fatal("the directory was still readable; the test would not prove anything")
	}
	info, err := os.Stat(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !configLockPathsOneEntry(info) {
		t.Fatal("a file with one directory entry was not recognised without a directory read")
	}
}

// The same discriminator on a readable directory: one entry is one entry, and a second name for the
// same inode is two, so the hard link stays refused where the comparison reaches this check.
func TestConfigLockPathsOneEntryCountsEveryNameOfTheInode(t *testing.T) {
	home := configLockActivationHome(t)
	cfg := filepath.Join(home, "config.toml")
	activationWrite(t, cfg, deactivationConfig)
	info, err := os.Stat(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !configLockPathsOneEntry(info) {
		t.Fatal("a file with one directory entry was reported as having more than one")
	}
	if err := os.Link(cfg, filepath.Join(home, "linked.toml")); err != nil {
		t.Skipf("this filesystem does not support hard links: %v", err)
	}
	info, err = os.Stat(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if configLockPathsOneEntry(info) {
		t.Fatal("a file with two directory entries was reported as having one")
	}
}

// configLockPathsCaseInsensitiveDir answers a directory whose filesystem folds case: the caller's
// CRW_899_CASE_INSENSITIVE_DIR when it is set (an ext4 casefold or vfat mount a runner provides),
// else the test's temporary directory. It skips when the filesystem distinguishes the two
// spellings, because the acceptance cannot be exercised there.
func configLockPathsCaseInsensitiveDir(t *testing.T) string {
	t.Helper()
	root := os.Getenv("CRW_899_CASE_INSENSITIVE_DIR")
	if root == "" {
		root = t.TempDir()
	}
	if err := os.WriteFile(filepath.Join(root, "CaseProbe.toml"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "caseprobe.toml")); err != nil {
		t.Skip("this filesystem distinguishes the two spellings")
	}
	return root
}

// The seventh-generation d1 case, through the public entry point: the manifest names the same config
// through a differently cased spelling and the parent directory has no read permission. The restore
// needs only search permission, so the comparison must accept the spelling without enumerating the
// directory. Red on the head that read the directory: it refused and left the owned key behind.
func TestConfigLockPathsDeactivateAcceptsACaseVariantUnderAnUnreadableParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0300 directory")
	}
	root := configLockPathsCaseInsensitiveDir(t)
	home := configLockActivationHome(t)
	// A directory of this run's own, so a leftover from an earlier run cannot decide the result.
	dir, err := os.MkdirTemp(root, "locked-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0700)
		_ = os.RemoveAll(dir)
	})
	path := filepath.Join(dir, "config.toml")
	activationWrite(t, path, deactivationConfig)
	hash, err := hashOrNull(path)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}
	stale := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: path, PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})
	// The same file named through a differently cased spelling of the same directory.
	fresh := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: filepath.Join(root, strings.ToUpper(filepath.Base(dir)), "CONFIG.TOML"), PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})

	held := configLockWritersHold(t, path)
	configLockActivationHandover(t, home, stale, func() error {
		// The directory loses its read permission while the deactivation waits, so only a
		// comparison that reads the directory would notice a difference.
		if err := os.Chmod(dir, 0300); err != nil {
			return err
		}
		return os.WriteFile(manifestPath(home), fresh, 0o644)
	}, held.Release)

	r, err := Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
	if err != nil {
		t.Fatalf("the deactivation refused a case variant under an unreadable parent: %v", err)
	}
	if r == nil || r.NoManifest || len(r.RestoredKeys) != 1 || r.RestoredKeys[0] != "memories.dedicated_tools" {
		t.Fatalf("the deactivation did not restore the owned key: %+v", r)
	}
	if got := activationRead(t, path); strings.Contains(got, "dedicated_tools") {
		t.Fatalf("the managed key was left behind: %q", got)
	}
}

// The root-parent boundary: a config named directly at the filesystem root must resolve through
// "/". Dropping the trailing separator unconditionally would leave an empty parent and resolve the
// working directory instead, so the pin would reject a correctly held root sidecar.
func TestConfigLockPathsRootParentKeepsTheRoot(t *testing.T) {
	if _, err := os.Stat(string(filepath.Separator)); err != nil {
		t.Skip("no filesystem root")
	}
	name := string(filepath.Separator) + "config.toml.crw-899-absent"
	got, ok := configLockPathsRealPath(name)
	if !ok {
		t.Fatalf("a root-parented absent file did not resolve")
	}
	if got != name {
		t.Fatalf("the root parent resolved to %q, want %q", got, name)
	}
	if root, ok := configLockPathsRealPath(string(filepath.Separator)); !ok || root != string(filepath.Separator) {
		t.Fatalf("the root itself resolved to %q ok=%v", root, ok)
	}
}

// The pre-merge d1 case (second record), pinned directly: only the FINAL component may be missing.
// A missing intermediate directory must not be synthesised, because the kernel cannot resolve such a
// path and the answer would otherwise be folded back onto the locked file with a lexical Join.
func TestConfigLockPathsMissingIntermediateDirectoryIsNotSynthesised(t *testing.T) {
	home := configLockActivationHome(t)
	real := filepath.Join(home, "real")
	if err := os.MkdirAll(real, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(real, "config.toml")
	activationWrite(t, cfg, deactivationConfig)
	spelling := real + string(filepath.Separator) + "missing" + string(filepath.Separator) + ".." + string(filepath.Separator) + "config.toml"
	if _, ok := configLockPathsRealPath(spelling); ok {
		t.Fatalf("a missing intermediate directory was synthesised: %q", spelling)
	}
	if configLockPathsSameTarget(spelling, configLockPathsTestPin(t, cfg)) {
		t.Fatalf("the unresolvable spelling was accepted as the locked file: %q", spelling)
	}
}

// The pre-merge d2 case (second record): a bare relative name whose file was removed must still
// resolve through the resolved working directory, so an uninstall with a relative CODEX_HOME and a
// deleted config.toml keeps working instead of failing the new pin.
func TestConfigLockPathsBareRelativeAbsentConfigResolves(t *testing.T) {
	home := configLockActivationHome(t)
	t.Chdir(home)
	lock, err := crwdir.LockConfig("config.toml", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	pin, err := configLockPathsPinned(lock)
	if err != nil {
		t.Fatalf("the pin refused a bare relative absent config: %v", err)
	}
	if want := filepath.Join(home, "config.toml"); pin.path != want {
		t.Fatalf("the pin answered %q, want %q", pin.path, want)
	}
}

// The pre-merge d2 case, pinned directly: a spelling whose parent is a symlink followed by ".."
// must be resolved by the kernel, not cleaned lexically. The old helper removed the ".." before
// resolving and answered a path the manifest does not name, accepting a genuinely different file.
func TestConfigLockPathsSpellingWithDotDotResolvesThroughTheKernel(t *testing.T) {
	home := configLockActivationHome(t)
	x := filepath.Join(home, "x")
	sub := filepath.Join(home, "y", "sub")
	for _, dir := range []string{x, sub} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(x, "config.toml")
	activationWrite(t, cfg, deactivationConfig)
	if err := os.Symlink(sub, filepath.Join(x, "alias")); err != nil {
		t.Fatal(err)
	}
	// alias -> /y/sub, so /x/alias/../config.toml is /y/config.toml, which does not exist. A
	// lexical clean would answer /x/config.toml, the pinned file, and accept the manifest.
	spelling := x + string(filepath.Separator) + "alias" + string(filepath.Separator) + ".." + string(filepath.Separator) + "config.toml"
	if configLockPathsSameTarget(spelling, configLockPathsTestPin(t, cfg)) {
		t.Fatalf("the comparison cleaned the spelling before resolving it: %q", spelling)
	}
}

// The pre-merge d1 case, pinned directly: the comparison must not re-resolve the pinned path. Once
// the path is pinned, replacing it with a link to another file must not make the manifest's name
// of that other file compare equal — otherwise the restore would write the other file without its
// lock. Red on the head that resolved both sides: it accepted the pair.
func TestConfigLockPathsComparisonDoesNotResolveThePinAgain(t *testing.T) {
	home := configLockActivationHome(t)
	realA := filepath.Join(home, "realA")
	realB := filepath.Join(home, "realB")
	for _, dir := range []string{realA, realB} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	pathA := filepath.Join(realA, "config.toml")
	pathB := filepath.Join(realB, "config.toml")
	activationWrite(t, pathA, deactivationConfig)
	activationWrite(t, pathB, deactivationConfig)

	// The pin is captured once, while the locked file is where it was.
	pin := configLockPathsTestPin(t, pathA)
	// Control: a spelling of the locked file matches the pin.
	if !configLockPathsSameTarget(pathA, pin) {
		t.Fatal("the comparison rejected the locked file before it changed")
	}
	// A link now stands where the pinned file was, naming the other file.
	if err := os.Remove(pathA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../realB/config.toml", pathA); err != nil {
		t.Fatal(err)
	}
	// The other file must not be accepted as the locked one: the pin is not re-interpreted.
	if configLockPathsSameTarget(pathB, pin) {
		t.Fatal("the comparison re-resolved the pin and accepted another file as the locked one")
	}
}

// configLockPathsRenameRunner is the issue's reproduction: the injected CLI writes the new settings
// to a temporary file and renames it over the caller's path, so a symlink there is replaced by a
// regular file and the old target keeps its bytes.
func configLockPathsRenameRunner(t *testing.T, path string, calls *[][]string) CodexRunner {
	t.Helper()
	return func(args []string) CodexRunResult {
		if calls != nil {
			*calls = append(*calls, append([]string(nil), args...))
		}
		if args[1] == "list" {
			return CodexRunResult{Stdout: configLockPathsFeatureList()}
		}
		tmp := path + ".runner.tmp"
		if err := os.WriteFile(tmp, []byte(configLockPathsRunnerPost), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
		return CodexRunResult{}
	}
}

// configLockPathsWriteThroughLinkRunner edits the file the link names, the way a CLI that rewrites
// config.toml in place does: the caller's pathname stays a link.
func configLockPathsWriteThroughLinkRunner(t *testing.T, target string, calls *[][]string) CodexRunner {
	t.Helper()
	return func(args []string) CodexRunResult {
		if calls != nil {
			*calls = append(*calls, append([]string(nil), args...))
		}
		if args[1] == "list" {
			return CodexRunResult{Stdout: configLockPathsFeatureList()}
		}
		activationWrite(t, target, configLockPathsRunnerPost)
		return CodexRunResult{}
	}
}

func configLockPathsHash(t *testing.T, path string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(activationRead(t, path)))
	return hex.EncodeToString(sum[:])
}

// The issue's reproduction on the activation side. The runner replaces the symlink pathname
// during the injected "codex features enable" calls, so the managed key must be written to the
// file the caller's path now names and the recorded hash must describe that file. Red on the
// baseline: the key lands in the lock's old target and the hash describes the target.
func TestConfigLockPathsActivateWritesThroughTheReplacedCallerPath(t *testing.T) {
	home := configLockActivationHome(t)
	path := filepath.Join(home, "config.toml")
	target := filepath.Join(home, "target.toml")
	activationWrite(t, target, configLockPathsPre)
	if err := os.Symlink("target.toml", path); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	deps := ActivateDeps{CodexHome: home, Run: configLockPathsRenameRunner(t, path, &calls), Now: func() string { return "2026-06-30T00:00:00.000Z" }}

	m, err := Activate(deps)
	if err != nil {
		t.Fatal(err)
	}
	if info, e := os.Lstat(path); e != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the runner's rename did not replace the link: %v, %v", info, e)
	}
	live := activationRead(t, path)
	if !strings.Contains(live, "dedicated_tools = true") {
		t.Fatalf("the managed key is not in the file read through the caller's path: %q", live)
	}
	if m.PostActivateHash == nil || *m.PostActivateHash != configLockPathsHash(t, path) {
		t.Fatalf("the manifest hash does not describe the caller's path: %v", m.PostActivateHash)
	}
	if got := activationRead(t, target); got != configLockPathsPre {
		t.Fatalf("the activation wrote through the lock's old target: %q", got)
	}
	if m.TableKeys["memories.dedicated_tools"].AppliedValue != "true" {
		t.Fatalf("manifest table key = %+v", m.TableKeys["memories.dedicated_tools"])
	}
}

// Control A: a regular-file config.toml and the same runner keep today's answer, because the
// caller's path and the lock's target are the same file at every step.
func TestConfigLockPathsActivateRegularFileControl(t *testing.T) {
	home := configLockActivationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, configLockPathsPre)
	var calls [][]string
	deps := ActivateDeps{CodexHome: home, Run: configLockPathsRenameRunner(t, path, &calls), Now: func() string { return "2026-06-30T00:00:00.000Z" }}

	m, err := Activate(deps)
	if err != nil {
		t.Fatal(err)
	}
	live := activationRead(t, path)
	if !strings.Contains(live, "dedicated_tools = true") {
		t.Fatalf("config = %q", live)
	}
	if m.PostActivateHash == nil || *m.PostActivateHash != configLockPathsHash(t, path) {
		t.Fatalf("the manifest hash does not describe config.toml: %v", m.PostActivateHash)
	}
}

// Control B: a runner that writes through the link keeps the link and repairs its target, because
// the caller's path still resolves to the file the lock guards.
func TestConfigLockPathsActivateWriteThroughTheLinkControl(t *testing.T) {
	home := configLockActivationHome(t)
	path := filepath.Join(home, "config.toml")
	target := filepath.Join(home, "target.toml")
	activationWrite(t, target, configLockPathsPre)
	if err := os.Symlink("target.toml", path); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	deps := ActivateDeps{CodexHome: home, Run: configLockPathsWriteThroughLinkRunner(t, target, &calls), Now: func() string { return "2026-06-30T00:00:00.000Z" }}

	m, err := Activate(deps)
	if err != nil {
		t.Fatal(err)
	}
	if link, e := os.Readlink(path); e != nil || link != "target.toml" {
		t.Fatalf("the symlink was replaced: %v, %v", link, e)
	}
	live := activationRead(t, path)
	if !strings.Contains(live, "dedicated_tools = true") {
		t.Fatalf("config through the link = %q", live)
	}
	if m.PostActivateHash == nil || *m.PostActivateHash != configLockPathsHash(t, target) {
		t.Fatalf("the manifest hash does not describe the link's target: %v", m.PostActivateHash)
	}
}

// A contended lock is answered as it is, before the unlocked re-read. With another CRW writer
// holding the lock and config.toml unreadable, the baseline replaces the busy text with the read
// path's message ("could not read ... left unchanged"), so the operator is told the wrong thing.
func TestConfigLockPathsActivateAnswersBusyWhenTheConfigIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0200 file")
	}
	home := configLockActivationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, "[features]\nhooks = false\n")
	if err := os.Chmod(path, 0200); err != nil {
		t.Fatal(err)
	}
	held := configLockWritersHold(t, path)
	defer held.Release()

	var calls [][]string
	_, err := Activate(activationDeps(t, home, allActivationFlags(), &calls))
	if err == nil || err.Error() != crwdir.ConfigLockBusy {
		t.Fatalf("the contended activation did not answer the busy text: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("the refused activation reached the runner: %v", calls)
	}
}

// The same-file-under-another-spelling case: CODEX_HOME/config.toml is reached through a directory
// symlink, and the manifest republished while the deactivation waits names the same file through
// that alias. The identity comparison accepts it and restores the key; the baseline's string
// comparison refuses it as a different config file.
func TestConfigLockPathsDeactivateAcceptsTheSameFileUnderAnotherSpelling(t *testing.T) {
	home := configLockActivationHome(t)
	realDir := filepath.Join(home, "real")
	if err := os.MkdirAll(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(realDir, "config.toml")
	activationWrite(t, path, deactivationConfig)
	alias := filepath.Join(home, "alias")
	if err := os.Symlink("real", alias); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(alias, "config.toml")
	hash, err := hashOrNull(path)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}
	stale := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: path, PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})
	fresh := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: aliasPath, PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})

	held := configLockWritersHold(t, path)
	configLockActivationHandover(t, home, stale, func() error {
		// While it waits, another writer republishes the manifest naming the same file through the
		// directory symlink.
		return os.WriteFile(manifestPath(home), fresh, 0o644)
	}, held.Release)

	r, err := Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.NoManifest || len(r.RestoredKeys) != 1 || r.RestoredKeys[0] != "memories.dedicated_tools" {
		t.Fatalf("the deactivation did not accept the same file under another spelling: %+v", r)
	}
	if got := activationRead(t, path); strings.Contains(got, "dedicated_tools") {
		t.Fatalf("the managed key was left behind: %q", got)
	}
}

// Control C: a manifest that names a really different config file is still refused, and the

// configLockPathsHandoverRetarget is the generation-2 d1 rendezvous. The deactivation must be
// provably past LockConfig, with the alias still pointing at the old directory, before the alias is
// retargeted: the generation-1 single-FIFO handover gates only the first manifest read, so the
// retarget raced the sidecar open (measured 9/10 runs landed after it, where the deactivation
// legitimately opened the new directory's sidecar and acceptance is correct). The second FIFO's
// open handshake is what proves the lock was already taken through the old alias.
func configLockPathsHandoverRetarget(t *testing.T, home string, stale []byte, retarget func() error, release func(), fresh []byte) {
	t.Helper()
	first := manifestPath(home)
	if err := unix.Mkfifo(first, 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		f, err := os.OpenFile(first, os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := f.Write(stale); err != nil {
			t.Error(err)
		}
		if err := f.Close(); err != nil {
			t.Error(err)
		}
		if err := os.Remove(first); err != nil {
			t.Error(err)
			return
		}
		// The second FIFO is the handshake: its open for writing blocks until the deactivation
		// opens the manifest for its second reading, which happens only after it holds the lock.
		second := manifestPath(home)
		if err := unix.Mkfifo(second, 0o600); err != nil {
			t.Error(err)
			return
		}
		release()
		f, err = os.OpenFile(second, os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = f.Close() }()
		// The deactivation holds the lock now; retarget the alias and republish through the open
		// descriptor, so the write cannot race the retarget.
		if err := retarget(); err != nil {
			t.Error(err)
			return
		}
		if _, err := f.Write(fresh); err != nil {
			t.Error(err)
		}
	}()
}

// refusal keeps the message the operator already knows. Both files exist, so the identity
// comparison runs and answers false rather than failing to stat.
func TestConfigLockPathsDeactivateRefusesAReallyDifferentConfigFile(t *testing.T) {
	home := configLockActivationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, deactivationConfig)
	other := filepath.Join(home, "other.toml")
	activationWrite(t, other, deactivationConfig)
	hash, err := hashOrNull(path)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}
	stale := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: path, PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})
	fresh := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: other, PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})

	held := configLockWritersHold(t, path)
	configLockActivationHandover(t, home, stale, func() error {
		return os.WriteFile(manifestPath(home), fresh, 0o644)
	}, held.Release)

	_, err = Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
	if err == nil || !strings.Contains(err.Error(), "names a different config file") {
		t.Fatalf("the deactivation did not refuse the moved manifest: %v", err)
	}
	if got := activationRead(t, path); got != deactivationConfig {
		t.Fatalf("the refused deactivation wrote: %q", got)
	}
}

// Control D: a hard link is the same inode under another name, but the restore publishes through
// the locked path with an atomic rename, so accepting the manifest's name would leave the managed
// key in place while the command reported it restored. The comparison is directory-entry identity,
// so a hard link is refused exactly as it was before this change. This is the case the reviewer
// raised against a device-and-inode comparison.
func TestConfigLockPathsDeactivateRefusesAHardLinkedConfigFile(t *testing.T) {
	home := configLockActivationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, deactivationConfig)
	linked := filepath.Join(home, "linked.toml")
	if err := os.Link(path, linked); err != nil {
		t.Skipf("this filesystem does not support hard links: %v", err)
	}
	hash, err := hashOrNull(path)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}
	stale := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: path, PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})
	fresh := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: linked, PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})

	held := configLockWritersHold(t, path)
	configLockActivationHandover(t, home, stale, func() error {
		return os.WriteFile(manifestPath(home), fresh, 0o644)
	}, held.Release)

	_, err = Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
	if err == nil || !strings.Contains(err.Error(), "names a different config file") {
		t.Fatalf("the deactivation did not refuse the hard-linked manifest: %v", err)
	}
	if got := activationRead(t, path); got != deactivationConfig {
		t.Fatalf("the refused deactivation wrote: %q", got)
	}
	if got := activationRead(t, linked); got != deactivationConfig {
		t.Fatalf("the refused deactivation wrote through the hard link: %q", got)
	}
}

// configLockPathsTestPin captures the pin for a path the way Deactivate does, so a test can compare a
// spelling against it.
func configLockPathsTestPin(t *testing.T, path string) *configLockPathsPin {
	t.Helper()
	lock, err := crwdir.LockConfig(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lock.Release)
	pin, err := configLockPathsPinned(lock)
	if err != nil {
		t.Fatal(err)
	}
	return pin
}
