package configguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

type DeactivateDeps struct {
	Run                   CodexRunner
	CodexHome, ConfigPath string
	Now                   func() string
}

type SkipReason string

const (
	SkipMissing      SkipReason = "missing"
	SkipChanged      SkipReason = "changed"
	SkipUnverifiable SkipReason = "unverifiable"
)

type SkippedExternal struct {
	Target string     `json:"target"`
	Reason SkipReason `json:"reason"`
}

type DeactivateResult struct {
	Disabled                 []string          `json:"disabled"`
	SkippedPreExisting       []string          `json:"skippedPreExisting"`
	NoManifest               bool              `json:"noManifest"`
	RestoredKeys             []string          `json:"restoredKeys"`
	SkippedExternal          []SkippedExternal `json:"skippedExternal"`
	FileDrifted              bool              `json:"fileDrifted"`
	FeaturesStateUnavailable bool              `json:"featuresStateUnavailable"`
}

// configLockPathsPinned answers the path this deactivation must work on, and refuses when the file
// the lock guards cannot be proven. The lock is keyed by the file it guards, but its spelling was
// resolved before this command waited, and a directory symlink in the path can be retargeted during
// the wait. The path is therefore pinned to the file the held sidecar belongs to: the sidecar beside
// the resolved real path must be the very file this lock holds (fstat), or the directory changed
// under the wait and acting now would edit a file this lock does not guard (CRW-899 E1, fail
// closed). Everything downstream — the drift hash, the read, the restore and the injected CLI calls
// — works on the pinned path, never on a spelling re-resolved after the wait.
func configLockPathsPinned(lock *crwdir.ConfigLock) (string, error) {
	pinned, ok := configLockPathsRealPath(lock.Target)
	if !ok || !lock.HoldsSidecar(pinned) {
		return "", fmt.Errorf("the config file's directory changed while the lock was being taken (%s); run the deactivation again", lock.Target)
	}
	return pinned, nil
}

func readTextOrNull(path string) (*string, error) {
	b, exists, err := activationReadFile(path)
	if err != nil || !exists {
		return nil, err
	}
	s := string(b)
	return &s, nil
}

// configLockPathsRealPath answers the path a config file really has on disk once every symlink in it
// is followed, so two spellings of one file compare equal. Links are resolved first and only then is
// a relative result joined to the resolved working directory: cleaning or absolutising the input
// before the resolution would compare spellings instead of the file, which is the defect CRW-899
// fixes (a relative CODEX_HOME and an absolute manifest path named one file and compared unequal).
// A final component that is not there — an install whose config.toml was removed — is named through
// its resolved parent, so the caller still pins a real path for the directory entry it will use.
// The second answer is false when nothing can be resolved, and every caller treats that as "not the
// same file": the comparison only ever widens acceptance to spellings that name one path.
func configLockPathsRealPath(p string) (string, bool) {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return configLockPathsAbsolute(resolved)
	}
	// Only the final component may be missing — an install whose config.toml was removed. It is
	// named through its parent, and that parent must resolve completely: synthesising a missing
	// intermediate directory would let a path the kernel cannot resolve be answered as the locked
	// file (CRW-899's pre-merge d1). The split is lexical and does NOT clean, so a ".." reaches the
	// kernel applied to the directory it has already resolved rather than being folded into the
	// spelling (E2).
	dir, base := filepath.Split(p)
	if base == "" || base == "." || base == ".." {
		return "", false
	}
	if dir == "" {
		dir = "."
	}
	realDir, err := filepath.EvalSymlinks(strings.TrimSuffix(dir, string(filepath.Separator)))
	if err != nil {
		return "", false
	}
	absDir, ok := configLockPathsAbsolute(realDir)
	if !ok {
		return "", false
	}
	return filepath.Join(absDir, base), true
}

// configLockPathsAbsolute makes a kernel-resolved path absolute in kernel terms: a relative result
// is joined to the resolved working directory, never to the lexical one, so two spellings of one
// file compare equal however the process was started.
func configLockPathsAbsolute(resolved string) (string, bool) {
	if filepath.IsAbs(resolved) {
		return resolved, true
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	realCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", false
	}
	return filepath.Join(realCwd, resolved), true
}

// configLockPathsSameTarget reports whether a spelling names the file the pinned path names.
// Only the spelling is resolved; the pinned path is already kernel-resolved and is compared as it
// is. Re-resolving the pinned path would follow a link that appeared after it was pinned, which is
// exactly how a path that now names another file would be accepted as the locked one, so the pin is
// never re-interpreted (CRW-899 E1). The manifest is allowed to name the config file through a
// different spelling — CODEX_HOME behind a directory symlink, for example, which crwdir's lock
// resolution leaves spelled through the alias because it follows only a symlink in the final
// component — and a deactivation that treated that as a different file would refuse to restore an
// install it owns. The comparison is deliberately directory-entry identity, not inode identity: the
// restore publishes through the pinned path with an atomic rename, which replaces that one pathname,
// so a hard link to the same inode under another name would keep the managed key while this command
// reported it restored (fail open). A hard link therefore stays refused, and a spelling that cannot
// be resolved is not the same target, so the comparison never accepts what it could not prove.
func configLockPathsSameTarget(spelling, pinned string) bool {
	real, ok := configLockPathsRealPath(spelling)
	if !ok {
		return false
	}
	return real == pinned
}

// DecideKeyRestore is deactivate.ts's per-key decision table. backupKnown=false means
// unreadable/absent backup; a known nil value means the backup confirms the key was absent.
// A successful decision has an empty reason.
func DecideKeyRestore(rec TableKeyRecord, live *string, drift, backupKnown bool, backup *string) (bool, SkipReason) {
	switch {
	case !rec.SetByCodexclaw:
		return false, SkipChanged
	case live == nil:
		return false, SkipMissing
	case *live != rec.AppliedValue:
		return false, SkipChanged
	case rec.PriorValue == nil && drift && (!backupKnown || backup != nil):
		return false, SkipUnverifiable
	default:
		return true, ""
	}
}

// configLockWritersDeactivateWrites reports whether this deactivation will write config.toml: it
// restores owned table keys, or it asks the injected CLI to disable a flag CRW enabled. With
// neither, uninstall is not gated on the lock, because there is no config.toml write to serialize.
func configLockWritersDeactivateWrites(m *InstallManifest) bool {
	if len(m.TableKeys) > 0 {
		return true
	}
	for _, key := range manifestOrder(m.flagOrder, m.Flags) {
		if f := m.Flags[key]; !f.PriorEnabled && f.EnabledByCodexclaw {
			return true
		}
	}
	return false
}

// Deactivate restores owned table keys before asking the injected CLI to disable flags.
// The manifest/backup remain ownership evidence, never overwritten by deactivation.
func Deactivate(deps DeactivateDeps) (*DeactivateResult, error) {
	now := deps.Now
	if now == nil {
		now = func() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
	}
	// Oracle parity: marker failure never gates uninstall, including early exits. The
	// marker API itself refuses unreadable records rather than replacing their consent data.
	markOptedOut := func() { _ = MarkSelfHealOptedOut(deps.CodexHome, now()) }
	r := &DeactivateResult{Disabled: []string{}, SkippedPreExisting: []string{}, NoManifest: true, RestoredKeys: []string{}, SkippedExternal: []SkippedExternal{}}
	noManifest := func() (*DeactivateResult, error) {
		markOptedOut()
		r.NoManifest = true
		return r, nil
	}
	// The first reading decides only whether and where to lock; the reading every decision below uses
	// is taken after the lock is held (CRW-877).
	raw, _ := readTextOrNull(manifestPath(deps.CodexHome))
	if raw == nil {
		return noManifest()
	}
	m := parseInstallManifest(*raw)
	if m == nil {
		return noManifest()
	}
	r.NoManifest = false
	path := deps.ConfigPath
	if path == "" {
		path = m.ConfigPath
	}
	// lockedPath is the spelling the first reading chose; the re-read below must still name the same
	// file for the held lock to be the lock over the manifest's own config file.
	lockedPath := path
	// One critical section for the whole command's writes to config.toml under the sidecar lock
	// every CRW writer of config.toml takes (CRW-866), the shape of activate.go's
	// activationSetKeyLocked: the drift check, the read, the restore, and the injected CLI calls
	// that disable flags and rewrite the same file. A CRW writer that published between the read
	// and the restore, or between the restore and the CLI's own read-modify-write, would otherwise
	// have its change discarded with neither command reporting it. The lock is taken only when this
	// deactivation writes config.toml, so an uninstall with nothing to restore and no flag CRW
	// enabled is never gated on it, and an empty config path names no file to guard.
	if path != "" && configLockWritersDeactivateWrites(m) {
		lock, err := crwdir.LockConfig(path, activationLockWait)
		if err != nil {
			return nil, err
		}
		defer lock.Release()
		pinned, pinErr := configLockPathsPinned(lock)
		if pinErr != nil {
			return nil, pinErr
		}
		path = pinned
		// The manifest read before the lock answered only whether and where to lock. An activation
		// that published while this command waited would otherwise be ignored, and the restore would
		// be computed from a manifest that no longer describes the install: the drift hash, the table
		// keys and the flags below all decide from this second reading. A manifest missing or
		// unreadable here answers as the no-manifest branch above does.
		fresh, _ := readTextOrNull(manifestPath(deps.CodexHome))
		if fresh == nil {
			return noManifest()
		}
		if m = parseInstallManifest(*fresh); m == nil {
			return noManifest()
		}
		// The lock is held on the file the first reading named. A manifest that now names a different
		// config file would have this deactivation apply one file's ownership records to another, so
		// it refuses rather than acting under the wrong lock (fail closed). An explicit ConfigPath
		// overrides the manifest in both readings, so only the derived path can disagree.
		if deps.ConfigPath == "" && !configLockPathsSameTarget(m.ConfigPath, path) {
			return nil, fmt.Errorf("the install manifest now names a different config file (%s, was %s); run the deactivation again", m.ConfigPath, lockedPath)
		}
	}
	// The opt-out is recorded only once this deactivation is going to do its work: a busy lock
	// refuses the command before this line, and a refusal must not leave self-healing off for an
	// uninstall that never ran (the commit-order rule: never record an effect that did not happen).
	markOptedOut()
	if m.PostActivateHash != nil {
		hash, err := hashOrNull(path)
		if err != nil {
			return nil, err
		}
		r.FileDrifted = hash == nil || *hash != *m.PostActivateHash
	}
	// Even without a hash or managed keys, the later CLI may rewrite settings. Never
	// hand it an unreadable file: the oracle's swallowed read can lose the old settings.
	content, err := readTextOrNull(path)
	if err != nil {
		return nil, err
	}
	if content != nil && len(m.TableKeys) > 0 {
		if err := deactivateTableKeys(path, *content, m, r); err != nil {
			return nil, err
		}
	}
	live, err := ReadDeclaredState(deps.Run)
	r.FeaturesStateUnavailable = err != nil
	for _, key := range manifestOrder(m.flagOrder, m.Flags) {
		flag := m.Flags[key]
		if flag.PriorEnabled {
			r.SkippedPreExisting = append(r.SkippedPreExisting, key)
			continue
		}
		if !flag.EnabledByCodexclaw {
			continue
		}
		if enabled, present := live[key]; present && !enabled {
			r.SkippedExternal = append(r.SkippedExternal, SkippedExternal{key, SkipMissing})
			continue
		}
		if deps.Run([]string{"features", "disable", key}).ExitCode == 0 {
			r.Disabled = append(r.Disabled, key)
		}
	}
	return r, nil
}

func deactivateTableKeys(path, content string, m *InstallManifest, r *DeactivateResult) error {
	var backup *string
	if m.BackupPath != nil && *m.BackupPath != "" {
		// A read failure is unknown provenance, not evidence of an absent backup key.
		backup, _ = readTextOrNull(*m.BackupPath)
	}
	changed := false
	for _, id := range manifestOrder(m.tableOrder, m.TableKeys) {
		rec := m.TableKeys[id]
		value, found := ReadTableKey(content, rec.Table, rec.Key)
		var live, prior *string
		if found {
			live = &value
		}
		if backup != nil {
			if v, ok := ReadTableKey(*backup, rec.Table, rec.Key); ok {
				prior = &v
			}
		}
		restore, reason := DecideKeyRestore(rec, live, r.FileDrifted, backup != nil, prior)
		if !restore {
			r.SkippedExternal = append(r.SkippedExternal, SkippedExternal{id, reason})
			continue
		}
		edit := RestoreTableKey(content, rec.Table, rec.Key, rec.PriorValue)
		content, changed = edit.Content, changed || edit.Changed
		r.RestoredKeys = append(r.RestoredKeys, id)
	}
	if changed {
		return activationPublish(path, []byte(content))
	}
	return nil
}
