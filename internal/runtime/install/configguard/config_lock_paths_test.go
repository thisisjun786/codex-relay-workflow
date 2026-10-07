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

// The generation-2 d1 case: the manifest names the config through a directory alias that is
// retargeted while the deactivation waits on the lock. The held sidecar belongs to the old file, so
// the deactivation must refuse rather than restore the new file under the old lock. Red on the
// generation-1 head: it restored the new file's key.
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
	stale := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: aliasPath, PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})
	fresh := configLockActivationManifestBytes(t, &InstallManifest{Version: 2, ConfigPath: pathB, PostActivateHash: hash, Flags: map[string]FlagRecord{}, TableKeys: keys})

	held := configLockWritersHold(t, aliasPath)
	configLockPathsHandoverRetarget(t, home, stale, fresh, held.Release, func() error {
		// The lock is already held through the old alias: retarget it at the new directory.
		if err := os.Remove(alias); err != nil {
			return err
		}
		return os.Symlink("realB", alias)
	})

	_, err = Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} }))
	if err == nil || !strings.Contains(err.Error(), "names a different config file") {
		t.Fatalf("the deactivation did not refuse the retargeted alias: %v", err)
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
// another file, and false once the lock is released.
func TestConfigLockPathsHoldsSidecarContract(t *testing.T) {
	home := configLockActivationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, deactivationConfig)
	lock, err := crwdir.LockConfig(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !lock.HoldsSidecar(path) {
		t.Fatal("the lock did not recognise its own sidecar")
	}
	other := filepath.Join(home, "other.toml")
	activationWrite(t, other, deactivationConfig)
	if lock.HoldsSidecar(other) {
		t.Fatal("the lock accepted another file's sidecar")
	}
	lock.Release()
	if lock.HoldsSidecar(path) {
		t.Fatal("a released lock still answered true")
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
func configLockPathsHandoverRetarget(t *testing.T, home string, stale, fresh []byte, release func(), retarget func() error) {
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
		// The second FIFO is the handshake: its open for writing blocks until the deactivation,
		// already holding the lock it took through the old alias, opens it for reading.
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
		// The lock is held on the old file; retarget the alias and republish through the open
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
