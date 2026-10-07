package configguard

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
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
// closed). The Go-side operations — the drift hash, the read and the restore — work on the pinned
// path, never on a spelling re-resolved after the wait. The injected CLI calls take only feature
// names and read CODEX_HOME from the environment, so they are not pinned by this path.
func configLockPathsPinned(lock *crwdir.ConfigLock) (*configLockPathsPin, error) {
	pinned, ok := configLockPathsRealPath(lock.Target)
	if !ok || !lock.HoldsSidecar(pinned) {
		return nil, fmt.Errorf("the config file's directory changed while the lock was being taken (%s); run the deactivation again", lock.Target)
	}
	// The identities are captured HERE, once, while the sidecar proof above holds. Every later
	// comparison is judged against these captured values, never against the pinned path read from
	// the filesystem again: re-reading it would follow a directory replaced after this point and
	// accept the replacement as the locked file (CRW-899's fifth-generation evaluation).
	// A config file that is already gone is not an error — an uninstall whose config.toml was
	// removed still disables flags — so a missing file leaves the file identity nil and the
	// comparison falls back to comparing resolved paths.
	dirPath := filepath.Dir(pinned)
	dir, err := os.Stat(dirPath)
	if err != nil {
		return nil, fmt.Errorf("the config file's directory could not be inspected (%s); run the deactivation again", dirPath)
	}
	file, err := os.Stat(pinned)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("the config file could not be inspected (%s); run the deactivation again", pinned)
	}
	return &configLockPathsPin{lock: lock, path: pinned, file: file, dir: dir}, nil
}

// configLockPathsPin is the locked config file as it was when the lock was proven: its resolved path
// and the identities of the file and of its parent directory at that moment. A comparison against a
// later manifest is judged against these captured values, so a path replaced after the pin cannot be
// accepted as the locked file. The lock itself is kept so the pin can re-prove, at the moment of the
// decision, that the pinned path still names the sidecar the lock holds; without that proof the
// captured identities describe a file the lock no longer guards.
type configLockPathsPin struct {
	lock *crwdir.ConfigLock
	path string
	file os.FileInfo
	dir  os.FileInfo
}

// configLockPathsPinHolds reports whether the pinned path still names the very sidecar this lock
// holds. The pin is captured while the sidecar proof holds, but the directory behind the pinned path
// can be replaced afterwards: swapping the pinned directory with another one makes the pinned
// pathname name a different file while the held sidecar now lives beside the moved directory. The
// captured identities alone cannot see that — the replacement's file and directory are a different
// pair, but a manifest naming the moved directory still matches the captured pair — so the comparison
// re-proves the lock's own sidecar identity here and refuses when it no longer holds (CRW-899 E1,
// fail closed). This re-stat is the lock's identity check, not a re-interpretation of the pin as a
// comparison target: a stale pin makes the answer false, never a different acceptance.
func configLockPathsPinHolds(pinned *configLockPathsPin) bool {
	return pinned != nil && pinned.lock.HoldsSidecar(pinned.path)
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
	} else if trimmed := strings.TrimSuffix(dir, string(filepath.Separator)); trimmed != "" {
		// A trailing separator is dropped so the parent resolves as a directory, but the root
		// separator is not: "" would resolve to the working directory instead of "/".
		dir = trimmed
	}
	realDir, err := filepath.EvalSymlinks(dir)
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

// configLockPathsSameTarget reports whether a manifest's spelling names the file the lock still
// guards. It first re-proves that the pinned path still names the held sidecar
// (configLockPathsPinHolds): a directory replaced after the pin moves the held sidecar away from the
// pinned pathname, so the answer is false whatever the manifest names (CRW-899 E1). The manifest is
// allowed to name the config file through a different spelling — CODEX_HOME behind a directory
// symlink, for example, which crwdir's lock resolution leaves spelled through the alias because it
// follows only a symlink in the final component — and a deactivation that treated that as a different
// file would refuse to restore an install it owns. The comparison is deliberately directory-entry
// identity, not inode identity: the restore publishes through the pinned path with an atomic rename,
// which replaces that one pathname, so a hard link to the same inode under another name would keep
// the managed key while this command reported it restored (fail open). A hard link therefore stays
// refused, and a spelling that cannot be resolved is not the same target, so the comparison never
// accepts what it could not prove.
func configLockPathsSameTarget(spelling string, pinned *configLockPathsPin) bool {
	if !configLockPathsPinHolds(pinned) {
		return false
	}
	real, ok := configLockPathsRealPath(spelling)
	if !ok {
		return false
	}
	if real == pinned.path {
		return true
	}
	if pinned.file == nil {
		// The config file was already gone at pin time: there is nothing to compare it with, and
		// the absent-config path restores nothing.
		return false
	}
	// The spellings differ. On a case-insensitive filesystem they can still name one directory
	// entry, and filepath.EvalSymlinks follows links without canonicalising the case of ordinary
	// components. That is the same target — a rename over it replaces the entry both spellings
	// name — but a second entry whose name differs only by case (a case-sensitive sibling, or a
	// hard link) is a DIFFERENT entry a rename would not reach, so it stays refused. The two are
	// told apart with the identities captured at pin time: the candidate must be the pinned file,
	// in the pinned directory, and that directory must hold exactly one entry under the folded
	// name. The pin itself is proven live by configLockPathsPinHolds above, so the captured
	// directory is the one the pinned path still names.
	if !strings.EqualFold(filepath.Base(real), filepath.Base(pinned.path)) {
		return false
	}
	realFile, err := os.Stat(real)
	if err != nil || !os.SameFile(realFile, pinned.file) {
		return false
	}
	realDir, err := os.Stat(filepath.Dir(real))
	if err != nil || !os.SameFile(realDir, pinned.dir) {
		return false
	}
	if filepath.Base(real) == filepath.Base(pinned.path) {
		// The two spellings reach one file and one parent directory under the SAME basename, so
		// they are one directory entry whatever else the directory holds: a rename over either
		// spelling replaces that entry, and an unrelated case-differing sibling does not change
		// that. The folded-name count below is only needed when the basenames differ by case.
		return true
	}
	return configLockPathsOneFoldedEntry(real, pinned)
}

// configLockPathsOneFoldedEntry reports whether the candidate's spelling and the locked file's name
// reach exactly one directory entry. Where the directory can be read the entries are counted: the
// kernel permits no two entries with fold-equal names on a directory that folds case, so one such
// entry IS the locked entry, and two — a case-sensitive sibling or a hard link — are entries a
// rename over the locked path would not reach. Where it cannot be read (a parent without read
// permission, which still allows the stat, open, create and rename the restore needs), the count is
// replaced by a direct test of the filesystem itself: a probe name is created in that directory and
// read back under a different case. A filesystem that resolves it to the probe is one where the two
// spellings are necessarily the same entry, so the alias is accepted; a filesystem that does not is
// one where a second, case-differing entry can exist — the hard-link shape — and the comparison
// refuses rather than restore under a name the lock does not guard. An inode link count is
// deliberately not used: it cannot separate the locked entry from a hard link (CRW-899's eighth
// evaluation) and a count captured at pin time is stale by the time the manifest is read, which
// accepted an entry created after the pin (the ninth evaluation's fail-open).
func configLockPathsOneFoldedEntry(real string, pinned *configLockPathsPin) bool {
	dir := filepath.Dir(real)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return configLockPathsDirFoldsCase(dir)
	}
	matches := 0
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), filepath.Base(pinned.path)) {
			matches++
		}
	}
	return matches == 1
}

// configLockPathsDirFoldsCase reports whether a directory resolves two spellings of one name to the
// same entry, by creating a probe and reading it back under a different case. It needs only the
// write and search permission the restore itself needs, so it answers where the directory cannot be
// enumerated. Anything that prevents the probe — a create that fails, a name that cannot be removed
// — leaves the question unproven and the answer false, so the comparison refuses (fail closed).
func configLockPathsDirFoldsCase(dir string) bool {
	probe := filepath.Join(dir, "crw-899-CaseProbe-"+rand.Text())
	f, err := os.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false
	}
	_ = f.Close()
	defer func() { _ = os.Remove(probe) }()
	info, err := os.Stat(probe)
	if err != nil {
		return false
	}
	folded, err := os.Stat(filepath.Join(dir, strings.ToLower(filepath.Base(probe))))
	if err != nil {
		return false
	}
	return os.SameFile(info, folded)
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
		pin, pinErr := configLockPathsPinned(lock)
		if pinErr != nil {
			return nil, pinErr
		}
		path = pin.path
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
		if deps.ConfigPath != "" {
			// An explicit ConfigPath has no manifest spelling to agree with, but the pinned path must
			// still name the pinned file: the override chooses WHICH file this command acts on, and a
			// directory replacement or a final-component symlink swapped in after the pin would send
			// the read, the restore and the hash to another file without its lock. The same proof the
			// manifest branch uses runs here, against the pinned path itself (CRW-899's eleventh and
			// twelfth evaluations).
			if !configLockPathsSameTarget(pin.path, pin) {
				return nil, fmt.Errorf("the config file's directory changed while the lock was held (%s); run the deactivation again", lockedPath)
			}
		} else if !configLockPathsSameTarget(m.ConfigPath, pin) {
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
